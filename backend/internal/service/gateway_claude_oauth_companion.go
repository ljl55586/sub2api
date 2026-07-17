package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ClaudeOAuthSessionCompanionClaimStore optionally records the first request for
// a Claude OAuth session. Implementations must claim atomically across processes.
type ClaudeOAuthSessionCompanionClaimStore interface {
	TryClaimClaudeOAuthSessionCompanions(ctx context.Context, accountID int64, sessionID string, ttl time.Duration) (bool, error)
}

func (s *GatewayService) claudeOAuthSessionCompanionClaimStore() ClaudeOAuthSessionCompanionClaimStore {
	if s == nil || s.cache == nil {
		return nil
	}
	store, ok := s.cache.(ClaudeOAuthSessionCompanionClaimStore)
	if !ok {
		return nil
	}
	return store
}

func (s *GatewayService) claimClaudeOAuthSessionCompanions(ctx context.Context, accountID int64, sessionID string) bool {
	if accountID <= 0 || strings.TrimSpace(sessionID) == "" {
		return false
	}
	store := s.claudeOAuthSessionCompanionClaimStore()
	if store == nil {
		return false
	}
	claimed, err := store.TryClaimClaudeOAuthSessionCompanions(ctx, accountID, sessionID, stickySessionTTL)
	if err != nil {
		logger.LegacyPrintf("service.gateway", "Claude OAuth companion claim failed for account %d: %v", accountID, err)
		return false
	}
	return claimed
}
