//go:build integration

package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type GatewayCacheSuite struct {
	IntegrationRedisSuite
	cache service.GatewayCache
}

func (s *GatewayCacheSuite) SetupTest() {
	s.IntegrationRedisSuite.SetupTest()
	s.cache = NewGatewayCache(s.rdb)
}

func (s *GatewayCacheSuite) TestGetSessionAccountID_Missing() {
	_, err := s.cache.GetSessionAccountID(s.ctx, 1, "nonexistent")
	require.True(s.T(), errors.Is(err, redis.Nil), "expected redis.Nil for missing session")
}

func (s *GatewayCacheSuite) TestSetAndGetSessionAccountID() {
	sessionID := "s1"
	accountID := int64(99)
	groupID := int64(1)
	sessionTTL := 1 * time.Minute

	require.NoError(s.T(), s.cache.SetSessionAccountID(s.ctx, groupID, sessionID, accountID, sessionTTL), "SetSessionAccountID")

	sid, err := s.cache.GetSessionAccountID(s.ctx, groupID, sessionID)
	require.NoError(s.T(), err, "GetSessionAccountID")
	require.Equal(s.T(), accountID, sid, "session id mismatch")
}

func (s *GatewayCacheSuite) TestSessionAccountID_TTL() {
	sessionID := "s2"
	accountID := int64(100)
	groupID := int64(1)
	sessionTTL := 1 * time.Minute

	require.NoError(s.T(), s.cache.SetSessionAccountID(s.ctx, groupID, sessionID, accountID, sessionTTL), "SetSessionAccountID")

	sessionKey := buildSessionKey(groupID, sessionID)
	ttl, err := s.rdb.TTL(s.ctx, sessionKey).Result()
	require.NoError(s.T(), err, "TTL sessionKey after Set")
	s.AssertTTLWithin(ttl, 1*time.Second, sessionTTL)
}

func (s *GatewayCacheSuite) TestRefreshSessionTTL() {
	sessionID := "s3"
	accountID := int64(101)
	groupID := int64(1)
	initialTTL := 1 * time.Minute
	refreshTTL := 3 * time.Minute

	require.NoError(s.T(), s.cache.SetSessionAccountID(s.ctx, groupID, sessionID, accountID, initialTTL), "SetSessionAccountID")

	require.NoError(s.T(), s.cache.RefreshSessionTTL(s.ctx, groupID, sessionID, refreshTTL), "RefreshSessionTTL")

	sessionKey := buildSessionKey(groupID, sessionID)
	ttl, err := s.rdb.TTL(s.ctx, sessionKey).Result()
	require.NoError(s.T(), err, "TTL after Refresh")
	s.AssertTTLWithin(ttl, 1*time.Second, refreshTTL)
}

func (s *GatewayCacheSuite) TestRefreshSessionTTL_MissingKey() {
	// RefreshSessionTTL on a missing key should not error (no-op)
	err := s.cache.RefreshSessionTTL(s.ctx, 1, "missing-session", 1*time.Minute)
	require.NoError(s.T(), err, "RefreshSessionTTL on missing key should not error")
}

func (s *GatewayCacheSuite) TestDeleteSessionAccountID() {
	sessionID := "openai:s4"
	accountID := int64(102)
	groupID := int64(1)
	sessionTTL := 1 * time.Minute

	require.NoError(s.T(), s.cache.SetSessionAccountID(s.ctx, groupID, sessionID, accountID, sessionTTL), "SetSessionAccountID")
	require.NoError(s.T(), s.cache.DeleteSessionAccountID(s.ctx, groupID, sessionID), "DeleteSessionAccountID")

	_, err := s.cache.GetSessionAccountID(s.ctx, groupID, sessionID)
	require.True(s.T(), errors.Is(err, redis.Nil), "expected redis.Nil after delete")
}

func (s *GatewayCacheSuite) TestGetSessionAccountID_CorruptedValue() {
	sessionID := "corrupted"
	groupID := int64(1)
	sessionKey := buildSessionKey(groupID, sessionID)

	// Set a non-integer value
	require.NoError(s.T(), s.rdb.Set(s.ctx, sessionKey, "not-a-number", 1*time.Minute).Err(), "Set invalid value")

	_, err := s.cache.GetSessionAccountID(s.ctx, groupID, sessionID)
	require.Error(s.T(), err, "expected error for corrupted value")
	require.False(s.T(), errors.Is(err, redis.Nil), "expected parsing error, not redis.Nil")
}

func (s *GatewayCacheSuite) TestClaudeOAuthSessionActions() {
	store, ok := s.cache.(service.ClaudeOAuthSessionActionStore)
	require.True(s.T(), ok, "GatewayCache should expose the optional session action store")

	claimed, err := store.TryClaimClaudeOAuthSessionAction(s.ctx, 99, "session-a", "title", time.Minute)
	require.NoError(s.T(), err)
	require.True(s.T(), claimed)
	claimTTL, err := s.rdb.TTL(s.ctx, buildClaudeOAuthSessionActionKey(99, "session-a", "title")).Result()
	require.NoError(s.T(), err)
	s.AssertTTLWithin(claimTTL, time.Second, time.Minute)

	claimed, err = store.TryClaimClaudeOAuthSessionAction(s.ctx, 99, "session-a", "title", time.Minute)
	require.NoError(s.T(), err)
	require.False(s.T(), claimed)

	require.NoError(s.T(), store.ReleaseClaudeOAuthSessionAction(s.ctx, 99, "session-a", "title"))
	claimed, err = store.TryClaimClaudeOAuthSessionAction(s.ctx, 99, "session-a", "title", time.Minute)
	require.NoError(s.T(), err)
	require.True(s.T(), claimed)
}

func (s *GatewayCacheSuite) TestClaudeOAuthRuntimeCAS() {
	store, ok := s.cache.(service.ClaudeOAuthRuntimeStore)
	require.True(s.T(), ok, "GatewayCache should expose the runtime store")

	candidate := &service.ClaudeOAuthSessionRuntime{
		SchemaVersion: 1,
		Version:       1,
		SessionID:     "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		CreatedAtUnix: 1,
		UpdatedAtUnix: 1,
	}
	runtime, created, err := store.GetOrCreateClaudeOAuthRuntime(s.ctx, 102, "runtime-a", candidate, time.Minute)
	require.NoError(s.T(), err)
	require.True(s.T(), created)
	require.Equal(s.T(), candidate.SessionID, runtime.SessionID)

	next := *runtime
	next.Version = 2
	next.Quota = service.ClaudeOAuthRuntimeAction{State: "completed", StatusCode: 429}
	swapped, err := store.CompareAndSwapClaudeOAuthRuntime(s.ctx, 102, "runtime-a", 1, &next, time.Minute)
	require.NoError(s.T(), err)
	require.True(s.T(), swapped)

	stored, err := store.GetClaudeOAuthRuntime(s.ctx, 102, "runtime-a", time.Minute)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(2), stored.Version)
	require.Equal(s.T(), 429, stored.Quota.StatusCode)
}

func TestGatewayCacheSuite(t *testing.T) {
	suite.Run(t, new(GatewayCacheSuite))
}
