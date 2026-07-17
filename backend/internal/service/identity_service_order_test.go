package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type identityCacheStub struct {
	maskedSessionID     string
	maskedSessionErr    error
	setMaskedSessionErr error
	fingerprint         *Fingerprint
	fingerprintErr      error
	setFingerprintErr   error
	setFingerprintCall  int
}

func (s *identityCacheStub) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	if s.fingerprintErr != nil {
		return nil, s.fingerprintErr
	}
	return s.fingerprint, nil
}
func (s *identityCacheStub) SetFingerprint(_ context.Context, _ int64, fp *Fingerprint) error {
	s.setFingerprintCall++
	if s.setFingerprintErr != nil {
		return s.setFingerprintErr
	}
	s.fingerprint = fp
	return nil
}
func (s *identityCacheStub) GetMaskedSessionID(_ context.Context, _ int64) (string, error) {
	if s.maskedSessionErr != nil {
		return "", s.maskedSessionErr
	}
	return s.maskedSessionID, nil
}
func (s *identityCacheStub) SetMaskedSessionID(_ context.Context, _ int64, sessionID string) error {
	if s.setMaskedSessionErr != nil {
		return s.setMaskedSessionErr
	}
	s.maskedSessionID = sessionID
	return nil
}

type atomicMaskedSessionCacheStub struct {
	mu sync.Mutex

	maskedSessionID string
	getErr          error
	setErr          error
	claimErr        error

	forcedMisses      int
	coldMissesReady   chan struct{}
	releaseColdMisses <-chan struct{}

	claimCalls int
	setCalls   int
	setValues  []string
}

func (s *atomicMaskedSessionCacheStub) GetFingerprint(context.Context, int64) (*Fingerprint, error) {
	return nil, nil
}

func (s *atomicMaskedSessionCacheStub) SetFingerprint(context.Context, int64, *Fingerprint) error {
	return nil
}

func (s *atomicMaskedSessionCacheStub) GetMaskedSessionID(_ context.Context, _ int64) (string, error) {
	s.mu.Lock()
	if s.getErr != nil {
		err := s.getErr
		s.mu.Unlock()
		return "", err
	}
	if s.forcedMisses > 0 {
		s.forcedMisses--
		ready := s.coldMissesReady
		if s.forcedMisses != 0 {
			ready = nil
		}
		release := s.releaseColdMisses
		s.mu.Unlock()
		if ready != nil {
			close(ready)
		}
		if release != nil {
			<-release
		}
		return "", nil
	}
	maskedSessionID := s.maskedSessionID
	s.mu.Unlock()
	return maskedSessionID, nil
}

func (s *atomicMaskedSessionCacheStub) SetMaskedSessionID(_ context.Context, _ int64, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.setCalls++
	s.setValues = append(s.setValues, sessionID)
	s.maskedSessionID = sessionID
	return nil
}

func (s *atomicMaskedSessionCacheStub) TryClaimMaskedSessionID(_ context.Context, _ int64, sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return false, s.claimErr
	}
	s.claimCalls++
	if s.maskedSessionID != "" {
		return false, nil
	}
	s.maskedSessionID = sessionID
	return true, nil
}

func TestIdentityService_RewriteUserID_PreservesTopLevelFieldOrder(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)

	originalUserID := FormatMetadataUserID(
		"d61f76d0730d2b920763648949bad5c79742155c27037fc77ac3f9805cb90169",
		"",
		"7578cf37-aaca-46e4-a45c-71285d9dbb83",
		"2.1.78",
	)
	body := []byte(`{"alpha":1,"messages":[],"metadata":{"user_id":` + strconvQuote(originalUserID) + `},"max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"stream":true}`)

	result, err := svc.RewriteUserID(body, 123, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	resultStr := string(result)

	assertJSONTokenOrder(t, resultStr, `"alpha"`, `"messages"`, `"metadata"`, `"max_tokens"`, `"thinking"`, `"output_config"`, `"stream"`)
	require.NotContains(t, resultStr, originalUserID)
	require.Contains(t, resultStr, `"metadata":{"user_id":"`)
}

func TestIdentityService_RewriteUserIDWithMasking_PreservesTopLevelFieldOrder(t *testing.T) {
	cache := &identityCacheStub{maskedSessionID: "11111111-2222-4333-8444-555555555555"}
	svc := NewIdentityService(cache)

	originalUserID := FormatMetadataUserID(
		"d61f76d0730d2b920763648949bad5c79742155c27037fc77ac3f9805cb90169",
		"",
		"7578cf37-aaca-46e4-a45c-71285d9dbb83",
		"2.1.78",
	)
	body := []byte(`{"alpha":1,"messages":[],"metadata":{"user_id":` + strconvQuote(originalUserID) + `},"max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"stream":true}`)

	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"session_id_masking_enabled": true,
		},
	}

	result, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	resultStr := string(result)

	assertJSONTokenOrder(t, resultStr, `"alpha"`, `"messages"`, `"metadata"`, `"max_tokens"`, `"thinking"`, `"output_config"`, `"stream"`)
	require.Contains(t, resultStr, cache.maskedSessionID)
	require.True(t, strings.Contains(resultStr, `"metadata":{"user_id":"`))
}

func TestIdentityService_GetOrCreateFingerprint_ReturnsPersistenceError(t *testing.T) {
	cacheErr := errors.New("redis unavailable")
	svc := NewIdentityService(&identityCacheStub{setFingerprintErr: cacheErr})

	_, err := svc.GetOrCreateFingerprint(context.Background(), 123, nil)

	require.ErrorIs(t, err, cacheErr)
}

func TestIdentityService_GetOrCreateFingerprint_ReturnsReadError(t *testing.T) {
	cacheErr := errors.New("redis read unavailable")
	svc := NewIdentityService(&identityCacheStub{fingerprintErr: cacheErr})

	_, err := svc.GetOrCreateFingerprint(context.Background(), 123, nil)

	require.ErrorIs(t, err, cacheErr)
}

func TestIdentityService_GetOrCreateFingerprint_CacheMissCreatesAndPersistsFingerprint(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 123, nil)

	require.NoError(t, err)
	require.NotNil(t, fp)
	require.NotEmpty(t, fp.ClientID)
	require.NotZero(t, fp.UpdatedAt)
	require.Equal(t, 1, cache.setFingerprintCall)
	require.Same(t, fp, cache.fingerprint)
}

func TestIdentityService_GetOrCreateFingerprint_RepairsEmptyClientIDBeforeReturning(t *testing.T) {
	cache := &identityCacheStub{fingerprint: &Fingerprint{UserAgent: "claude-cli/2.1.161 (external, cli)"}}
	svc := NewIdentityService(cache)

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 123, nil)

	require.NoError(t, err)
	require.NotEmpty(t, fp.ClientID)
	require.Equal(t, 1, cache.setFingerprintCall)
	require.Equal(t, fp.ClientID, cache.fingerprint.ClientID)
}

func TestIdentityService_CreateFingerprintDefaultsToMacOS(t *testing.T) {
	svc := NewIdentityService(&identityCacheStub{})

	fp := svc.createFingerprintFromHeaders(nil)

	require.Equal(t, "MacOS", fp.StainlessOS)
}

func TestIdentityService_RewriteUserIDWithMaskingReturnsStorageError(t *testing.T) {
	cacheErr := errors.New("masked session cache unavailable")
	svc := NewIdentityService(&identityCacheStub{maskedSessionErr: cacheErr})
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"session_id_masking_enabled": true},
	}
	metadataUserID := FormatMetadataUserID("device", "account", "11111111-2222-4333-8444-555555555555", "2.1.161")
	body := []byte(`{"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},"messages":[]}`)

	_, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, "account", "device", "claude-cli/2.1.161 (external, cli)")

	require.ErrorIs(t, err, cacheErr)
}

func TestIdentityService_GetOrCreateMaskedSessionID_AtomicClaimKeepsColdCacheConcurrentCallersStable(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	cache := &atomicMaskedSessionCacheStub{
		forcedMisses:      2,
		coldMissesReady:   ready,
		releaseColdMisses: release,
	}
	svc := NewIdentityService(cache)

	type result struct {
		maskedSessionID string
		err             error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			maskedSessionID, err := svc.GetOrCreateMaskedSessionID(context.Background(), 123)
			results <- result{maskedSessionID: maskedSessionID, err: err}
		}()
	}

	<-ready
	close(release)
	first := <-results
	second := <-results

	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotEmpty(t, first.maskedSessionID)
	require.Equal(t, first.maskedSessionID, second.maskedSessionID)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Equal(t, 2, cache.claimCalls)
	require.Equal(t, 1, cache.setCalls, "loser should refresh the winning value's TTL")
	require.Equal(t, []string{first.maskedSessionID}, cache.setValues)
	require.Equal(t, first.maskedSessionID, cache.maskedSessionID)
}

func TestIdentityService_GetOrCreateMaskedSessionID_AtomicLoserReadsWinnerAndRefreshesTTL(t *testing.T) {
	const winner = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	cache := &atomicMaskedSessionCacheStub{
		maskedSessionID: winner,
		forcedMisses:    1,
	}
	svc := NewIdentityService(cache)

	maskedSessionID, err := svc.GetOrCreateMaskedSessionID(context.Background(), 123)

	require.NoError(t, err)
	require.Equal(t, winner, maskedSessionID)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Equal(t, 1, cache.claimCalls)
	require.Equal(t, 1, cache.setCalls)
	require.Equal(t, []string{winner}, cache.setValues)
}

func TestIdentityService_GetOrCreateMaskedSessionID_AtomicClaimErrorPropagates(t *testing.T) {
	claimErr := errors.New("masked session claim unavailable")
	cache := &atomicMaskedSessionCacheStub{
		claimErr:     claimErr,
		forcedMisses: 1,
	}
	svc := NewIdentityService(cache)

	maskedSessionID, err := svc.GetOrCreateMaskedSessionID(context.Background(), 123)

	require.Empty(t, maskedSessionID)
	require.ErrorIs(t, err, claimErr)
}

func TestIdentityService_GetOrCreateMaskedSessionID_FallsBackForLegacyIdentityCache(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)

	maskedSessionID, err := svc.GetOrCreateMaskedSessionID(context.Background(), 123)

	require.NoError(t, err)
	require.NotEmpty(t, maskedSessionID)
	require.Equal(t, maskedSessionID, cache.maskedSessionID)
}

func strconvQuote(v string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
}
