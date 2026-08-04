package openai

import _ "embed"

// codexTools0145 is the direct-tool schema captured from Codex CLI 0.145.0.
// It is used by models such as GPT-5.5 that advertise tools at the top level.
//
//go:embed codex_tools_0_145.json
var codexTools0145 []byte

// codexResponsesLiteTools0145 is the outer Code Mode tool schema used by
// Responses Lite models. These tools are carried by input.additional_tools,
// not by the top-level tools property.
//
//go:embed codex_responses_lite_tools_0_145.json
var codexResponsesLiteTools0145 []byte

// CodexAgentToolsJSON returns a copy of the embedded direct agent tool schema.
// Keep this name for callers that predate the model-aware curl profile.
func CodexAgentToolsJSON() []byte {
	return append([]byte(nil), codexTools0145...)
}

// CodexResponsesLiteAgentToolsJSON returns a copy of the embedded Responses
// Lite outer tool schema.
func CodexResponsesLiteAgentToolsJSON() []byte {
	return append([]byte(nil), codexResponsesLiteTools0145...)
}
