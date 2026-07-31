//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAICodexCLICaptureOptInEnv = "SUB2API_RUN_OPENAI_CODEX_CLI_CAPTURE"
	openAICodexCLICaptureLogDir   = "/Users/ling/Demo/PacketCapture/sub2api/openai/codex-cli"
	openAICodexCLICaptureTrace    = "/Users/ling/Demo/PacketCapture/.codex-trace/log-2026-07-28-06-33-07.json"
	openAICodexCLICaptureFileMode = 0o600

	openAICodexCLICaptureUpstreamToken = "SYNTHETIC-CODEX-CLI-UPSTREAM-OAUTH-TOKEN"
	openAICodexCLICaptureClientKey     = "sk-sub2api-synthetic-codex-cli-key"
	openAICodexCLICaptureSessionID     = "11111111-1111-4111-8111-111111111111"
	openAICodexCLICaptureTurnID        = "22222222-2222-4222-8222-222222222222"
	openAICodexCLICaptureInstallID     = "33333333-3333-4333-8333-333333333333"
	openAICodexCLICaptureWindowID      = openAICodexCLICaptureSessionID + ":0"
)

type openAICodexCLITraceEntry struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
}

type openAICodexCLIOriginalIDs struct {
	Authorization    string
	ChatGPTAccountID string
	SessionID        string
	ThreadID         string
	ClientRequestID  string
	TurnID           string
	InstallationID   string
	WindowID         string
}

// TestCaptureOpenAICodexCLIUpstreamRequest replays the first request shape
// captured from a real Codex CLI trace as a downstream request to sub2api.
// Sensitive IDs are replaced while preserving their equality relationships.
// The production zstd request-body decoder and OpenAIGatewayService.Forward
// path both run; the injected HTTPUpstream recorder prevents network access.
func TestCaptureOpenAICodexCLIUpstreamRequest(t *testing.T) {
	if os.Getenv(openAICodexCLICaptureOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the developer-local Codex CLI offline capture", openAICodexCLICaptureOptInEnv)
	}

	gin.SetMode(gin.TestMode)
	traceHeaders, traceBody, originalIDs := loadSanitizedOpenAICodexCLITraceRequest(t)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(traceBody, "model").String())
	require.False(t, gjson.GetBytes(traceBody, "instructions").Exists())

	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressedBody := encoder.EncodeAll(traceBody, nil)
	encoder.Close()
	require.Less(t, len(compressedBody), len(traceBody))

	clientRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(clientRecorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedBody))
	for key, values := range traceHeaders {
		for _, value := range values {
			c.Request.Header.Add(key, value)
		}
	}
	c.Request.Host = "sub2api.synthetic.local"
	c.Request.Header.Set("Content-Encoding", "zstd")
	c.Request.Header.Set("Content-Length", fmt.Sprintf("%d", len(compressedBody)))
	c.Request.ContentLength = int64(len(compressedBody))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	c.Set("api_key", &APIKey{ID: 7003})

	// Preserve the real Codex wire header shape for the readable client
	// snapshot. The production decoder mutates c.Request headers after decode.
	clientWireHeaders := c.Request.Header.Clone()
	decodedBody, err := pkghttputil.ReadLenientJSONRequestBodyWithPrealloc(c.Request, 64<<20)
	require.NoError(t, err)
	require.JSONEq(t, string(traceBody), string(decodedBody))
	require.Empty(t, c.Request.Header.Get("Content-Encoding"))
	require.Empty(t, c.Request.Header.Get("Content-Length"))
	require.Equal(t, int64(len(decodedBody)), c.Request.ContentLength)

	account := &Account{
		ID:          2002,
		Name:        "openai-codex-cli-offline-capture",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       openAICodexCLICaptureUpstreamToken,
			"chatgpt_account_id": "synthetic-upstream-chatgpt-account",
		},
		Extra: map[string]any{
			"openai_device_id":                          openAICodexCLICaptureInstallID,
			"openai_passthrough":                        false,
			"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff,
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	upstream := &openAICurlCaptureUpstream{}
	svc := &OpenAIGatewayService{
		cfg:                  cfg,
		httpUpstream:         upstream,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		toolCorrector:        NewCodexToolCorrector(),
	}

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	logPath := filepath.Join(openAICodexCLICaptureLogDir, "log_"+now.Format("20060102_150405")+".log")
	require.NoError(t, os.MkdirAll(openAICodexCLICaptureLogDir, 0o755))
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, openAICodexCLICaptureFileMode)
	require.NoError(t, err, "capture log name must be new; never append to an existing artifact")
	t.Cleanup(func() { _ = file.Close() })
	require.NoError(t, file.Chmod(openAICodexCLICaptureFileMode))

	snapshotLogger := &GatewayService{}
	snapshotLogger.debugGatewayBodyFile.Store(file)
	t.Cleanup(func() { snapshotLogger.debugGatewayBodyFile.Store(nil) })

	writeOpenAICodexCLICaptureBanner(t, file, now, account, len(compressedBody), len(decodedBody))
	snapshotLogger.debugLogGatewaySnapshot(
		"OPENAI_CODEX_CLI_CLIENT_ORIGINAL",
		clientWireHeaders,
		decodedBody,
		map[string]string{
			"account":            fmt.Sprintf("%d(%s)", account.ID, account.Name),
			"account_type":       string(account.Type),
			"body_display":       "decoded JSON; wire body was zstd",
			"client_host":        c.Request.Host,
			"compressed_bytes":   fmt.Sprintf("%d", len(compressedBody)),
			"decompressed_bytes": fmt.Sprintf("%d", len(decodedBody)),
			"model":              gjson.GetBytes(decodedBody, "model").String(),
			"route":              c.Request.URL.Path,
			"source_trace_index": "0",
			"stream":             "true",
		},
	)

	result, err := svc.Forward(context.Background(), c, account, decodedBody)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)

	snapshot, ok := upstream.Snapshot()
	require.True(t, ok, "the offline Codex CLI capture must issue exactly one HTTPUpstream request")
	require.NotNil(t, snapshot.Request)
	require.Equal(t, chatgptCodexURL, snapshot.Request.URL.String())
	require.Equal(t, "chatgpt.com", snapshot.Request.Host)
	require.Equal(t, "Bearer "+openAICodexCLICaptureUpstreamToken, snapshot.Request.Header.Get("Authorization"))
	require.Equal(t, "synthetic-upstream-chatgpt-account", snapshot.Request.Header.Get("chatgpt-account-id"))
	require.Equal(t, traceHeaders.Get("User-Agent"), snapshot.Request.Header.Get("User-Agent"))
	require.Equal(t, "codex-tui", snapshot.Request.Header.Get("Originator"))
	require.Equal(t, "remote_compaction_v2", snapshot.Request.Header.Get("x-codex-beta-features"))
	require.Equal(t, traceHeaders.Get("x-codex-turn-metadata"), snapshot.Request.Header.Get("x-codex-turn-metadata"))
	require.Equal(t, "responses=experimental", snapshot.Request.Header.Get("OpenAI-Beta"))
	require.Empty(t, snapshot.Request.Header.Get("Version"))
	require.Empty(t, snapshot.Request.Header.Get("Thread-Id"))
	require.Empty(t, snapshot.Request.Header.Get("x-client-request-id"))
	require.Empty(t, snapshot.Request.Header.Get("x-codex-window-id"))
	require.Empty(t, snapshot.Request.Header.Get("x-openai-internal-codex-responses-lite"))
	require.Empty(t, snapshot.Request.Header.Get("Content-Encoding"))

	isolatedSessionID := snapshot.Request.Header.Get("session_id")
	require.NotEmpty(t, isolatedSessionID)
	require.Equal(t, isolatedSessionID, snapshot.Request.Header.Get("conversation_id"))
	require.NotEqual(t, openAICodexCLICaptureSessionID, isolatedSessionID)

	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(snapshot.Body, "model").String())
	require.False(t, gjson.GetBytes(snapshot.Body, "store").Bool())
	require.True(t, gjson.GetBytes(snapshot.Body, "stream").Bool())
	require.Equal(t, "xhigh", gjson.GetBytes(snapshot.Body, "reasoning.effort").String())
	require.Equal(t, "all_turns", gjson.GetBytes(snapshot.Body, "reasoning.context").String())
	require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(snapshot.Body, "include.0").String())
	require.Equal(t, openAICodexCLICaptureSessionID, gjson.GetBytes(snapshot.Body, "prompt_cache_key").String())
	require.Equal(t, openAICodexCLICaptureSessionID, gjson.GetBytes(snapshot.Body, "client_metadata.session_id").String())
	require.Equal(t, openAICodexCLICaptureSessionID, gjson.GetBytes(snapshot.Body, "client_metadata.thread_id").String())
	require.Equal(t, openAICodexCLICaptureTurnID, gjson.GetBytes(snapshot.Body, "client_metadata.turn_id").String())
	require.Equal(t, openAICodexCLICaptureInstallID, gjson.GetBytes(snapshot.Body, "client_metadata.x-codex-installation-id").String())
	require.Equal(t, traceHeaders.Get("x-codex-turn-metadata"), gjson.GetBytes(snapshot.Body, "client_metadata.x-codex-turn-metadata").String())
	require.Equal(t, len(gjson.GetBytes(decodedBody, "input").Array()), len(gjson.GetBytes(snapshot.Body, "input").Array()))
	require.True(t, gjson.GetBytes(snapshot.Body, `input.#(type=="additional_tools")`).Exists())
	require.NotEmpty(t, strings.TrimSpace(gjson.GetBytes(snapshot.Body, "instructions").String()),
		"the current synthetic Forward path injects top-level instructions even for an official Codex request")

	snapshotLogger.debugLogGatewaySnapshot(
		"OPENAI_CODEX_CLI_UPSTREAM_FORWARD",
		snapshot.Request.Header,
		snapshot.Body,
		map[string]string{
			"account":            fmt.Sprintf("%d(%s)", account.ID, account.Name),
			"account_type":       string(account.Type),
			"host":               snapshot.Request.Host,
			"is_codex_cli":       "true",
			"network":            "disabled",
			"original_model":     gjson.GetBytes(decodedBody, "model").String(),
			"stream":             "true",
			"uncompressed_bytes": fmt.Sprintf("%d", len(snapshot.Body)),
			"upstream_model":     gjson.GetBytes(snapshot.Body, "model").String(),
			"url":                snapshot.Request.URL.String(),
		},
	)

	require.NoError(t, file.Sync())
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logBytes), "OPENAI_CODEX_CLI_CLIENT_ORIGINAL")
	require.Contains(t, string(logBytes), "OPENAI_CODEX_CLI_UPSTREAM_FORWARD")
	require.Contains(t, string(logBytes), chatgptCodexURL)
	require.Contains(t, string(logBytes), `"model": "gpt-5.6-sol"`)
	require.Contains(t, string(logBytes), `"additional_tools"`)
	require.Contains(t, string(logBytes), `"instructions":`)
	require.Contains(t, string(logBytes), "[redacted]")
	require.NotContains(t, string(logBytes), openAICodexCLICaptureUpstreamToken)
	require.NotContains(t, string(logBytes), openAICodexCLICaptureClientKey)
	assertOpenAICodexCLIOriginalIDsRedacted(t, logBytes, originalIDs)

	info, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(openAICodexCLICaptureFileMode), info.Mode().Perm())
	require.Greater(t, info.Size(), int64(0))
	t.Logf("OPENAI_CODEX_CLI_CAPTURE_LOG=%s", logPath)
}

func loadSanitizedOpenAICodexCLITraceRequest(t *testing.T) (http.Header, []byte, openAICodexCLIOriginalIDs) {
	t.Helper()
	raw, err := os.ReadFile(openAICodexCLICaptureTrace)
	require.NoError(t, err)

	var entries []openAICodexCLITraceEntry
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.NotEmpty(t, entries)
	entry := entries[0]
	require.Equal(t, http.MethodPost, entry.Request.Method)
	require.Equal(t, chatgptCodexURL, entry.Request.URL)

	headers := make(http.Header, len(entry.Request.Headers))
	for key, value := range entry.Request.Headers {
		if strings.EqualFold(key, "host") || strings.EqualFold(key, "content-length") {
			continue
		}
		headers.Set(key, value)
	}

	var compact bytes.Buffer
	require.NoError(t, json.Compact(&compact, entry.Request.Body))
	body := compact.Bytes()
	originalIDs := openAICodexCLIOriginalIDs{
		Authorization:    headers.Get("Authorization"),
		ChatGPTAccountID: headers.Get("chatgpt-account-id"),
		SessionID:        headers.Get("session-id"),
		ThreadID:         headers.Get("thread-id"),
		ClientRequestID:  headers.Get("x-client-request-id"),
		TurnID:           gjson.GetBytes(body, "client_metadata.turn_id").String(),
		InstallationID:   gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(),
		WindowID:         headers.Get("x-codex-window-id"),
	}

	turnMetadata := sanitizedOpenAICodexCLITurnMetadata(t, headers.Get("x-codex-turn-metadata"))
	headers.Set("Authorization", "Bearer "+openAICodexCLICaptureClientKey)
	headers.Set("chatgpt-account-id", "synthetic-client-chatgpt-account")
	headers.Set("session-id", openAICodexCLICaptureSessionID)
	headers.Set("thread-id", openAICodexCLICaptureSessionID)
	headers.Set("x-client-request-id", openAICodexCLICaptureSessionID)
	headers.Set("x-codex-window-id", openAICodexCLICaptureWindowID)
	headers.Set("x-codex-turn-metadata", turnMetadata)

	body = setOpenAICodexCLITraceJSONValue(t, body, "prompt_cache_key", openAICodexCLICaptureSessionID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.session_id", openAICodexCLICaptureSessionID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.thread_id", openAICodexCLICaptureSessionID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.turn_id", openAICodexCLICaptureTurnID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.x-codex-installation-id", openAICodexCLICaptureInstallID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.x-codex-window-id", openAICodexCLICaptureWindowID)
	body = setOpenAICodexCLITraceJSONValue(t, body, "client_metadata.x-codex-turn-metadata", turnMetadata)
	for index, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("internal_chat_message_metadata_passthrough.turn_id").Exists() {
			body = setOpenAICodexCLITraceJSONValue(
				t,
				body,
				fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", index),
				openAICodexCLICaptureTurnID,
			)
		}
	}

	require.True(t, gjson.ValidBytes(body))
	return headers, body, originalIDs
}

func sanitizedOpenAICodexCLITurnMetadata(t *testing.T, raw string) string {
	t.Helper()
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
	metadata["installation_id"] = openAICodexCLICaptureInstallID
	metadata["session_id"] = openAICodexCLICaptureSessionID
	metadata["thread_id"] = openAICodexCLICaptureSessionID
	metadata["turn_id"] = openAICodexCLICaptureTurnID
	metadata["window_id"] = openAICodexCLICaptureWindowID
	metadata["turn_started_at_unix_ms"] = int64(1785283200000)
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	return string(encoded)
}

func setOpenAICodexCLITraceJSONValue(t *testing.T, body []byte, path string, value any) []byte {
	t.Helper()
	updated, err := sjson.SetBytes(body, path, value)
	require.NoError(t, err)
	return updated
}

func assertOpenAICodexCLIOriginalIDsRedacted(t *testing.T, logBytes []byte, ids openAICodexCLIOriginalIDs) {
	t.Helper()
	for name, value := range map[string]string{
		"authorization":      ids.Authorization,
		"chatgpt_account_id": ids.ChatGPTAccountID,
		"session_id":         ids.SessionID,
		"thread_id":          ids.ThreadID,
		"client_request_id":  ids.ClientRequestID,
		"turn_id":            ids.TurnID,
		"installation_id":    ids.InstallationID,
		"window_id":          ids.WindowID,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		require.NotContains(t, string(logBytes), value, "original %s must not appear in the synthetic capture", name)
	}
}

func writeOpenAICodexCLICaptureBanner(
	t *testing.T,
	file *os.File,
	now time.Time,
	account *Account,
	compressedBytes int,
	decompressedBytes int,
) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api Codex CLI -> OpenAI OAuth upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# source_trace: " + openAICodexCLICaptureTrace + " (request index 0)",
		"# account: " + account.Name,
		"# network: disabled; production zstd decoding + OpenAIGatewayService.Forward + in-process fake HTTPUpstream",
		"# sensitive trace IDs and both client/upstream credentials were replaced with synthetic values",
		fmt.Sprintf("# inbound_wire_bytes: %d zstd; decoded_json_bytes: %d", compressedBytes, decompressedBytes),
		"# OPENAI_CODEX_CLI_CLIENT_ORIGINAL = decoded view of the Codex CLI request sent to sub2api",
		"# OPENAI_CODEX_CLI_UPSTREAM_FORWARD = object that sub2api would send to chatgpt.com/backend-api/codex/responses",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}
