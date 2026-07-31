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
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	bigModelCaptureOptInEnv = "SUB2API_RUN_BIGMODEL_CAPTURE"
	bigModelCaptureLogDir   = "/Users/ling/Demo/PacketCapture/sub2api/glm"
	bigModelCaptureFileMode = 0o600
)

// bigModelCaptureOfflineUpstream records the final request passed to the
// transport abstraction and returns a synthetic in-process SSE response. It
// never resolves DNS, opens a socket, or contacts BigModel.
type bigModelCaptureOfflineUpstream struct {
	request *http.Request
	body    []byte
	calls   int
}

func (u *bigModelCaptureOfflineUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *bigModelCaptureOfflineUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("BigModel offline capture received nil request")
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read BigModel offline capture request: %w", err)
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))

	cloned := req.Clone(context.Background())
	cloned.Header = req.Header.Clone()
	cloned.Body = io.NopCloser(bytes.NewReader(bytes.Clone(body)))
	cloned.ContentLength = int64(len(body))
	u.request = cloned
	u.body = bytes.Clone(body)
	u.calls++

	responseBody := strings.Join([]string{
		`data: {"id":"chatcmpl_offline_glm","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_offline_glm","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"offline capture"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_offline_glm","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_offline_glm","object":"chat.completion.chunk","model":"glm-5.2","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"offline-bigmodel-request"},
		},
		Body: io.NopCloser(strings.NewReader(responseBody)),
	}, nil
}

// TestCaptureBigModelGLMUpstreamRequest runs the real /v1/messages compatibility
// forwarding code up to the HTTPUpstream boundary. The final wire request is
// dumped with the same debug snapshot formatter used by the Anthropic capture.
func TestCaptureBigModelGLMUpstreamRequest(t *testing.T) {
	if os.Getenv(bigModelCaptureOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the developer-local BigModel capture", bigModelCaptureOptInEnv)
	}

	gin.SetMode(gin.TestMode)
	clientBody := []byte(`{` +
		`"model":"glm-5.2",` +
		`"max_tokens":1024,` +
		`"stream":true,` +
		`"messages":[{"role":"user","content":"Hello, please introduce yourself."}]` +
		`}`)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(clientBody))
	c.Request.Header.Set("User-Agent", "curl/8.4.0")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Anthropic-Version", "2023-06-01")

	const apiKey = "REDACTED-DEMO-BIGMODEL-API-KEY"
	account := &Account{
		ID:          2001,
		Name:        "bigmodel-glm-offline-capture",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  apiKey,
			"base_url": "https://open.bigmodel.cn/api/paas/v4",
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	cfg := &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
	upstream := &bigModelCaptureOfflineUpstream{}
	svc := &OpenAIGatewayService{
		cfg:          cfg,
		httpUpstream: upstream,
	}

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(shanghai)
	require.NoError(t, os.MkdirAll(bigModelCaptureLogDir, 0o700))
	logPath := filepath.Join(bigModelCaptureLogDir, "log_"+now.Format("20060102_150405")+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, bigModelCaptureFileMode)
	require.NoError(t, err, "capture log name must be new; never append to an existing artifact")
	require.NoError(t, file.Chmod(bigModelCaptureFileMode))

	snapshotLogger := &GatewayService{cfg: cfg}
	snapshotLogger.debugGatewayBodyFile.Store(file)
	t.Cleanup(func() {
		snapshotLogger.debugGatewayBodyFile.Store(nil)
		_ = file.Close()
	})

	writeBigModelCaptureBanner(t, file, now, account)
	snapshotLogger.debugLogGatewaySnapshot("CLIENT_ORIGINAL", c.Request.Header, clientBody, map[string]string{
		"account":        fmt.Sprintf("%d(%s)", account.ID, account.Name),
		"account_type":   string(account.Type),
		"inbound_method": http.MethodPost,
		"inbound_url":    "/v1/messages",
		"model":          gjson.GetBytes(clientBody, "model").String(),
		"provider":       "bigmodel",
		"stream":         "true",
	})

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, clientBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)

	require.Equal(t, 1, upstream.calls)
	require.NotNil(t, upstream.request)
	require.Equal(t, http.MethodPost, upstream.request.Method)
	require.Equal(t, "https://open.bigmodel.cn/api/paas/v4/chat/completions", upstream.request.URL.String())
	require.Equal(t, "Bearer "+apiKey, upstream.request.Header.Get("Authorization"))
	require.Equal(t, "glm-5.2", gjson.GetBytes(upstream.body, "model").String())
	require.True(t, gjson.GetBytes(upstream.body, "stream").Bool())
	require.True(t, gjson.GetBytes(upstream.body, "stream_options.include_usage").Bool())
	require.False(t, gjson.GetBytes(upstream.body, "input").Exists())

	snapshotLogger.debugLogGatewaySnapshot("UPSTREAM_FORWARD", upstream.request.Header, upstream.body, map[string]string{
		"account":           fmt.Sprintf("%d(%s)", account.ID, account.Name),
		"account_type":      string(account.Type),
		"network":           "disabled (in-process HTTPUpstream recorder)",
		"provider":          "bigmodel",
		"source_endpoint":   "/v1/messages",
		"upstream_endpoint": "/chat/completions",
		"url":               upstream.request.URL.String(),
	})
	require.NoError(t, file.Sync())

	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logText := string(logBytes)
	require.Contains(t, logText, "CLIENT_ORIGINAL")
	require.Contains(t, logText, "UPSTREAM_FORWARD")
	require.Contains(t, logText, "https://open.bigmodel.cn/api/paas/v4/chat/completions")
	require.Contains(t, logText, "Authorization: Bearer [redacted]")
	require.Contains(t, logText, `"stream_options": {`)
	require.Contains(t, logText, `"include_usage": true`)
	require.NotContains(t, logText, apiKey)

	logInfo, err := os.Stat(logPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(bigModelCaptureFileMode), logInfo.Mode().Perm())
	t.Logf("BIGMODEL_CAPTURE_LOG=%s", logPath)
}

func writeBigModelCaptureBanner(t *testing.T, file *os.File, now time.Time, account *Account) {
	t.Helper()
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api curl /v1/messages -> BigModel GLM upstream request offline capture",
		"# generated_at: " + now.Format("2006-01-02 15:04:05 MST"),
		"# account: " + account.Name,
		"# model: glm-5.2",
		"# upstream: https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"# network: disabled; OpenAIGatewayService.ForwardAsAnthropic uses only an in-process fake HTTPUpstream",
		"# CLIENT_ORIGINAL = curl input to sub2api",
		"# UPSTREAM_FORWARD = object that would be sent to BigModel",
		"################################################################",
		"",
	}, "\n")
	_, err := file.WriteString(banner)
	require.NoError(t, err)
}
