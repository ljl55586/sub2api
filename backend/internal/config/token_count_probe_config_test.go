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
