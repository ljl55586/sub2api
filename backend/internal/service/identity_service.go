package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 预编译正则表达式（避免每次调用重新编译）
var (
	// 匹配 User-Agent 版本号: xxx/x.y.z
	userAgentVersionRegex = regexp.MustCompile(`/(\d+)\.(\d+)\.(\d+)`)
)

// 默认指纹值（当客户端未提供时使用）
var defaultFingerprint = Fingerprint{
	UserAgent:               "claude-cli/" + claude.CLICurrentVersion + " (external, cli)",
	StainlessLang:           "js",
	StainlessPackageVersion: "0.94.0",
	StainlessOS:             claude.DefaultStainlessOS,
	StainlessArch:           "arm64",
	StainlessRuntime:        "node",
	StainlessRuntimeVersion: "v26.3.0",
}

// Fingerprint represents account fingerprint data
type Fingerprint struct {
	ClientID                string
	UserAgent               string
	StainlessLang           string
	StainlessPackageVersion string
	StainlessOS             string
	StainlessArch           string
	StainlessRuntime        string
	StainlessRuntimeVersion string
	UpdatedAt               int64 `json:",omitempty"` // Unix timestamp，用于判断是否需要续期TTL
}

// AccountIdentity contains the account-scoped fields used by metadata.user_id.
// DeviceID and AccountUUID must remain stable for one OAuth account across
// downstream clients and requests.
type AccountIdentity struct {
	DeviceID    string
	AccountUUID string
}

var ErrIncompleteAccountIdentity = errors.New("incomplete account identity")

// IdentityCache defines cache operations for identity service
type IdentityCache interface {
	// GetFingerprint returns (nil, nil) when the account has no cached fingerprint
	// or its cached fingerprint has expired. Infrastructure and decode errors are
	// returned as errors.
	GetFingerprint(ctx context.Context, accountID int64) (*Fingerprint, error)
	SetFingerprint(ctx context.Context, accountID int64, fp *Fingerprint) error
	// GetMaskedSessionID 获取固定的会话ID（用于会话ID伪装功能）
	// 返回的 sessionID 是一个 UUID 格式的字符串
	// 如果不存在或已过期（15分钟无请求），返回空字符串
	GetMaskedSessionID(ctx context.Context, accountID int64) (string, error)
	// SetMaskedSessionID 设置固定的会话ID，TTL 为 15 分钟
	// 每次调用都会刷新 TTL
	SetMaskedSessionID(ctx context.Context, accountID int64, sessionID string) error
}

// MaskedSessionIDAtomicClaimStore is an optional IdentityCache capability for
// atomically creating an account-scoped masked session ID with the normal mask
// TTL. It intentionally stays separate from IdentityCache so existing cache
// implementations remain compatible.
type MaskedSessionIDAtomicClaimStore interface {
	// TryClaimMaskedSessionID stores sessionID only when the account has no
	// current mask. It returns true to the single winning caller.
	TryClaimMaskedSessionID(ctx context.Context, accountID int64, sessionID string) (bool, error)
}

// MaskedSessionIDAtomicReadRefreshStore atomically reads an existing mask and
// refreshes its TTL. A separate GET followed by SET is unsafe at the expiry
// boundary: a newly claimed value could be overwritten by a stale reader.
type MaskedSessionIDAtomicReadRefreshStore interface {
	GetAndRefreshMaskedSessionID(ctx context.Context, accountID int64) (string, error)
}

// MaskedSessionIDAtomicStore provides both operations required to keep an
// account-scoped mask stable through concurrent cold starts and TTL renewal.
// It remains optional so existing IdentityCache implementations are source
// compatible, while production Redis uses the full atomic path.
type MaskedSessionIDAtomicStore interface {
	MaskedSessionIDAtomicClaimStore
	MaskedSessionIDAtomicReadRefreshStore
}

// FingerprintAtomicClaimStore atomically persists a newly generated account
// fingerprint only when no fingerprint exists. It prevents concurrent cache
// misses from assigning more than one fallback device/client identity.
type FingerprintAtomicClaimStore interface {
	TryClaimFingerprint(ctx context.Context, accountID int64, fp *Fingerprint) (bool, error)
}

// FingerprintAtomicRepairStore atomically assigns a ClientID to a legacy
// fingerprint record that exists but has an empty ClientID. It returns the
// canonical stored fingerprint so concurrent repairers converge before using
// a fallback device identity on the wire.
type FingerprintAtomicRepairStore interface {
	EnsureFingerprintClientID(ctx context.Context, accountID int64, candidate string) (*Fingerprint, error)
}

// AccountDeviceStore persists the Claude OAuth device identity independently
// from the expiring SDK fingerprint. Production Redis stores this key without
// a TTL so inactive accounts do not silently acquire a new device ID.
type AccountDeviceStore interface {
	GetClaudeOAuthDeviceID(ctx context.Context, accountID int64) (string, error)
	TryClaimClaudeOAuthDeviceID(ctx context.Context, accountID int64, candidate string) (bool, error)
}

// IdentityService 管理OAuth账号的请求身份指纹
type IdentityService struct {
	cache IdentityCache
}

// NewIdentityService 创建新的IdentityService
func NewIdentityService(cache IdentityCache) *IdentityService {
	return &IdentityService{cache: cache}
}

// GetOrCreateFingerprint 获取或创建账号的指纹
// 如果缓存存在，检测user-agent版本，新版本则更新
// 如果缓存不存在，生成随机ClientID并从请求头创建指纹，然后缓存
func (s *IdentityService) GetOrCreateFingerprint(ctx context.Context, accountID int64, headers http.Header) (*Fingerprint, error) {
	if s == nil || s.cache == nil {
		return nil, errors.New("identity cache is unavailable")
	}

	// 尝试从缓存获取指纹
	cached, err := s.cache.GetFingerprint(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get fingerprint for account %d: %w", accountID, err)
	}
	if cached != nil {
		return s.refreshCachedFingerprint(ctx, accountID, cached, headers)
	}

	// 缓存不存在，创建新指纹
	fp := s.createFingerprintFromHeaders(headers)

	// 生成随机ClientID
	fp.ClientID = generateClientID()
	fp.UpdatedAt = time.Now().Unix()

	// 有原子 claim 能力时，cold-cache 并发调用必须收敛到单个持久指纹。
	if claimer, ok := s.cache.(FingerprintAtomicClaimStore); ok {
		claimed, claimErr := claimer.TryClaimFingerprint(ctx, accountID, fp)
		if claimErr != nil {
			return nil, fmt.Errorf("claim fingerprint for account %d: %w", accountID, claimErr)
		}
		if claimed {
			logger.LegacyPrintf("service.identity", "Created new fingerprint for account %d with client_id: %s", accountID, fp.ClientID)
			return fp, nil
		}

		// A concurrent caller won the cold-cache race. Always use its persisted
		// value instead of returning this request's random candidate.
		cached, err = s.cache.GetFingerprint(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("get winning fingerprint for account %d: %w", accountID, err)
		}
		if cached == nil {
			return nil, fmt.Errorf("winning fingerprint for account %d is unavailable", accountID)
		}
		return s.refreshCachedFingerprint(ctx, accountID, cached, headers)
	}

	// 保存到缓存（7天TTL，每24小时自动续期）。Legacy IdentityCache
	// implementations keep their historical non-atomic behavior for source
	// compatibility; the production Redis cache implements the atomic claim.
	if err := s.cache.SetFingerprint(ctx, accountID, fp); err != nil {
		return nil, fmt.Errorf("persist fingerprint for account %d: %w", accountID, err)
	}

	logger.LegacyPrintf("service.identity", "Created new fingerprint for account %d with client_id: %s", accountID, fp.ClientID)
	return fp, nil
}

func (s *IdentityService) refreshCachedFingerprint(ctx context.Context, accountID int64, cached *Fingerprint, headers http.Header) (*Fingerprint, error) {
	needWrite := false
	if strings.TrimSpace(cached.ClientID) == "" {
		if repairer, ok := s.cache.(FingerprintAtomicRepairStore); ok {
			repaired, err := repairer.EnsureFingerprintClientID(ctx, accountID, generateClientID())
			if err != nil {
				return nil, fmt.Errorf("repair fingerprint client ID for account %d: %w", accountID, err)
			}
			if repaired == nil || strings.TrimSpace(repaired.ClientID) == "" {
				return nil, fmt.Errorf("repaired fingerprint for account %d is unavailable", accountID)
			}
			cached = repaired
		} else {
			// Legacy cache implementations cannot compare-and-set a partially
			// initialized record. Use a deterministic repair value so concurrent
			// callers still converge even if their normal Set operations race.
			cached.ClientID = generateStableFallbackClientID(accountID, cached)
			needWrite = true
		}
	}

	// 检查客户端的user-agent是否是更新版本
	clientUA := headers.Get("User-Agent")
	if clientUA != "" && isNewerVersion(clientUA, cached.UserAgent) {
		// 版本升级：merge 语义 — 仅更新请求中实际携带的字段，保留缓存值
		// 避免缺失的头被硬编码默认值覆盖（如新 CLI 版本 + 旧 SDK 默认值的不一致）
		mergeHeadersIntoFingerprint(cached, headers)
		needWrite = true
		logger.LegacyPrintf("service.identity", "Updated fingerprint for account %d: %s (merge update)", accountID, clientUA)
	} else if time.Since(time.Unix(cached.UpdatedAt, 0)) > 24*time.Hour {
		// 距上次写入超过24小时，续期TTL
		needWrite = true
	}

	if needWrite {
		cached.UpdatedAt = time.Now().Unix()
		if err := s.cache.SetFingerprint(ctx, accountID, cached); err != nil {
			return nil, fmt.Errorf("persist fingerprint for account %d: %w", accountID, err)
		}
	}
	return cached, nil
}

func generateStableFallbackClientID(accountID int64, fp *Fingerprint) string {
	if fp == nil {
		hash := sha256.Sum256([]byte(fmt.Sprintf("legacy-fingerprint:%d", accountID)))
		return hex.EncodeToString(hash[:])
	}
	seed := strings.Join([]string{
		"legacy-fingerprint",
		strconv.FormatInt(accountID, 10),
		fp.UserAgent,
		fp.StainlessLang,
		fp.StainlessPackageVersion,
		fp.StainlessOS,
		fp.StainlessArch,
		fp.StainlessRuntime,
		fp.StainlessRuntimeVersion,
	}, "\x00")
	hash := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(hash[:])
}

// ResolveStableAccountIdentity resolves the account-scoped identity used for
// generated metadata.user_id values. It refuses incomplete identities instead
// of falling back to a request-local random device ID.
func (s *IdentityService) ResolveStableAccountIdentity(ctx context.Context, account *Account, headers http.Header) (AccountIdentity, error) {
	if account == nil {
		return AccountIdentity{}, fmt.Errorf("%w: account is nil", ErrIncompleteAccountIdentity)
	}

	accountUUID := strings.TrimSpace(account.GetExtraString("account_uuid"))
	if accountUUID == "" {
		return AccountIdentity{}, fmt.Errorf("%w: account_uuid", ErrIncompleteAccountIdentity)
	}

	deviceID := strings.TrimSpace(account.GetClaudeUserID())
	if deviceID == "" {
		if store, ok := s.cache.(AccountDeviceStore); ok {
			var err error
			deviceID, err = store.GetClaudeOAuthDeviceID(ctx, account.ID)
			if err != nil {
				return AccountIdentity{}, fmt.Errorf("resolve persistent device ID for account %d: %w", account.ID, err)
			}
			if deviceID = strings.TrimSpace(deviceID); deviceID == "" {
				fp, fingerprintErr := s.GetOrCreateFingerprint(ctx, account.ID, headers)
				if fingerprintErr != nil {
					return AccountIdentity{}, fingerprintErr
				}
				candidate := ""
				if fp != nil {
					candidate = strings.TrimSpace(fp.ClientID)
				}
				deviceID, err = getOrCreateClaudeOAuthDeviceID(ctx, store, account.ID, candidate)
				if err != nil {
					return AccountIdentity{}, fmt.Errorf("resolve persistent device ID for account %d: %w", account.ID, err)
				}
			}
		} else {
			fp, err := s.GetOrCreateFingerprint(ctx, account.ID, headers)
			if err != nil {
				return AccountIdentity{}, err
			}
			if fp != nil {
				deviceID = strings.TrimSpace(fp.ClientID)
			}
		}
	}
	if deviceID == "" {
		return AccountIdentity{}, fmt.Errorf("%w: device_id", ErrIncompleteAccountIdentity)
	}

	return AccountIdentity{DeviceID: deviceID, AccountUUID: accountUUID}, nil
}

func getOrCreateClaudeOAuthDeviceID(ctx context.Context, store AccountDeviceStore, accountID int64, candidate string) (string, error) {
	if store == nil || accountID <= 0 {
		return "", nil
	}
	current, err := store.GetClaudeOAuthDeviceID(ctx, accountID)
	if err != nil {
		return "", err
	}
	if current = strings.TrimSpace(current); current != "" {
		return current, nil
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		candidate = generateClientID()
	}
	claimed, err := store.TryClaimClaudeOAuthDeviceID(ctx, accountID, candidate)
	if err != nil {
		return "", err
	}
	if claimed {
		return candidate, nil
	}
	current, err = store.GetClaudeOAuthDeviceID(ctx, accountID)
	if err != nil {
		return "", err
	}
	if current = strings.TrimSpace(current); current == "" {
		return "", errors.New("winning persistent device ID is unavailable")
	}
	return current, nil
}

// createFingerprintFromHeaders 从请求头创建指纹
func (s *IdentityService) createFingerprintFromHeaders(headers http.Header) *Fingerprint {
	fp := &Fingerprint{}

	// 获取User-Agent
	if ua := headers.Get("User-Agent"); ua != "" {
		fp.UserAgent = ua
	} else {
		fp.UserAgent = defaultFingerprint.UserAgent
	}

	// 获取x-stainless-*头，如果没有则使用默认值
	fp.StainlessLang = getHeaderOrDefault(headers, "X-Stainless-Lang", defaultFingerprint.StainlessLang)
	fp.StainlessPackageVersion = getHeaderOrDefault(headers, "X-Stainless-Package-Version", defaultFingerprint.StainlessPackageVersion)
	fp.StainlessOS = getHeaderOrDefault(headers, "X-Stainless-OS", defaultFingerprint.StainlessOS)
	fp.StainlessArch = getHeaderOrDefault(headers, "X-Stainless-Arch", defaultFingerprint.StainlessArch)
	fp.StainlessRuntime = getHeaderOrDefault(headers, "X-Stainless-Runtime", defaultFingerprint.StainlessRuntime)
	fp.StainlessRuntimeVersion = getHeaderOrDefault(headers, "X-Stainless-Runtime-Version", defaultFingerprint.StainlessRuntimeVersion)

	return fp
}

// mergeHeadersIntoFingerprint 将请求头中实际存在的字段合并到现有指纹中（用于版本升级场景）
// 关键语义：请求中有的字段 → 用新值覆盖；缺失的头 → 保留缓存中的已有值
// 与 createFingerprintFromHeaders 的区别：后者用于首次创建，缺失头回退到 defaultFingerprint；
// 本函数用于升级更新，缺失头保留缓存值，避免将已知的真实值退化为硬编码默认值
func mergeHeadersIntoFingerprint(fp *Fingerprint, headers http.Header) {
	// User-Agent：版本升级的触发条件，一定存在
	if ua := headers.Get("User-Agent"); ua != "" {
		fp.UserAgent = ua
	}
	// X-Stainless-* 头：仅在请求中实际携带时才更新，否则保留缓存值
	mergeHeader(headers, "X-Stainless-Lang", &fp.StainlessLang)
	mergeHeader(headers, "X-Stainless-Package-Version", &fp.StainlessPackageVersion)
	mergeHeader(headers, "X-Stainless-OS", &fp.StainlessOS)
	mergeHeader(headers, "X-Stainless-Arch", &fp.StainlessArch)
	mergeHeader(headers, "X-Stainless-Runtime", &fp.StainlessRuntime)
	mergeHeader(headers, "X-Stainless-Runtime-Version", &fp.StainlessRuntimeVersion)
}

// mergeHeader 如果请求头中存在该字段则更新目标值，否则保留原值
func mergeHeader(headers http.Header, key string, target *string) {
	if v := headers.Get(key); v != "" {
		*target = v
	}
}

// getHeaderOrDefault 获取header值，如果不存在则返回默认值
func getHeaderOrDefault(headers http.Header, key, defaultValue string) string {
	if v := headers.Get(key); v != "" {
		return v
	}
	return defaultValue
}

// ApplyFingerprint 将指纹应用到请求头（覆盖原有的x-stainless-*头）
// 使用 setHeaderRaw 保持原始大小写（如 X-Stainless-OS 而非 X-Stainless-Os）
func (s *IdentityService) ApplyFingerprint(req *http.Request, fp *Fingerprint) {
	if fp == nil {
		return
	}

	// 设置user-agent
	if fp.UserAgent != "" {
		setHeaderRaw(req.Header, "User-Agent", fp.UserAgent)
	}

	// 设置x-stainless-*头（保持与 claude.DefaultHeaders 一致的大小写）
	if fp.StainlessLang != "" {
		setHeaderRaw(req.Header, "X-Stainless-Lang", fp.StainlessLang)
	}
	if fp.StainlessPackageVersion != "" {
		setHeaderRaw(req.Header, "X-Stainless-Package-Version", fp.StainlessPackageVersion)
	}
	if fp.StainlessOS != "" {
		setHeaderRaw(req.Header, "X-Stainless-OS", fp.StainlessOS)
	}
	if fp.StainlessArch != "" {
		setHeaderRaw(req.Header, "X-Stainless-Arch", fp.StainlessArch)
	}
	if fp.StainlessRuntime != "" {
		setHeaderRaw(req.Header, "X-Stainless-Runtime", fp.StainlessRuntime)
	}
	if fp.StainlessRuntimeVersion != "" {
		setHeaderRaw(req.Header, "X-Stainless-Runtime-Version", fp.StainlessRuntimeVersion)
	}
}

// RewriteUserID 重写body中的metadata.user_id
// 支持旧拼接格式和新 JSON 格式的 user_id 解析，
// 根据 fingerprintUA 版本选择输出格式。
//
// 重要：此函数使用 json.RawMessage 保留其他字段的原始字节，
// 避免重新序列化导致 thinking 块等内容被修改。
func (s *IdentityService) RewriteUserID(body []byte, accountID int64, accountUUID, cachedClientID, fingerprintUA string) ([]byte, error) {
	if len(body) == 0 || accountUUID == "" || cachedClientID == "" {
		return body, nil
	}

	metadata := gjson.GetBytes(body, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return body, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return body, nil
	}

	userIDResult := metadata.Get("user_id")
	if !userIDResult.Exists() || userIDResult.Type != gjson.String {
		return body, nil
	}
	userID := userIDResult.String()
	if userID == "" {
		return body, nil
	}

	// 解析 user_id（兼容旧拼接格式和新 JSON 格式）
	parsed := ParseMetadataUserID(userID)
	if parsed == nil {
		return body, nil
	}

	sessionTail := parsed.SessionID // 原始session UUID

	// 生成新的session hash: SHA256(accountID::sessionTail) -> UUID格式
	seed := fmt.Sprintf("%d::%s", accountID, sessionTail)
	newSessionHash := generateUUIDFromSeed(seed)

	// 根据客户端版本选择输出格式
	version := ExtractCLIVersion(fingerprintUA)
	newUserID := FormatMetadataUserID(cachedClientID, accountUUID, newSessionHash, version)
	if newUserID == userID {
		return body, nil
	}

	newBody, err := sjson.SetBytes(body, "metadata.user_id", newUserID)
	if err != nil {
		return body, nil
	}
	return newBody, nil
}

// RewriteUserIDWithMasking 重写body中的metadata.user_id，支持会话ID伪装
// 如果账号启用了会话ID伪装（session_id_masking_enabled），
// 则在完成常规重写后，将 session 部分替换为固定的伪装ID（15分钟内保持不变）
//
// 重要：此函数使用 json.RawMessage 保留其他字段的原始字节，
// 避免重新序列化导致 thinking 块等内容被修改。
func (s *IdentityService) RewriteUserIDWithMasking(ctx context.Context, body []byte, account *Account, accountUUID, cachedClientID, fingerprintUA string) ([]byte, error) {
	// 先执行常规的 RewriteUserID 逻辑
	newBody, err := s.RewriteUserID(body, account.ID, accountUUID, cachedClientID, fingerprintUA)
	if err != nil {
		return newBody, err
	}

	// 检查是否启用会话ID伪装
	if !account.IsSessionIDMaskingEnabled() {
		return newBody, nil
	}

	metadata := gjson.GetBytes(newBody, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return newBody, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return newBody, nil
	}

	userIDResult := metadata.Get("user_id")
	if !userIDResult.Exists() || userIDResult.Type != gjson.String {
		return newBody, nil
	}
	userID := userIDResult.String()
	if userID == "" {
		return newBody, nil
	}

	// 解析已重写的 user_id
	uidParsed := ParseMetadataUserID(userID)
	if uidParsed == nil {
		return newBody, nil
	}

	maskedSessionID, err := s.GetOrCreateMaskedSessionID(ctx, account.ID)
	if err != nil {
		return newBody, err
	}

	// 用 FormatMetadataUserID 重建（保持与 RewriteUserID 相同的格式）
	version := ExtractCLIVersion(fingerprintUA)
	newUserID := FormatMetadataUserID(uidParsed.DeviceID, uidParsed.AccountUUID, maskedSessionID, version)

	slog.Debug("session_id_masking_applied",
		"account_id", account.ID,
		"before", userID,
		"after", newUserID,
	)

	if newUserID == userID {
		return newBody, nil
	}

	maskedBody, setErr := sjson.SetBytes(newBody, "metadata.user_id", newUserID)
	if setErr != nil {
		return newBody, nil
	}
	return maskedBody, nil
}

// GetOrCreateMaskedSessionID resolves the account-scoped session mask and
// refreshes its TTL. A storage error is returned instead of falling back to an
// unmasked session: callers that send multiple related requests must never
// split one lifecycle across masked and unmasked identities.
func (s *IdentityService) GetOrCreateMaskedSessionID(ctx context.Context, accountID int64) (string, error) {
	if s == nil || s.cache == nil {
		return "", errors.New("identity cache is unavailable")
	}
	if atomicStore, ok := s.cache.(MaskedSessionIDAtomicStore); ok {
		return s.getOrCreateMaskedSessionIDAtomically(ctx, accountID, atomicStore)
	}
	if claimer, ok := s.cache.(MaskedSessionIDAtomicClaimStore); ok {
		return s.getOrCreateMaskedSessionIDWithAtomicClaim(ctx, accountID, claimer)
	}

	maskedSessionID, err := s.cache.GetMaskedSessionID(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("get masked session ID for account %d: %w", accountID, err)
	}
	if maskedSessionID == "" {
		// Legacy IdentityCache implementations do not support atomic claims.
		// Keep their historical behavior rather than making the new capability a
		// breaking interface requirement.
		maskedSessionID = generateRandomUUID()
		logger.LegacyPrintf("service.identity", "Generated new masked session ID for account %d: %s", accountID, maskedSessionID)
	}
	if err := s.cache.SetMaskedSessionID(ctx, accountID, maskedSessionID); err != nil {
		return "", fmt.Errorf("persist masked session ID for account %d: %w", accountID, err)
	}
	return maskedSessionID, nil
}

// getOrCreateMaskedSessionIDWithAtomicClaim preserves the old SETNX-only
// extension contract without reintroducing a stale GET → SET TTL refresh. A
// cache that cannot atomically refresh may let the original TTL expire; that
// is preferable to overwriting a newer winner and splitting a live session.
func (s *IdentityService) getOrCreateMaskedSessionIDWithAtomicClaim(ctx context.Context, accountID int64, claimer MaskedSessionIDAtomicClaimStore) (string, error) {
	maskedSessionID, err := s.cache.GetMaskedSessionID(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("get masked session ID for account %d: %w", accountID, err)
	}
	if maskedSessionID != "" {
		return maskedSessionID, nil
	}

	candidate := generateRandomUUID()
	claimed, claimErr := claimer.TryClaimMaskedSessionID(ctx, accountID, candidate)
	if claimErr != nil {
		return "", fmt.Errorf("claim masked session ID for account %d: %w", accountID, claimErr)
	}
	if claimed {
		logger.LegacyPrintf("service.identity", "Claimed new masked session ID for account %d: %s", accountID, candidate)
		return candidate, nil
	}

	maskedSessionID, err = s.cache.GetMaskedSessionID(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("get winning masked session ID for account %d: %w", accountID, err)
	}
	if maskedSessionID == "" {
		return "", fmt.Errorf("winning masked session ID for account %d is unavailable", accountID)
	}
	return maskedSessionID, nil
}

func (s *IdentityService) getOrCreateMaskedSessionIDAtomically(ctx context.Context, accountID int64, store MaskedSessionIDAtomicStore) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		maskedSessionID, err := store.GetAndRefreshMaskedSessionID(ctx, accountID)
		if err != nil {
			return "", fmt.Errorf("get and refresh masked session ID for account %d: %w", accountID, err)
		}
		if maskedSessionID != "" {
			return maskedSessionID, nil
		}

		candidate := generateRandomUUID()
		claimed, claimErr := store.TryClaimMaskedSessionID(ctx, accountID, candidate)
		if claimErr != nil {
			return "", fmt.Errorf("claim masked session ID for account %d: %w", accountID, claimErr)
		}
		if claimed {
			logger.LegacyPrintf("service.identity", "Claimed new masked session ID for account %d: %s", accountID, candidate)
			return candidate, nil
		}
	}

	return "", fmt.Errorf("winning masked session ID for account %d is unavailable", accountID)
}

// generateRandomUUID 生成随机 UUID v4 格式字符串
func generateRandomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback: 使用时间戳生成
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		b = h[:16]
	}

	// 设置 UUID v4 版本和变体位
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateClientID 生成64位十六进制客户端ID（32字节随机数）
func generateClientID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 极罕见的情况，使用时间戳+固定值作为fallback
		logger.LegacyPrintf("service.identity", "Warning: crypto/rand.Read failed: %v, using fallback", err)
		// 使用SHA256(当前纳秒时间)作为fallback
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(h[:])
	}
	return hex.EncodeToString(b)
}

// generateUUIDFromSeed 从种子生成确定性UUID v4格式字符串
func generateUUIDFromSeed(seed string) string {
	hash := sha256.Sum256([]byte(seed))
	bytes := hash[:16]

	// 设置UUID v4版本和变体位
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// parseUserAgentVersion 解析user-agent版本号
// 例如：claude-cli/2.1.2 -> (2, 1, 2)
func parseUserAgentVersion(ua string) (major, minor, patch int, ok bool) {
	// 匹配 xxx/x.y.z 格式
	matches := userAgentVersionRegex.FindStringSubmatch(ua)
	if len(matches) != 4 {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(matches[1])
	minor, _ = strconv.Atoi(matches[2])
	patch, _ = strconv.Atoi(matches[3])
	return major, minor, patch, true
}

// extractProduct 提取 User-Agent 中 "/" 前的产品名
// 例如：claude-cli/2.1.22 (external, cli) -> "claude-cli"
func extractProduct(ua string) string {
	if idx := strings.Index(ua, "/"); idx > 0 {
		return strings.ToLower(ua[:idx])
	}
	return ""
}

// isNewerVersion 比较版本号，判断newUA是否比cachedUA更新
// 要求产品名一致（防止浏览器 UA 如 Mozilla/5.0 误判为更新版本）
func isNewerVersion(newUA, cachedUA string) bool {
	// 校验产品名一致性
	newProduct := extractProduct(newUA)
	cachedProduct := extractProduct(cachedUA)
	if newProduct == "" || cachedProduct == "" || newProduct != cachedProduct {
		return false
	}

	newMajor, newMinor, newPatch, newOk := parseUserAgentVersion(newUA)
	cachedMajor, cachedMinor, cachedPatch, cachedOk := parseUserAgentVersion(cachedUA)

	if !newOk || !cachedOk {
		return false
	}

	// 比较版本号
	if newMajor > cachedMajor {
		return true
	}
	if newMajor < cachedMajor {
		return false
	}

	if newMinor > cachedMinor {
		return true
	}
	if newMinor < cachedMinor {
		return false
	}

	return newPatch > cachedPatch
}
