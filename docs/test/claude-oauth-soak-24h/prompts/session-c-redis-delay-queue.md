我在维护一个用 Go 和 Redis 实现的延迟任务队列，主要处理通知、账单重算和第三方 webhook 重试。这个队列最初只服务一个进程，后来扩成了 8 个 worker 实例。最近业务量上来后出现两类问题：少量任务会被执行两次，另一些任务在 worker 崩溃后长时间不再执行。重复并不是完全不能接受，但现在有些重复发生在几百毫秒内，下游幂等键还没来得及落库，所以仍然造成了两次外部调用。

我把设计、Redis key、Lua、Go 实现、测试和一段故障日志都放在下面。请先不要直接换成 Kafka 或其他中间件，我们至少还要维持 Redis 方案一个季度。第一轮我想先得到一份故障路径分析：分别说明“任务重复”“任务永久卡住”“任务提前执行”可能经过哪些状态转换，并指出当前代码里最值得优先修的三个地方。请把 Redis 命令原子性、客户端超时、进程崩溃三个层面分开讲，不确定的地方列出假设。

## 业务和运行条件

生产环境使用 Redis 7.4 Cluster，三个主分片，每个主分片一个副本。Go 版本是 1.23，Redis 客户端是 go-redis/v9。worker 部署在 Kubernetes，同一个 deployment 通常 8 个 pod，滚动发布时最多短暂出现 10 个。

任务类型：

- `send_email`：允许至少一次投递，业务侧以 message key 去重。
- `recalculate_invoice`：必须串行处理同一 invoice，但不同 invoice 可以并行。
- `deliver_webhook`：最多重试 8 次，退避上限 6 小时。
- `sync_profile`：只保留同一用户最新的一次任务，旧任务可以丢弃。

目前队列承诺的是 at-least-once，不承诺 exactly-once。我们仍然希望满足：

1. 任务到期前不能进入 ready。
2. worker 领取任务后有 90 秒 lease。
3. worker 每 30 秒续租一次，最多运行 20 分钟。
4. worker 在 lease 内完成后 ack，任务从 processing 移除。
5. worker 崩溃或网络分区导致 lease 过期，任务应重新进入 ready。
6. 同一个 task id 在 processing 中同一时刻只能有一个有效 owner。
7. publish 使用业务方生成的 task id，重复 publish 必须幂等。
8. Redis Cluster 下 Lua 涉及的 key 必须落在同一个 hash slot。
9. 单个 payload 最大 256KB，正常约 2KB。
10. 队列峰值每秒发布约 1200 个任务，单个 worker 并发执行 32 个。

## Redis key 设计

所有 key 都带队列 hash tag：

```text
dq:{notifications}:scheduled       ZSET  score=due_at_ms, member=task_id
dq:{notifications}:ready           LIST  task_id
dq:{notifications}:processing      ZSET  score=lease_deadline_ms, member=task_id
dq:{notifications}:payload         HASH  task_id -> task JSON
dq:{notifications}:owner           HASH  task_id -> worker_id
dq:{notifications}:attempts        HASH  task_id -> attempt count
dq:{notifications}:dedup           HASH  dedup_key -> task_id
dq:{notifications}:dead            ZSET  score=failed_at_ms, member=task_id
```

应用层认为 scheduled、ready、processing、dead 是互斥状态，但 Redis 没有单独的状态字段。只要 task id 出现在某个集合里，就推断它处于对应状态。payload 在 ack 或 dead 后保留 24 小时，由另一个清理任务删除。

## 任务结构

```go
package delayqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type Task struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	DedupKey  string          `json:"dedup_key,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	DueAt     time.Time       `json:"due_at"`
	CreatedAt time.Time       `json:"created_at"`
	MaxRetry  int             `json:"max_retry"`
}

type ClaimedTask struct {
	Task
	Owner    string
	Attempt  int
	Deadline time.Time
}

var (
	ErrNotOwner = errors.New("worker does not own task")
	ErrNotFound = errors.New("task not found")
)

type Queue struct {
	rdb      redis.UniversalClient
	name     string
	lease    time.Duration
	now      func() time.Time
	key      keys
}

type keys struct {
	scheduled  string
	ready      string
	processing string
	payload    string
	owner      string
	attempts   string
	dedup      string
	dead       string
}

func New(rdb redis.UniversalClient, name string) *Queue {
	tag := "{" + name + "}"
	prefix := "dq:" + tag + ":"
	return &Queue{
		rdb:   rdb,
		name:  name,
		lease: 90 * time.Second,
		now:   time.Now,
		key: keys{
			scheduled:  prefix + "scheduled",
			ready:      prefix + "ready",
			processing: prefix + "processing",
			payload:    prefix + "payload",
			owner:      prefix + "owner",
			attempts:   prefix + "attempts",
			dedup:      prefix + "dedup",
			dead:       prefix + "dead",
		},
	}
}
```

## 发布任务

```go
func (q *Queue) Publish(ctx context.Context, task Task) error {
	if task.ID == "" || task.Type == "" {
		return errors.New("missing task id or type")
	}
	if task.MaxRetry <= 0 {
		task.MaxRetry = 8
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = q.now()
	}
	if task.DueAt.IsZero() {
		task.DueAt = task.CreatedAt
	}

	body, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}

	if task.DedupKey != "" {
		existing, err := q.rdb.HGet(ctx, q.key.dedup, task.DedupKey).Result()
		if err == nil && existing != "" {
			return nil
		}
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
	}

	pipe := q.rdb.TxPipeline()
	pipe.HSetNX(ctx, q.key.payload, task.ID, body)
	pipe.ZAddNX(ctx, q.key.scheduled, redis.Z{
		Score:  float64(task.DueAt.UnixMilli()),
		Member: task.ID,
	})
	if task.DedupKey != "" {
		pipe.HSetNX(ctx, q.key.dedup, task.DedupKey, task.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}
```

业务方有时会用相同 task id、不同 due time 重试 Publish，希望第一次成功的值胜出。对于 `sync_profile`，业务方则会使用不同 task id、相同 dedup key，希望最新任务替换旧任务。当前 Publish 对这两种需求没有做区分。

## 把到期任务搬到 ready

每个 worker 都每 200ms 执行一次 promote，单次最多搬 100 个：

```go
var promoteScript = redis.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1],
  'LIMIT', 0, ARGV[2])
for _, id in ipairs(ids) do
  redis.call('RPUSH', KEYS[2], id)
end
if #ids > 0 then
  redis.call('ZREM', KEYS[1], unpack(ids))
end
return ids
`)

func (q *Queue) Promote(ctx context.Context, limit int64) ([]string, error) {
	result, err := promoteScript.Run(
		ctx,
		q.rdb,
		[]string{q.key.scheduled, q.key.ready},
		q.now().UnixMilli(),
		limit,
	).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("promote due tasks: %w", err)
	}
	return result, nil
}
```

这里使用应用进程的 `time.Now()`，不是 Redis `TIME`。集群节点和 Kubernetes 节点都配置了 NTP，监控显示通常误差在 50ms 内，但发生过节点恢复后短暂偏差 700ms 的记录。

## 领取任务

```go
var claimScript = redis.NewScript(`
local id = redis.call('LPOP', KEYS[1])
if not id then
  return nil
end
if redis.call('HEXISTS', KEYS[2], id) == 0 then
  return {id, false, false}
end
redis.call('ZADD', KEYS[3], ARGV[2], id)
redis.call('HSET', KEYS[4], id, ARGV[1])
local attempt = redis.call('HINCRBY', KEYS[5], id, 1)
local payload = redis.call('HGET', KEYS[2], id)
return {id, payload, tostring(attempt)}
`)

func (q *Queue) Claim(ctx context.Context, workerID string) (*ClaimedTask, error) {
	deadline := q.now().Add(q.lease)
	values, err := claimScript.Run(
		ctx,
		q.rdb,
		[]string{
			q.key.ready,
			q.key.payload,
			q.key.processing,
			q.key.owner,
			q.key.attempts,
		},
		workerID,
		deadline.UnixMilli(),
	).Slice()
	if errors.Is(err, redis.Nil) || len(values) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	if len(values) != 3 {
		return nil, fmt.Errorf("unexpected claim result: %#v", values)
	}

	id, _ := values[0].(string)
	payload, _ := values[1].(string)
	attemptText, _ := values[2].(string)
	if payload == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	attempt, err := strconv.Atoi(attemptText)
	if err != nil {
		return nil, fmt.Errorf("parse attempt %q: %w", attemptText, err)
	}

	var task Task
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		return nil, fmt.Errorf("decode task %s: %w", id, err)
	}
	return &ClaimedTask{
		Task:     task,
		Owner:    workerID,
		Attempt:  attempt,
		Deadline: deadline,
	}, nil
}
```

## 续租、完成和重试

```go
var renewScript = redis.NewScript(`
local owner = redis.call('HGET', KEYS[2], ARGV[1])
if owner ~= ARGV[2] then
  return 0
end
if redis.call('ZSCORE', KEYS[1], ARGV[1]) == false then
  return 0
end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
return 1
`)

func (q *Queue) Renew(ctx context.Context, taskID, workerID string) error {
	deadline := q.now().Add(q.lease).UnixMilli()
	n, err := renewScript.Run(
		ctx,
		q.rdb,
		[]string{q.key.processing, q.key.owner},
		taskID,
		workerID,
		deadline,
	).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotOwner
	}
	return nil
}

var ackScript = redis.NewScript(`
local owner = redis.call('HGET', KEYS[2], ARGV[1])
if owner ~= ARGV[2] then
  return 0
end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[2], ARGV[1])
return 1
`)

func (q *Queue) Ack(ctx context.Context, taskID, workerID string) error {
	n, err := ackScript.Run(
		ctx,
		q.rdb,
		[]string{q.key.processing, q.key.owner},
		taskID,
		workerID,
	).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotOwner
	}
	return nil
}

func (q *Queue) Retry(
	ctx context.Context,
	task ClaimedTask,
	delay time.Duration,
) error {
	if err := q.Ack(ctx, task.ID, task.Owner); err != nil {
		return err
	}
	return q.rdb.ZAdd(ctx, q.key.scheduled, redis.Z{
		Score:  float64(q.now().Add(delay).UnixMilli()),
		Member: task.ID,
	}).Err()
}
```

worker 在 handler 返回 nil 后调用 Ack；返回可重试错误时调用 Retry；返回永久错误或 attempts 超过 MaxRetry 时写入 dead。实际 handler 可能执行 10ms，也可能执行 15 分钟。续租 goroutine 每 30 秒调用 Renew，但 Renew 错误只记日志，不会取消正在执行的 handler。

## 回收过期 lease

```go
var reclaimScript = redis.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1],
  'LIMIT', 0, ARGV[2])
for _, id in ipairs(ids) do
  redis.call('RPUSH', KEYS[2], id)
  redis.call('HDEL', KEYS[3], id)
end
if #ids > 0 then
  redis.call('ZREM', KEYS[1], unpack(ids))
end
return ids
`)

func (q *Queue) Reclaim(ctx context.Context, limit int64) ([]string, error) {
	return reclaimScript.Run(
		ctx,
		q.rdb,
		[]string{q.key.processing, q.key.ready, q.key.owner},
		q.now().UnixMilli(),
		limit,
	).StringSlice()
}
```

每个 worker 每秒执行一次 Reclaim。为了降低扫描量，没有单独 leader。监控里偶尔能看到多个 worker 在同一毫秒执行脚本，但 Lua 本身会在 Redis 单线程顺序执行。

## worker 主循环的简化版本

```go
func (w *Worker) runOne(ctx context.Context) error {
	task, err := w.queue.Claim(ctx, w.id)
	if err != nil || task == nil {
		return err
	}

	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	go w.keepRenewing(renewCtx, task.ID)

	err = w.handler.Handle(ctx, task.Task)
	if err == nil {
		return w.queue.Ack(ctx, task.ID, w.id)
	}
	if IsPermanent(err) || task.Attempt >= task.MaxRetry {
		return w.queue.MoveToDead(ctx, *task, err)
	}
	delay := retryDelay(task.Attempt)
	return w.queue.Retry(ctx, *task, delay)
}

func (w *Worker) keepRenewing(ctx context.Context, taskID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.queue.Renew(ctx, taskID, w.id); err != nil {
				w.log.Warn("lease renew failed", "task_id", taskID, "error", err)
			}
		}
	}
}
```

外部 webhook handler 的流程是：先发 HTTP 请求，收到 2xx 后把 delivery result 写数据库，最后返回 nil。下游支持 `Idempotency-Key`，但旧客户没有实现。HTTP 客户端超时 20 秒。Redis 命令超时 2 秒。

## 当前测试

测试使用单节点 Redis 容器，时间通过替换 `q.now` 控制，但测试从未暂停 Redis 网络：

```go
func TestPublishPromoteClaimAck(t *testing.T) {
	q := newTestQueue(t)
	now := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	task := Task{
		ID:       "task-1",
		Type:     "send_email",
		Payload:  json.RawMessage(`{"to":"test@example.invalid"}`),
		DueAt:    now.Add(time.Minute),
		MaxRetry: 3,
	}
	require.NoError(t, q.Publish(context.Background(), task))

	ids, err := q.Promote(context.Background(), 100)
	require.NoError(t, err)
	require.Empty(t, ids)

	now = now.Add(time.Minute)
	ids, err = q.Promote(context.Background(), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"task-1"}, ids)

	claimed, err := q.Claim(context.Background(), "worker-a")
	require.NoError(t, err)
	require.Equal(t, "task-1", claimed.ID)
	require.NoError(t, q.Ack(context.Background(), "task-1", "worker-a"))
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	q := newTestQueue(t)
	now := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	publishDueTask(t, q, "task-2", now)
	_, _ = q.Promote(context.Background(), 100)
	first, _ := q.Claim(context.Background(), "worker-a")
	require.Equal(t, "task-2", first.ID)

	now = now.Add(91 * time.Second)
	ids, err := q.Reclaim(context.Background(), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"task-2"}, ids)
	second, err := q.Claim(context.Background(), "worker-b")
	require.NoError(t, err)
	require.Equal(t, 2, second.Attempt)
}
```

## 故障日志一：疑似重复执行

```text
2026-07-22T03:10:00.004Z worker-2 claim.ok task=wh-991 owner=worker-2 attempt=1 lease=03:11:30.002Z
2026-07-22T03:10:00.011Z worker-2 webhook.start task=wh-991 target=customer-17
2026-07-22T03:10:30.008Z worker-2 lease.renew.start task=wh-991
2026-07-22T03:10:32.014Z worker-2 lease.renew.error task=wh-991 error="context deadline exceeded"
2026-07-22T03:11:00.009Z worker-2 lease.renew.start task=wh-991
2026-07-22T03:11:02.015Z worker-2 lease.renew.error task=wh-991 error="context deadline exceeded"
2026-07-22T03:11:30.108Z worker-6 reclaim.ok task=wh-991 old_owner=worker-2
2026-07-22T03:11:30.114Z worker-6 claim.ok task=wh-991 owner=worker-6 attempt=2 lease=03:13:00.112Z
2026-07-22T03:11:30.121Z worker-6 webhook.start task=wh-991 target=customer-17
2026-07-22T03:11:30.208Z worker-2 webhook.response task=wh-991 status=204 elapsed_ms=90197
2026-07-22T03:11:30.216Z worker-2 delivery.persisted task=wh-991
2026-07-22T03:11:30.222Z worker-2 ack.error task=wh-991 error="worker does not own task"
2026-07-22T03:11:30.340Z worker-6 webhook.response task=wh-991 status=204 elapsed_ms=219
2026-07-22T03:11:30.347Z worker-6 delivery.persisted task=wh-991
2026-07-22T03:11:30.352Z worker-6 ack.ok task=wh-991
```

这次 worker-2 没有崩溃，只是在两次 Redis 网络超时后仍继续执行 handler。客户的 webhook 端没有实现幂等键。

## 故障日志二：疑似永久卡住

```text
2026-07-22T04:20:10.001Z publisher publish.start task=mail-882 due=04:20:10.000Z dedup=welcome:552
2026-07-22T04:20:12.006Z publisher publish.error task=mail-882 error="i/o timeout"
2026-07-22T04:20:12.210Z publisher publish.retry task=mail-882 due=04:20:10.000Z
2026-07-22T04:20:12.215Z publisher publish.done task=mail-882
2026-07-22T04:20:12.400Z metrics payload_exists=1 scheduled_score_missing=1 ready_contains=0 processing_contains=0
```

Redis 慢日志显示第一次 pipeline 只确认了部分响应，客户端无法判断三个命令是否都执行。第二次 Publish 因 `dedup` 已存在直接返回 nil，没有检查 scheduled 是否存在。

## 故障日志三：疑似提前执行

```text
2026-07-22T05:00:00.100Z publisher publish.done task=invoice-73 due=05:00:05.000Z
2026-07-22T05:00:04.420Z worker-4 promote.ok task=invoice-73 redis_node=10.0.8.14 app_clock_offset_ms=+690
2026-07-22T05:00:04.426Z worker-4 claim.ok task=invoice-73
2026-07-22T05:00:04.431Z worker-4 invoice.start task=invoice-73
```

监控同时显示该 Kubernetes 节点的系统时间比 Redis 主节点快约 700ms。业务要求宁可晚一点，也不能提前执行。

请先按前面的要求分析三类故障路径和三个最高优先级问题。我们后续再讨论 Lua 修改、fencing token、测试和迁移步骤。
