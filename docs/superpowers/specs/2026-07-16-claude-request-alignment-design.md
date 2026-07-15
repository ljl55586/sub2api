# Claude OAuth 请求形状对齐设计

## 1. 背景与目标

本设计以以下两份本地证据为基线：

- 真实 Claude Code CLI 2.1.161 请求：`/Users/ling/Demo/PacketCapture/.claude-trace/log-2026-07-15-03-02-52.json`
- Sub2API 现有离线构造请求：`/Users/ling/Demo/PacketCapture/sub2api/claude/log_20260714_062041.log`

目标是在不向 Anthropic 发起真实请求的前提下，使 curl 等非 Claude Code 客户端通过 Sub2API 使用 Anthropic OAuth 账号时，最终构造出的 `/v1/messages` 主请求在指定字段上更接近真实 Claude Code 请求：

1. 生成账号绑定且会话稳定的 `metadata.user_id`；
2. 删除 mimic 路径多出的 `x-client-request-id`，并生成与 body session 一致的 `x-claude-code-session-id`；
3. 保持当前 `anthropic-beta` 集合不变，为目标 Opus 4.8 主请求补齐 `thinking`、`output_config` 和 `context_management`，删除 `temperature`，且在下游没有 tools 时保持 `tools: []`；
4. 不改变同一 OAuth 账号上的真实 Claude Code 请求身份；
5. 通过进程内生产构造逻辑生成新的时间戳日志和逐字段风险报告。

本设计只验证协议字段与代码行为，不承诺降低账号封禁概率，也不把请求对齐描述为绕过上游识别。

## 2. 已批准的策略

采用“路径级隔离”的方案 A：

- **真实 Claude Code 路径**：合法、可解析的 `metadata.user_id` 和客户端 session header 保持原样，不执行账号级身份重写。
- **非 Claude Code OAuth mimic 路径**：由 Sub2API 生成并维护账号级 device/account 身份和会话级 session，再补齐目标请求头及请求体字段。
- **API key 账号、非 Anthropic 平台及 count-tokens 路径**：不应用本设计新增的主请求对齐规则。

这样可以让 curl 获得稳定身份，同时避免 curl 产生的 fingerprint 或 session 覆盖同一账号上真实 Claude Code 客户端的原生身份。

## 3. 请求分类与数据流

现有主链路保持不变：

`GatewayHandler.Messages -> GatewayService.Forward -> normalizeClaudeOAuthRequestBody -> buildUpstreamRequest -> httpUpstream.DoWithTLS`

在 `GatewayService.Forward` 中根据现有 Claude Code 客户端识别结果区分：

- `isClaudeCode == true`：真实 CLI 路径；不注入 mimic metadata 和主请求默认字段。
- `account.IsOAuth() && isClaudeCode == false`：mimic 路径；执行本设计的身份和 body 对齐。

“真实 CLI 保持原样”只覆盖现有 detector 识别为 Claude Code、且 `metadata.user_id` 可解析的请求。缺失或非法 metadata 的伪装/异常请求不属于该保证范围，本改动不为它新增透传特权。

最终 header 处理放在 body 已完成规范化、metadata 已确定之后。`x-claude-code-session-id` 必须从最终 body 的可解析 `metadata.user_id.session_id` 得到，避免 header/body 分别生成而发生漂移。

## 4. 账号身份与 session 设计

### 4.1 账号级身份

引入一个边界清晰的内部结果：

```go
type AccountIdentity struct {
    DeviceID    string
    AccountUUID string
}
```

`ResolveStableAccountIdentity` 按以下规则解析：

1. `account_uuid` 必须来自当前 OAuth 账号的 account extra；
2. `device_id` 优先使用 `account.GetClaudeUserID()`；
3. 如果账号没有保存 Claude/Anthropic user ID，则使用 `fingerprint:<accountID>` 中的 `ClientID`；
4. 缓存 miss 时生成的 `ClientID` 只有成功持久化后才能返回；
5. 缓存命中但 `ClientID` 为空时，修复并成功持久化后才能返回；
6. `account_uuid`、`device_id` 缺失或 fingerprint 持久化失败时返回错误，在构造上游请求前停止，不使用请求级随机 fallback。

因此，同一 Sub2API OAuth 账号的 mimic 请求复用同一 device/account，不同账号使用不同的 fingerprint key，不共享 device。

### 4.2 metadata 格式

mimic 路径生成：

```json
{
  "metadata": {
    "user_id": "{\"device_id\":\"...\",\"account_uuid\":\"...\",\"session_id\":\"...\"}"
  }
}
```

格式选择必须依据最终出站 Claude CLI 版本 `claude.CLICurrentVersion`，不能依据 curl User-Agent 或从 curl 创建的 fingerprint User-Agent。这样出站 `User-Agent: claude-cli/2.1.161 ...` 与新版 JSON metadata 格式保持一致。

真实 Claude Code 路径中，只要现有 `metadata.user_id` 可被 `ParseMetadataUserID` 解析，就保持完整三元组不变。

### 4.3 session 稳定性

mimic 请求没有真实 Claude Code session，因此使用现有会话级稳定种子：

- 当前 OAuth account ID；
- `SessionContext` 中的 API key/client 区分信息；
- 首条 user 消息的规范化文本。

种子派生 UUID 形态的 session：同一对话随 messages 追加保持稳定，不同账号、下游上下文或首条消息产生不同 session。该值是 Sub2API 的兼容 session，不声称是 Claude Code 官方签发的 session。

## 5. 请求头规则

规则只作用于非 Claude Code Anthropic OAuth mimic 请求：

1. `applyClaudeCodeMimicHeaders` 不再创建 `x-client-request-id`；
2. 即使下游或通用 header 复制逻辑带入了 `x-client-request-id`，最终 mimic 请求也必须删除它；
3. body 完成后解析 `metadata.user_id`，将其中的 `session_id` 设置为 `x-claude-code-session-id`；
4. 测试强制断言 header session 与 body session 完全相同；
5. 真实 Claude Code 路径不新增、删除或重写其 session header；
6. `anthropic-beta` 继续使用现有策略，不新增真实样本中多出的 beta token；
7. `x-stainless-helper-method`、OS/runtime/TLS 等未被用户指定的字段本次不修改，只在最终报告中列为残余差异。

## 6. 请求体规则

### 6.1 适用范围

新增默认字段只应用于非 Claude Code OAuth mimic 的 `/v1/messages` 主请求。`/v1/messages/count_tokens` 不注入 `thinking`、`output_config` 或 `context_management`。

### 6.2 目标 Opus 4.8 主请求

对本次对比使用的 Opus 4.8 mimic 请求执行：

- 删除顶层 `temperature`，无论它来自 curl 还是旧的代理默认值；
- 设置 `thinking` 为 `{"type":"adaptive"}`；
- 将 `output_config.effort` 设置为 `"high"`；
- 保留 `output_config` 中与 effort 不冲突的其他合法子字段；
- 设置 `context_management` 为：

```json
{
  "edits": [
    {"type": "clear_thinking_20251015", "keep": "all"}
  ]
}
```

- 下游未提供 `tools` 时生成 `tools: []`；下游明确提供非空 tools 时保持其功能，不伪造真实 Claude Code 的内置工具目录；本次 curl 捕获输入不提供 tools，因此输出必须是空数组；
- 保留下游明确提供的 `max_tokens`、messages、system 和 stream；本次不把 `max_tokens` 改成真实样本的 64000。

模型判断使用规范化后的 Opus 4.8 model ID。其他模型不强制注入 adaptive thinking/high effort，而是保持现有模型兼容行为，避免把 Opus 4.8 的请求形状错误套到不支持这些字段的模型。

### 6.3 beta 联动

`context_management` 必须继续经过现有 `sanitizeAnthropicBodyForBetaTokens`。本设计不追加 beta：

- 当前最终 beta 包含 context-management 能力时，保留上述 body 字段；
- 如果未来配置使最终 beta 不再允许该字段，现有 sanitizer 应删除它，回归测试必须暴露该差异；
- 不允许出现 header 不声明能力但 body 静默携带该能力的情况。

body 修改完成后必须重新解析 `ParsedRequest`，使 thinking/output effort 等派生字段与最终 body 一致。

## 7. 错误处理

以下情况在发起上游请求前返回明确错误：

- OAuth 账号缺少 `account_uuid`；
- 无法得到或持久化账号级 `device_id`；
- mimic body 生成后无法得到合法的 metadata session；
- JSON 修改或重新解析失败。

这些错误不得回退为随机身份、空 metadata 或 header/body 不一致的请求。错误日志不得包含 OAuth token 或其他凭据。

## 8. 测试策略

严格按 TDD 增加以下测试：

1. 同一账号重复解析得到相同 device/account；不同账号得到不同 device；
2. fingerprint 写入失败、空 ClientID 修复写入失败、缺少 account UUID 时安全失败；
3. curl OAuth mimic 最终得到可解析的 JSON metadata；
4. 同一会话追加消息时 session 稳定，不同首条消息时 session 不同；
5. 真实 Claude Code 的 metadata/session 经过 builder 后不变；
6. mimic 最终没有 `x-client-request-id`，并且 header/body session 相同；
7. Opus 4.8 mimic 最终存在 adaptive thinking、high effort、context management、空 tools，且不存在 temperature；
8. `anthropic-beta` 集合不因本改动扩充；
9. count_tokens 不被主请求字段注入；
10. 现有 body-order、context-management、metadata masking、count-tokens 和完整 service 测试继续通过。

测试使用内存 cache、`httptest` 和构造后的 `http.Request`，不调用 Anthropic。

## 9. 离线捕获与报告

实现完成后，复用现有带 `//go:build unit` 的捕获测试思路：

1. 在进程内运行真实的 body normalize 和 `buildUpstreamRequest`；
2. 配置内存 IdentityCache 和带 `account_uuid` 的模拟 OAuth account；
3. 复用 `SUB2API_DEBUG_GATEWAY_BODY` 快照机制；
4. 不调用 `DoWithTLS` 或任何真实网络发送；
5. Authorization 在落盘前替换为明确的 redacted 占位值；
6. body 保留合成的测试 metadata、提示词和结构，以便逐字段核对；
7. 写入 `/Users/ling/Demo/PacketCapture/sub2api/claude/log_YYYYMMDD_HHMMSS.log`；
8. 生成同目录下 `same_version_comparison_after_alignment_YYYYMMDD_HHMMSS.md`，保留旧报告不覆盖。

报告逐项覆盖 method/URL、所有请求头、所有顶层 body 字段、metadata 三元组、system blocks、messages、tools、beta/body 联动，并把结论区分为：

- 已由代码和测试证明；
- 仅由两份本地样本观察；
- 无法从请求对象证明的传输或上游行为。

## 10. 风险分析要求

最终报告不得把字段对齐等同于风险降低。至少列出：

- beta token、system prompt、tools 数量、max_tokens 和伴生 quota/title 请求仍与真实主请求不同；
- `x-stainless-helper-method`、OS/runtime、TLS、出口 IP、并发、频率和请求时序仍可能形成组合画像；
- 合成 session 的稳定边界是启发式的，相同上下文和相同首条消息可能复用 session；
- `metadata.user_id` 会让账号/device/session 关系更稳定，也会让不一致更容易被关联观察；
- 使用 Claude Pro OAuth 经第三方中转的条款和账号风险不会因字段更像官方客户端而消失；
- 本地 `http.Request` 捕获不能证明真实网络层或上游服务端看到的完整特征。

## 11. 非目标

- 不补齐真实 trace 中额外的 `anthropic-beta` token；
- 不复制真实 Claude Code 的 30 个内置 tools；
- 不复制或硬编码真实 Claude Code system prompt；
- 不伪造 MacOS、TLS 或出口网络环境；
- 不实现 quota/title 伴生请求；
- 不发送真实 OAuth 请求验证账号是否会被识别或限制；
- 不引入通用 request-audit 子系统、golden fixture 框架或与本目标无关的日志重构。

## 12. 验收标准

只有同时满足以下条件才算完成：

- 生产代码实现方案 A 的真实 CC/mimic 隔离；
- 所有新增字段和删除字段都有最终请求级测试；
- metadata account/device 稳定且失败时不随机回退；
- header/body session 完全一致；
- 目标 Opus 4.8 curl 捕获满足指定 body/header 形状；
- `anthropic-beta` 未扩充，tools 在本次捕获中仍为空；
- 相关定向测试和 `go test ./internal/service` 通过，并按风险决定是否运行更广测试；
- 新日志按命名规范落盘且确认没有真实上游网络发送；
- 新的逐字段 Markdown 报告完成，并包含剩余差异和被上游识别的风险边界。
