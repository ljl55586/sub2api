package claude

import "strings"

// ClaudeCodeCCHProfile describes the native client-attestation token embedded
// in the billing system block. The token is computed from the exact finalized
// request body bytes while the five-character value is still "00000".
type ClaudeCodeCCHProfile struct {
	Enabled bool
	Seed    uint64
}

// ClaudeCodeModelTokenLimits keeps the normal output-token default separate
// from the model's hard upper limit. Claude Code uses the default when no
// caller/env/retry override is present and clamps overrides to the upper limit.
type ClaudeCodeModelTokenLimits struct {
	DefaultMaxTokens int
	UpperMaxTokens   int
}

// ClaudeCodeProfile is the atomic wire profile used by OAuth mimic requests.
// Version-dependent fields live together so a future upgrade cannot silently
// combine headers, beta lists, billing attestation, and model defaults from
// different Claude Code releases.
type ClaudeCodeProfile struct {
	Version                 string
	Headers                 map[string]string
	MainBetas               []string
	QuotaBetas              []string
	TitleBetas              []string
	CountTokensBetas        []string
	CCH                     ClaudeCodeCCHProfile
	TitleMinUTF16CodeUnits  int
	UnknownModelTokenLimits ClaudeCodeModelTokenLimits
}

var claudeCodeProfile21161 = ClaudeCodeProfile{
	Version: CLICurrentVersion,
	Headers: map[string]string{
		"User-Agent":                                "claude-cli/" + CLICurrentVersion + " (external, cli)",
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Package-Version":               "0.94.0",
		"X-Stainless-OS":                            DefaultStainlessOS,
		"X-Stainless-Arch":                          "arm64",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Runtime-Version":               "v24.3.0",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Timeout":                       "600",
		"X-App":                                     "cli",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
	},
	MainBetas: []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaContext1M,
		BetaInterleavedThinking,
		BetaRedactThinking,
		BetaThinkingTokenCount,
		BetaContextManagement,
		BetaPromptCachingScope,
		BetaMidConversationSystem,
		BetaAdvisorTool,
		BetaEffort,
		BetaExtendedCacheTTL,
	},
	QuotaBetas: []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaInterleavedThinking,
		BetaRedactThinking,
		BetaThinkingTokenCount,
		BetaContextManagement,
		BetaPromptCachingScope,
	},
	TitleBetas: []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaContext1M,
		BetaInterleavedThinking,
		BetaRedactThinking,
		BetaThinkingTokenCount,
		BetaContextManagement,
		BetaPromptCachingScope,
		BetaMidConversationSystem,
		BetaAdvisorTool,
		BetaEffort,
		BetaStructuredOutputs,
	},
	CountTokensBetas: []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaInterleavedThinking,
		BetaPromptCachingScope,
		BetaEffort,
		BetaContextManagement,
		BetaExtendedCacheTTL,
	},
	CCH: ClaudeCodeCCHProfile{
		Enabled: true,
		Seed:    0x4D659218E32A3268,
	},
	TitleMinUTF16CodeUnits: 10,
	UnknownModelTokenLimits: ClaudeCodeModelTokenLimits{
		DefaultMaxTokens: 32000,
		UpperMaxTokens:   128000,
	},
}

// CurrentClaudeCodeProfile returns a defensive copy of the active profile.
func CurrentClaudeCodeProfile() ClaudeCodeProfile {
	profile := claudeCodeProfile21161
	profile.Headers = cloneStringMap(profile.Headers)
	profile.MainBetas = append([]string(nil), profile.MainBetas...)
	profile.QuotaBetas = append([]string(nil), profile.QuotaBetas...)
	profile.TitleBetas = append([]string(nil), profile.TitleBetas...)
	profile.CountTokensBetas = append([]string(nil), profile.CountTokensBetas...)
	return profile
}

// CurrentClaudeCodeModelTokenLimits resolves the 2.1.161 model table. Matching
// is intentionally ordered from the most specific release to broader model
// families so aliases and dated model IDs receive the same limits.
func CurrentClaudeCodeModelTokenLimits(modelID string) ClaudeCodeModelTokenLimits {
	model := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(model, "opus-4-8"),
		strings.Contains(model, "opus-4-7"),
		strings.Contains(model, "opus-4-6"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 64000, UpperMaxTokens: 128000}
	case strings.Contains(model, "sonnet-4-6"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 32000, UpperMaxTokens: 128000}
	case strings.Contains(model, "opus-4-5"),
		strings.Contains(model, "sonnet-4-5"),
		strings.Contains(model, "haiku-4-5"),
		strings.Contains(model, "sonnet-4-"),
		strings.HasSuffix(model, "sonnet-4"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 32000, UpperMaxTokens: 64000}
	case strings.Contains(model, "opus-4-1"),
		strings.Contains(model, "opus-4-"),
		strings.HasSuffix(model, "opus-4"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 32000, UpperMaxTokens: 32000}
	case strings.Contains(model, "3-7-sonnet"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 32000, UpperMaxTokens: 64000}
	case strings.Contains(model, "3-5-sonnet"),
		strings.Contains(model, "3-5-haiku"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 8192, UpperMaxTokens: 8192}
	case strings.Contains(model, "claude-3-opus"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 4096, UpperMaxTokens: 4096}
	case strings.Contains(model, "claude-3-sonnet"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 8192, UpperMaxTokens: 8192}
	case strings.Contains(model, "claude-3-haiku"):
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 4096, UpperMaxTokens: 4096}
	default:
		return claudeCodeProfile21161.UnknownModelTokenLimits
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
