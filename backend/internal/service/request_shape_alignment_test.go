package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type accountScopedIdentityCache struct {
	fingerprints map[int64]*Fingerprint
}

func (c *accountScopedIdentityCache) GetFingerprint(_ context.Context, accountID int64) (*Fingerprint, error) {
	if fp := c.fingerprints[accountID]; fp != nil {
		copy := *fp
		return &copy, nil
	}
	return nil, nil
}

func (c *accountScopedIdentityCache) SetFingerprint(_ context.Context, accountID int64, fp *Fingerprint) error {
	if c.fingerprints == nil {
		c.fingerprints = make(map[int64]*Fingerprint)
	}
	copy := *fp
	c.fingerprints[accountID] = &copy
	return nil
}

func (c *accountScopedIdentityCache) GetMaskedSessionID(context.Context, int64) (string, error) {
	return "", nil
}

func (c *accountScopedIdentityCache) SetMaskedSessionID(context.Context, int64, string) error {
	return nil
}

func TestResolveStableAccountIdentity_BindsDeviceAndAccountToAccount(t *testing.T) {
	cache := &accountScopedIdentityCache{}
	svc := NewIdentityService(cache)
	accountOne := &Account{ID: 101, Extra: map[string]any{"account_uuid": "account-one"}}
	accountTwo := &Account{ID: 202, Extra: map[string]any{"account_uuid": "account-two"}}

	first, err := svc.ResolveStableAccountIdentity(context.Background(), accountOne, nil)
	require.NoError(t, err)
	second, err := svc.ResolveStableAccountIdentity(context.Background(), accountOne, nil)
	require.NoError(t, err)
	other, err := svc.ResolveStableAccountIdentity(context.Background(), accountTwo, nil)
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.Equal(t, "account-one", first.AccountUUID)
	require.NotEmpty(t, first.DeviceID)
	require.NotEqual(t, first.AccountUUID, other.AccountUUID)
	require.NotEqual(t, first.DeviceID, other.DeviceID)
}

func TestDebugLogGatewaySnapshot_PreservesBodyForFieldComparison(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "gateway.log")
	svc := &GatewayService{cfg: &config.Config{}}
	svc.initDebugGatewayBodyFile(logPath)
	file := svc.debugGatewayBodyFile.Load()
	require.NotNil(t, file)
	t.Cleanup(func() { _ = file.Close() })

	body := []byte(`{"metadata":{"user_id":"synthetic-device-account-session"},"messages":[{"role":"user","content":"comparison prompt"}]}`)
	svc.debugLogGatewaySnapshot("UPSTREAM_FORWARD", nil, body, nil)
	require.NoError(t, file.Sync())

	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logBytes), "synthetic-device-account-session")
	require.Contains(t, string(logBytes), "comparison prompt")
}
