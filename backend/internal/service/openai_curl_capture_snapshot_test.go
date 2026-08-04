package service

import (
	"bytes"
	"context"
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
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCaptureCurlOpenAIForwardSnapshot runs the production OAuth passthrough
// request builder in-process and captures the request immediately before the
// HTTP transport. The upstream transport is a recorder that returns a local
// synthetic error response, so this test never performs a network request.
//
// The test is opt-in because it writes forensic snapshots outside the Go test
// temporary directory. Set SUB2API_CURL_CAPTURE_DIR to an empty output directory
// when a snapshot is needed.
func TestCaptureCurlOpenAIForwardSnapshot(t *testing.T) {
	outputDir := strings.TrimSpace(os.Getenv("SUB2API_CURL_CAPTURE_DIR"))
	if outputDir == "" {
		t.Skip("set SUB2API_CURL_CAPTURE_DIR to generate curl forwarding snapshots")
	}

	gin.SetMode(gin.TestMode)
	capture, err := newOpenAIRequestCapture(outputDir)
	require.NoError(t, err)
	t.Cleanup(capture.close)

	snapshotTime := time.Date(2026, time.August, 3, 14, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	capture.now = func() time.Time { return snapshotTime }
	downstreamPath := filepath.Join(outputDir, "openai-downstream-20260803-14.jsonl")
	upstreamPath := filepath.Join(outputDir, "openai-upstream-20260803-14.jsonl")
	require.NoFileExists(t, downstreamPath, "use an empty snapshot output directory")
	require.NoFileExists(t, upstreamPath, "use an empty snapshot output directory")

	body := []byte(`{"model":"gpt-5.6-sol","instructions":"You are a concise assistant.","input":"Hello from curl.","stream":false,"reasoning":{"effort":"high","context":"all_turns"},"prompt_cache_key":"019fca9a-9610-7aa0-bba1-75113215d92d","text":{"verbosity":"low"}}`)
	requestContext := context.WithValue(context.Background(), ctxkey.RequestID, "curl-snapshot-request")
	requestContext = context.WithValue(requestContext, ctxkey.ClientRequestID, "curl-snapshot-client")

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/responses", bytes.NewReader(body)).WithContext(requestContext)
	ginContext.Request.Host = "localhost:8080"
	ginContext.Request.Header.Set("Accept", "*/*")
	ginContext.Request.Header.Set("Authorization", "Bearer sk-sub2api-curl-snapshot")
	ginContext.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	ginContext.Request.Header.Set("User-Agent", "curl/8.7.1")
	ginContext.Request.Header.Set(curlCodexProfileHeader, curlCodexProfileAgent)
	ginContext.Request.Header.Set(curlCodexCapabilitiesHeader, "tool-loop, encrypted-reasoning, code-mode-exec, async-wait, interactive-input, multi-agent, remote-compaction-v2")
	ginContext.Set("api_key", &APIKey{ID: 4242})

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
				DefaultModel:              "gpt-5.6-sol",
				DefaultReasoningEffort:    "high",
				DefaultReasoningContext:   "all_turns",
				EnableRemoteCompactionV2:  true,
				RequireToolLoopCapability: true,
			},
		}},
		httpUpstream:   upstream,
		requestCapture: capture,
	}
	account := &Account{
		ID:          9001,
		Name:        "curl-snapshot@example.invalid",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "snapshot-oauth-token",
			"chatgpt_account_id": "00000000-0000-4000-8000-000000000042",
		},
		Extra: map[string]any{
			"openai_passthrough": true,
			"openai_device_id":   "48ca0404-a22d-4f52-8407-b9f6fc68bd04",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	service.CaptureDownstreamRequest(ginContext, body)
	result, forwardErr := service.Forward(requestContext, ginContext, account, body)
	require.Error(t, forwardErr, "synthetic upstream response should stop after request capture")
	require.Nil(t, result)
	require.NotNil(t, upstream.lastReq)

	capture.close()
	downstream := readSingleOpenAIRequestCaptureRecord(t, downstreamPath)
	upstreamRecord := readSingleOpenAIRequestCaptureRecord(t, upstreamPath)
	require.Equal(t, openAIRequestCaptureDownstream, downstream.Direction)
	require.Equal(t, openAIRequestCaptureUpstream, upstreamRecord.Direction)
	require.Equal(t, "curl-snapshot-request", upstreamRecord.RequestID)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", upstreamRecord.URL)
	var upstreamBody map[string]any
	require.NoError(t, json.Unmarshal(upstreamRecord.Body, &upstreamBody))
	require.Equal(t, true, upstreamBody["stream"])
	require.Equal(t, false, upstreamBody["store"])
	require.Equal(t, "gpt-5.6-sol", upstreamBody["model"])
	require.NotContains(t, upstreamBody, "instructions")
	require.Equal(t, map[string]any{"context": "all_turns", "effort": "high"}, upstreamBody["reasoning"])
	require.Contains(t, string(upstreamRecord.Body), `"reasoning":{"effort":"high","context":"all_turns"}`)
	require.NotContains(t, upstreamBody, "tools")
	require.Equal(t, "auto", upstreamBody["tool_choice"])
	require.Equal(t, false, upstreamBody["parallel_tool_calls"])
	input := upstreamBody["input"].([]any)
	require.Equal(t, "additional_tools", input[0].(map[string]any)["type"])
	require.Len(t, input[0].(map[string]any)["tools"], 4)
	require.Equal(t, openai.CodexCLI0145BaseInstructionsForModel("gpt-5.6-sol"), input[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	upstreamHeaders := http.Header(upstreamRecord.Headers)
	require.Equal(t, "text/event-stream", upstreamHeaders.Get("Accept"))
	require.Equal(t, curlCodexProfileDefaultUserAgent, upstreamHeaders.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", upstreamHeaders.Get("Originator"))
	require.Equal(t, "true", upstreamHeaders.Get(responsesLiteHeader))
	require.Equal(t, "remote_compaction_v2", upstreamHeaders.Get("X-Codex-Beta-Features"))
	require.Equal(t, upstreamBody["client_metadata"].(map[string]any)["x-codex-turn-metadata"], upstreamHeaders.Get("X-Codex-Turn-Metadata"))
	require.NotEmpty(t, recorder.Header().Get(curlCodexResponseSessionHeader))
}

// TestCaptureNativeCodexCLIForwardSnapshot replays a request captured directly
// from the Codex CLI through the production OAuth passthrough path and records
// the request immediately before the HTTP transport. Both endpoints are local:
// the source request comes from a JSONL fixture and the upstream is an in-memory
// recorder, so the test never contacts ChatGPT.
//
// Set SUB2API_CODEX_CLI_REQUEST_PATH to the direct CLI capture and
// SUB2API_CODEX_CLI_CAPTURE_DIR to an empty output directory.
func TestCaptureNativeCodexCLIForwardSnapshot(t *testing.T) {
	requestPath := strings.TrimSpace(os.Getenv("SUB2API_CODEX_CLI_REQUEST_PATH"))
	outputDir := strings.TrimSpace(os.Getenv("SUB2API_CODEX_CLI_CAPTURE_DIR"))
	if requestPath == "" || outputDir == "" {
		t.Skip("set SUB2API_CODEX_CLI_REQUEST_PATH and SUB2API_CODEX_CLI_CAPTURE_DIR to generate a native CLI forwarding snapshot")
	}

	rawRecord, err := os.ReadFile(requestPath)
	require.NoError(t, err)
	var source struct {
		Method  string          `json:"method"`
		Headers http.Header     `json:"headers"`
		Body    json.RawMessage `json:"body"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(rawRecord), &source))
	canonicalHeaders := make(http.Header, len(source.Headers))
	for key, values := range source.Headers {
		canonicalHeaders[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	source.Headers = canonicalHeaders
	require.Equal(t, http.MethodPost, source.Method)
	require.NotEmpty(t, source.Body)
	require.Contains(t, source.Headers.Get("User-Agent"), "codex-tui/0.145.0")
	require.Equal(t, "codex-tui", source.Headers.Get("Originator"))

	gin.SetMode(gin.TestMode)
	capture, err := newOpenAIRequestCapture(outputDir)
	require.NoError(t, err)
	t.Cleanup(capture.close)

	snapshotTime := time.Date(2026, time.August, 4, 3, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	capture.now = func() time.Time { return snapshotTime }
	downstreamPath := filepath.Join(outputDir, "openai-downstream-20260804-03.jsonl")
	upstreamPath := filepath.Join(outputDir, "openai-upstream-20260804-03.jsonl")
	require.NoFileExists(t, downstreamPath, "use an empty snapshot output directory")
	require.NoFileExists(t, upstreamPath, "use an empty snapshot output directory")

	requestContext := context.WithValue(context.Background(), ctxkey.RequestID, "codex-cli-snapshot-request")
	requestContext = context.WithValue(requestContext, ctxkey.ClientRequestID, "codex-cli-snapshot-client")
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/responses", bytes.NewReader(source.Body)).WithContext(requestContext)
	ginContext.Request.Host = "localhost:8080"
	for key, values := range source.Headers {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			ginContext.Request.Header.Add(key, value)
		}
	}
	ginContext.Request.Header.Set("Authorization", "Bearer sk-sub2api-codex-cli-snapshot")
	ginContext.Request.Header.Set("Content-Length", strconv.Itoa(len(source.Body)))
	ginContext.Set("api_key", &APIKey{ID: 4242})

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
	}}
	service := &OpenAIGatewayService{
		cfg:            &config.Config{},
		httpUpstream:   upstream,
		requestCapture: capture,
	}
	account := &Account{
		ID:          9001,
		Name:        "codex-cli-snapshot@example.invalid",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "snapshot-oauth-token",
			"chatgpt_account_id": "00000000-0000-4000-8000-000000000042",
		},
		Extra: map[string]any{
			"openai_passthrough": true,
			"openai_device_id":   "48ca0404-a22d-4f52-8407-b9f6fc68bd04",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	service.CaptureDownstreamRequest(ginContext, source.Body)
	result, forwardErr := service.Forward(requestContext, ginContext, account, source.Body)
	require.Error(t, forwardErr, "synthetic upstream response should stop after request capture")
	require.Nil(t, result)
	require.NotNil(t, upstream.lastReq)

	capture.close()
	downstream := readSingleOpenAIRequestCaptureRecord(t, downstreamPath)
	upstreamRecord := readSingleOpenAIRequestCaptureRecord(t, upstreamPath)
	require.Equal(t, openAIRequestCaptureDownstream, downstream.Direction)
	require.Equal(t, openAIRequestCaptureUpstream, upstreamRecord.Direction)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", upstreamRecord.URL)
	require.JSONEq(t, string(source.Body), string(upstreamRecord.Body), "native CLI body should pass through unchanged")

	upstreamHeaders := http.Header(upstreamRecord.Headers)
	require.Equal(t, "codex_cli_rs", upstreamHeaders.Get("Originator"))
	require.Contains(t, upstreamHeaders.Get("User-Agent"), "codex_cli_rs/0.145.0")
	require.Equal(t, "true", upstreamHeaders.Get(responsesLiteHeader))
	require.Equal(t, source.Headers.Get("X-Codex-Turn-Metadata"), upstreamHeaders.Get("X-Codex-Turn-Metadata"))
}
