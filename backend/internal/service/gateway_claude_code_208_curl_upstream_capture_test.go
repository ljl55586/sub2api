//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const captureClaudeCode208CurlOptInEnv = "SUB2API_RUN_CLAUDE_208_CURL_CAPTURE"

// TestCaptureClaudeCode208CurlUpstreamRequests runs the production
// GatewayService.Forward construction path in-process. The HTTPUpstream is an
// in-memory recorder, so the finalized requests are captured without resolving
// or connecting to api.anthropic.com.
func TestCaptureClaudeCode208CurlUpstreamRequests(t *testing.T) {
	if os.Getenv(captureClaudeCode208CurlOptInEnv) != "1" {
		t.Skipf("set %s=1 to create the developer-local offline capture", captureClaudeCode208CurlOptInEnv)
	}

	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	clientBody := []byte(`{` +
		`"model":"claude-opus-4-8",` +
		`"max_tokens":64000,` +
		`"stream":true,` +
		`"messages":[{"role":"user","content":"Hello, please introduce yourself."}]` +
		`}`)

	clientRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(clientRecorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.7.1")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Anthropic-Version", "2023-06-01")
	c.Request.Header.Set(claudeOAuthSessionInitHeader, "true")
	c.Request.Header.Set(claudeCode208CWDHeader, "/Users/ling/sub2api")
	c.Request.Header.Set(claudeCode208Context1MHeader, "true")

	parsed, err := ParseGatewayRequest(NewRequestBodyRef(clientBody), PlatformAnthropic)
	require.NoError(t, err)
	parsed.SessionContext = &SessionContext{
		ClientIP:  "127.0.0.1",
		UserAgent: "curl/8.7.1",
		APIKeyID:  7208,
	}

	const fakeToken = "OFFLINE-CAPTURE-NOT-A-REAL-TOKEN"
	account := &Account{
		ID:          1208,
		Name:        "claude-code-2.1.208-offline-capture",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": fakeToken,
			"email":        "offline-capture@example.invalid",
		},
		Extra: map[string]any{
			"account_uuid": "22222222-1208-4208-8208-222222222222",
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

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	require.NoError(t, os.MkdirAll(captureLogDir, 0o755))
	logPath := filepath.Join(captureLogDir, "log_"+now.Format("20060102_150405")+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, captureArtifactMode)
	require.NoError(t, err, "capture log name must be new; never append to an existing artifact")
	t.Cleanup(func() {
		svc.debugGatewayBodyFile.Store(nil)
		_ = file.Close()
	})
	require.NoError(t, file.Chmod(captureArtifactMode))
	svc.debugGatewayBodyFile.Store(file)

	writeClaudeCode208CurlCaptureBanner(t, file, now, account, parsed.Model)
	result, err := svc.Forward(ctx, c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, upstream.WaitForCalls(3, time.Second), "offline capture must observe quota, title, and main")

	snapshots := upstream.Snapshots()
	require.Len(t, snapshots, 3)
	quota := findCaptureWireSnapshot(t, snapshots, "quota")
	title := findCaptureWireSnapshot(t, snapshots, "title")
	main := findCaptureWireSnapshot(t, snapshots, "main")
	assertClaudeCode208CurlCaptureCommon(t, quota)
	assertClaudeCode208CurlCaptureCommon(t, title)
	assertClaudeCode208CurlCaptureCommon(t, main)
	assertCaptureWireSessionsMatch(t, quota, title, main)

	require.Equal(t, claude.ClaudeCodeDirectSmallModel, gjson.GetBytes(quota.Body, "model").String())
	require.Equal(t, int64(1), gjson.GetBytes(quota.Body, "max_tokens").Int())
	require.Equal(t, "quota", gjson.GetBytes(quota.Body, "messages.0.content").String())

	require.Equal(t, claude.ClaudeCodeDirectSmallModel, gjson.GetBytes(title.Body, "model").String())
	require.Equal(t, int64(32000), gjson.GetBytes(title.Body, "max_tokens").Int())
	require.Equal(t, "disabled", gjson.GetBytes(title.Body, "thinking.type").String())
	require.Equal(t, "json_schema", gjson.GetBytes(title.Body, "output_config.format.type").String())
	require.False(t, gjson.GetBytes(title.Body, "output_config.effort").Exists())

	require.Equal(t, "claude-opus-4-8", gjson.GetBytes(main.Body, "model").String())
	require.Equal(t, int64(64000), gjson.GetBytes(main.Body, "max_tokens").Int())
	require.Equal(t, "adaptive", gjson.GetBytes(main.Body, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(main.Body, "output_config.effort").String())
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(main.Body, "context_management.edits.0.type").String())
	require.Equal(t, "all", gjson.GetBytes(main.Body, "context_management.edits.0.keep").String())
	require.Len(t, gjson.GetBytes(main.Body, "messages.0.content").Array(), 2)
	reminder := gjson.GetBytes(main.Body, "messages.0.content.0.text").String()
	require.Contains(t, reminder, "<system-reminder>")
	require.True(t, strings.HasSuffix(reminder, "</system-reminder>\n\n"))
	require.False(t, strings.HasSuffix(reminder, "</system-reminder>\n\n\n"))
	require.Equal(t, "ephemeral", gjson.GetBytes(main.Body, "messages.0.content.1.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(main.Body, "messages.0.content.1.cache_control.ttl").String())
	require.Equal(t, 3, countCaptureCacheControls(t, main.Body))

	require.NoError(t, file.Sync())
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logText := string(logBytes)
	require.Contains(t, logText, "CLIENT_ORIGINAL")
	require.Contains(t, logText, "UPSTREAM_SESSION_COMPANION_QUOTA")
	require.Contains(t, logText, "UPSTREAM_SESSION_COMPANION_TITLE")
	require.Contains(t, logText, "UPSTREAM_FORWARD")
	require.Contains(t, logText, "claude-cli/2.1.208 (external, cli)")
	require.Contains(t, logText, "X-Stainless-Runtime-Version: v26.3.0")
	require.NotContains(t, logText, fakeToken)
	require.Contains(t, logText, "Bearer [redacted]")
	info, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(captureArtifactMode), info.Mode().Perm())

	t.Logf("CAPTURE_LOG=%s", logPath)
}

func assertClaudeCode208CurlCaptureCommon(t *testing.T, snapshot captureWireSnapshot) {
	t.Helper()
	require.NotNil(t, snapshot.Request)
	require.Equal(t, claudeAPIURL, snapshot.Request.URL.String())
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(snapshot.Request.Header, "user-agent"))
	require.Equal(t, "v26.3.0", getHeaderRaw(snapshot.Request.Header, "x-stainless-runtime-version"))
	require.NoError(t, uuid.Validate(getHeaderRaw(snapshot.Request.Header, "x-client-request-id")))
	require.NotEmpty(t, getHeaderRaw(snapshot.Request.Header, "x-claude-code-session-id"))
	require.Empty(t, getHeaderRaw(snapshot.Request.Header, claudeOAuthSessionInitHeader))
	require.Empty(t, getHeaderRaw(snapshot.Request.Header, claudeCode208CWDHeader))
	require.Empty(t, getHeaderRaw(snapshot.Request.Header, claudeCode208LanguageHeader))
	require.Empty(t, getHeaderRaw(snapshot.Request.Header, claudeCode208Context1MHeader))
}

func writeClaudeCode208CurlCaptureBanner(
	t *testing.T,
	file *os.File,
	now time.Time,
	account *Account,
	modelID string,
) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api Claude Code 2.1.208 curl upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# account: " + account.Name + " (synthetic)",
		"# model: " + modelID,
		"# lifecycle: explicit session init; captures quota + title + main",
		"# input: same English prompt and max_tokens as the 2026-07-14 CLI trace",
		"# identity: account/token/email/device/session values are synthetic",
		"# network: disabled; GatewayService.Forward uses only an in-process fake HTTPUpstream",
		"# CLIENT_ORIGINAL = curl input to sub2api",
		"# UPSTREAM_* = finalized objects that would be sent to api.anthropic.com",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}
