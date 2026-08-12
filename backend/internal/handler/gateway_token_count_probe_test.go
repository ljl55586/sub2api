package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func tokenCountProbeTestBody(t *testing.T, content string, tools []any, extra map[string]any) []byte {
	t.Helper()
	metadataUserID, err := json.Marshal(map[string]string{
		"device_id":    "01a070fe42e5f8661185c6d8bacbe265f335e0cc87e840246465f335b8ea4d6a",
		"account_uuid": "",
		"session_id":   "043bb1e1-41b2-40bb-bf9a-41d5ce2cc47a",
	})
	require.NoError(t, err)
	body := map[string]any{
		"model":      "glm-5-turbo",
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": content}},
		"metadata":   map[string]any{"user_id": string(metadataUserID)},
	}
	if tools != nil {
		body["tools"] = tools
	}
	for key, value := range extra {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func tokenCountProbeTestRequest(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("User-Agent", "claude-cli/2.1.222 (external, claude-desktop-3p, agent-sdk/0.3.222)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("anthropic-version", "2023-06-01")
	return req
}

func TestDetectClaudeDesktopTokenCountProbeKnownComponents(t *testing.T) {
	cases := []struct {
		name    string
		content string
		reason  string
		tools   []any
	}{
		{name: "baseline", content: "count", reason: "count"},
		{name: "tools", content: "count", reason: "count", tools: []any{map[string]any{"name": "Skill", "input_schema": map[string]any{"type": "object"}}}},
		{name: "format", content: "\n\nWhen referencing files in your responses, format them as markdown links.", reason: "response_format"},
		{name: "environment", content: "# Environment\n - Platform: darwin", reason: "environment"},
		{name: "git", content: "This is the git status at the start of the conversation.\n\nCurrent branch: main", reason: "git_status"},
		{name: "memory", content: "# Memory\nPersistent memory rules", reason: "memory"},
		{name: "pronouns", content: "When you use a pronoun for someone, use they/them.", reason: "pronoun_guidance"},
		{name: "session", content: "# Session-specific guidance\n - Invoke skills explicitly.", reason: "session_guidance"},
		{name: "model", content: "This iteration of Claude is Claude Fable 5.", reason: "model_identity"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tokenCountProbeTestBody(t, tc.content, tc.tools, nil)
			decision := detectClaudeDesktopTokenCountProbe(tokenCountProbeTestRequest(body), body)
			require.True(t, decision.Matched)
			require.Equal(t, tc.reason, decision.Reason)
			require.Equal(t, "043bb1e1-41b2-40bb-bf9a-41d5ce2cc47a", decision.SessionID)
		})
	}
}

func TestDetectClaudeDesktopTokenCountProbeRejectsAmbiguousRequests(t *testing.T) {
	tests := []struct {
		name   string
		body   func(t *testing.T) []byte
		mutate func(*http.Request)
	}{
		{name: "ordinary one token request", body: func(t *testing.T) []byte { return tokenCountProbeTestBody(t, "answer yes or no", nil, nil) }},
		{name: "streaming", body: func(t *testing.T) []byte {
			return tokenCountProbeTestBody(t, "count", nil, map[string]any{"stream": true})
		}},
		{name: "system present", body: func(t *testing.T) []byte {
			return tokenCountProbeTestBody(t, "count", nil, map[string]any{"system": nil})
		}},
		{name: "unknown top level field", body: func(t *testing.T) []byte {
			return tokenCountProbeTestBody(t, "count", nil, map[string]any{"temperature": 0})
		}},
		{name: "not desktop 3p", body: func(t *testing.T) []byte { return tokenCountProbeTestBody(t, "count", nil, nil) }, mutate: func(r *http.Request) {
			r.Header.Set("User-Agent", "claude-cli/2.1.222")
		}},
		{name: "missing anthropic version", body: func(t *testing.T) []byte { return tokenCountProbeTestBody(t, "count", nil, nil) }, mutate: func(r *http.Request) {
			r.Header.Del("anthropic-version")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			req := tokenCountProbeTestRequest(body)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			require.False(t, detectClaudeDesktopTokenCountProbe(req, body).Matched)
		})
	}
}

func TestTokenCountProbeSessionCorrelation(t *testing.T) {
	body := tokenCountProbeTestBody(t, "a future static context block", nil, nil)
	decision := detectClaudeDesktopTokenCountProbe(tokenCountProbeTestRequest(body), body)
	require.False(t, decision.Matched)
	require.True(t, decision.Candidate)

	cache := newTokenCountProbeEstimateCache()
	now := time.Now()
	key := "7:" + decision.SessionID
	require.False(t, cache.isProbeSessionActive(key, now))
	cache.markProbeSession(key, now)
	require.True(t, cache.isProbeSessionActive(key, now.Add(time.Second)))
	require.False(t, cache.isProbeSessionActive(key, now.Add(tokenCountProbeSessionCorrelationTTL+time.Millisecond)))
}

func TestGatewayHandlerTokenCountProbeModeForModel(t *testing.T) {
	h := &GatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{TokenCountProbe: config.GatewayTokenCountProbeConfig{
		Mode:          config.TokenCountProbeModeEnforce,
		ModelPrefixes: []string{"glm-"},
	}}}}
	require.Equal(t, config.TokenCountProbeModeEnforce, h.tokenCountProbeModeForModel("GLM-5-Turbo"))
	require.Equal(t, config.TokenCountProbeModeOff, h.tokenCountProbeModeForModel("claude-sonnet-4-5"))
	h.cfg.Gateway.TokenCountProbe.Mode = "invalid"
	require.Equal(t, config.TokenCountProbeModeOff, h.tokenCountProbeModeForModel("glm-5-turbo"))
}

func TestEstimateTokenCountProbeCachesNormalizedPayload(t *testing.T) {
	h := &GatewayHandler{
		cfg: &config.Config{Gateway: config.GatewayConfig{TokenCountProbe: config.GatewayTokenCountProbeConfig{
			SafetyMarginPercent: 10,
			CacheTTLSeconds:     60,
			CacheMaxEntries:     8,
		}}},
		tokenCountProbeCache: newTokenCountProbeEstimateCache(),
	}
	body := tokenCountProbeTestBody(t, "count", nil, nil)
	first, source, err := h.estimateTokenCountProbe(body)
	require.NoError(t, err)
	require.Positive(t, first)
	require.Equal(t, tokenCountProbeEstimatorVersion, source)

	var changed map[string]any
	require.NoError(t, json.Unmarshal(body, &changed))
	changed["metadata"] = map[string]any{"user_id": `{"device_id":"other","account_uuid":"","session_id":"another"}`}
	changedBody, err := json.Marshal(changed)
	require.NoError(t, err)
	second, source, err := h.estimateTokenCountProbe(changedBody)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, "cache", source)
}

func TestTokenCountProbeToolFramingOverhead(t *testing.T) {
	oneTool := tokenCountProbeTestBody(t, "count", []any{map[string]any{"name": "Skill"}}, nil)
	require.Equal(t, tokenCountProbeToolBaseOverhead+tokenCountProbeToolPerDefinitionCost, tokenCountProbeToolFramingOverhead(oneTool))

	tools := make([]any, 11)
	for i := range tools {
		tools[i] = map[string]any{"name": "tool"}
	}
	elevenTools := tokenCountProbeTestBody(t, "count", tools, nil)
	require.Equal(t, tokenCountProbeToolBaseOverhead+11*tokenCountProbeToolPerDefinitionCost, tokenCountProbeToolFramingOverhead(elevenTools))
}

func TestSendTokenCountProbeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	sendTokenCountProbeResponse(ctx, "glm-5-turbo", 10063)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "max_tokens", response["stop_reason"])
	usage := response["usage"].(map[string]any)
	require.Equal(t, float64(10063), usage["input_tokens"])
	require.Equal(t, float64(1), usage["output_tokens"])
}

func TestIsCountTokensRequestIncludesMessagesProbeContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request = ctx.Request.WithContext(service.WithIsTokenCountProbeRequest(ctx.Request.Context(), true))
	require.True(t, isCountTokensRequest(ctx))
}
