package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	tokenCountProbeEstimatorVersion      = "o200k-anthropic-v2"
	tokenCountProbeToolBaseOverhead      = 96
	tokenCountProbeToolPerDefinitionCost = 8
	tokenCountProbeSessionCorrelationTTL = 5 * time.Second
	tokenCountProbeMaxCorrelatedSessions = 4096
)

var tokenCountProbeStaticPrefixes = []string{
	"When referencing files in your responses",
	"# Communicating with the user",
	"# Environment",
	"This is the git status at the start of the conversation.",
	"# Memory",
	"When you use a pronoun for someone",
	"# Session-specific guidance",
	"This iteration of Claude is",
}

type tokenCountProbeDecision struct {
	Matched   bool
	Candidate bool
	Reason    string
	SessionID string
}

type tokenCountProbeWireRequest struct {
	MaxTokens int  `json:"max_tokens"`
	Stream    bool `json:"stream"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Tools []json.RawMessage `json:"tools"`
}

type tokenCountProbeCacheEntry struct {
	inputTokens int
	expiresAt   time.Time
}

type tokenCountProbeEstimateCache struct {
	mu       sync.Mutex
	entries  map[string]tokenCountProbeCacheEntry
	sessions map[string]time.Time
}

func newTokenCountProbeEstimateCache() *tokenCountProbeEstimateCache {
	return &tokenCountProbeEstimateCache{
		entries:  make(map[string]tokenCountProbeCacheEntry),
		sessions: make(map[string]time.Time),
	}
}

func (c *tokenCountProbeEstimateCache) markProbeSession(key string, now time.Time) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for existingKey, expiresAt := range c.sessions {
		if !expiresAt.After(now) {
			delete(c.sessions, existingKey)
		}
	}
	if len(c.sessions) >= tokenCountProbeMaxCorrelatedSessions {
		for existingKey := range c.sessions {
			delete(c.sessions, existingKey)
			break
		}
	}
	c.sessions[key] = now.Add(tokenCountProbeSessionCorrelationTTL)
}

func (c *tokenCountProbeEstimateCache) isProbeSessionActive(key string, now time.Time) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt, ok := c.sessions[key]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(c.sessions, key)
		return false
	}
	return true
}

func (c *tokenCountProbeEstimateCache) get(key string, now time.Time) (int, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return 0, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return 0, false
	}
	return entry.inputTokens, true
}

func (c *tokenCountProbeEstimateCache) put(key string, inputTokens int, now time.Time, ttl time.Duration, maxEntries int) {
	if c == nil || key == "" || inputTokens <= 0 || ttl <= 0 || maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxEntries {
		// Probe payloads are highly repetitive. A bounded wholesale cleanup keeps
		// the hot path simple and prevents an attacker from growing this map.
		for existingKey, entry := range c.entries {
			if !entry.expiresAt.After(now) {
				delete(c.entries, existingKey)
			}
		}
		if len(c.entries) >= maxEntries {
			for existingKey := range c.entries {
				delete(c.entries, existingKey)
				break
			}
		}
	}
	c.entries[key] = tokenCountProbeCacheEntry{inputTokens: inputTokens, expiresAt: now.Add(ttl)}
}

func detectClaudeDesktopTokenCountProbe(r *http.Request, body []byte) tokenCountProbeDecision {
	if r == nil || r.URL == nil || !strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/messages") {
		return tokenCountProbeDecision{}
	}

	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	uaLower := strings.ToLower(ua)
	if !claudeCodeValidator.ValidateUserAgent(ua) ||
		!strings.Contains(uaLower, "claude-desktop-3p") ||
		!strings.Contains(uaLower, "agent-sdk/") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("X-App")), "cli") ||
		strings.TrimSpace(r.Header.Get("anthropic-version")) == "" {
		return tokenCountProbeDecision{}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return tokenCountProbeDecision{}
	}
	allowedKeys := map[string]struct{}{
		"model": {}, "max_tokens": {}, "stream": {}, "messages": {}, "metadata": {}, "tools": {},
	}
	for key := range top {
		if _, ok := allowedKeys[key]; !ok {
			return tokenCountProbeDecision{}
		}
	}
	if _, hasSystem := top["system"]; hasSystem {
		return tokenCountProbeDecision{}
	}

	var req tokenCountProbeWireRequest
	if err := json.Unmarshal(body, &req); err != nil || req.MaxTokens != 1 || req.Stream || len(req.Messages) != 1 {
		return tokenCountProbeDecision{}
	}
	if req.Messages[0].Role != "user" {
		return tokenCountProbeDecision{}
	}
	var content string
	if err := json.Unmarshal(req.Messages[0].Content, &content); err != nil {
		return tokenCountProbeDecision{}
	}
	parsedUserID := service.ParseMetadataUserID(req.Metadata.UserID)
	if parsedUserID == nil || strings.TrimSpace(parsedUserID.SessionID) == "" {
		return tokenCountProbeDecision{}
	}

	trimmed := strings.TrimSpace(content)
	if trimmed == "count" {
		return tokenCountProbeDecision{Matched: true, Candidate: true, Reason: "count", SessionID: parsedUserID.SessionID}
	}
	if len(req.Tools) != 0 {
		return tokenCountProbeDecision{Candidate: true, SessionID: parsedUserID.SessionID}
	}
	for _, prefix := range tokenCountProbeStaticPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return tokenCountProbeDecision{Matched: true, Candidate: true, Reason: tokenCountProbeReason(prefix), SessionID: parsedUserID.SessionID}
		}
	}
	return tokenCountProbeDecision{Candidate: true, SessionID: parsedUserID.SessionID}
}

func tokenCountProbeReason(prefix string) string {
	switch prefix {
	case "# Environment":
		return "environment"
	case "# Memory":
		return "memory"
	case "# Session-specific guidance":
		return "session_guidance"
	case "This is the git status at the start of the conversation.":
		return "git_status"
	case "When referencing files in your responses":
		return "response_format"
	case "# Communicating with the user":
		return "communication_guidance"
	case "When you use a pronoun for someone":
		return "pronoun_guidance"
	case "This iteration of Claude is":
		return "model_identity"
	default:
		return "static_context"
	}
}

func (h *GatewayHandler) tokenCountProbeModeForModel(model string) string {
	if h == nil || h.cfg == nil {
		return config.TokenCountProbeModeOff
	}
	cfg := h.cfg.Gateway.TokenCountProbe
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode != config.TokenCountProbeModeShadow && mode != config.TokenCountProbeModeEnforce {
		return config.TokenCountProbeModeOff
	}
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range cfg.ModelPrefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(normalizedModel, prefix) {
			return mode
		}
	}
	return config.TokenCountProbeModeOff
}

func (h *GatewayHandler) estimateTokenCountProbe(body []byte) (inputTokens int, source string, err error) {
	cfg := h.cfg.Gateway.TokenCountProbe
	key, keyErr := normalizedTokenCountProbeCacheKey(body)
	if keyErr != nil {
		return 0, "", keyErr
	}
	now := time.Now()
	if cached, ok := h.tokenCountProbeCache.get(key, now); ok {
		return applyTokenCountProbeSafetyMargin(cached, cfg.SafetyMarginPercent), "cache", nil
	}

	estimated, err := service.EstimateAnthropicInputTokens(body)
	if err != nil {
		return 0, "", err
	}
	estimated += tokenCountProbeToolFramingOverhead(body)
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	h.tokenCountProbeCache.put(key, estimated, now, ttl, cfg.CacheMaxEntries)
	return applyTokenCountProbeSafetyMargin(estimated, cfg.SafetyMarginPercent), tokenCountProbeEstimatorVersion, nil
}

func tokenCountProbeToolFramingOverhead(body []byte) int {
	var req struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Tools) == 0 {
		return 0
	}
	// Anthropic-compatible providers count a fixed tool-list envelope plus
	// per-definition framing that is not represented by the Responses bridge.
	// These conservative constants are calibrated against captured GLM usage;
	// the configurable safety margin remains the final guard against undercount.
	return tokenCountProbeToolBaseOverhead + tokenCountProbeToolPerDefinitionCost*len(req.Tools)
}

func normalizedTokenCountProbeCacheKey(body []byte) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", err
	}
	delete(fields, "metadata")
	delete(fields, "max_tokens")
	delete(fields, "stream")
	canonical, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(tokenCountProbeEstimatorVersion+":"), canonical...))
	return hex.EncodeToString(digest[:]), nil
}

func applyTokenCountProbeSafetyMargin(inputTokens, marginPercent int) int {
	if inputTokens < 1 {
		inputTokens = 1
	}
	if marginPercent < 0 {
		marginPercent = 0
	}
	if marginPercent > 100 {
		marginPercent = 100
	}
	return (inputTokens*(100+marginPercent) + 99) / 100
}

func sendTokenCountProbeResponse(c *gin.Context, model string, inputTokens int) {
	c.JSON(http.StatusOK, gin.H{
		"model":         model,
		"id":            generateRealisticMsgID(),
		"type":          "message",
		"role":          "assistant",
		"content":       []gin.H{{"type": "text", "text": "#"}},
		"stop_reason":   "max_tokens",
		"stop_sequence": nil,
		"stop_details":  nil,
		"usage": gin.H{
			"input_tokens":                inputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"output_tokens":               1,
		},
	})
}
