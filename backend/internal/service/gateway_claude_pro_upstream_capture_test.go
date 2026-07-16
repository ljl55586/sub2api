//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	fingerprint *Fingerprint
}

func (c *captureIdentityCache) GetFingerprint(context.Context, int64) (*Fingerprint, error) {
	if c.fingerprint == nil {
		return nil, nil
	}
	copy := *c.fingerprint
	return &copy, nil
}

func (c *captureIdentityCache) SetFingerprint(_ context.Context, _ int64, fp *Fingerprint) error {
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

// captureOfflineUpstreamRecorder delegates to the in-process Anthropic test
// recorder. It records the final wire request without ever opening a socket.
type captureOfflineUpstreamRecorder struct {
	*anthropicHTTPUpstreamRecorder
	calls int
}

func (u *captureOfflineUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.calls++
	return u.anthropicHTTPUpstreamRecorder.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *captureOfflineUpstreamRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return u.anthropicHTTPUpstreamRecorder.DoWithTLS(req, proxyURL, accountID, accountConcurrency, profile)
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
	Method             string
	Path               string
	Model              string
	Stream             bool
	MaxTokens          int64
	MaxTokensPresent   bool
	ThinkingType       string
	OutputFormat       bool
	OutputEffort       string
	TemperaturePresent bool

	ContextManagementPresent bool
	ContextEditCount         int
	ContextEditType          string
	ContextEditKeep          string

	BetaTokenCount         int
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
	upstream := &captureOfflineUpstreamRecorder{
		anthropicHTTPUpstreamRecorder: &anthropicHTTPUpstreamRecorder{
			resp: claudeOAuthNoToolsProfileResponseForTest(parsed.Stream),
		},
	}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       NewSettingService(&gatewayTTLSettingRepo{data: gatewaySettings}, cfg),
		identityService:      NewIdentityService(cache),
	}

	trace20260715, err := selectUniqueCaptureTraceMainRequest(captureTrace20260715)
	require.NoError(t, err)
	trace20260716, err := selectUniqueCaptureTraceMainRequest(captureTrace20260716)
	require.NoError(t, err)

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	runID := now.Format("20060102_150405")
	logPath := filepath.Join(captureLogDir, "log_"+runID+".log")
	reportPath := filepath.Join(captureLogDir, "same_version_comparison_after_no_tools_alignment_"+runID+".md")
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
	require.Equal(t, 1, upstream.calls, "the capture must call only the local fake HTTPUpstream once")
	require.NotNil(t, upstream.lastReq)
	require.NotEmpty(t, upstream.lastBody)
	assertAlignedCaptureRequest(t, upstream.lastReq, upstream.lastBody)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "message_start")
	require.Contains(t, recorder.Body.String(), "event: message_stop")
	require.NoError(t, file.Sync())

	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotEmpty(t, logBytes)
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

	synthetic, err := summarizeCaptureRequest("offline synthetic capture", -1, upstream.lastReq.Method, upstream.lastReq.URL.String(), upstream.lastReq.Header, upstream.lastBody)
	require.NoError(t, err)
	report := buildNoToolsAlignmentCaptureReport(now, []captureRequestSummary{trace20260715, trace20260716}, synthetic)
	require.Contains(t, report, "Bearer [redacted]")
	require.Contains(t, report, "device_id=[redacted]")
	require.Contains(t, report, "## 上游识别风险")
	require.Contains(t, report, "部分对齐；仍有结构差异")
	require.Contains(t, report, "| `model` / `stream` |")
	require.Contains(t, report, "| `max_tokens` |")
	require.Contains(t, report, "不同；下游显式值已保留")
	require.Contains(t, report, "| `thinking.type` |")
	require.Contains(t, report, "| `output_config` |")
	require.Contains(t, report, "effort=high；format=缺失")
	require.Contains(t, report, "| `context_management.edits[0]` |")
	require.Contains(t, report, "edits=1；type=clear_thinking_20251015；keep=all")
	require.Contains(t, report, "| `temperature` |")
	require.Contains(t, report, "## 固定 Header profile")
	require.Contains(t, report, "| `accept` |")
	require.Contains(t, report, "| `x-claude-code-session-id` |")
	require.Contains(t, report, "| `connection` / `accept-encoding` |")
	require.NotContains(t, report, token)
	require.NotContains(t, report, "11111111-2222-4333-8444-555555555555")
	require.NotContains(t, report, "cch=")
	require.NotContains(t, report, "Hello, please introduce yourself.")
	_, err = writeCaptureReportAtomically(reportPath, []byte(report))
	require.NoError(t, err)
	reportInfo, err := os.Stat(reportPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(captureArtifactMode), reportInfo.Mode().Perm())
	t.Logf("CAPTURE_LOG=%s", logPath)
	t.Logf("COMPARISON_REPORT=%s", reportPath)
}

func assertAlignedCaptureRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	require.Equal(t, claudeAPIURL, req.URL.String())
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
	require.Equal(t, "MacOS", getHeaderRaw(req.Header, "x-stainless-os"))
	require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.NotEmpty(t, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	require.Equal(t, expectedCaptureMainBetas, parseAnthropicBetaHeader(getHeaderRaw(req.Header, "anthropic-beta")))

	billingText := gjson.GetBytes(body, "system.0.text").String()
	require.Equal(t, "cc_version="+ExtractCLIVersion(getHeaderRaw(req.Header, "User-Agent")), ccVersionInBillingRe.FindString(billingText))
	require.Contains(t, billingText, "cc_entrypoint=cli;")
	require.NotContains(t, billingText, "cch=")
	require.False(t, gjson.GetBytes(body, "system.0.cache_control").Exists())

	userID := gjson.GetBytes(body, "metadata.user_id").String()
	require.True(t, gjson.Valid(userID))
	parsedUserID := ParseMetadataUserID(userID)
	require.NotNil(t, parsedUserID)
	require.NotEmpty(t, parsedUserID.DeviceID)
	require.Equal(t, "11111111-2222-4333-8444-555555555555", parsedUserID.AccountUUID)
	require.Equal(t, parsedUserID.SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))

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

func selectUniqueCaptureTraceMainRequest(tracePath string) (captureRequestSummary, error) {
	source := filepath.Base(tracePath)
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		return captureRequestSummary{}, fmt.Errorf("read trace %s: %w", source, err)
	}

	var entries []captureTraceEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return captureRequestSummary{}, fmt.Errorf("parse trace %s: invalid JSON", source)
	}
	if len(entries) == 0 {
		return captureRequestSummary{}, fmt.Errorf("trace %s has no requests", source)
	}

	candidates := make([]captureRequestSummary, 0, 1)
	for index, entry := range entries {
		headers := make(http.Header, len(entry.Request.Headers))
		for key, value := range entry.Request.Headers {
			headers.Set(key, value)
		}
		summary, err := summarizeCaptureRequest(source, index, entry.Request.Method, entry.Request.URL, headers, entry.Request.Body)
		if err != nil {
			return captureRequestSummary{}, fmt.Errorf("trace %s request index %d cannot be summarized safely: %w", source, index, err)
		}
		summary.TraceRequestCount = len(entries)
		if isCaptureMainRequest(summary) {
			candidates = append(candidates, summary)
		}
	}

	if len(candidates) != 1 {
		return captureRequestSummary{}, fmt.Errorf(
			"trace %s expected exactly one main-request candidate; candidates=%s",
			source,
			formatCaptureCandidates(candidates),
		)
	}
	return candidates[0], nil
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

func buildNoToolsAlignmentCaptureReport(now time.Time, traces []captureRequestSummary, synthetic captureRequestSummary) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# Claude OAuth 无工具主请求离线对齐报告\n\n生成时间：%s\n\n", now.Format("2006-01-02 15:04:05 MST"))
	report.WriteString("## 证据边界与脱敏\n\n")
	report.WriteString("- 两份真实 trace 仅在进程内读取，用结构条件唯一选择主请求；报告不会写入其原始 header 或 body。\n")
	report.WriteString("- authorization 统一表示为 `Bearer [redacted]`；metadata 三元组的三个值均表示为 `[redacted]`；CCH 仅报告存在性；system/messages 原文永不输出。\n")
	report.WriteString("- 合成 capture 只使用测试账号、测试 token 与固定输入；它完整调用 `GatewayService.Forward`，但 HTTPUpstream 是进程内 fake recorder，不建立网络连接。\n\n")

	report.WriteString("## 真实主请求的结构化选择\n\n")
	report.WriteString("| trace | 数组索引 | 满足的唯一条件 | 会话请求数 |\n|---|---:|---|---:|\n")
	for _, trace := range traces {
		fmt.Fprintf(&report, "| `%s` | %d | POST /v1/messages；stream=true；thinking.type=adaptive；max_tokens!=1；无 output_config.format | %d |\n", trace.Source, trace.Index, trace.TraceRequestCount)
	}

	report.WriteString("\n## 逐字段白名单比较\n\n")
	report.WriteString("| 字段 | 真实 trace（两份） | 合成离线 capture | 结论 |\n|---|---|---|---|\n")
	fmt.Fprintf(&report, "| 有序主请求 beta | %s | %d 项；与主 profile 精确有序匹配=%t | %s |\n", captureTraceStatus(traces, captureBetaStatus), synthetic.BetaTokenCount, synthetic.BetaMatchesMainProfile, captureExpectedFieldVerdict(traces, synthetic, captureExpectedMainBeta))
	fmt.Fprintf(&report, "| `x-stainless-os` | %s | %s | %s |\n", captureTraceStatus(traces, captureOSStatus), captureOSStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedStainlessOS))
	fmt.Fprintf(&report, "| billing / 最终 UA semver | %s | %s | %s |\n", captureTraceStatus(traces, captureBillingStatus), captureBillingStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedBillingVersion))
	fmt.Fprintf(&report, "| metadata/session | %s | %s | %s；三元组值均脱敏 |\n", captureTraceStatus(traces, captureMetadataStatus), captureMetadataStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedMetadataSession))
	fmt.Fprintf(&report, "| 删除的 identity helper headers | %s | x-client-request-id=%t；x-stainless-helper-method=%t | %s |\n", captureTraceStatus(traces, captureDeletedHeaderStatus), synthetic.ClientRequestIDPresent, synthetic.HelperMethodPresent, captureExpectedFieldVerdict(traces, synthetic, captureExpectedDeletedHeaders))
	fmt.Fprintf(&report, "| `model` / `stream` | %s | %s | %s |\n", captureTraceStatus(traces, captureModelStreamStatus), captureModelStreamStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedModelStream))
	fmt.Fprintf(&report, "| `max_tokens` | %s | %s | %s |\n", captureTraceStatus(traces, captureMaxTokensStatus), captureMaxTokensStatus(synthetic), captureMaxTokensVerdict(traces, synthetic))
	fmt.Fprintf(&report, "| `thinking.type` | %s | %s | %s |\n", captureTraceStatus(traces, captureThinkingStatus), captureThinkingStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedThinking))
	fmt.Fprintf(&report, "| `output_config` | %s | %s | %s；与 effort beta 自洽 |\n", captureTraceStatus(traces, captureOutputConfigStatus), captureOutputConfigStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedOutputConfig))
	fmt.Fprintf(&report, "| `context_management.edits[0]` | %s | %s | %s；与 context-management beta 自洽 |\n", captureTraceStatus(traces, captureContextManagementStatus), captureContextManagementStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedContextManagement))
	fmt.Fprintf(&report, "| `temperature` | %s | %s | %s |\n", captureTraceStatus(traces, captureTemperatureStatus), captureTemperatureStatus(synthetic), captureExpectedFieldVerdict(traces, synthetic, captureExpectedTemperatureAbsent))
	fmt.Fprintf(&report, "| cache_control 布局 | %s | %s | 部分对齐；仍有结构差异（仅输出位置/类型/TTL） |\n", captureTraceStatus(traces, captureCacheLayout), captureCacheLayout(synthetic))
	fmt.Fprintf(&report, "| system/messages 结构 | %s | %s | 部分对齐；仍有结构差异（文本只列 block 类型与长度） |\n", captureTraceStatus(traces, capturePromptLayout), capturePromptLayout(synthetic))
	fmt.Fprintf(&report, "| authorization | %s | %s | 值已脱敏 |\n", captureTraceStatus(traces, captureAuthorizationStatus), captureAuthorizationStatus(synthetic))
	fmt.Fprintf(&report, "| tools / tool_choice | %s | %s | 有意保留差异 |\n", captureTraceStatus(traces, captureToolsStatus), captureToolsStatus(synthetic))
	fmt.Fprintf(&report, "| CCH | %s | %s | 有意不生成或复制 |\n", captureTraceStatus(traces, captureCCHStatus), captureCCHStatus(synthetic))

	report.WriteString("\n## 固定 Header profile\n\n")
	report.WriteString("下表只输出与固定预期的匹配状态；不保留任意未知 header 原值。\n\n")
	report.WriteString("| Header | 真实 trace（两份） | 合成离线 capture | 结论 |\n|---|---|---|---|\n")
	for _, expected := range captureExpectedFixedHeaders() {
		fmt.Fprintf(
			&report,
			"| `%s` | %s | %s | %s |\n",
			expected.Name,
			captureTraceStatus(traces, func(summary captureRequestSummary) string {
				return captureFixedHeaderStatusText(summary, expected.Name)
			}),
			captureFixedHeaderStatusText(synthetic, expected.Name),
			captureFixedHeaderVerdict(traces, synthetic, expected.Name),
		)
	}
	fmt.Fprintf(&report, "| `x-claude-code-session-id` | %s | %s | %s |\n", captureTraceStatus(traces, captureSessionHeaderStatus), captureSessionHeaderStatus(synthetic), captureSessionHeaderVerdict(traces, synthetic))
	fmt.Fprintf(&report, "| `connection` / `accept-encoding` | %s | %s | 离线 http.Request 不验证实际 transport 层 |\n", captureTraceStatus(traces, captureTransportHeaderPresence), captureTransportHeaderPresence(synthetic))

	report.WriteString("\n## 已修复差异\n\n")
	report.WriteString("- 主请求 beta 使用精确的 11-token 顺序，且合成请求声明 MacOS。\n")
	report.WriteString("- billing `cc_version` 与最终发送的 User-Agent semver 一致；合成 billing block 不带 CCH。\n")
	report.WriteString("- `thinking.type=adaptive`、`output_config.effort=high`、`context_management.edits[0]={type=clear_thinking_20251015,keep=all}` 和缺失的 `temperature` 均与两份真实主请求一致。\n")
	report.WriteString("- 合成无工具 profile 形成 3 个 `ephemeral/1h` cache_control：两个 system block 与一个用户 text block；这只对齐数量、类型与 TTL，不宣称位置或完整 prompt 结构一致。\n")
	report.WriteString("- metadata 三元组可解析，且其 session 与 `x-claude-code-session-id` 一致；两个已删除 helper header 均不存在。\n")

	report.WriteString("\n## 有意保留差异\n\n")
	report.WriteString("- 合成请求的 `tools` 是显式空数组、没有 `tool_choice`，不会伪造工具 schema、role-system 或 system-reminder。\n")
	report.WriteString("- 合成 capture 只有一个主请求，不伪造真实会话中的 quota/title 等伴生请求。\n")
	report.WriteString("- `max_tokens` 按下游显式值透传：本次 curl 合成输入为 1024，真实 Claude Code 主请求为 64000；这不是伪造为固定 CLI 值的目标。\n")
	report.WriteString("- 不生成、重放或复制 CCH；真实 trace 中 CCH 的具体值不写入本报告。\n")
	report.WriteString("- 本轮不复制真实 `system/messages` 的完整结构：真实 trace 的用户 cache 位于 `content[1]`，合成请求位于 `content[0]`；真实 role=system 内容和 system[2] 文本长度也可能不同。这些是有意保留的结构差异，不属于“已修复”。\n")

	report.WriteString("\n## 上游识别风险\n\n")
	report.WriteString("不能据此离线对齐报告宣称请求不可被上游识别。真实主请求仍带有 CCH，而合成无工具请求不生成 CCH；合成请求也没有真实 tools schema、tool_choice 或 quota/title 等伴生请求序列。虽然双方各有 3 个 `ephemeral/1h` cache_control，真实用户 cache 在 `content[1]`、合成在 `content[0]`，且真实 role=system 内容与 system[2] 长度仍可不同。TLS、HTTP/2、IP/ASN、连接复用、`connection` 与 `accept-encoding` 仍未经实际写线验证。这些均是尚未消除的上游识别风险，而非已修复项。\n")

	report.WriteString("\n## 传输层未知差异\n\n")
	report.WriteString("TLS、HTTP/2 行为、IP/ASN、连接复用、`connection` 与 `accept-encoding` 由实际写线或网络环境决定；离线 http.Request capture 不对这些项目作一致性结论。\n")
	return report.String()
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

func TestBuildNoToolsAlignmentCaptureReportListsBodyControlFields(t *testing.T) {
	trace := captureRequestSummary{
		Source:                   "trace.json",
		Model:                    "claude-opus-4-8",
		Stream:                   true,
		MaxTokensPresent:         true,
		MaxTokens:                64000,
		ThinkingType:             "adaptive",
		OutputFormat:             false,
		OutputEffort:             "high",
		ContextManagementPresent: true,
		ContextEditCount:         1,
		ContextEditType:          "clear_thinking_20251015",
		ContextEditKeep:          "all",
		TemperaturePresent:       false,
		ToolsPresent:             true,
		ToolsCount:               30,
	}
	synthetic := trace
	synthetic.Source = "offline synthetic capture"
	synthetic.MaxTokens = 1024
	synthetic.ToolsCount = 0

	report := buildNoToolsAlignmentCaptureReport(time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC), []captureRequestSummary{trace}, synthetic)
	for _, field := range []string{
		"| `model` / `stream` |",
		"| `max_tokens` |",
		"| `thinking.type` |",
		"| `output_config` |",
		"| `context_management.edits[0]` |",
		"| `temperature` |",
	} {
		require.Contains(t, report, field)
	}
	require.Contains(t, report, "| `max_tokens` | trace.json：64000 | 1024 |")
	require.Contains(t, report, "| `thinking.type` | trace.json：adaptive | adaptive |")
	require.Contains(t, report, "| `output_config` | trace.json：effort=high；format=缺失 | effort=high；format=缺失 |")
	require.Contains(t, report, "| `context_management.edits[0]` | trace.json：edits=1；type=clear_thinking_20251015；keep=all | edits=1；type=clear_thinking_20251015；keep=all |")
	require.Contains(t, report, "| `temperature` | trace.json：缺失 | 缺失 |")
	require.Contains(t, report, "## 固定 Header profile")
	require.Contains(t, report, "| `accept` |")
	require.Contains(t, report, "| `anthropic-version` |")
	require.Contains(t, report, "| `x-claude-code-session-id` |")
	require.Contains(t, report, "| `connection` / `accept-encoding` |")
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
