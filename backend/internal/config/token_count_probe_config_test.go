package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestLoadTokenCountProbeDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, TokenCountProbeModeOff, cfg.Gateway.TokenCountProbe.Mode)
	require.Equal(t, []string{"glm-"}, cfg.Gateway.TokenCountProbe.ModelPrefixes)
	require.Equal(t, 10, cfg.Gateway.TokenCountProbe.SafetyMarginPercent)
	require.Equal(t, 3600, cfg.Gateway.TokenCountProbe.CacheTTLSeconds)
	require.Equal(t, 2048, cfg.Gateway.TokenCountProbe.CacheMaxEntries)
}

func TestLoadTokenCountProbeOverrides(t *testing.T) {
	resetViperWithJWTSecret(t)
	viper.Set("gateway.token_count_probe.mode", TokenCountProbeModeEnforce)
	viper.Set("gateway.token_count_probe.model_prefixes", []string{"glm-5-", "custom-glm"})
	viper.Set("gateway.token_count_probe.safety_margin_percent", 15)
	viper.Set("gateway.token_count_probe.cache_ttl_seconds", 120)
	viper.Set("gateway.token_count_probe.cache_max_entries", 64)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, TokenCountProbeModeEnforce, cfg.Gateway.TokenCountProbe.Mode)
	require.Equal(t, []string{"glm-5-", "custom-glm"}, cfg.Gateway.TokenCountProbe.ModelPrefixes)
	require.Equal(t, 15, cfg.Gateway.TokenCountProbe.SafetyMarginPercent)
	require.Equal(t, 120, cfg.Gateway.TokenCountProbe.CacheTTLSeconds)
	require.Equal(t, 64, cfg.Gateway.TokenCountProbe.CacheMaxEntries)
}

func TestLoadInputTokenGuardDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, TokenCountProbeModeOff, cfg.Gateway.InputTokenGuard.Mode)
	require.Empty(t, cfg.Gateway.InputTokenGuard.ModelLimits)
	require.Equal(t, 5, cfg.Gateway.InputTokenGuard.TolerancePercent)
	require.Equal(t, 262144, cfg.Gateway.InputTokenGuard.MinBodyBytes)
}

func TestLoadInputTokenGuardOverrides(t *testing.T) {
	resetViperWithJWTSecret(t)
	viper.Set("gateway.input_token_guard.mode", TokenCountProbeModeEnforce)
	viper.Set("gateway.input_token_guard.model_limits", []map[string]any{{"model": "glm-5-turbo", "input_tokens": 200000}})
	viper.Set("gateway.input_token_guard.tolerance_percent", 8)
	viper.Set("gateway.input_token_guard.min_body_bytes", 524288)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, TokenCountProbeModeEnforce, cfg.Gateway.InputTokenGuard.Mode)
	require.Equal(t, []GatewayInputTokenGuardModelLimit{{Model: "glm-5-turbo", InputTokens: 200000}}, cfg.Gateway.InputTokenGuard.ModelLimits)
	require.Equal(t, 8, cfg.Gateway.InputTokenGuard.TolerancePercent)
	require.Equal(t, 524288, cfg.Gateway.InputTokenGuard.MinBodyBytes)
}
