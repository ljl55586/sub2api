//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	openAICurlCaptureOptInEnv = "SUB2API_RUN_OPENAI_CURL_CAPTURE"
	openAICurlCaptureLogDir   = "/Users/ling/Demo/PacketCapture/sub2api/openai/curl"
	openAICurlCaptureFileMode = 0o600
)

const openAICurlCaptureToken = "SYNTHETIC-OPENAI-OAUTH-TOKEN"

type openAICurlCaptureSnapshot struct {
	Request *http.Request
	Body    []byte
}

// openAICurlCaptureUpstream records the request at the final HTTPUpstream
// boundary and returns a synthetic response. It never opens a socket.
type openAICurlCaptureUpstream struct {
	mu        sync.Mutex
	snapshots []openAICurlCaptureSnapshot
}

func (u *openAICurlCaptureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("OpenAI curl capture received a nil upstream request")
	}

	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read OpenAI curl capture request body: %w", err)
		}
		_ = req.Body.Close()
	}

	clonedRequest := req.Clone(req.Context())
	clonedRequest.Header = req.Header.Clone()
	clonedRequest.Body = io.NopCloser(bytes.NewReader(bytes.Clone(body)))
	clonedRequest.GetBody = nil
	clonedRequest.ContentLength = int64(len(body))

	u.mu.Lock()
	u.snapshots = append(u.snapshots, openAICurlCaptureSnapshot{
		Request: clonedRequest,
		Body:    bytes.Clone(body),
	})
	u.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_offline_openai_curl_capture"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_offline_openai_curl_capture","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"message","id":"msg_offline_capture","role":"assistant","status":"completed","content":[{"type":"output_text","text":"offline capture complete"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
			"",
			"data: [DONE]",
			"",
			"",
		}, "\n"))),
	}, nil
}

func (u *openAICurlCaptureUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *openAICurlCaptureUpstream) Snapshot() (openAICurlCaptureSnapshot, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.snapshots) != 1 {
		return openAICurlCaptureSnapshot{}, false
	}

	snapshot := u.snapshots[0]
	clonedRequest := snapshot.Request.Clone(snapshot.Request.Context())
	clonedRequest.Header = snapshot.Request.Header.Clone()
	clonedRequest.Body = io.NopCloser(bytes.NewReader(bytes.Clone(snapshot.Body)))
	clonedRequest.GetBody = nil
	clonedRequest.ContentLength = int64(len(snapshot.Body))
	return openAICurlCaptureSnapshot{
		Request: clonedRequest,
		Body:    bytes.Clone(snapshot.Body),
	}, true
}

// TestCaptureOpenAICurlUpstreamRequest drives the real
// OpenAIGatewayService.Forward path for a curl -> sub2api -> OpenAI OAuth
// request. The injected HTTPUpstream records the fully constructed wire
// request and prevents all network access. The existing gateway debug snapshot
// formatter then writes the client and upstream request shapes to a local file.
func TestCaptureOpenAICurlUpstreamRequest(t *testing.T) {
	if os.Getenv(openAICurlCaptureOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the developer-local offline capture", openAICurlCaptureOptInEnv)
	}

	gin.SetMode(gin.TestMode)
	clientBody := []byte(`{` +
		`"model":"gpt-5.6",` +
		`"instructions":"You are a helpful assistant.",` +
		`"input":"Hello, please introduce yourself.",` +
		`"stream":false` +
		`}`)

	clientRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(clientRecorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(clientBody))
	c.Request.Header.Set("Accept", "*/*")
	c.Request.Header.Set("Authorization", "Bearer sk-sub2api-synthetic-client-key")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "curl/8.4.0")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	c.Set("api_key", &APIKey{ID: 7002})

	account := &Account{
		ID:          2001,
		Name:        "openai-oauth-offline-capture",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       openAICurlCaptureToken,
			"chatgpt_account_id": "synthetic-chatgpt-account",
		},
		Extra: map[string]any{
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
	logPath := filepath.Join(openAICurlCaptureLogDir, "log_"+now.Format("20060102_150405")+".log")
	require.NoError(t, os.MkdirAll(openAICurlCaptureLogDir, 0o755))
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, openAICurlCaptureFileMode)
	require.NoError(t, err, "capture log name must be new; never append to an existing artifact")
	t.Cleanup(func() { _ = file.Close() })
	require.NoError(t, file.Chmod(openAICurlCaptureFileMode))

	// Reuse the existing GatewayService debug snapshot formatter. OpenAI's
	// final wire request is supplied by the in-process HTTPUpstream recorder.
	snapshotLogger := &GatewayService{}
	snapshotLogger.debugGatewayBodyFile.Store(file)
	t.Cleanup(func() { snapshotLogger.debugGatewayBodyFile.Store(nil) })

	writeOpenAICurlCaptureBanner(t, file, now, account)
	snapshotLogger.debugLogGatewaySnapshot(
		"OPENAI_CLIENT_ORIGINAL",
		c.Request.Header,
		clientBody,
		map[string]string{
			"account":      fmt.Sprintf("%d(%s)", account.ID, account.Name),
			"account_type": string(account.Type),
			"model":        gjson.GetBytes(clientBody, "model").String(),
			"route":        c.Request.URL.Path,
			"stream":       "false",
		},
	)

	result, err := svc.Forward(context.Background(), c, account, clientBody)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)

	snapshot, ok := upstream.Snapshot()
	require.True(t, ok, "the offline capture must issue exactly one HTTPUpstream request")
	require.NotNil(t, snapshot.Request)
	require.Equal(t, chatgptCodexURL, snapshot.Request.URL.String())
	require.Equal(t, "chatgpt.com", snapshot.Request.Host)
	require.Equal(t, "Bearer "+openAICurlCaptureToken, snapshot.Request.Header.Get("Authorization"))
	require.Equal(t, "synthetic-chatgpt-account", snapshot.Request.Header.Get("chatgpt-account-id"))
	require.Equal(t, "text/event-stream", snapshot.Request.Header.Get("Accept"))
	require.Equal(t, "application/json", snapshot.Request.Header.Get("Content-Type"))
	require.Equal(t, "responses=experimental", snapshot.Request.Header.Get("OpenAI-Beta"))
	require.Equal(t, "codex_cli_rs", snapshot.Request.Header.Get("Originator"))
	require.Equal(t, codexCLIUserAgent, snapshot.Request.Header.Get("User-Agent"))
	require.NotContains(t, snapshot.Request.Header.Get("User-Agent"), "curl")

	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(snapshot.Body, "model").String())
	require.Equal(t, "You are a helpful assistant.", gjson.GetBytes(snapshot.Body, "instructions").String())
	require.False(t, gjson.GetBytes(snapshot.Body, "store").Bool())
	require.True(t, gjson.GetBytes(snapshot.Body, "stream").Bool())
	require.True(t, gjson.GetBytes(snapshot.Body, "input").IsArray())
	require.Equal(t, "message", gjson.GetBytes(snapshot.Body, "input.0.type").String())
	require.Equal(t, "user", gjson.GetBytes(snapshot.Body, "input.0.role").String())
	require.Equal(t, "Hello, please introduce yourself.", gjson.GetBytes(snapshot.Body, "input.0.content").String())

	snapshotLogger.debugLogGatewaySnapshot(
		"OPENAI_UPSTREAM_FORWARD",
		snapshot.Request.Header,
		snapshot.Body,
		map[string]string{
			"account":        fmt.Sprintf("%d(%s)", account.ID, account.Name),
			"account_type":   string(account.Type),
			"host":           snapshot.Request.Host,
			"is_codex_cli":   "false",
			"network":        "disabled",
			"original_model": gjson.GetBytes(clientBody, "model").String(),
			"stream":         "true",
			"upstream_model": gjson.GetBytes(snapshot.Body, "model").String(),
			"url":            snapshot.Request.URL.String(),
		},
	)

	require.NoError(t, file.Sync())
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logBytes), "OPENAI_CLIENT_ORIGINAL")
	require.Contains(t, string(logBytes), "OPENAI_UPSTREAM_FORWARD")
	require.Contains(t, string(logBytes), chatgptCodexURL)
	require.Contains(t, string(logBytes), `"model": "gpt-5.6-sol"`)
	require.Contains(t, string(logBytes), `"store": false`)
	require.Contains(t, string(logBytes), `"stream": true`)
	require.Contains(t, string(logBytes), "[redacted]")
	require.NotContains(t, string(logBytes), openAICurlCaptureToken)
	require.NotContains(t, string(logBytes), "sk-sub2api-synthetic-client-key")

	info, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(openAICurlCaptureFileMode), info.Mode().Perm())
	require.Greater(t, info.Size(), int64(0))
	t.Logf("OPENAI_CURL_CAPTURE_LOG=%s", logPath)
}

func writeOpenAICurlCaptureBanner(t *testing.T, file *os.File, now time.Time, account *Account) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api OpenAI OAuth upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# account: " + account.Name,
		"# network: disabled; OpenAIGatewayService.Forward uses only an in-process fake HTTPUpstream",
		"# OPENAI_CLIENT_ORIGINAL = curl input to sub2api",
		"# OPENAI_UPSTREAM_FORWARD = object that would be sent to chatgpt.com/backend-api/codex/responses",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}
