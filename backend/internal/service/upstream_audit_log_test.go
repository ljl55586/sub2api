package service

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamAuditLogWritesJSONLAndRedactsSecrets(t *testing.T) {
	debugPath := t.TempDir() + "/gateway_debug.log"
	InitUpstreamAuditLog(debugPath)
	t.Cleanup(ResetUpstreamAuditLogForTest)

	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=secret-cookie")
	req.Header.Set("Content-Type", "application/json")

	LogUpstreamHTTPRequest(req, UpstreamAuditMeta{
		Provider:    "openai",
		Route:       "/v1/responses",
		AccountID:   11,
		AccountType: "oauth",
		Profile:     HTTPUpstreamProfileOpenAI,
		Transport:   "http",
	}, body)

	events := readAuditEvents(t, debugPath)
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, "HTTP_UPSTREAM_REQUEST", event["tag"])
	require.Equal(t, "openai", event["provider"])
	require.Equal(t, "/v1/responses", event["route"])
	require.Equal(t, "POST", event["method"])
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", event["url"])
	require.Equal(t, false, event["body_truncated"])

	headers := event["headers"].(map[string]any)
	require.Equal(t, "Bearer [redacted]", headers["Authorization"])
	require.Equal(t, "[redacted]", headers["Cookie"])
	require.NotContains(t, string(mustReadFile(t, debugPath)), "secret-token")
	require.NotContains(t, string(mustReadFile(t, debugPath)), "secret-cookie")

	bodyJSON := event["body_json"].(map[string]any)
	require.Equal(t, "gpt-5.5", bodyJSON["model"])
	require.Equal(t, "hello", bodyJSON["input"])
}

func TestUpstreamAuditLogTruncatesLargeBody(t *testing.T) {
	debugPath := t.TempDir() + "/gateway_debug.log"
	InitUpstreamAuditLog(debugPath)
	t.Cleanup(ResetUpstreamAuditLogForTest)

	body := []byte(`{"input":"` + strings.Repeat("x", upstreamAuditBodyLimit+1) + `"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(string(body)))
	require.NoError(t, err)

	LogUpstreamHTTPRequest(req, UpstreamAuditMeta{
		Provider:  "openai",
		Profile:   HTTPUpstreamProfileOpenAI,
		Transport: "http",
	}, body)

	events := readAuditEvents(t, debugPath)
	require.Len(t, events, 1)
	require.Equal(t, true, events[0]["body_truncated"])
	require.NotNil(t, events[0]["body_raw"])
}

func readAuditEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var events []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), upstreamAuditBodyLimit+64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &event), line)
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	return events
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
