我在接手一个交易对账服务，代码是 Java 21 + Spring Boot 3.5，数据放 PostgreSQL 17，支付平台的异步通知通过 Kafka 进来。服务本身不真的扣款，它负责把我们订单库里的应收金额、支付平台回调和每天下载的渠道账单对齐，然后生成一份不可变的资金流水。最近灰度切到新 consumer 后出现了一个很麻烦的问题：极少数订单会多记一笔入账，另一些订单明明支付成功却一直停在 `PENDING`。财务抽样发现，同一事件重放、Kafka rebalance 和数据库提交超时都可能相关，但现在没有一条证据能完整解释。

这是一个正在运行的内部系统，我希望先收敛故障模型，不想一上来就换技术栈。下面把业务口径、表结构、消费代码、定时对账、测试和脱敏日志都贴出来。请先像资深同事做事故复盘那样，画出一笔支付从 webhook 到 ledger 再到 reconciliation 的状态变化，分别分析“重复入账”“漏记入账”“状态已成功但余额没变”可能经过哪些路径。然后按风险排序指出最应该先修的三个地方，并说明每个结论依赖什么假设。第一轮先不要给完整重构代码，也不要用一句“Kafka 只能至少一次”带过数据库里的问题。

## 业务口径

系统每天大约处理 420 万笔支付，峰值每秒 900 条渠道事件。一个业务订单可以有多次支付尝试，但最多只能有一次最终成功的 capture。退款可能分多次发生，总退款金额不能超过成功入账金额。

我们目前承诺：

1. 相同 `provider + provider_event_id` 的回调重放不能产生第二笔账务流水。
2. 相同 `merchant_id + order_no` 最多有一个 `CAPTURED` 支付；失败后发起的新 attempt 有新的 `payment_id`。
3. ledger 只追加，不原地修改；冲正通过一条方向相反、引用原流水的 entry 表示。
4. `payments.status=CAPTURED`、capture ledger entry 和商户可用余额必须在同一个数据库提交结果里保持一致。
5. Kafka offset 允许重复消费，不能早于对应数据库事务成功提交。
6. 对渠道返回超时的主动查询可能与迟到 webhook 同时到达，两条路径必须收敛到同一个结果。
7. 渠道账单是外部事实来源，但迟到一天很常见；不能因为日报缺一行就自动改写内部 ledger。
8. 金额都用最小货币单位的 `bigint`，不同币种不能相加，不使用浮点数。
9. 数据库主从切换时客户端可能不知道刚才的 COMMIT 是否成功，重试必须安全。
10. 财务要求每个自动修复都有审计记录，不能直接删掉“多出来”的流水。

## 主要表结构

```sql
CREATE TYPE payment_status AS ENUM (
  'PENDING', 'CAPTURED', 'FAILED', 'CANCELLED'
);

CREATE TABLE payments (
  id                  uuid PRIMARY KEY,
  merchant_id         bigint NOT NULL,
  order_no            text NOT NULL,
  provider             text NOT NULL,
  provider_payment_id text,
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency             char(3) NOT NULL,
  status               payment_status NOT NULL,
  captured_at          timestamptz,
  version              integer NOT NULL DEFAULT 0,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (merchant_id, order_no, id)
);

CREATE UNIQUE INDEX one_captured_payment_per_order
  ON payments (merchant_id, order_no)
  WHERE status = 'CAPTURED';

CREATE TABLE provider_events (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  provider             text NOT NULL,
  provider_event_id   text NOT NULL,
  provider_payment_id text NOT NULL,
  event_type           text NOT NULL,
  amount_minor        bigint NOT NULL,
  currency             char(3) NOT NULL,
  payload_sha256       bytea NOT NULL,
  payload              jsonb NOT NULL,
  received_at          timestamptz NOT NULL,
  processed_at         timestamptz,
  processing_error     text,
  UNIQUE (provider, provider_event_id)
);

CREATE TYPE ledger_direction AS ENUM ('CREDIT', 'DEBIT');

CREATE TABLE ledger_entries (
  id                  uuid PRIMARY KEY,
  merchant_id         bigint NOT NULL,
  payment_id          uuid NOT NULL REFERENCES payments(id),
  source_event_id     bigint REFERENCES provider_events(id),
  entry_kind          text NOT NULL,
  direction           ledger_direction NOT NULL,
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency             char(3) NOT NULL,
  reversal_of          uuid REFERENCES ledger_entries(id),
  idempotency_key     text NOT NULL,
  occurred_at          timestamptz NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (merchant_id, idempotency_key)
);

CREATE TABLE merchant_balances (
  merchant_id       bigint NOT NULL,
  currency          char(3) NOT NULL,
  available_minor   bigint NOT NULL DEFAULT 0,
  pending_minor     bigint NOT NULL DEFAULT 0,
  version           bigint NOT NULL DEFAULT 0,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (merchant_id, currency)
);

CREATE TABLE reconciliation_findings (
  id                 uuid PRIMARY KEY,
  business_date      date NOT NULL,
  provider           text NOT NULL,
  provider_payment_id text NOT NULL,
  finding_type       text NOT NULL,
  internal_amount    bigint,
  external_amount    bigint,
  status             text NOT NULL,
  evidence           jsonb NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  resolved_at        timestamptz,
  UNIQUE (business_date, provider, provider_payment_id, finding_type)
);
```

`provider_events` 是审计收件箱；Kafka 消息的 value 里既有完整 payload，也有 gateway 提前解析出来的 provider 和 event id。当前 consumer 会先用 Kafka value 查 payment，再开事务。团队当时认为 `provider_events` 的唯一键足以解决幂等。

## Consumer 配置

```yaml
spring:
  kafka:
    consumer:
      group-id: payment-ledger-v2
      enable-auto-commit: false
      isolation-level: read_committed
      max-poll-records: 200
      properties:
        max.poll.interval.ms: 300000
    listener:
      ack-mode: manual_immediate
      concurrency: 12
```

listener：

```java
@KafkaListener(topics = "provider-payment-events")
public void onMessage(
    ConsumerRecord<String, ProviderEventMessage> record,
    Acknowledgment acknowledgment
) {
    ProviderEventMessage message = record.value();
    try {
        paymentEventService.apply(message);
        acknowledgment.acknowledge();
    } catch (DuplicateKeyException duplicate) {
        log.info("duplicate provider event provider={} eventId={}",
            message.provider(), message.eventId());
        acknowledgment.acknowledge();
    } catch (IllegalStateException invalidState) {
        deadLetterPublisher.publish(record, invalidState.getMessage());
        acknowledgment.acknowledge();
    }
}
```

错误处理器还配置了三次指数退避。`deadLetterPublisher.publish` 使用普通 Kafka producer，没开事务；发布失败会抛异常，但 listener 外层的框架错误处理器是否还会重试这条原消息，团队里说法不一致。

## 当前事务代码

```java
@Service
public class PaymentEventService {
    private final JdbcClient jdbc;
    private final TransactionTemplate transactions;
    private final Clock clock;

    public void apply(ProviderEventMessage message) {
        PaymentRow payment = jdbc.sql("""
                select * from payments
                 where provider = :provider
                   and provider_payment_id = :providerPaymentId
                """)
            .param("provider", message.provider())
            .param("providerPaymentId", message.providerPaymentId())
            .query(PaymentRow.class)
            .single();

        transactions.executeWithoutResult(status -> {
            Long eventPk = jdbc.sql("""
                    insert into provider_events(
                      provider, provider_event_id, provider_payment_id,
                      event_type, amount_minor, currency,
                      payload_sha256, payload, received_at
                    ) values (
                      :provider, :eventId, :providerPaymentId,
                      :eventType, :amount, :currency,
                      :payloadHash, cast(:payload as jsonb), :receivedAt
                    )
                    returning id
                    """)
                .params(message.toParams())
                .query(Long.class)
                .single();

            switch (message.eventType()) {
                case "payment.captured" -> capture(payment, eventPk, message);
                case "payment.failed" -> fail(payment, eventPk, message);
                case "refund.succeeded" -> refund(payment, eventPk, message);
                default -> throw new IllegalStateException("unsupported event");
            }

            jdbc.sql("""
                    update provider_events
                       set processed_at = :now, processing_error = null
                     where id = :id
                    """)
                .param("now", clock.instant())
                .param("id", eventPk)
                .update();
        });
    }

    private void capture(PaymentRow stalePayment, long eventPk,
                         ProviderEventMessage message) {
        if (stalePayment.status() == PaymentStatus.CAPTURED) {
            return;
        }
        if (stalePayment.amountMinor() != message.amountMinor()
                || !stalePayment.currency().equals(message.currency())) {
            throw new IllegalStateException("capture amount mismatch");
        }

        int changed = jdbc.sql("""
                update payments
                   set status = 'CAPTURED', captured_at = :capturedAt,
                       version = version + 1, updated_at = :now
                 where id = :paymentId
                """)
            .param("capturedAt", message.occurredAt())
            .param("now", clock.instant())
            .param("paymentId", stalePayment.id())
            .update();
        if (changed != 1) {
            throw new IllegalStateException("payment not updated");
        }

        UUID entryId = UUID.randomUUID();
        jdbc.sql("""
                insert into ledger_entries(
                  id, merchant_id, payment_id, source_event_id, entry_kind,
                  direction, amount_minor, currency, idempotency_key, occurred_at
                ) values (
                  :id, :merchantId, :paymentId, :eventPk, 'CAPTURE',
                  'CREDIT', :amount, :currency, :idempotencyKey, :occurredAt
                )
                """)
            .param("id", entryId)
            .param("merchantId", stalePayment.merchantId())
            .param("paymentId", stalePayment.id())
            .param("eventPk", eventPk)
            .param("amount", message.amountMinor())
            .param("currency", message.currency())
            .param("idempotencyKey", "capture:" + message.eventId())
            .param("occurredAt", message.occurredAt())
            .update();

        jdbc.sql("""
                insert into merchant_balances(
                  merchant_id, currency, available_minor, pending_minor, version
                ) values (:merchantId, :currency, :amount, 0, 1)
                on conflict (merchant_id, currency) do update
                   set available_minor = merchant_balances.available_minor + excluded.available_minor,
                       version = merchant_balances.version + 1,
                       updated_at = now()
                """)
            .param("merchantId", stalePayment.merchantId())
            .param("currency", message.currency())
            .param("amount", message.amountMinor())
            .update();
    }
}
```

这里的 `JdbcClient` 和 `TransactionTemplate` 确实共享同一个 Spring datasource，但 `payment` 的 SELECT 在事务外。默认隔离级别是 READ COMMITTED。数据库连接池最大 30，consumer 并发 12，主动查询任务也使用同一个池。

## 主动查询路径

创建支付 90 秒仍为 PENDING 时，scheduler 会查询渠道 API。查到成功后，它不会伪造 provider event，而是直接调用下面的方法：

```java
@Transactional
public void markCapturedFromQuery(UUID paymentId, ProviderPayment snapshot) {
    PaymentRow payment = repository.findById(paymentId).orElseThrow();
    if (payment.status() == PaymentStatus.CAPTURED) {
        return;
    }
    repository.updateStatus(paymentId, PaymentStatus.CAPTURED, snapshot.capturedAt());
    ledgerRepository.insertCapture(
        UUID.randomUUID(),
        payment.merchantId(),
        payment.id(),
        snapshot.amountMinor(),
        snapshot.currency(),
        "query:" + payment.id() + ":" + snapshot.providerStatus()
    );
    balanceRepository.credit(payment.merchantId(), snapshot.currency(), snapshot.amountMinor());
}
```

这个方法没有 provider event 外键。查询任务可能重复执行，同一次渠道成功快照的 `providerStatus()` 都是字符串 `succeeded`。数据库里曾看到 `capture:event-8891` 和 `query:payment-uuid:succeeded` 两条 entry 指向同一个 payment。

## 退款路径的关键片段

```java
private void refund(PaymentRow payment, long eventPk, ProviderEventMessage message) {
    Long refunded = jdbc.sql("""
            select coalesce(sum(amount_minor), 0)
              from ledger_entries
             where payment_id = :paymentId
               and entry_kind = 'REFUND'
            """)
        .param("paymentId", payment.id())
        .query(Long.class)
        .single();

    if (refunded + message.amountMinor() > payment.amountMinor()) {
        throw new IllegalStateException("refund exceeds capture");
    }

    ledgerRepository.insertRefund(payment, eventPk, message);
    balanceRepository.debit(payment.merchantId(), payment.currency(), message.amountMinor());
}
```

`ledger_entries` 里退款也是正数金额、`direction=DEBIT`。两条部分退款可以并发到达。现在没有数据库约束把退款总额限制在 capture 金额内。

## 定时对账

每天 02:30 下载前一日渠道 CSV。文件中的唯一键是 `provider_payment_id + record_type + provider_sequence`。导入任务按 5000 行一批，把记录放临时表，再用下面的查询找差异：

```sql
SELECT s.provider_payment_id,
       s.record_type,
       s.amount_minor AS external_amount,
       sum(CASE e.direction WHEN 'CREDIT' THEN e.amount_minor
                            ELSE -e.amount_minor END) AS internal_amount
FROM provider_statement_rows s
LEFT JOIN payments p
  ON p.provider = s.provider
 AND p.provider_payment_id = s.provider_payment_id
LEFT JOIN ledger_entries e ON e.payment_id = p.id
WHERE s.business_date = :businessDate
GROUP BY s.provider_payment_id, s.record_type, s.amount_minor
HAVING s.amount_minor <> coalesce(
  sum(CASE e.direction WHEN 'CREDIT' THEN e.amount_minor ELSE -e.amount_minor END),
  0
);
```

渠道文件把 capture 和每次 refund 分成不同 `record_type`，但这个 SQL 对每一行都 join 了该 payment 的全部 ledger，所以发生过退款的订单经常被误报。补传文件还可能重复包含前一版的行。

## 当前测试

```java
@Test
void duplicateEventIsIgnored() {
    service.apply(captured("evt-1", "pay-1", 1200));
    assertThatThrownBy(() -> service.apply(captured("evt-1", "pay-1", 1200)))
        .isInstanceOf(DuplicateKeyException.class);
    assertThat(ledger.countByPayment("pay-1")).isEqualTo(1);
}

@Test
void captureCreditsBalance() {
    service.apply(captured("evt-2", "pay-2", 3500));
    assertThat(payment("pay-2").status()).isEqualTo(CAPTURED);
    assertThat(balance(merchantId, "CNY")).isEqualTo(3500);
}
```

测试都在一个线程里，数据库每条用例结束 rollback。没有并发 consumer、未知提交结果、主动查询与 webhook 竞争、两个 refund 并发、Kafka ack 失败或 rebalance 的场景。

## 一段脱敏日志

```text
2026-07-20T11:04:31.008Z INFO  listener-7 event_received partition=9 offset=771204 provider=alpha event_id=evt-8891 payment=pp-417
2026-07-20T11:04:31.017Z INFO  query-2 provider_query_success payment_id=9bce... provider_payment=pp-417 amount=6800
2026-07-20T11:04:31.039Z INFO  query-2 ledger_inserted key=query:9bce...:succeeded entry=ae12...
2026-07-20T11:04:31.044Z WARN  query-2 commit_result_unknown payment_id=9bce... sqlstate=08006
2026-07-20T11:04:31.052Z INFO  listener-7 provider_event_inserted event_pk=5520181 event_id=evt-8891
2026-07-20T11:04:31.070Z INFO  listener-7 ledger_inserted key=capture:evt-8891 entry=b71f...
2026-07-20T11:04:31.076Z INFO  listener-7 balance_credited merchant=481 currency=CNY amount=6800
2026-07-20T11:04:31.081Z INFO  listener-7 transaction_committed event_id=evt-8891
2026-07-20T11:04:31.083Z WARN  listener-7 ack_failed partition=9 offset=771204 reason=rebalance_in_progress
2026-07-20T11:04:33.228Z INFO  listener-3 event_received partition=9 offset=771204 provider=alpha event_id=evt-8891 payment=pp-417
2026-07-20T11:04:33.235Z INFO  listener-3 duplicate provider event provider=alpha event_id=evt-8891
```

财务查询到这个 payment 有两条 capture entry，余额也增加了两次；`payments.version=2`。`provider_events` 只有一条 evt-8891。另一个问题样本里 provider_events 已 processed、payment 是 CAPTURED、ledger 有 entry，但 merchant_balances 没有增加，尚未确认是读副本延迟、人工脚本还是旧版本代码造成的。

## 我希望第一轮回答覆盖的范围

请尽量沿着一笔具体支付讲，不要只列原则：

- 给出 webhook、主动查询、数据库事务、Kafka ack 和日终对账之间的状态图或文字状态机；
- 判断现有唯一键分别防住了什么、没有防住什么；
- 解释事务外读取的 `PaymentRow`、两个入口使用不同 idempotency key、部分退款并发各自会怎样出错；
- 对“数据库已经提交但客户端/consumer 不知道”的情况给出可验证的恢复思路；
- 按 P0/P1 排出三个最先处理的问题，并列出需要补采的日志或 SQL 证据。

第一轮先不引入分布式事务，也不用写出所有 Java 文件。我们可以在后续逐步讨论数据库不变量、统一命令入口、Kafka offset、退款、reconciliation、迁移和故障演练。
