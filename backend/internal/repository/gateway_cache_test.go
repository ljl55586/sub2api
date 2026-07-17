//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

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

func TestGatewayCacheTryClaimClaudeOAuthSessionCompanions(t *testing.T) {
	cache, mr := newGatewayCacheForUnitTest(t)
	ctx := context.Background()
	const ttl = time.Minute

	claimed, err := cache.TryClaimClaudeOAuthSessionCompanions(ctx, 99, "session-a", ttl)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, ttl, mr.TTL(buildClaudeOAuthSessionCompanionKey(99, "session-a")))

	claimed, err = cache.TryClaimClaudeOAuthSessionCompanions(ctx, 99, "session-a", ttl)
	require.NoError(t, err)
	require.False(t, claimed)

	claimed, err = cache.TryClaimClaudeOAuthSessionCompanions(ctx, 100, "session-a", ttl)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestGatewayCacheTryClaimClaudeOAuthSessionCompanions_InvalidInputDoesNotWrite(t *testing.T) {
	cache, _ := newGatewayCacheForUnitTest(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		accountID int64
		sessionID string
		ttl       time.Duration
	}{
		{name: "invalid account", accountID: 0, sessionID: "session-a", ttl: time.Minute},
		{name: "blank session", accountID: 99, sessionID: "  ", ttl: time.Minute},
		{name: "zero ttl", accountID: 99, sessionID: "session-a", ttl: 0},
		{name: "negative ttl", accountID: 99, sessionID: "session-a", ttl: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claimed, err := cache.TryClaimClaudeOAuthSessionCompanions(ctx, tt.accountID, tt.sessionID, tt.ttl)
			require.NoError(t, err)
			require.False(t, claimed)

			keys, err := cache.rdb.Keys(ctx, "*").Result()
			require.NoError(t, err)
			require.Empty(t, keys)
		})
	}
}
