package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	curlCodexLoadtestReviewRequestEnv = "SUB2API_CODEX_LOADTEST_REVIEW_REQUEST"
	curlCodexLoadtestReviewOutputEnv  = "SUB2API_CODEX_LOADTEST_REVIEW_OUTPUT"
	curlCodexLoadtestReviewSessionEnv = "SUB2API_CODEX_LOADTEST_REVIEW_SESSION_ID"
)

// TestBuildCurlCodexLoadtestUpstreamPreview is an opt-in bridge used by the
// local load-test review mode. It runs the production OAuth passthrough and
// curl Codex profile builders in-process, records the final request at the
// HTTP transport boundary, and never opens a network connection.
//
// The preview intentionally uses a synthetic OAuth account. The request body,
// URL, client identity, profile headers, and serialization are constructed by
// production code; account-specific IDs and per-turn timestamps are preview
// values and can differ from a separately running sub2api deployment.
func TestBuildCurlCodexLoadtestUpstreamPreview(t *testing.T) {
	requestPath := strings.TrimSpace(os.Getenv(curlCodexLoadtestReviewRequestEnv))
	outputPath := strings.TrimSpace(os.Getenv(curlCodexLoadtestReviewOutputEnv))
	sessionID := strings.TrimSpace(os.Getenv(curlCodexLoadtestReviewSessionEnv))
	if requestPath == "" && outputPath == "" && sessionID == "" {
		t.Skip("load-test upstream preview environment is not set")
	}
	require.NotEmpty(t, requestPath)
	require.NotEmpty(t, outputPath)
	require.NotEmpty(t, sessionID)
	_, err := uuid.Parse(sessionID)
	require.NoError(t, err, "review session ID must be a UUID")

	body, err := os.ReadFile(requestPath)
	require.NoError(t, err)
	require.True(t, json.Valid(body), "review request must contain valid JSON")
	var downstreamBody map[string]any
	require.NoError(t, json.Unmarshal(body, &downstreamBody))
	require.Equal(t, sessionID, downstreamBody["prompt_cache_key"])

	requestContext := context.WithValue(context.Background(), ctxkey.RequestID, "curl-loadtest-review")
	requestContext = context.WithValue(requestContext, ctxkey.ClientRequestID, "curl-loadtest-review")
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/responses", bytes.NewReader(body)).WithContext(requestContext)
	c.Request.Host = "localhost:8080"
	c.Request.Header.Set("Accept", "*/*")
	c.Request.Header.Set("Authorization", "Bearer synthetic-review-downstream-key")
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "curl/8.7.1")
	c.Request.Header.Set(curlCodexProfileHeader, curlCodexProfileText)
	c.Request.Header.Set(curlCodexSessionHeader, sessionID)
	c.Set("api_key", &APIKey{ID: 4242})

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
	}}
	service := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			CurlCodexProfile: config.GatewayCurlCodexProfileConfig{
				Enabled:                   true,
				RequireOptInHeader:        true,
				DefaultMode:               curlCodexProfileText,
				DefaultModel:              curlCodexProfileDefaultModel,
				DefaultReasoningEffort:    "high",
				DefaultReasoningContext:   "all_turns",
				RequireToolLoopCapability: true,
			},
		}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          9001,
		Name:        "curl-loadtest-review@example.invalid",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "synthetic-review-oauth-token",
			"chatgpt_account_id": "00000000-0000-4000-8000-000000000042",
		},
		Extra: map[string]any{
			"openai_passthrough": true,
			"openai_device_id":   "48ca0404-a22d-4f52-8407-b9f6fc68bd04",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, forwardErr := service.Forward(requestContext, c, account, body)
	require.Error(t, forwardErr, "synthetic upstream response must stop after construction")
	require.Nil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.NotEmpty(t, upstream.lastBody)
	require.Equal(t, sessionID, recorder.Header().Get(curlCodexResponseSessionHeader))

	var finalBody map[string]any
	require.NoError(t, json.Unmarshal(upstream.lastBody, &finalBody))
	require.NotContains(t, finalBody, "tools")
	require.NotContains(t, finalBody, "tool_choice")
	require.NotContains(t, finalBody, "parallel_tool_calls")
	require.Equal(t, sessionID, finalBody["prompt_cache_key"])

	sum := sha256.Sum256(upstream.lastBody)
	preview := openAIRequestCaptureRecord{
		CapturedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Direction:       openAIRequestCaptureUpstream,
		RequestID:       "curl-loadtest-review",
		ClientRequestID: "curl-loadtest-review",
		AccountID:       account.ID,
		AccountName:     account.Name,
		Method:          upstream.lastReq.Method,
		URL:             requestURLForCapture(upstream.lastReq, openAIRequestCaptureUpstream),
		Host:            upstream.lastReq.Host,
		Headers:         redactedRequestHeaders(upstream.lastReq.Header),
		BodySHA256:      hex.EncodeToString(sum[:]),
		BodyBytes:       len(upstream.lastBody),
		Body:            append(json.RawMessage(nil), upstream.lastBody...),
	}
	encoded, err := json.MarshalIndent(preview, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	require.NoError(t, os.MkdirAll(filepath.Dir(outputPath), 0o700))
	require.NoError(t, os.WriteFile(outputPath, encoded, 0o600))
}
