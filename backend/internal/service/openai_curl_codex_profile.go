package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	curlCodexProfileHeader             = "X-Sub2API-Codex-Profile"
	curlCodexCapabilitiesHeader        = "X-Sub2API-Codex-Capabilities"
	curlCodexSessionHeader             = "X-Sub2API-Codex-Session-Id"
	curlCodexInstallationHeader        = "X-Sub2API-Codex-Installation-Id"
	curlCodexResponseSessionHeader     = "X-Sub2API-Codex-Session-Id"
	curlCodexResponseThreadHeader      = "X-Sub2API-Codex-Thread-Id"
	curlCodexResponseTurnHeader        = "X-Sub2API-Codex-Turn-Id"
	curlCodexProfileContextKey         = "openai_curl_codex_profile_state"
	curlCodexProfileText               = "text"
	curlCodexProfileAgent              = "agent"
	curlCodexCapabilityToolLoop        = "tool-loop"
	curlCodexCapabilityReasoning       = "encrypted-reasoning"
	curlCodexCapabilityDirectTools     = "direct-tools"
	curlCodexCapabilityCodeModeExec    = "code-mode-exec"
	curlCodexCapabilityAsyncWait       = "async-wait"
	curlCodexCapabilityInteractive     = "interactive-input"
	curlCodexCapabilityMultiAgent      = "multi-agent"
	curlCodexCapabilityRemoteCompactV2 = "remote-compaction-v2"
	curlCodexProfileDefaultModel       = "gpt-5.6-sol"
	curlCodexProfileDefaultUserAgent   = "codex_cli_rs/0.145.0 (Mac OS 26.5.2; arm64) vscode/1.130.0"
	curlCodexRequestContextField       = "sub2api_codex_context"
	curlCodexContextTextLimit          = 64 << 10
	curlCodexContextMessagesLimit      = 8
	curlCodexContextBlocksLimit        = 16
	curlCodexContextWorkspacesLimit    = 32
	curlCodexContextRemotesLimit       = 32
)

type curlCodexProfileError struct {
	status  int
	message string
}

func (e *curlCodexProfileError) Error() string { return e.message }

type curlCodexProfileState struct {
	Mode                    string
	Model                   string
	ResponsesLite           bool
	SessionID               string
	ThreadID                string
	TurnID                  string
	InstallationID          string
	WindowID                string
	TurnMetadata            string
	UserAgent               string
	Originator              string
	RemoteCompactionEnabled bool
}

// curlCodexRequestContext contains caller-provided facts about the execution
// environment. The compatibility profile consumes this private field locally
// and never forwards it upstream. Nothing here is synthesized: callers should
// provide only context their curl-side executor actually implements.
type curlCodexRequestContext struct {
	Sandbox            string                        `json:"sandbox,omitempty"`
	Workspaces         map[string]curlCodexWorkspace `json:"workspaces,omitempty"`
	DeveloperMessages  [][]string                    `json:"developer_messages,omitempty"`
	EnvironmentContext string                        `json:"environment_context,omitempty"`
}

type curlCodexWorkspace struct {
	AssociatedRemoteURLs map[string]string `json:"associated_remote_urls,omitempty"`
	LatestGitCommitHash  string            `json:"latest_git_commit_hash,omitempty"`
	HasChanges           *bool             `json:"has_changes,omitempty"`
}

// orderedCurlCodexReasoning preserves the field order emitted by Codex 0.145:
// effort, context, followed by any caller-supplied extension fields in stable
// lexical order. JSON object order is not semantically significant, but keeping
// the native wire order avoids an unnecessary request fingerprint difference.
type orderedCurlCodexReasoning map[string]any

func (r orderedCurlCodexReasoning) MarshalJSON() ([]byte, error) {
	return marshalCurlCodexOrderedMap(map[string]any(r), []string{"effort", "context"})
}

// orderedCurlCodexRequest preserves the top-level field order emitted by the
// captured Codex CLI 0.145.0 request. Extra caller fields follow in lexical
// order. Object order is semantically irrelevant JSON, but stable wire order
// removes a low-value fingerprint difference.
type orderedCurlCodexRequest map[string]any

func (r orderedCurlCodexRequest) MarshalJSON() ([]byte, error) {
	return marshalCurlCodexOrderedMap(map[string]any(r), []string{
		"model", "input", "tool_choice", "parallel_tool_calls", "reasoning",
		"store", "stream", "include", "prompt_cache_key", "text", "client_metadata",
	})
}

type orderedCurlCodexAdditionalTools map[string]any

func (item orderedCurlCodexAdditionalTools) MarshalJSON() ([]byte, error) {
	return marshalCurlCodexOrderedMap(map[string]any(item), []string{"type", "role", "tools"})
}

func marshalCurlCodexOrderedMap(value map[string]any, preferred []string) ([]byte, error) {
	keys := make([]string, 0, len(value))
	seen := make(map[string]bool, len(preferred))
	for _, key := range preferred {
		if _, ok := value[key]; ok {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	extra := make([]string, 0, len(value)-len(keys))
	for key := range value {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		encodedKey, err := marshalOpenAIUpstreamJSON(key)
		if err != nil {
			return nil, err
		}
		encodedValue, err := marshalOpenAIUpstreamJSON(value[key])
		if err != nil {
			return nil, err
		}
		buf.Write(encodedKey)
		buf.WriteByte(':')
		buf.Write(encodedValue)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type curlCodexTurnMetadata struct {
	InstallationID  string                        `json:"installation_id"`
	SessionID       string                        `json:"session_id"`
	ThreadID        string                        `json:"thread_id"`
	TurnID          string                        `json:"turn_id"`
	WindowID        string                        `json:"window_id"`
	RequestKind     string                        `json:"request_kind"`
	ThreadSource    string                        `json:"thread_source"`
	Sandbox         string                        `json:"sandbox"`
	Workspaces      map[string]curlCodexWorkspace `json:"workspaces,omitempty"`
	TurnStartedAtMS int64                         `json:"turn_started_at_unix_ms"`
}

func (s *OpenAIGatewayService) resolveCurlCodexProfile(c *gin.Context) (string, bool, error) {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.CurlCodexProfile.Enabled {
		return "", false, nil
	}
	if c == nil || c.Request == nil {
		return "", false, nil
	}

	cfg := s.cfg.Gateway.CurlCodexProfile
	mode := strings.ToLower(strings.TrimSpace(c.Request.Header.Get(curlCodexProfileHeader)))
	if mode == "" {
		if cfg.RequireOptInHeader {
			return "", false, nil
		}
		// Even with header opt-in disabled, scope automatic activation to curl.
		// A generic SDK must not silently acquire agent semantics.
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Request.UserAgent())), "curl/") {
			return "", false, nil
		}
		mode = cfg.DefaultMode
		if mode == "" {
			mode = curlCodexProfileText
		}
	}
	if mode != curlCodexProfileText && mode != curlCodexProfileAgent {
		return "", false, &curlCodexProfileError{
			status:  http.StatusBadRequest,
			message: curlCodexProfileHeader + " must be text or agent",
		}
	}
	if isOpenAIResponsesCompactPath(c) {
		return "", false, &curlCodexProfileError{
			status:  http.StatusBadRequest,
			message: "curl Codex profile is not supported on /responses/compact",
		}
	}
	return mode, true, nil
}

func parseCurlCodexCapabilities(c *gin.Context) map[string]bool {
	capabilities := make(map[string]bool)
	if c == nil || c.Request == nil {
		return capabilities
	}
	for _, raw := range c.Request.Header.Values(curlCodexCapabilitiesHeader) {
		for _, item := range strings.Split(raw, ",") {
			if normalized := strings.ToLower(strings.TrimSpace(item)); normalized != "" {
				capabilities[normalized] = true
			}
		}
	}
	return capabilities
}

func writeCurlCodexProfileError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "failed to construct Codex upstream request"
	var profileErr *curlCodexProfileError
	if errors.As(err, &profileErr) {
		status = profileErr.status
		message = profileErr.message
	}
	c.JSON(status, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"message": message,
	}})
}

func (s *OpenAIGatewayService) applyCurlCodexProfile(
	c *gin.Context,
	account *Account,
	body []byte,
	mode string,
) ([]byte, error) {
	var reqBody map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&reqBody); err != nil {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "request body must be a JSON object"}
	}
	if reqBody == nil {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "request body must be a JSON object"}
	}

	cfg := s.cfg.Gateway.CurlCodexProfile
	model, err := resolveCurlCodexModel(reqBody, cfg.DefaultModel)
	if err != nil {
		return nil, err
	}
	reqBody["model"] = model
	requestContext, err := extractCurlCodexRequestContext(reqBody)
	if err != nil {
		return nil, err
	}

	callerInstructions, err := curlCodexCallerInstructions(reqBody["instructions"])
	if err != nil {
		return nil, err
	}
	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		IsCodexCLI:              true,
		IsCompact:               false,
		PreserveToolCallIDs:     true,
		SkipDefaultInstructions: true,
	})

	baseInstructions := openai.CodexCLI0145BaseInstructionsForModel(model)
	if strings.TrimSpace(baseInstructions) == "" {
		return nil, fmt.Errorf("embedded Codex instructions are empty for model %q", model)
	}
	reqBody["instructions"] = baseInstructions
	if err := prependCurlCodexDeveloperContext(reqBody, cfg.DeveloperContext, callerInstructions, baseInstructions); err != nil {
		return nil, err
	}
	normalizeCurlCodexMessageContent(reqBody)

	responsesLite := curlCodexUsesResponsesLite(model)
	if responsesLite {
		// Codex Responses Lite carries the base instructions as a developer input
		// item and omits the top-level instructions property.
		delete(reqBody, "instructions")
		if err := prependCurlCodexDeveloperMessage(reqBody, baseInstructions); err != nil {
			return nil, err
		}
	}
	if err := appendCurlCodexExecutionContext(reqBody, requestContext); err != nil {
		return nil, err
	}
	if err := removeCurlCodexAdditionalTools(reqBody); err != nil {
		return nil, err
	}

	capabilities := parseCurlCodexCapabilities(c)
	if mode == curlCodexProfileAgent {
		if cfg.RequireToolLoopCapability && !capabilities[curlCodexCapabilityToolLoop] {
			return nil, &curlCodexProfileError{
				status:  http.StatusBadRequest,
				message: "agent profile requires X-Sub2API-Codex-Capabilities: tool-loop",
			}
		}
		if !capabilities[curlCodexCapabilityReasoning] {
			return nil, &curlCodexProfileError{
				status:  http.StatusBadRequest,
				message: "agent profile requires encrypted-reasoning capability for store=false continuation",
			}
		}
		if responsesLite {
			if cfg.RequireToolLoopCapability {
				if err := requireCurlCodexCapability(capabilities, curlCodexCapabilityCodeModeExec, "GPT-5.6 agent profile requires a real Code Mode executor"); err != nil {
					return nil, err
				}
				if err := requireCurlCodexCapability(capabilities, curlCodexCapabilityAsyncWait, "GPT-5.6 agent profile requires async wait support"); err != nil {
					return nil, err
				}
			}
			tools, err := curlCodexResponsesLiteAgentTools(cfg.ResponsesLiteToolsTemplate, capabilities)
			if err != nil {
				return nil, err
			}
			delete(reqBody, "tools")
			if err := prependCurlCodexAdditionalTools(reqBody, tools); err != nil {
				return nil, err
			}
			reqBody["parallel_tool_calls"] = false
		} else {
			if cfg.RequireToolLoopCapability {
				if err := requireCurlCodexCapability(capabilities, curlCodexCapabilityDirectTools, "direct agent profile requires the configured tool handlers"); err != nil {
					return nil, err
				}
			}
			tools, err := curlCodexDirectAgentTools(cfg.ToolsTemplate)
			if err != nil {
				return nil, err
			}
			reqBody["tools"] = tools
			reqBody["parallel_tool_calls"] = true
		}
		reqBody["tool_choice"] = "auto"
	} else {
		delete(reqBody, "tools")
		delete(reqBody, "tool_choice")
		// chatgpt's Responses Lite endpoint rejects the request when parallel_tool_calls
		// is absent while X-OpenAI-Internal-Codex-Responses-Lite is set
		// ("X-OpenAI-Internal-Codex-Responses-Lite requires `parallel_tool_calls` to be false").
		// Text mode carries no tools but still rides the Responses Lite envelope, so keep
		// the explicit false the upstream requires instead of deleting the field.
		if responsesLite {
			reqBody["parallel_tool_calls"] = false
		} else {
			delete(reqBody, "parallel_tool_calls")
		}
	}

	reasoning, err := resolveCurlCodexReasoning(
		reqBody["reasoning"], model, cfg.DefaultReasoningEffort, cfg.DefaultReasoningContext,
	)
	if err != nil {
		return nil, err
	}
	if responsesLite {
		// Codex 0.145.0 fixes Responses Lite turns to all-turns reasoning while
		// still preserving the caller's valid effort and summary settings.
		reasoning["context"] = "all_turns"
	}
	// Keep the ordinary map visible while include policy is applied; the shared
	// helper intentionally recognizes only a real reasoning object. Wrap it in
	// the ordered wire type after that semantic mutation is complete.
	reqBody["reasoning"] = reasoning
	if _, ok := reqBody["text"].(map[string]any); !ok {
		reqBody["text"] = map[string]any{"verbosity": "low"}
	}
	if capabilities[curlCodexCapabilityReasoning] {
		ensureCodexReasoningInclude(reqBody)
	} else {
		removeCurlCodexEncryptedReasoningInclude(reqBody)
	}
	reqBody["reasoning"] = orderedCurlCodexReasoning(reasoning)

	requestedSessionID := strings.TrimSpace(firstNonEmptyString(reqBody["prompt_cache_key"]))
	state, err := s.newCurlCodexProfileState(c, account, mode, model, responsesLite, requestedSessionID, capabilities, requestContext)
	if err != nil {
		return nil, err
	}
	reqBody["prompt_cache_key"] = state.SessionID
	reqBody["store"] = false
	reqBody["stream"] = true
	reqBody["client_metadata"] = map[string]any{
		"session_id":              state.SessionID,
		"thread_id":               state.ThreadID,
		"turn_id":                 state.TurnID,
		"x-codex-installation-id": state.InstallationID,
		"x-codex-turn-metadata":   state.TurnMetadata,
		"x-codex-window-id":       state.WindowID,
	}
	if err := validateCurlCodexProfileBody(reqBody, state); err != nil {
		return nil, err
	}
	encoded, err := marshalOpenAIUpstreamJSON(orderedCurlCodexRequest(reqBody))
	if err != nil {
		return nil, fmt.Errorf("marshal curl Codex profile body: %w", err)
	}
	c.Set(curlCodexProfileContextKey, state)
	return encoded, nil
}

func extractCurlCodexRequestContext(reqBody map[string]any) (*curlCodexRequestContext, error) {
	raw, exists := reqBody[curlCodexRequestContextField]
	delete(reqBody, curlCodexRequestContextField)
	requestContext := &curlCodexRequestContext{Sandbox: "none"}
	if !exists || raw == nil {
		return requestContext, nil
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + " must be an object"}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(requestContext); err != nil {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "invalid " + curlCodexRequestContextField + ": " + err.Error()}
	}

	requestContext.Sandbox = strings.ToLower(strings.TrimSpace(requestContext.Sandbox))
	if requestContext.Sandbox == "" {
		requestContext.Sandbox = "none"
	}
	if requestContext.Sandbox != "none" && requestContext.Sandbox != "seatbelt" {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".sandbox must be none or seatbelt"}
	}
	if len(requestContext.Workspaces) > curlCodexContextWorkspacesLimit {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".workspaces has too many entries"}
	}
	for workspacePath, workspace := range requestContext.Workspaces {
		if workspacePath != strings.TrimSpace(workspacePath) || !filepath.IsAbs(workspacePath) || len(workspacePath) > 4096 {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".workspaces keys must be absolute paths"}
		}
		if len(workspace.AssociatedRemoteURLs) > curlCodexContextRemotesLimit {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".workspaces remote URL map has too many entries"}
		}
		for name, remoteURL := range workspace.AssociatedRemoteURLs {
			if strings.TrimSpace(name) == "" || len(name) > 256 || strings.TrimSpace(remoteURL) == "" || len(remoteURL) > 4096 {
				return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".workspaces contains an invalid remote URL entry"}
			}
		}
		if len(workspace.LatestGitCommitHash) > 128 || strings.ContainsAny(workspace.LatestGitCommitHash, " \t\r\n") {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".workspaces contains an invalid git commit hash"}
		}
	}
	if len(requestContext.DeveloperMessages) > curlCodexContextMessagesLimit {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".developer_messages has too many messages"}
	}
	for _, message := range requestContext.DeveloperMessages {
		if len(message) == 0 || len(message) > curlCodexContextBlocksLimit {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".developer_messages contains an invalid content block list"}
		}
		for _, block := range message {
			if strings.TrimSpace(block) == "" || len(block) > curlCodexContextTextLimit {
				return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".developer_messages contains an invalid content block"}
			}
		}
	}
	if len(requestContext.EnvironmentContext) > curlCodexContextTextLimit {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: curlCodexRequestContextField + ".environment_context is too large"}
	}
	return requestContext, nil
}

func appendCurlCodexExecutionContext(reqBody map[string]any, requestContext *curlCodexRequestContext) error {
	if requestContext == nil || (len(requestContext.DeveloperMessages) == 0 && strings.TrimSpace(requestContext.EnvironmentContext) == "") {
		return nil
	}
	input, ok := reqBody["input"].([]any)
	if !ok {
		return &curlCodexProfileError{status: http.StatusBadRequest, message: "input must be a string, object, or array"}
	}
	insertAt := 0
	for insertAt < len(input) {
		item, ok := curlCodexItemMap(input[insertAt])
		if !ok || firstNonEmptyString(item["type"]) != "message" || firstNonEmptyString(item["role"]) != "developer" {
			break
		}
		insertAt++
	}

	additional := make([]any, 0, len(requestContext.DeveloperMessages)+1)
	for _, message := range requestContext.DeveloperMessages {
		additional = append(additional, newCurlCodexMessage("developer", message))
	}
	if strings.TrimSpace(requestContext.EnvironmentContext) != "" {
		additional = append(additional, newCurlCodexMessage("user", []string{requestContext.EnvironmentContext}))
	}
	updated := make([]any, 0, len(input)+len(additional))
	updated = append(updated, input[:insertAt]...)
	updated = append(updated, additional...)
	updated = append(updated, input[insertAt:]...)
	reqBody["input"] = updated
	return nil
}

func newCurlCodexMessage(role string, texts []string) map[string]any {
	content := make([]any, 0, len(texts))
	for _, text := range texts {
		content = append(content, map[string]any{"type": "input_text", "text": text})
	}
	return map[string]any{"type": "message", "role": role, "content": content}
}

func curlCodexCallerInstructions(raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", &curlCodexProfileError{status: http.StatusBadRequest, message: "instructions must be a string"}
	}
	return strings.TrimSpace(value), nil
}

func resolveCurlCodexModel(reqBody map[string]any, defaultModel string) (string, error) {
	if raw, exists := reqBody["model"]; exists {
		model, ok := raw.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return "", &curlCodexProfileError{status: http.StatusBadRequest, message: "model must be a non-empty string"}
		}
		return strings.TrimSpace(model), nil
	}
	model := strings.TrimSpace(defaultModel)
	if model == "" {
		model = curlCodexProfileDefaultModel
	}
	return model, nil
}

func resolveCurlCodexReasoning(raw any, model, defaultEffort, defaultContext string) (map[string]any, error) {
	reasoning := make(map[string]any)
	if raw != nil {
		existing, ok := raw.(map[string]any)
		if !ok {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "reasoning must be an object"}
		}
		for key, value := range existing {
			reasoning[key] = value
		}
	}

	effort := strings.TrimSpace(defaultEffort)
	if effort == "" {
		effort = "high"
	}
	if callerEffort, exists := reasoning["effort"]; exists {
		value, ok := callerEffort.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "reasoning.effort must be a non-empty string"}
		}
		effort = value
	}
	normalizedEffort, ok := normalizeCurlCodexReasoningEffort(effort, model)
	if !ok {
		return nil, &curlCodexProfileError{
			status:  http.StatusBadRequest,
			message: "reasoning.effort must be one of none, low, medium, high, xhigh, or max (gpt-5.6 only)",
		}
	}
	reasoning["effort"] = normalizedEffort

	reasoningContext := strings.TrimSpace(defaultContext)
	if reasoningContext == "" {
		reasoningContext = "all_turns"
	}
	if callerContext, exists := reasoning["context"]; exists {
		value, ok := callerContext.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "reasoning.context must be a non-empty string"}
		}
		reasoningContext = value
	}
	reasoningContext = strings.ToLower(strings.TrimSpace(reasoningContext))
	if reasoningContext != "all_turns" && reasoningContext != "current_turn" {
		return nil, &curlCodexProfileError{status: http.StatusBadRequest, message: "reasoning.context must be all_turns or current_turn"}
	}
	reasoning["context"] = reasoningContext
	return reasoning, nil
}

func normalizeCurlCodexReasoningEffort(raw, model string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	switch value {
	case "none", "minimal":
		return "none", true
	case "low", "medium", "high":
		return value, true
	case "xhigh", "extrahigh":
		return "xhigh", true
	case "max":
		if isOpenAIGPT56Model(model) {
			return "max", true
		}
	}
	return "", false
}

func curlCodexUsesResponsesLite(model string) bool {
	return isOpenAIGPT56Model(strings.TrimSpace(model))
}

func requireCurlCodexCapability(capabilities map[string]bool, capability, reason string) error {
	if capabilities[capability] {
		return nil
	}
	return &curlCodexProfileError{
		status:  http.StatusBadRequest,
		message: reason + "; add " + capability + " to " + curlCodexCapabilitiesHeader,
	}
}

func prependCurlCodexDeveloperContext(reqBody map[string]any, configured, caller, base string) error {
	parts := make([]string, 0, 2)
	if configured = strings.TrimSpace(configured); configured != "" {
		parts = append(parts, configured)
	}
	if caller != "" && caller != strings.TrimSpace(base) {
		parts = append(parts, "<caller_instructions>\n"+caller+"\n</caller_instructions>")
	}
	if len(parts) == 0 {
		return nil
	}
	return prependCurlCodexDeveloperMessage(reqBody, strings.Join(parts, "\n\n"))
}

func prependCurlCodexDeveloperMessage(reqBody map[string]any, text string) error {
	input, ok := reqBody["input"].([]any)
	if !ok {
		return &curlCodexProfileError{status: http.StatusBadRequest, message: "input must be a string, object, or array"}
	}
	developer := newCurlCodexMessage("developer", []string{text})
	reqBody["input"] = append([]any{developer}, input...)
	return nil
}

func normalizeCurlCodexMessageContent(reqBody map[string]any) {
	input, ok := reqBody["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "message" {
			continue
		}
		content, ok := item["content"].(string)
		if !ok {
			continue
		}
		item["content"] = []any{map[string]any{
			"type": "input_text",
			"text": content,
		}}
	}
	reqBody["input"] = input
}

func removeCurlCodexAdditionalTools(reqBody map[string]any) error {
	input, ok := reqBody["input"].([]any)
	if !ok {
		return &curlCodexProfileError{status: http.StatusBadRequest, message: "input must be a string, object, or array"}
	}
	filtered := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := curlCodexItemMap(rawItem)
		if ok && strings.TrimSpace(firstNonEmptyString(item["type"])) == "additional_tools" {
			continue
		}
		filtered = append(filtered, rawItem)
	}
	reqBody["input"] = filtered
	return nil
}

func prependCurlCodexAdditionalTools(reqBody map[string]any, tools []any) error {
	if len(tools) == 0 {
		return fmt.Errorf("curl Codex Responses Lite tools template must expose at least one supported tool")
	}
	input, ok := reqBody["input"].([]any)
	if !ok {
		return &curlCodexProfileError{status: http.StatusBadRequest, message: "input must be a string, object, or array"}
	}
	additional := orderedCurlCodexAdditionalTools{
		"type":  "additional_tools",
		"role":  "developer",
		"tools": tools,
	}
	reqBody["input"] = append([]any{additional}, input...)
	return nil
}

func curlCodexItemMap(raw any) (map[string]any, bool) {
	switch item := raw.(type) {
	case map[string]any:
		return item, true
	case orderedCurlCodexAdditionalTools:
		return map[string]any(item), true
	default:
		return nil, false
	}
}

func curlCodexDirectAgentTools(configured []byte) ([]any, error) {
	raw := configured
	if len(raw) == 0 {
		raw = openai.CodexAgentToolsJSON()
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode curl Codex tools template: %w", err)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("curl Codex tools template must contain at least one tool")
	}
	result := make([]any, 0, len(tools))
	for index, tool := range tools {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool, &probe); err != nil || strings.TrimSpace(probe.Type) == "" {
			return nil, fmt.Errorf("curl Codex tool at index %d must be an object with a type", index)
		}
		if probe.Type != "tool_search" && probe.Type != "web_search" && strings.TrimSpace(probe.Name) == "" {
			return nil, fmt.Errorf("curl Codex tool at index %d must include a name", index)
		}
		result = append(result, tool)
	}
	return result, nil
}

func curlCodexResponsesLiteAgentTools(configured []byte, capabilities map[string]bool) ([]any, error) {
	raw := configured
	if len(raw) == 0 {
		raw = openai.CodexResponsesLiteAgentToolsJSON()
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode curl Codex Responses Lite tools template: %w", err)
	}

	filtered := make([]any, 0, len(tools))
	for index, rawTool := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rawTool, &tool); err != nil {
			return nil, fmt.Errorf("curl Codex Responses Lite tool at index %d must be an object", index)
		}
		name := strings.TrimSpace(tool.Name)
		var capability string
		switch name {
		case "exec":
			capability = curlCodexCapabilityCodeModeExec
		case "wait":
			capability = curlCodexCapabilityAsyncWait
		case "request_user_input":
			capability = curlCodexCapabilityInteractive
		case "collaboration":
			capability = curlCodexCapabilityMultiAgent
		default:
			return nil, fmt.Errorf("curl Codex Responses Lite tools template contains unsupported outer tool %q", name)
		}
		if capabilities[capability] {
			filtered = append(filtered, rawTool)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("curl Codex Responses Lite tools template has no tools enabled by caller capabilities")
	}
	return filtered, nil
}

func removeCurlCodexEncryptedReasoningInclude(reqBody map[string]any) {
	const encrypted = "reasoning.encrypted_content"
	values, ok := reqBody["include"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && text == encrypted {
			continue
		}
		filtered = append(filtered, value)
	}
	if len(filtered) == 0 {
		delete(reqBody, "include")
		return
	}
	reqBody["include"] = filtered
}

func (s *OpenAIGatewayService) newCurlCodexProfileState(
	c *gin.Context,
	account *Account,
	mode string,
	model string,
	responsesLite bool,
	requestedSessionID string,
	capabilities map[string]bool,
	requestContext *curlCodexRequestContext,
) (*curlCodexProfileState, error) {
	cfg := s.cfg.Gateway.CurlCodexProfile
	sessionID := ""
	if c != nil && c.Request != nil {
		sessionID = strings.TrimSpace(c.Request.Header.Get(curlCodexSessionHeader))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(requestedSessionID)
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		sessionID = newCurlCodexUUID()
	}

	installationID := ""
	if c != nil && c.Request != nil {
		installationID = strings.TrimSpace(c.Request.Header.Get(curlCodexInstallationHeader))
	}
	if _, err := uuid.Parse(installationID); err != nil {
		installationID = ""
	}
	if installationID == "" && account != nil {
		if candidate := strings.TrimSpace(account.GetOpenAIDeviceID()); candidate != "" {
			if _, err := uuid.Parse(candidate); err == nil {
				installationID = candidate
			}
		}
	}
	if installationID == "" {
		seed := fmt.Sprintf("sub2api:api-key:%d:account:%d", getAPIKeyIDFromContext(c), accountID(account))
		installationID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
	}

	turnID := newCurlCodexUUID()
	windowID := sessionID + ":0"
	sandbox := "none"
	var workspaces map[string]curlCodexWorkspace
	if requestContext != nil {
		if requestContext.Sandbox != "" {
			sandbox = requestContext.Sandbox
		}
		workspaces = requestContext.Workspaces
	}
	metadataBytes, err := json.Marshal(curlCodexTurnMetadata{
		InstallationID:  installationID,
		SessionID:       sessionID,
		ThreadID:        sessionID,
		TurnID:          turnID,
		WindowID:        windowID,
		RequestKind:     "turn",
		ThreadSource:    "user",
		Sandbox:         sandbox,
		Workspaces:      workspaces,
		TurnStartedAtMS: time.Now().UnixMilli(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal curl Codex turn metadata: %w", err)
	}
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = curlCodexProfileDefaultUserAgent
	}
	originator, pairedUserAgent, ok := openai.PairCodexClientIdentity(userAgent)
	if !ok {
		return nil, fmt.Errorf("gateway.curl_codex_profile.user_agent must identify an official Codex client")
	}
	userAgent = pairedUserAgent
	return &curlCodexProfileState{
		Mode:                    mode,
		Model:                   model,
		ResponsesLite:           responsesLite,
		SessionID:               sessionID,
		ThreadID:                sessionID,
		TurnID:                  turnID,
		InstallationID:          installationID,
		WindowID:                windowID,
		TurnMetadata:            string(metadataBytes),
		UserAgent:               userAgent,
		Originator:              originator,
		RemoteCompactionEnabled: mode == curlCodexProfileAgent && cfg.EnableRemoteCompactionV2 && capabilities[curlCodexCapabilityRemoteCompactV2],
	}, nil
}

func accountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

func newCurlCodexUUID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func curlCodexProfileStateFromContext(c *gin.Context) (*curlCodexProfileState, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get(curlCodexProfileContextKey)
	if !ok {
		return nil, false
	}
	state, ok := value.(*curlCodexProfileState)
	return state, ok && state != nil
}

func validateCurlCodexProfileBody(reqBody map[string]any, state *curlCodexProfileState) error {
	metadata, ok := reqBody["client_metadata"].(map[string]any)
	if !ok {
		return fmt.Errorf("curl Codex profile invariant: client_metadata missing")
	}
	want := map[string]string{
		"session_id": state.SessionID, "thread_id": state.ThreadID, "turn_id": state.TurnID,
		"x-codex-installation-id": state.InstallationID,
		"x-codex-turn-metadata":   state.TurnMetadata, "x-codex-window-id": state.WindowID,
	}
	for key, expected := range want {
		if actual := strings.TrimSpace(firstNonEmptyString(metadata[key])); actual != expected {
			return fmt.Errorf("curl Codex profile invariant: client_metadata.%s mismatch", key)
		}
	}
	if firstNonEmptyString(reqBody["prompt_cache_key"]) != state.SessionID {
		return fmt.Errorf("curl Codex profile invariant: prompt_cache_key mismatch")
	}
	var turnMetadata curlCodexTurnMetadata
	if err := json.Unmarshal([]byte(state.TurnMetadata), &turnMetadata); err != nil {
		return fmt.Errorf("curl Codex profile invariant: invalid turn metadata: %w", err)
	}
	if turnMetadata.InstallationID != state.InstallationID ||
		turnMetadata.SessionID != state.SessionID || turnMetadata.ThreadID != state.ThreadID ||
		turnMetadata.TurnID != state.TurnID || turnMetadata.WindowID != state.WindowID {
		return fmt.Errorf("curl Codex profile invariant: turn metadata identifiers mismatch")
	}
	if strings.TrimSpace(firstNonEmptyString(reqBody["model"])) != state.Model {
		return fmt.Errorf("curl Codex profile invariant: model mismatch")
	}
	_, hasTopLevelTools := reqBody["tools"]
	additionalToolItems, additionalToolCount, firstItemIsAdditional, err := curlCodexAdditionalToolsState(reqBody)
	if err != nil {
		return err
	}
	if state.Mode == curlCodexProfileAgent {
		if reqBody["tool_choice"] != "auto" {
			return fmt.Errorf("curl Codex profile invariant: agent tool controls mismatch")
		}
		if state.ResponsesLite {
			if hasTopLevelTools || additionalToolItems != 1 || additionalToolCount == 0 || !firstItemIsAdditional {
				return fmt.Errorf("curl Codex profile invariant: Responses Lite tools carrier mismatch")
			}
			if reqBody["parallel_tool_calls"] != false {
				return fmt.Errorf("curl Codex profile invariant: Responses Lite parallel tool controls mismatch")
			}
		} else {
			if !hasTopLevelTools || additionalToolItems != 0 {
				return fmt.Errorf("curl Codex profile invariant: direct tools carrier mismatch")
			}
			if reqBody["parallel_tool_calls"] != true {
				return fmt.Errorf("curl Codex profile invariant: direct parallel tool controls mismatch")
			}
		}
	} else {
		if hasTopLevelTools || additionalToolItems != 0 {
			return fmt.Errorf("curl Codex profile invariant: text mode advertises tools")
		}
		if _, hasChoice := reqBody["tool_choice"]; hasChoice {
			return fmt.Errorf("curl Codex profile invariant: text mode advertises tool choice")
		}
		// chatgpt's Responses Lite endpoint requires parallel_tool_calls:false to be present
		// (gpt-5.6 rides Responses Lite). Non-Responses-Lite text mode (e.g. gpt-5.5) must
		// still omit it entirely.
		if state.ResponsesLite {
			if reqBody["parallel_tool_calls"] != false {
				return fmt.Errorf("curl Codex profile invariant: Responses Lite text mode requires parallel_tool_calls=false")
			}
		} else if _, hasParallel := reqBody["parallel_tool_calls"]; hasParallel {
			return fmt.Errorf("curl Codex profile invariant: text mode advertises parallel tool calls")
		}
	}
	if state.ResponsesLite {
		if _, hasInstructions := reqBody["instructions"]; hasInstructions {
			return fmt.Errorf("curl Codex profile invariant: Responses Lite advertises top-level instructions")
		}
	} else if strings.TrimSpace(firstNonEmptyString(reqBody["instructions"])) == "" {
		return fmt.Errorf("curl Codex profile invariant: direct instructions missing")
	}
	return nil
}

func curlCodexAdditionalToolsState(reqBody map[string]any) (items int, tools int, first bool, err error) {
	input, ok := reqBody["input"].([]any)
	if !ok {
		return 0, 0, false, fmt.Errorf("curl Codex profile invariant: input missing")
	}
	for index, rawItem := range input {
		item, ok := curlCodexItemMap(rawItem)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		items++
		if index == 0 {
			first = true
		}
		declared, ok := item["tools"].([]any)
		if !ok {
			return 0, 0, false, fmt.Errorf("curl Codex profile invariant: additional_tools.tools must be an array")
		}
		tools += len(declared)
	}
	return items, tools, first, nil
}

func applyCurlCodexProfileHeaders(c *gin.Context, req *http.Request, apiKeyID int64, state *curlCodexProfileState) error {
	if req == nil || state == nil {
		return nil
	}
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("openai-beta", "responses=experimental")
	req.Header.Set("originator", state.Originator)
	req.Header.Set("user-agent", state.UserAgent)
	req.Header.Set("session_id", isolateOpenAISessionID(apiKeyID, state.SessionID))
	req.Header.Set("conversation_id", isolateOpenAISessionID(apiKeyID, state.SessionID))
	req.Header.Set("x-codex-turn-metadata", state.TurnMetadata)
	req.Header.Set("x-codex-window-id", state.WindowID)
	// The real Codex request carries installation_id in client_metadata and in
	// the turn-metadata JSON, not as a standalone upstream header. Remove any
	// contradictory passthrough residue supplied by the caller.
	req.Header.Del("x-codex-installation-id")
	req.Header.Del("x-codex-turn-state")
	req.Header.Del("accept-language")
	if state.ResponsesLite {
		req.Header.Set(responsesLiteHeader, "true")
	} else {
		req.Header.Del(responsesLiteHeaderKey)
	}
	if state.RemoteCompactionEnabled {
		req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	} else {
		req.Header.Del("x-codex-beta-features")
	}
	if c != nil {
		c.Header(curlCodexResponseSessionHeader, state.SessionID)
		c.Header(curlCodexResponseThreadHeader, state.ThreadID)
		c.Header(curlCodexResponseTurnHeader, state.TurnID)
	}
	return validateCurlCodexProfileHeaders(req.Header, apiKeyID, state)
}

func validateCurlCodexProfileHeaders(headers http.Header, apiKeyID int64, state *curlCodexProfileState) error {
	isolated := isolateOpenAISessionID(apiKeyID, state.SessionID)
	checks := map[string]string{
		"accept": "text/event-stream", "originator": state.Originator,
		"user-agent": state.UserAgent, "openai-beta": "responses=experimental",
		"session_id": isolated, "conversation_id": isolated,
		"x-codex-turn-metadata": state.TurnMetadata, "x-codex-window-id": state.WindowID,
	}
	for key, expected := range checks {
		if actual := strings.TrimSpace(headers.Get(key)); actual != expected {
			return fmt.Errorf("curl Codex profile invariant: header %s mismatch", key)
		}
	}
	if state.RemoteCompactionEnabled != (headers.Get("x-codex-beta-features") == "remote_compaction_v2") {
		return fmt.Errorf("curl Codex profile invariant: remote compaction capability mismatch")
	}
	if state.ResponsesLite != isOpenAIResponsesLiteHeader(headers.Get(responsesLiteHeader)) {
		return fmt.Errorf("curl Codex profile invariant: Responses Lite header mismatch")
	}
	return nil
}
