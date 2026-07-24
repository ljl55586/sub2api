# 多租户 Webhook 投递平台可靠性审查

我们维护一个用 Go、PostgreSQL 和 Redis 实现的多租户 Webhook 投递平台。业务服务在事务中写入事件，平台负责把事件转换成 HTTP 请求投递到客户配置的 endpoint。系统已经上线半年，日均约 8000 万次投递，最近在扩容和故障恢复时出现了重复投递、长时间卡住、租户互相挤占资源和签名时间戳异常。

请把下面材料当成一个真实但经过裁剪的项目。第一轮先做整体正确性审查，找出最危险的设计问题并排序。重点关注数据库状态机、任务所有权、重试幂等、同 endpoint 顺序、Redis 与 PostgreSQL 一致性、HTTP 安全、多租户公平性和进程崩溃恢复。暂时不要重写全部代码，也不要假设可以立即引入 Kafka。

## 产品语义

- 每个 tenant 可以创建多个 endpoint。
- 业务事件写入 `webhook_events` 后必须最终进入终态：`delivered`、`exhausted` 或 `cancelled`。
- 一个事件可以关联多个 endpoint，每个关联关系生成一条 delivery。
- 平台语义是至少一次投递；客户可能收到重复请求，但同一 attempt 不应被平台并发执行两次。
- 同一个 endpoint 上，同一个 `ordering_key` 的事件应该按 `sequence_no` 投递。前一个事件永久失败后，后一个事件允许继续。
- endpoint 返回任意 2xx 都表示成功；408、425、429 和 5xx 可以重试；其他 4xx 默认永久失败。
- 429 优先使用 `Retry-After`，但单次等待不能超过 6 小时。
- 每条 delivery 最多尝试 12 次，总生命周期不超过 72 小时。
- 客户可以暂停 endpoint、轮换 secret、取消尚未成功的 delivery，并从死信列表手工重放。
- 同一 tenant 默认最多 100 个并发 HTTP 请求，单 endpoint 默认最多 8 个。
- 平台不能访问环回地址、链路本地地址、云 metadata 地址或公司内网。
- 请求签名使用 endpoint secret，对 `timestamp + "." + raw_body` 做 HMAC-SHA256。
- 审计要求保留 delivery 摘要两年，响应 body 样本最多保留七天。

## 数据表

```sql
CREATE TABLE webhook_endpoints (
    id                 uuid PRIMARY KEY,
    tenant_id          bigint NOT NULL,
    url                text NOT NULL,
    secret_ciphertext  bytea NOT NULL,
    secret_version     integer NOT NULL DEFAULT 1,
    status             text NOT NULL DEFAULT 'active',
    max_concurrency    integer NOT NULL DEFAULT 8,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhook_endpoints_tenant_idx
    ON webhook_endpoints (tenant_id, id);

CREATE TABLE webhook_events (
    id              uuid PRIMARY KEY,
    tenant_id       bigint NOT NULL,
    event_type      text NOT NULL,
    ordering_key    text,
    sequence_no     bigint,
    payload         jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id                  uuid PRIMARY KEY,
    tenant_id           bigint NOT NULL,
    event_id            uuid NOT NULL REFERENCES webhook_events(id),
    endpoint_id         uuid NOT NULL REFERENCES webhook_endpoints(id),
    status              text NOT NULL DEFAULT 'pending',
    attempt_count       integer NOT NULL DEFAULT 0,
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    locked_by           text,
    locked_until        timestamptz,
    last_status_code    integer,
    last_error          text,
    delivered_at        timestamptz,
    cancelled_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);

CREATE INDEX webhook_deliveries_ready_idx
    ON webhook_deliveries (next_attempt_at, id)
    WHERE status IN ('pending', 'retrying');

CREATE TABLE webhook_attempts (
    id                  uuid PRIMARY KEY,
    delivery_id         uuid NOT NULL REFERENCES webhook_deliveries(id),
    attempt_no          integer NOT NULL,
    worker_id           text NOT NULL,
    started_at          timestamptz NOT NULL,
    finished_at         timestamptz,
    status_code         integer,
    error_class         text,
    response_sample     bytea,
    duration_ms         integer,
    UNIQUE (delivery_id, attempt_no)
);
```

应用没有给 `status` 增加 CHECK constraint。历史数据里已经发现少量 `processing`、`failed` 和空字符串状态，这些值来自旧版本。

## 事件创建

业务服务不会直接写平台数据库，而是调用下面的 API：

```go
type CreateEventRequest struct {
    TenantID    int64           `json:"tenant_id"`
    EventID     uuid.UUID       `json:"event_id"`
    EventType   string          `json:"event_type"`
    OrderingKey string          `json:"ordering_key"`
    SequenceNo  int64           `json:"sequence_no"`
    Payload     json.RawMessage `json:"payload"`
    Endpoints   []uuid.UUID     `json:"endpoints"`
}

func (s *Service) CreateEvent(ctx context.Context, req CreateEventRequest) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO webhook_events
            (id, tenant_id, event_type, ordering_key, sequence_no, payload)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (id) DO NOTHING
    `, req.EventID, req.TenantID, req.EventType, req.OrderingKey, req.SequenceNo, req.Payload)
    if err != nil {
        return err
    }

    for _, endpointID := range req.Endpoints {
        _, err = tx.ExecContext(ctx, `
            INSERT INTO webhook_deliveries
                (id, tenant_id, event_id, endpoint_id)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (event_id, endpoint_id) DO NOTHING
        `, uuid.New(), req.TenantID, req.EventID, endpointID)
        if err != nil {
            return err
        }
    }

    if err := tx.Commit(); err != nil {
        return err
    }

    return s.redis.Publish(ctx, "webhook-ready", req.EventID.String()).Err()
}
```

调用方会在超时后使用同一个 `event_id` 重试，但可能因为业务 bug 携带不同 payload 或不同 endpoint 列表。目前 API 对这种冲突不会报错。

## Scheduler

每个 scheduler 实例每 200ms 扫描一次：

```go
func (s *Scheduler) Tick(ctx context.Context) error {
    rows, err := s.db.QueryContext(ctx, `
        SELECT id
        FROM webhook_deliveries
        WHERE status IN ('pending', 'retrying')
          AND next_attempt_at <= now()
          AND (locked_until IS NULL OR locked_until < now())
        ORDER BY next_attempt_at, id
        LIMIT 500
    `)
    if err != nil {
        return err
    }
    defer rows.Close()

    var ids []uuid.UUID
    for rows.Next() {
        var id uuid.UUID
        if err := rows.Scan(&id); err != nil {
            return err
        }
        ids = append(ids, id)
    }

    for _, id := range ids {
        if err := s.redis.LPush(ctx, "webhook-jobs", id.String()).Err(); err != nil {
            return err
        }
    }
    return rows.Err()
}
```

生产有 12 个 scheduler 实例。没有 scheduler leader，也没有在入 Redis 前更新 delivery。一个 delivery 在 `next_attempt_at` 到达后，通常会被多个 scheduler 同时推入队列。

Redis list 没有长度上限。数据库慢时 scheduler 查询会重叠；Redis 恢复后曾在 90 秒内积累约 3000 万条重复 job。

## Worker 主循环

```go
func (w *Worker) Run(ctx context.Context) error {
    for {
        result, err := w.redis.BRPop(ctx, 5*time.Second, "webhook-jobs").Result()
        if errors.Is(err, redis.Nil) {
            continue
        }
        if err != nil {
            return err
        }
        if len(result) != 2 {
            continue
        }

        id, err := uuid.Parse(result[1])
        if err != nil {
            continue
        }
        go w.deliver(ctx, id)
    }
}
```

每个 worker pod 配置 `GOMAXPROCS=4`，但 `deliver` goroutine 没有限制。流量突增时单 pod 曾出现 6 万个 goroutine 和 4 万个打开连接。

Redis job 使用 `BRPOP` 后立即从 list 删除。worker 在取得 job 后、更新数据库前崩溃时，job 只能等 scheduler 下次扫描重新生成。

## Claim delivery

```go
func (w *Worker) claim(ctx context.Context, id uuid.UUID) (*Delivery, error) {
    leaseUntil := time.Now().Add(30 * time.Second)

    row := w.db.QueryRowContext(ctx, `
        UPDATE webhook_deliveries
        SET status = 'processing',
            locked_by = $2,
            locked_until = $3,
            attempt_count = attempt_count + 1,
            updated_at = now()
        WHERE id = $1
          AND status IN ('pending', 'retrying', 'processing')
          AND (locked_until IS NULL OR locked_until < now())
        RETURNING id, tenant_id, event_id, endpoint_id,
                  status, attempt_count, next_attempt_at,
                  locked_by, locked_until
    `, id, w.workerID, leaseUntil)

    var d Delivery
    if err := row.Scan(
        &d.ID,
        &d.TenantID,
        &d.EventID,
        &d.EndpointID,
        &d.Status,
        &d.AttemptCount,
        &d.NextAttemptAt,
        &d.LockedBy,
        &d.LockedUntil,
    ); err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return nil, ErrNotClaimed
        }
        return nil, err
    }
    return &d, nil
}
```

`time.Now()` 使用 worker 主机时间，而 SQL 中的过期判断使用数据库时间。部分节点发生过 8–20 秒时钟偏差。

租约固定 30 秒，HTTP timeout 是 45 秒；读取大响应 body 最多再花 10 秒。worker 没有续租。旧 owner 完成时也不检查 `locked_by`。

## 投递实现

```go
func (w *Worker) deliver(parent context.Context, id uuid.UUID) {
    d, err := w.claim(parent, id)
    if err != nil {
        return
    }

    endpoint, err := w.loadEndpoint(parent, d.EndpointID)
    if err != nil {
        w.retry(parent, d, "endpoint_load", 0, err)
        return
    }
    event, err := w.loadEvent(parent, d.EventID)
    if err != nil {
        w.retry(parent, d, "event_load", 0, err)
        return
    }

    body, err := json.Marshal(struct {
        ID        uuid.UUID       `json:"id"`
        Type      string          `json:"type"`
        CreatedAt time.Time       `json:"created_at"`
        Data      json.RawMessage `json:"data"`
    }{
        ID:        event.ID,
        Type:      event.EventType,
        CreatedAt: event.CreatedAt,
        Data:      event.Payload,
    })
    if err != nil {
        w.retry(parent, d, "encode", 0, err)
        return
    }

    req, err := http.NewRequestWithContext(
        parent,
        http.MethodPost,
        endpoint.URL,
        bytes.NewReader(body),
    )
    if err != nil {
        w.retry(parent, d, "request", 0, err)
        return
    }

    timestamp := strconv.FormatInt(time.Now().Unix(), 10)
    signature := sign(endpoint.Secret, timestamp+"."+string(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("User-Agent", "Acme-Webhooks/1.7")
    req.Header.Set("X-Webhook-ID", d.ID.String())
    req.Header.Set("X-Webhook-Attempt", strconv.Itoa(d.AttemptCount))
    req.Header.Set("X-Webhook-Timestamp", timestamp)
    req.Header.Set("X-Webhook-Signature", "v1="+signature)

    started := time.Now()
    resp, err := w.httpClient.Do(req)
    duration := time.Since(started)
    if err != nil {
        w.retry(parent, d, classifyNetError(err), duration, err)
        return
    }
    defer resp.Body.Close()

    sample, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
    if readErr != nil {
        w.retry(parent, d, "response_read", duration, readErr)
        return
    }

    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        w.markDelivered(parent, d, resp.StatusCode, duration, sample)
        return
    }
    if retryableStatus(resp.StatusCode) {
        w.retry(parent, d, "http_retryable", duration, fmt.Errorf("status %d", resp.StatusCode))
        return
    }
    w.markExhausted(parent, d, resp.StatusCode, duration, sample)
}
```

`httpClient` 是所有 tenant 共用的全局 client：

```go
transport := &http.Transport{
    MaxIdleConns:        20000,
    MaxIdleConnsPerHost: 100,
    IdleConnTimeout:     90 * time.Second,
}

client := &http.Client{
    Transport: transport,
    Timeout:   45 * time.Second,
}
```

系统会自动跟随最多 10 次 redirect。创建 endpoint 时只解析一次 URL，解析到公网 IP 就允许保存。真正投递时没有重新解析和校验目标 IP。

## 成功与失败更新

```go
func (w *Worker) markDelivered(
    ctx context.Context,
    d *Delivery,
    statusCode int,
    duration time.Duration,
    response []byte,
) {
    tx, err := w.db.BeginTx(ctx, nil)
    if err != nil {
        return
    }
    defer tx.Rollback()

    _, _ = tx.ExecContext(ctx, `
        INSERT INTO webhook_attempts
            (id, delivery_id, attempt_no, worker_id, started_at,
             finished_at, status_code, response_sample, duration_ms)
        VALUES ($1, $2, $3, $4, now() - $5::interval,
                now(), $6, $7, $8)
        ON CONFLICT (delivery_id, attempt_no) DO NOTHING
    `, uuid.New(), d.ID, d.AttemptCount, w.workerID,
        duration.String(), statusCode, response, duration.Milliseconds())

    _, _ = tx.ExecContext(ctx, `
        UPDATE webhook_deliveries
        SET status = 'delivered',
            delivered_at = now(),
            last_status_code = $2,
            locked_by = NULL,
            locked_until = NULL,
            updated_at = now()
        WHERE id = $1
    `, d.ID, statusCode)

    _ = tx.Commit()
}

func (w *Worker) retry(
    ctx context.Context,
    d *Delivery,
    class string,
    duration time.Duration,
    cause error,
) {
    delay := retryDelay(d.AttemptCount)
    next := time.Now().Add(delay)

    if d.AttemptCount >= 12 || time.Since(d.CreatedAt) >= 72*time.Hour {
        w.markExhausted(ctx, d, 0, duration, []byte(cause.Error()))
        return
    }

    _, _ = w.db.ExecContext(ctx, `
        UPDATE webhook_deliveries
        SET status = 'retrying',
            next_attempt_at = $2,
            last_error = $3,
            locked_by = NULL,
            locked_until = NULL,
            updated_at = now()
        WHERE id = $1
    `, d.ID, next, truncate(class+": "+cause.Error(), 4000))
}
```

数据库更新错误只写 debug 日志，调用方看不到。旧 owner 即使租约已经被新 worker 接管，也能把新 owner 正在执行的 delivery 改成 `delivered`、`retrying` 或 `exhausted`。

`attempt_count` 在 claim 时增加，但 attempt row 在请求结束后才创建。进程崩溃会留下没有 attempt row 的编号。两个 owner 竞争时，旧 owner 的 `ON CONFLICT DO NOTHING` 会悄悄丢失自己的结果。

## Retry-After 与退避

```go
func retryDelay(attempt int) time.Duration {
    base := time.Second * time.Duration(1<<min(attempt, 16))
    jitter := time.Duration(rand.Int63n(int64(base / 4)))
    return minDuration(base+jitter, 6*time.Hour)
}

func delayFromResponse(resp *http.Response, attempt int) time.Duration {
    raw := resp.Header.Get("Retry-After")
    if raw == "" {
        return retryDelay(attempt)
    }
    if seconds, err := strconv.Atoi(raw); err == nil {
        return time.Duration(seconds) * time.Second
    }
    if when, err := http.ParseTime(raw); err == nil {
        return time.Until(when)
    }
    return retryDelay(attempt)
}
```

当前调用链实际上没有使用 `delayFromResponse`，所有 429 都走普通指数退避。即使以后接入，该函数也没有限制负数和超过 6 小时的 header 值。

所有 worker 启动时都使用默认随机种子。一批同时失败的请求经常得到近似的退避时间，形成周期性尖峰。

## 顺序控制

团队尝试用 Redis lock 保证同一 ordering key 串行：

```go
func (w *Worker) acquireOrderLock(
    ctx context.Context,
    endpointID uuid.UUID,
    orderingKey string,
) (string, bool) {
    key := "webhook-order:" + endpointID.String() + ":" + orderingKey
    token := uuid.NewString()
    ok, err := w.redis.SetNX(ctx, key, token, 60*time.Second).Result()
    return token, err == nil && ok
}

func (w *Worker) releaseOrderLock(
    ctx context.Context,
    endpointID uuid.UUID,
    orderingKey string,
) {
    key := "webhook-order:" + endpointID.String() + ":" + orderingKey
    _ = w.redis.Del(ctx, key).Err()
}
```

释放时不校验 token。锁超时后旧 worker 会删除新 worker 的锁。即使锁工作正常，scheduler 也不检查前序 delivery 的状态；后序任务反复抢锁失败后会立即重新入队，造成热点。

`ordering_key` 允许空字符串和 8KB 字符串，没有哈希或长度限制。业务侧的 `sequence_no` 偶尔重复，没有数据库唯一约束。

## Tenant 与 endpoint 并发

```go
func (w *Worker) allowed(ctx context.Context, tenantID int64, endpointID uuid.UUID) bool {
    tenantKey := fmt.Sprintf("webhook-tenant-inflight:%d", tenantID)
    endpointKey := "webhook-endpoint-inflight:" + endpointID.String()

    tenantCount, _ := w.redis.Incr(ctx, tenantKey).Result()
    endpointCount, _ := w.redis.Incr(ctx, endpointKey).Result()
    _ = w.redis.Expire(ctx, tenantKey, time.Minute).Err()
    _ = w.redis.Expire(ctx, endpointKey, time.Minute).Err()

    if tenantCount > 100 || endpointCount > 8 {
        return false
    }
    return true
}

func (w *Worker) done(ctx context.Context, tenantID int64, endpointID uuid.UUID) {
    _ = w.redis.Decr(ctx, fmt.Sprintf("webhook-tenant-inflight:%d", tenantID)).Err()
    _ = w.redis.Decr(ctx, "webhook-endpoint-inflight:"+endpointID.String()).Err()
}
```

`allowed` 的两个 INCR 不是原子操作。超过限制时不会回滚已经增加的计数。worker 崩溃不会调用 `done`，正常路径有两个 error return 也漏掉了 `done`。TTL 每次 INCR 都刷新，因此持续流量下错误计数可能很久不归零。

任务队列只有一个全局 list。一个大 tenant 曾在 20 分钟内写入 1200 万条事件，其他 tenant 的 p99 排队时间从 8 秒上升到 47 分钟。

## Endpoint 暂停、取消和重放

```go
func (s *Service) PauseEndpoint(ctx context.Context, tenantID int64, endpointID uuid.UUID) error {
    _, err := s.db.ExecContext(ctx, `
        UPDATE webhook_endpoints
        SET status = 'paused', updated_at = now()
        WHERE id = $1 AND tenant_id = $2
    `, endpointID, tenantID)
    return err
}

func (s *Service) CancelDelivery(ctx context.Context, tenantID int64, id uuid.UUID) error {
    _, err := s.db.ExecContext(ctx, `
        UPDATE webhook_deliveries
        SET status = 'cancelled',
            cancelled_at = now(),
            locked_by = NULL,
            locked_until = NULL,
            updated_at = now()
        WHERE id = $1
          AND tenant_id = $2
          AND status <> 'delivered'
    `, id, tenantID)
    return err
}

func (s *Service) Replay(ctx context.Context, tenantID int64, id uuid.UUID) error {
    _, err := s.db.ExecContext(ctx, `
        UPDATE webhook_deliveries
        SET status = 'pending',
            attempt_count = 0,
            next_attempt_at = now(),
            last_error = NULL,
            locked_by = NULL,
            locked_until = NULL,
            delivered_at = NULL,
            cancelled_at = NULL,
            updated_at = now()
        WHERE id = $1 AND tenant_id = $2
    `, id, tenantID)
    return err
}
```

worker 只在 claim 前读取 endpoint status，实际 HTTP 请求前不再检查。暂停或取消与在途请求竞争时没有定义。Replay 会复用同一个 delivery id 和 attempt 编号，旧 attempt rows 仍在，因此新结果可能与历史唯一键冲突。

## Secret 轮换

endpoint 表只保存当前 secret：

```go
func (s *Service) RotateSecret(ctx context.Context, tenantID int64, endpointID uuid.UUID) error {
    secret := randomSecret()
    ciphertext, err := s.kms.Encrypt(ctx, []byte(secret))
    if err != nil {
        return err
    }
    _, err = s.db.ExecContext(ctx, `
        UPDATE webhook_endpoints
        SET secret_ciphertext = $3,
            secret_version = secret_version + 1,
            updated_at = now()
        WHERE id = $1 AND tenant_id = $2
    `, endpointID, tenantID, ciphertext)
    return err
}
```

delivery 没有保存使用哪个 secret version。重试会使用轮换后的新 secret，客户若只接受旧 secret，正在重试的事件会突然全部验签失败。管理 API 在数据库提交成功后才把明文 secret 返回给用户；响应丢失时无法再次取得同一个 secret。

## SSRF 防护

创建 endpoint 时的校验：

```go
func validateURL(ctx context.Context, raw string) error {
    u, err := url.Parse(raw)
    if err != nil {
        return err
    }
    if u.Scheme != "https" {
        return errors.New("https required")
    }
    ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
    if err != nil {
        return err
    }
    for _, ip := range ips {
        if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
            return errors.New("private address")
        }
    }
    return nil
}
```

实际 Transport 使用默认 DialContext，投递时会重新 DNS 解析。客户可以先让域名解析到公网，通过校验后再切到私网。redirect 的目标完全没有调用 `validateURL`。代码没有拒绝 IPv4-mapped IPv6、未指定地址、多播地址和部分特殊网段。

HTTP 代理可以通过环境变量启用；启用后 DNS 解析可能发生在代理侧，应用无法知道最终连接 IP。

## 可观测性

指标包括：

```text
webhook_attempt_total{tenant_id,endpoint_id,status_code,error_class}
webhook_delivery_latency_seconds{tenant_id,endpoint_id}
webhook_queue_depth
webhook_worker_goroutines{pod}
```

生产约有 40 万个 endpoint，Prometheus 已因为高基数 label 出现内存压力。日志会记录完整 endpoint URL、last_error 和最多 1MB response sample。部分客户响应包含 access token 和个人信息。

trace 只覆盖 HTTP 请求，没有覆盖 scheduler claim、Redis enqueue 和数据库终态提交。值班人员无法从一个 delivery id 找到所有 owner、attempt 和状态覆盖。

## 已观察到的事故

### 事故一：重复扣减库存

客户 endpoint 收到同一个 delivery 的两个并发请求，`X-Webhook-Attempt` 都是 4。两个请求都返回 204。数据库只有一条 attempt_no=4，因为另一个 INSERT 被 `ON CONFLICT DO NOTHING` 丢弃。

### 事故二：成功后又变回 retrying

新 owner 在 14:03:10 收到 200 并写入 delivered；旧 owner 在 14:03:12 因读 response body 超时执行 retry，无条件把状态改成 retrying。scheduler 随后投递了第五次。

### 事故三：暂停未生效

用户暂停 endpoint 后 18 分钟内仍收到请求。队列中已有大量 job，worker claim 没有 join endpoint 状态，`loadEndpoint` 使用 20 分钟本地缓存。

### 事故四：内网探测

一个 endpoint 域名创建时解析为公网地址，几分钟后被修改为 `169.254.169.254`。worker 跟随一次 302 后访问了 metadata 路径。WAF 没有覆盖 worker 出站流量。

### 事故五：小租户饥饿

全局 Redis list 被大 tenant 填满。虽然 tenant INCR 超限后会拒绝执行，但 worker 仍不断弹出大 tenant job，小 tenant 的 job 长时间排在尾部。

## 部署约束

- 目前不能立即引入 Kafka、Pulsar 或新的托管队列。
- 可以修改 PostgreSQL schema、Redis key 设计和 worker/scheduler 协议。
- 可以增加一到两张表。
- 数据库是 PostgreSQL 16，主库带两个只读副本。
- Redis 是 cluster，Lua 脚本只能访问同一个 hash slot 的 key。
- worker 运行在 Kubernetes，滚动发布期间新旧版本会并存约 20 分钟。
- 不能要求所有客户实现新的幂等协议，但可以稳定发送 delivery id 和 attempt id。
- API 与 worker 必须支持逐步灰度和快速回滚。
- 任何修复都必须说明旧队列、旧状态值和正在执行的旧 owner 怎样兼容。

## 第一轮问题

请先完成一次整体审查：

1. 按严重程度列出最危险的正确性和安全问题。
2. 画出你认为合理的 delivery 状态机与 owner/lease 不变量。
3. 给出一个不引入新消息系统的渐进修复顺序。
4. 指出哪些问题可以局部修补，哪些必须同时修改数据库和 worker 协议。
5. 说明第一阶段上线前必须补哪些确定性并发测试和故障注入测试。

先以分析和方案为主，不需要一次贴出所有完整实现。
