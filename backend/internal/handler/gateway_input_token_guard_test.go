package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func inputTokenGuardTestBody(t *testing.T, model, content string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"stream":     true,
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
	})
	require.NoError(t, err)
	return body
}

func TestEvaluateInputTokenGuard(t *testing.T) {
	h := &GatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{InputTokenGuard: config.GatewayInputTokenGuardConfig{
		Mode:             config.TokenCountProbeModeEnforce,
		ModelLimits:      []config.GatewayInputTokenGuardModelLimit{{Model: "glm-5-turbo", InputTokens: 100}},
		TolerancePercent: 5,
		MinBodyBytes:     1,
	}}}}

	result, err := h.evaluateInputTokenGuard("GLM-5-Turbo", inputTokenGuardTestBody(t, "glm-5-turbo", strings.Repeat("token ", 400)))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 100, result.Limit)
	require.Equal(t, 105, result.Threshold)
	require.Greater(t, result.EstimatedInputTokens, result.Threshold)
	require.True(t, result.Exceeded)
}

func TestEvaluateInputTokenGuardSkipsUnconfiguredOrSmallRequests(t *testing.T) {
	h := &GatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{InputTokenGuard: config.GatewayInputTokenGuardConfig{
		Mode:         config.TokenCountProbeModeEnforce,
		ModelLimits:  []config.GatewayInputTokenGuardModelLimit{{Model: "glm-5-turbo", InputTokens: 200000}},
		MinBodyBytes: 4096,
	}}}}
	body := inputTokenGuardTestBody(t, "glm-5-turbo", "hello")

	result, err := h.evaluateInputTokenGuard("glm-5-turbo", body)
	require.NoError(t, err)
	require.Nil(t, result)

	h.cfg.Gateway.InputTokenGuard.MinBodyBytes = 1
	result, err = h.evaluateInputTokenGuard("glm-5.2", body)
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestEvaluateInputTokenGuardOffFailsOpen(t *testing.T) {
	h := &GatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{InputTokenGuard: config.GatewayInputTokenGuardConfig{
		Mode:         config.TokenCountProbeModeOff,
		ModelLimits:  []config.GatewayInputTokenGuardModelLimit{{Model: "glm-5-turbo", InputTokens: 1}},
		MinBodyBytes: 1,
	}}}}

	result, err := h.evaluateInputTokenGuard("glm-5-turbo", inputTokenGuardTestBody(t, "glm-5-turbo", "hello"))
	require.NoError(t, err)
	require.Nil(t, result)
}
