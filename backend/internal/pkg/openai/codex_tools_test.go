package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddedCodexToolProfiles(t *testing.T) {
	var direct []map[string]any
	require.NoError(t, json.Unmarshal(CodexAgentToolsJSON(), &direct))
	require.Len(t, direct, 14)

	var lite []map[string]any
	require.NoError(t, json.Unmarshal(CodexResponsesLiteAgentToolsJSON(), &lite))
	require.Len(t, lite, 4)
	names := make([]string, 0, len(lite))
	for _, tool := range lite {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	require.Equal(t, []string{"exec", "wait", "request_user_input", "collaboration"}, names)
}
