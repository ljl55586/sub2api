package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	fingerprintKeyPrefix    = "fingerprint:"
	fingerprintTTL          = 7 * 24 * time.Hour // 7天，配合每24小时懒续期可保持活跃账号永不过期
	maskedSessionKeyPrefix  = "masked_session:"
	maskedSessionTTL        = 15 * time.Minute
	claudeOAuthDevicePrefix = "claude_oauth_device:"
)

// fingerprintKey generates the Redis key for account fingerprint cache.
func fingerprintKey(accountID int64) string {
	return fmt.Sprintf("%s%d", fingerprintKeyPrefix, accountID)
}

// maskedSessionKey generates the Redis key for masked session ID cache.
func maskedSessionKey(accountID int64) string {
	return fmt.Sprintf("%s%d", maskedSessionKeyPrefix, accountID)
}

func claudeOAuthDeviceKey(accountID int64) string {
	return fmt.Sprintf("%s%d", claudeOAuthDevicePrefix, accountID)
}

type identityCache struct {
	rdb *redis.Client
}

var _ service.FingerprintAtomicClaimStore = (*identityCache)(nil)
var _ service.FingerprintAtomicRepairStore = (*identityCache)(nil)
var _ service.MaskedSessionIDAtomicStore = (*identityCache)(nil)
var _ service.AccountDeviceStore = (*identityCache)(nil)

func NewIdentityCache(rdb *redis.Client) service.IdentityCache {
	return &identityCache{rdb: rdb}
}

func (c *identityCache) GetFingerprint(ctx context.Context, accountID int64) (*service.Fingerprint, error) {
	key := fingerprintKey(accountID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var fp service.Fingerprint
	if err := json.Unmarshal([]byte(val), &fp); err != nil {
		return nil, err
	}
	return &fp, nil
}

func (c *identityCache) SetFingerprint(ctx context.Context, accountID int64, fp *service.Fingerprint) error {
	key := fingerprintKey(accountID)
	val, err := json.Marshal(fp)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, val, fingerprintTTL).Err()
}

// TryClaimFingerprint atomically writes a new account fingerprint only when
// the cache key is absent. This makes concurrent cache misses converge on one
// ClientID instead of allowing the last normal SET to win.
func (c *identityCache) TryClaimFingerprint(ctx context.Context, accountID int64, fp *service.Fingerprint) (bool, error) {
	key := fingerprintKey(accountID)
	val, err := json.Marshal(fp)
	if err != nil {
		return false, err
	}
	return c.rdb.SetNX(ctx, key, val, fingerprintTTL).Result()
}

func (c *identityCache) GetClaudeOAuthDeviceID(ctx context.Context, accountID int64) (string, error) {
	value, err := c.rdb.Get(ctx, claudeOAuthDeviceKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return value, err
}

func (c *identityCache) TryClaimClaudeOAuthDeviceID(ctx context.Context, accountID int64, candidate string) (bool, error) {
	if accountID <= 0 || candidate == "" {
		return false, nil
	}
	// A zero expiration intentionally makes the device identity durable and
	// independent from the seven-day SDK fingerprint cache.
	return c.rdb.SetNX(ctx, claudeOAuthDeviceKey(accountID), candidate, 0).Result()
}

// EnsureFingerprintClientID atomically repairs a legacy fingerprint record
// whose ClientID is empty. WATCH prevents two concurrent repairers from
// returning different fallback device IDs; SET with KeepTTL preserves the
// record's existing expiry rather than turning a repair into a TTL change.
func (c *identityCache) EnsureFingerprintClientID(ctx context.Context, accountID int64, candidate string) (*service.Fingerprint, error) {
	key := fingerprintKey(accountID)
	for attempt := 0; attempt < 3; attempt++ {
		var repaired *service.Fingerprint
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Result()
			if errors.Is(err, redis.Nil) {
				return nil
			}
			if err != nil {
				return err
			}

			var fp service.Fingerprint
			if err := json.Unmarshal([]byte(val), &fp); err != nil {
				return err
			}
			if fp.ClientID != "" {
				repaired = &fp
				return nil
			}

			fp.ClientID = candidate
			fp.UpdatedAt = time.Now().Unix()
			encoded, err := json.Marshal(&fp)
			if err != nil {
				return err
			}
			if _, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, encoded, redis.KeepTTL)
				return nil
			}); err != nil {
				return err
			}
			repaired = &fp
			return nil
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return repaired, nil
	}
	return nil, redis.TxFailedErr
}

func (c *identityCache) GetMaskedSessionID(ctx context.Context, accountID int64) (string, error) {
	key := maskedSessionKey(accountID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

// GetAndRefreshMaskedSessionID uses Redis GETEX so a reader never observes a
// nearly-expired value and later overwrites a newer claimant while refreshing
// the TTL. GETEX performs the read and renewal as one Redis command.
func (c *identityCache) GetAndRefreshMaskedSessionID(ctx context.Context, accountID int64) (string, error) {
	key := maskedSessionKey(accountID)
	val, err := c.rdb.GetEx(ctx, key, maskedSessionTTL).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

func (c *identityCache) SetMaskedSessionID(ctx context.Context, accountID int64, sessionID string) error {
	key := maskedSessionKey(accountID)
	return c.rdb.Set(ctx, key, sessionID, maskedSessionTTL).Err()
}

// TryClaimMaskedSessionID atomically initializes an account-scoped session
// mask. SET NX ensures concurrent cold-cache callers converge on one value.
func (c *identityCache) TryClaimMaskedSessionID(ctx context.Context, accountID int64, sessionID string) (bool, error) {
	key := maskedSessionKey(accountID)
	return c.rdb.SetNX(ctx, key, sessionID, maskedSessionTTL).Result()
}
