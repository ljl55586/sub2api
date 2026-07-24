# Claude Code 请求对齐：P2 / P3 交接

更新时间：2026-07-24

## 下一次任务目标

当前先停止修改。未来恢复工作时，在已经完成的 P0、P1 基础上继续：

1. P2：完善真实 Claude Code 的请求后处理、模型能力选择、缓存和恢复逻辑。
2. P3：只有用户明确确认确实需要本地 agent 能力后，才实现完整工具执行循环。

不要重新调查或推翻已经确认的 P0/P1 结论。完整证据、bundle 位置、trace 观察和当前对齐矩阵见：

- `docs/CLAUDE_CODE_UPSTREAM_REQUEST_MECHANISMS_REPORT.md`
- 重点阅读其中“1.2 P1 实施更新”“16. 改进路线”“17. 推荐验收测试”。

该报告被仓库 `.gitignore` 的 `docs/*` 规则忽略；文件存在于本地，但普通 `git status` 不会显示它。

## 当前代码状态

- 当前分支：`codex/claude-request-alignment`
- P0、P1 改动尚未 stage、commit。
- 工作树中还存在与本任务无关或更早存在的 soak 脚本、图片和其他用户文件。不得执行 `git reset --hard`、`git checkout -- .`、批量删除或 `git add -A`。
- 恢复工作后，先运行 `git status --short`，按文件确认改动归属。

已经完成的基线：

- P0：原子 2.1.161 profile、模型 token 默认值/上限、动态 CCH、title 长度门槛。
- P1：随机 runtime session UUID、账号级 device ID、Redis CAS runtime、quota/title 状态机、后台并发、标题与限流 header 保存、assistant 原始 blocks 收集、跨轮 transcript 恢复。
- P1 已覆盖原生 Anthropic Messages、OpenAI Chat Completions 和 Responses 三条 OAuth mimic 入口。

核心实现入口：

- `backend/internal/pkg/claude/profile.go`
- `backend/internal/service/gateway_claude_cch.go`
- `backend/internal/service/gateway_claude_oauth_runtime.go`
- `backend/internal/service/gateway_claude_oauth_compat_runtime.go`
- `backend/internal/service/gateway_claude_oauth_transcript.go`
- `backend/internal/service/gateway_claude_oauth_companion.go`
- `backend/internal/service/gateway_forward.go`
- `backend/internal/service/gateway_forward_as_chat_completions.go`
- `backend/internal/service/gateway_forward_as_responses.go`
- `backend/internal/repository/gateway_cache.go`
- `backend/internal/service/identity_service.go`
- `backend/internal/repository/identity_cache.go`

## P1 尚存的明确边界

这些不是需要推翻 P1 的 bug，但开始 P2 前应决定是否顺手补齐：

1. 当前没有显式的 session 创建 API。quota 是首个 HTTP 请求创建 runtime 时异步启动，而不是在用户首问之前启动。
2. quota/title 仍复用主请求 model，没有实现官方 small/fast model resolver。
3. device ID 使用 Redis 无 TTL key，不是数据库持久化。
4. delta-only 客户端必须提供稳定 session key；没有明确 key 时只能使用首条 user 文本 fallback。
5. 开启 `session_id_masking_enabled` 会有意覆盖 wire session UUID，不属于严格仿真模式。

如果用户只要求 P2，不要擅自新增 session 管理 API 或数据库 schema；先说明这些取舍。

## P2 实施范围

### P2.1 下一提示建议

目标：正常回答结束后，后台生成一个“用户下一步可能想做什么”的建议，但不影响正式回答。

实施要求：

- 从官方 bundle/报告中的 suggestion 门控开始，不凭空设计触发规则。
- suggestion 必须使用同一个 runtime session。
- 独立维护 `idle / pending / generated / retryable_failed` 状态和 claim lease。
- `ClaudeOAuthSessionRuntime.Suggestion` 已预留，但当前 action helper 只处理 quota/title，需要补齐。
- suggestion 请求不得写入正式 transcript。
- 实现 `skipTranscript`、`skipCacheWrite` 对应行为；缓存书签应落在合成 user 之前的 assistant 历史上。
- 对响应做格式和内容校验；失败必须 fail-open，不能使主请求失败。
- 工具循环未结束时不得提前生成 suggestion。

验收重点：

- suggestion body、beta、metadata/session 与 profile 一致。
- suggestion 不出现在下一轮主请求历史中。
- 同一轮不会重复发送；失败或 lease 过期后允许重试。
- suggestion 失败不影响主响应和 transcript。

### P2.2 模型能力驱动的 thinking / effort / context management

目标：不同模型使用自己支持的参数，不再把 Opus 4.8 的固定组合套到所有模型上。

建议做法：

- 在 `internal/pkg/claude` 的 profile/capability 层集中决策。
- 输入至少包含：请求角色、规范化模型 ID、调用方 override、模型能力。
- 输出至少包含：thinking 类型、budget、effort、context management、允许的 beta、默认/最大 `max_tokens`。
- 不支持 adaptive 的模型使用显式 budget，且 `budget_tokens < max_tokens`。
- 不支持某字段时删除该字段，不要发送“看起来合理但模型不接受”的值。
- 保持 passback-required 国产模型的现有 thinking 行为，不得把 Anthropic 严格规则误用到这些模型。

验收重点：

- 为 Opus、Sonnet、Haiku 和至少一个 passback-required 模型建立表驱动测试。
- 非 OAuth mimic、真实 Claude Code passthrough、普通 API key 路径行为不变。

### P2.3 context overflow 自动恢复

目标：历史太长、预留输出太大时，自动降低 `max_tokens` 并有限重试。

实施要求：

- 只识别明确的 context/max-token 超限错误，不对所有 400 重试。
- 根据上游限制和当前输入占用计算新的 `max_tokens`，不得低于安全下限。
- 重试次数和总耗时必须有硬上限。
- 保持同一 runtime session 和同一 turn claim。
- 修改 body 后重新执行依赖最终字节的步骤，尤其是 CCH。
- `ParsedRequest`、usage 和日志必须记录最终被上游接受的请求体。
- 只有最终成功响应可以 commit transcript；所有失败路径释放 turn lease。

验收重点：

- 首次 overflow、降低 max 后成功。
- 非 overflow 400 不重试。
- 第二次仍失败时正确返回原有错误语义。
- 重试 body 的 CCH 与最终字节重新匹配。

### P2.4 动态 cache marker

目标：把缓存书签放在真正能复用历史的位置，而不是固定改最后一个 block。

需要区分：

- 普通 main。
- 多轮 main。
- tool_use / tool_result continuation。
- suggestion 的 `skipCacheWrite`。
- system、tools 和 message marker 的四块上限。

实施要求：

- 将 marker 选择做成可独立测试的纯函数。
- 输入是请求角色、规范化历史和 profile；输出是明确 JSON path。
- 先移除代理管理的旧 marker，再注入新 marker，避免无限累积。
- 不误删真实 Claude Code passthrough 请求的客户端 marker。
- TTL 和 marker 数量继续服从原子 profile。

验收重点：

- cold main、cache-hit main、工具续跑、suggestion 分别有 golden。
- 第二轮请求能证明前缀未变化且 marker 位置正确。
- 始终不超过上游 cache block 限制。

### P2.5 严格模式下 upstream 始终 stream

目标：即使下游要求普通 JSON，OAuth mimic 上游也按真实 Claude Code 使用 SSE。

现状：

- Chat Completions 和 Responses 兼容入口已经强制 Anthropic upstream streaming，并在服务端转换。
- 原生 `/v1/messages` 仍主要跟随下游 `stream`。

实施要求：

- 只在明确的严格 OAuth mimic 模式启用，不偷偷改变所有代理请求。
- 下游 `stream=false` 时，在服务端完整读取 SSE，再组装 Anthropic JSON。
- 正确处理 `message_start`、content blocks、thinking/signature、tool_use JSON、usage、stop reason 和 `message_stop`。
- 设置最大累计字节、行长度、总超时和空流检测。
- SSE error、断流或缺少终止事件时不得 commit transcript。

验收重点：

- 同一输入的 downstream stream=true/false 使用相同的 upstream Claude Code 请求形态。
- 非流式 JSON 的 content、usage、stop reason 与流式重建结果一致。
- 客户端断开、上游断流和超大响应均有测试。

## P2 推荐实施顺序

1. 先补模型 capability planner，避免后续 suggestion/cache 继续复制硬编码。
2. 实现 suggestion 状态机与响应保存。
3. 实现动态 cache marker。
4. 实现 context overflow retry，并确保每次重试重新计算 CCH。
5. 最后实现原生 Messages 的 upstream-always-stream 聚合。

每一项单独提交和测试；不要一次性重写整个 `GatewayService.Forward`。

## P3 实施范围

P3 是从 API proxy 走向本地 agent runtime 的架构升级。开始前必须再次得到用户明确授权和产品边界，不应因为模型返回了 `tool_use` 就自动执行本地命令。

### P3.1 版本化工具 registry

- 定义 profile 版本对应的工具名、描述和 input schema。
- 只声明系统真正能执行的工具。
- 工具 schema 版本必须与 headers、beta、system prompt 同属一个 profile。

### P3.2 权限与安全边界

- 每类工具定义只读、写文件、执行命令、网络访问等权限。
- 默认拒绝写操作和命令执行，除非用户和部署配置明确授权。
- 限制工作目录、路径穿越、环境变量和敏感文件访问。
- 对危险操作保留人工确认，不允许模型自行扩大权限。

### P3.3 工具执行器

- 本地工具和 MCP 工具使用统一调用接口。
- 支持 context cancellation、超时、输出大小限制和结构化错误。
- 每次调用使用稳定 tool_use ID，防止重试时重复执行有副作用的工具。
- 对可重试和不可重试错误做明确区分。

### P3.4 完整工具循环

正确循环必须是：

1. 上游返回 `assistant`，其中包含原始 thinking/signature 和 `tool_use`。
2. runtime 原样提交 assistant blocks。
3. 本地执行工具。
4. 构造匹配 tool_use ID 的 `user(tool_result)`。
5. 使用同一 session、完整历史再次请求上游。
6. 直到模型正常结束、达到最大迭代次数、用户取消或发生不可恢复错误。

不得伪造 thinking signature，不得让 tool_result 找不到对应 tool_use，也不得把不同并发分支写入同一 transcript。

### P3.5 运行限制和恢复

- 设置最大工具轮数、单工具超时、总会话耗时和 transcript 大小。
- 每轮持久化状态，使进程重启后能够判断“尚未执行”“执行中”“已执行待续跑”。
- 有副作用工具需要幂等键或人工恢复策略。
- suggestion 只能在整个工具循环结束后运行。

P3 验收至少覆盖：

- 单工具成功。
- 多工具并行或串行。
- tool error 后模型恢复。
- 客户端取消。
- 进程在工具完成后、续跑前重启。
- 重复请求不会重复执行有副作用工具。
- thinking/signature/tool_use/tool_result 全链路原样保存。

## 不得破坏的行为

- CCH 不是固定字面量；必须根据最终 wire body 重新计算。
- 真实 Claude Code 客户端、metadata passthrough、非 OAuth mimic 和普通 API key 路径不得套用严格 mimic runtime。
- title/suggestion/工具专用 system 和响应不得混入主 transcript。
- companion、suggestion 或工具后台失败不得让已经成功的主请求失败。
- 不要把用户 IP/UA 作为明确 session key 的组成部分。
- 不要对未取得 active turn lease 的并发请求写 canonical transcript。

## 当前验证基线

在 2026-07-24 的当前工作树上已通过：

```text
go test ./internal/service -count=1
go test -tags=unit ./internal/service -count=1
go test ./internal/pkg/claude -count=1
go test -tags=unit ./internal/repository -count=1
```

P1、CCH、identity 和兼容入口的定向 race 测试也已通过。恢复修改前先重跑上述基线；完成每个阶段后再运行：

```text
go test -race ./internal/service -run 'Test(PrepareClaudeOAuthRuntime|ClaudeOAuthRuntimeActions|ClaudeOAuthStreamContentCollector|GatewayServiceForward_.*OAuthMimic|GatewayServiceForwardAs(ChatCompletions|Responses)_OAuthMimic|FinalizeClaudeCodeCCH|IdentityService_ResolveStableAccountIdentityPersistsDevice)' -count=1
git diff --check
```

## 建议技能

- 本地 Go 实现与测试没有直接匹配的专用技能；不要为了满足形式而调用无关技能。
- 如果用户要求读取或处理 GitHub issue/PR，再调用 `github:github`。
- 如果用户明确要求提交、推送并创建 PR，再调用 `github:yeet`；在此之前不要自动 stage、commit 或 push。

## 下一位 agent 的开工步骤

1. 完整阅读本文件和 `docs/CLAUDE_CODE_UPSTREAM_REQUEST_MECHANISMS_REPORT.md`。
2. 检查当前 branch、`git status --short` 和相关 diff，确认 P0/P1 是否已被用户提交。
3. 跑“当前验证基线”，先确认没有环境或工作树回归。
4. 向用户确认本次只做哪一个 P2 子项；默认从 capability planner 开始。
5. 保持小步修改、逐项测试，不要顺带进入 P3。
