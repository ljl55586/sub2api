package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type identityCacheStub struct {
	maskedSessionID    string
	fingerprint        *Fingerprint
	fingerprintErr     error
	setFingerprintErr  error
	setFingerprintCall int
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
	return s.maskedSessionID, nil
}
func (s *identityCacheStub) SetMaskedSessionID(_ context.Context, _ int64, sessionID string) error {
	s.maskedSessionID = sessionID
	return nil
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

func strconvQuote(v string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
}
