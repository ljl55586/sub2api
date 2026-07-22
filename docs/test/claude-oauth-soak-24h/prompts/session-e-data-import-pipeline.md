我们有一个给运营同事用的数据导入功能，允许把供应商提供的 CSV 上传后批量更新商品、库存和价格。后端是 Python 3.13 + FastAPI，任务跑在 Celery worker，PostgreSQL 17 存状态，原始文件放 S3 兼容对象存储，消息 broker 是 RabbitMQ。这个功能原来只处理几万行的小文件，现在最大的文件接近 12GB、约 4800 万行。最近一次扩容后出现过三种现象：任务进度卡在 99%，取消后仍继续写数据库，以及 worker 重启后从头解析导致同一批库存被重复覆盖。

我不想得到一份脱离现状的“换成 Spark”方案。下面是产品规则、表、Python 代码、任务配置、测试和一段故障记录。请第一轮先帮我把 upload、validate、apply、cancel、resume 的状态变化梳理清楚，找出最可能导致这三种现象的根因，并按修复优先级排序。请特别区分数据库事务边界、Celery 的至少一次投递、对象存储流式读取和业务幂等各自应该负责什么。如果现有需求彼此冲突，请先指出，先不要直接贴一套完整重写代码。

## 使用方式和业务规则

用户在网页选择租户、数据类型和 CSV 文件。浏览器先向 API 申请 multipart upload，直接把文件传到对象存储，完成后调用 `/imports/{id}/commit`。服务异步做格式校验和正式导入。

当前规则：

1. 一个 import 只属于一个 tenant，任何查询和写入都不能越租户。
2. 文件首行是 header，允许 UTF-8 BOM；字段顺序可变，但字段名不能重复。
3. 一行用 `external_sku` 标识商品。同一个文件内重复 SKU 时，产品目前要求“最后一行胜出”。
4. `replace` 模式会把文件里出现的 SKU 更新成给定值，并把这次文件里没出现的旧 SKU 标成 inactive；`patch` 模式只更新非空列。
5. 价格以分为单位，库存为非负整数。解析错误要记录行号、字段和最多 200 个样本，不能把 4800 万条错误全放内存。
6. validate 阶段不能修改正式商品表；验证通过后用户还可以预览统计再点击 apply。
7. apply 必须可重试。同一个 import 被 Celery 重投，不能重复应用非幂等的库存增量。
8. 用户可以取消 validating 或 applying。已经提交的数据库事务不回滚，但取消确认后不应再开始新批次。
9. worker 崩溃后应该从安全 checkpoint 恢复，不要求精确从某个字节继续，但不能总是从 12GB 文件开头重跑。
10. 正式 apply 期间网站的商品读取不能停；允许短暂旧数据，但同一 SKU 不能出现半行新、半行旧。
11. import 完成后原文件保留 30 天，staging 数据保留 7 天，审计摘要保留两年。
12. 整个流程 p95 没有秒级要求，但单个 tenant 同时最多两个 import，避免把共享数据库打满。

## 表结构

```sql
CREATE TYPE import_status AS ENUM (
  'UPLOADING',
  'UPLOADED',
  'VALIDATING',
  'READY',
  'APPLYING',
  'COMPLETED',
  'FAILED',
  'CANCEL_REQUESTED',
  'CANCELLED'
);

CREATE TABLE imports (
  id                    uuid PRIMARY KEY,
  tenant_id             bigint NOT NULL,
  kind                  text NOT NULL,
  mode                  text NOT NULL,
  status                import_status NOT NULL,
  object_key            text NOT NULL,
  object_etag           text,
  object_size           bigint,
  upload_id             text,
  total_rows            bigint,
  valid_rows            bigint NOT NULL DEFAULT 0,
  invalid_rows          bigint NOT NULL DEFAULT 0,
  parsed_bytes          bigint NOT NULL DEFAULT 0,
  applied_rows          bigint NOT NULL DEFAULT 0,
  checkpoint_row        bigint NOT NULL DEFAULT 0,
  checkpoint_byte       bigint NOT NULL DEFAULT 0,
  version               integer NOT NULL DEFAULT 0,
  error_summary         jsonb NOT NULL DEFAULT '{}',
  created_by            bigint NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  completed_at          timestamptz,
  CHECK (mode IN ('replace', 'patch'))
);

CREATE TABLE import_rows (
  import_id             uuid NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
  tenant_id             bigint NOT NULL,
  row_number            bigint NOT NULL,
  external_sku          text,
  name                  text,
  price_minor           bigint,
  stock_value           bigint,
  stock_delta           bigint,
  parsed                jsonb NOT NULL,
  row_hash              bytea NOT NULL,
  validation_error      jsonb,
  applied_at            timestamptz,
  PRIMARY KEY (import_id, row_number)
);

CREATE INDEX import_rows_valid_sku_idx
  ON import_rows(import_id, external_sku, row_number DESC)
  WHERE validation_error IS NULL;

CREATE TABLE products (
  tenant_id             bigint NOT NULL,
  external_sku          text NOT NULL,
  name                  text NOT NULL,
  price_minor           bigint NOT NULL CHECK (price_minor >= 0),
  stock                 bigint NOT NULL CHECK (stock >= 0),
  active                boolean NOT NULL DEFAULT true,
  source_import_id      uuid,
  version               bigint NOT NULL DEFAULT 0,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, external_sku)
);

CREATE TABLE import_batches (
  import_id             uuid NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
  phase                 text NOT NULL,
  batch_no              bigint NOT NULL,
  first_row             bigint NOT NULL,
  last_row              bigint NOT NULL,
  row_count             integer NOT NULL,
  status                text NOT NULL,
  checksum              bytea NOT NULL,
  started_at            timestamptz,
  finished_at           timestamptz,
  worker_id             text,
  PRIMARY KEY(import_id, phase, batch_no)
);
```

`stock_value` 用于 replace，`stock_delta` 是两个月前为了 patch 新增的。现在同一个模板允许这两列同时存在，代码只是优先使用 delta，产品文档没有说明。

## API 提交和任务启动

```python
@router.post("/imports/{import_id}/commit", status_code=202)
async def commit_import(
    import_id: UUID,
    request: Request,
    session: AsyncSession = Depends(db_session),
    actor: Actor = Depends(current_actor),
) -> dict[str, str]:
    item = await session.scalar(
        select(Import).where(
            Import.id == import_id,
            Import.tenant_id == actor.tenant_id,
        )
    )
    if item is None:
        raise HTTPException(404)
    if item.status == ImportStatus.UPLOADED:
        item.status = ImportStatus.VALIDATING
        item.updated_at = utcnow()
        await session.commit()
        validate_import.delay(str(item.id))
    return {"id": str(item.id), "status": item.status.value}
```

上传完成回调会把状态从 UPLOADING 改为 UPLOADED，并记录浏览器上报的 ETag 和 size。它没有再对对象存储做 `HEAD`。前端网络超时时可能重复调用 commit；两个 API 实例都可能在第一次事务提交后执行 `validate_import.delay`。

## Celery 配置

```python
broker_transport_options = {"visibility_timeout": 60 * 60}
task_acks_late = True
task_reject_on_worker_lost = True
worker_prefetch_multiplier = 1
task_time_limit = 6 * 60 * 60
task_soft_time_limit = 5 * 60 * 60 + 50 * 60
```

大文件 validate 最长跑过 9 小时。运维为了避免连接超时，把部分 worker 的 `visibility_timeout` 临时改成了 12 小时，但不是所有 pod 同时更新。任务运行期间没有显式续租概念。

## validate 任务

```python
@celery.task(bind=True, autoretry_for=(TransientStorageError,),
             retry_backoff=True, max_retries=5)
def validate_import(self, import_id: str) -> None:
    with sync_session() as session:
        item = session.get(Import, UUID(import_id))
        if item is None or item.status not in {
            ImportStatus.VALIDATING,
            ImportStatus.CANCEL_REQUESTED,
        }:
            return

    body = object_store.get_object(item.object_key)["Body"]
    text = io.TextIOWrapper(body, encoding="utf-8-sig", newline="")
    reader = csv.DictReader(text)

    errors: list[dict[str, object]] = []
    batch: list[dict[str, object]] = []
    parsed_bytes = 0
    row_number = 1

    for raw in reader:
        row_number += 1
        parsed_bytes += sum(len(value or "") for value in raw.values())
        parsed, error = parse_row(raw)
        if error:
            errors.append({"row": row_number, "error": error, "raw": raw})
        batch.append({
            "import_id": item.id,
            "tenant_id": item.tenant_id,
            "row_number": row_number,
            **parsed,
            "parsed": parsed,
            "validation_error": error,
            "row_hash": hashlib.sha256(
                json.dumps(raw, sort_keys=True).encode()
            ).digest(),
        })

        if len(batch) == 5000:
            write_validation_batch(item.id, batch, errors, parsed_bytes)
            batch.clear()

    if batch:
        write_validation_batch(item.id, batch, errors, parsed_bytes)

    with sync_session.begin() as session:
        item = session.get(Import, UUID(import_id), with_for_update=True)
        if item.status == ImportStatus.CANCEL_REQUESTED:
            item.status = ImportStatus.CANCELLED
        else:
            item.status = ImportStatus.READY
        item.total_rows = row_number - 1
        item.error_summary = {"samples": errors[:200]}
```

问题点之一是 `errors` 会持续 append 到文件结束，最后才截 200。`parsed_bytes` 统计的是 Python 字符长度，不是对象存储 byte offset。`item` 离开第一个 session 后仍被后面的代码读取；目前 SQLAlchemy 设置 `expire_on_commit=False`，所以测试里没报错。

批量写：

```python
def write_validation_batch(
    import_id: UUID,
    rows: list[dict[str, object]],
    errors: list[dict[str, object]],
    parsed_bytes: int,
) -> None:
    with sync_session.begin() as session:
        session.execute(insert(ImportRow), rows)
        session.execute(
            update(Import)
            .where(Import.id == import_id)
            .values(
                valid_rows=Import.valid_rows + sum(
                    1 for row in rows if row["validation_error"] is None
                ),
                invalid_rows=Import.invalid_rows + sum(
                    1 for row in rows if row["validation_error"] is not None
                ),
                parsed_bytes=parsed_bytes,
                checkpoint_row=rows[-1]["row_number"],
                updated_at=utcnow(),
                error_summary={"samples": errors[:200]},
            )
        )
```

如果任务在事务 COMMIT 成功后、下一行 Python 代码前丢连接，Celery 会重试。重试仍从文件开头开始，第一批 `import_rows` 主键冲突，任务就进入 autoretry；重试耗尽后状态没有统一改为 FAILED。

## apply 任务

用户预览后点击 apply，API 把 READY 改成 APPLYING 并投递：

```python
@celery.task(bind=True, acks_late=True)
def apply_import(self, import_id: str) -> None:
    batch_no = 0
    while True:
        with sync_session.begin() as session:
            item = session.get(Import, UUID(import_id))
            if item.status == ImportStatus.CANCEL_REQUESTED:
                item.status = ImportStatus.CANCELLED
                return
            if item.status != ImportStatus.APPLYING:
                return

            rows = session.execute(
                select(ImportRow)
                .where(
                    ImportRow.import_id == item.id,
                    ImportRow.validation_error.is_(None),
                    ImportRow.applied_at.is_(None),
                )
                .order_by(ImportRow.row_number)
                .limit(2000)
            ).scalars().all()

            if not rows:
                if item.mode == "replace":
                    session.execute(
                        update(Product)
                        .where(
                            Product.tenant_id == item.tenant_id,
                            Product.source_import_id != item.id,
                        )
                        .values(active=False, updated_at=utcnow())
                    )
                item.status = ImportStatus.COMPLETED
                item.completed_at = utcnow()
                return

            for row in rows:
                apply_one_row(session, item, row)
                row.applied_at = utcnow()
                item.applied_rows += 1

        batch_no += 1
```

`apply_one_row`：

```python
def apply_one_row(session: Session, item: Import, row: ImportRow) -> None:
    existing = session.get(Product, (item.tenant_id, row.external_sku))
    if existing is None:
        session.add(Product(
            tenant_id=item.tenant_id,
            external_sku=row.external_sku,
            name=row.name or "",
            price_minor=row.price_minor or 0,
            stock=(row.stock_value if row.stock_value is not None
                   else row.stock_delta or 0),
            source_import_id=item.id,
        ))
        return

    if row.name not in (None, ""):
        existing.name = row.name
    if row.price_minor is not None:
        existing.price_minor = row.price_minor
    if row.stock_delta is not None:
        existing.stock += row.stock_delta
    elif row.stock_value is not None:
        existing.stock = row.stock_value
    existing.active = True
    existing.source_import_id = item.id
    existing.version += 1
    existing.updated_at = utcnow()
```

这里没有 `FOR UPDATE SKIP LOCKED`，也没有 batch owner。理论上一个 import 只跑一个任务，但重复投递时两个 worker 能同时查到同一批 `applied_at IS NULL`。如果是 stock_delta，两边都可能加一次；如果是 value，结果可能相同但 version 加两次。

“最后一行胜出”也还没有真正实现：apply 按 row_number 从小到大，每一行都写；中途用户读取会看到前一条值。replace 完成时的 inactive SQL 用 `source_import_id != item.id`，`NULL != uuid` 为 unknown，因此历史上从未通过 import 写入的商品不会被禁用。

## 取消接口

```python
@router.post("/imports/{import_id}/cancel")
async def cancel_import(...):
    result = await session.execute(
        update(Import)
        .where(
            Import.id == import_id,
            Import.tenant_id == actor.tenant_id,
            Import.status.in_([
                ImportStatus.VALIDATING,
                ImportStatus.READY,
                ImportStatus.APPLYING,
            ]),
        )
        .values(status=ImportStatus.CANCEL_REQUESTED, updated_at=utcnow())
        .returning(Import.status)
    )
    await session.commit()
    if result.scalar_one_or_none() is None:
        raise HTTPException(409, "import cannot be cancelled")
    celery.control.revoke(str(import_id), terminate=False)
```

实际 Celery task id 是自动生成的 UUID，不是 import id，所以 revoke 基本没有效果。validate 每 5000 行提交时不检查取消，apply 每批开始读取一次状态，但查询没有锁，两个 worker 也可能互相覆盖状态。

## 当前测试

```python
def test_patch_updates_product(session, import_factory):
    item = import_factory(mode="patch", rows=[
        {"external_sku": "A-1", "price_minor": 1299},
    ])
    apply_import.run(str(item.id))
    product = session.get(Product, (item.tenant_id, "A-1"))
    assert product.price_minor == 1299

def test_duplicate_sku_last_row_wins(session, import_factory):
    item = import_factory(mode="replace", rows=[
        {"external_sku": "A-1", "stock_value": 5},
        {"external_sku": "A-1", "stock_value": 8},
    ])
    apply_import.run(str(item.id))
    assert load_product(item.tenant_id, "A-1").stock == 8
```

测试文件最多 20 行，任务通过 `.run()` 同步调用，没有 RabbitMQ redelivery、两个 worker、真实对象存储 Range、进程 SIGKILL 或数据库连接断开。测试数据库每次清空，所以也没覆盖 replace 对旧商品的处理。

## 一段故障记录

```text
2026-07-19T02:11:08.412Z INFO task_start task=3a17 import=812e status=VALIDATING worker=import-4
2026-07-19T03:11:09.028Z INFO task_redelivered task=3a17 import=812e worker=import-7 redelivered=true
2026-07-19T03:11:14.602Z WARN batch_insert_conflict import=812e first_row=2 last_row=5001 sqlstate=23505 worker=import-7
2026-07-19T03:11:16.088Z INFO validation_progress import=812e checkpoint_row=18350001 valid=18294428 invalid=5572 worker=import-4
2026-07-19T03:37:45.904Z INFO cancel_requested import=812e actor=901
2026-07-19T03:38:02.117Z INFO validation_progress import=812e checkpoint_row=19125001 valid=19066781 invalid=5820 worker=import-4
2026-07-19T07:41:10.381Z WARN soft_time_limit import=812e checkpoint_row=41710001 worker=import-4
2026-07-19T07:41:10.407Z ERROR task_retry import=812e retry=1 reason=SoftTimeLimitExceeded
2026-07-19T07:41:12.910Z INFO task_start task=91cd import=812e status=CANCEL_REQUESTED worker=import-2
```

另一次 apply 故障里，同一个 import 的两个 worker 在 180ms 内都选中了 rows 60001–62000。事后 `applied_rows` 比有效行数多 2000，部分带 `stock_delta` 的 SKU 恰好多加了一次。还有一批任务显示 `parsed_bytes` 大于 `object_size`，页面因此提前显示 100%，但后台继续跑了很久。

## 第一轮希望你回答什么

请结合上面的具体实现回答：

- 用清晰的状态机说明 API、Celery task、批次事务和 import 总状态之间的关系；
- 分析 99% 卡住、取消不生效、恢复后重复写三种现象的完整故障路径；
- 指出哪些操作必须通过条件更新或数据库唯一性变成幂等，哪些只能接受至少一次再恢复；
- 判断 checkpoint_row、checkpoint_byte、import_rows 和 import_batches 中哪些信息目前可信；
- 给出前三项最小风险修复和必须新增的可重复并发测试，但先别写全部实现。

后续我会继续补充产品口径，我们再逐步讨论断点读取、同文件重复 SKU、批次认领、replace 原子可见性、取消、对象存储完整性、性能、数据清理和上线方案。
