//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	captureLogDir = "/Users/ling/Demo/PacketCapture/sub2api/claude"

	captureTrace20260715 = "/Users/ling/Demo/PacketCapture/.claude-trace/log-2026-07-15-03-02-52.json"
	captureTrace20260716 = "/Users/ling/Demo/PacketCapture/.claude-trace/log-2026-07-16-07-15-52.json"

	captureArtifactMode = 0o600
)

var expectedCaptureQuotaBetas = claude.ClaudeCodeOAuthQuotaMimicryBetas()

var expectedCaptureTitleBetas = claude.ClaudeCodeOAuthTitleMimicryBetas()

var expectedCaptureMainBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"redact-thinking-2026-02-12",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"advisor-tool-2026-03-01",
	"effort-2025-11-24",
	"extended-cache-ttl-2025-04-11",
}

type captureExpectedFixedHeader struct {
	Name     string
	Expected string
}

type captureFixedHeaderStatus struct {
	Present         bool
	MatchesExpected bool
}

func captureExpectedFixedHeaders() []captureExpectedFixedHeader {
	return []captureExpectedFixedHeader{
		{Name: "accept", Expected: "application/json"},
		{Name: "content-type", Expected: "application/json"},
		{Name: "anthropic-version", Expected: "2023-06-01"},
		{Name: "anthropic-dangerous-direct-browser-access", Expected: claude.DefaultHeaders["Anthropic-Dangerous-Direct-Browser-Access"]},
		{Name: "x-app", Expected: claude.DefaultHeaders["X-App"]},
		{Name: "user-agent", Expected: claude.DefaultHeaders["User-Agent"]},
		{Name: "x-stainless-lang", Expected: claude.DefaultHeaders["X-Stainless-Lang"]},
		{Name: "x-stainless-package-version", Expected: claude.DefaultHeaders["X-Stainless-Package-Version"]},
		{Name: "x-stainless-os", Expected: claude.DefaultHeaders["X-Stainless-OS"]},
		{Name: "x-stainless-arch", Expected: claude.DefaultHeaders["X-Stainless-Arch"]},
		{Name: "x-stainless-runtime", Expected: claude.DefaultHeaders["X-Stainless-Runtime"]},
		{Name: "x-stainless-runtime-version", Expected: claude.DefaultHeaders["X-Stainless-Runtime-Version"]},
		{Name: "x-stainless-retry-count", Expected: claude.DefaultHeaders["X-Stainless-Retry-Count"]},
		{Name: "x-stainless-timeout", Expected: claude.DefaultHeaders["X-Stainless-Timeout"]},
	}
}

// captureReportBeforePublishHook is only used to make the no-overwrite
// publication race deterministic in TestWriteCaptureReportAtomicallyPreservesConcurrentReport.
var captureReportBeforePublishHook func()

type captureIdentityCache struct {
	mu          sync.Mutex
	fingerprint *Fingerprint
	claimed     map[string]struct{}
}

func (c *captureIdentityCache) GetFingerprint(context.Context, int64) (*Fingerprint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fingerprint == nil {
		return nil, nil
	}
	copy := *c.fingerprint
	return &copy, nil
}

func (c *captureIdentityCache) SetFingerprint(_ context.Context, _ int64, fp *Fingerprint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := *fp
	c.fingerprint = &copy
	return nil
}

func (c *captureIdentityCache) GetMaskedSessionID(context.Context, int64) (string, error) {
	return "", nil
}

func (c *captureIdentityCache) SetMaskedSessionID(context.Context, int64, string) error {
	return nil
}

func (c *captureIdentityCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (c *captureIdentityCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (c *captureIdentityCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *captureIdentityCache) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

func (c *captureIdentityCache) TryClaimClaudeOAuthSessionCompanions(_ context.Context, accountID int64, sessionID string, ttl time.Duration) (bool, error) {
	if accountID <= 0 || strings.TrimSpace(sessionID) == "" || ttl <= 0 {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed == nil {
		c.claimed = make(map[string]struct{})
	}
	key := fmt.Sprintf("%d:%s", accountID, strings.TrimSpace(sessionID))
	if _, claimed := c.claimed[key]; claimed {
		return false, nil
	}
	c.claimed[key] = struct{}{}
	return true, nil
}

var _ GatewayCache = (*captureIdentityCache)(nil)
var _ ClaudeOAuthSessionCompanionClaimStore = (*captureIdentityCache)(nil)

type captureWireSnapshot struct {
	Role           string
	Request        *http.Request
	Body           []byte
	ResponseStatus int
}

// captureOfflineUpstreamRecorder records a deep copy of each final wire
// request and returns a new in-process response. It never opens a socket.
type captureOfflineUpstreamRecorder struct {
	mu        sync.Mutex
	snapshots []captureWireSnapshot
	notify    chan struct{}
}

func newCaptureOfflineUpstreamRecorder() *captureOfflineUpstreamRecorder {
	return &captureOfflineUpstreamRecorder{notify: make(chan struct{}, 1)}
}

func (u *captureOfflineUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *captureOfflineUpstreamRecorder) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("capture upstream received nil request")
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read capture request body: %w", err)
		}
		_ = req.Body.Close()
	}

	role := classifyCaptureWireRequest(body)
	response := captureOfflineResponse(role)
	snapshot := captureWireSnapshot{
		Role:           role,
		Request:        cloneCaptureWireRequest(req, body),
		Body:           bytes.Clone(body),
		ResponseStatus: response.StatusCode,
	}
	u.mu.Lock()
	u.snapshots = append(u.snapshots, snapshot)
	u.mu.Unlock()
	select {
	case u.notify <- struct{}{}:
	default:
	}
	return response, nil
}

func (u *captureOfflineUpstreamRecorder) WaitForCalls(want int, timeout time.Duration) bool {
	if want <= 0 {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		u.mu.Lock()
		count := len(u.snapshots)
		u.mu.Unlock()
		if count >= want {
			return true
		}
		select {
		case <-u.notify:
		case <-timer.C:
			return false
		}
	}
}

func (u *captureOfflineUpstreamRecorder) Snapshots() []captureWireSnapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	clones := make([]captureWireSnapshot, len(u.snapshots))
	for index, snapshot := range u.snapshots {
		clones[index] = captureWireSnapshot{
			Role:           snapshot.Role,
			Request:        cloneCaptureWireRequest(snapshot.Request, snapshot.Body),
			Body:           bytes.Clone(snapshot.Body),
			ResponseStatus: snapshot.ResponseStatus,
		}
	}
	return clones
}

func cloneCaptureWireRequest(req *http.Request, body []byte) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Body = io.NopCloser(bytes.NewReader(bytes.Clone(body)))
	clone.GetBody = nil
	clone.ContentLength = int64(len(body))
	return clone
}

func classifyCaptureWireRequest(body []byte) string {
	if gjson.GetBytes(body, "max_tokens").Int() == 1 &&
		gjson.GetBytes(body, "messages.0.content").String() == "quota" {
		return "quota"
	}
	if gjson.GetBytes(body, "output_config.format.type").String() == "json_schema" {
		return "title"
	}
	if gjson.GetBytes(body, "stream").Bool() && gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
		return "main"
	}
	return "other"
}

func captureOfflineResponse(role string) *http.Response {
	switch role {
	case "quota":
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"offline quota response"}}`)),
		}
	case "title":
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
				"",
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				"",
				"",
			}, "\n"))),
		}
	default:
		return claudeOAuthNoToolsProfileResponseForTest(true)
	}
}

type captureTraceEntry struct {
	Request captureTraceRequest `json:"request"`
}

type captureTraceRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// captureRequestSummary intentionally contains only report-safe information.
// It must never retain authorization, metadata values, CCH, or prompt text.
type captureRequestSummary struct {
	Source             string
	Index              int
	TraceRequestCount  int
	Role               string
	Method             string
	Path               string
	Model              string
	Stream             bool
	MaxTokens          int64
	MaxTokensPresent   bool
	ThinkingType       string
	OutputFormat       bool
	OutputFormatType   string
	OutputEffort       string
	TemperaturePresent bool

	ContextManagementPresent bool
	ContextEditCount         int
	ContextEditType          string
	ContextEditKeep          string

	BetaTokenCount         int
	BetaTokens             []string
	BetaMatchesMainProfile bool
	StainlessOS            string
	UserAgentVersion       string
	BillingVersion         string
	BillingHasCCH          bool

	AuthorizationPresent         bool
	SessionHeaderPresent         bool
	MetadataParseable            bool
	MetadataHasThreeFields       bool
	MetadataSessionMatchesHeader bool
	ClientRequestIDPresent       bool
	HelperMethodPresent          bool
	ConnectionPresent            bool
	AcceptEncodingPresent        bool
	FixedHeaders                 map[string]captureFixedHeaderStatus

	SystemBlocks      []captureBlockSummary
	Messages          []captureMessageSummary
	CacheControls     []captureCacheControl
	ToolsPresent      bool
	ToolsCount        int
	ToolChoicePresent bool
}

type captureBlockSummary struct {
	Location   string
	Type       string
	TextLength int
}

type captureMessageSummary struct {
	Role         string
	ContentShape string
	BlockTypes   []string
	TextLengths  []int
}

type captureCacheControl struct {
	Location string
	Type     string
	TTL      string
}

// TestCaptureClaudeProUpstreamRequest drives the real GatewayService.Forward
// path through a local HTTPUpstream recorder. The recorder captures the final
// wire request without opening a network connection.
func TestCaptureClaudeProUpstreamRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	clientBody := []byte(`{` +
		`"model":"claude-opus-4-8",` +
		`"max_tokens":1024,` +
		`"stream":true,` +
		`"messages":[{"role":"user","content":"Hello, please introduce yourself."}]` +
		`}`)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Anthropic-Version", "2023-06-01")

	parsed, err := ParseGatewayRequest(NewRequestBodyRef(clientBody), PlatformAnthropic)
	require.NoError(t, err)
	parsed.SessionContext = &SessionContext{
		ClientIP:  "127.0.0.1",
		UserAgent: "curl/8.4.0",
		APIKeyID:  7001,
	}

	const token = "REDACTED-DEMO-OAUTH-TOKEN"
	account := &Account{
		ID:          1001,
		Name:        "claude-pro-oauth-offline-capture",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": token},
		Extra: map[string]any{
			"account_uuid": "11111111-2222-4333-8444-555555555555",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
	resetGatewayForwardingSettingsCacheForTest(t)
	gatewaySettings := map[string]string{
		SettingKeyEnableClaudeOAuthSystemPromptInjection: "true",
		SettingKeyRewriteMessageCacheControl:             "false",
		SettingKeyEnableAnthropicCacheTTL1hInjection:     "false",
	}
	cache := &captureIdentityCache{}
	upstream := newCaptureOfflineUpstreamRecorder()
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cache:                cache,
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       NewSettingService(&gatewayTTLSettingRepo{data: gatewaySettings}, cfg),
		identityService:      NewIdentityService(cache),
	}

	trace20260715, err := selectCaptureTraceSessionRequests(captureTrace20260715)
	require.NoError(t, err)
	trace20260716, err := selectCaptureTraceSessionRequests(captureTrace20260716)
	require.NoError(t, err)

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	runID := now.Format("20060102_150405")
	logPath := filepath.Join(captureLogDir, "log_"+runID+".log")
	reportPath := filepath.Join(captureLogDir, "same_version_comparison_after_session_companions_alignment_"+runID+".md")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, captureArtifactMode)
	require.NoError(t, err, "capture log name must be new; never append to an existing artifact")
	t.Cleanup(func() {
		svc.debugGatewayBodyFile.Store(nil)
		_ = file.Close()
		// Keep incomplete artifacts rather than unlinking a global path during
		// cleanup: another process may have replaced it after this test created
		// its file. O_EXCL and report publication already prevent overwrites.
	})
	require.NoError(t, file.Chmod(captureArtifactMode))
	svc.debugGatewayBodyFile.Store(file)

	writeCaptureBanner(t, file, now, account, parsed.Model)
	result, err := svc.Forward(ctx, c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.False(t, result.ClientDisconnect)
	require.True(t, upstream.WaitForCalls(3, time.Second), "the first OAuth mimic session must capture quota, title, and main through the local fake HTTPUpstream")
	snapshots := upstream.Snapshots()
	require.Len(t, snapshots, 3)
	quota := findCaptureWireSnapshot(t, snapshots, "quota")
	title := findCaptureWireSnapshot(t, snapshots, "title")
	main := findCaptureWireSnapshot(t, snapshots, "main")
	require.Equal(t, http.StatusTooManyRequests, quota.ResponseStatus)
	require.Equal(t, http.StatusOK, title.ResponseStatus)
	require.Equal(t, http.StatusOK, main.ResponseStatus)
	assertAlignedCaptureQuotaRequest(t, quota.Request, quota.Body)
	assertAlignedCaptureTitleRequest(t, title.Request, title.Body)
	assertAlignedCaptureMainRequest(t, main.Request, main.Body)
	assertCaptureWireSessionsMatch(t, quota, title, main)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "message_start")
	require.Contains(t, recorder.Body.String(), "event: message_stop")
	require.NoError(t, file.Sync())

	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotEmpty(t, logBytes)
	require.Contains(t, string(logBytes), "UPSTREAM_SESSION_COMPANION_QUOTA")
	require.Contains(t, string(logBytes), "UPSTREAM_SESSION_COMPANION_TITLE")
	require.Contains(t, string(logBytes), "UPSTREAM_FORWARD")
	require.Contains(t, string(logBytes), `"thinking": {`)
	require.Contains(t, string(logBytes), `"output_config": {`)
	require.Contains(t, string(logBytes), `"context_management": {`)
	require.Contains(t, string(logBytes), `"metadata": {`)
	require.NotContains(t, string(logBytes), token)
	require.Contains(t, string(logBytes), "[redacted]")
	logInfo, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(captureArtifactMode), logInfo.Mode().Perm())

	synthetic, err := summarizeCaptureSessionSnapshots("offline synthetic capture", snapshots)
	require.NoError(t, err)
	report := buildSessionCompanionsAlignmentCaptureReport(now, trace20260715, trace20260716, synthetic)
	require.Contains(t, report, "Bearer [redacted]")
	require.Contains(t, report, "device_id=[redacted]")
	require.Contains(t, report, "## 上游识别风险")
	require.Contains(t, report, "## quota：trace15 与合成 capture")
	require.Contains(t, report, "## title：trace15 与合成 capture")
	require.Contains(t, report, "## main：trace15 与合成 capture")
	require.Contains(t, report, "有序 `anthropic-beta`")
	require.Contains(t, report, "output_config.effort=high")
	require.Contains(t, report, "trace16")
	require.Contains(t, report, "仅为观察事实")
	require.Contains(t, report, "## 固定 Header profile")
	require.Contains(t, report, "| `accept` |")
	require.Contains(t, report, "| `x-claude-code-session-id` |")
	require.Contains(t, report, "| `connection` / `accept-encoding` |")
	require.NotContains(t, report, token)
	require.NotContains(t, report, "11111111-2222-4333-8444-555555555555")
	require.NotContains(t, report, "cch=")
	require.NotContains(t, report, "Hello, please introduce yourself.")
	require.NotContains(t, report, claudeOAuthCompanionTitleSystemPrompt)
	published, err := writeCaptureReportAtomically(reportPath, []byte(report))
	require.NoError(t, err)
	require.True(t, published)
	reportInfo, err := os.Stat(reportPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(captureArtifactMode), reportInfo.Mode().Perm())
	t.Logf("CAPTURE_LOG=%s", logPath)
	t.Logf("COMPARISON_REPORT=%s", reportPath)
}

func findCaptureWireSnapshot(t *testing.T, snapshots []captureWireSnapshot, role string) captureWireSnapshot {
	t.Helper()
	for _, snapshot := range snapshots {
		if snapshot.Role == role {
			return snapshot
		}
	}
	require.FailNow(t, "captured upstream snapshot not found", "role=%s", role)
	return captureWireSnapshot{}
}

func assertCaptureWireSessionsMatch(t *testing.T, quota, title, main captureWireSnapshot) {
	t.Helper()
	quotaSession := captureWireSession(t, quota.Request, quota.Body)
	require.Equal(t, quotaSession, captureWireSession(t, title.Request, title.Body), "title session must match quota")
	require.Equal(t, quotaSession, captureWireSession(t, main.Request, main.Body), "main session must match quota")
}

func captureWireSession(t *testing.T, req *http.Request, body []byte) string {
	t.Helper()
	require.NotNil(t, req)
	userID := gjson.GetBytes(body, "metadata.user_id").String()
	require.True(t, gjson.Valid(userID))
	metadata := ParseMetadataUserID(userID)
	require.NotNil(t, metadata)
	require.NotEmpty(t, metadata.SessionID)
	require.Equal(t, metadata.SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	return metadata.SessionID
}

func assertAlignedCaptureCommonRequest(t *testing.T, req *http.Request, body []byte, expectedBetas []string) {
	t.Helper()
	require.NotNil(t, req)
	require.Equal(t, claudeAPIURL, req.URL.String())
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
	require.Equal(t, claude.DefaultStainlessOS, getHeaderRaw(req.Header, "x-stainless-os"))
	require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.NotEmpty(t, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	require.Equal(t, expectedBetas, parseAnthropicBetaHeader(getHeaderRaw(req.Header, "anthropic-beta")))
	captureWireSession(t, req, body)
}

func assertCaptureTopLevelKeys(t *testing.T, body []byte, expected ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &object))
	require.Len(t, object, len(expected))
	for _, key := range expected {
		require.Contains(t, object, key)
	}
}

func assertAlignedCaptureQuotaRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	assertAlignedCaptureCommonRequest(t, req, body, expectedCaptureQuotaBetas)
	assertCaptureTopLevelKeys(t, body, "max_tokens", "messages", "metadata", "model")
	require.Equal(t, "claude-opus-4-8", gjson.GetBytes(body, "model").String())
	require.Equal(t, int64(1), gjson.GetBytes(body, "max_tokens").Int())
	require.Equal(t, "user", gjson.GetBytes(body, "messages.0.role").String())
	require.Equal(t, "quota", gjson.GetBytes(body, "messages.0.content").String())
}

func assertAlignedCaptureTitleRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	assertAlignedCaptureCommonRequest(t, req, body, expectedCaptureTitleBetas)
	assertCaptureTopLevelKeys(t, body, "max_tokens", "messages", "metadata", "model", "output_config", "stream", "system", "tools")
	require.Equal(t, "claude-opus-4-8", gjson.GetBytes(body, "model").String())
	require.Equal(t, int64(64000), gjson.GetBytes(body, "max_tokens").Int())
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.True(t, gjson.GetBytes(body, "tools").IsArray())
	require.Empty(t, gjson.GetBytes(body, "tools").Array())
	require.False(t, gjson.GetBytes(body, "thinking").Exists())
	require.False(t, gjson.GetBytes(body, "context_management").Exists())
	require.False(t, gjson.GetBytes(body, "temperature").Exists())
	require.Equal(t, "json_schema", gjson.GetBytes(body, "output_config.format.type").String())
	require.Equal(t, "high", gjson.GetBytes(body, "output_config.effort").String())
	require.Equal(t, "object", gjson.GetBytes(body, "output_config.format.schema.type").String())
	require.Equal(t, "string", gjson.GetBytes(body, "output_config.format.schema.properties.title.type").String())
	require.Equal(t, "title", gjson.GetBytes(body, "output_config.format.schema.required.0").String())
	require.False(t, gjson.GetBytes(body, "output_config.format.schema.additionalProperties").Bool())

	message := gjson.GetBytes(body, "messages.0")
	require.Equal(t, "user", message.Get("role").String())
	require.True(t, message.Get("content").IsArray())
	require.Len(t, message.Get("content").Array(), 1)
	require.Equal(t, "text", message.Get("content.0.type").String())
	require.NotEmpty(t, message.Get("content.0.text").String())

	system := gjson.GetBytes(body, "system")
	require.True(t, system.IsArray())
	require.Len(t, system.Array(), 3)
	for _, block := range system.Array() {
		require.Equal(t, "text", block.Get("type").String())
		require.NotEmpty(t, block.Get("text").String())
		require.False(t, block.Get("cache_control").Exists())
	}
	billingText := system.Get("0.text").String()
	require.Equal(t, "cc_version="+ExtractCLIVersion(getHeaderRaw(req.Header, "User-Agent")), ccVersionInBillingRe.FindString(billingText))
	require.Contains(t, billingText, "cc_entrypoint=cli;")
	require.NotContains(t, billingText, "cch=")
}

func assertAlignedCaptureMainRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	assertAlignedCaptureCommonRequest(t, req, body, expectedCaptureMainBetas)

	billingText := gjson.GetBytes(body, "system.0.text").String()
	require.Equal(t, "cc_version="+ExtractCLIVersion(getHeaderRaw(req.Header, "User-Agent")), ccVersionInBillingRe.FindString(billingText))
	require.Contains(t, billingText, "cc_entrypoint=cli;")
	require.NotContains(t, billingText, "cch=")
	require.False(t, gjson.GetBytes(body, "system.0.cache_control").Exists())

	require.False(t, gjson.GetBytes(body, "temperature").Exists())
	require.Equal(t, "adaptive", gjson.GetBytes(body, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(body, "output_config.effort").String())
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(body, "context_management.edits.0.type").String())
	require.Equal(t, "all", gjson.GetBytes(body, "context_management.edits.0.keep").String())
	require.True(t, gjson.GetBytes(body, "tools").IsArray())
	require.Empty(t, gjson.GetBytes(body, "tools").Array())
	require.False(t, gjson.GetBytes(body, "tool_choice").Exists())
	require.Equal(t, int64(1024), gjson.GetBytes(body, "max_tokens").Int())

	require.Equal(t, "ephemeral", gjson.GetBytes(body, "system.1.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(body, "system.1.cache_control.ttl").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(body, "system.2.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(body, "system.2.cache_control.ttl").String())
	require.True(t, gjson.GetBytes(body, "messages.0.content").IsArray())
	require.Len(t, gjson.GetBytes(body, "messages.0.content").Array(), 1)
	require.Equal(t, "text", gjson.GetBytes(body, "messages.0.content.0.type").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(body, "messages.0.content.0.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(body, "messages.0.content.0.cache_control.ttl").String())
	require.Equal(t, 3, countCaptureCacheControls(t, body))
}

func writeCaptureBanner(t *testing.T, file *os.File, now time.Time, account *Account, modelID string) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api Claude Pro OAuth upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# account: " + account.Name,
		"# model: " + modelID,
		"# network: disabled; GatewayService.Forward uses only an in-process fake HTTPUpstream",
		"# CLIENT_ORIGINAL = curl input to sub2api",
		"# UPSTREAM_FORWARD = object that would be sent to api.anthropic.com",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}

func countCaptureCacheControls(t *testing.T, body []byte) int {
	t.Helper()
	var decoded any
	require.NoError(t, json.Unmarshal(body, &decoded))
	return countCaptureCacheControlsValue(decoded)
}

func countCaptureCacheControlsValue(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		count := 0
		for key, child := range typed {
			if key == "cache_control" {
				count++
			}
			count += countCaptureCacheControlsValue(child)
		}
		return count
	case []any:
		count := 0
		for _, child := range typed {
			count += countCaptureCacheControlsValue(child)
		}
		return count
	default:
		return 0
	}
}

type captureSessionRequests struct {
	Source       string
	RequestCount int
	Quota        *captureRequestSummary
	Title        *captureRequestSummary
	Main         *captureRequestSummary
}

func selectCaptureTraceSessionRequests(tracePath string) (captureSessionRequests, error) {
	source := filepath.Base(tracePath)
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		return captureSessionRequests{}, fmt.Errorf("read trace %s: %w", source, err)
	}

	var entries []captureTraceEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return captureSessionRequests{}, fmt.Errorf("parse trace %s: invalid JSON", source)
	}
	if len(entries) == 0 {
		return captureSessionRequests{}, fmt.Errorf("trace %s has no requests", source)
	}

	requests := captureSessionRequests{Source: source, RequestCount: len(entries)}
	for index, entry := range entries {
		headers := make(http.Header, len(entry.Request.Headers))
		for key, value := range entry.Request.Headers {
			headers.Set(key, value)
		}
		summary, err := summarizeCaptureRequest(source, index, entry.Request.Method, entry.Request.URL, headers, entry.Request.Body)
		if err != nil {
			return captureSessionRequests{}, fmt.Errorf("trace %s request index %d cannot be summarized safely: %w", source, index, err)
		}
		summary.TraceRequestCount = len(entries)
		summary.Role = classifyCaptureWireRequest(entry.Request.Body)
		if err := addCaptureSessionRequest(&requests, summary); err != nil {
			return captureSessionRequests{}, fmt.Errorf("trace %s request index %d: %w", source, index, err)
		}
	}

	if requests.Quota == nil || requests.Main == nil {
		return captureSessionRequests{}, fmt.Errorf("trace %s must contain exactly one quota and one main request", source)
	}
	return requests, nil
}

func summarizeCaptureSessionSnapshots(source string, snapshots []captureWireSnapshot) (captureSessionRequests, error) {
	requests := captureSessionRequests{Source: source, RequestCount: len(snapshots)}
	for index, snapshot := range snapshots {
		if snapshot.Request == nil {
			return captureSessionRequests{}, fmt.Errorf("capture snapshot %d has no request", index)
		}
		summary, err := summarizeCaptureRequest(source, index, snapshot.Request.Method, snapshot.Request.URL.String(), snapshot.Request.Header, snapshot.Body)
		if err != nil {
			return captureSessionRequests{}, fmt.Errorf("capture snapshot %d cannot be summarized safely: %w", index, err)
		}
		summary.Role = snapshot.Role
		if err := addCaptureSessionRequest(&requests, summary); err != nil {
			return captureSessionRequests{}, fmt.Errorf("capture snapshot %d: %w", index, err)
		}
	}
	if requests.Quota == nil || requests.Title == nil || requests.Main == nil {
		return captureSessionRequests{}, fmt.Errorf("offline capture must contain exactly one quota, title, and main request")
	}
	return requests, nil
}

func addCaptureSessionRequest(requests *captureSessionRequests, summary captureRequestSummary) error {
	if requests == nil {
		return fmt.Errorf("nil capture session")
	}
	copy := summary
	switch summary.Role {
	case "quota":
		if requests.Quota != nil {
			return fmt.Errorf("duplicate quota request")
		}
		requests.Quota = &copy
	case "title":
		if requests.Title != nil {
			return fmt.Errorf("duplicate title request")
		}
		requests.Title = &copy
	case "main":
		if requests.Main != nil {
			return fmt.Errorf("duplicate main request")
		}
		requests.Main = &copy
	default:
		return fmt.Errorf("unexpected request role %q", summary.Role)
	}
	return nil
}

func summarizeCaptureRequest(source string, index int, method, rawURL string, headers http.Header, body []byte) (captureRequestSummary, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Path == "" {
		return captureRequestSummary{}, fmt.Errorf("invalid request URL shape")
	}
	if !json.Valid(body) {
		return captureRequestSummary{}, fmt.Errorf("request body is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return captureRequestSummary{}, fmt.Errorf("request body is not a JSON object")
	}

	summary := captureRequestSummary{
		Source:     source,
		Index:      index,
		Method:     method,
		Path:       parsedURL.Path,
		ToolsCount: -1,
	}
	summary.Model = gjson.GetBytes(body, "model").String()
	summary.Stream = gjson.GetBytes(body, "stream").Bool()
	maxTokens := gjson.GetBytes(body, "max_tokens")
	summary.MaxTokensPresent = maxTokens.Exists()
	summary.MaxTokens = maxTokens.Int()
	summary.ThinkingType = gjson.GetBytes(body, "thinking.type").String()
	outputConfig := gjson.GetBytes(body, "output_config")
	summary.OutputFormat = outputConfig.Get("format").Exists()
	summary.OutputFormatType = captureSafeOutputFormatType(outputConfig.Get("format.type").String())
	summary.OutputEffort = outputConfig.Get("effort").String()
	summary.TemperaturePresent = gjson.GetBytes(body, "temperature").Exists()

	contextManagement := gjson.GetBytes(body, "context_management")
	summary.ContextManagementPresent = contextManagement.Exists()
	contextEdits := contextManagement.Get("edits")
	if contextEdits.IsArray() {
		edits := contextEdits.Array()
		summary.ContextEditCount = len(edits)
		if len(edits) > 0 {
			summary.ContextEditType = edits[0].Get("type").String()
			summary.ContextEditKeep = edits[0].Get("keep").String()
		}
	}

	betaTokens := parseAnthropicBetaHeader(getHeaderRaw(headers, "anthropic-beta"))
	summary.BetaTokenCount = len(betaTokens)
	summary.BetaTokens = captureSafeBetaTokens(betaTokens)
	summary.BetaMatchesMainProfile = captureStringSlicesEqual(betaTokens, expectedCaptureMainBetas)
	summary.StainlessOS = getHeaderRaw(headers, "x-stainless-os")
	summary.UserAgentVersion = ExtractCLIVersion(getHeaderRaw(headers, "user-agent"))
	summary.AuthorizationPresent = captureHeaderPresent(headers, "authorization")
	summary.SessionHeaderPresent = captureHeaderPresent(headers, "x-claude-code-session-id")
	summary.ClientRequestIDPresent = captureHeaderPresent(headers, "x-client-request-id")
	summary.HelperMethodPresent = captureHeaderPresent(headers, "x-stainless-helper-method")
	summary.ConnectionPresent = captureHeaderPresent(headers, "connection")
	summary.AcceptEncodingPresent = captureHeaderPresent(headers, "accept-encoding")
	summary.FixedHeaders = make(map[string]captureFixedHeaderStatus, len(captureExpectedFixedHeaders()))
	for _, expected := range captureExpectedFixedHeaders() {
		actual := getHeaderRaw(headers, expected.Name)
		summary.FixedHeaders[expected.Name] = captureFixedHeaderStatus{
			Present:         strings.TrimSpace(actual) != "",
			MatchesExpected: actual == expected.Expected,
		}
	}
	summary.ToolChoicePresent = gjson.GetBytes(body, "tool_choice").Exists()

	tools := gjson.GetBytes(body, "tools")
	summary.ToolsPresent = tools.Exists()
	if tools.IsArray() {
		summary.ToolsCount = len(tools.Array())
	}

	metadata := gjson.GetBytes(body, "metadata.user_id")
	if metadata.Type == gjson.String {
		parsedMetadata := ParseMetadataUserID(metadata.String())
		summary.MetadataParseable = parsedMetadata != nil
		if parsedMetadata != nil {
			summary.MetadataHasThreeFields = parsedMetadata.DeviceID != "" && parsedMetadata.AccountUUID != "" && parsedMetadata.SessionID != ""
			summary.MetadataSessionMatchesHeader = summary.SessionHeaderPresent && parsedMetadata.SessionID == getHeaderRaw(headers, "x-claude-code-session-id")
		}
	}

	summarizeCaptureSystem(&summary, body)
	summarizeCaptureMessages(&summary, body)
	return summary, nil
}

func summarizeCaptureSystem(summary *captureRequestSummary, body []byte) {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		return
	}
	index := 0
	system.ForEach(func(_, block gjson.Result) bool {
		location := fmt.Sprintf("system[%d]", index)
		text := block.Get("text")
		textLength := 0
		if text.Type == gjson.String {
			textLength = len(text.String())
			if strings.Contains(text.String(), "x-anthropic-billing-header:") {
				if match := ccVersionInBillingRe.FindString(text.String()); match != "" {
					summary.BillingVersion = strings.TrimPrefix(match, "cc_version=")
				}
				summary.BillingHasCCH = strings.Contains(text.String(), "cch=")
			}
		}
		summary.SystemBlocks = append(summary.SystemBlocks, captureBlockSummary{
			Location:   location,
			Type:       captureSafeBlockType(block.Get("type").String()),
			TextLength: textLength,
		})
		appendCaptureCacheControl(summary, location, block)
		index++
		return true
	})
}

func summarizeCaptureMessages(summary *captureRequestSummary, body []byte) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return
	}
	messageIndex := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		messageSummary := captureMessageSummary{
			Role: captureSafeRole(message.Get("role").String()),
		}
		content := message.Get("content")
		switch {
		case content.Type == gjson.String:
			messageSummary.ContentShape = "string"
			messageSummary.TextLengths = append(messageSummary.TextLengths, len(content.String()))
		case content.IsArray():
			messageSummary.ContentShape = "array"
			blockIndex := 0
			content.ForEach(func(_, block gjson.Result) bool {
				messageSummary.BlockTypes = append(messageSummary.BlockTypes, captureSafeBlockType(block.Get("type").String()))
				text := block.Get("text")
				if text.Type == gjson.String {
					messageSummary.TextLengths = append(messageSummary.TextLengths, len(text.String()))
				}
				appendCaptureCacheControl(summary, fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex), block)
				blockIndex++
				return true
			})
		default:
			messageSummary.ContentShape = "other"
		}
		summary.Messages = append(summary.Messages, messageSummary)
		messageIndex++
		return true
	})
}

func appendCaptureCacheControl(summary *captureRequestSummary, location string, block gjson.Result) {
	cacheControl := block.Get("cache_control")
	if !cacheControl.Exists() {
		return
	}
	summary.CacheControls = append(summary.CacheControls, captureCacheControl{
		Location: location,
		Type:     captureSafeCacheType(cacheControl.Get("type").String()),
		TTL:      captureSafeCacheTTL(cacheControl.Get("ttl").String()),
	})
}

func isCaptureMainRequest(summary captureRequestSummary) bool {
	return summary.Method == http.MethodPost &&
		summary.Path == "/v1/messages" &&
		summary.Stream &&
		summary.ThinkingType == "adaptive" &&
		summary.MaxTokensPresent && summary.MaxTokens != 1 &&
		!summary.OutputFormat
}

func formatCaptureCandidates(candidates []captureRequestSummary) string {
	if len(candidates) == 0 {
		return "none"
	}
	items := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, fmt.Sprintf(
			"{index=%d method=%s path=%s model=%s stream=%t thinking_adaptive=%t max_tokens_not_1=%t output_format_absent=%t}",
			candidate.Index,
			captureSafeMethod(candidate.Method),
			captureSafePath(candidate.Path),
			captureSafeModel(candidate.Model),
			candidate.Stream,
			candidate.ThinkingType == "adaptive",
			candidate.MaxTokensPresent && candidate.MaxTokens != 1,
			!candidate.OutputFormat,
		))
	}
	return strings.Join(items, ", ")
}

func captureHeaderPresent(headers http.Header, name string) bool {
	return strings.TrimSpace(getHeaderRaw(headers, name)) != ""
}

func captureStringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func captureSafeBlockType(value string) string {
	if value == "text" {
		return "text"
	}
	if value == "" {
		return "missing"
	}
	return "other"
}

func captureSafeRole(value string) string {
	switch value {
	case "user", "assistant", "system":
		return value
	default:
		return "other"
	}
}

func captureSafeCacheType(value string) string {
	if value == "ephemeral" {
		return value
	}
	if value == "" {
		return "missing"
	}
	return "other"
}

func captureSafeCacheTTL(value string) string {
	switch value {
	case "1h", "5m":
		return value
	case "":
		return "missing"
	default:
		return "other"
	}
}

func captureSafeMethod(value string) string {
	if value == http.MethodPost {
		return http.MethodPost
	}
	return "other"
}

func captureSafePath(value string) string {
	if value == "/v1/messages" {
		return value
	}
	return "other"
}

func captureSafeModel(value string) string {
	if value == "claude-opus-4-8" {
		return value
	}
	return "other"
}

func buildSessionCompanionsAlignmentCaptureReport(now time.Time, trace15, trace16, synthetic captureSessionRequests) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# Claude OAuth 首问 quota/title/main 离线对齐报告\n\n生成时间：%s\n\n", now.Format("2006-01-02 15:04:05 MST"))

	report.WriteString("## 证据边界与脱敏\n\n")
	report.WriteString("- 两份真实 trace 仅在进程内读取并按结构分类；报告不保留原始 header、metadata、CCH、认证值、首问或 title system prompt。\n")
	report.WriteString("- 合成 capture 完整调用 `GatewayService.Forward`，但 HTTPUpstream 是进程内 fake recorder；不会建立网络连接或使用真实账号。\n")
	report.WriteString("- 下文只列角色、字段存在性、固定 header 的预期匹配状态、公开 beta 名称及顺序、block 类型/数量/长度、以及脱敏的一致性布尔值。\n\n")

	report.WriteString("## 会话角色观察\n\n")
	report.WriteString("| 来源 | 捕获请求数 | quota | title | main |\n|---|---:|---|---|---|\n")
	writeCaptureSessionObservation(&report, trace15)
	writeCaptureSessionObservation(&report, trace16)
	writeCaptureSessionObservation(&report, synthetic)

	writeCaptureRoleComparison(&report, "quota", trace15.Quota, synthetic.Quota)
	writeCaptureRoleComparison(&report, "title", trace15.Title, synthetic.Title)
	writeCaptureRoleComparison(&report, "main", trace15.Main, synthetic.Main)

	report.WriteString("## trace16 的伴生请求观察\n\n")
	fmt.Fprintf(&report, "`%s` 共记录 %d 个请求：quota=%s，title=%s，main=%s。", trace16.Source, trace16.RequestCount, captureReportRolePresence(trace16.Quota), captureReportRolePresence(trace16.Title), captureReportRolePresence(trace16.Main))
	if trace16.Title == nil {
		report.WriteString("其中未观察到 title；这仅为观察事实，不能据此证明 title 的状态机、持久化规则或所有会话的发送条件。\n\n")
	} else {
		report.WriteString("该 trace 中也观察到 title；这同样不能单独证明其完整状态机。\n\n")
	}

	report.WriteString("## 固定 Header profile\n\n")
	report.WriteString("下表仅报告固定预期是否匹配，未知 header 原值不会写入报告。\n\n")
	report.WriteString("| Header | quota（trace15 / 合成） | title（trace15 / 合成） | main（trace15 / 合成） |\n|---|---|---|---|\n")
	for _, expected := range captureExpectedFixedHeaders() {
		fmt.Fprintf(
			&report,
			"| `%s` | %s | %s | %s |\n",
			expected.Name,
			captureFixedHeaderPairStatus(trace15.Quota, synthetic.Quota, expected.Name),
			captureFixedHeaderPairStatus(trace15.Title, synthetic.Title, expected.Name),
			captureFixedHeaderPairStatus(trace15.Main, synthetic.Main, expected.Name),
		)
	}
	fmt.Fprintf(
		&report,
		"| `x-claude-code-session-id` | %s | %s | %s |\n",
		captureSessionHeaderPairStatus(trace15.Quota, synthetic.Quota),
		captureSessionHeaderPairStatus(trace15.Title, synthetic.Title),
		captureSessionHeaderPairStatus(trace15.Main, synthetic.Main),
	)
	fmt.Fprintf(
		&report,
		"| `connection` / `accept-encoding` | %s | %s | %s |\n",
		captureTransportHeaderPairStatus(trace15.Quota, synthetic.Quota),
		captureTransportHeaderPairStatus(trace15.Title, synthetic.Title),
		captureTransportHeaderPairStatus(trace15.Main, synthetic.Main),
	)

	report.WriteString("\n## 已对齐或明确保留的差异\n\n")
	report.WriteString("- quota、title、main 均使用各自有序的 beta profile；title 的 json schema 与 `output_config.effort=high` 一并发送。\n")
	report.WriteString("- 三个合成请求均带可解析 metadata，且其 session 与 session header 一致；已删除的两个 helper header 均不出现。\n")
	report.WriteString("- main 的 `max_tokens` 继续按 curl 下游显式值透传；因此它与真实 CLI 的数值不同，这不是本轮伪造为固定值的目标。\n")
	report.WriteString("- tools 仍保持现有策略；本轮不伪造真实 CLI 的完整 tools schema、tool_choice 或 prompt 原文。\n\n")

	report.WriteString("## 上游识别风险\n\n")
	report.WriteString("不能把本离线结果视为不可识别的证明。合成请求仍不生成 CCH，而真实 trace 存在 CCH；这是明确保留的高风险差异。quota/title/main 在代码中只保证逻辑 dispatch，title 与 main 的实际写线微秒级先后、连接排队和并发复用并未由该测试证明。TLS 指纹、HTTP/2、IP/ASN、代理、连接复用以及真实 Claude Code 进程状态均未模拟。伴生请求 claim 的 TTL 为 1 小时；curl 若缺少稳定的会话输入，可能重复发送或漏发首问伴生请求。最后，首问额外增加 quota 与 title 上游流量及其可观察的失败/限流行为。\n")

	return report.String()
}

func writeCaptureSessionObservation(report *strings.Builder, requests captureSessionRequests) {
	fmt.Fprintf(
		report,
		"| `%s` | %d | %s | %s | %s |\n",
		requests.Source,
		requests.RequestCount,
		captureReportRolePresence(requests.Quota),
		captureReportRolePresence(requests.Title),
		captureReportRolePresence(requests.Main),
	)
}

func captureReportRolePresence(summary *captureRequestSummary) string {
	if summary == nil {
		return "未观察到"
	}
	return "存在"
}

func writeCaptureRoleComparison(report *strings.Builder, role string, trace, synthetic *captureRequestSummary) {
	fmt.Fprintf(report, "## %s：trace15 与合成 capture\n\n", role)
	if trace == nil || synthetic == nil {
		fmt.Fprintf(report, "该角色缺失：trace15=%s，合成=%s。\n\n", captureReportRolePresence(trace), captureReportRolePresence(synthetic))
		return
	}
	report.WriteString("| 字段 | trace15 | 合成离线 capture | 结论 |\n|---|---|---|---|\n")
	fmt.Fprintf(report, "| method / path | %s | %s | %s |\n", captureMethodPathStatus(*trace), captureMethodPathStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedMethodPath))
	fmt.Fprintf(report, "| `model` / `stream` | %s | %s | %s |\n", captureModelStreamStatus(*trace), captureModelStreamStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedRoleModelStream))
	fmt.Fprintf(report, "| `max_tokens` | %s | %s | %s |\n", captureMaxTokensStatus(*trace), captureMaxTokensStatus(*synthetic), captureMaxTokensRoleVerdict(role, trace, synthetic))
	fmt.Fprintf(report, "| 有序 `anthropic-beta` | %s | %s | %s |\n", captureRoleBetaStatus(*trace), captureRoleBetaStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedRoleBeta))
	fmt.Fprintf(report, "| authorization | %s | %s | 值已脱敏 |\n", captureAuthorizationStatus(*trace), captureAuthorizationStatus(*synthetic))
	fmt.Fprintf(report, "| metadata/session | %s | %s | %s；值均脱敏 |\n", captureMetadataStatus(*trace), captureMetadataStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedMetadataSession))
	fmt.Fprintf(report, "| identity helper headers | %s | %s | %s |\n", captureDeletedHeaderStatus(*trace), captureDeletedHeaderStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedDeletedHeaders))
	fmt.Fprintf(report, "| output_config | %s | %s | %s |\n", captureSessionOutputConfigStatus(*trace), captureSessionOutputConfigStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedRoleOutputConfig))
	fmt.Fprintf(report, "| thinking/context/temperature | %s | %s | %s |\n", captureBodyControlStatus(*trace), captureBodyControlStatus(*synthetic), captureCaptureRoleVerdict(trace, synthetic, captureExpectedRoleBodyControls))
	fmt.Fprintf(report, "| tools / tool_choice | %s | %s | 仅报告结构 |\n", captureToolsStatus(*trace), captureToolsStatus(*synthetic))
	fmt.Fprintf(report, "| system/messages | %s | %s | 仅报告 block 类型与长度 |\n", capturePromptLayout(*trace), capturePromptLayout(*synthetic))
	fmt.Fprintf(report, "| cache_control | %s | %s | 仅报告位置、类型与 TTL |\n", captureCacheLayout(*trace), captureCacheLayout(*synthetic))
	fmt.Fprintf(report, "| CCH | %s | %s | 合成明确不生成 |\n\n", captureCCHStatus(*trace), captureCCHStatus(*synthetic))
}

func captureMethodPathStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("%s %s", captureSafeMethod(summary.Method), captureSafePath(summary.Path))
}

func captureExpectedMethodPath(summary captureRequestSummary) bool {
	return summary.Method == http.MethodPost && summary.Path == "/v1/messages"
}

func captureExpectedRoleModelStream(summary captureRequestSummary) bool {
	if summary.Model != "claude-opus-4-8" {
		return false
	}
	if summary.Role == "quota" {
		return !summary.Stream
	}
	return summary.Stream
}

func captureCaptureRoleVerdict(trace, synthetic *captureRequestSummary, predicate func(captureRequestSummary) bool) string {
	if trace == nil || synthetic == nil {
		return "缺失"
	}
	if predicate(*trace) && predicate(*synthetic) {
		return "已对齐"
	}
	return "不一致"
}

func captureMaxTokensRoleVerdict(role string, trace, synthetic *captureRequestSummary) string {
	if trace == nil || synthetic == nil || !trace.MaxTokensPresent || !synthetic.MaxTokensPresent {
		return "未验证"
	}
	if trace.MaxTokens == synthetic.MaxTokens {
		return "已对齐"
	}
	if role == "main" {
		return "不同；下游显式值透传"
	}
	return "不一致"
}

func captureExpectedRoleBeta(summary captureRequestSummary) bool {
	return captureStringSlicesEqual(summary.BetaTokens, captureExpectedBetasForRole(summary.Role))
}

func captureExpectedBetasForRole(role string) []string {
	switch role {
	case "quota":
		return expectedCaptureQuotaBetas
	case "title":
		return expectedCaptureTitleBetas
	case "main":
		return expectedCaptureMainBetas
	default:
		return nil
	}
}

func captureRoleBetaStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("%d 项：%s；匹配角色 profile=%t", len(summary.BetaTokens), strings.Join(summary.BetaTokens, ","), captureExpectedRoleBeta(summary))
}

func captureExpectedRoleOutputConfig(summary captureRequestSummary) bool {
	switch summary.Role {
	case "quota":
		return !summary.OutputFormat && summary.OutputEffort == ""
	case "title":
		return summary.OutputFormatType == "json_schema" && summary.OutputEffort == "high"
	case "main":
		return !summary.OutputFormat && summary.OutputEffort == "high"
	default:
		return false
	}
}

func captureSessionOutputConfigStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("format=%s；output_config.effort=%s", summary.OutputFormatType, captureSafeOutputEffort(summary.OutputEffort))
}

func captureExpectedRoleBodyControls(summary captureRequestSummary) bool {
	switch summary.Role {
	case "quota", "title":
		return summary.ThinkingType == "" && !summary.ContextManagementPresent && !summary.TemperaturePresent
	case "main":
		return captureExpectedThinking(summary) && captureExpectedContextManagement(summary) && captureExpectedTemperatureAbsent(summary)
	default:
		return false
	}
}

func captureBodyControlStatus(summary captureRequestSummary) string {
	return fmt.Sprintf(
		"thinking=%s；context_management=%s；temperature=%s",
		captureThinkingStatus(summary),
		captureContextManagementStatus(summary),
		captureTemperatureStatus(summary),
	)
}

func captureFixedHeaderPairStatus(trace, synthetic *captureRequestSummary, name string) string {
	if trace == nil || synthetic == nil {
		return "缺失"
	}
	return fmt.Sprintf("trace=%s；合成=%s", captureFixedHeaderStatusText(*trace, name), captureFixedHeaderStatusText(*synthetic, name))
}

func captureSessionHeaderPairStatus(trace, synthetic *captureRequestSummary) string {
	if trace == nil || synthetic == nil {
		return "缺失"
	}
	return fmt.Sprintf("trace=%s；合成=%s", captureSessionHeaderStatus(*trace), captureSessionHeaderStatus(*synthetic))
}

func captureTransportHeaderPairStatus(trace, synthetic *captureRequestSummary) string {
	if trace == nil || synthetic == nil {
		return "缺失"
	}
	return fmt.Sprintf("trace=%s；合成=%s", captureTransportHeaderPresence(*trace), captureTransportHeaderPresence(*synthetic))
}

func captureSafeBetaTokens(tokens []string) []string {
	safe := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if captureKnownBetaToken(token) {
			safe = append(safe, token)
			continue
		}
		safe = append(safe, "unknown")
	}
	return safe
}

func captureKnownBetaToken(token string) bool {
	for _, expected := range expectedCaptureQuotaBetas {
		if token == expected {
			return true
		}
	}
	for _, expected := range expectedCaptureTitleBetas {
		if token == expected {
			return true
		}
	}
	for _, expected := range expectedCaptureMainBetas {
		if token == expected {
			return true
		}
	}
	return false
}

func captureSafeOutputFormatType(value string) string {
	switch value {
	case "json_schema":
		return value
	case "":
		return "缺失"
	default:
		return "非预期值（已省略）"
	}
}

func captureTraceStatus(traces []captureRequestSummary, render func(captureRequestSummary) string) string {
	items := make([]string, 0, len(traces))
	for _, trace := range traces {
		items = append(items, fmt.Sprintf("%s：%s", trace.Source, render(trace)))
	}
	return strings.Join(items, "<br>")
}

func captureBetaStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("%d 项；精确有序匹配主 profile=%t", summary.BetaTokenCount, summary.BetaMatchesMainProfile)
}

func captureExpectedFieldVerdict(traces []captureRequestSummary, synthetic captureRequestSummary, predicate func(captureRequestSummary) bool) string {
	if captureAllSummariesMatch(traces, synthetic, predicate) {
		return "已对齐"
	}
	return "不一致"
}

func captureExpectedMainBeta(summary captureRequestSummary) bool {
	return summary.BetaTokenCount == len(expectedCaptureMainBetas) && summary.BetaMatchesMainProfile
}

func captureExpectedStainlessOS(summary captureRequestSummary) bool {
	return summary.StainlessOS == claude.DefaultStainlessOS
}

func captureExpectedBillingVersion(summary captureRequestSummary) bool {
	return summary.UserAgentVersion == claude.CLICurrentVersion && summary.BillingVersion == summary.UserAgentVersion
}

func captureExpectedMetadataSession(summary captureRequestSummary) bool {
	return summary.MetadataParseable && summary.MetadataHasThreeFields && summary.MetadataSessionMatchesHeader
}

func captureExpectedDeletedHeaders(summary captureRequestSummary) bool {
	return !summary.ClientRequestIDPresent && !summary.HelperMethodPresent
}

func captureExpectedModelStream(summary captureRequestSummary) bool {
	return summary.Model == "claude-opus-4-8" && summary.Stream
}

func captureExpectedThinking(summary captureRequestSummary) bool {
	return summary.ThinkingType == "adaptive"
}

func captureExpectedOutputConfig(summary captureRequestSummary) bool {
	return summary.OutputEffort == "high" && !summary.OutputFormat
}

func captureExpectedContextManagement(summary captureRequestSummary) bool {
	return summary.ContextManagementPresent &&
		summary.ContextEditCount == 1 &&
		summary.ContextEditType == "clear_thinking_20251015" &&
		summary.ContextEditKeep == "all"
}

func captureExpectedTemperatureAbsent(summary captureRequestSummary) bool {
	return !summary.TemperaturePresent
}

func captureMaxTokensVerdict(traces []captureRequestSummary, synthetic captureRequestSummary) string {
	if len(traces) == 0 || !synthetic.MaxTokensPresent {
		return "未验证"
	}
	for _, trace := range traces {
		if !trace.MaxTokensPresent {
			return "未验证"
		}
		if trace.MaxTokens == synthetic.MaxTokens {
			return "相同；下游显式值已保留"
		}
	}
	return "不同；下游显式值已保留"
}

func captureOSStatus(summary captureRequestSummary) string {
	if summary.StainlessOS == claude.DefaultStainlessOS {
		return claude.DefaultStainlessOS
	}
	return "非预期值（已省略）"
}

func captureBillingStatus(summary captureRequestSummary) string {
	uaVersion := captureSafeCLIVersion(summary.UserAgentVersion)
	return fmt.Sprintf("UA=%s；billing/UA 一致=%t", uaVersion, summary.BillingVersion != "" && summary.BillingVersion == summary.UserAgentVersion)
}

func captureSafeCLIVersion(version string) string {
	if version == claude.CLICurrentVersion {
		return version
	}
	return "非预期值（已省略）"
}

func captureMetadataStatus(summary captureRequestSummary) string {
	return fmt.Sprintf(
		"可解析=%t；device_id=[redacted]，account_uuid=[redacted]，session_id=[redacted]；session/header 一致=%t",
		summary.MetadataParseable && summary.MetadataHasThreeFields,
		summary.MetadataSessionMatchesHeader,
	)
}

func captureDeletedHeaderStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("x-client-request-id=%t；x-stainless-helper-method=%t", summary.ClientRequestIDPresent, summary.HelperMethodPresent)
}

func captureModelStreamStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("model=%s；stream=%t", captureSafeModel(summary.Model), summary.Stream)
}

func captureMaxTokensStatus(summary captureRequestSummary) string {
	if !summary.MaxTokensPresent {
		return "缺失"
	}
	return fmt.Sprintf("%d", summary.MaxTokens)
}

func captureThinkingStatus(summary captureRequestSummary) string {
	if summary.ThinkingType == "adaptive" {
		return "adaptive"
	}
	if summary.ThinkingType == "" {
		return "缺失"
	}
	return "非预期值（已省略）"
}

func captureOutputConfigStatus(summary captureRequestSummary) string {
	return fmt.Sprintf("effort=%s；format=%s", captureSafeOutputEffort(summary.OutputEffort), capturePresenceStatus(summary.OutputFormat))
}

func captureContextManagementStatus(summary captureRequestSummary) string {
	if !summary.ContextManagementPresent {
		return "缺失"
	}
	return fmt.Sprintf(
		"edits=%d；type=%s；keep=%s",
		summary.ContextEditCount,
		captureSafeContextEditType(summary.ContextEditType),
		captureSafeContextEditKeep(summary.ContextEditKeep),
	)
}

func captureTemperatureStatus(summary captureRequestSummary) string {
	return capturePresenceStatus(summary.TemperaturePresent)
}

func capturePresenceStatus(present bool) string {
	if present {
		return "存在"
	}
	return "缺失"
}

func captureSafeOutputEffort(value string) string {
	switch value {
	case "high":
		return value
	case "":
		return "缺失"
	default:
		return "非预期值（已省略）"
	}
}

func captureSafeContextEditType(value string) string {
	switch value {
	case "clear_thinking_20251015":
		return value
	case "":
		return "缺失"
	default:
		return "非预期值（已省略）"
	}
}

func captureSafeContextEditKeep(value string) string {
	switch value {
	case "all":
		return value
	case "":
		return "缺失"
	default:
		return "非预期值（已省略）"
	}
}

func captureCacheLayout(summary captureRequestSummary) string {
	if len(summary.CacheControls) == 0 {
		return "0 个"
	}
	items := make([]string, 0, len(summary.CacheControls))
	for _, cacheControl := range summary.CacheControls {
		items = append(items, fmt.Sprintf("%s:{type=%s,ttl=%s}", cacheControl.Location, cacheControl.Type, cacheControl.TTL))
	}
	return fmt.Sprintf("%d 个：%s", len(items), strings.Join(items, "; "))
}

func capturePromptLayout(summary captureRequestSummary) string {
	system := make([]string, 0, len(summary.SystemBlocks))
	for _, block := range summary.SystemBlocks {
		system = append(system, fmt.Sprintf("%s:%s,len=%d", block.Location, block.Type, block.TextLength))
	}
	messages := make([]string, 0, len(summary.Messages))
	for index, message := range summary.Messages {
		messages = append(messages, fmt.Sprintf("messages[%d]:role=%s,content=%s,types=%v,lengths=%v", index, message.Role, message.ContentShape, message.BlockTypes, message.TextLengths))
	}
	return fmt.Sprintf("system[%s]；messages[%s]", strings.Join(system, "; "), strings.Join(messages, "; "))
}

func captureAuthorizationStatus(summary captureRequestSummary) string {
	if summary.AuthorizationPresent {
		return "Bearer [redacted]"
	}
	return "缺失"
}

func captureToolsStatus(summary captureRequestSummary) string {
	if summary.ToolsPresent && summary.ToolsCount == 0 && !summary.ToolChoicePresent {
		return "tools=[]；tool_choice=缺失"
	}
	if summary.ToolsPresent {
		return fmt.Sprintf("tools 数量=%d；tool_choice=%t", summary.ToolsCount, summary.ToolChoicePresent)
	}
	return fmt.Sprintf("tools 缺失；tool_choice=%t", summary.ToolChoicePresent)
}

func captureCCHStatus(summary captureRequestSummary) string {
	if summary.BillingHasCCH {
		return "存在（值已脱敏）"
	}
	return "不存在"
}

func captureFixedHeaderStatusText(summary captureRequestSummary, name string) string {
	status, ok := summary.FixedHeaders[name]
	if !ok || !status.Present {
		return "缺失"
	}
	if status.MatchesExpected {
		return "与预期一致"
	}
	return "非预期值（已省略）"
}

func captureFixedHeaderVerdict(traces []captureRequestSummary, synthetic captureRequestSummary, name string) string {
	if captureAllSummariesMatch(traces, synthetic, func(summary captureRequestSummary) bool {
		status, ok := summary.FixedHeaders[name]
		return ok && status.Present && status.MatchesExpected
	}) {
		return "已对齐"
	}
	return "不一致"
}

func captureSessionHeaderStatus(summary captureRequestSummary) string {
	if !summary.SessionHeaderPresent {
		return "缺失"
	}
	if summary.MetadataSessionMatchesHeader {
		return "存在；与 metadata.session 一致（值已脱敏）"
	}
	return "存在；与 metadata.session 不一致（值已脱敏）"
}

func captureSessionHeaderVerdict(traces []captureRequestSummary, synthetic captureRequestSummary) string {
	if captureAllSummariesMatch(traces, synthetic, func(summary captureRequestSummary) bool {
		return summary.MetadataSessionMatchesHeader
	}) {
		return "已对齐"
	}
	return "不一致"
}

func captureTransportHeaderPresence(summary captureRequestSummary) string {
	return fmt.Sprintf("connection=%s；accept-encoding=%s", capturePresenceStatus(summary.ConnectionPresent), capturePresenceStatus(summary.AcceptEncodingPresent))
}

func captureAllSummariesMatch(traces []captureRequestSummary, synthetic captureRequestSummary, predicate func(captureRequestSummary) bool) bool {
	if len(traces) == 0 || !predicate(synthetic) {
		return false
	}
	for _, trace := range traces {
		if !predicate(trace) {
			return false
		}
	}
	return true
}

func TestBuildSessionCompanionsAlignmentCaptureReportListsRolesAndRisks(t *testing.T) {
	newSummary := func(role string) *captureRequestSummary {
		summary := &captureRequestSummary{
			Role:                         role,
			Model:                        "claude-opus-4-8",
			MaxTokensPresent:             true,
			AuthorizationPresent:         true,
			SessionHeaderPresent:         true,
			MetadataParseable:            true,
			MetadataHasThreeFields:       true,
			MetadataSessionMatchesHeader: true,
			FixedHeaders:                 map[string]captureFixedHeaderStatus{},
		}
		for _, expected := range captureExpectedFixedHeaders() {
			summary.FixedHeaders[expected.Name] = captureFixedHeaderStatus{Present: true, MatchesExpected: true}
		}
		switch role {
		case "quota":
			summary.MaxTokens = 1
			summary.BetaTokens = expectedCaptureQuotaBetas
		case "title":
			summary.Stream = true
			summary.MaxTokens = 64000
			summary.OutputFormat = true
			summary.OutputFormatType = "json_schema"
			summary.OutputEffort = "high"
			summary.BetaTokens = expectedCaptureTitleBetas
		case "main":
			summary.Stream = true
			summary.MaxTokens = 1024
			summary.ThinkingType = "adaptive"
			summary.OutputEffort = "high"
			summary.ContextManagementPresent = true
			summary.ContextEditCount = 1
			summary.ContextEditType = "clear_thinking_20251015"
			summary.ContextEditKeep = "all"
			summary.BetaTokens = expectedCaptureMainBetas
		}
		return summary
	}
	trace15 := captureSessionRequests{
		Source:       "trace15.json",
		RequestCount: 3,
		Quota:        newSummary("quota"),
		Title:        newSummary("title"),
		Main:         newSummary("main"),
	}
	trace16 := captureSessionRequests{
		Source:       "trace16.json",
		RequestCount: 2,
		Quota:        newSummary("quota"),
		Main:         newSummary("main"),
	}
	synthetic := captureSessionRequests{
		Source:       "offline synthetic capture",
		RequestCount: 3,
		Quota:        newSummary("quota"),
		Title:        newSummary("title"),
		Main:         newSummary("main"),
	}

	report := buildSessionCompanionsAlignmentCaptureReport(time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC), trace15, trace16, synthetic)
	for _, section := range []string{
		"## quota：trace15 与合成 capture",
		"## title：trace15 与合成 capture",
		"## main：trace15 与合成 capture",
		"output_config.effort=high",
		"仅为观察事实",
		"## 固定 Header profile",
		"| `connection` / `accept-encoding` |",
		"## 上游识别风险",
		"CCH",
		"TLS 指纹、HTTP/2、IP/ASN",
		"TTL 为 1 小时",
	} {
		require.Contains(t, report, section)
	}
	require.NotContains(t, report, "cch=")
}

func TestWriteCaptureReportAtomicallyPreservesConcurrentReport(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "comparison.md")
	original := []byte("pre-existing comparison report")
	previousHook := captureReportBeforePublishHook
	captureReportBeforePublishHook = func() {
		require.NoError(t, os.WriteFile(reportPath, original, captureArtifactMode))
	}
	t.Cleanup(func() { captureReportBeforePublishHook = previousHook })

	published, err := writeCaptureReportAtomically(reportPath, []byte("new comparison report"))
	require.False(t, published)
	require.Error(t, err)
	actual, readErr := os.ReadFile(reportPath)
	require.NoError(t, readErr)
	require.Equal(t, original, actual)
	entries, readDirErr := os.ReadDir(filepath.Dir(reportPath))
	require.NoError(t, readDirErr)
	require.Len(t, entries, 1, "a rejected publish must remove its temporary report")
	require.Equal(t, "comparison.md", entries[0].Name())
}

func writeCaptureReportAtomically(reportPath string, contents []byte) (published bool, err error) {
	if _, err := os.Lstat(reportPath); err == nil {
		return false, fmt.Errorf("refusing to overwrite existing comparison report")
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect comparison report destination: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(reportPath), ".same_version_comparison_*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temporary comparison report: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	temporaryRemoved := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if !temporaryRemoved {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(captureArtifactMode); err != nil {
		return false, fmt.Errorf("set temporary comparison report permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return false, fmt.Errorf("write temporary comparison report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return false, fmt.Errorf("sync temporary comparison report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close temporary comparison report: %w", err)
	}
	closed = true

	if captureReportBeforePublishHook != nil {
		captureReportBeforePublishHook()
	}
	if err := os.Link(temporaryPath, reportPath); err != nil {
		return false, fmt.Errorf("atomically publish comparison report without overwrite: %w", err)
	}
	published = true
	if err := os.Remove(temporaryPath); err != nil {
		return true, fmt.Errorf("remove temporary comparison report after publish: %w", err)
	}
	temporaryRemoved = true
	return true, nil
}
