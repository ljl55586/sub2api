# Claude OAuth Request Alignment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make non-Claude-Code Anthropic OAuth `/v1/messages` requests carry stable account-bound metadata and the requested Claude Code 2.1.161 header/body shape, while preserving valid real Claude Code identity and producing a no-network capture plus field-by-field risk report.

**Architecture:** Keep the existing real-CLI versus OAuth-mimic split in `GatewayService.Forward`. Resolve account identity once for the mimic body, normalize Opus 4.8 main-request fields behind an explicit option, then derive the outbound session header from the finalized body in `buildUpstreamRequest`. Keep count-tokens, API-key accounts, other models, beta membership, and real Claude Code requests outside the new defaults.

**Tech Stack:** Go, Gin, `net/http`, `gjson`, `sjson`, `httptest`, existing gateway debug snapshots, Go test.

## Global Constraints

- Canonical real trace: `/Users/ling/Demo/PacketCapture/.claude-trace/log-2026-07-15-03-02-52.json` from Claude Code CLI `2.1.161`.
- Existing simulated baseline: `/Users/ling/Demo/PacketCapture/sub2api/claude/log_20260714_062041.log`.
- Never call Anthropic or execute `DoWithTLS` in tests or capture generation.
- Preserve parseable metadata and session headers from requests already classified as real Claude Code.
- Generate mimic `account_uuid` only from account extra and `device_id` only from account identity or a successfully persisted account fingerprint.
- Format generated metadata using `claude.CLICurrentVersion`, never the incoming curl User-Agent.
- Do not add beta tokens, copy Claude Code tools/system prompts, emulate MacOS/TLS, or synthesize quota/title requests.
- Only normalized Opus 4.8 OAuth mimic main requests receive adaptive thinking, high effort, context management, and temperature removal.
- Count-tokens does not receive main-request defaults.
- In the capture input, tools are absent, so the finalized body must contain `tools: []`.
- Preserve the original comparison report and create new timestamped capture/report files.

---

### Task 1: Remove Out-of-Scope Draft Audit Work

**Files:**
- Modify: `backend/internal/service/gateway_service.go`
- Modify: `backend/internal/service/gateway_upstream_request.go`
- Modify: `backend/internal/service/gateway_count_tokens.go`
- Delete: `backend/internal/pkg/requestaudit/snapshot.go`
- Delete: `backend/internal/pkg/requestaudit/snapshot_test.go`
- Delete: `backend/internal/service/testdata/claude_code/2.1.161/manifest.json`
- Delete: `backend/internal/service/testdata/claude_code/2.1.161/quota.json`
- Delete: `backend/internal/service/testdata/claude_code/2.1.161/title.json`
- Delete: `backend/internal/service/testdata/claude_code/2.1.161/main.json`
- Delete or replace: `backend/internal/service/request_shape_alignment_test.go`

**Interfaces:**
- Consumes: approved design section 11, which explicitly excludes a generic request-audit/golden-fixture subsystem.
- Produces: the original debug snapshot behavior, with capture-time credentials represented by synthetic placeholders rather than global body hashing.

- [ ] **Step 1: Remove the generic audit hooks**

Remove `requestaudit` import/use, `GatewayService.auditGatewayRequest`, and both calls after `ApplyHeaderOverrides`. Restore `debugLogGatewaySnapshot` to log the body passed by the opt-in test without hashing metadata or prompt content.

- [ ] **Step 2: Delete the unused audit package and redacted fixtures**

Use `apply_patch` to remove the files above. The final comparison will read the authoritative trace directly and create one requested capture/report pair.

- [ ] **Step 3: Verify the cleanup compiles**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'TestNormalizeClaudeOAuthRequestBody_PreservesTopLevelFieldOrder|TestBuildUpstreamRequest_RealClaudeCodePreservesMetadata' -count=1
```

Expected: PASS; no package import references `internal/pkg/requestaudit`.

- [ ] **Step 4: Commit the cleanup**

```bash
git add backend/internal/service backend/internal/pkg/requestaudit
git commit -m "chore: remove out-of-scope request audit draft"
```

### Task 2: Make Account Identity Persistence Fail Closed

**Files:**
- Modify: `backend/internal/service/identity_service.go`
- Modify: `backend/internal/service/identity_service_order_test.go`
- Modify: `backend/internal/service/gateway_oauth_metadata_test.go`

**Interfaces:**
- Produces: `ResolveStableAccountIdentity(ctx context.Context, account *Account, headers http.Header) (AccountIdentity, error)`.
- Produces: `buildOAuthMetadataUserID(parsed *ParsedRequest, account *Account, identity AccountIdentity, uaVersion string) (string, error)` for the non-CLI mimic path.
- Consumes: `IdentityCache.GetFingerprint` and `IdentityCache.SetFingerprint`.

- [ ] **Step 1: Add failing cache-read and account-stability tests**

Add tests equivalent to:

```go
func TestIdentityService_GetOrCreateFingerprint_ReturnsReadError(t *testing.T) {
    cacheErr := errors.New("redis read unavailable")
    svc := NewIdentityService(&identityCacheStub{fingerprintErr: cacheErr})
    _, err := svc.GetOrCreateFingerprint(context.Background(), 123, http.Header{"User-Agent": {"curl/8.4.0"}})
    require.ErrorIs(t, err, cacheErr)
}

func TestResolveStableAccountIdentity_BindsDeviceToAccount(t *testing.T) {
    cache := &accountScopedIdentityCache{}
    svc := NewIdentityService(cache)
    account := &Account{ID: 123, Extra: map[string]any{"account_uuid": "account-123"}}
    first, err := svc.ResolveStableAccountIdentity(context.Background(), account, http.Header{"User-Agent": {"curl/8.4.0"}})
    require.NoError(t, err)
    second, err := svc.ResolveStableAccountIdentity(context.Background(), account, http.Header{"User-Agent": {"curl/8.5.0"}})
    require.NoError(t, err)
    require.Equal(t, first, second)
}
```

- [ ] **Step 2: Run tests and confirm the cache-read case fails**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'TestIdentityService_GetOrCreateFingerprint_(ReturnsReadError|ReturnsPersistenceError|RepairsEmptyClientIDBeforeReturning)|TestResolveStableAccountIdentity' -count=1
```

Expected before implementation: `ReturnsReadError` fails because the draft silently creates a new fingerprint after a cache read error.

- [ ] **Step 3: Return cache read failures and preserve existing persistence checks**

Implement the entry condition as:

```go
cached, err := s.cache.GetFingerprint(ctx, accountID)
if err != nil {
    return nil, fmt.Errorf("get fingerprint for account %d: %w", accountID, err)
}
if cached != nil {
    // Repair ClientID, refresh/merge, persist before return.
}
```

Keep the draft behavior that returns `SetFingerprint` errors for both cache miss and empty-ClientID repair.

- [ ] **Step 4: Make mimic metadata generation canonical**

Remove the early return that preserves a valid `parsed.MetadataUserID` inside `buildOAuthMetadataUserID`; this helper is called only after the request has entered the non-CLI mimic branch. Require non-empty `DeviceID` and `AccountUUID`, derive session from account/context/first user text, and format with the caller-supplied version.

Update the test so an existing downstream mimic metadata value is replaced by the account identity:

```go
parsed := mustParse(`{"metadata":{"user_id":"{\"device_id\":\"foreign\",\"account_uuid\":\"foreign\",\"session_id\":\"00000000-0000-4000-8000-000000000000\"}"},"messages":[{"role":"user","content":"hi"}]}`)
got, err := svc.buildOAuthMetadataUserID(parsed, account, AccountIdentity{DeviceID: "device-123", AccountUUID: "account-123"}, claude.CLICurrentVersion)
require.NoError(t, err)
require.Equal(t, "device-123", ParseMetadataUserID(got).DeviceID)
```

- [ ] **Step 5: Run identity tests**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'Test(IdentityService_GetOrCreateFingerprint|ResolveStableAccountIdentity|BuildOAuthMetadataUserID)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit identity behavior**

```bash
git add backend/internal/service/identity_service.go backend/internal/service/identity_service_order_test.go backend/internal/service/gateway_oauth_metadata_test.go
git commit -m "fix: bind OAuth mimic metadata to account identity"
```

### Task 3: Align Opus 4.8 Mimic Main-Request Body

**Files:**
- Modify: `backend/internal/service/gateway_claude_oauth_body.go`
- Modify: `backend/internal/service/gateway_body_order_test.go`
- Modify: `backend/internal/service/gateway_context_management_test.go`
- Create: `backend/internal/service/gateway_request_alignment_test.go`

**Interfaces:**
- Extends: `claudeOAuthNormalizeOptions` with `alignClaudeCodeMainRequest bool`.
- Produces: `alignClaudeCodeOpus48MainBody(body []byte, modelID string) ([]byte, bool)`.
- Consumes: normalized model ID, `setJSONRawBytes`, `setJSONValueBytes`, and `deleteJSONPathBytes`.

- [ ] **Step 1: Write failing target-shape tests**

Add:

```go
func TestNormalizeClaudeOAuthRequestBody_AlignsOpus48MimicMainRequest(t *testing.T) {
    input := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"temperature":1,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)
    out, model := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{alignClaudeCodeMainRequest: true})
    require.Equal(t, "claude-opus-4-8", model)
    require.False(t, gjson.GetBytes(out, "temperature").Exists())
    require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
    require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
    require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(out, "context_management.edits.0.type").String())
    require.Equal(t, "all", gjson.GetBytes(out, "context_management.edits.0.keep").String())
    require.True(t, gjson.GetBytes(out, "tools").IsArray())
    require.Len(t, gjson.GetBytes(out, "tools").Array(), 0)
}

func TestNormalizeClaudeOAuthRequestBody_DoesNotAlignOtherModelsOrCountTokens(t *testing.T) {
    input := []byte(`{"model":"claude-sonnet-4-6","temperature":0.2,"messages":[]}`)
    out, _ := normalizeClaudeOAuthRequestBody(input, "claude-sonnet-4-6", claudeOAuthNormalizeOptions{})
    require.Equal(t, 0.2, gjson.GetBytes(out, "temperature").Float())
    require.False(t, gjson.GetBytes(out, "thinking").Exists())
    require.False(t, gjson.GetBytes(out, "output_config").Exists())
}
```

Also test that an existing `output_config.format` survives while effort becomes high, and existing non-empty tools survive.

- [ ] **Step 2: Run tests and confirm missing fields fail**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'TestNormalizeClaudeOAuthRequestBody_(AlignsOpus48MimicMainRequest|DoesNotAlignOtherModelsOrCountTokens|PreservesOutputFormatAndTools)' -count=1
```

Expected before implementation: compile failure for the missing option or assertion failure for thinking/output_config.

- [ ] **Step 3: Implement the explicit Opus 4.8 alignment helper**

When `alignClaudeCodeMainRequest` is true and the normalized model equals `claude-opus-4-8`:

```go
out, _ = deleteJSONPathBytes(out, "temperature")
out, _ = setJSONRawBytes(out, "thinking", []byte(`{"type":"adaptive"}`))
if cfg := gjson.GetBytes(out, "output_config"); !cfg.Exists() || !strings.HasPrefix(strings.TrimSpace(cfg.Raw), "{") {
    out, _ = setJSONRawBytes(out, "output_config", []byte(`{}`))
}
out, _ = setJSONValueBytes(out, "output_config.effort", "high")
out, _ = setJSONRawBytes(out, "context_management", []byte(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`))
```

Run this after model normalization and tools defaulting, and before the existing tool-choice cleanup. Track every successful mutation in `modified`.

- [ ] **Step 4: Set the option only on main-request mimic callers**

Set `alignClaudeCodeMainRequest: true` in `GatewayService.Forward`'s `shouldMimicClaudeCode` body normalization and in `applyClaudeCodeOAuthMimicryToBody` for OpenAI-compatible message conversions. Leave `gateway_count_tokens.go` at the zero value.

- [ ] **Step 5: Run body and context tests**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'Test(NormalizeClaudeOAuthRequestBody|.*ContextManagement|.*BodyOrder)' -count=1
```

Expected: PASS; existing non-target tests remain unchanged.

- [ ] **Step 6: Commit body alignment**

```bash
git add backend/internal/service/gateway_claude_oauth_body.go backend/internal/service/gateway_forward.go backend/internal/service/gateway_body_order_test.go backend/internal/service/gateway_context_management_test.go backend/internal/service/gateway_request_alignment_test.go
git commit -m "feat: align Opus 4.8 OAuth mimic body"
```

### Task 4: Integrate Metadata and Session Headers in Final Requests

**Files:**
- Modify: `backend/internal/service/gateway_forward.go`
- Modify: `backend/internal/service/gateway_upstream_request.go`
- Modify: `backend/internal/service/gateway_oauth_metadata_test.go`
- Modify: `backend/internal/service/gateway_request_alignment_test.go`

**Interfaces:**
- Consumes: `ResolveStableAccountIdentity` and `buildOAuthMetadataUserID`.
- Produces: final mimic requests with no `x-client-request-id` and `x-claude-code-session-id == ParseMetadataUserID(body.metadata.user_id).SessionID`.
- Preserves: real Claude Code body metadata and session header.

- [ ] **Step 1: Add failing final-request tests**

Construct an OAuth account, Gin context with curl headers, in-memory identity cache, and an already normalized body containing generated metadata. Assert:

```go
require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
uid := ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String())
require.NotNil(t, uid)
require.Equal(t, uid.SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
require.Equal(t, expectedBeta, getHeaderRaw(req.Header, "anthropic-beta"))
```

Keep the real-CLI test that supplies a parseable metadata value and `session-cli` header, calls `buildUpstreamRequest(... mimicClaudeCode=false)`, and asserts both remain unchanged.

- [ ] **Step 2: Run tests and verify header failures**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'TestBuildUpstreamRequest_(MimicSessionHeaders|RealClaudeCodePreservesMetadata)' -count=1
```

Expected before implementation: mimic test fails because `x-client-request-id` exists and session header is absent.

- [ ] **Step 3: Resolve identity once in the Forward mimic body stage**

Replace the draft double fingerprint lookup with:

```go
if s.identityService == nil || c == nil || c.Request == nil {
    return nil, fmt.Errorf("resolve OAuth metadata identity: identity service or request is unavailable")
}
identity, err := s.identityService.ResolveStableAccountIdentity(ctx, account, c.Request.Header)
if err != nil {
    return nil, fmt.Errorf("resolve OAuth metadata identity: %w", err)
}
metadataUserID, err := s.buildOAuthMetadataUserID(parsed, account, identity, claude.CLICurrentVersion)
if err != nil {
    return nil, err
}
normalizeOpts.injectMetadata = true
normalizeOpts.metadataUserID = metadataUserID
```

Read metadata passthrough settings with a nil-safe default. When injection is requested, replace any non-CLI downstream metadata with the account-canonical value rather than preserving it.

- [ ] **Step 4: Remove request ID generation and derive the session header**

Remove the `github.com/google/uuid` import and UUID generation from `applyClaudeCodeMimicHeaders`. In `buildUpstreamRequest`, after body sanitize and mimic headers:

```go
if tokenType == "oauth" && mimicClaudeCode {
    deleteHeaderAllForms(req.Header, "x-client-request-id")
    parsedUserID := ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String())
    if parsedUserID == nil || strings.TrimSpace(parsedUserID.SessionID) == "" {
        return nil, nil, fmt.Errorf("OAuth mimic request is missing a valid metadata session")
    }
    setHeaderRaw(req.Header, "x-claude-code-session-id", parsedUserID.SessionID)
}
```

Delete the old conditional session synchronization that only ran when the header already existed. Do not alter the non-mimic path.

- [ ] **Step 5: Confirm beta/body coupling**

Assert the beta header is identical to `claude.FullClaudeCodeMimicryBetas()` after existing drop policy, and that `context_management` survives only because the current beta contains the required context-management token. Do not add tokens.

- [ ] **Step 6: Run targeted and service tests**

Run:

```bash
cd /Users/ling/sub2api/backend
go test ./internal/service -run 'Test(BuildUpstreamRequest|BuildOAuthMetadataUserID|NormalizeClaudeOAuthRequestBody|.*ContextManagement)' -count=1
go test ./internal/service -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit final request integration**

```bash
git add backend/internal/service/gateway_forward.go backend/internal/service/gateway_upstream_request.go backend/internal/service/gateway_oauth_metadata_test.go backend/internal/service/gateway_request_alignment_test.go
git commit -m "feat: synchronize Claude OAuth mimic session identity"
```

### Task 5: Produce the No-Network Capture

**Files:**
- Create: `backend/internal/service/gateway_claude_pro_upstream_capture_test.go`
- Create at runtime: `/Users/ling/Demo/PacketCapture/sub2api/claude/log_YYYYMMDD_HHMMSS.log`

**Interfaces:**
- Consumes: `normalizeClaudeOAuthRequestBody`, `ResolveStableAccountIdentity`, `buildOAuthMetadataUserID`, and `buildUpstreamRequest`.
- Produces: one CLIENT_ORIGINAL/UPSTREAM_FORWARD debug log without calling `DoWithTLS`.

- [ ] **Step 1: Port and update the unit-tagged capture test**

Use `//go:build unit`. Build the exact curl body:

```json
{"model":"claude-opus-4-8","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hello, please introduce yourself."}]}
```

Create an OAuth account with `account_uuid: "11111111-2222-4333-8444-555555555555"`, an in-memory IdentityCache, curl User-Agent, `SessionContext`, and synthetic token `REDACTED-DEMO-OAUTH-TOKEN`. Run the same system rewrite, identity generation, normalize options, `ParsedRequest.ReplaceBody`, and `buildUpstreamRequest` used by production. Never call `Forward` past the builder or `DoWithTLS`.

- [ ] **Step 2: Assert the captured object before writing**

Require:

```go
require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
require.NotEmpty(t, getHeaderRaw(req.Header, "x-claude-code-session-id"))
require.Equal(t, "adaptive", gjson.GetBytes(outBody, "thinking.type").String())
require.Equal(t, "high", gjson.GetBytes(outBody, "output_config.effort").String())
require.False(t, gjson.GetBytes(outBody, "temperature").Exists())
require.Len(t, gjson.GetBytes(outBody, "tools").Array(), 0)
require.Equal(t, ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String()).SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
```

- [ ] **Step 3: Run the capture exactly once**

Run:

```bash
cd /Users/ling/sub2api/backend
go test -tags=unit ./internal/service -run '^TestCaptureClaudeProUpstreamRequest$' -count=1 -v
```

Expected: PASS and a test log line containing the absolute `log_YYYYMMDD_HHMMSS.log` path.

- [ ] **Step 4: Verify no secret or network call**

Check that the log contains only the synthetic token placeholder/redacted authorization and that the test contains no invocation of `DoWithTLS`, `http.Client.Do`, or the upstream transport.

### Task 6: Generate the Field-by-Field Comparison and Complete the Audit

**Files:**
- Read: `/Users/ling/Demo/PacketCapture/.claude-trace/log-2026-07-15-03-02-52.json`
- Read: the new `log_YYYYMMDD_HHMMSS.log`
- Create: `/Users/ling/Demo/PacketCapture/sub2api/claude/same_version_comparison_after_alignment_YYYYMMDD_HHMMSS.md`
- Preserve: `/Users/ling/Demo/PacketCapture/sub2api/claude/same_version_comparison.md`

**Interfaces:**
- Consumes: the real trace main request (array entry 2) and the new UPSTREAM_FORWARD snapshot.
- Produces: a Markdown report with exact header/body field tables and risk classification.

- [ ] **Step 1: Parse both artifacts into comparable field inventories**

Record method, URL, every header key/value, top-level body keys and types, metadata components, system block count, message count/roles, tools count, thinking, output config, context management, temperature, max tokens, and stream.

- [ ] **Step 2: Write the report**

Include:

- evidence and request selection;
- before/after change summary;
- complete header table;
- complete body table;
- metadata device/account/session stability analysis;
- beta/body coupling analysis;
- remaining differences and their source (`gateway-transform`, `downstream-input`, `environment/transport`, or `client-agent-loop`);
- recognition/account-risk section that distinguishes code-proven facts, sample observations, and inference.

- [ ] **Step 3: Run fresh verification**

Run:

```bash
cd /Users/ling/sub2api/backend
gofmt -w internal/service/gateway_claude_pro_upstream_capture_test.go internal/service/gateway_request_alignment_test.go internal/service/gateway_oauth_metadata_test.go internal/service/gateway_body_order_test.go internal/service/identity_service_order_test.go internal/service/gateway_claude_oauth_body.go internal/service/gateway_forward.go internal/service/gateway_upstream_request.go internal/service/identity_service.go
go test ./internal/service -count=1
go test -tags=unit ./internal/service -run '^TestCaptureClaudeProUpstreamRequest$' -count=1
```

Expected: PASS.

- [ ] **Step 4: Audit every explicit requirement**

Use the new request object/log as evidence for each requested field, use tests as evidence for identity stability and real-CLI preservation, confirm the old report is unchanged, and confirm both new artifacts exist and are non-empty.

- [ ] **Step 5: Commit implementation and capture test**

```bash
git add backend/internal/service
git commit -m "test: capture aligned Claude OAuth upstream request"
```

Do not commit external logs containing synthetic identity values unless the user explicitly asks; deliver them by absolute path.
