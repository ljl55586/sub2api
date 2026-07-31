package service

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCode208CWDHeader       = "X-Sub2API-Claude-CWD"
	claudeCode208LanguageHeader  = "X-Sub2API-Claude-Language"
	claudeCode208Context1MHeader = "X-Sub2API-Claude-Context-1M"

	claudeCode208DefaultVirtualCWD = "/workspace"
	claudeCode208IdentityPrompt    = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeCode208Context1MKey      = "claudeCode208Context1M"
	claudeCode208SimpleProfileKey  = "claudeCode208SimpleProfile"
	claudeCode208StartedAtKey      = "claudeCode208StartedAt"

	claudeCode208ReminderPrefix = `<system-reminder>
As you answer the user's questions, you can use the following context:`
)

func isClaudeCode208DirectOAuthAccount(account *Account) bool {
	if account == nil || !account.IsOAuth() {
		return false
	}
	return !account.IsCustomBaseURLEnabled() || strings.TrimSpace(account.GetCustomBaseURL()) == ""
}

func resolveClaudeCode208Context1M(c interface{ GetHeader(string) string }, modelID string) bool {
	caps := claude.ResolveClaudeCodeModelCapabilities(modelID)
	if !caps.SupportsContext1M {
		return false
	}
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.GetHeader(claudeCode208Context1MHeader))) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	if caps.Context1M {
		return true
	}
	// The selected 2.1.208 curl profile mirrors the captured Opus 4.8 session,
	// whose local model selector was "[1m]" even though the wire model omits
	// that suffix. Callers can explicitly disable this default with the private
	// downstream-only header above.
	return strings.Contains(caps.APIModelID(), "opus-4-8")
}

type claudeCode208SessionFacts struct {
	CWD         string
	StartDate   string
	CurrentDate string
	UserEmail   string
	Language    string
}

type claudeCode208WireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeCode208Thinking struct {
	Type string `json:"type"`
}

type claudeCode208ContextManagement struct {
	Edits []claudeCode208ContextManagementEdit `json:"edits"`
}

type claudeCode208ContextManagementEdit struct {
	Type string `json:"type"`
	Keep string `json:"keep"`
}

type claudeCode208MainOutputConfig struct {
	Effort string `json:"effort"`
}

type claudeCode208MainBody struct {
	Model             string                            `json:"model"`
	Messages          []claudeCode208WireMessage        `json:"messages"`
	System            []anthropicSystemTextBlockPayload `json:"system"`
	Tools             []struct{}                        `json:"tools"`
	Metadata          anthropicMetadataPayload          `json:"metadata"`
	MaxTokens         int                               `json:"max_tokens"`
	Thinking          *claudeCode208Thinking            `json:"thinking,omitempty"`
	ContextManagement *claudeCode208ContextManagement   `json:"context_management,omitempty"`
	OutputConfig      *claudeCode208MainOutputConfig    `json:"output_config,omitempty"`
	Stream            bool                              `json:"stream"`
}

// isClaudeOAuthNoToolsMainCandidate identifies the narrow interactive request
// shape that can be represented by Claude Code 2.1.208's source-backed SIMPLE
// system prompt. Tool histories and explicit tool selection are deliberately
// excluded because SIMPLE + tools:[] must remain internally consistent.
func isClaudeOAuthNoToolsMainCandidate(body []byte, modelID string) bool {
	if strings.TrimSpace(claude.ResolveClaudeCodeModelCapabilities(modelID).APIModelID()) == "" ||
		!gjson.GetBytes(body, "stream").Bool() ||
		gjson.GetBytes(body, "tool_choice").Exists() {
		return false
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && (!tools.IsArray() || len(tools.Array()) != 0) {
		return false
	}
	if messagesContainToolUseOrResult(body) {
		return false
	}
	_, _, _, ok := lastNormalUserMessage(body)
	return ok
}

func messagesContainToolUseOrResult(body []byte) bool {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return false
	}

	found := false
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				if isClaudeOAuthToolHistoryBlockType(block.Get("type").String()) {
					found = true
					return false
				}
				return true
			})
		} else if content.Type == gjson.JSON && isClaudeOAuthToolHistoryBlockType(content.Get("type").String()) {
			found = true
		}
		return !found
	})
	return found
}

func isClaudeOAuthToolHistoryBlockType(blockType string) bool {
	switch blockType {
	case "tool_use", "tool_result", "server_tool_use", "web_search_tool_result", "tool_use_result":
		return true
	default:
		return false
	}
}

func resolveClaudeCode208SessionFacts(c interface{ GetHeader(string) string }, account *Account, startedAt time.Time) claudeCode208SessionFacts {
	now := time.Now()
	if startedAt.IsZero() {
		startedAt = now
	}

	cwd := ""
	language := ""
	if c != nil {
		cwd = strings.TrimSpace(c.GetHeader(claudeCode208CWDHeader))
		language = strings.TrimSpace(c.GetHeader(claudeCode208LanguageHeader))
	}
	if cwd == "" && account != nil {
		cwd = strings.TrimSpace(account.GetExtraString("claude_cwd"))
	}
	cwd = sanitizeClaudeCode208Fact(cwd)
	if cwd == "" {
		cwd = claudeCode208DefaultVirtualCWD
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Clean("/" + cwd)
	}

	email := ""
	if account != nil {
		email = strings.TrimSpace(account.GetCredential("email"))
		if email == "" {
			email = strings.TrimSpace(account.GetExtraString("email"))
		}
	}

	return claudeCode208SessionFacts{
		CWD:         cwd,
		StartDate:   startedAt.Format("2006-01-02"),
		CurrentDate: now.Format("2006-01-02"),
		UserEmail:   sanitizeClaudeCode208Fact(email),
		Language:    sanitizeClaudeCode208Fact(language),
	}
}

func sanitizeClaudeCode208Fact(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func buildClaudeCode208SystemReminder(facts claudeCode208SessionFacts) string {
	var context strings.Builder
	if facts.UserEmail != "" {
		context.WriteString("# userEmail\n")
		context.WriteString("The user's email address is ")
		context.WriteString(facts.UserEmail)
		context.WriteString(".\n")
	}
	context.WriteString("# currentDate\n")
	context.WriteString("Today's date is ")
	context.WriteString(facts.CurrentDate)
	context.WriteString(".")

	return claudeCode208ReminderPrefix + "\n" + context.String() + `

      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>

`
}

func isClaudeCode208GeneratedReminder(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, strings.TrimSpace(claudeCode208ReminderPrefix)) &&
		strings.Contains(trimmed, "IMPORTANT: this context may or may not be relevant to your tasks.")
}

func isClaudeCode208MetaText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return isClaudeCode208GeneratedReminder(trimmed) ||
		strings.HasPrefix(trimmed, "<local-command-caveat>") ||
		strings.HasPrefix(trimmed, "<command-name>") ||
		strings.HasPrefix(trimmed, "<command-message>") ||
		strings.HasPrefix(trimmed, "<command-args>") ||
		strings.HasPrefix(trimmed, "<local-command-stdout>")
}

func parseClaudeCode208Messages(body []byte) ([]claudeCode208WireMessage, error) {
	raw := gjson.GetBytes(body, "messages")
	if !raw.IsArray() {
		return nil, nil
	}
	var messages []claudeCode208WireMessage
	if err := json.Unmarshal([]byte(raw.Raw), &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func claudeCode208ContentBlocks(content json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, err
		}
		block, err := json.Marshal(claudeOAuthCompanionTextBlock{Type: "text", Text: text})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{block}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func marshalClaudeCode208ContentBlocks(blocks []json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(blocks)
	return json.RawMessage(encoded), err
}

func stripClaudeCode208BlockCacheControl(block json.RawMessage) json.RawMessage {
	if !gjson.GetBytes(block, "cache_control").Exists() {
		return append(json.RawMessage(nil), block...)
	}
	next, err := sjson.DeleteBytes(block, "cache_control")
	if err != nil {
		return append(json.RawMessage(nil), block...)
	}
	return json.RawMessage(next)
}

func addClaudeCode208CacheControl(block json.RawMessage) (json.RawMessage, error) {
	text := gjson.GetBytes(block, "text")
	if gjson.GetBytes(block, "type").String() != "text" || text.Type != gjson.String {
		return block, nil
	}
	return json.Marshal(struct {
		Type         string                       `json:"type"`
		Text         string                       `json:"text"`
		CacheControl anthropicCacheControlPayload `json:"cache_control"`
	}{
		Type: "text",
		Text: text.String(),
		CacheControl: anthropicCacheControlPayload{
			Type: "ephemeral",
			TTL:  cacheTTLTarget1h,
		},
	})
}

func normalizeClaudeCode208Messages(messages []claudeCode208WireMessage, reminder string) ([]claudeCode208WireMessage, error) {
	firstHumanMessage := -1
	lastHumanMessage := -1
	lastHumanBlock := -1

	for messageIndex := range messages {
		blocks, err := claudeCode208ContentBlocks(messages[messageIndex].Content)
		if err != nil {
			return nil, err
		}
		nextBlocks := make([]json.RawMessage, 0, len(blocks))
		for _, rawBlock := range blocks {
			block := stripClaudeCode208BlockCacheControl(rawBlock)
			text := gjson.GetBytes(block, "text")
			if gjson.GetBytes(block, "type").String() == "text" && text.Type == gjson.String {
				if isClaudeCode208GeneratedReminder(text.String()) {
					continue
				}
				if messages[messageIndex].Role == "user" && !isClaudeCode208MetaText(text.String()) && strings.TrimSpace(text.String()) != "" {
					if firstHumanMessage < 0 {
						firstHumanMessage = messageIndex
					}
					lastHumanMessage = messageIndex
					lastHumanBlock = len(nextBlocks)
				}
			}
			nextBlocks = append(nextBlocks, block)
		}
		content, err := marshalClaudeCode208ContentBlocks(nextBlocks)
		if err != nil {
			return nil, err
		}
		messages[messageIndex].Content = content
	}

	if firstHumanMessage < 0 || lastHumanMessage < 0 || lastHumanBlock < 0 {
		return nil, nil
	}
	reminderBlock, err := json.Marshal(claudeOAuthCompanionTextBlock{Type: "text", Text: reminder})
	if err != nil {
		return nil, err
	}
	firstBlocks, err := claudeCode208ContentBlocks(messages[firstHumanMessage].Content)
	if err != nil {
		return nil, err
	}
	firstBlocks = append([]json.RawMessage{reminderBlock}, firstBlocks...)
	if firstHumanMessage == lastHumanMessage {
		lastHumanBlock++
	}
	messages[firstHumanMessage].Content, err = marshalClaudeCode208ContentBlocks(firstBlocks)
	if err != nil {
		return nil, err
	}

	lastBlocks, err := claudeCode208ContentBlocks(messages[lastHumanMessage].Content)
	if err != nil {
		return nil, err
	}
	if lastHumanBlock >= len(lastBlocks) {
		return nil, nil
	}
	lastBlocks[lastHumanBlock], err = addClaudeCode208CacheControl(lastBlocks[lastHumanBlock])
	if err != nil {
		return nil, err
	}
	messages[lastHumanMessage].Content, err = marshalClaudeCode208ContentBlocks(lastBlocks)
	if err != nil {
		return nil, err
	}
	return messages, nil
}

func claudeCode208MaxTokens(body []byte, limits claude.ClaudeCodeModelTokenLimits) int {
	maxTokens := int(gjson.GetBytes(body, "max_tokens").Int())
	if maxTokens <= 0 {
		maxTokens = limits.DefaultMaxTokens
	}
	if maxTokens > limits.UpperMaxTokens {
		maxTokens = limits.UpperMaxTokens
	}
	return maxTokens
}

// applyClaudeOAuthNoToolsMainProfile is retained as a pure compatibility
// wrapper for unit tests and non-handler callers. Production paths should pass
// resolved session facts through applyClaudeOAuthNoToolsMainProfileWithFacts.
func applyClaudeOAuthNoToolsMainProfile(body []byte, modelID string, useDefaultSystemBlocks bool) ([]byte, bool) {
	facts := resolveClaudeCode208SessionFacts(nil, nil, time.Now())
	return applyClaudeOAuthNoToolsMainProfileWithFacts(body, modelID, useDefaultSystemBlocks, facts)
}

func applyClaudeOAuthNoToolsMainProfileWithFacts(
	body []byte,
	modelID string,
	useDefaultSystemBlocks bool,
	facts claudeCode208SessionFacts,
) ([]byte, bool) {
	if !useDefaultSystemBlocks || !isClaudeOAuthNoToolsMainCandidate(body, modelID) {
		return body, false
	}

	metadataUserID := gjson.GetBytes(body, "metadata.user_id")
	if metadataUserID.Type != gjson.String || ParseMetadataUserID(metadataUserID.String()) == nil {
		return body, false
	}
	messages, err := parseClaudeCode208Messages(body)
	if err != nil {
		return body, false
	}
	messages, err = normalizeClaudeCode208Messages(messages, buildClaudeCode208SystemReminder(facts))
	if err != nil || len(messages) == 0 {
		return body, false
	}

	fingerprintBody, err := json.Marshal(struct {
		Messages []claudeCode208WireMessage `json:"messages"`
	}{Messages: messages})
	if err != nil {
		return body, false
	}
	billingText, err := buildBillingAttributionText(fingerprintBody, claude.CLICurrentVersion)
	if err != nil {
		return body, false
	}

	caps := claude.ResolveClaudeCodeModelCapabilities(modelID)
	if caps.APIModelID() == "" {
		return body, false
	}
	cacheControl := &anthropicCacheControlPayload{Type: "ephemeral", TTL: cacheTTLTarget1h}
	system := []anthropicSystemTextBlockPayload{
		{Type: "text", Text: billingText},
		{Type: "text", Text: claudeCode208IdentityPrompt, CacheControl: cacheControl},
		{
			Type: "text",
			Text: "CWD: " + facts.CWD + "\nDate: " + facts.StartDate,
			CacheControl: &anthropicCacheControlPayload{
				Type: "ephemeral",
				TTL:  cacheTTLTarget1h,
			},
		},
	}

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	var thinking *claudeCode208Thinking
	var contextManagement *claudeCode208ContextManagement
	if thinkingType == "disabled" {
		thinking = &claudeCode208Thinking{Type: "disabled"}
	} else if caps.SupportsAdaptiveThinking {
		thinking = &claudeCode208Thinking{Type: "adaptive"}
		if caps.SupportsContextManagement {
			contextManagement = &claudeCode208ContextManagement{Edits: []claudeCode208ContextManagementEdit{{
				Type: "clear_thinking_20251015",
				Keep: "all",
			}}}
		}
	}

	var outputConfig *claudeCode208MainOutputConfig
	if caps.SupportsEffort {
		outputConfig = &claudeCode208MainOutputConfig{Effort: "high"}
	}

	finalBody := claudeCode208MainBody{
		Model:             caps.APIModelID(),
		Messages:          messages,
		System:            system,
		Tools:             []struct{}{},
		Metadata:          anthropicMetadataPayload{UserID: metadataUserID.String()},
		MaxTokens:         claudeCode208MaxTokens(body, caps.TokenLimits),
		Thinking:          thinking,
		ContextManagement: contextManagement,
		OutputConfig:      outputConfig,
		Stream:            true,
	}
	encoded, err := json.Marshal(finalBody)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func lastNormalUserMessage(body []byte) (index int, text string, contentIsString bool, ok bool) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return 0, "", false, false
	}
	items := messages.Array()
	if len(items) == 0 {
		return 0, "", false, false
	}

	index = len(items) - 1
	message := items[index]
	if message.Get("role").String() != "user" {
		return 0, "", false, false
	}
	content := message.Get("content")
	if content.Type == gjson.String {
		if isClaudeCode208MetaText(content.String()) || strings.TrimSpace(content.String()) == "" {
			return 0, "", false, false
		}
		return index, content.String(), true, true
	}
	if !content.IsArray() {
		return 0, "", false, false
	}
	for _, block := range content.Array() {
		if block.Get("type").String() != "text" || block.Get("text").Type != gjson.String {
			continue
		}
		candidate := block.Get("text").String()
		if isClaudeCode208MetaText(candidate) || strings.TrimSpace(candidate) == "" {
			continue
		}
		return index, candidate, false, true
	}
	return 0, "", false, false
}
