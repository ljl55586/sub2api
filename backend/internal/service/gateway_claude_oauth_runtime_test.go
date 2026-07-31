package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrepareClaudeOAuthRuntime_RandomSessionStableAcrossTurns(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("conversation_id", "runtime-random-stable")

	firstBody := []byte(`{"messages":[{"role":"user","content":"hello runtime"}]}`)
	first := mustParseClaudeOAuthRuntimeRequest(t, firstBody)
	turn1, hydrated1, err := svc.prepareClaudeOAuthRuntime(context.Background(), c, first, 77, "device-a", firstBody)
	require.NoError(t, err)
	require.NotNil(t, turn1)
	require.True(t, turn1.Claimed)
	require.NoError(t, uuid.Validate(turn1.SessionID))
	require.Equal(t, firstBody, hydrated1)
	require.NotEqual(t, generateSessionUUID(buildStableSessionSeed(77, sessionContextDiscriminator(first.SessionContext), "hello runtime")), turn1.SessionID)

	require.NoError(t, svc.commitClaudeOAuthRuntimeTurn(
		context.Background(),
		77,
		turn1,
		json.RawMessage(`[{"type":"text","text":"first answer"}]`),
	))

	secondBody := []byte(`{"messages":[{"role":"user","content":"next question"}]}`)
	second := mustParseClaudeOAuthRuntimeRequest(t, secondBody)
	turn2, hydrated2, err := svc.prepareClaudeOAuthRuntime(context.Background(), c, second, 77, "device-a", secondBody)
	require.NoError(t, err)
	require.NotNil(t, turn2)
	require.True(t, turn2.Claimed)
	require.Equal(t, turn1.SessionID, turn2.SessionID)
	require.Len(t, gjson.GetBytes(hydrated2, "messages").Array(), 3)
	require.Equal(t, "first answer", gjson.GetBytes(hydrated2, "messages.1.content.0.text").String())
	require.Equal(t, "next question", gjson.GetBytes(hydrated2, "messages.2.content").String())
	svc.releaseClaudeOAuthRuntimeTurn(context.Background(), 77, turn2)
}

func TestResolveClaudeOAuthRuntimeKey_ExplicitSessionSurvivesClientNetworkChanges(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	first := mustParseClaudeOAuthRuntimeRequest(t, body)
	second := mustParseClaudeOAuthRuntimeRequest(t, body)
	second.SessionContext.ClientIP = "203.0.113.8"
	second.SessionContext.UserAgent = "another-client/9.0"
	view := &ginContextView{header: func(name string) string {
		if name == "conversation_id" {
			return "conversation-stable"
		}
		return ""
	}}

	require.Equal(
		t,
		resolveClaudeOAuthRuntimeKey(view, first, body),
		resolveClaudeOAuthRuntimeKey(view, second, body),
	)
}

func TestPrepareClaudeOAuthRuntime_DoesNotDuplicateFullClientHistory(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	firstBody := []byte(`{"messages":[{"role":"user","content":"hello runtime"}]}`)
	first := mustParseClaudeOAuthRuntimeRequest(t, firstBody)
	turn1, _, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, first, 78, "device-a", firstBody)
	require.NoError(t, err)
	require.NoError(t, svc.commitClaudeOAuthRuntimeTurn(
		context.Background(),
		78,
		turn1,
		json.RawMessage(`[{"type":"text","text":"first answer"}]`),
	))

	fullBody := []byte(`{"messages":[{"role":"user","content":"hello runtime"},{"role":"assistant","content":[{"type":"text","text":"first answer"}]},{"role":"user","content":"next question"}]}`)
	full := mustParseClaudeOAuthRuntimeRequest(t, fullBody)
	turn2, hydrated, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, full, 78, "device-a", fullBody)
	require.NoError(t, err)
	require.Equal(t, fullBody, hydrated)
	require.Len(t, gjson.GetBytes(hydrated, "messages").Array(), 3)
	svc.releaseClaudeOAuthRuntimeTurn(context.Background(), 78, turn2)
}

func TestPrepareClaudeOAuthRuntime_DoesNotDuplicateFullClientHistoryAfterCacheMarkerCleanup(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	firstBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello runtime","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	first := mustParseClaudeOAuthRuntimeRequest(t, firstBody)
	turn1, _, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, first, 178, "device-a", firstBody)
	require.NoError(t, err)
	require.NoError(t, svc.commitClaudeOAuthRuntimeTurn(
		context.Background(),
		178,
		turn1,
		json.RawMessage(`[{"type":"thinking","thinking":"","signature":"signed"},{"type":"text","text":"first answer"}]`),
	))

	fullBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello runtime"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"signed"},{"type":"text","text":"first answer"}]},{"role":"user","content":[{"type":"text","text":"next question","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	full := mustParseClaudeOAuthRuntimeRequest(t, fullBody)
	turn2, hydrated, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, full, 178, "device-a", fullBody)
	require.NoError(t, err)
	require.Equal(t, fullBody, hydrated)
	require.Len(t, gjson.GetBytes(hydrated, "messages").Array(), 3)
	require.Equal(t, "hello runtime", gjson.GetBytes(hydrated, "messages.0.content.0.text").String())
	require.Equal(t, "next question", gjson.GetBytes(hydrated, "messages.2.content.0.text").String())
	svc.releaseClaudeOAuthRuntimeTurn(context.Background(), 178, turn2)
}

func TestClaudeOAuthIncomingContainsRuntimeHistory_DifferentAnchorStillRequiresHydration(t *testing.T) {
	incoming := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"different question"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"first answer"}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"next question"}]}`),
	}
	stored := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hello runtime","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"first answer"}]}`),
	}

	require.False(t, claudeOAuthIncomingContainsRuntimeHistory(incoming, stored))
}

func TestPrepareClaudeOAuthRuntime_ConcurrentTurnIsRejected(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	body := []byte(`{"messages":[{"role":"user","content":"same conversation"}]}`)
	first := mustParseClaudeOAuthRuntimeRequest(t, body)
	second := mustParseClaudeOAuthRuntimeRequest(t, body)

	turn1, _, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, first, 79, "device-a", body)
	require.NoError(t, err)
	require.True(t, turn1.Claimed)

	turn2, hydrated, err := svc.prepareClaudeOAuthRuntime(context.Background(), nil, second, 79, "device-a", body)
	require.ErrorIs(t, err, errClaudeOAuthRuntimeTurnBusy)
	require.NotNil(t, turn2)
	require.False(t, turn2.Claimed)
	require.Equal(t, turn1.SessionID, turn2.SessionID)
	require.Equal(t, body, hydrated)

	require.NoError(t, svc.commitClaudeOAuthRuntimeTurn(
		context.Background(),
		79,
		turn1,
		json.RawMessage(`[{"type":"text","text":"winner"}]`),
	))
}

func TestClaudeOAuthRuntimeActions_LeaseCompletionAndRetry(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	runtime := newClaudeOAuthSessionRuntime("device-a")
	_, _, err := store.GetOrCreateClaudeOAuthRuntime(context.Background(), 80, "runtime-a", runtime, time.Hour)
	require.NoError(t, err)

	quotaClaimID, claimed, err := svc.claimClaudeOAuthRuntimeAction(context.Background(), 80, "runtime-a", claudeOAuthSessionActionQuota)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotEmpty(t, quotaClaimID)
	_, claimed, err = svc.claimClaudeOAuthRuntimeAction(context.Background(), 80, "runtime-a", claudeOAuthSessionActionQuota)
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, svc.completeClaudeOAuthRuntimeAction(
		context.Background(), 80, "runtime-a", claudeOAuthSessionActionQuota, quotaClaimID,
		true, http.StatusTooManyRequests, "", "", map[string]string{"retry-after": "60"},
	))
	_, claimed, err = svc.claimClaudeOAuthRuntimeAction(context.Background(), 80, "runtime-a", claudeOAuthSessionActionQuota)
	require.NoError(t, err)
	require.False(t, claimed)

	firstTitleClaimID, claimed, err := svc.claimClaudeOAuthRuntimeAction(context.Background(), 80, "runtime-a", claudeOAuthSessionActionTitle)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, svc.completeClaudeOAuthRuntimeAction(
		context.Background(), 80, "runtime-a", claudeOAuthSessionActionTitle, firstTitleClaimID,
		false, http.StatusBadGateway, "", "transport failed", nil,
	))
	secondTitleClaimID, claimed, err := svc.claimClaudeOAuthRuntimeAction(context.Background(), 80, "runtime-a", claudeOAuthSessionActionTitle)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotEqual(t, firstTitleClaimID, secondTitleClaimID)
	require.NoError(t, svc.completeClaudeOAuthRuntimeAction(
		context.Background(), 80, "runtime-a", claudeOAuthSessionActionTitle, firstTitleClaimID,
		true, http.StatusOK, "Stale title", "", nil,
	))
	require.NoError(t, svc.completeClaudeOAuthRuntimeAction(
		context.Background(), 80, "runtime-a", claudeOAuthSessionActionTitle, secondTitleClaimID,
		true, http.StatusOK, "Runtime title", "", nil,
	))

	got, err := store.GetClaudeOAuthRuntime(context.Background(), 80, "runtime-a", time.Hour)
	require.NoError(t, err)
	require.Equal(t, claudeOAuthRuntimeActionCompleted, got.Quota.State)
	require.Equal(t, http.StatusTooManyRequests, got.Quota.StatusCode)
	require.Equal(t, "60", got.Quota.ResponseHeader["retry-after"])
	require.Equal(t, claudeOAuthRuntimeActionGenerated, got.Title.State)
	require.Equal(t, "Runtime title", got.Title.Value)
	require.Equal(t, 2, got.Title.Attempts)
}

func TestClaudeOAuthStreamContentCollector_PreservesThinkingSignatureAndToolInput(t *testing.T) {
	collector := newClaudeOAuthStreamContentCollector()
	collector.Observe(map[string]any{
		"type":  "content_block_start",
		"index": float64(0),
		"content_block": map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		},
	})
	collector.Observe(map[string]any{
		"type":  "content_block_delta",
		"index": float64(0),
		"delta": map[string]any{"type": "thinking_delta", "thinking": "reason"},
	})
	collector.Observe(map[string]any{
		"type":  "content_block_delta",
		"index": float64(0),
		"delta": map[string]any{"type": "signature_delta", "signature": "signed"},
	})
	collector.Observe(map[string]any{
		"type":  "content_block_start",
		"index": float64(1),
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    "tool-1",
			"name":  "Read",
			"input": map[string]any{},
		},
	})
	collector.Observe(map[string]any{
		"type":  "content_block_delta",
		"index": float64(1),
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"file_path":"a.go"}`},
	})
	collector.Observe(map[string]any{"type": "content_block_stop", "index": float64(1)})

	content := collector.Content()
	require.Equal(t, "reason", gjson.GetBytes(content, "0.thinking").String())
	require.Equal(t, "signed", gjson.GetBytes(content, "0.signature").String())
	require.Equal(t, "Read", gjson.GetBytes(content, "1.name").String())
	require.Equal(t, "a.go", gjson.GetBytes(content, "1.input.file_path").String())
}

func TestFinalizeClaudeOAuthRuntimeMessagesStripsWireOnlyReminderAndCache(t *testing.T) {
	reminder := buildClaudeCode208SystemReminder(claudeCode208SessionFacts{CurrentDate: "2026-07-29"})
	body, err := json.Marshal(map[string]any{
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": reminder},
				{
					"type":          "text",
					"text":          "human question",
					"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
				},
			},
		}},
	})
	require.NoError(t, err)
	turn := &claudeOAuthRuntimeTurn{Claimed: true}

	require.NoError(t, finalizeClaudeOAuthRuntimeRequestMessages(turn, body))
	require.NotContains(t, string(turn.RequestMessages), "system-reminder")
	require.NotContains(t, string(turn.RequestMessages), "cache_control")
	require.Equal(t, "human question", gjson.GetBytes(turn.RequestMessages, "0.content.0.text").String())

	nextBody := []byte(`{"messages":[{"role":"user","content":"human question"},{"role":"user","content":"next question"}]}`)
	hydrated, _, err := hydrateClaudeOAuthRuntimeTranscript(nextBody, turn.RequestMessages)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(hydrated, "messages").Array(), 2)
	require.Equal(t, "next question", gjson.GetBytes(hydrated, "messages.1.content").String())
}

func mustParseClaudeOAuthRuntimeRequest(t *testing.T, body []byte) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	parsed.SessionContext = &SessionContext{
		ClientIP:  "127.0.0.1",
		UserAgent: "runtime-test/1.0",
		APIKeyID:  991,
	}
	return parsed
}
