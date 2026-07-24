package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const stickySessionPrefix = "sticky_session:"

const claudeOAuthSessionCompanionPrefix = "claude_oauth_session_companion:"
const claudeOAuthRuntimePrefix = "claude_oauth_runtime:"

type gatewayCache struct {
	rdb *redis.Client
}

func NewGatewayCache(rdb *redis.Client) service.GatewayCache {
	return &gatewayCache{rdb: rdb}
}

// buildSessionKey 构建 session key，包含 groupID 实现分组隔离
// 格式: sticky_session:{groupID}:{sessionHash}
func buildSessionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", stickySessionPrefix, groupID, sessionHash)
}

func (c *gatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Get(ctx, key).Int64()
}

func (c *gatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Set(ctx, key, accountID, ttl).Err()
}

func (c *gatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// DeleteSessionAccountID 删除粘性会话与账号的绑定关系。
// 当检测到绑定的账号不可用（如状态错误、禁用、不可调度等）时调用，
// 以便下次请求能够重新选择可用账号。
//
// DeleteSessionAccountID removes the sticky session binding for the given session.
// Called when the bound account becomes unavailable (e.g., error status, disabled,
// or unschedulable), allowing subsequent requests to select a new available account.
func (c *gatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Del(ctx, key).Err()
}

// Compile-time assertion: gatewayCache must implement CyberSessionBlockStore.
var _ service.CyberSessionBlockStore = (*gatewayCache)(nil)
var _ service.ClaudeOAuthSessionActionStore = (*gatewayCache)(nil)
var _ service.ClaudeOAuthRuntimeStore = (*gatewayCache)(nil)

func buildClaudeOAuthSessionActionKey(accountID int64, sessionID, action string) string {
	return fmt.Sprintf(
		"%s%d:%s:%s",
		claudeOAuthSessionCompanionPrefix,
		accountID,
		strings.TrimSpace(sessionID),
		strings.TrimSpace(action),
	)
}

func (c *gatewayCache) TryClaimClaudeOAuthSessionAction(ctx context.Context, accountID int64, sessionID, action string, ttl time.Duration) (bool, error) {
	if accountID <= 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(action) == "" || ttl <= 0 {
		return false, nil
	}
	return c.rdb.SetNX(ctx, buildClaudeOAuthSessionActionKey(accountID, sessionID, action), 1, ttl).Result()
}

func (c *gatewayCache) ReleaseClaudeOAuthSessionAction(ctx context.Context, accountID int64, sessionID, action string) error {
	if accountID <= 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(action) == "" {
		return nil
	}
	return c.rdb.Del(ctx, buildClaudeOAuthSessionActionKey(accountID, sessionID, action)).Err()
}

func buildClaudeOAuthRuntimeKey(accountID int64, runtimeKey string) string {
	return fmt.Sprintf("%s%d:%s", claudeOAuthRuntimePrefix, accountID, strings.TrimSpace(runtimeKey))
}

var getOrCreateClaudeOAuthRuntimeScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 0 then
  redis.call("HSET", KEYS[1], "version", ARGV[1], "data", ARGV[2])
  redis.call("EXPIRE", KEYS[1], ARGV[3])
  return {1, ARGV[2]}
end
redis.call("EXPIRE", KEYS[1], ARGV[3])
return {0, redis.call("HGET", KEYS[1], "data")}
`)

var compareAndSwapClaudeOAuthRuntimeScript = redis.NewScript(`
local current = redis.call("HGET", KEYS[1], "version")
if not current or current ~= ARGV[1] then
  return 0
end
redis.call("HSET", KEYS[1], "version", ARGV[2], "data", ARGV[3])
redis.call("EXPIRE", KEYS[1], ARGV[4])
return 1
`)

func (c *gatewayCache) GetOrCreateClaudeOAuthRuntime(
	ctx context.Context,
	accountID int64,
	runtimeKey string,
	candidate *service.ClaudeOAuthSessionRuntime,
	ttl time.Duration,
) (*service.ClaudeOAuthSessionRuntime, bool, error) {
	if accountID <= 0 || strings.TrimSpace(runtimeKey) == "" || candidate == nil || ttl <= 0 {
		return nil, false, nil
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, false, err
	}
	result, err := getOrCreateClaudeOAuthRuntimeScript.Run(
		ctx,
		c.rdb,
		[]string{buildClaudeOAuthRuntimeKey(accountID, runtimeKey)},
		strconv.FormatInt(candidate.Version, 10),
		string(encoded),
		strconv.FormatInt(int64(ttl/time.Second), 10),
	).Slice()
	if err != nil {
		return nil, false, err
	}
	if len(result) != 2 {
		return nil, false, fmt.Errorf("unexpected Claude OAuth runtime script result")
	}
	created, err := redisResultInt64(result[0])
	if err != nil {
		return nil, false, err
	}
	raw, err := redisResultString(result[1])
	if err != nil {
		return nil, false, err
	}
	runtime := &service.ClaudeOAuthSessionRuntime{}
	if err := json.Unmarshal([]byte(raw), runtime); err != nil {
		return nil, false, err
	}
	return runtime, created == 1, nil
}

func (c *gatewayCache) GetClaudeOAuthRuntime(
	ctx context.Context,
	accountID int64,
	runtimeKey string,
	ttl time.Duration,
) (*service.ClaudeOAuthSessionRuntime, error) {
	if accountID <= 0 || strings.TrimSpace(runtimeKey) == "" {
		return nil, nil
	}
	key := buildClaudeOAuthRuntimeKey(accountID, runtimeKey)
	pipe := c.rdb.TxPipeline()
	dataCmd := pipe.HGet(ctx, key, "data")
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}
	raw, err := dataCmd.Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runtime := &service.ClaudeOAuthSessionRuntime{}
	if err := json.Unmarshal([]byte(raw), runtime); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (c *gatewayCache) CompareAndSwapClaudeOAuthRuntime(
	ctx context.Context,
	accountID int64,
	runtimeKey string,
	expectedVersion int64,
	next *service.ClaudeOAuthSessionRuntime,
	ttl time.Duration,
) (bool, error) {
	if accountID <= 0 || strings.TrimSpace(runtimeKey) == "" || next == nil || ttl <= 0 {
		return false, nil
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	result, err := compareAndSwapClaudeOAuthRuntimeScript.Run(
		ctx,
		c.rdb,
		[]string{buildClaudeOAuthRuntimeKey(accountID, runtimeKey)},
		strconv.FormatInt(expectedVersion, 10),
		strconv.FormatInt(next.Version, 10),
		string(encoded),
		strconv.FormatInt(int64(ttl/time.Second), 10),
	).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func redisResultInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis integer type %T", value)
	}
}

func redisResultString(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	default:
		return "", fmt.Errorf("unexpected Redis string type %T", value)
	}
}

const cyberSessionBlockPrefix = "cyber_session_block:"

// SetCyberSessionBlocked 把被 cyber_policy 命中的会话写入屏蔽表（TTL 自动过期）。
// 存储值 "1" 作为存在标记（IsCyberSessionBlocked 只检查 key 是否存在，不读值）。
func (c *gatewayCache) SetCyberSessionBlocked(ctx context.Context, key string, ttl time.Duration) error {
	return c.rdb.Set(ctx, cyberSessionBlockPrefix+key, "1", ttl).Err()
}

// IsCyberSessionBlocked 查询会话是否在屏蔽表中。
func (c *gatewayCache) IsCyberSessionBlocked(ctx context.Context, key string) (bool, error) {
	n, err := c.rdb.Exists(ctx, cyberSessionBlockPrefix+key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
