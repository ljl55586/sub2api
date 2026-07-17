//go:build unit

package repository

import (
	"context"
	"math"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newIdentityCacheForUnitTest(t *testing.T) *identityCache {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &identityCache{rdb: rdb}
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
