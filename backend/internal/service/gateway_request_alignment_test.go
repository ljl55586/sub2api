package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func billingSystemText(body []byte) string {
	return gjson.GetBytes(body, "system.0.text").String()
}

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

func TestNormalizeClaudeOAuthRequestBody_UsesProfileDefaultAndUpperLimit(t *testing.T) {
	t.Run("missing uses model default", func(t *testing.T) {
		input := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
		out, _ := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{
			alignClaudeCodeMainRequest: true,
		})
		require.Equal(t, int64(64000), gjson.GetBytes(out, "max_tokens").Int())
	})

	t.Run("explicit value below limit is preserved", func(t *testing.T) {
		input := []byte(`{"model":"claude-opus-4-8","max_tokens":70000,"messages":[]}`)
		out, _ := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{
			alignClaudeCodeMainRequest: true,
		})
		require.Equal(t, int64(70000), gjson.GetBytes(out, "max_tokens").Int())
	})

	t.Run("explicit value above limit is clamped", func(t *testing.T) {
		input := []byte(`{"model":"claude-opus-4-8","max_tokens":200000,"messages":[]}`)
		out, _ := normalizeClaudeOAuthRequestBody(input, "claude-opus-4-8", claudeOAuthNormalizeOptions{
			alignClaudeCodeMainRequest: true,
		})
		require.Equal(t, int64(128000), gjson.GetBytes(out, "max_tokens").Int())
	})
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

func TestClaudeOAuthNoToolsMainProfile_ForwardAppliesDefaultProfile(t *testing.T) {
	out := forwardClaudeOAuthNoToolsProfileBodyForTest(t,
		[]byte(`{"model":"claude-opus-4-8","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`),
		map[string]string{},
		"curl/8.4.0",
	)

	system := gjson.GetBytes(out, "system")
	require.True(t, system.IsArray())
	blocks := system.Array()
	require.Len(t, blocks, 3)
	require.Contains(t, blocks[0].Get("text").String(), "x-anthropic-billing-header:")
	require.Contains(t, blocks[0].Get("text").String(), "cc_version=")
	require.Contains(t, blocks[0].Get("text").String(), "cc_entrypoint=cli")
	require.Contains(t, blocks[0].Get("text").String(), "cch=")
	require.NotContains(t, blocks[0].Get("text").String(), claudeCodeCCHPlaceholder)
	require.False(t, blocks[0].Get("cache_control").Exists())
	require.Equal(t, "ephemeral", blocks[1].Get("cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, blocks[1].Get("cache_control.ttl").String())
	require.Equal(t, claudeCode208IdentityPrompt, blocks[1].Get("text").String())
	require.Contains(t, blocks[2].Get("text").String(), "CWD: "+claudeCode208DefaultVirtualCWD+"\nDate: ")
	require.Equal(t, "ephemeral", blocks[2].Get("cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, blocks[2].Get("cache_control.ttl").String())

	messages := gjson.GetBytes(out, "messages")
	require.True(t, messages.IsArray())
	lastMessage := messages.Array()[len(messages.Array())-1]
	require.Equal(t, "user", lastMessage.Get("role").String())
	content := lastMessage.Get("content")
	require.True(t, content.IsArray())
	require.Len(t, content.Array(), 2)
	require.Contains(t, content.Get("0.text").String(), "<system-reminder>")
	require.Equal(t, "text", content.Get("1.type").String())
	require.Equal(t, "Hello", content.Get("1.text").String())
	require.Equal(t, "ephemeral", content.Get("1.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, content.Get("1.cache_control.ttl").String())

	require.True(t, gjson.GetBytes(out, "tools").IsArray())
	require.Empty(t, gjson.GetBytes(out, "tools").Array())
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(out, "context_management.edits.0.type").String())
	require.Equal(t, "all", gjson.GetBytes(out, "context_management.edits.0.keep").String())
	require.Equal(t, int64(1024), gjson.GetBytes(out, "max_tokens").Int())
	require.Equal(t, 3, strings.Count(string(out), `"cache_control"`))
}

func TestResolveClaudeCode208SessionFactsUsesExplicitSessionInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set(claudeCode208CWDHeader, "repo/subdir")
	c.Request.Header.Set(claudeCode208LanguageHeader, "zh-CN")
	account := &Account{
		Credentials: map[string]any{"email": "user@example.com\n"},
		Extra:       map[string]any{"claude_cwd": "/must-not-win"},
	}
	startedAt := time.Date(2026, time.July, 14, 2, 55, 30, 0, time.UTC)

	facts := resolveClaudeCode208SessionFacts(c, account, startedAt)

	require.Equal(t, "/repo/subdir", facts.CWD)
	require.Equal(t, "2026-07-14", facts.StartDate)
	require.Equal(t, time.Now().Format("2006-01-02"), facts.CurrentDate)
	require.Equal(t, "user@example.com", facts.UserEmail)
	require.Equal(t, "zh-CN", facts.Language)
	reminder := buildClaudeCode208SystemReminder(facts)
	require.Contains(t, reminder, "# userEmail\nThe user's email address is user@example.com.")
	require.Contains(t, reminder, "# currentDate\nToday's date is "+facts.CurrentDate+".")
}

func TestBuildClaudeCode208SystemReminder_FinalWireGolden(t *testing.T) {
	facts := claudeCode208SessionFacts{
		UserEmail:   "user@example.com",
		CurrentDate: "2026-07-29",
	}

	require.Equal(t, `<system-reminder>
As you answer the user's questions, you can use the following context:
# userEmail
The user's email address is user@example.com.
# currentDate
Today's date is 2026-07-29.

      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>

`, buildClaudeCode208SystemReminder(facts))
}

func TestClaudeOAuthNoToolsMainProfile_OfficialSimpleSystemPromptGolden(t *testing.T) {
	metadata := FormatMetadataUserID(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"account",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":64000,"stream":true,"metadata":{"user_id":` + strconvQuote(metadata) + `},"messages":[{"role":"user","content":"Hello, please introduce yourself."}]}`)
	facts := claudeCode208SessionFacts{
		CWD:         "/Users/ling/sub2api",
		StartDate:   "2026-07-14",
		CurrentDate: "2026-07-29",
		UserEmail:   "user@example.com",
	}

	out, applied := applyClaudeOAuthNoToolsMainProfileWithFacts(body, "claude-opus-4-8", true, facts)

	require.True(t, applied)
	require.Equal(t, claudeCode208IdentityPrompt, gjson.GetBytes(out, "system.1.text").String())
	require.Equal(t, "CWD: /Users/ling/sub2api\nDate: 2026-07-14", gjson.GetBytes(out, "system.2.text").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(out, "system.1.cache_control.ttl").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(out, "system.2.cache_control.ttl").String())
	require.Equal(t, buildClaudeCode208SystemReminder(facts), gjson.GetBytes(out, "messages.0.content.0.text").String())
	require.Equal(t, "Hello, please introduce yourself.", gjson.GetBytes(out, "messages.0.content.1.text").String())
	require.Equal(t, cacheTTLTarget1h, gjson.GetBytes(out, "messages.0.content.1.cache_control.ttl").String())
	require.Equal(t, 3, strings.Count(string(out), `"cache_control"`))
}

func TestClaudeOAuthNoToolsMainProfile_PreservesNormalizedDefaultMaxTokens(t *testing.T) {
	metadata := FormatMetadataUserID(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"account",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"metadata":{"user_id":` + strconvQuote(metadata) + `},"messages":[{"role":"user","content":"Hello"}]}`)
	body, modelID := normalizeClaudeOAuthRequestBody(body, "claude-opus-4-8", claudeOAuthNormalizeOptions{
		alignClaudeCodeMainRequest: true,
	})
	require.Equal(t, int64(64000), gjson.GetBytes(body, "max_tokens").Int())

	out, applied := applyClaudeOAuthNoToolsMainProfile(body, modelID, true)
	require.True(t, applied)
	require.Equal(t, int64(64000), gjson.GetBytes(out, "max_tokens").Int())
	require.Contains(t, gjson.GetBytes(out, "system.0.text").String(), "cc_version="+claude.CLICurrentVersion+".")
}

func TestClaudeOAuthNoToolsMainCandidate(t *testing.T) {
	base := `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	tests := []struct {
		name  string
		body  string
		model string
		want  bool
	}{
		{
			name:  "missing tools is accepted",
			body:  base,
			model: "claude-opus-4-8",
			want:  true,
		},
		{
			name:  "empty tools is accepted",
			body:  `{"model":"claude-opus-4-8","stream":true,"tools":[],"messages":[{"role":"user","content":"Hello"}]}`,
			model: "claude-opus-4-8",
			want:  true,
		},
		{
			name:  "other supported model is accepted",
			body:  base,
			model: "claude-sonnet-4-6",
			want:  true,
		},
		{
			name:  "non streaming is rejected",
			body:  `{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"Hello"}]}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name:  "nonempty tools is rejected",
			body:  `{"model":"claude-opus-4-8","stream":true,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Hello"}]}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name:  "nonarray tools is rejected",
			body:  `{"model":"claude-opus-4-8","stream":true,"tools":{},"messages":[{"role":"user","content":"Hello"}]}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name:  "raw tool choice is rejected before normalization",
			body:  `{"model":"claude-opus-4-8","stream":true,"tools":[],"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"Hello"}]}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name: "formatted tool use block is rejected structurally",
			body: `{
				"stream": true,
				"messages": [
					{"content":"First","role":"user"},
					{"content":[ { "id":"toolu_1", "input":{}, "type" : "tool_use", "name":"read" } ],"role":"assistant"}
				],
				"tools": []
			}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name: "formatted tool result block is rejected structurally",
			body: `{
				"stream": true,
				"messages": [
					{"role":"user","content":[{"content":"result","tool_use_id":"toolu_1","type" : "tool_result"}]}
				]
			}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name: "server tool use history is rejected structurally",
			body: `{
				"stream": true,
				"messages": [
					{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"weather"}}]}
				]
			}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name: "web search tool result history is rejected structurally",
			body: `{
				"stream": true,
				"messages": [
					{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}]}
				]
			}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name: "tool use result history is rejected structurally",
			body: `{
				"stream": true,
				"messages": [
					{"role":"user","content":[{"type":"tool_use_result","tool_use_id":"toolu_1","content":"result"}]}
				]
			}`,
			model: "claude-opus-4-8",
			want:  false,
		},
		{
			name:  "tool type text literal is not a structured tool block",
			body:  `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"literal {\\\"type\\\":\\\"tool_use\\\"}"}]}`,
			model: "claude-opus-4-8",
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Truef(t, gjson.Valid(tt.body), "candidate fixture must be valid JSON: %s", tt.body)
			require.Equal(t, tt.want, isClaudeOAuthNoToolsMainCandidate([]byte(tt.body), tt.model))
		})
	}
}

func TestClaudeOAuthNoToolsMainProfile_ForwardKeepsGenericPathForInverseCandidates(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "non streaming",
			body: `{"model":"claude-opus-4-8","stream":false,"messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			name: "nonempty tools",
			body: `{"model":"claude-opus-4-8","stream":true,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			name: "raw tool choice survives profile guard even though normalizer deletes it",
			body: `{"model":"claude-opus-4-8","stream":true,"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			name: "tool use history",
			body: `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"First"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read","input":{}}]},{"role":"user","content":"Final"}]}`,
		},
		{
			name: "tool result history",
			body: `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"result"}]},{"role":"assistant","content":"Acknowledged"},{"role":"user","content":"Final"}]}`,
		},
		{
			name: "server web search history",
			body: `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"Search"},{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"weather"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}]},{"role":"user","content":"Final"}]}`,
		},
		{
			name: "tool use result history variant",
			body: `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":[{"type":"tool_use_result","tool_use_id":"toolu_1","content":"result"}]},{"role":"assistant","content":"Acknowledged"},{"role":"user","content":"Final"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := forwardClaudeOAuthNoToolsProfileBodyForTest(t, []byte(tt.body), map[string]string{}, "curl/8.4.0")
			require.Equal(t, claudeCodeSystemPromptExpansion, gjson.GetBytes(out, "system.2.text").String())
			require.False(t, gjson.GetBytes(out, "system.1.cache_control").Exists())
			require.Equal(t, claude.DefaultCacheControlTTL, gjson.GetBytes(out, "system.2.cache_control.ttl").String())
			require.NotContains(t, string(out), "CWD: "+claudeCode208DefaultVirtualCWD)
		})
	}
}

func TestClaudeOAuthNoToolsMainProfile_ForwardOverridesMessageCacheRewriteTTL(t *testing.T) {
	out := forwardClaudeOAuthNoToolsProfileBodyForTest(t,
		[]byte(`{"model":"claude-opus-4-8","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`),
		map[string]string{
			SettingKeyRewriteMessageCacheControl:             "true",
			SettingKeyEnableAnthropicCacheTTL1hInjection:     "false",
			SettingKeyEnableClaudeOAuthSystemPromptInjection: "true",
		},
		"curl/8.4.0",
	)

	content := gjson.GetBytes(out, "messages.0.content")
	require.True(t, content.IsArray())
	require.Len(t, content.Array(), 2)
	require.Contains(t, content.Get("0.text").String(), "<system-reminder>")
	require.Equal(t, "text", content.Get("1.type").String())
	require.Equal(t, "Hello", content.Get("1.text").String())
	require.Equal(t, "ephemeral", content.Get("1.cache_control.type").String())
	require.Equal(t, cacheTTLTarget1h, content.Get("1.cache_control.ttl").String())
}

func TestClaudeOAuthNoToolsMainProfile_ForwardDoesNotOverrideCustomSystemSettings(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"Hello"}]}`)

	t.Run("custom expansion prompt", func(t *testing.T) {
		out := forwardClaudeOAuthNoToolsProfileBodyForTest(t, body, map[string]string{
			SettingKeyClaudeOAuthSystemPrompt: "administrator custom expansion",
		}, "curl/8.4.0")
		require.Equal(t, "administrator custom expansion", gjson.GetBytes(out, "system.2.text").String())
		require.False(t, gjson.GetBytes(out, "system.1.cache_control").Exists())
		require.NotContains(t, string(out), "CWD: "+claudeCode208DefaultVirtualCWD)
	})

	t.Run("custom blocks", func(t *testing.T) {
		out := forwardClaudeOAuthNoToolsProfileBodyForTest(t, body, map[string]string{
			SettingKeyClaudeOAuthSystemPromptBlocks: `[
				{"type":"text","text":"administrator first block"},
				{"type":"text","text":"administrator second block","cache_control":{"type":"ephemeral","ttl":"1h"}}
			]`,
		}, "curl/8.4.0")
		system := gjson.GetBytes(out, "system")
		require.True(t, system.IsArray())
		require.Len(t, system.Array(), 2)
		require.Equal(t, "administrator first block", system.Get("0.text").String())
		require.Equal(t, "administrator second block", system.Get("1.text").String())
		require.NotContains(t, string(out), "CWD: "+claudeCode208DefaultVirtualCWD)
	})
}

func TestClaudeOAuthNoToolsMainProfile_ForwardSkipsRealClaudeCode(t *testing.T) {
	metadataUserID := FormatMetadataUserID(
		"real-claude-code-device",
		"account-no-tools-profile",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"stream":true,"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},"messages":[{"role":"user","content":"Hello"}]}`)

	out := forwardClaudeOAuthNoToolsProfileBodyForTest(t, body, map[string]string{}, "claude-cli/2.1.208 (external, cli)")
	require.False(t, gjson.GetBytes(out, "system").Exists())
	require.NotContains(t, string(out), "CWD: "+claudeCode208DefaultVirtualCWD)
}

func forwardClaudeOAuthNoToolsProfileBodyForTest(t *testing.T, body []byte, settings map[string]string, userAgent string) []byte {
	t.Helper()
	resetGatewayForwardingSettingsCacheForTest(t)
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", userAgent)

	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	upstream := &anthropicHTTPUpstreamRecorder{
		resp: claudeOAuthNoToolsProfileResponseForTest(parsed.Stream),
	}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       NewSettingService(&gatewayTTLSettingRepo{data: claudeOAuthNoToolsProfileSettingsForTest(settings)}, cfg),
		identityService:      NewIdentityService(&identityCacheStub{}),
	}
	account := &Account{
		ID:          4801,
		Name:        "claude-oauth-no-tools-profile",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"account_uuid": "account-no-tools-profile"},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.NotEmpty(t, upstream.lastBody)
	return upstream.lastBody
}

func claudeOAuthNoToolsProfileSettingsForTest(overrides map[string]string) map[string]string {
	settings := map[string]string{
		SettingKeyEnableClaudeOAuthSystemPromptInjection: "true",
		SettingKeyRewriteMessageCacheControl:             "false",
		SettingKeyEnableAnthropicCacheTTL1hInjection:     "false",
	}
	for key, value := range overrides {
		settings[key] = value
	}
	return settings
}

func claudeOAuthNoToolsProfileResponseForTest(stream bool) *http.Response {
	if stream {
		payload := strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
			"",
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":9,"output_tokens":3}}`,
		)),
	}
}

func TestBuildUpstreamRequest_MimicSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")
	c.Request.Header.Set(claudeCode208CWDHeader, "/private/repo")
	c.Request.Header.Set(claudeCode208LanguageHeader, "zh-CN")
	c.Request.Header.Set(claudeCode208Context1MHeader, "true")

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
		Extra: map[string]any{
			"account_uuid":   "account-123",
			"claude_user_id": "account-claude-device",
		},
	}
	metadataUserID := FormatMetadataUserID(
		"account-claude-device",
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
	_, requestIDErr := uuid.Parse(getHeaderRaw(req.Header, "x-client-request-id"))
	require.NoError(t, requestIDErr)
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.Empty(t, getHeaderRaw(req.Header, claudeCode208CWDHeader))
	require.Empty(t, getHeaderRaw(req.Header, claudeCode208LanguageHeader))
	require.Empty(t, getHeaderRaw(req.Header, claudeCode208Context1MHeader))
	parsedUserID := ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String())
	require.NotNil(t, parsedUserID)
	require.True(t, gjson.Valid(gjson.GetBytes(outBody, "metadata.user_id").String()), "the final 2.1.208 request must retain JSON metadata format")
	require.Equal(t, "account-claude-device", parsedUserID.DeviceID)
	require.Equal(t, parsedUserID.SessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"))
	require.Equal(t, claude.ClaudeCodeOAuthBetasForRequest(
		claude.ClaudeCodeRequestMain,
		"claude-opus-4-8",
		claude.ClaudeCodeRequestFeatures{
			Context1M:           true,
			Thinking:            true,
			ContextManagement:   true,
			Effort:              true,
			MidConversationRole: true,
			Advisor:             true,
		},
	), parseAnthropicBetaHeader(getHeaderRaw(req.Header, "anthropic-beta")))
	require.Equal(t, "clear_thinking_20251015", gjson.GetBytes(outBody, "context_management.edits.0.type").String())
}

func TestBuildUpstreamRequest_MimicUsesMacOSAndFinalBillingVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")

	const originalClientID = "stable-device-id"
	cachedFingerprint := &Fingerprint{
		ClientID:    originalClientID,
		UserAgent:   "claude-cli/2.1.160 (external, cli)",
		StainlessOS: "Linux",
		UpdatedAt:   time.Now().Unix(),
	}
	cache := &identityCacheStub{fingerprint: cachedFingerprint}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       456,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"account_uuid": "account-456"},
	}
	metadataUserID := FormatMetadataUserID(
		originalClientID,
		"account-456",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{
		"model":"claude-opus-4-8",
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.161.abc; cc_entrypoint=cli;"},
			{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"type":"text","text":"default expansion"}
		],
		"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},
		"messages":[{"role":"user","content":"Hello"}]
	}`)

	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"synthetic-token", "oauth", "claude-opus-4-8", true, true,
	)

	require.NoError(t, err)
	require.Contains(t, billingSystemText(outBody), "cc_version=2.1.208.")
	require.Contains(t, billingSystemText(outBody), "cc_entrypoint=cli;")
	require.Contains(t, billingSystemText(outBody), "cch=")
	require.NotContains(t, billingSystemText(outBody), claudeCodeCCHPlaceholder)
	require.Equal(t, "MacOS", getHeaderRaw(req.Header, "x-stainless-os"))
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "user-agent"))
	require.Equal(t, originalClientID, cachedFingerprint.ClientID)
	require.Equal(t, "Linux", cache.fingerprint.StainlessOS)
	require.Zero(t, cache.setFingerprintCall)
	parsedUserID := ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String())
	require.NotNil(t, parsedUserID)
	require.Equal(t, originalClientID, parsedUserID.DeviceID)
	require.Equal(t, "account-456", parsedUserID.AccountUUID)
	_, requestIDErr := uuid.Parse(getHeaderRaw(req.Header, "x-client-request-id"))
	require.NoError(t, requestIDErr)
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
}

func TestBuildUpstreamRequest_MimicMaskedSessionHeaderUsesFinalMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")

	const (
		originalClientID = "stable-device-id"
		maskedSessionID  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	)
	cache := &identityCacheStub{
		maskedSessionID: maskedSessionID,
		fingerprint: &Fingerprint{
			ClientID:  originalClientID,
			UserAgent: "claude-cli/2.1.160 (external, cli)",
			UpdatedAt: time.Now().Unix(),
		},
	}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       789,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"account_uuid":               "account-789",
			"session_id_masking_enabled": true,
		},
	}
	metadataUserID := FormatMetadataUserID(
		originalClientID,
		"account-789",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{"model":"claude-opus-4-8","metadata":{"user_id":` + strconvQuote(metadataUserID) + `},"messages":[{"role":"user","content":"Hello"}]}`)

	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"synthetic-token", "oauth", "claude-opus-4-8", true, true,
	)

	require.NoError(t, err)
	parsedUserID := ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String())
	require.NotNil(t, parsedUserID)
	require.Equal(t, originalClientID, parsedUserID.DeviceID)
	require.Equal(t, "account-789", parsedUserID.AccountUUID)
	require.Equal(t, maskedSessionID, parsedUserID.SessionID)
	require.Equal(t, maskedSessionID, getHeaderRaw(req.Header, "x-claude-code-session-id"))
	_, requestIDErr := uuid.Parse(getHeaderRaw(req.Header, "x-client-request-id"))
	require.NoError(t, requestIDErr)
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
}

func TestBuildUpstreamRequest_MimicCustomRelayOmitsDirectOnlyArtifacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	cache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID:  "relay-device",
		UserAgent: claude.DefaultHeaders["User-Agent"],
		UpdatedAt: time.Now().Unix(),
	}}
	svc := &GatewayService{
		cfg:             &config.Config{},
		identityService: NewIdentityService(cache),
	}
	account := &Account{
		ID:       790,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"account_uuid":            "account-relay",
			"custom_base_url_enabled": true,
			"custom_base_url":         "https://relay.example.com",
		},
	}
	metadataUserID := FormatMetadataUserID(
		"relay-device",
		"account-relay",
		"11111111-2222-4333-8444-555555555555",
		claude.CLICurrentVersion,
	)
	body := []byte(`{
		"model":"claude-opus-4-8",
		"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.208.abc; cc_entrypoint=cli; cch=12345;"}],
		"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},
		"messages":[{"role":"user","content":"Hello"}]
	}`)

	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"synthetic-token", "oauth", "claude-opus-4-8", true, true,
	)

	require.NoError(t, err)
	require.Equal(t, "relay.example.com", req.URL.Hostname())
	require.Empty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	require.NotContains(t, billingSystemText(outBody), "cch=")
	require.Equal(t,
		ParseMetadataUserID(gjson.GetBytes(outBody, "metadata.user_id").String()).SessionID,
		getHeaderRaw(req.Header, "x-claude-code-session-id"),
	)
}

func TestApplyClaudeCodeMimicHeadersUsesRequestRetryCount(t *testing.T) {
	ctx := withClaudeCodeRetryCount(context.Background(), 3)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIURL, nil)
	require.NoError(t, err)

	applyClaudeCodeMimicHeaders(req)

	require.Equal(t, "3", getHeaderRaw(req.Header, "x-stainless-retry-count"))
}

func TestClaudeCodeBodyUsesCacheTTLInspectsCacheControlFieldsOnly(t *testing.T) {
	require.True(t, claudeCodeBodyUsesCacheTTL(
		[]byte(`{"system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`),
		"1h",
	))
	require.False(t, claudeCodeBodyUsesCacheTTL(
		[]byte(`{"messages":[{"role":"user","content":"literal text: \"ttl\":\"1h\""}]}`),
		"1h",
	))
}

func TestBuildUpstreamRequest_MimicRejectsMissingMetadataSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "curl/8.4.0")

	cache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID:  "stable-device-id",
		UserAgent: "claude-cli/2.1.160 (external, cli)",
		UpdatedAt: time.Now().Unix(),
	}}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       999,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"account_uuid": "account-999"},
	}
	body := []byte(`{"model":"claude-opus-4-8","metadata":{"user_id":"invalid"},"messages":[{"role":"user","content":"Hello"}]}`)

	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"synthetic-token", "oauth", "claude-opus-4-8", true, true,
	)

	require.ErrorContains(t, err, "OAuth mimic request is missing a valid metadata session")
	require.Nil(t, req)
	require.Nil(t, outBody)
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
