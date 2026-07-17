//go:build unit

package repository

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newIdentityCacheForUnitTest(t *testing.T) *identityCache {
	t.Helper()
	cache, _ := newIdentityCacheWithMiniRedisForUnitTest(t)
	return cache
}

func newIdentityCacheWithMiniRedisForUnitTest(t *testing.T) (*identityCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &identityCache{rdb: rdb}, mr
}

func TestFingerprintKey(t *testing.T) {
	tests := []struct {
		name      string
		accountID int64
		expected  string
	}{
		{
			name:      "normal_account_id",
			accountID: 123,
			expected:  "fingerprint:123",
		},
		{
			name:      "zero_account_id",
			accountID: 0,
			expected:  "fingerprint:0",
		},
		{
			name:      "negative_account_id",
			accountID: -1,
			expected:  "fingerprint:-1",
		},
		{
			name:      "max_int64",
			accountID: math.MaxInt64,
			expected:  "fingerprint:9223372036854775807",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fingerprintKey(tc.accountID)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestIdentityCacheGetFingerprint_MissingReturnsNilFingerprint(t *testing.T) {
	cache := newIdentityCacheForUnitTest(t)

	fp, err := cache.GetFingerprint(context.Background(), 123)

	require.NoError(t, err)
	require.Nil(t, fp)
}

func TestIdentityCacheGetFingerprint_PreservesJSONErrors(t *testing.T) {
	cache := newIdentityCacheForUnitTest(t)
	ctx := context.Background()
	require.NoError(t, cache.rdb.Set(ctx, fingerprintKey(123), "not-json", 0).Err())

	fp, err := cache.GetFingerprint(ctx, 123)

	require.Nil(t, fp)
	require.Error(t, err)
	require.NotErrorIs(t, err, redis.Nil)
}

func TestIdentityCacheGetFingerprint_PreservesRedisErrors(t *testing.T) {
	cache := newIdentityCacheForUnitTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fp, err := cache.GetFingerprint(ctx, 123)

	require.Nil(t, fp)
	require.ErrorIs(t, err, context.Canceled)
}

func TestIdentityCacheTryClaimMaskedSessionID_OnlyFirstClaimWinsAndSetsTTL(t *testing.T) {
	cache, mr := newIdentityCacheWithMiniRedisForUnitTest(t)
	ctx := context.Background()
	const accountID = int64(123)

	claimed, err := cache.TryClaimMaskedSessionID(ctx, accountID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, maskedSessionTTL, mr.TTL(maskedSessionKey(accountID)))

	claimed, err = cache.TryClaimMaskedSessionID(ctx, accountID, "ffffffff-eeee-4ddd-8ccc-bbbbbbbbbbbb")
	require.NoError(t, err)
	require.False(t, claimed)

	stored, err := cache.GetMaskedSessionID(ctx, accountID)
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", stored)
}

func TestIdentityCacheGetAndRefreshMaskedSessionID_RenewsTTLAtomically(t *testing.T) {
	cache, mr := newIdentityCacheWithMiniRedisForUnitTest(t)
	ctx := context.Background()
	const accountID = int64(124)
	const sessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.NoError(t, cache.rdb.Set(ctx, maskedSessionKey(accountID), sessionID, time.Minute).Err())
	mr.FastForward(30 * time.Second)

	got, err := cache.GetAndRefreshMaskedSessionID(ctx, accountID)

	require.NoError(t, err)
	require.Equal(t, sessionID, got)
	require.Equal(t, maskedSessionTTL, mr.TTL(maskedSessionKey(accountID)))
}

func TestIdentityCacheTryClaimFingerprint_OnlyFirstClaimWinsAndSetsTTL(t *testing.T) {
	cache, mr := newIdentityCacheWithMiniRedisForUnitTest(t)
	ctx := context.Background()
	const accountID = int64(125)
	first := &service.Fingerprint{ClientID: "first-client", UserAgent: "claude-cli/2.1.161"}
	second := &service.Fingerprint{ClientID: "second-client", UserAgent: "claude-cli/2.1.161"}

	claimed, err := cache.TryClaimFingerprint(ctx, accountID, first)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, fingerprintTTL, mr.TTL(fingerprintKey(accountID)))

	claimed, err = cache.TryClaimFingerprint(ctx, accountID, second)
	require.NoError(t, err)
	require.False(t, claimed)

	stored, err := cache.GetFingerprint(ctx, accountID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, first.ClientID, stored.ClientID)
}

func TestIdentityCacheEnsureFingerprintClientID_RepairsOnlyOnceAndPreservesTTL(t *testing.T) {
	cache, mr := newIdentityCacheWithMiniRedisForUnitTest(t)
	ctx := context.Background()
	const accountID = int64(126)
	initial := &service.Fingerprint{UserAgent: "claude-cli/2.1.161"}
	require.NoError(t, cache.SetFingerprint(ctx, accountID, initial))
	mr.FastForward(time.Hour)
	ttlBefore := mr.TTL(fingerprintKey(accountID))

	first, err := cache.EnsureFingerprintClientID(ctx, accountID, "first-client")
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, "first-client", first.ClientID)
	require.Equal(t, ttlBefore, mr.TTL(fingerprintKey(accountID)))

	second, err := cache.EnsureFingerprintClientID(ctx, accountID, "second-client")
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, "first-client", second.ClientID)
}

func TestIdentityCacheTryClaimMaskedSessionID_PropagatesRedisErrors(t *testing.T) {
	cache := newIdentityCacheForUnitTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	claimed, err := cache.TryClaimMaskedSessionID(ctx, 123, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")

	require.False(t, claimed)
	require.ErrorIs(t, err, context.Canceled)
}
