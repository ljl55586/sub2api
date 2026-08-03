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

type requestCaptureHTTPUpstreamStub struct {
	body []byte
}

func (s *requestCaptureHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
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

func (s *requestCaptureHTTPUpstreamStub) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestOpenAIRequestCaptureSeparatesDirectionsAndRotatesHourly(t *testing.T) {
	dir := t.TempDir()
	capture, err := newOpenAIRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)

	now := time.Date(2026, 7, 31, 10, 59, 59, 0, time.UTC)
	capture.now = func() time.Time { return now }
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-capture-1")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-capture-1")

	downstreamBody := []byte("{\n  \"model\": \"gpt-5.6\",\n  \"input\": \"hello\"\n}")
	downstreamReq := httptest.NewRequest(http.MethodPost, "/v1/responses?trace=1", bytes.NewReader(downstreamBody)).WithContext(ctx)
	downstreamReq.Header.Set("Authorization", "Bearer sk-downstream-secret")
	downstreamReq.Header.Set("Cookie", "session=downstream-secret")
	downstreamReq.Header.Set("X-Trace", "visible")
	require.NoError(t, capture.captureRequest(openAIRequestCaptureDownstream, downstreamReq, downstreamBody, nil))

	upstreamBody := []byte(`{"model":"gpt-5.6-sol","stream":true}`)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(upstreamBody))
	require.NoError(t, err)
	upstreamReq.Header.Set("Authorization", "Bearer upstream-secret")
	upstreamReq.Header.Set("ChatGPT-Account-ID", "account-visible")
	account := &Account{ID: 42, Name: "capture-account", Platform: PlatformOpenAI}
	require.NoError(t, capture.captureRequest(openAIRequestCaptureUpstream, upstreamReq, upstreamBody, account))

	now = now.Add(time.Second)
	require.NoError(t, capture.captureRequest(openAIRequestCaptureDownstream, downstreamReq, downstreamBody, nil))
	require.NoError(t, capture.captureRequest(openAIRequestCaptureUpstream, upstreamReq, upstreamBody, account))
	capture.close()

	wantFiles := []string{
		"openai-downstream-20260731-10.jsonl",
		"openai-downstream-20260731-11.jsonl",
		"openai-upstream-20260731-10.jsonl",
		"openai-upstream-20260731-11.jsonl",
	}
	for _, name := range wantFiles {
		path := filepath.Join(dir, name)
		info, statErr := os.Stat(path)
		require.NoError(t, statErr, name)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}

	downstreamRecord := readSingleOpenAIRequestCaptureRecord(t, filepath.Join(dir, wantFiles[0]))
	require.Equal(t, openAIRequestCaptureDownstream, downstreamRecord.Direction)
	require.Equal(t, "req-capture-1", downstreamRecord.RequestID)
	require.Equal(t, "client-capture-1", downstreamRecord.ClientRequestID)
	require.Equal(t, "/v1/responses?trace=1", downstreamRecord.URL)
	require.JSONEq(t, string(downstreamBody), string(downstreamRecord.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, downstreamRecord.Headers["Authorization"])
	require.Equal(t, []string{"[redacted]"}, downstreamRecord.Headers["Cookie"])
	require.Equal(t, []string{"visible"}, downstreamRecord.Headers["X-Trace"])

	upstreamRecord := readSingleOpenAIRequestCaptureRecord(t, filepath.Join(dir, wantFiles[2]))
	require.Equal(t, openAIRequestCaptureUpstream, upstreamRecord.Direction)
	require.Equal(t, int64(42), upstreamRecord.AccountID)
	require.Equal(t, "capture-account", upstreamRecord.AccountName)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", upstreamRecord.URL)
	require.JSONEq(t, string(upstreamBody), string(upstreamRecord.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, upstreamRecord.Headers["Authorization"])
}

func TestCloneRequestBodyRestoresBodyWithoutGetBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = io.NopCloser(strings.NewReader("request-body"))
	req.GetBody = nil

	body, err := cloneRequestBody(req)
	require.NoError(t, err)
	require.Equal(t, "request-body", string(body))

	restored, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "request-body", string(restored))
}

func TestDoCapturedHTTPUpstreamCapturesOpenAIWithoutChangingForwardedBody(t *testing.T) {
	dir := t.TempDir()
	capture, err := newOpenAIRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)
	capture.now = func() time.Time {
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	}

	upstream := &requestCaptureHTTPUpstreamStub{}
	svc := &OpenAIGatewayService{httpUpstream: upstream, requestCapture: capture}
	body := []byte(`{"model":"gpt-5.6-sol","input":"forward me"}`)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-forward")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer upstream-secret")

	resp, err := svc.doCapturedHTTPUpstream(req, "", &Account{
		ID:          99,
		Name:        "openai-account",
		Platform:    PlatformOpenAI,
		Concurrency: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, body, upstream.body)

	capture.close()
	record := readSingleOpenAIRequestCaptureRecord(t, filepath.Join(dir, "openai-upstream-20260731-12.jsonl"))
	require.Equal(t, "req-forward", record.RequestID)
	require.JSONEq(t, string(body), string(record.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, record.Headers["Authorization"])
}

func TestCaptureOpenAIUpstreamWebSocketWritesFinalFrame(t *testing.T) {
	dir := t.TempDir()
	capture, err := newOpenAIRequestCapture(dir)
	require.NoError(t, err)
	t.Cleanup(capture.close)
	capture.now = func() time.Time {
		return time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
	}

	svc := &OpenAIGatewayService{requestCapture: capture}
	body := []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)
	headers := http.Header{
		"Authorization": {"Bearer websocket-secret"},
		"OpenAI-Beta":   {"responses=experimental"},
	}
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-websocket")
	svc.captureOpenAIUpstreamWebSocket(
		ctx,
		"wss://chatgpt.com/backend-api/codex/responses",
		headers,
		body,
		&Account{ID: 100, Name: "ws-account", Platform: PlatformOpenAI},
	)
	capture.close()

	record := readSingleOpenAIRequestCaptureRecord(t, filepath.Join(dir, "openai-upstream-20260731-13.jsonl"))
	require.Equal(t, "WEBSOCKET", record.Method)
	require.Equal(t, "wss://chatgpt.com/backend-api/codex/responses", record.URL)
	require.Equal(t, "req-websocket", record.RequestID)
	require.JSONEq(t, string(body), string(record.Body))
	require.Equal(t, []string{"Bearer [redacted]"}, record.Headers["Authorization"])
}

func readSingleOpenAIRequestCaptureRecord(t *testing.T, path string) openAIRequestCaptureRecord {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	scanner := bufio.NewScanner(file)
	require.True(t, scanner.Scan())
	var record openAIRequestCaptureRecord
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
	require.False(t, scanner.Scan(), "one capture call must produce exactly one JSONL record")
	require.NoError(t, scanner.Err())
	return record
}
