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

// ClaudeCodeModelCapabilities contains the model-dependent decisions made by
// Claude Code before it serializes a request. These flags must be resolved
// before the body and anthropic-beta header are built so those two surfaces
// cannot contradict one another.
type ClaudeCodeModelCapabilities struct {
	APImodelID                string
	Context1M                 bool
	SupportsContext1M         bool
	SupportsAdaptiveThinking  bool
	SupportsEffort            bool
	SupportsContextManagement bool
	SupportsMidConversation   bool
	SupportsAdvisor           bool
	TokenLimits               ClaudeCodeModelTokenLimits
}

// ClaudeCodeRequestKind identifies the 2.1.208 query lifecycle whose ordered
// beta list is being constructed.
type ClaudeCodeRequestKind string

const (
	ClaudeCodeRequestMain        ClaudeCodeRequestKind = "main"
	ClaudeCodeRequestQuota       ClaudeCodeRequestKind = "quota"
	ClaudeCodeRequestTitle       ClaudeCodeRequestKind = "title"
	ClaudeCodeRequestCountTokens ClaudeCodeRequestKind = "count_tokens"
)

// ClaudeCodeRequestFeatures are body/query-source facts that affect beta
// selection independently of the selected model.
type ClaudeCodeRequestFeatures struct {
	Context1M           bool
	Thinking            bool
	ContextManagement   bool
	Effort              bool
	StructuredOutput    bool
	ExtendedCacheTTL    bool
	MidConversationRole bool
	Advisor             bool
}

// ClaudeCodeProfile is the atomic wire profile used by OAuth mimic requests.
// Version-dependent fields live together so a future upgrade cannot silently
// combine headers, beta lists, billing attestation, and model defaults from
// different Claude Code releases.
type ClaudeCodeProfile struct {
	Version                         string
	Headers                         map[string]string
	MainBetas                       []string
	QuotaBetas                      []string
	TitleBetas                      []string
	CountTokensBetas                []string
	CCH                             ClaudeCodeCCHProfile
	DirectFirstPartyClientRequestID bool
	TitleMinUTF16CodeUnits          int
	UnknownModelTokenLimits         ClaudeCodeModelTokenLimits
}

var claudeCodeProfile21208 = ClaudeCodeProfile{
	Version: CLICurrentVersion,
	Headers: map[string]string{
		"User-Agent":                                "claude-cli/" + CLICurrentVersion + " (external, cli)",
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Package-Version":               "0.94.0",
		"X-Stainless-OS":                            DefaultStainlessOS,
		"X-Stainless-Arch":                          "arm64",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Runtime-Version":               "v26.3.0",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Timeout":                       "600",
		"X-App":                                     "cli",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
	},
	MainBetas:        ClaudeCodeOAuthBetasForRequest(ClaudeCodeRequestMain, "claude-opus-4-8[1m]", ClaudeCodeRequestFeatures{Context1M: true, Thinking: true, ContextManagement: true, Effort: true, ExtendedCacheTTL: true, MidConversationRole: true, Advisor: true}),
	QuotaBetas:       ClaudeCodeOAuthBetasForRequest(ClaudeCodeRequestQuota, "claude-haiku-4-5-20251001", ClaudeCodeRequestFeatures{}),
	TitleBetas:       ClaudeCodeOAuthBetasForRequest(ClaudeCodeRequestTitle, "claude-haiku-4-5-20251001", ClaudeCodeRequestFeatures{StructuredOutput: true}),
	CountTokensBetas: ClaudeCodeOAuthBetasForRequest(ClaudeCodeRequestCountTokens, "claude-opus-4-8[1m]", ClaudeCodeRequestFeatures{Context1M: true, ContextManagement: true, Effort: true, ExtendedCacheTTL: true}),
	CCH: ClaudeCodeCCHProfile{
		Enabled: true,
		Seed:    0x4D659218E32A3268,
	},
	// Claude Code 2.1.208 adds this only for the firstParty provider when the
	// resolved base URL is api.anthropic.com. A local trace/custom relay makes
	// the official ep() predicate false, so both this header and CCH disappear.
	DirectFirstPartyClientRequestID: true,
	TitleMinUTF16CodeUnits:          10,
	UnknownModelTokenLimits: ClaudeCodeModelTokenLimits{
		DefaultMaxTokens: 32000,
		UpperMaxTokens:   128000,
	},
}

// CurrentClaudeCodeProfile returns a defensive copy of the active profile.
func CurrentClaudeCodeProfile() ClaudeCodeProfile {
	profile := claudeCodeProfile21208
	profile.Headers = cloneStringMap(profile.Headers)
	profile.MainBetas = append([]string(nil), profile.MainBetas...)
	profile.QuotaBetas = append([]string(nil), profile.QuotaBetas...)
	profile.TitleBetas = append([]string(nil), profile.TitleBetas...)
	profile.CountTokensBetas = append([]string(nil), profile.CountTokensBetas...)
	return profile
}

// CurrentClaudeCodeModelTokenLimits resolves the 2.1.208 model table. Matching
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
		return ClaudeCodeModelTokenLimits{DefaultMaxTokens: 32000, UpperMaxTokens: 128000}
	}
}

// ResolveClaudeCodeModelCapabilities resolves the API model ID, optional [1m]
// selector and the capability gates used by Claude Code 2.1.208.
func ResolveClaudeCodeModelCapabilities(modelID string) ClaudeCodeModelCapabilities {
	raw := strings.ToLower(strings.TrimSpace(modelID))
	context1M := strings.HasSuffix(raw, "[1m]")
	apiModel := strings.TrimSpace(strings.TrimSuffix(raw, "[1m]"))
	apiModel = NormalizeModelID(apiModel)

	caps := ClaudeCodeModelCapabilities{
		APImodelID:                apiModel,
		Context1M:                 context1M,
		SupportsContextManagement: true,
		TokenLimits:               CurrentClaudeCodeModelTokenLimits(apiModel),
	}
	switch {
	case strings.Contains(apiModel, "opus-4-8"):
		caps.SupportsContext1M = true
		caps.SupportsAdaptiveThinking = true
		caps.SupportsEffort = true
		caps.SupportsMidConversation = true
		caps.SupportsAdvisor = true
	case strings.Contains(apiModel, "opus-4-7"),
		strings.Contains(apiModel, "opus-4-6"),
		strings.Contains(apiModel, "sonnet-5"),
		strings.Contains(apiModel, "sonnet-4-6"):
		caps.SupportsContext1M = true
		caps.SupportsAdaptiveThinking = true
		caps.SupportsEffort = true
		caps.SupportsAdvisor = true
	case strings.Contains(apiModel, "haiku-4-5"):
		caps.SupportsAdaptiveThinking = false
		caps.SupportsEffort = false
		caps.SupportsMidConversation = false
		caps.SupportsAdvisor = false
	default:
		caps.SupportsContextManagement = false
	}
	return caps
}

// APIModelID is the provider model ID after removing Claude Code's local [1m]
// selector and applying the normal OAuth aliases.
func (c ClaudeCodeModelCapabilities) APIModelID() string {
	return c.APImodelID
}

// ClaudeCodeOAuthBetasForRequest builds the ordered 2.1.208 beta list from
// provider/model/body facts. Callers may still apply administrator drop rules
// after this function; they must sanitize the body against the final list.
func ClaudeCodeOAuthBetasForRequest(kind ClaudeCodeRequestKind, modelID string, features ClaudeCodeRequestFeatures) []string {
	caps := ResolveClaudeCodeModelCapabilities(modelID)
	out := make([]string, 0, 12)
	add := func(token string) {
		if token == "" {
			return
		}
		for _, existing := range out {
			if existing == token {
				return
			}
		}
		out = append(out, token)
	}

	if !strings.Contains(caps.APIModelID(), "haiku") {
		add(BetaClaudeCode)
	}
	add(BetaOAuth)
	if (features.Context1M || caps.Context1M) && caps.SupportsContext1M {
		add(BetaContext1M)
	}
	add(BetaInterleavedThinking)
	add(BetaRedactThinking)
	add(BetaThinkingTokenCount)
	if caps.SupportsContextManagement {
		add(BetaContextManagement)
	}
	add(BetaPromptCachingScope)
	if features.MidConversationRole && caps.SupportsMidConversation {
		add(BetaMidConversationSystem)
	}
	if features.Advisor && caps.SupportsAdvisor {
		add(BetaAdvisorTool)
	}
	if features.Effort && caps.SupportsEffort {
		add(BetaEffort)
	}
	if features.ExtendedCacheTTL {
		add(BetaExtendedCacheTTL)
	}
	if features.StructuredOutput {
		add(BetaStructuredOutputs)
	}
	if kind == ClaudeCodeRequestCountTokens {
		add(BetaTokenCounting)
	}
	return out
}

// ClaudeCodeDirectSmallModel is the 2.1.208 first-party default selected by
// qD() for auxiliary title/quota calls when ANTHROPIC_BASE_URL is not custom.
const ClaudeCodeDirectSmallModel = "claude-haiku-4-5-20251001"

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
