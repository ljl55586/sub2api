package service

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

const claudeOAuthAssistantContentContextKey = "claudeOAuthAssistantContent"

type claudeOAuthStreamContentBlock struct {
	value        map[string]any
	partialInput strings.Builder
}

// claudeOAuthStreamContentCollector reconstructs the assistant content array
// from Anthropic SSE events without changing the bytes sent to the downstream
// client. Unknown block fields from content_block_start are retained.
type claudeOAuthStreamContentCollector struct {
	blocks map[int]*claudeOAuthStreamContentBlock
}

func newClaudeOAuthStreamContentCollector() *claudeOAuthStreamContentCollector {
	return &claudeOAuthStreamContentCollector{blocks: make(map[int]*claudeOAuthStreamContentBlock)}
}

func (c *claudeOAuthStreamContentCollector) Observe(event map[string]any) {
	if c == nil || event == nil {
		return
	}
	eventType, _ := event["type"].(string)
	index, ok := sseEventIndex(event)
	switch eventType {
	case "content_block_start":
		if !ok {
			return
		}
		content, _ := event["content_block"].(map[string]any)
		if content == nil {
			return
		}
		c.blocks[index] = &claudeOAuthStreamContentBlock{value: cloneJSONMap(content)}
	case "content_block_delta":
		if !ok {
			return
		}
		block := c.blocks[index]
		if block == nil {
			block = &claudeOAuthStreamContentBlock{value: make(map[string]any)}
			c.blocks[index] = block
		}
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return
		}
		deltaType, _ := delta["type"].(string)
		switch deltaType {
		case "text_delta":
			appendJSONTextField(block.value, "text", delta["text"])
		case "thinking_delta":
			appendJSONTextField(block.value, "thinking", delta["thinking"])
		case "signature_delta":
			appendJSONTextField(block.value, "signature", delta["signature"])
		case "input_json_delta":
			if partial, ok := delta["partial_json"].(string); ok {
				block.partialInput.WriteString(partial)
			}
		case "citations_delta":
			if citation, exists := delta["citation"]; exists {
				citations, _ := block.value["citations"].([]any)
				block.value["citations"] = append(citations, cloneJSONValue(citation))
			}
		}
	case "content_block_stop":
		if !ok {
			return
		}
		block := c.blocks[index]
		if block == nil || block.partialInput.Len() == 0 {
			return
		}
		var input any
		if json.Unmarshal([]byte(block.partialInput.String()), &input) == nil {
			block.value["input"] = input
		}
	}
}

func (c *claudeOAuthStreamContentCollector) Content() json.RawMessage {
	if c == nil || len(c.blocks) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(c.blocks))
	for index := range c.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	blocks := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		if block := c.blocks[index]; block != nil && block.value != nil {
			blocks = append(blocks, block.value)
		}
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil
	}
	return encoded
}

// observeClaudeOAuthCompatStreamPayload records the original Anthropic event
// before an OpenAI protocol adapter transforms it. The boolean reports whether
// this is the terminal message_stop event, which gates transcript commits.
func observeClaudeOAuthCompatStreamPayload(collector *claudeOAuthStreamContentCollector, payload []byte) bool {
	if collector == nil {
		return false
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	collector.Observe(event)
	eventType, _ := event["type"].(string)
	return eventType == "message_stop"
}

func storeClaudeOAuthCompatAssistantContent(c *gin.Context, collector *claudeOAuthStreamContentCollector, sawMessageStop bool) {
	if c == nil || collector == nil || !sawMessageStop {
		return
	}
	if content := collector.Content(); len(content) > 0 {
		c.Set(claudeOAuthAssistantContentContextKey, append([]byte(nil), content...))
	}
}

func appendJSONTextField(target map[string]any, name string, value any) {
	text, _ := value.(string)
	if text == "" {
		return
	}
	existing, _ := target[name].(string)
	target[name] = existing + text
}

func cloneJSONMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	cloned, _ := cloneJSONValue(input).(map[string]any)
	return cloned
}

func cloneJSONValue(input any) any {
	encoded, err := json.Marshal(input)
	if err != nil {
		return input
	}
	var output any
	if json.Unmarshal(encoded, &output) != nil {
		return input
	}
	return output
}
