//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newGatewayCacheForUnitTest(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &gatewayCache{rdb: rdb}, mr
}

func TestGatewayCacheClaudeOAuthSessionActions(t *testing.T) {
	cache, mr := newGatewayCacheForUnitTest(t)
	ctx := context.Background()
	const ttl = time.Minute

	claimed, err := cache.TryClaimClaudeOAuthSessionAction(ctx, 99, "session-a", "quota", ttl)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, ttl, mr.TTL(buildClaudeOAuthSessionActionKey(99, "session-a", "quota")))

	claimed, err = cache.TryClaimClaudeOAuthSessionAction(ctx, 99, "session-a", "quota", ttl)
	require.NoError(t, err)
	require.False(t, claimed)

	claimed, err = cache.TryClaimClaudeOAuthSessionAction(ctx, 99, "session-a", "title", ttl)
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, cache.ReleaseClaudeOAuthSessionAction(ctx, 99, "session-a", "title"))
	claimed, err = cache.TryClaimClaudeOAuthSessionAction(ctx, 99, "session-a", "title", ttl)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestGatewayCacheClaudeOAuthSessionActions_InvalidInputDoesNotWrite(t *testing.T) {
	cache, _ := newGatewayCacheForUnitTest(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		accountID int64
		sessionID string
		action    string
		ttl       time.Duration
	}{
		{name: "invalid account", accountID: 0, sessionID: "session-a", action: "quota", ttl: time.Minute},
		{name: "blank session", accountID: 99, sessionID: "  ", action: "quota", ttl: time.Minute},
		{name: "blank action", accountID: 99, sessionID: "session-a", action: "  ", ttl: time.Minute},
		{name: "zero ttl", accountID: 99, sessionID: "session-a", action: "quota", ttl: 0},
		{name: "negative ttl", accountID: 99, sessionID: "session-a", action: "quota", ttl: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claimed, err := cache.TryClaimClaudeOAuthSessionAction(ctx, tt.accountID, tt.sessionID, tt.action, tt.ttl)
			require.NoError(t, err)
			require.False(t, claimed)

			keys, err := cache.rdb.Keys(ctx, "*").Result()
			require.NoError(t, err)
			require.Empty(t, keys)
		})
	}
}

func TestGatewayCacheClaudeOAuthRuntime_GetOrCreateAndCAS(t *testing.T) {
	cache, mr := newGatewayCacheForUnitTest(t)
	ctx := context.Background()
	const (
		accountID  = int64(101)
		runtimeKey = "runtime-a"
		ttl        = time.Hour
	)
	candidate := &service.ClaudeOAuthSessionRuntime{
		SchemaVersion: 1,
		Version:       1,
		SessionID:     "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		CreatedAtUnix: 1,
		UpdatedAtUnix: 1,
	}

	runtime, created, err := cache.GetOrCreateClaudeOAuthRuntime(ctx, accountID, runtimeKey, candidate, ttl)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, candidate.SessionID, runtime.SessionID)
	require.Equal(t, ttl, mr.TTL(buildClaudeOAuthRuntimeKey(accountID, runtimeKey)))

	other := *candidate
	other.SessionID = "ffffffff-eeee-4ddd-8ccc-bbbbbbbbbbbb"
	runtime, created, err = cache.GetOrCreateClaudeOAuthRuntime(ctx, accountID, runtimeKey, &other, ttl)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, candidate.SessionID, runtime.SessionID)

	next := *runtime
	next.Version = 2
	next.Title = service.ClaudeOAuthRuntimeAction{State: "generated", Value: "Runtime title"}
	swapped, err := cache.CompareAndSwapClaudeOAuthRuntime(ctx, accountID, runtimeKey, 1, &next, ttl)
	require.NoError(t, err)
	require.True(t, swapped)

	stale := next
	stale.Version = 3
	swapped, err = cache.CompareAndSwapClaudeOAuthRuntime(ctx, accountID, runtimeKey, 1, &stale, ttl)
	require.NoError(t, err)
	require.False(t, swapped)

	stored, err := cache.GetClaudeOAuthRuntime(ctx, accountID, runtimeKey, ttl)
	require.NoError(t, err)
	require.Equal(t, int64(2), stored.Version)
	require.Equal(t, "Runtime title", stored.Title.Value)
}
