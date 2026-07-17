package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type companionClaimStoreForTest struct {
	mu       sync.Mutex
	claimed  map[string]struct{}
	claimErr error
}

func newCompanionClaimStoreForTest() *companionClaimStoreForTest {
	return &companionClaimStoreForTest{claimed: make(map[string]struct{})}
}

func (s *companionClaimStoreForTest) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (s *companionClaimStoreForTest) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (s *companionClaimStoreForTest) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (s *companionClaimStoreForTest) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

func (s *companionClaimStoreForTest) TryClaimClaudeOAuthSessionCompanions(_ context.Context, accountID int64, sessionID string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.claimErr != nil {
		return false, s.claimErr
	}
	key := fmt.Sprintf("%d:%s", accountID, sessionID)
	if _, ok := s.claimed[key]; ok {
		return false, nil
	}
	s.claimed[key] = struct{}{}
	return true, nil
}

type gatewayCacheWithoutCompanionClaimStoreForTest struct{}

func (gatewayCacheWithoutCompanionClaimStoreForTest) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (gatewayCacheWithoutCompanionClaimStoreForTest) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (gatewayCacheWithoutCompanionClaimStoreForTest) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (gatewayCacheWithoutCompanionClaimStoreForTest) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

func TestClaimClaudeOAuthSessionCompanions_OnlyOneConcurrentWinner(t *testing.T) {
	store := newCompanionClaimStoreForTest()
	svc := &GatewayService{cache: store}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a") {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), winners.Load())
}

func TestClaimClaudeOAuthSessionCompanions_SecondClaimForSameSessionReturnsFalse(t *testing.T) {
	svc := &GatewayService{cache: newCompanionClaimStoreForTest()}

	require.True(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a"))
	require.False(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a"))
}

func TestClaimClaudeOAuthSessionCompanions_DifferentAccountsCanEachClaim(t *testing.T) {
	svc := &GatewayService{cache: newCompanionClaimStoreForTest()}

	require.True(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a"))
	require.True(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1002, "session-a"))
}

func TestClaimClaudeOAuthSessionCompanions_UnsupportedOrFailingStoreDoesNotClaim(t *testing.T) {
	t.Run("unsupported cache", func(t *testing.T) {
		svc := &GatewayService{cache: gatewayCacheWithoutCompanionClaimStoreForTest{}}
		require.NotPanics(t, func() {
			require.False(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a"))
		})
	})

	t.Run("claim store error", func(t *testing.T) {
		svc := &GatewayService{cache: &companionClaimStoreForTest{claimErr: errors.New("redis unavailable")}}
		require.NotPanics(t, func() {
			require.False(t, svc.claimClaudeOAuthSessionCompanions(context.Background(), 1001, "session-a"))
		})
	})
}
