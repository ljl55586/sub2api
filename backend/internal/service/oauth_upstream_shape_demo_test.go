package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
)

func TestDemoAnthropicOAuthUpstreamShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
  "model": "claude-opus-4-8",
  "max_tokens": 32,
  "messages": [
    { "role": "user", "content": "packet-capture-test-abc-789" }
  ]
}`)
	req, err := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-LOCAL_SUB2API_KEY")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	c := &gin.Context{Request: req}
	c.Set(betaPolicyFilterSetKey, map[string]struct{}{})

	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	if err != nil {
		t.Fatal(err)
	}

	svc := &GatewayService{}
	modelID := parsed.Model
	rewritten := rewriteSystemForNonClaudeCodeWithPromptBlocks(parsed.Body.Bytes(), nil, "", "")
	metadataUserID := FormatMetadataUserID(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"11111111-2222-3333-4444-555555555555",
		"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		claude.CLICurrentVersion,
	)
	rewritten, modelID = normalizeClaudeOAuthRequestBody(rewritten, modelID, claudeOAuthNormalizeOptions{
		stripSystemCacheControl: false,
		injectMetadata:          true,
		metadataUserID:          metadataUserID,
	})
	rewritten = svc.rewriteMessageCacheControlIfEnabled(context.Background(), rewritten)
	rewritten = applyToolsLastCacheBreakpoint(rewritten)
	rewritten = enforceCacheControlLimit(rewritten)

	account := &Account{
		ID:       1001,
		Name:     "demo-anthropic-oauth",
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
	}
	upstreamReq, wireBody, err := svc.buildUpstreamRequest(
		context.Background(),
		c,
		account,
		rewritten,
		"UPSTREAM_OAUTH_ACCESS_TOKEN",
		"oauth",
		modelID,
		false,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]any{
		"method":  upstreamReq.Method,
		"url":     upstreamReq.URL.String(),
		"headers": upstreamReq.Header,
		"body":    mustDecodeJSONForDemo(t, wireBody),
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(encoded))
}

func mustDecodeJSONForDemo(t *testing.T, body []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
