package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCurlCodexAgentProfileBuildsCoherentUpstreamRequest(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, true)
	body := []byte(`{
		"model":"gpt-5.6-terra",
		"instructions":"You are a concise assistant.",
		"input":"Hello from curl.",
		"stream":false,
		"reasoning":{"effort":"medium","context":"current_turn","summary":"auto"},
		"prompt_cache_key":"00000000-0000-4000-8000-000000000001",
		"text":{"verbosity":"low"}
	}`)
	c, recorder := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent,
		"tool-loop, encrypted-reasoning, code-mode-exec, async-wait, interactive-input, multi-agent, remote-compaction-v2")
	// These ordinary passthrough headers deliberately conflict with the full
	// profile session. The final builder must replace them from one state.
	c.Request.Header.Set("Session-Id", "contradictory-session")
	c.Request.Header.Set("Conversation-Id", "contradictory-conversation")
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"wrong"}`)
	c.Request.Header.Set("X-Codex-Window-Id", "wrong:0")
	c.Request.Header.Set("X-Codex-Installation-Id", "00000000-0000-4000-8000-000000000099")
	c.Request.Header.Set("OpenAI-Beta", "contradictory-beta")
	c.Request.Header.Set("Accept-Language", "xx-test")
	c.Request.Header.Set(responsesLiteHeader, "true")

	mode, enabled, err := service.resolveCurlCodexProfile(c)
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, curlCodexProfileAgent, mode)

	account := curlCodexProfileTestAccount()
	profiled, err := service.applyCurlCodexProfile(c, account, body, mode)
	require.NoError(t, err)
	require.Contains(t, string(profiled), `"reasoning":{"effort":"medium","context":"all_turns","summary":"auto"}`)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.Equal(t, "gpt-5.6-terra", decoded["model"])
	require.NotContains(t, decoded, "instructions")
	require.Equal(t, map[string]any{"context": "all_turns", "effort": "medium", "summary": "auto"}, decoded["reasoning"])
	require.Equal(t, false, decoded["store"])
	require.Equal(t, true, decoded["stream"])
	require.Equal(t, "auto", decoded["tool_choice"])
	require.Equal(t, false, decoded["parallel_tool_calls"])
	require.NotContains(t, decoded, "tools")
	require.Equal(t, []any{"reasoning.encrypted_content"}, decoded["include"])

	input := decoded["input"].([]any)
	require.Len(t, input, 4)
	additional := input[0].(map[string]any)
	require.Equal(t, "additional_tools", additional["type"])
	require.Equal(t, "developer", additional["role"])
	additionalTools := additional["tools"].([]any)
	require.Len(t, additionalTools, 4)
	toolNames := make([]string, 0, len(additionalTools))
	for _, rawTool := range additionalTools {
		toolNames = append(toolNames, rawTool.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"exec", "wait", "request_user_input", "collaboration"}, toolNames)
	baseDeveloper := input[1].(map[string]any)
	require.Equal(t, openai.CodexCLI0145BaseInstructionsForModel("gpt-5.6-terra"), baseDeveloper["content"].([]any)[0].(map[string]any)["text"])
	developer := input[2].(map[string]any)
	require.Equal(t, "developer", developer["role"])
	developerContent := developer["content"].([]any)[0].(map[string]any)["text"].(string)
	require.Contains(t, developerContent, "<caller_instructions>")
	require.Contains(t, developerContent, "You are a concise assistant.")
	user := input[3].(map[string]any)
	require.Equal(t, []any{map[string]any{"type": "input_text", "text": "Hello from curl."}}, user["content"])

	metadata := decoded["client_metadata"].(map[string]any)
	require.Equal(t, "00000000-0000-4000-8000-000000000001", decoded["prompt_cache_key"])
	require.Equal(t, decoded["prompt_cache_key"], metadata["session_id"])
	require.Equal(t, metadata["session_id"], metadata["thread_id"])
	require.Equal(t, "48ca0404-a22d-4f52-8407-b9f6fc68bd04", metadata["x-codex-installation-id"])

	var turnMetadata curlCodexTurnMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata["x-codex-turn-metadata"].(string)), &turnMetadata))
	require.Equal(t, metadata["session_id"], turnMetadata.SessionID)
	require.Equal(t, metadata["thread_id"], turnMetadata.ThreadID)
	require.Equal(t, metadata["turn_id"], turnMetadata.TurnID)
	require.Equal(t, metadata["x-codex-window-id"], turnMetadata.WindowID)
	require.Equal(t, "none", turnMetadata.Sandbox)
	require.Positive(t, turnMetadata.TurnStartedAtMS)

	upstreamReq, err := service.buildUpstreamRequestOpenAIPassthrough(
		context.Background(), c, account, profiled, "oauth-token",
	)
	require.NoError(t, err)
	require.Equal(t, "text/event-stream", upstreamReq.Header.Get("Accept"))
	require.Equal(t, curlCodexProfileDefaultUserAgent, upstreamReq.Header.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", upstreamReq.Header.Get("Originator"))
	require.NotContains(t, upstreamReq.Header.Get("OpenAI-Beta"), "responses=experimental")
	require.Equal(t, "remote_compaction_v2", upstreamReq.Header.Get("X-Codex-Beta-Features"))
	require.Equal(t, metadata["x-codex-turn-metadata"], upstreamReq.Header.Get("X-Codex-Turn-Metadata"))
	require.Equal(t, metadata["x-codex-window-id"], upstreamReq.Header.Get("X-Codex-Window-Id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Installation-Id"))
	require.Empty(t, upstreamReq.Header.Get("Accept-Language"))
	require.Equal(t, "true", upstreamReq.Header.Get(responsesLiteHeader))
	isolated := isolateOpenAISessionID(4242, metadata["session_id"].(string))
	require.Equal(t, isolated, upstreamReq.Header.Get("Session_id"))
	require.Equal(t, isolated, upstreamReq.Header.Get("Conversation_id"))
	require.Equal(t, metadata["session_id"], recorder.Header().Get(curlCodexResponseSessionHeader))
	require.Equal(t, metadata["thread_id"], recorder.Header().Get(curlCodexResponseThreadHeader))
	require.Equal(t, metadata["turn_id"], recorder.Header().Get(curlCodexResponseTurnHeader))
}

func TestForwardCurlCodexAgentProfileUsesProductionPassthroughPath(t *testing.T) {
	body := []byte(`{"input":"hello without caller instructions"}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent,
		"tool-loop, encrypted-reasoning, code-mode-exec, async-wait, interactive-input, multi-agent")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
	}}
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, false)
	service.httpUpstream = upstream
	account := curlCodexProfileTestAccount()
	account.Credentials["access_token"] = "oauth-token"
	account.Name = "curl-profile-test"
	account.Concurrency = 1
	account.Status = StatusActive
	account.Schedulable = true

	result, err := service.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.NotNil(t, upstream.lastReq)

	var upstreamBody map[string]any
	require.NoError(t, json.Unmarshal(upstream.lastBody, &upstreamBody))
	require.Equal(t, "gpt-5.6-sol", upstreamBody["model"])
	require.NotContains(t, upstreamBody, "instructions")
	require.Equal(t, map[string]any{"context": "all_turns", "effort": "high"}, upstreamBody["reasoning"])
	require.NotContains(t, upstreamBody, "tools")
	require.Len(t, upstreamBody["input"].([]any)[0].(map[string]any)["tools"], 4)
	require.Equal(t, true, upstreamBody["stream"])
	require.Equal(t, false, upstreamBody["store"])
	require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
}

func TestCurlCodexAgentProfileRejectsMissingCallerCapabilities(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, true)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)

	t.Run("tool loop", func(t *testing.T) {
		c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent, "encrypted-reasoning, direct-tools")
		_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
		var profileErr *curlCodexProfileError
		require.ErrorAs(t, err, &profileErr)
		require.Equal(t, http.StatusBadRequest, profileErr.status)
		require.Contains(t, profileErr.message, "tool-loop")
	})

	t.Run("encrypted reasoning", func(t *testing.T) {
		c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent, "tool-loop, direct-tools")
		_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
		var profileErr *curlCodexProfileError
		require.ErrorAs(t, err, &profileErr)
		require.Equal(t, http.StatusBadRequest, profileErr.status)
		require.Contains(t, profileErr.message, "encrypted-reasoning")
	})

	t.Run("code mode exec", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
		c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent, "tool-loop, encrypted-reasoning, async-wait")
		_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
		var profileErr *curlCodexProfileError
		require.ErrorAs(t, err, &profileErr)
		require.Contains(t, profileErr.message, "code-mode-exec")
	})

	t.Run("async wait", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
		c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent, "tool-loop, encrypted-reasoning, code-mode-exec")
		_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
		var profileErr *curlCodexProfileError
		require.ErrorAs(t, err, &profileErr)
		require.Contains(t, profileErr.message, "async-wait")
	})
}

func TestCurlCodexProfileRejectsInvalidModelAndReasoning(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, false)

	tests := []struct {
		name    string
		body    string
		message string
	}{
		{name: "model type", body: `{"model":56,"input":"hello"}`, message: "model must be a non-empty string"},
		{name: "reasoning type", body: `{"model":"gpt-5.6-sol","input":"hello","reasoning":"high"}`, message: "reasoning must be an object"},
		{name: "effort enum", body: `{"model":"gpt-5.6-sol","input":"hello","reasoning":{"effort":"ultra"}}`, message: "reasoning.effort must be one of"},
		{name: "max on older model", body: `{"model":"gpt-5.5","input":"hello","reasoning":{"effort":"max"}}`, message: "reasoning.effort must be one of"},
		{name: "context enum", body: `{"model":"gpt-5.6-sol","input":"hello","reasoning":{"context":"future_turn"}}`, message: "reasoning.context must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "")
			_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
			var profileErr *curlCodexProfileError
			require.ErrorAs(t, err, &profileErr)
			require.Equal(t, http.StatusBadRequest, profileErr.status)
			require.Contains(t, profileErr.message, tt.message)
		})
	}
}

func TestCurlCodexProfileAllowsGPT56MaxEffort(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, false)
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","reasoning":{"effort":"max"}}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "")
	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.Equal(t, map[string]any{"context": "all_turns", "effort": "max"}, decoded["reasoning"])
}

func TestCurlCodexTextProfileDoesNotAdvertiseUnsupportedCapabilities(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, true)
	body := []byte(`{
		"model":"gpt-5.5",
		"instructions":"Answer in one sentence.",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"hidden","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hello"}
		],
		"reasoning":{"effort":"high"},
		"tools":[{"type":"function","name":"dangerous","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "remote-compaction-v2")
	c.Request.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.NotContains(t, decoded, "tools")
	require.NotContains(t, decoded, "tool_choice")
	require.NotContains(t, decoded, "parallel_tool_calls")
	require.NotContains(t, decoded, "include")
	input := decoded["input"].([]any)
	require.Len(t, input, 2)
	for _, rawItem := range input {
		require.NotEqual(t, "additional_tools", rawItem.(map[string]any)["type"])
	}

	upstreamReq, err := service.buildUpstreamRequestOpenAIPassthrough(
		context.Background(), c, curlCodexProfileTestAccount(), profiled, "oauth-token",
	)
	require.NoError(t, err)
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
	require.Equal(t, "text/event-stream", upstreamReq.Header.Get("Accept"))
}

func TestCurlCodexTextProfileKeepsParallelToolCallsFalseForResponsesLite(t *testing.T) {
	// chatgpt's Responses Lite endpoint requires parallel_tool_calls:false to be present
	// when X-OpenAI-Internal-Codex-Responses-Lite is set; absence yields
	// "X-OpenAI-Internal-Codex-Responses-Lite requires `parallel_tool_calls` to be false."
	// gpt-5.6 rides Responses Lite, so text mode must keep the explicit false even though
	// it advertises no tools.
	service := newCurlCodexProfileTestService(curlCodexProfileText, true)
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":"Answer in one sentence.",
		"input":[{"type":"message","role":"user","content":"hello"}],
		"reasoning":{"effort":"high"}
	}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.Contains(t, decoded, "parallel_tool_calls")
	require.Equal(t, false, decoded["parallel_tool_calls"])
	// text mode still carries no tools and no tool_choice.
	require.NotContains(t, decoded, "tools")
	require.NotContains(t, decoded, "tool_choice")
}

func TestCurlCodexAgentProfileKeepsDirectToolsForGPT55(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, false)
	body := []byte(`{"model":"gpt-5.5","input":"hello","reasoning":{"effort":"high"}}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent,
		"tool-loop, encrypted-reasoning, direct-tools")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.Len(t, decoded["tools"], 14)
	require.Equal(t, "auto", decoded["tool_choice"])
	require.Equal(t, true, decoded["parallel_tool_calls"])
	require.Equal(t, openai.CodexBaseInstructionsForModel("gpt-5.5"), decoded["instructions"])
	additionalItems, _, _, err := curlCodexAdditionalToolsState(decoded)
	require.NoError(t, err)
	require.Zero(t, additionalItems)

	upstreamReq, err := service.buildUpstreamRequestOpenAIPassthrough(
		context.Background(), c, curlCodexProfileTestAccount(), profiled, "oauth-token",
	)
	require.NoError(t, err)
	require.Empty(t, upstreamReq.Header.Get(responsesLiteHeader))
}

func TestCurlCodexResponsesLiteToolsFollowCallerCapabilities(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, false)
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent,
		"tool-loop, encrypted-reasoning, code-mode-exec, async-wait")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	input := decoded["input"].([]any)
	tools := input[0].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, "exec", tools[0].(map[string]any)["name"])
	require.Equal(t, "wait", tools[1].(map[string]any)["name"])
}

func TestCurlCodexTextProfileUsesResponsesLiteWithoutAdvertisingTools(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, false)
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":"Answer only.",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"hidden","parameters":{"type":"object"}}]},
			{"type":"message","role":"user","content":"hello"}
		],
		"tools":[{"type":"function","name":"also_hidden","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true
	}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	require.NotContains(t, decoded, "instructions")
	require.NotContains(t, decoded, "tools")
	require.NotContains(t, decoded, "tool_choice")
	// Responses Lite (gpt-5.6) requires parallel_tool_calls:false to be present.
	require.Equal(t, false, decoded["parallel_tool_calls"])
	additionalItems, _, _, err := curlCodexAdditionalToolsState(decoded)
	require.NoError(t, err)
	require.Zero(t, additionalItems)
	input := decoded["input"].([]any)
	require.Equal(t, openai.CodexCLI0145BaseInstructionsForModel("gpt-5.6-sol"), input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])

	upstreamReq, err := service.buildUpstreamRequestOpenAIPassthrough(
		context.Background(), c, curlCodexProfileTestAccount(), profiled, "oauth-token",
	)
	require.NoError(t, err)
	require.Equal(t, "true", upstreamReq.Header.Get(responsesLiteHeader))
}

func TestCurlCodexProfileAcceptsTruthfulExecutionContextAndPreservesCLIWireOrder(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileAgent, false)
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":"Answer directly without tools.",
		"input":"explain one stable database concept",
		"prompt_cache_key":"019fca9a-9610-7aa0-bba1-75113215d92d",
		"sub2api_codex_context":{
			"sandbox":"seatbelt",
			"workspaces":{
				"/Users/example/project":{
					"associated_remote_urls":{"origin":"git@example.invalid:team/project.git"},
					"latest_git_commit_hash":"0123456789abcdef0123456789abcdef01234567",
					"has_changes":true
				}
			},
			"developer_messages":[
				["<permissions instructions>read-only workspace</permissions instructions>","<skills_instructions>no installed skills</skills_instructions>"],
				["You are the primary agent; answer directly and do not delegate."]
			],
			"environment_context":"<environment_context><cwd>/Users/example/project</cwd><shell>zsh</shell></environment_context>"
		}
	}`)
	c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileAgent,
		"tool-loop, encrypted-reasoning, code-mode-exec, async-wait, interactive-input, multi-agent")

	profiled, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileAgent)
	require.NoError(t, err)
	require.NotContains(t, string(profiled), curlCodexRequestContextField)
	require.Contains(t, string(profiled), `"reasoning":{"effort":"high","context":"all_turns"}`)
	require.Contains(t, string(profiled), `"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec","description":`)

	orderedKeys := []string{
		`"model":`, `"input":`, `"tool_choice":`, `"parallel_tool_calls":`,
		`"reasoning":`, `"store":`, `"stream":`, `"include":`,
		`"prompt_cache_key":`, `"text":{"verbosity":`, `"client_metadata":`,
	}
	previous := -1
	for _, key := range orderedKeys {
		position := strings.Index(string(profiled), key)
		require.Greater(t, position, previous, "top-level key %s is out of Codex CLI order", key)
		previous = position
	}

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(profiled, &decoded))
	input := decoded["input"].([]any)
	require.Len(t, input, 7)
	require.Equal(t, "developer", input[1].(map[string]any)["role"])
	require.Equal(t, "developer", input[2].(map[string]any)["role"])
	require.Equal(t, "developer", input[3].(map[string]any)["role"])
	require.Len(t, input[3].(map[string]any)["content"], 2)
	require.Equal(t, "developer", input[4].(map[string]any)["role"])
	require.Equal(t, "user", input[5].(map[string]any)["role"])
	require.Contains(t, input[5].(map[string]any)["content"].([]any)[0].(map[string]any)["text"], "<environment_context>")
	require.Equal(t, "user", input[6].(map[string]any)["role"])

	metadata := decoded["client_metadata"].(map[string]any)
	var turnMetadata curlCodexTurnMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata["x-codex-turn-metadata"].(string)), &turnMetadata))
	require.Equal(t, "seatbelt", turnMetadata.Sandbox)
	workspace, ok := turnMetadata.Workspaces["/Users/example/project"]
	require.True(t, ok)
	require.NotNil(t, workspace.HasChanges)
	require.True(t, *workspace.HasChanges)
	require.Equal(t, "git@example.invalid:team/project.git", workspace.AssociatedRemoteURLs["origin"])
}

func TestCurlCodexProfileRejectsInvalidExecutionContext(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, false)
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"input":"hello","sub2api_codex_context":{"invented":true}}`},
		{name: "sandbox", body: `{"input":"hello","sub2api_codex_context":{"sandbox":"docker"}}`},
		{name: "relative workspace", body: `{"input":"hello","sub2api_codex_context":{"workspaces":{"relative/path":{}}}}`},
		{name: "empty developer block", body: `{"input":"hello","sub2api_codex_context":{"developer_messages":[[""]]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			c, _ := newCurlCodexProfileTestContext(t, body, curlCodexProfileText, "")
			_, err := service.applyCurlCodexProfile(c, curlCodexProfileTestAccount(), body, curlCodexProfileText)
			var profileErr *curlCodexProfileError
			require.ErrorAs(t, err, &profileErr)
			require.Equal(t, http.StatusBadRequest, profileErr.status)
		})
	}
}

func TestResolveCurlCodexProfileRequiresExplicitOptInByDefault(t *testing.T) {
	service := newCurlCodexProfileTestService(curlCodexProfileText, true)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	c, _ := newCurlCodexProfileTestContext(t, body, "", "")
	mode, enabled, err := service.resolveCurlCodexProfile(c)
	require.NoError(t, err)
	require.False(t, enabled)
	require.Empty(t, mode)

	service.cfg.Gateway.CurlCodexProfile.RequireOptInHeader = false
	mode, enabled, err = service.resolveCurlCodexProfile(c)
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, curlCodexProfileText, mode)

	c.Request.Header.Set("User-Agent", "openai-go/1.0")
	mode, enabled, err = service.resolveCurlCodexProfile(c)
	require.NoError(t, err)
	require.False(t, enabled)
	require.Empty(t, mode)
}

func newCurlCodexProfileTestService(defaultMode string, remoteCompaction bool) *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		CurlCodexProfile: config.GatewayCurlCodexProfileConfig{
			Enabled:                   true,
			RequireOptInHeader:        true,
			DefaultMode:               defaultMode,
			DefaultModel:              "gpt-5.6-sol",
			DefaultReasoningEffort:    "high",
			DefaultReasoningContext:   "all_turns",
			EnableRemoteCompactionV2:  remoteCompaction,
			RequireToolLoopCapability: true,
		},
	}}}
}

func newCurlCodexProfileTestContext(
	t *testing.T,
	body []byte,
	mode string,
	capabilities string,
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Accept", "*/*")
	c.Request.Header.Set("User-Agent", "curl/8.7.1")
	if strings.TrimSpace(mode) != "" {
		c.Request.Header.Set(curlCodexProfileHeader, mode)
	}
	if strings.TrimSpace(capabilities) != "" {
		c.Request.Header.Set(curlCodexCapabilitiesHeader, capabilities)
	}
	c.Set("api_key", &APIKey{ID: 4242})
	return c, recorder
}

func curlCodexProfileTestAccount() *Account {
	return &Account{
		ID:       9001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "00000000-0000-4000-8000-000000000042",
		},
		Extra: map[string]any{
			"openai_passthrough": true,
			"openai_device_id":   "48ca0404-a22d-4f52-8407-b9f6fc68bd04",
		},
	}
}
