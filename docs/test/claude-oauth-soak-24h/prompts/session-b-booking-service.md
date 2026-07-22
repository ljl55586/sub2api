我在帮一个小团队维护会议室预约服务。系统不大，前端是内部网页，后端是 Node.js 24 + TypeScript，数据库用 PostgreSQL 17。最近增加了“先占用 10 分钟再确认”的功能，之后偶尔会出现同一个房间同一时段被两个人都确认成功，或者用户取消后库存没有及时释放。问题只在并发请求和任务重试时出现，普通单元测试一直是绿的。

下面是业务规则、表结构、当前实现、测试和一段脱敏日志。先不要急着给完整重构代码。我更希望你先把一次预约从 hold、confirm、expire 到 cancel 的状态变化画清楚，然后指出当前实现里最危险的三个竞态。请区分“数据库隔离级别本身能解决的问题”和“必须靠业务幂等或约束解决的问题”。如果现有需求存在互相冲突，也请先指出，不要默认选一种解释。

## 业务规则

系统以 30 分钟为最小预约单位，时间统一存 UTC，页面按用户时区显示。一次预约可以连续占用多个时间片，例如 09:00–10:30 会占用三个 slot。

预约状态如下：

- `holding`：用户刚提交，暂时占用时间片，10 分钟后未确认则过期。
- `confirmed`：用户确认，正式预约成功。
- `cancelled`：用户或管理员主动取消。
- `expired`：hold 到期后由后台任务释放。

已确认规则：

1. 同一个房间的重叠时间段最多只能存在一条 `holding` 或 `confirmed` 预约。
2. 创建 hold 的接口支持客户端传入 `Idempotency-Key`，同一个用户用同一个 key 重试必须拿到同一预约。
3. confirm 也可能因为移动网络超时而重试；重复 confirm 应返回第一次成功的结果。
4. cancel 和 expire 同时发生时，最终状态允许是 `cancelled` 或 `expired`，但时间片必须释放且只能释放一次。
5. confirm 和 expire 同时发生时，以数据库中 `expires_at` 为准：事务开始时已经过期就不能确认。
6. 管理员可以把确认后的预约延长，但第一版暂时不做跨房间移动。
7. 后台过期任务至少投递一次，可能重复执行，也可能在执行一半后进程退出。
8. 不允许用全局进程锁，因为服务部署了 6 个实例。
9. PostgreSQL 默认隔离级别仍是 `READ COMMITTED`，除非某个事务显式调整。
10. 接口 p95 目标是 200ms，不能把所有房间的预约串行化。

## 数据库表

```sql
CREATE TYPE booking_status AS ENUM (
  'holding',
  'confirmed',
  'cancelled',
  'expired'
);

CREATE TABLE rooms (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name          text NOT NULL,
  timezone      text NOT NULL DEFAULT 'Asia/Shanghai',
  enabled       boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bookings (
  id                uuid PRIMARY KEY,
  room_id           bigint NOT NULL REFERENCES rooms(id),
  user_id           bigint NOT NULL,
  idempotency_key   text NOT NULL,
  starts_at         timestamptz NOT NULL,
  ends_at           timestamptz NOT NULL,
  status            booking_status NOT NULL,
  expires_at        timestamptz,
  version           integer NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (starts_at < ends_at),
  CHECK (
    (status = 'holding' AND expires_at IS NOT NULL)
    OR status <> 'holding'
  )
);

CREATE UNIQUE INDEX bookings_user_idempotency_uq
  ON bookings(user_id, idempotency_key);

CREATE INDEX bookings_room_time_idx
  ON bookings(room_id, starts_at, ends_at);

CREATE TABLE booking_events (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  booking_id     uuid NOT NULL REFERENCES bookings(id),
  event_type     text NOT NULL,
  operation_id   uuid NOT NULL,
  payload        jsonb NOT NULL DEFAULT '{}',
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX booking_events_operation_uq
  ON booking_events(operation_id, event_type);
```

最初有人提议用 PostgreSQL exclusion constraint，但当时团队担心 enum 状态过滤和历史数据迁移太复杂，所以没有加。现在的冲突检测是在应用里查一次。

## 当前领域类型

```ts
export type BookingStatus =
  | "holding"
  | "confirmed"
  | "cancelled"
  | "expired";

export interface Booking {
  id: string;
  roomId: bigint;
  userId: bigint;
  idempotencyKey: string;
  startsAt: Date;
  endsAt: Date;
  status: BookingStatus;
  expiresAt: Date | null;
  version: number;
  createdAt: Date;
  updatedAt: Date;
}

export interface HoldCommand {
  roomId: bigint;
  userId: bigint;
  idempotencyKey: string;
  startsAt: Date;
  endsAt: Date;
}

export interface ConfirmCommand {
  bookingId: string;
  userId: bigint;
  operationId: string;
}
```

## 数据库封装

项目里没有 ORM，只用了 `pg`。事务助手如下：

```ts
import { Pool, PoolClient, QueryResultRow } from "pg";

export class Database {
  constructor(private readonly pool: Pool) {}

  async query<T extends QueryResultRow>(text: string, values: unknown[] = []) {
    return this.pool.query<T>(text, values);
  }

  async transaction<T>(fn: (client: PoolClient) => Promise<T>): Promise<T> {
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      const value = await fn(client);
      await client.query("COMMIT");
      return value;
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }
}
```

## 创建 hold

```ts
import { randomUUID } from "node:crypto";
import type { PoolClient } from "pg";

const HOLD_TTL_MS = 10 * 60 * 1000;

export class BookingService {
  constructor(
    private readonly db: Database,
    private readonly clock: () => Date = () => new Date(),
  ) {}

  async createHold(command: HoldCommand): Promise<Booking> {
    const existing = await this.db.query<BookingRow>(
      `SELECT * FROM bookings
       WHERE user_id = $1 AND idempotency_key = $2`,
      [command.userId, command.idempotencyKey],
    );
    if (existing.rowCount) {
      return mapBooking(existing.rows[0]);
    }

    const conflict = await this.db.query<{ id: string }>(
      `SELECT id FROM bookings
       WHERE room_id = $1
         AND status IN ('holding', 'confirmed')
         AND starts_at < $3
         AND ends_at > $2
       LIMIT 1`,
      [command.roomId, command.startsAt, command.endsAt],
    );
    if (conflict.rowCount) {
      throw new ConflictError("room is no longer available");
    }

    const now = this.clock();
    const expiresAt = new Date(now.getTime() + HOLD_TTL_MS);
    const id = randomUUID();

    const inserted = await this.db.query<BookingRow>(
      `INSERT INTO bookings (
         id, room_id, user_id, idempotency_key,
         starts_at, ends_at, status, expires_at
       ) VALUES ($1, $2, $3, $4, $5, $6, 'holding', $7)
       RETURNING *`,
      [
        id,
        command.roomId,
        command.userId,
        command.idempotencyKey,
        command.startsAt,
        command.endsAt,
        expiresAt,
      ],
    );

    await this.db.query(
      `INSERT INTO booking_events
         (booking_id, event_type, operation_id, payload)
       VALUES ($1, 'hold_created', $2, $3)`,
      [id, randomUUID(), JSON.stringify({ expiresAt })],
    );

    return mapBooking(inserted.rows[0]);
  }
```

这里为了简化示例省略了一部分参数校验。实际 controller 已经验证时间按 30 分钟对齐、时长不超过 8 小时、开始时间不早于当前时间 5 分钟。

## confirm

```ts
  async confirm(command: ConfirmCommand): Promise<Booking> {
    return this.db.transaction(async (client) => {
      const result = await client.query<BookingRow>(
        `SELECT * FROM bookings WHERE id = $1`,
        [command.bookingId],
      );
      if (!result.rowCount) {
        throw new NotFoundError("booking not found");
      }

      const booking = mapBooking(result.rows[0]);
      if (booking.userId !== command.userId) {
        throw new ForbiddenError("booking belongs to another user");
      }
      if (booking.status === "confirmed") {
        return booking;
      }
      if (booking.status !== "holding") {
        throw new ConflictError(`cannot confirm ${booking.status} booking`);
      }
      if (!booking.expiresAt || booking.expiresAt <= this.clock()) {
        throw new ConflictError("hold has expired");
      }

      const updated = await client.query<BookingRow>(
        `UPDATE bookings
         SET status = 'confirmed', expires_at = NULL,
             version = version + 1, updated_at = now()
         WHERE id = $1
         RETURNING *`,
        [booking.id],
      );

      await client.query(
        `INSERT INTO booking_events
           (booking_id, event_type, operation_id, payload)
         VALUES ($1, 'confirmed', $2, '{}')
         ON CONFLICT (operation_id, event_type) DO NOTHING`,
        [booking.id, command.operationId],
      );

      return mapBooking(updated.rows[0]);
    });
  }
```

## cancel

```ts
  async cancel(
    bookingId: string,
    actorUserId: bigint,
    operationId: string,
  ): Promise<Booking> {
    return this.db.transaction(async (client) => {
      const current = await client.query<BookingRow>(
        `SELECT * FROM bookings WHERE id = $1`,
        [bookingId],
      );
      if (!current.rowCount) {
        throw new NotFoundError("booking not found");
      }

      const booking = mapBooking(current.rows[0]);
      if (booking.userId !== actorUserId) {
        throw new ForbiddenError("booking belongs to another user");
      }
      if (booking.status === "cancelled") {
        return booking;
      }
      if (booking.status === "expired") {
        throw new ConflictError("booking already expired");
      }

      const updated = await client.query<BookingRow>(
        `UPDATE bookings
         SET status = 'cancelled', expires_at = NULL,
             version = version + 1, updated_at = now()
         WHERE id = $1
         RETURNING *`,
        [bookingId],
      );

      await client.query(
        `INSERT INTO booking_events
           (booking_id, event_type, operation_id, payload)
         VALUES ($1, 'cancelled', $2, '{}')
         ON CONFLICT (operation_id, event_type) DO NOTHING`,
        [bookingId, operationId],
      );
      return mapBooking(updated.rows[0]);
    });
  }
```

## 后台过期任务

任务每分钟运行一次，每批处理 200 条。六个应用实例都会启动 worker。

```ts
  async expireHolds(batchSize = 200): Promise<number> {
    const candidates = await this.db.query<{ id: string }>(
      `SELECT id FROM bookings
       WHERE status = 'holding'
         AND expires_at <= now()
       ORDER BY expires_at ASC
       LIMIT $1`,
      [batchSize],
    );

    let expired = 0;
    for (const candidate of candidates.rows) {
      const result = await this.db.query<BookingRow>(
        `UPDATE bookings
         SET status = 'expired', expires_at = NULL,
             version = version + 1, updated_at = now()
         WHERE id = $1
         RETURNING *`,
        [candidate.id],
      );
      if (!result.rowCount) continue;

      await this.db.query(
        `INSERT INTO booking_events
           (booking_id, event_type, operation_id, payload)
         VALUES ($1, 'expired', $2, '{}')`,
        [candidate.id, randomUUID()],
      );
      expired++;
    }
    return expired;
  }
}
```

## 映射代码

```ts
interface BookingRow {
  id: string;
  room_id: string;
  user_id: string;
  idempotency_key: string;
  starts_at: Date;
  ends_at: Date;
  status: BookingStatus;
  expires_at: Date | null;
  version: number;
  created_at: Date;
  updated_at: Date;
}

function mapBooking(row: BookingRow): Booking {
  return {
    id: row.id,
    roomId: BigInt(row.room_id),
    userId: BigInt(row.user_id),
    idempotencyKey: row.idempotency_key,
    startsAt: row.starts_at,
    endsAt: row.ends_at,
    status: row.status,
    expiresAt: row.expires_at,
    version: row.version,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}
```

## 当前测试方式

大部分测试把 `Database` mock 掉，所以不会真正产生并发事务。只有下面这个集成测试连接临时 PostgreSQL，但请求仍然是串行执行：

```ts
test("a confirmed booking cannot be held again", async () => {
  const first = await service.createHold({
    roomId: 1n,
    userId: 101n,
    idempotencyKey: "key-a",
    startsAt: new Date("2026-08-03T01:00:00Z"),
    endsAt: new Date("2026-08-03T02:00:00Z"),
  });
  await service.confirm({
    bookingId: first.id,
    userId: 101n,
    operationId: "10000000-0000-4000-8000-000000000001",
  });

  await expect(service.createHold({
    roomId: 1n,
    userId: 102n,
    idempotencyKey: "key-b",
    startsAt: new Date("2026-08-03T01:30:00Z"),
    endsAt: new Date("2026-08-03T02:30:00Z"),
  })).rejects.toThrow("room is no longer available");
});

test("create hold is idempotent", async () => {
  const command = {
    roomId: 1n,
    userId: 101n,
    idempotencyKey: "same-key",
    startsAt: new Date("2026-08-03T03:00:00Z"),
    endsAt: new Date("2026-08-03T04:00:00Z"),
  };
  const a = await service.createHold(command);
  const b = await service.createHold(command);
  expect(b.id).toBe(a.id);
});
```

## 脱敏故障日志

实例 `api-2` 和 `api-5` 同时收到同一个房间重叠时间段的请求：

```text
2026-07-22T01:14:03.112Z api-2 hold.start user=801 room=12 key=mobile-f447 range=03:00..04:00
2026-07-22T01:14:03.114Z api-5 hold.start user=913 room=12 key=web-a821 range=03:30..04:30
2026-07-22T01:14:03.119Z api-2 hold.conflict_check rows=0 elapsed_ms=4
2026-07-22T01:14:03.121Z api-5 hold.conflict_check rows=0 elapsed_ms=5
2026-07-22T01:14:03.127Z api-5 hold.inserted booking=47ce... expires_at=01:24:03.121Z
2026-07-22T01:14:03.129Z api-2 hold.inserted booking=9ac0... expires_at=01:24:03.119Z
2026-07-22T01:14:05.991Z api-2 confirm.start booking=9ac0... op=1f2d...
2026-07-22T01:14:06.010Z api-5 confirm.start booking=47ce... op=9b6a...
2026-07-22T01:14:06.025Z api-2 confirm.done booking=9ac0... version=2
2026-07-22T01:14:06.032Z api-5 confirm.done booking=47ce... version=2
```

另一次 confirm 和 expire 竞争：

```text
2026-07-22T02:30:00.001Z worker-3 expire.scan candidate=ad11... expires_at=02:29:59.900Z
2026-07-22T02:30:00.008Z api-1 confirm.start booking=ad11... op=8041...
2026-07-22T02:30:00.014Z api-1 confirm.loaded booking=ad11... status=holding expires_at=02:30:00.050Z
2026-07-22T02:30:00.019Z worker-3 expire.updated booking=ad11... status=expired version=2
2026-07-22T02:30:00.024Z api-1 confirm.updated booking=ad11... status=confirmed version=3
2026-07-22T02:30:00.031Z worker-5 expire.updated booking=ad11... status=expired version=4
```

还有一次客户端对 createHold 使用同一个 idempotency key 并发重试，一个请求成功，另一个得到唯一键冲突，controller 把它转换成了 500；理论上第二个请求应该读取并返回第一条记录。

目前团队在讨论三种方案：给房间行加 `SELECT ... FOR UPDATE`；使用 advisory lock；或者给活动预约增加 exclusion constraint。请先按前面要求分析状态变化和三个竞态，不需要一上来就替我们选最终方案。
