package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const upstreamAuditBodyLimit = 256 << 10

type UpstreamAuditMeta struct {
	Provider        string              `json:"provider,omitempty"`
	Route           string              `json:"route,omitempty"`
	AccountID       int64               `json:"account_id,omitempty"`
	AccountType     string              `json:"account_type,omitempty"`
	Profile         HTTPUpstreamProfile `json:"profile,omitempty"`
	Method          string              `json:"method,omitempty"`
	URL             string              `json:"url,omitempty"`
	Attempt         int                 `json:"attempt,omitempty"`
	Transport       string              `json:"transport,omitempty"`
	ProxyConfigured bool                `json:"proxy_configured,omitempty"`
	Status          int                 `json:"status,omitempty"`
	Error           string              `json:"error,omitempty"`
}

type upstreamAuditMetaContextKey struct{}
type upstreamAuditBodyContextKey struct{}

type upstreamAuditLogger struct {
	mu sync.Mutex
	f  *os.File
}

var globalUpstreamAuditLogger atomic.Pointer[upstreamAuditLogger]

func InitUpstreamAuditLog(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if parseDebugEnvBool(path) {
		path = debugGatewayBodyDefaultFilename
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, debugGatewayBodyDefaultFilename)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("failed to create upstream audit log directory", "dir", dir, "error", err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.Error("failed to open upstream audit log file", "path", path, "error", err)
		return
	}
	if old := globalUpstreamAuditLogger.Swap(&upstreamAuditLogger{f: f}); old != nil && old.f != nil {
		_ = old.f.Close()
	}
	slog.Info("upstream audit logging enabled", "path", path)
}

func ResetUpstreamAuditLogForTest() {
	if old := globalUpstreamAuditLogger.Swap(nil); old != nil && old.f != nil {
		_ = old.f.Close()
	}
}

func WithUpstreamAuditMeta(ctx context.Context, meta UpstreamAuditMeta) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamAuditMetaContextKey{}, meta)
}

func UpstreamAuditMetaFromContext(ctx context.Context) UpstreamAuditMeta {
	if ctx == nil {
		return UpstreamAuditMeta{}
	}
	meta, _ := ctx.Value(upstreamAuditMetaContextKey{}).(UpstreamAuditMeta)
	return meta
}

func WithUpstreamAuditBody(ctx context.Context, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	copied := append([]byte(nil), body...)
	return context.WithValue(ctx, upstreamAuditBodyContextKey{}, copied)
}

func UpstreamAuditBodyFromContext(ctx context.Context) ([]byte, bool) {
	if ctx == nil {
		return nil, false
	}
	body, ok := ctx.Value(upstreamAuditBodyContextKey{}).([]byte)
	if !ok {
		return nil, false
	}
	return append([]byte(nil), body...), true
}

func LogUpstreamHTTPRequest(req *http.Request, meta UpstreamAuditMeta, body []byte) {
	if req != nil {
		meta.Method = req.Method
		if req.URL != nil {
			meta.URL = req.URL.String()
		}
	}
	event := newUpstreamAuditEvent("HTTP_UPSTREAM_REQUEST", meta)
	if req != nil {
		event["headers"] = sanitizeAuditHeaders(req.Header)
	}
	addAuditBody(event, body)
	writeUpstreamAuditEvent(event)
}

func LogUpstreamHTTPResponse(resp *http.Response, meta UpstreamAuditMeta) {
	if resp != nil {
		meta.Status = resp.StatusCode
		if meta.Method == "" && resp.Request != nil {
			meta.Method = resp.Request.Method
		}
		if meta.URL == "" && resp.Request != nil && resp.Request.URL != nil {
			meta.URL = resp.Request.URL.String()
		}
	}
	event := newUpstreamAuditEvent("HTTP_UPSTREAM_RESPONSE", meta)
	if resp != nil {
		event["headers"] = sanitizeAuditHeaders(resp.Header)
	}
	writeUpstreamAuditEvent(event)
}

func LogUpstreamHTTPError(err error, meta UpstreamAuditMeta) {
	if err != nil {
		meta.Error = err.Error()
	}
	writeUpstreamAuditEvent(newUpstreamAuditEvent("HTTP_UPSTREAM_ERROR", meta))
}

func LogOpenAIWSFrame(direction string, payload []byte, meta UpstreamAuditMeta) {
	tag := "OPENAI_WS_" + strings.ToUpper(strings.TrimSpace(direction))
	meta.Provider = firstNonEmptyAuditValue(meta.Provider, "openai")
	meta.Transport = firstNonEmptyAuditValue(meta.Transport, "websocket")
	event := newUpstreamAuditEvent(tag, meta)
	event["frame_bytes"] = len(payload)
	addAuditBody(event, payload)
	writeUpstreamAuditEvent(event)
}

func LogUpstreamAuditEvent(tag string, meta UpstreamAuditMeta) {
	writeUpstreamAuditEvent(newUpstreamAuditEvent(tag, meta))
}

func UpstreamAuditBodyForRequest(req *http.Request) []byte {
	if req == nil {
		return nil
	}
	if body, ok := UpstreamAuditBodyFromContext(req.Context()); ok {
		return body
	}
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, upstreamAuditBodyLimit+1))
	if err != nil {
		return nil
	}
	return body
}

func ShouldAuditUpstream(meta UpstreamAuditMeta) bool {
	if meta.Provider == "openai" || meta.Profile == HTTPUpstreamProfileOpenAI || meta.Transport == "websocket" {
		return true
	}
	return false
}

func ensureUpstreamAuditLogger() *upstreamAuditLogger {
	if logger := globalUpstreamAuditLogger.Load(); logger != nil {
		return logger
	}
	if path := strings.TrimSpace(os.Getenv(debugGatewayBodyEnv)); path != "" {
		InitUpstreamAuditLog(path)
	}
	return globalUpstreamAuditLogger.Load()
}

func writeUpstreamAuditEvent(event map[string]any) {
	logger := ensureUpstreamAuditLogger()
	if logger == nil || logger.f == nil {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		slog.Warn("upstream audit event marshal failed", "error", err)
		return
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if _, err := logger.f.Write(append(line, '\n')); err != nil {
		slog.Warn("upstream audit event write failed", "error", err)
	}
}

func newUpstreamAuditEvent(tag string, meta UpstreamAuditMeta) map[string]any {
	event := map[string]any{
		"ts":               time.Now().Format(time.RFC3339Nano),
		"tag":              tag,
		"provider":         meta.Provider,
		"route":            meta.Route,
		"account_id":       meta.AccountID,
		"account_type":     meta.AccountType,
		"profile":          string(meta.Profile),
		"method":           meta.Method,
		"url":              meta.URL,
		"attempt":          meta.Attempt,
		"transport":        firstNonEmptyAuditValue(meta.Transport, "http"),
		"proxy_configured": meta.ProxyConfigured,
	}
	if meta.Status != 0 {
		event["status"] = meta.Status
	}
	if meta.Error != "" {
		event["error"] = meta.Error
	}
	return event
}

func addAuditBody(event map[string]any, body []byte) {
	if len(body) == 0 {
		event["body_truncated"] = false
		return
	}
	truncated := len(body) > upstreamAuditBodyLimit
	if truncated {
		body = body[:upstreamAuditBodyLimit]
	}
	event["body_truncated"] = truncated

	var decoded any
	if json.Unmarshal(body, &decoded) == nil {
		event["body_json"] = decoded
		return
	}
	event["body_raw"] = string(bytes.TrimSpace(body))
}

func sanitizeAuditHeaders(headers http.Header) map[string]any {
	out := make(map[string]any, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			out[key] = ""
			continue
		}
		copied := make([]string, 0, len(values))
		for _, value := range values {
			copied = append(copied, sanitizeAuditHeaderValue(key, value))
		}
		if len(copied) == 1 {
			out[key] = copied[0]
		} else {
			out[key] = copied
		}
	}
	return out
}

func sanitizeAuditHeaderValue(key, value string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization":
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "bearer ") {
			return "Bearer [redacted]"
		}
		return "[redacted]"
	case "cookie", "set-cookie", "x-api-key", "openai-organization", "chatgpt-account-id":
		return "[redacted]"
	default:
		return value
	}
}

func firstNonEmptyAuditValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
