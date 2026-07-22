我最近在维护一个 Go 写的小型网关日志分析工具，主要用来把下游请求、上游转发和最终 usage 记录串起来，方便值班时判断某个模型请求到底慢在哪里。最近碰到一个比较难复现的问题：日志跨天、出现乱序行、同一个请求发生上游重试，或者进程在请求中途重启时，按 session 聚合出来的 token、费用和耗时有时会不准确。我把背景、约束、现有实现、测试和一段脱敏日志都放在下面。

先不要直接重写全部代码，也不用一次给完整实现。请先像做 code review 一样指出最可能的三个根因，按影响从高到低排序；每个根因都说明它会被哪类输入触发、当前测试为什么没有覆盖，以及你还需要我确认什么信息。如果你认为某个现象只是日志本身无法推导，也请明确说出来，不要擅自补数据。

## 项目背景

工具叫 `gateway-audit`，运行环境是 Go 1.23、Linux amd64。生产服务器上的应用日志由 Docker JSON log driver 写入文件，再由一个采集进程把其中的 `log` 字段解出来形成普通 JSON Lines。分析器只读取这些已经解包的 JSON Lines，不需要处理 Docker 外层格式。

一次正常请求通常会出现四类事件：

1. `client_received`：网关收到下游请求。
2. `upstream_attempt`：准备向上游发送一次请求。重试时可能出现多条。
3. `upstream_result`：某次上游调用完成，可能成功也可能失败。
4. `usage_committed`：成功响应的 token 和费用已经写入数据库。

所有事件都有 `request_id`。`session_id` 只保证在一个用户对话中稳定；不同用户可能在不同时间使用相同的短 session 名称，所以跨文件聚合时还要考虑 `api_key_id`。同一个请求的日志可能来自两个应用实例。实例时钟通过 NTP 同步，但极端情况下仍可能相差 1–2 秒。日志采集器按文件读取，不保证合并后的行严格按时间排序。

业务方现在最关心下面几个指标：

- 请求总数、成功数和最终失败数；
- 每个请求真正采用的最后一次上游 attempt；
- 从 `client_received` 到成功 `upstream_result` 的端到端耗时；
- 上游服务端耗时和网关额外耗时；
- input、output、cache creation、cache read tokens；
- 标准费用；
- 每个会话的首个请求时间、最后请求时间和请求数量。

## 已确认的约束

- 单个日志文件最大 20GB，不能一次性读入内存。
- 一天大约 3000 万行，正常情况下 request_id 的生命周期不超过 15 分钟。
- 进程允许最多使用 512MB 内存。
- 只能使用 Go 标准库，暂时不能引入数据库或第三方流处理框架。
- 日志里可能出现未知事件类型，必须忽略但记录计数。
- JSON 损坏行不能让整个任务失败；需要记录行号和错误数量。
- 输出按日期分区，每天一个汇总 JSON 文件。
- 日期以 `Asia/Shanghai` 自然日计算，但原始时间戳都是 RFC3339 UTC。
- `usage_committed` 可能晚于 `upstream_result` 几秒，也可能因为数据库故障完全缺失。
- 一次成功请求即使此前有失败 attempt，也只能计为一次成功，失败 attempt 单独进入 retry 统计。
- 如果同一个 `usage_committed` 因日志重复采集出现两次，不能重复计费。
- 暂时不要求把状态落盘恢复；单次分析可以从一个指定日期前 30 分钟开始扫描，覆盖跨日请求。

## 事件格式

```json
{"ts":"2026-07-21T15:59:59.812Z","instance":"gw-a","event":"client_received","request_id":"req-1001","api_key_id":17,"session_id":"green-42","model":"claude-opus-4-8"}
{"ts":"2026-07-21T16:00:00.018Z","instance":"gw-a","event":"upstream_attempt","request_id":"req-1001","attempt":1,"upstream_request_id":"up-701"}
{"ts":"2026-07-21T16:00:01.420Z","instance":"gw-a","event":"upstream_result","request_id":"req-1001","attempt":1,"upstream_request_id":"up-701","status":529,"upstream_ms":1398}
{"ts":"2026-07-21T16:00:02.035Z","instance":"gw-b","event":"upstream_attempt","request_id":"req-1001","attempt":2,"upstream_request_id":"up-702"}
{"ts":"2026-07-21T16:00:04.630Z","instance":"gw-b","event":"upstream_result","request_id":"req-1001","attempt":2,"upstream_request_id":"up-702","status":200,"upstream_ms":2588}
{"ts":"2026-07-21T16:00:04.711Z","instance":"gw-b","event":"usage_committed","request_id":"req-1001","input_tokens":321,"output_tokens":188,"cache_creation_tokens":5120,"cache_read_tokens":0,"standard_cost":0.041825}
```

不是所有事件都包含所有字段。`attempt` 从 1 开始，但旧版本偶尔没有写这个字段。成功状态目前定义为 200–299。`upstream_ms` 来自应用内部计时，不受实例时钟差影响。端到端耗时只能用事件时间戳计算。

## 当前实现

目录结构：

```text
cmd/gateway-audit/main.go
internal/audit/event.go
internal/audit/aggregate.go
internal/audit/report.go
internal/audit/aggregate_test.go
```

`event.go`：

```go
package audit

import "time"

type Event struct {
	TS                time.Time `json:"ts"`
	Instance          string    `json:"instance"`
	Event             string    `json:"event"`
	RequestID         string    `json:"request_id"`
	APIKeyID          int64     `json:"api_key_id"`
	SessionID         string    `json:"session_id"`
	Model             string    `json:"model"`
	Attempt           int       `json:"attempt"`
	UpstreamRequestID string    `json:"upstream_request_id"`
	Status            int       `json:"status"`
	UpstreamMS        int64     `json:"upstream_ms"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	CacheCreateTokens int64     `json:"cache_creation_tokens"`
	CacheReadTokens   int64     `json:"cache_read_tokens"`
	StandardCost      float64   `json:"standard_cost"`
}

type RequestState struct {
	RequestID    string
	APIKeyID     int64
	SessionID    string
	Model        string
	ReceivedAt   time.Time
	LastAt       time.Time
	LastAttempt  int
	LastStatus   int
	UpstreamMS   int64
	InputTokens  int64
	OutputTokens int64
	CacheCreate  int64
	CacheRead    int64
	Cost         float64
	Committed    bool
}

type SessionKey struct {
	APIKeyID  int64
	SessionID string
}

type SessionSummary struct {
	APIKeyID      int64     `json:"api_key_id"`
	SessionID     string    `json:"session_id"`
	FirstRequest  time.Time `json:"first_request"`
	LastRequest   time.Time `json:"last_request"`
	Requests      int64     `json:"requests"`
	Success       int64     `json:"success"`
	Failed        int64     `json:"failed"`
	Retries       int64     `json:"retries"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CacheCreate   int64     `json:"cache_creation_tokens"`
	CacheRead     int64     `json:"cache_read_tokens"`
	StandardCost  float64   `json:"standard_cost"`
	TotalE2EMS    int64     `json:"total_e2e_ms"`
	TotalUpstream int64     `json:"total_upstream_ms"`
}
```

`aggregate.go`：

```go
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

type Aggregator struct {
	requests map[string]*RequestState
	sessions map[SessionKey]*SessionSummary
	badLines int64
	unknown  map[string]int64
}

func NewAggregator() *Aggregator {
	return &Aggregator{
		requests: make(map[string]*RequestState),
		sessions: make(map[SessionKey]*SessionSummary),
		unknown:  make(map[string]int64),
	}
}

func (a *Aggregator) Scan(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			a.badLines++
			continue
		}
		if e.RequestID == "" {
			a.badLines++
			continue
		}
		a.Apply(e)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan log: %w", err)
	}
	return nil
}

func (a *Aggregator) Apply(e Event) {
	st := a.requests[e.RequestID]
	if st == nil {
		st = &RequestState{RequestID: e.RequestID}
		a.requests[e.RequestID] = st
	}
	st.LastAt = e.TS

	switch e.Event {
	case "client_received":
		st.APIKeyID = e.APIKeyID
		st.SessionID = e.SessionID
		st.Model = e.Model
		st.ReceivedAt = e.TS
	case "upstream_attempt":
		st.LastAttempt = e.Attempt
	case "upstream_result":
		st.LastAttempt = e.Attempt
		st.LastStatus = e.Status
		st.UpstreamMS = e.UpstreamMS
	case "usage_committed":
		st.InputTokens = e.InputTokens
		st.OutputTokens = e.OutputTokens
		st.CacheCreate = e.CacheCreateTokens
		st.CacheRead = e.CacheReadTokens
		st.Cost = e.StandardCost
		st.Committed = true
		a.finish(st)
	default:
		a.unknown[e.Event]++
	}
}

func (a *Aggregator) finish(st *RequestState) {
	key := SessionKey{APIKeyID: st.APIKeyID, SessionID: st.SessionID}
	s := a.sessions[key]
	if s == nil {
		s = &SessionSummary{
			APIKeyID:     st.APIKeyID,
			SessionID:    st.SessionID,
			FirstRequest: st.ReceivedAt,
			LastRequest:  st.ReceivedAt,
		}
		a.sessions[key] = s
	}

	s.Requests++
	if st.LastStatus >= 200 && st.LastStatus < 300 {
		s.Success++
	} else {
		s.Failed++
	}
	if st.LastAttempt > 1 {
		s.Retries += int64(st.LastAttempt - 1)
	}
	s.InputTokens += st.InputTokens
	s.OutputTokens += st.OutputTokens
	s.CacheCreate += st.CacheCreate
	s.CacheRead += st.CacheRead
	s.StandardCost += st.Cost
	s.TotalUpstream += st.UpstreamMS
	s.TotalE2EMS += st.LastAt.Sub(st.ReceivedAt).Milliseconds()
	if st.ReceivedAt.Before(s.FirstRequest) {
		s.FirstRequest = st.ReceivedAt
	}
	if st.ReceivedAt.After(s.LastRequest) {
		s.LastRequest = st.ReceivedAt
	}
	delete(a.requests, st.RequestID)
}

func (a *Aggregator) ExpireBefore(cutoff time.Time) {
	for id, st := range a.requests {
		if st.LastAt.Before(cutoff) {
			if !st.Committed {
				a.finish(st)
			}
			delete(a.requests, id)
		}
	}
}

func (a *Aggregator) Summaries() []SessionSummary {
	out := make([]SessionSummary, 0, len(a.sessions))
	for _, summary := range a.sessions {
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].APIKeyID != out[j].APIKeyID {
			return out[i].APIKeyID < out[j].APIKeyID
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
```

`report.go`：

```go
package audit

import (
	"encoding/json"
	"io"
)

type Report struct {
	Date            string           `json:"date"`
	Sessions        []SessionSummary `json:"sessions"`
	MalformedLines  int64            `json:"malformed_lines"`
	UnknownByType   map[string]int64  `json:"unknown_by_type"`
	IncompleteCount int64            `json:"incomplete_count"`
}

func WriteReport(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
```

## 当前测试

```go
package audit

import (
	"strings"
	"testing"
	"time"
)

func TestSingleSuccessfulRequest(t *testing.T) {
	input := strings.Join([]string{
		`{"ts":"2026-07-21T08:00:00Z","event":"client_received","request_id":"r1","api_key_id":7,"session_id":"s1","model":"m"}`,
		`{"ts":"2026-07-21T08:00:01Z","event":"upstream_attempt","request_id":"r1","attempt":1}`,
		`{"ts":"2026-07-21T08:00:03Z","event":"upstream_result","request_id":"r1","attempt":1,"status":200,"upstream_ms":1900}`,
		`{"ts":"2026-07-21T08:00:04Z","event":"usage_committed","request_id":"r1","input_tokens":10,"output_tokens":5,"standard_cost":0.01}`,
	}, "\n")
	a := NewAggregator()
	if err := a.Scan(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	got := a.Summaries()
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Requests != 1 || got[0].Success != 1 {
		t.Fatalf("summary=%+v", got[0])
	}
}

func TestMalformedLineIsSkipped(t *testing.T) {
	input := "not json\n" +
		`{"ts":"2026-07-21T08:00:00Z","event":"client_received","request_id":"r1"}`
	a := NewAggregator()
	if err := a.Scan(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if a.badLines != 1 {
		t.Fatalf("badLines=%d", a.badLines)
	}
}

func TestExpireIncompleteRequest(t *testing.T) {
	a := NewAggregator()
	a.Apply(Event{
		TS:        time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC),
		Event:     "client_received",
		RequestID: "r1",
		APIKeyID:  7,
		SessionID: "s1",
	})
	a.ExpireBefore(time.Date(2026, 7, 21, 8, 20, 0, 0, time.UTC))
	got := a.Summaries()
	if len(got) != 1 || got[0].Failed != 1 {
		t.Fatalf("summary=%+v", got)
	}
}
```

## 一段出现问题的脱敏日志

下面这些行保持了采集后的实际顺序。注意它不是按 `ts` 排序的。

```jsonl
{"ts":"2026-07-21T15:59:59.812Z","instance":"gw-a","event":"client_received","request_id":"req-1001","api_key_id":17,"session_id":"green-42","model":"claude-opus-4-8"}
{"ts":"2026-07-21T16:00:00.018Z","instance":"gw-a","event":"upstream_attempt","request_id":"req-1001","attempt":1,"upstream_request_id":"up-701"}
{"ts":"2026-07-21T16:00:01.420Z","instance":"gw-a","event":"upstream_result","request_id":"req-1001","attempt":1,"upstream_request_id":"up-701","status":529,"upstream_ms":1398}
{"ts":"2026-07-21T16:00:02.035Z","instance":"gw-b","event":"upstream_attempt","request_id":"req-1001","attempt":2,"upstream_request_id":"up-702"}
{"ts":"2026-07-21T16:00:04.711Z","instance":"gw-b","event":"usage_committed","request_id":"req-1001","input_tokens":321,"output_tokens":188,"cache_creation_tokens":5120,"cache_read_tokens":0,"standard_cost":0.041825}
{"ts":"2026-07-21T16:00:04.630Z","instance":"gw-b","event":"upstream_result","request_id":"req-1001","attempt":2,"upstream_request_id":"up-702","status":200,"upstream_ms":2588}
{"ts":"2026-07-21T16:00:10.100Z","instance":"gw-a","event":"client_received","request_id":"req-1002","api_key_id":17,"session_id":"green-42","model":"claude-opus-4-8"}
{"ts":"2026-07-21T16:00:10.230Z","instance":"gw-a","event":"upstream_attempt","request_id":"req-1002","attempt":1,"upstream_request_id":"up-703"}
{"ts":"2026-07-21T16:00:11.500Z","instance":"gw-a","event":"upstream_result","request_id":"req-1002","attempt":1,"upstream_request_id":"up-703","status":200,"upstream_ms":1250}
{"ts":"2026-07-21T16:00:11.780Z","instance":"gw-a","event":"usage_committed","request_id":"req-1002","input_tokens":240,"output_tokens":91,"cache_creation_tokens":0,"cache_read_tokens":5120,"standard_cost":0.005875}
{"ts":"2026-07-21T16:00:11.780Z","instance":"gw-a","event":"usage_committed","request_id":"req-1002","input_tokens":240,"output_tokens":91,"cache_creation_tokens":0,"cache_read_tokens":5120,"standard_cost":0.005875}
{"ts":"2026-07-21T16:00:20.000Z","instance":"gw-b","event":"client_received","request_id":"req-1003","api_key_id":18,"session_id":"green-42","model":"claude-opus-4-8"}
{"ts":"2026-07-21T16:00:20.400Z","instance":"gw-b","event":"upstream_attempt","request_id":"req-1003","upstream_request_id":"up-704"}
{"ts":"2026-07-21T16:00:23.200Z","instance":"gw-b","event":"upstream_result","request_id":"req-1003","status":200,"upstream_ms":2780}
{"ts":"2026-07-21T16:15:01.000Z","instance":"gw-a","event":"client_received","request_id":"req-1004","api_key_id":17,"session_id":"green-42","model":"claude-opus-4-8"}
{"ts":"2026-07-21T16:15:01.050Z","instance":"gw-a","event":"future_event","request_id":"req-1004","detail":"new-version-field"}
{"ts":"2026-07-21T16:15:01.300Z","instance":"gw-a","event":"upstream_attempt","request_id":"req-1004","attempt":1,"upstream_request_id":"up-705"}
{"ts":"2026-07-21T16:15:05.000Z","instance":"gw-a","event":"upstream_result","request_id":"req-1004","attempt":1,"status":500,"upstream_ms":3680}
{"ts":"2026-07-21T16:15:06.200Z","instance":"gw-a","event":"upstream_attempt","request_id":"req-1004","attempt":2,"upstream_request_id":"up-706"}
{"ts":"2026-07-21T16:15:07.400Z","instance":"gw-a","event":"upstream_result","request_id":"req-1004","attempt":2,"status":200,"upstream_ms":1180}
{"ts":"2026-07-21T16:15:07.600Z","instance":"gw-a","event":"usage_committed","request_id":"req-1004","input_tokens":110,"output_tokens":44,"cache_creation_tokens":0,"cache_read_tokens":5120,"standard_cost":0.0031}
```

目前实际汇总里看到三种异常：`req-1001` 被算成失败但产生了费用；`req-1002` 的费用被加了两次；`req-1003` 在超时清理后有时会落到空 session。我们还怀疑跨午夜扫描时，同一个请求可能同时进入前一天和后一天的报告，但还没有稳定复现。

请先完成前面要求的根因排序和分析，不要直接输出替换后的完整文件。
