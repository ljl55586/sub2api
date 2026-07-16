package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
)

const claudeOAuthNoToolsMainExpansion = "You are an interactive assistant that helps users with software engineering tasks. " +
	"Follow the conversation instructions, give concise and accurate answers, " +
	"and do not claim that you can run tools or take actions outside this conversation."

const claudeOAuthNoToolsMainCacheControl = `{"type":"ephemeral","ttl":"` + cacheTTLTarget1h + `"}`

// isClaudeOAuthNoToolsMainCandidate identifies the narrow native OAuth request
// shape whose default Claude Code system blocks can safely be specialized for a
// no-tools conversation. It deliberately inspects only request shape and never
// changes the incoming tool definitions.
func isClaudeOAuthNoToolsMainCandidate(body []byte, modelID string) bool {
	if claude.NormalizeModelID(modelID) != "claude-opus-4-8" ||
		!gjson.GetBytes(body, "stream").Bool() ||
		gjson.GetBytes(body, "tool_choice").Exists() {
		return false
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && (!tools.IsArray() || len(tools.Array()) != 0) {
		return false
	}

	return !messagesContainToolUseOrResult(body)
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
		} else if content.Type == gjson.JSON {
			if isClaudeOAuthToolHistoryBlockType(content.Get("type").String()) {
				found = true
			}
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

// applyClaudeOAuthNoToolsMainProfile converts only the gateway-generated
// default system shape into the no-tools Opus 4.8 main-request profile. The
// preflight is intentionally strict so custom administrator prompts and
// unexpected payload shapes remain untouched.
func applyClaudeOAuthNoToolsMainProfile(body []byte, modelID string, useDefaultSystemBlocks bool) ([]byte, bool) {
	if !useDefaultSystemBlocks || !isClaudeOAuthNoToolsMainCandidate(body, modelID) {
		return body, false
	}
	if !hasDefaultClaudeOAuthSystemBlocks(body) {
		return body, false
	}

	userMessageIndex, userText, userContentIsString, ok := lastNormalUserMessage(body)
	if !ok {
		return body, false
	}

	userContent, err := json.Marshal([]map[string]any{{
		"type": "text",
		"text": userText,
		"cache_control": map[string]string{
			"type": "ephemeral",
			"ttl":  cacheTTLTarget1h,
		},
	}})
	if err != nil {
		return body, false
	}

	out := body
	if next, ok := setJSONRawBytes(out, "system.1.cache_control", []byte(claudeOAuthNoToolsMainCacheControl)); ok {
		out = next
	} else {
		return body, false
	}
	if next, ok := setJSONValueBytes(out, "system.2.text", claudeOAuthNoToolsMainExpansion); ok {
		out = next
	} else {
		return body, false
	}
	if next, ok := setJSONRawBytes(out, "system.2.cache_control", []byte(claudeOAuthNoToolsMainCacheControl)); ok {
		out = next
	} else {
		return body, false
	}

	userContentPath := fmt.Sprintf("messages.%d.content", userMessageIndex)
	if userContentIsString {
		if next, ok := setJSONRawBytes(out, userContentPath, userContent); ok {
			out = next
		} else {
			return body, false
		}
	} else if next, ok := setJSONRawBytes(out, fmt.Sprintf("%s.0.cache_control", userContentPath), []byte(claudeOAuthNoToolsMainCacheControl)); ok {
		out = next
	} else {
		return body, false
	}

	return out, true
}

func hasDefaultClaudeOAuthSystemBlocks(body []byte) bool {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		return false
	}
	blocks := system.Array()
	if len(blocks) != 3 {
		return false
	}

	billing := blocks[0]
	billingText := billing.Get("text").String()
	if billing.Get("type").String() != "text" ||
		!strings.Contains(billingText, "x-anthropic-billing-header:") ||
		!strings.Contains(billingText, "cc_version=") ||
		!strings.Contains(billingText, "cc_entrypoint=cli;") ||
		strings.Contains(billingText, "cch=") ||
		billing.Get("cache_control").Exists() {
		return false
	}

	identity := blocks[1]
	if identity.Get("type").String() != "text" ||
		identity.Get("text").String() != claudeCodeSystemPrompt ||
		identity.Get("cache_control").Exists() {
		return false
	}

	expansion := blocks[2]
	return expansion.Get("type").String() == "text" &&
		expansion.Get("text").String() == claudeCodeSystemPromptExpansion &&
		expansion.Get("cache_control.type").String() == "ephemeral" &&
		expansion.Get("cache_control.ttl").String() == claude.DefaultCacheControlTTL
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
		return index, content.String(), true, true
	}
	if !content.IsArray() || len(content.Array()) != 1 {
		return 0, "", false, false
	}
	block := content.Array()[0]
	if block.Get("type").String() != "text" || block.Get("text").Type != gjson.String {
		return 0, "", false, false
	}
	return index, block.Get("text").String(), false, true
}
