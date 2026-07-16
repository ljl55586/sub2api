//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const captureLogDir = "/Users/ling/Demo/PacketCapture/sub2api/claude"

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

// TestCaptureClaudeProUpstreamRequest constructs the exact upstream request
// object through the production OAuth mimic helpers. It never calls DoWithTLS
// or any other network transport.
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

	account := &Account{
		ID:          1001,
		Name:        "claude-pro-oauth-offline-capture",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "REDACTED-DEMO-OAUTH-TOKEN"},
		Extra: map[string]any{
			"account_uuid": "11111111-2222-4333-8444-555555555555",
		},
		Status:      StatusActive,
		Schedulable: true,
	}
	cache := &captureIdentityCache{}
	svc := &GatewayService{
		cfg:             &config.Config{},
		identityService: NewIdentityService(cache),
	}

	body := parsed.Body.Bytes()
	systemRaw, _ := parsed.SystemValue()
	systemPromptInjectionEnabled, systemPrompt, systemPromptBlocks := svc.claudeOAuthSystemPromptInjectionSettings(ctx)
	require.True(t, systemPromptInjectionEnabled)
	body = rewriteSystemForNonClaudeCodeWithPromptBlocks(body, systemRaw, systemPrompt, systemPromptBlocks)
	require.NoError(t, parsed.ReplaceBody(body))

	metadataUserID, err := svc.buildOAuthMimicMetadataUserID(ctx, c, parsed, account)
	require.NoError(t, err)
	body, modelID := normalizeClaudeOAuthRequestBody(parsed.Body.Bytes(), parsed.Model, claudeOAuthNormalizeOptions{
		injectMetadata:             true,
		metadataUserID:             metadataUserID,
		stripSystemCacheControl:    false,
		alignClaudeCodeMainRequest: true,
	})
	require.NoError(t, parsed.ReplaceBody(body))
	body = svc.rewriteMessageCacheControlIfEnabled(ctx, parsed.Body.Bytes())
	if rw := buildToolNameRewriteFromBody(body); rw != nil {
		body = applyToolNameRewriteToBody(body, rw)
	} else {
		body = applyToolsLastCacheBreakpoint(body)
	}
	body = enforceCacheControlLimit(body)
	require.NoError(t, parsed.ReplaceBody(body))

	const token = "REDACTED-DEMO-OAUTH-TOKEN"
	req, outBody, err := svc.buildUpstreamRequest(
		ctx, c, account, parsed.Body.Bytes(), token, "oauth", modelID, true, true,
	)
	require.NoError(t, err)
	assertAlignedCaptureRequest(t, req, outBody)

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	logPath := filepath.Join(captureLogDir, "log_"+now.Format("20060102_150405")+".log")
	svc.initDebugGatewayBodyFile(logPath)
	file := svc.debugGatewayBodyFile.Load()
	require.NotNil(t, file)
	t.Cleanup(func() { _ = file.Close() })

	writeCaptureBanner(t, file, now, account, modelID)
	svc.debugLogGatewaySnapshot("CLIENT_ORIGINAL", c.Request.Header, clientBody, map[string]string{
		"account":      fmt.Sprintf("%d(%s)", account.ID, account.Name),
		"account_type": string(account.Type),
		"model":        modelID,
		"stream":       strconv.FormatBool(true),
		"note":         "offline curl input; no upstream network request is sent",
	})

	loggedReq, loggedBody, err := svc.buildUpstreamRequest(
		ctx, c, account, parsed.Body.Bytes(), token, "oauth", modelID, true, true,
	)
	require.NoError(t, err)
	assertAlignedCaptureRequest(t, loggedReq, loggedBody)
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

	t.Logf("CAPTURE_LOG=%s", logPath)
}

func assertAlignedCaptureRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	require.Equal(t, claudeAPIURL, req.URL.String())
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
	require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.NotEmpty(t, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	require.Equal(t, claude.ClaudeCodeOAuthMainMimicryBetas(), parseAnthropicBetaHeader(getHeaderRaw(req.Header, "anthropic-beta")))

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
	require.Equal(t, int64(1024), gjson.GetBytes(body, "max_tokens").Int())
}

func writeCaptureBanner(t *testing.T, file *os.File, now time.Time, account *Account, modelID string) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api Claude Pro OAuth upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# account: " + account.Name,
		"# model: " + modelID,
		"# network: disabled; the test stops after constructing http.Request",
		"# CLIENT_ORIGINAL = curl input to sub2api",
		"# UPSTREAM_FORWARD = object that would be sent to api.anthropic.com",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}
