package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeOAuthRequestBody_AlignsOpus48MimicMainRequest(t *testing.T) {
	input := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"temperature":1,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)

	out, model := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{
		alignClaudeCodeMainRequest: true,
	})

	require.Equal(t, "claude-opus-4-8", model)
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(out, "context_management.edits.0.type").String())
	require.Equal(t, "all", gjson.GetBytes(out, "context_management.edits.0.keep").String())
	require.True(t, gjson.GetBytes(out, "tools").IsArray())
	require.Empty(t, gjson.GetBytes(out, "tools").Array())
	require.Equal(t, int64(1024), gjson.GetBytes(out, "max_tokens").Int())
}

func TestNormalizeClaudeOAuthRequestBody_DoesNotAlignOtherModelsOrCountTokens(t *testing.T) {
	input := []byte(`{"model":"claude-sonnet-4-6","temperature":0.2,"messages":[]}`)

	out, _ := normalizeClaudeOAuthRequestBody(input, "claude-sonnet-4-6", claudeOAuthNormalizeOptions{})

	require.Equal(t, 0.2, gjson.GetBytes(out, "temperature").Float())
	require.False(t, gjson.GetBytes(out, "thinking").Exists())
	require.False(t, gjson.GetBytes(out, "output_config").Exists())
	require.False(t, gjson.GetBytes(out, "context_management").Exists())
}

func TestNormalizeClaudeOAuthRequestBody_PreservesLegacyDefaultsOutsideTargetAlignment(t *testing.T) {
	input := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)

	out, _ := normalizeClaudeOAuthRequestBody(input, "claude-sonnet-4-6", claudeOAuthNormalizeOptions{})

	require.Equal(t, float64(1), gjson.GetBytes(out, "temperature").Float())
	require.Equal(t, int64(128000), gjson.GetBytes(out, "max_tokens").Int())
}

func TestNormalizeClaudeOAuthRequestBody_PreservesOutputFormatAndToolsWhileAligning(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-8",
		"temperature":0.7,
		"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}},
		"thinking":{"type":"disabled"},
		"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
		"tools":[{"name":"lookup","description":"look up","input_schema":{"type":"object"}}],
		"messages":[]
	}`)

	out, _ := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{
		alignClaudeCodeMainRequest: true,
	})

	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
	require.Equal(t, "json_schema", gjson.GetBytes(out, "output_config.format.type").String())
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(out, "context_management.edits.0.type").String())
	require.Len(t, gjson.GetBytes(out, "tools").Array(), 1)
	require.Equal(t, "lookup", gjson.GetBytes(out, "tools.0.name").String())
}

func TestBuildUpstreamRequest_MimicSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")

	cache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID:                "account-device-123",
		UserAgent:               "curl/8.4.0",
		StainlessLang:           "js",
		StainlessPackageVersion: "0.94.0",
		StainlessOS:             "Linux",
		StainlessArch:           "arm64",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: "v24.3.0",
		UpdatedAt:               time.Now().Unix(),
	}}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"account_uuid": "account-123"},
	}
	metadataUserID := FormatMetadataUserID(
		"account-device-123",
		"account-123",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":1024,
		"stream":true,
		"messages":[{"role":"user","content":"Hello"}],
		"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},
		"tools":[],
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}
	}`)

	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"synthetic-token", "oauth", "claude-opus-4-8", true, true,
	)

	require.NoError(t, err)
	require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	parsedUserID := ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String())
	require.NotNil(t, parsedUserID)
	require.True(t, gjson.Valid(gjson.GetBytes(outBody, "metadata.user_id").String()), "the final 2.1.161 request must retain JSON metadata format")
	require.Equal(t, parsedUserID.SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
	require.ElementsMatch(t, claude.FullClaudeCodeMimicryBetas(), parseAnthropicBetaHeader(getHeaderRaw(req.Header, "anthropic-beta")))
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(outBody, "context_management.edits.0.type").String())
}

func TestBuildOAuthMimicMetadataUserID_UsesOutboundCLIVersionForCurl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")

	cache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID:  "account-device-123",
		UserAgent: "curl/8.4.0",
		UpdatedAt: time.Now().Unix(),
	}}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"account_uuid": "account-123"},
	}
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{
		"model":"claude-opus-4-8",
		"messages":[{"role":"user","content":"Hello"}]
	}`)), PlatformAnthropic)
	require.NoError(t, err)
	parsed.SessionContext = &SessionContext{ClientIP: "127.0.0.1", UserAgent: "curl/8.4.0", APIKeyID: 77}

	metadataUserID, err := svc.buildOAuthMimicMetadataUserID(context.Background(), c, parsed, account)

	require.NoError(t, err)
	require.True(t, gjson.Valid(metadataUserID), "CLI 2.1.161 must use the JSON metadata format")
	parsedUserID := ParseMetadataUserID(metadataUserID)
	require.NotNil(t, parsedUserID)
	require.Equal(t, "account-device-123", parsedUserID.DeviceID)
	require.Equal(t, "account-123", parsedUserID.AccountUUID)
	require.NotEmpty(t, parsedUserID.SessionID)
}
