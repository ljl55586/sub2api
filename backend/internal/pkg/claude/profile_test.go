package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentClaudeCodeProfile_21161IsAtomic(t *testing.T) {
	profile := CurrentClaudeCodeProfile()

	require.Equal(t, "2.1.161", profile.Version)
	require.Equal(t, "claude-cli/2.1.161 (external, cli)", profile.Headers["User-Agent"])
	require.Equal(t, "v24.3.0", profile.Headers["X-Stainless-Runtime-Version"])
	require.Equal(t, "MacOS", profile.Headers["X-Stainless-OS"])
	require.Equal(t, uint64(0x4D659218E32A3268), profile.CCH.Seed)
	require.True(t, profile.CCH.Enabled)
	require.Equal(t, 10, profile.TitleMinUTF16CodeUnits)
	require.Equal(t, ClaudeCodeOAuthMainMimicryBetas(), profile.MainBetas)
	require.Equal(t, ClaudeCodeOAuthQuotaMimicryBetas(), profile.QuotaBetas)
	require.Equal(t, ClaudeCodeOAuthTitleMimicryBetas(), profile.TitleBetas)
}

func TestCurrentClaudeCodeProfile_ReturnsDefensiveCopies(t *testing.T) {
	first := CurrentClaudeCodeProfile()
	first.Headers["User-Agent"] = "mutated"
	first.MainBetas[0] = "mutated"

	second := CurrentClaudeCodeProfile()
	require.Equal(t, "claude-cli/2.1.161 (external, cli)", second.Headers["User-Agent"])
	require.Equal(t, BetaClaudeCode, second.MainBetas[0])
}

func TestCurrentClaudeCodeModelTokenLimits(t *testing.T) {
	tests := []struct {
		model string
		want  ClaudeCodeModelTokenLimits
	}{
		{"claude-opus-4-8", ClaudeCodeModelTokenLimits{64000, 128000}},
		{"claude-opus-4-7", ClaudeCodeModelTokenLimits{64000, 128000}},
		{"claude-sonnet-4-6", ClaudeCodeModelTokenLimits{32000, 128000}},
		{"claude-opus-4-6", ClaudeCodeModelTokenLimits{64000, 128000}},
		{"claude-opus-4-5-20251101", ClaudeCodeModelTokenLimits{32000, 64000}},
		{"claude-sonnet-4-20250514", ClaudeCodeModelTokenLimits{32000, 64000}},
		{"claude-haiku-4-5-20251001", ClaudeCodeModelTokenLimits{32000, 64000}},
		{"claude-opus-4-1", ClaudeCodeModelTokenLimits{32000, 32000}},
		{"claude-3-opus-20240229", ClaudeCodeModelTokenLimits{4096, 4096}},
		{"claude-3-sonnet-20240229", ClaudeCodeModelTokenLimits{8192, 8192}},
		{"claude-3-haiku-20240307", ClaudeCodeModelTokenLimits{4096, 4096}},
		{"claude-3-5-sonnet-20241022", ClaudeCodeModelTokenLimits{8192, 8192}},
		{"claude-3-5-haiku-20241022", ClaudeCodeModelTokenLimits{8192, 8192}},
		{"claude-3-7-sonnet-20250219", ClaudeCodeModelTokenLimits{32000, 64000}},
		{"future-unknown-model", ClaudeCodeModelTokenLimits{32000, 128000}},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, CurrentClaudeCodeModelTokenLimits(tt.model))
		})
	}
}
