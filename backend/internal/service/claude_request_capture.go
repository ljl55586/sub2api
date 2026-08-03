package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

const claudeRequestCaptureDirEnv = "SUB2API_CLAUDE_REQUEST_CAPTURE_DIR"

type claudeRequestCaptureDirection string

const (
	claudeRequestCaptureDownstream claudeRequestCaptureDirection = "downstream"
	claudeRequestCaptureUpstream   claudeRequestCaptureDirection = "upstream"
)

type claudeRequestCapture struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	writers map[claudeRequestCaptureDirection]*claudeRequestCaptureWriter
}

type claudeRequestCaptureWriter struct {
	hour string
	file *os.File
}

type claudeRequestCaptureRecord struct {
	CapturedAt      string                        `json:"captured_at"`
	Direction       claudeRequestCaptureDirection `json:"direction"`
	RequestID       string                        `json:"request_id,omitempty"`
	ClientRequestID string                        `json:"client_request_id,omitempty"`
	AccountID       int64                         `json:"account_id,omitempty"`
	AccountName     string                        `json:"account_name,omitempty"`
	AccountType     string                        `json:"account_type,omitempty"`
	Platform        string                        `json:"platform,omitempty"`
	Method          string                        `json:"method"`
	URL             string                        `json:"url"`
	Host            string                        `json:"host,omitempty"`
	Headers         map[string][]string           `json:"headers"`
	BodySHA256      string                        `json:"body_sha256"`
	BodyBytes       int                           `json:"body_bytes"`
	Body            json.RawMessage               `json:"body,omitempty"`
	BodyBase64      string                        `json:"body_base64,omitempty"`
}

func newClaudeRequestCaptureFromEnv() *claudeRequestCapture {
	dir := strings.TrimSpace(os.Getenv(claudeRequestCaptureDirEnv))
	if dir == "" {
		return nil
	}
	capture, err := newClaudeRequestCapture(dir)
	if err != nil {
		slog.Error("claude request capture disabled", "dir", dir, "error", err)
		return nil
	}
	slog.Warn("claude request capture enabled; request bodies may contain sensitive data", "dir", dir)
	return capture
}

func newClaudeRequestCapture(dir string) (*claudeRequestCapture, error) {
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
	return &claudeRequestCapture{
		dir:     dir,
		now:     time.Now,
		writers: make(map[claudeRequestCaptureDirection]*claudeRequestCaptureWriter, 2),
	}, nil
}

// CaptureDownstreamRequest records the original Claude-compatible HTTP request
// after content decoding but before model mapping or request-body rewriting.
// Capture failures are deliberately fail-open and never affect the API call.
func (s *GatewayService) CaptureDownstreamRequest(c *gin.Context, body []byte) {
	if s == nil || s.requestCapture == nil || c == nil || c.Request == nil {
		return
	}
	if err := s.requestCapture.captureRequest(claudeRequestCaptureDownstream, c.Request, body, nil); err != nil {
		slog.Warn("capture downstream Claude request failed", "error", err)
	}
}

func (s *GatewayService) doCapturedClaudeHTTPUpstream(
	req *http.Request,
	proxyURL string,
	account *Account,
	tlsProfile *tlsfingerprint.Profile,
) (*http.Response, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, fmt.Errorf("Claude HTTP upstream is unavailable")
	}
	if account == nil {
		return nil, fmt.Errorf("Claude account is required")
	}
	if s.requestCapture != nil && account.Platform == PlatformAnthropic {
		body, err := cloneClaudeRequestBody(req)
		if err != nil {
			slog.Warn("read Claude upstream request for capture failed", "account_id", account.ID, "error", err)
		} else if err := s.requestCapture.captureRequest(claudeRequestCaptureUpstream, req, body, account); err != nil {
			slog.Warn("capture Claude upstream request failed", "account_id", account.ID, "error", err)
		}
	}
	return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
}

func cloneClaudeRequestBody(req *http.Request) ([]byte, error) {
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

func (c *claudeRequestCapture) captureRequest(
	direction claudeRequestCaptureDirection,
	req *http.Request,
	body []byte,
	account *Account,
) error {
	if c == nil || req == nil {
		return nil
	}
	now := c.now()
	record := claudeRequestCaptureRecord{
		CapturedAt: now.Format(time.RFC3339Nano),
		Direction:  direction,
		Method:     req.Method,
		URL:        claudeRequestURLForCapture(req, direction),
		Host:       req.Host,
		Headers:    redactedClaudeRequestHeaders(req.Header),
		BodyBytes:  len(body),
	}
	if req.Context() != nil {
		record.RequestID, _ = req.Context().Value(ctxkey.RequestID).(string)
		record.ClientRequestID, _ = req.Context().Value(ctxkey.ClientRequestID).(string)
	}
	if account != nil {
		record.AccountID = account.ID
		record.AccountName = account.Name
		record.AccountType = account.Type
		record.Platform = account.Platform
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

func claudeRequestURLForCapture(req *http.Request, direction claudeRequestCaptureDirection) string {
	if req == nil || req.URL == nil {
		return ""
	}
	u := *req.URL
	if u.User != nil {
		u.User = url.User("[redacted]")
	}
	query := u.Query()
	for key := range query {
		if isSensitiveClaudeCaptureQueryParam(key) {
			query.Set(key, "[redacted]")
		}
	}
	u.RawQuery = query.Encode()
	if direction == claudeRequestCaptureDownstream {
		return u.RequestURI()
	}
	return u.String()
}

func isSensitiveClaudeCaptureQueryParam(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "proxy" || key == "authorization" {
		return true
	}
	for _, fragment := range []string{"api_key", "apikey", "token", "secret", "password", "credential", "signature"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func redactedClaudeRequestHeaders(headers http.Header) map[string][]string {
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
			redacted[i] = redactClaudeRequestCaptureHeader(key, value)
		}
		result[key] = redacted
	}
	return result
}

func redactClaudeRequestCaptureHeader(key, value string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key",
		"anthropic-api-key", "x-goog-api-key", "x-amz-security-token",
		"cookie", "set-cookie", "x-sub2api-upstream-approval-token":
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "bearer ") {
			return "Bearer [redacted]"
		}
		return "[redacted]"
	default:
		return value
	}
}

func (c *claudeRequestCapture) write(direction claudeRequestCaptureDirection, now time.Time, encoded []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	hour := now.Format("20060102-15")
	writer := c.writers[direction]
	if writer == nil || writer.hour != hour {
		if writer != nil && writer.file != nil {
			_ = writer.file.Close()
		}
		path := filepath.Join(c.dir, fmt.Sprintf("claude-%s-%s.jsonl", direction, hour))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open hourly capture file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("secure hourly capture file: %w", err)
		}
		writer = &claudeRequestCaptureWriter{hour: hour, file: file}
		c.writers[direction] = writer
	}
	line := append(append([]byte(nil), encoded...), '\n')
	if _, err := writer.file.Write(line); err != nil {
		return fmt.Errorf("append capture record: %w", err)
	}
	return nil
}

func (c *claudeRequestCapture) close() {
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
