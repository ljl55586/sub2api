package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

const openAIRequestCaptureDirEnv = "SUB2API_OPENAI_REQUEST_CAPTURE_DIR"

type openAIRequestCaptureDirection string

const (
	openAIRequestCaptureDownstream openAIRequestCaptureDirection = "downstream"
	openAIRequestCaptureUpstream   openAIRequestCaptureDirection = "upstream"
)

type openAIRequestCapture struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	writers map[openAIRequestCaptureDirection]*openAIRequestCaptureWriter
}

type openAIRequestCaptureWriter struct {
	hour string
	file *os.File
}

type openAIRequestCaptureRecord struct {
	CapturedAt      string                        `json:"captured_at"`
	Direction       openAIRequestCaptureDirection `json:"direction"`
	RequestID       string                        `json:"request_id,omitempty"`
	ClientRequestID string                        `json:"client_request_id,omitempty"`
	AccountID       int64                         `json:"account_id,omitempty"`
	AccountName     string                        `json:"account_name,omitempty"`
	Method          string                        `json:"method"`
	URL             string                        `json:"url"`
	Host            string                        `json:"host,omitempty"`
	Headers         map[string][]string           `json:"headers"`
	BodySHA256      string                        `json:"body_sha256"`
	BodyBytes       int                           `json:"body_bytes"`
	Body            json.RawMessage               `json:"body,omitempty"`
	BodyBase64      string                        `json:"body_base64,omitempty"`
}

func newOpenAIRequestCaptureFromEnv() *openAIRequestCapture {
	dir := strings.TrimSpace(os.Getenv(openAIRequestCaptureDirEnv))
	if dir == "" {
		return nil
	}
	capture, err := newOpenAIRequestCapture(dir)
	if err != nil {
		slog.Error("openai request capture disabled", "dir", dir, "error", err)
		return nil
	}
	slog.Warn("openai request capture enabled; request bodies may contain sensitive data", "dir", dir)
	return capture
}

func newOpenAIRequestCapture(dir string) (*openAIRequestCapture, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("capture directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create capture directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure capture directory: %w", err)
	}
	return &openAIRequestCapture{
		dir:     dir,
		now:     time.Now,
		writers: make(map[openAIRequestCaptureDirection]*openAIRequestCaptureWriter, 2),
	}, nil
}

// CaptureDownstreamRequest records the original OpenAI-compatible HTTP request
// after content decoding but before model mapping or request-body rewriting.
// Capture failures are deliberately fail-open and never affect the API call.
func (s *OpenAIGatewayService) CaptureDownstreamRequest(c *gin.Context, body []byte) {
	if s == nil || s.requestCapture == nil || c == nil || c.Request == nil {
		return
	}
	if err := s.requestCapture.captureRequest(openAIRequestCaptureDownstream, c.Request, body, nil); err != nil {
		slog.Warn("capture downstream OpenAI request failed", "error", err)
	}
}

func (s *OpenAIGatewayService) captureDownstreamWebSocketFrame(c *gin.Context, body []byte) {
	if s == nil || s.requestCapture == nil || c == nil || c.Request == nil {
		return
	}
	req := c.Request.Clone(c.Request.Context())
	req.Method = "WEBSOCKET"
	if err := s.requestCapture.captureRequest(openAIRequestCaptureDownstream, req, body, nil); err != nil {
		slog.Warn("capture downstream OpenAI websocket frame failed", "error", err)
	}
}

func (s *OpenAIGatewayService) doCapturedHTTPUpstream(
	req *http.Request,
	proxyURL string,
	account *Account,
) (*http.Response, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, fmt.Errorf("OpenAI HTTP upstream is unavailable")
	}
	if account == nil {
		return nil, fmt.Errorf("OpenAI account is required")
	}
	if s.requestCapture != nil && account.Platform == PlatformOpenAI {
		body, err := cloneRequestBody(req)
		if err != nil {
			slog.Warn("read OpenAI upstream request for capture failed", "account_id", account.ID, "error", err)
		} else if err := s.requestCapture.captureRequest(openAIRequestCaptureUpstream, req, body, account); err != nil {
			slog.Warn("capture OpenAI upstream request failed", "account_id", account.ID, "error", err)
		}
	}
	return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
}

func (s *OpenAIGatewayService) captureOpenAIUpstreamWebSocket(
	ctx context.Context,
	targetURL string,
	headers http.Header,
	body []byte,
	account *Account,
) {
	if s == nil || s.requestCapture == nil || account == nil || account.Platform != PlatformOpenAI || strings.TrimSpace(targetURL) == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "WEBSOCKET", targetURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("build OpenAI websocket request capture failed", "account_id", account.ID, "error", err)
		return
	}
	req.Header = headers.Clone()
	if err := s.requestCapture.captureRequest(openAIRequestCaptureUpstream, req, body, account); err != nil {
		slog.Warn("capture OpenAI websocket upstream request failed", "account_id", account.ID, "error", err)
	}
}

func cloneRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	if req.GetBody != nil {
		reader, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(reader)
		_ = reader.Close()
		return body, readErr
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, nil
}

func (c *openAIRequestCapture) captureRequest(
	direction openAIRequestCaptureDirection,
	req *http.Request,
	body []byte,
	account *Account,
) error {
	if c == nil || req == nil {
		return nil
	}
	now := c.now()
	record := openAIRequestCaptureRecord{
		CapturedAt: now.Format(time.RFC3339Nano),
		Direction:  direction,
		Method:     req.Method,
		URL:        requestURLForCapture(req, direction),
		Host:       req.Host,
		Headers:    redactedRequestHeaders(req.Header),
		BodyBytes:  len(body),
	}
	if req.Context() != nil {
		record.RequestID, _ = req.Context().Value(ctxkey.RequestID).(string)
		record.ClientRequestID, _ = req.Context().Value(ctxkey.ClientRequestID).(string)
	}
	if account != nil {
		record.AccountID = account.ID
		record.AccountName = account.Name
	}
	sum := sha256.Sum256(body)
	record.BodySHA256 = hex.EncodeToString(sum[:])
	if len(body) > 0 {
		if json.Valid(body) {
			record.Body = append(json.RawMessage(nil), body...)
		} else {
			record.BodyBase64 = base64.StdEncoding.EncodeToString(body)
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal capture record: %w", err)
	}
	return c.write(direction, now, encoded)
}

func requestURLForCapture(req *http.Request, direction openAIRequestCaptureDirection) string {
	if req == nil || req.URL == nil {
		return ""
	}
	if direction == openAIRequestCaptureDownstream {
		return req.URL.RequestURI()
	}
	return req.URL.String()
}

func redactedRequestHeaders(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return map[string][]string{}
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string][]string, len(headers))
	for _, key := range keys {
		values := headers[key]
		redacted := make([]string, len(values))
		for i, value := range values {
			redacted[i] = redactOpenAIRequestCaptureHeader(key, value)
		}
		result[key] = redacted
	}
	return result
}

func redactOpenAIRequestCaptureHeader(key, value string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key",
		"cookie", "set-cookie", "x-sub2api-upstream-approval-token":
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "bearer ") {
			return "Bearer [redacted]"
		}
		return "[redacted]"
	default:
		return value
	}
}

func (c *openAIRequestCapture) write(direction openAIRequestCaptureDirection, now time.Time, encoded []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	hour := now.Format("20060102-15")
	writer := c.writers[direction]
	if writer == nil || writer.hour != hour {
		if writer != nil && writer.file != nil {
			_ = writer.file.Close()
		}
		path := filepath.Join(c.dir, fmt.Sprintf("openai-%s-%s.jsonl", direction, hour))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open hourly capture file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("secure hourly capture file: %w", err)
		}
		writer = &openAIRequestCaptureWriter{hour: hour, file: file}
		c.writers[direction] = writer
	}
	line := append(append([]byte(nil), encoded...), '\n')
	if _, err := writer.file.Write(line); err != nil {
		return fmt.Errorf("append capture record: %w", err)
	}
	return nil
}

func (c *openAIRequestCapture) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for direction, writer := range c.writers {
		if writer != nil && writer.file != nil {
			_ = writer.file.Close()
		}
		delete(c.writers, direction)
	}
}
