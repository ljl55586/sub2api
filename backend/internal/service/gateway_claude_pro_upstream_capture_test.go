//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// captureLogDir 是需求指定的抓包日志输出目录。
const captureLogDir = "/Users/ling/Demo/PacketCapture/sub2api/claude"

// TestCaptureClaudeProUpstreamRequest 复现「配置 Claude Pro（OAuth）账号后，用户用 curl
// 调用 sub2api 提问」的完整转发流程，并把「客户端原始请求」与「sub2api 转发到上游
// (api.anthropic.com) 的请求」快照写入 captureLogDir/log_日期_时间.log。
//
// 目的：
//  1. 学习 sub2api 对 Claude Pro 账号向上游转发的真实请求头 / 请求体长什么样；
//  2. 验证 gateway 转发逻辑（OAuth mimic Claude Code 路径）正确无误。
//
// 该用例走的是与生产环境 GatewayService.Forward() 完全一致的核心函数：
//   - rewriteSystemForNonClaudeCode      : 把第三方客户端 system 迁移到 messages，
//     system 仅保留 Claude Code 标识（避免被上游判第三方）
//   - normalizeClaudeOAuthRequestBody    : OAuth body 归一化（cache_control 等）
//   - buildUpstreamRequest(mimic=true)   : 构造最终上游请求（鉴权头 / 指纹头 / anthropic-beta）
//     并内部触发 UPSTREAM_FORWARD 调试快照
func TestCaptureClaudeProUpstreamRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// ---- 1. 构造「用户用 curl 调用 sub2api」的原始请求 ----
	//
	// 等价 curl：
	//   curl https://<你的 sub2api 域名>/v1/messages \
	//     -H "Authorization: Bearer sk-<你在 sub2api 里创建的 API Key>" \
	//     -H "Content-Type: application/json" \
	//     -H "anthropic-version: 2023-06-01" \
	//     -d '{"model":"claude-sonnet-4-6","max_tokens":1024,"stream":true,
	//          "messages":[{"role":"user","content":"用一句话介绍你自己"}]}'
	clientBody := []byte(`{` +
		`"model":"claude-sonnet-4-6",` +
		`"max_tokens":1024,` +
		`"stream":true,` +
		`"messages":[{"role":"user","content":"用一句话介绍你自己"}]` +
		`}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// 一个普通第三方客户端（非官方 claude-cli）会带自己的 UA。
	c.Request.Header.Set("User-Agent", "curl/8.4.0")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Anthropic-Version", "2023-06-01")

	// ---- 2. 配置一个 Claude Pro（OAuth）账号 ----
	account := &Account{
		ID:          1001,
		Name:        "claude-pro-oauth-demo",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "sk-ant-oat01-DEMO-ACCESS-TOKEN"},
		Status:      StatusActive,
		Schedulable: true,
	}
	// mimicClaudeCode: OAuth 账号 + 非官方 CLI 客户端 → 走 Claude Code 伪装路径
	// （与 Forward() 中 shouldMimicClaudeCode = account.IsOAuth() && !isClaudeCode 一致）。
	const mimicClaudeCode = true
	const tokenType = "oauth"
	const modelID = "claude-sonnet-4-6"
	const reqStream = true
	token := account.Credentials["access_token"].(string)

	// ---- 3. 复现 Forward() 的 OAuth mimic body 改写 ----
	// 3a. 重写 system：把第三方 system 迁到 messages，system 只留 Claude Code 标识。
	body := rewriteSystemForNonClaudeCode(clientBody, nil)
	// 3b. OAuth body 归一化（cache_control 等）。
	body, _ = normalizeClaudeOAuthRequestBody(body, modelID, claudeOAuthNormalizeOptions{})

	// ---- 4. 打开需求指定的日志文件并接入现成的调试快照机制 ----
	logPath := filepath.Join(captureLogDir, "log_"+time.Now().Format("20060102_150405")+".log")
	svc := &GatewayService{cfg: &config.Config{}}
	svc.initDebugGatewayBodyFile(logPath) // 内部会 MkdirAll 父目录

	// 写入一段说明性抬头，便于日后阅读该日志。
	writeCaptureBanner(t, svc, logPath, account, modelID)

	// 4a. 记录客户端原始请求快照（与 Forward() 中 CLIENT_ORIGINAL 一致）。
	svc.debugLogGatewaySnapshot("CLIENT_ORIGINAL", c.Request.Header, clientBody, map[string]string{
		"account":      fmt.Sprintf("%d(%s)", account.ID, account.Name),
		"account_type": string(account.Type),
		"model":        modelID,
		"stream":       strconv.FormatBool(reqStream),
		"note":         "用户用 curl 打到 sub2api 的原始请求（鉴权头为 sub2api 自己的 API Key）",
	})

	// ---- 5. 构造上游请求（内部会写入 UPSTREAM_FORWARD 快照）----
	req, outBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		token, tokenType, modelID, reqStream, mimicClaudeCode,
	)
	require.NoError(t, err)

	// ---- 6. 断言转发逻辑正确（这些就是"检查逻辑"的验证项）----

	// 6a. 目标 URL：Claude 官方 messages 端点。
	require.Equal(t, "https://api.anthropic.com/v1/messages?beta=true", req.URL.String(),
		"OAuth 账号未配置 base_url 时应直连 api.anthropic.com")

	// 6b. 鉴权：使用账号的 OAuth access_token，而不是客户端的 sub2api API Key。
	require.Equal(t, "Bearer "+token, getHeaderRaw(req.Header, "authorization"),
		"上游鉴权头必须替换为 Claude Pro OAuth token")
	require.Empty(t, getHeaderRaw(req.Header, "x-api-key"),
		"OAuth 路径不应出现 x-api-key")

	// 6c. anthropic-beta：mimic 非 haiku → 完整 Claude Code 伪装 beta 集合。
	outBeta := getHeaderRaw(req.Header, "anthropic-beta")
	require.True(t, anthropicBetaTokensContains(outBeta, claude.BetaOAuth))
	require.True(t, anthropicBetaTokensContains(outBeta, claude.BetaClaudeCode),
		"mimic Claude Code 必须携带 claude-code beta，否则被上游判第三方")
	require.True(t, anthropicBetaTokensContains(outBeta, claude.BetaInterleavedThinking))

	// 6d. Claude Code 指纹头（伪装成官方 CLI）。
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(req.Header, "User-Agent"),
		"User-Agent 必须伪装成 claude-cli/<version>")
	require.Equal(t, "cli", getHeaderRaw(req.Header, "x-app"))
	require.Equal(t, "true", getHeaderRaw(req.Header, "anthropic-dangerous-direct-browser-access"))
	require.Equal(t, "application/json", getHeaderRaw(req.Header, "Accept"))
	require.NotEmpty(t, getHeaderRaw(req.Header, "x-client-request-id"),
		"每个请求都应生成新的 x-client-request-id")
	require.Equal(t, "stream", getHeaderRaw(req.Header, "x-stainless-helper-method"),
		"stream 请求应带 x-stainless-helper-method: stream")

	// 6e. 客户端 curl/8.4.0 的 UA 不应被透传到上游。
	require.NotContains(t, getHeaderRaw(req.Header, "User-Agent"), "curl",
		"mimic 路径不透传客户端 header，避免指纹冲突")

	// 6f. 请求体：system 仅剩 Claude Code 标识，用户问题被迁到 messages。
	require.Contains(t, string(outBody), "You are Claude Code",
		"system 字段应替换为 Claude Code 标识提示词")
	require.Contains(t, string(outBody), "用一句话介绍你自己",
		"用户的实际问题应保留在请求体中")

	// ---- 7. 确认日志已落盘 ----
	svc.debugGatewayBodyFile.Load().Sync()
	info, statErr := os.Stat(logPath)
	require.NoError(t, statErr, "日志文件应已生成")
	require.Greater(t, info.Size(), int64(0), "日志文件不应为空")

	t.Logf("上游转发请求快照已写入: %s (%d bytes)", logPath, info.Size())
}

// writeCaptureBanner 在日志开头写一段场景说明，方便脱离测试上下文阅读。
func writeCaptureBanner(t *testing.T, s *GatewayService, logPath string, account *Account, modelID string) {
	t.Helper()
	f := s.debugGatewayBodyFile.Load()
	require.NotNil(t, f, "调试日志文件应已初始化: %s", logPath)
	banner := strings.Join([]string{
		"################################################################",
		"# sub2api Claude Pro（OAuth）上游转发请求抓包",
		"# 生成时间: " + time.Now().Format("2006-01-02 15:04:05"),
		"# 账号: " + account.Name + " (OAuth / Claude Pro)",
		"# 模型: " + modelID,
		"#",
		"# 说明:",
		"#   CLIENT_ORIGINAL   = 用户用 curl 打到 sub2api 的原始请求",
		"#   UPSTREAM_FORWARD  = sub2api 转发到 api.anthropic.com 的真实请求",
		"#   authorization / x-api-key 出于安全在日志中脱敏为 [redacted]。",
		"#   生产环境下 User-Agent / x-stainless-* 等指纹头会按账号维度随机化，",
		"#   本抓包展示的是未接入 identityService 时的默认 Claude Code 指纹。",
		"################################################################",
		"",
	}, "\n")
	_, err := f.WriteString(banner)
	require.NoError(t, err)
}
