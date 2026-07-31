package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	claudeOAuthRuntimeSchemaVersion = 1
	claudeOAuthRuntimeTTL           = 30 * time.Hour
	claudeOAuthRuntimeTurnLease     = 20 * time.Minute
	claudeOAuthRuntimeQuotaLease    = 15 * time.Second
	claudeOAuthRuntimeTitleLease    = 30 * time.Second
	claudeOAuthRuntimeMaxBytes      = 8 * 1024 * 1024

	claudeOAuthRuntimeActionIdle            = "idle"
	claudeOAuthRuntimeActionPending         = "pending"
	claudeOAuthRuntimeActionCompleted       = "completed"
	claudeOAuthRuntimeActionGenerated       = "generated"
	claudeOAuthRuntimeActionRetryableFailed = "retryable_failed"
)

var errClaudeOAuthRuntimeCASConflict = errors.New("Claude OAuth runtime CAS conflict")
var errClaudeOAuthRuntimeTurnBusy = errors.New("Claude OAuth session already has an active main turn")

type ClaudeOAuthRuntimeAction struct {
	State          string            `json:"state"`
	Attempts       int               `json:"attempts,omitempty"`
	ClaimID        string            `json:"claim_id,omitempty"`
	LeaseUntilUnix int64             `json:"lease_until_unix,omitempty"`
	StatusCode     int               `json:"status_code,omitempty"`
	Value          string            `json:"value,omitempty"`
	ResponseHeader map[string]string `json:"response_headers,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	UpdatedAtUnix  int64             `json:"updated_at_unix,omitempty"`
}

// ClaudeOAuthSessionRuntime is the server-side lifecycle for one downstream
// conversation. SessionID is random and stable for the runtime lifetime;
// Version is advanced by every compare-and-swap update.
type ClaudeOAuthSessionRuntime struct {
	SchemaVersion int                      `json:"schema_version"`
	Version       int64                    `json:"version"`
	SessionID     string                   `json:"session_id"`
	DeviceID      string                   `json:"device_id,omitempty"`
	Quota         ClaudeOAuthRuntimeAction `json:"quota"`
	Title         ClaudeOAuthRuntimeAction `json:"title"`
	Suggestion    ClaudeOAuthRuntimeAction `json:"suggestion"`
	Transcript    json.RawMessage          `json:"transcript,omitempty"`
	ActiveTurnID  string                   `json:"active_turn_id,omitempty"`
	TurnLeaseUnix int64                    `json:"turn_lease_until_unix,omitempty"`
	CreatedAtUnix int64                    `json:"created_at_unix"`
	UpdatedAtUnix int64                    `json:"updated_at_unix"`
}

// ClaudeOAuthRuntimeStore keeps the runtime opaque to Redis except for the
// monotonic Version used by CompareAndSwap.
type ClaudeOAuthRuntimeStore interface {
	GetOrCreateClaudeOAuthRuntime(ctx context.Context, accountID int64, runtimeKey string, candidate *ClaudeOAuthSessionRuntime, ttl time.Duration) (*ClaudeOAuthSessionRuntime, bool, error)
	GetClaudeOAuthRuntime(ctx context.Context, accountID int64, runtimeKey string, ttl time.Duration) (*ClaudeOAuthSessionRuntime, error)
	CompareAndSwapClaudeOAuthRuntime(ctx context.Context, accountID int64, runtimeKey string, expectedVersion int64, next *ClaudeOAuthSessionRuntime, ttl time.Duration) (bool, error)
}

type claudeOAuthRuntimeTurn struct {
	RuntimeKey      string
	SessionID       string
	StartedAtUnix   int64
	TurnID          string
	Claimed         bool
	RequestMessages json.RawMessage
}

func newClaudeOAuthSessionRuntime(deviceID string) *ClaudeOAuthSessionRuntime {
	now := time.Now().Unix()
	return &ClaudeOAuthSessionRuntime{
		SchemaVersion: claudeOAuthRuntimeSchemaVersion,
		Version:       1,
		SessionID:     uuid.NewString(),
		DeviceID:      strings.TrimSpace(deviceID),
		Quota:         ClaudeOAuthRuntimeAction{State: claudeOAuthRuntimeActionIdle},
		Title:         ClaudeOAuthRuntimeAction{State: claudeOAuthRuntimeActionIdle},
		Suggestion:    ClaudeOAuthRuntimeAction{State: claudeOAuthRuntimeActionIdle},
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
	}
}

func (s *GatewayService) claudeOAuthRuntimeStore() ClaudeOAuthRuntimeStore {
	if s == nil || s.cache == nil {
		return nil
	}
	store, _ := s.cache.(ClaudeOAuthRuntimeStore)
	return store
}

func resolveClaudeOAuthRuntimeKey(c *ginContextView, parsed *ParsedRequest, body []byte) string {
	// This helper accepts the narrow view used below so tests can resolve keys
	// without constructing a complete gin engine.
	var explicit string
	if parsed != nil {
		if metadata := ParseMetadataUserID(parsed.MetadataUserID); metadata != nil {
			explicit = strings.TrimSpace(metadata.SessionID)
		}
	}
	if explicit == "" && c != nil {
		for _, name := range []string{
			"X-Claude-Code-Session-Id",
			"session_id",
			"conversation_id",
		} {
			if explicit = strings.TrimSpace(c.Header(name)); explicit != "" {
				break
			}
		}
	}
	if explicit == "" {
		explicit = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	hasExplicitSession := explicit != ""

	contextDiscriminator := ""
	if parsed != nil {
		contextDiscriminator = sessionContextDiscriminator(parsed.SessionContext)
	}
	if explicit == "" {
		firstUserText := extractFirstUserText(body)
		if contextDiscriminator == "" && strings.TrimSpace(firstUserText) == "" {
			return ""
		}
		explicit = contextDiscriminator + "\x00" + firstUserText
	}

	apiKeyID := int64(0)
	if parsed != nil && parsed.SessionContext != nil {
		apiKeyID = parsed.SessionContext.APIKeyID
	}
	source := "fallback"
	if hasExplicitSession {
		source = "explicit"
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", apiKeyID, source, explicit)))
	return hex.EncodeToString(sum[:])
}

// ginContextView deliberately exposes only header reads. It avoids coupling
// runtime key tests and helpers to the rest of gin.Context.
type ginContextView struct {
	header func(string) string
}

func (v *ginContextView) Header(name string) string {
	if v == nil || v.header == nil {
		return ""
	}
	return v.header(name)
}

func newGinContextView(c interface{ GetHeader(string) string }) *ginContextView {
	if c == nil {
		return nil
	}
	return &ginContextView{header: c.GetHeader}
}

func (s *GatewayService) prepareClaudeOAuthRuntime(
	ctx context.Context,
	c interface{ GetHeader(string) string },
	parsed *ParsedRequest,
	accountID int64,
	deviceID string,
	body []byte,
) (*claudeOAuthRuntimeTurn, []byte, error) {
	return s.prepareClaudeOAuthRuntimeWithLocatorBody(ctx, c, parsed, accountID, deviceID, body, body)
}

// prepareClaudeOAuthRuntimeWithLocatorBody separates the Anthropic body that
// receives transcript hydration from the downstream body used to locate the
// conversation. OpenAI-compatible requests can carry prompt_cache_key before
// they are converted to Anthropic Messages format.
func (s *GatewayService) prepareClaudeOAuthRuntimeWithLocatorBody(
	ctx context.Context,
	c interface{ GetHeader(string) string },
	parsed *ParsedRequest,
	accountID int64,
	deviceID string,
	body []byte,
	locatorBody []byte,
) (*claudeOAuthRuntimeTurn, []byte, error) {
	store := s.claudeOAuthRuntimeStore()
	if store == nil || accountID <= 0 {
		return nil, body, nil
	}
	if len(bytes.TrimSpace(locatorBody)) == 0 {
		locatorBody = body
	}
	runtimeKey := resolveClaudeOAuthRuntimeKey(newGinContextView(c), parsed, locatorBody)
	if runtimeKey == "" {
		return nil, body, nil
	}

	runtime, _, err := store.GetOrCreateClaudeOAuthRuntime(
		ctx,
		accountID,
		runtimeKey,
		newClaudeOAuthSessionRuntime(deviceID),
		claudeOAuthRuntimeTTL,
	)
	if err != nil {
		return nil, body, err
	}
	if runtime == nil || strings.TrimSpace(runtime.SessionID) == "" {
		return nil, body, errors.New("Claude OAuth runtime has no session ID")
	}

	turn := &claudeOAuthRuntimeTurn{
		RuntimeKey:    runtimeKey,
		SessionID:     runtime.SessionID,
		StartedAtUnix: runtime.CreatedAtUnix,
	}
	turnID := uuid.NewString()
	claimedRuntime, claimed, err := s.claimClaudeOAuthRuntimeTurn(ctx, accountID, runtimeKey, turnID)
	if err != nil {
		return nil, body, err
	}
	if !claimed {
		// A real Claude Code process serializes main turns within one session.
		// Allowing a second request to proceed with the same session ID but
		// without the canonical transcript creates an impossible lifecycle.
		return turn, body, errClaudeOAuthRuntimeTurnBusy
	}
	turn.Claimed = true
	turn.TurnID = turnID

	hydratedBody, requestMessages, err := hydrateClaudeOAuthRuntimeTranscript(body, claimedRuntime.Transcript)
	if err != nil {
		s.releaseClaudeOAuthRuntimeTurn(context.WithoutCancel(ctx), accountID, turn)
		return nil, body, err
	}
	// This is a fallback for callers that do not run the 2.1.208 finalizer.
	// Production main paths overwrite it after all semantic transformations.
	turn.RequestMessages = requestMessages
	return turn, hydratedBody, nil
}

// finalizeClaudeOAuthRuntimeRequestMessages records the final semantic message
// history after reminder/cache/system normalization, rather than the earlier
// hydration snapshot. Generated reminder blocks and wire-only cache markers are
// stripped before storage so the next turn can rebuild them from fresh session
// facts without duplicating meta content.
func finalizeClaudeOAuthRuntimeRequestMessages(turn *claudeOAuthRuntimeTurn, body []byte) error {
	if turn == nil || !turn.Claimed {
		return nil
	}
	messages, err := claudeCode208RuntimeMessagesForStorage(body)
	if err != nil {
		return err
	}
	turn.RequestMessages = messages
	return nil
}

func claudeCode208RuntimeMessagesForStorage(body []byte) (json.RawMessage, error) {
	messages, err := parseClaudeCode208Messages(body)
	if err != nil {
		return nil, err
	}
	for messageIndex := range messages {
		blocks, blockErr := claudeCode208ContentBlocks(messages[messageIndex].Content)
		if blockErr != nil {
			return nil, blockErr
		}
		nextBlocks := make([]json.RawMessage, 0, len(blocks))
		for _, rawBlock := range blocks {
			block := stripClaudeCode208BlockCacheControl(rawBlock)
			text := gjson.GetBytes(block, "text")
			if gjson.GetBytes(block, "type").String() == "text" &&
				text.Type == gjson.String &&
				isClaudeCode208GeneratedReminder(text.String()) {
				continue
			}
			nextBlocks = append(nextBlocks, block)
		}
		messages[messageIndex].Content, blockErr = marshalClaudeCode208ContentBlocks(nextBlocks)
		if blockErr != nil {
			return nil, blockErr
		}
	}
	return json.Marshal(messages)
}

func (s *GatewayService) claimClaudeOAuthRuntimeTurn(
	ctx context.Context,
	accountID int64,
	runtimeKey, turnID string,
) (*ClaudeOAuthSessionRuntime, bool, error) {
	var claimedRuntime *ClaudeOAuthSessionRuntime
	changed, err := s.mutateClaudeOAuthRuntime(ctx, accountID, runtimeKey, func(runtime *ClaudeOAuthSessionRuntime) (bool, error) {
		now := time.Now()
		if runtime.ActiveTurnID != "" && runtime.TurnLeaseUnix > now.Unix() {
			return false, nil
		}
		runtime.ActiveTurnID = turnID
		runtime.TurnLeaseUnix = now.Add(claudeOAuthRuntimeTurnLease).Unix()
		claimedRuntime = cloneClaudeOAuthRuntime(runtime)
		return true, nil
	})
	return claimedRuntime, changed, err
}

func (s *GatewayService) releaseClaudeOAuthRuntimeTurn(ctx context.Context, accountID int64, turn *claudeOAuthRuntimeTurn) {
	if turn == nil || !turn.Claimed {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.mutateClaudeOAuthRuntime(releaseCtx, accountID, turn.RuntimeKey, func(runtime *ClaudeOAuthSessionRuntime) (bool, error) {
		if runtime.ActiveTurnID != turn.TurnID {
			return false, nil
		}
		runtime.ActiveTurnID = ""
		runtime.TurnLeaseUnix = 0
		return true, nil
	})
}

func (s *GatewayService) commitClaudeOAuthRuntimeTurn(
	ctx context.Context,
	accountID int64,
	turn *claudeOAuthRuntimeTurn,
	assistantContent json.RawMessage,
) error {
	if turn == nil || !turn.Claimed {
		return nil
	}
	transcript, err := buildClaudeOAuthCommittedTranscript(turn.RequestMessages, assistantContent)
	if err != nil {
		s.releaseClaudeOAuthRuntimeTurn(ctx, accountID, turn)
		return err
	}
	if len(transcript) > claudeOAuthRuntimeMaxBytes {
		s.releaseClaudeOAuthRuntimeTurn(ctx, accountID, turn)
		return fmt.Errorf("Claude OAuth transcript exceeds %d bytes", claudeOAuthRuntimeMaxBytes)
	}

	changed, err := s.mutateClaudeOAuthRuntime(ctx, accountID, turn.RuntimeKey, func(runtime *ClaudeOAuthSessionRuntime) (bool, error) {
		if runtime.ActiveTurnID != turn.TurnID {
			return false, errClaudeOAuthRuntimeCASConflict
		}
		runtime.Transcript = append(json.RawMessage(nil), transcript...)
		runtime.ActiveTurnID = ""
		runtime.TurnLeaseUnix = 0
		return true, nil
	})
	if err != nil {
		s.releaseClaudeOAuthRuntimeTurn(ctx, accountID, turn)
		return err
	}
	if !changed {
		return errClaudeOAuthRuntimeCASConflict
	}
	return nil
}

func (s *GatewayService) mutateClaudeOAuthRuntime(
	ctx context.Context,
	accountID int64,
	runtimeKey string,
	mutate func(*ClaudeOAuthSessionRuntime) (bool, error),
) (bool, error) {
	store := s.claudeOAuthRuntimeStore()
	if store == nil {
		return false, nil
	}
	for range 8 {
		current, err := store.GetClaudeOAuthRuntime(ctx, accountID, runtimeKey, claudeOAuthRuntimeTTL)
		if err != nil {
			return false, err
		}
		if current == nil {
			return false, errors.New("Claude OAuth runtime not found")
		}
		next := cloneClaudeOAuthRuntime(current)
		changed, err := mutate(next)
		if err != nil || !changed {
			return changed, err
		}
		next.Version = current.Version + 1
		next.SchemaVersion = claudeOAuthRuntimeSchemaVersion
		next.UpdatedAtUnix = time.Now().Unix()
		swapped, err := store.CompareAndSwapClaudeOAuthRuntime(
			ctx,
			accountID,
			runtimeKey,
			current.Version,
			next,
			claudeOAuthRuntimeTTL,
		)
		if err != nil {
			return false, err
		}
		if swapped {
			return true, nil
		}
	}
	return false, errClaudeOAuthRuntimeCASConflict
}

func (s *GatewayService) claimClaudeOAuthRuntimeAction(
	ctx context.Context,
	accountID int64,
	runtimeKey, action string,
) (string, bool, error) {
	lease := claudeOAuthRuntimeTitleLease
	if action == claudeOAuthSessionActionQuota {
		lease = claudeOAuthRuntimeQuotaLease
	}
	claimID := uuid.NewString()
	changed, err := s.mutateClaudeOAuthRuntime(ctx, accountID, runtimeKey, func(runtime *ClaudeOAuthSessionRuntime) (bool, error) {
		state := claudeOAuthRuntimeActionForName(runtime, action)
		if state == nil {
			return false, fmt.Errorf("unknown Claude OAuth runtime action %q", action)
		}
		now := time.Now()
		if state.State == claudeOAuthRuntimeActionCompleted ||
			state.State == claudeOAuthRuntimeActionGenerated ||
			(state.State == claudeOAuthRuntimeActionPending && state.LeaseUntilUnix > now.Unix()) {
			return false, nil
		}
		state.State = claudeOAuthRuntimeActionPending
		state.Attempts++
		state.ClaimID = claimID
		state.LeaseUntilUnix = now.Add(lease).Unix()
		state.StatusCode = 0
		state.Value = ""
		state.ResponseHeader = nil
		state.LastError = ""
		state.UpdatedAtUnix = now.Unix()
		return true, nil
	})
	if err != nil || !changed {
		return "", changed, err
	}
	return claimID, true, nil
}

func (s *GatewayService) completeClaudeOAuthRuntimeAction(
	ctx context.Context,
	accountID int64,
	runtimeKey, action, claimID string,
	success bool,
	statusCode int,
	value, lastError string,
	responseHeader map[string]string,
) error {
	_, err := s.mutateClaudeOAuthRuntime(ctx, accountID, runtimeKey, func(runtime *ClaudeOAuthSessionRuntime) (bool, error) {
		state := claudeOAuthRuntimeActionForName(runtime, action)
		if state == nil {
			return false, fmt.Errorf("unknown Claude OAuth runtime action %q", action)
		}
		if strings.TrimSpace(claimID) != "" && state.ClaimID != claimID {
			// A newer retry owns the action lease. Ignore this stale completion
			// instead of letting a late response overwrite the newer result.
			return false, nil
		}
		if success {
			if action == claudeOAuthSessionActionTitle {
				state.State = claudeOAuthRuntimeActionGenerated
			} else {
				state.State = claudeOAuthRuntimeActionCompleted
			}
		} else {
			state.State = claudeOAuthRuntimeActionRetryableFailed
		}
		state.ClaimID = ""
		state.LeaseUntilUnix = 0
		state.StatusCode = statusCode
		state.Value = strings.TrimSpace(value)
		state.ResponseHeader = cloneRuntimeHeader(responseHeader)
		state.LastError = strings.TrimSpace(lastError)
		state.UpdatedAtUnix = time.Now().Unix()
		return true, nil
	})
	return err
}

func claudeOAuthRuntimeActionForName(runtime *ClaudeOAuthSessionRuntime, action string) *ClaudeOAuthRuntimeAction {
	if runtime == nil {
		return nil
	}
	switch action {
	case claudeOAuthSessionActionQuota:
		return &runtime.Quota
	case claudeOAuthSessionActionTitle:
		return &runtime.Title
	default:
		return nil
	}
}

func cloneClaudeOAuthRuntime(runtime *ClaudeOAuthSessionRuntime) *ClaudeOAuthSessionRuntime {
	if runtime == nil {
		return nil
	}
	cloned := *runtime
	cloned.Transcript = append(json.RawMessage(nil), runtime.Transcript...)
	cloned.Quota.ResponseHeader = cloneRuntimeHeader(runtime.Quota.ResponseHeader)
	cloned.Title.ResponseHeader = cloneRuntimeHeader(runtime.Title.ResponseHeader)
	cloned.Suggestion.ResponseHeader = cloneRuntimeHeader(runtime.Suggestion.ResponseHeader)
	return &cloned
}

func cloneRuntimeHeader(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func hydrateClaudeOAuthRuntimeTranscript(body []byte, stored json.RawMessage) ([]byte, json.RawMessage, error) {
	incoming, err := decodeClaudeOAuthMessages(gjson.GetBytes(body, "messages").Raw)
	if err != nil {
		return body, nil, err
	}
	if len(incoming) == 0 {
		return body, json.RawMessage("[]"), nil
	}
	storedMessages, err := decodeClaudeOAuthMessages(string(stored))
	if err != nil {
		return body, nil, err
	}

	merged := incoming
	if len(storedMessages) > 0 && !claudeOAuthIncomingContainsRuntimeHistory(incoming, storedMessages) {
		merged = append(append([]json.RawMessage(nil), storedMessages...), incoming...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return body, nil, err
	}
	if bytes.Equal(bytes.TrimSpace(encoded), bytes.TrimSpace([]byte(gjson.GetBytes(body, "messages").Raw))) {
		return body, encoded, nil
	}
	next, ok := setJSONRawBytes(body, "messages", encoded)
	if !ok {
		return body, nil, errors.New("hydrate Claude OAuth runtime transcript")
	}
	return next, encoded, nil
}

func decodeClaudeOAuthMessages(raw string) ([]json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return nil, fmt.Errorf("decode Claude OAuth messages: %w", err)
	}
	return messages, nil
}

func claudeOAuthIncomingContainsRuntimeHistory(incoming, stored []json.RawMessage) bool {
	if len(incoming) == 0 || len(stored) == 0 {
		return false
	}
	// Full-history clients may normalize historical assistant blocks. The first
	// message remains the stable conversation anchor, so a matching anchor and
	// a history at least as long as the runtime transcript is sufficient.
	//
	// Cache markers are transport hints rather than conversation content. Soak
	// clients intentionally remove old markers before replaying full history;
	// treating that normalization as a different anchor would prepend the
	// runtime transcript and duplicate every completed turn.
	return len(incoming) >= len(stored) && claudeOAuthRuntimeAnchorEqual(incoming[0], stored[0])
}

func claudeOAuthRuntimeAnchorEqual(left, right json.RawMessage) bool {
	leftRole, leftText, leftOK := claudeOAuthRuntimeAnchor(left)
	rightRole, rightText, rightOK := claudeOAuthRuntimeAnchor(right)
	if leftOK && rightOK {
		return leftRole == rightRole && leftText == rightText
	}
	return jsonRawSemanticallyEqualIgnoringCacheControl(left, right)
}

func claudeOAuthRuntimeAnchor(raw json.RawMessage) (string, string, bool) {
	var message claudeCode208WireMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", "", false
	}
	blocks, err := claudeCode208ContentBlocks(message.Content)
	if err != nil {
		return "", "", false
	}
	var texts []string
	for _, block := range blocks {
		text := gjson.GetBytes(block, "text")
		if gjson.GetBytes(block, "type").String() != "text" || text.Type != gjson.String ||
			isClaudeCode208GeneratedReminder(text.String()) {
			continue
		}
		texts = append(texts, text.String())
	}
	if len(texts) == 0 {
		return "", "", false
	}
	return message.Role, strings.Join(texts, "\n"), true
}

func jsonRawSemanticallyEqualIgnoringCacheControl(left, right json.RawMessage) bool {
	var l any
	var r any
	if json.Unmarshal(left, &l) != nil || json.Unmarshal(right, &r) != nil {
		return bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
	}
	removeClaudeOAuthCacheControl(l)
	removeClaudeOAuthCacheControl(r)
	lb, _ := json.Marshal(l)
	rb, _ := json.Marshal(r)
	return bytes.Equal(lb, rb)
}

func removeClaudeOAuthCacheControl(value any) {
	switch current := value.(type) {
	case map[string]any:
		delete(current, "cache_control")
		for _, child := range current {
			removeClaudeOAuthCacheControl(child)
		}
	case []any:
		for _, child := range current {
			removeClaudeOAuthCacheControl(child)
		}
	}
}

func buildClaudeOAuthCommittedTranscript(requestMessages, assistantContent json.RawMessage) (json.RawMessage, error) {
	messages, err := decodeClaudeOAuthMessages(string(requestMessages))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(assistantContent)) == 0 || bytes.Equal(bytes.TrimSpace(assistantContent), []byte("null")) {
		return json.Marshal(messages)
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(assistantContent, &blocks); err != nil {
		return nil, fmt.Errorf("decode Claude OAuth assistant content: %w", err)
	}
	assistant, err := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{
		Role:    "assistant",
		Content: assistantContent,
	})
	if err != nil {
		return nil, err
	}
	messages = append(messages, assistant)
	return json.Marshal(messages)
}

func claudeOAuthRuntimeResponseHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	output := make(map[string]string)
	for name, values := range header {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower != "retry-after" &&
			!strings.HasPrefix(lower, "anthropic-ratelimit-") &&
			!strings.HasPrefix(lower, "x-ratelimit-") {
			continue
		}
		if value := strings.TrimSpace(strings.Join(values, ",")); value != "" {
			output[lower] = value
		}
	}
	return output
}
