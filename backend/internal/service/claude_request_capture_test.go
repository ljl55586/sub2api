package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type claudeRequestCaptureHTTPUpstreamStub struct {
	body []byte
}

func (s *claudeRequestCaptureHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.respond(req)
}

func (s *claudeRequestCaptureHTTPUpstreamStub) DoWithTLS(
	req *http.Request,
	_ string,
	_ int64,
	_ int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return s.respond(req)
}

func (s *claudeRequestCaptureHTTPUpstreamStub) respond(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

func TestClaudeRequestCaptureSeparatesDirectionsAndRotatesHourly(t *testing.T) {
	dir := t.TempDir()
	capture, err := newClaudeRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	now := time.Date(2026, 8, 3, 10, 59, 59, 0, time.UTC)
	capture.now = func() time.Time { return now }
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-claude-capture-1")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-claude-capture-1")

	downstreamBody := []byte("{\n  \"model\": \"claude-sonnet-4-6\",\n  \"messages\": [{\"role\":\"user\",\"content\":\"hello\"}]\n}")
	downstreamReq := httptest.NewRequest(http.MethodPost, "/v1/messages?trace=1", bytes.NewReader(downstreamBody)).WithContext(ctx)
	downstreamReq.Header.Set("Authorization", "Bearer sk-downstream-secret")
	downstreamReq.Header.Set("Cookie", "session=downstream-secret")
	downstreamReq.Header.Set("X-Trace", "visible")
	require.NoError(t, capture.captureRequest(claudeRequestCaptureDownstream, downstreamReq, downstreamBody, nil))

	upstreamBody := []byte(`{"model":"claude-sonnet-4-6","stream":true}`)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(upstreamBody))
	require.NoError(t, err)
	upstreamReq.Header.Set("Authorization", "Bearer upstream-secret")
	upstreamReq.Header.Set("X-Api-Key", "anthropic-secret")
	upstreamReq.Header.Set("X-Amz-Security-Token", "aws-secret")
	upstreamReq.Header.Set("Anthropic-Version", "2023-06-01")
	account := &Account{ID: 42, Name: "claude-account", Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	require.NoError(t, capture.captureRequest(claudeRequestCaptureUpstream, upstreamReq, upstreamBody, account))

	now = now.Add(time.Second)
	require.NoError(t, capture.captureRequest(claudeRequestCaptureDownstream, downstreamReq, downstreamBody, nil))
	require.NoError(t, capture.captureRequest(claudeRequestCaptureUpstream, upstreamReq, upstreamBody, account))
	capture.close()

	wantFiles := []string{
		"claude-downstream-20260803-10.jsonl",
		"claude-downstream-20260803-11.jsonl",
		"claude-upstream-20260803-10.jsonl",
		"claude-upstream-20260803-11.jsonl",
	}
	for _, name := range wantFiles {
		path := filepath.Join(dir, name)
		info, statErr := os.Stat(path)
		require.NoError(t, statErr, name)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}

	downstreamRecord := readSingleClaudeRequestCaptureRecord(t, filepath.Join(dir, wantFiles[0]))
	require.Equal(t, claudeRequestCaptureDownstream, downstreamRecord.Direction)
	require.Equal(t, "req-claude-capture-1", downstreamRecord.RequestID)
	require.Equal(t, "client-claude-capture-1", downstreamRecord.ClientRequestID)
	require.Equal(t, "/v1/messages?trace=1", downstreamRecord.URL)
	require.JSONEq(t, string(downstreamBody), string(downstreamRecord.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, downstreamRecord.Headers["Authorization"])
	require.Equal(t, []string{"[redacted]"}, downstreamRecord.Headers["Cookie"])
	require.Equal(t, []string{"visible"}, downstreamRecord.Headers["X-Trace"])

	upstreamRecord := readSingleClaudeRequestCaptureRecord(t, filepath.Join(dir, wantFiles[2]))
	require.Equal(t, claudeRequestCaptureUpstream, upstreamRecord.Direction)
	require.Equal(t, int64(42), upstreamRecord.AccountID)
	require.Equal(t, "claude-account", upstreamRecord.AccountName)
	require.Equal(t, AccountTypeOAuth, upstreamRecord.AccountType)
	require.Equal(t, PlatformAnthropic, upstreamRecord.Platform)
	require.Equal(t, "https://api.anthropic.com/v1/messages", upstreamRecord.URL)
	require.JSONEq(t, string(upstreamBody), string(upstreamRecord.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, upstreamRecord.Headers["Authorization"])
	require.Equal(t, []string{"[redacted]"}, upstreamRecord.Headers["X-Api-Key"])
	require.Equal(t, []string{"[redacted]"}, upstreamRecord.Headers["X-Amz-Security-Token"])
	require.Equal(t, []string{"2023-06-01"}, upstreamRecord.Headers["Anthropic-Version"])
}

func TestCloneClaudeRequestBodyRestoresBodyWithoutGetBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = io.NopCloser(strings.NewReader("request-body"))
	req.GetBody = nil

	body, err := cloneClaudeRequestBody(req)
	require.NoError(t, err)
	require.Equal(t, "request-body", string(body))

	restored, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "request-body", string(restored))
}

func TestClaudeRequestCaptureRedactsSensitiveURLValues(t *testing.T) {
	req, err := http.NewRequest(
		http.MethodPost,
		"https://relay-user:relay-password@example.com/v1/messages?beta=true&proxy=socks5%3A%2F%2Fproxy-user%3Aproxy-password%40proxy.example%3A1080&X-Amz-Signature=secret-signature",
		nil,
	)
	require.NoError(t, err)

	capturedURL := claudeRequestURLForCapture(req, claudeRequestCaptureUpstream)
	require.Contains(t, capturedURL, "example.com/v1/messages")
	require.Contains(t, capturedURL, "beta=true")
	require.Contains(t, capturedURL, "%5Bredacted%5D")
	require.NotContains(t, capturedURL, "relay-password")
	require.NotContains(t, capturedURL, "proxy-password")
	require.NotContains(t, capturedURL, "secret-signature")
}

func TestDoCapturedClaudeHTTPUpstreamCapturesAnthropicWithoutChangingForwardedBody(t *testing.T) {
	dir := t.TempDir()
	capture, err := newClaudeRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)
	capture.now = func() time.Time {
		return time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	}

	upstream := &claudeRequestCaptureHTTPUpstreamStub{}
	svc := &GatewayService{httpUpstream: upstream, requestCapture: capture}
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"forward me"}]}`)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-claude-forward")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", "upstream-secret")

	resp, err := svc.doCapturedClaudeHTTPUpstream(req, "", &Account{
		ID:          99,
		Name:        "anthropic-account",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, body, upstream.body)

	capture.close()
	record := readSingleClaudeRequestCaptureRecord(t, filepath.Join(dir, "claude-upstream-20260803-12.jsonl"))
	require.Equal(t, "req-claude-forward", record.RequestID)
	require.JSONEq(t, string(body), string(record.Body))
	require.Equal(t, []string{"[redacted]"}, record.Headers["X-Api-Key"])
}

func TestDoCapturedClaudeHTTPUpstreamSkipsNonAnthropicAccounts(t *testing.T) {
	dir := t.TempDir()
	capture, err := newClaudeRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)

	upstream := &claudeRequestCaptureHTTPUpstreamStub{}
	svc := &GatewayService{httpUpstream: upstream, requestCapture: capture}
	body := []byte(`{"model":"gemini-2.5-pro"}`)
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/generate", bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := svc.doCapturedClaudeHTTPUpstream(req, "", &Account{
		ID:          100,
		Name:        "gemini-account",
		Platform:    PlatformGemini,
		Concurrency: 1,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, body, upstream.body)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func readSingleClaudeRequestCaptureRecord(t *testing.T, path string) claudeRequestCaptureRecord {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	scanner := bufio.NewScanner(file)
	require.True(t, scanner.Scan())
	var record claudeRequestCaptureRecord
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
	require.False(t, scanner.Scan(), "one capture call must produce exactly one JSONL record")
	require.NoError(t, scanner.Err())
	return record
}
