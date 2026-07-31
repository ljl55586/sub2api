package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/tidwall/gjson"
)

// fingerprintSalt 是计算 cc_version 后缀指纹的盐值。
//
// 来源：与 Parrot src/transform/cc_mimicry.py 的 FINGERPRINT_SALT 完全一致；
// 这是真实 Claude Code CLI 抓包推导出的常量，改动会导致 fp 与 CLI 不一致，
// 进一步触发 Anthropic 的第三方检测。
const fingerprintSalt = "59cf53e54c78"

// computeClaudeCodeFingerprint 复刻真实 Claude Code CLI 的 cc_version 指纹算法：
//
//  1. 取 messages 中第一条 role=user 的纯文本（首块 text）
//  2. 取该文本的第 4、7、20 字符（不足以 '0' 补齐）
//  3. SHA256(SALT + chars + cc_version) 取 hex 前 3 字符
//
// 算法来自 Parrot src/transform/cc_mimicry.py:compute_fingerprint，与官方 CLI 字节对齐。
// 任何偏差都会导致 cc_version=X.Y.Z.{fp} 在上游侧与真实 CLI 不一致。
func computeClaudeCodeFingerprint(body []byte, version string) string {
	firstText := extractFirstUserText(body)
	indices := []int{4, 7, 20}
	units := utf16.Encode([]rune(firstText))
	var chars strings.Builder
	for _, i := range indices {
		if i < len(units) {
			chars.WriteString(string(utf16.Decode([]uint16{units[i]})))
		} else {
			chars.WriteByte('0')
		}
	}
	sum := sha256.Sum256([]byte(fingerprintSalt + chars.String() + version))
	return hex.EncodeToString(sum[:])[:3]
}

// extractFirstUserText 提取 messages 中第一条 user 消息的首段 text 内容。
// 兼容 string 和 []block 两种 content 格式。
func extractFirstUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	first := ""
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			if !isClaudeCode208MetaText(content.String()) {
				first = content.String()
				return false
			}
			return true
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				text := block.Get("text")
				if block.Get("type").String() == "text" &&
					text.Type == gjson.String &&
					!isClaudeCode208MetaText(text.String()) {
					first = text.String()
					return false
				}
				return true
			})
			return first == ""
		}
		return true
	})
	return first
}

// extractLastUserText returns the newest human input in the request history.
// Title eligibility is evaluated against this value rather than the first user
// message so a short opening turn does not permanently suppress a later title.
func extractLastUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	last := ""
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		// Bind the candidate to this exact user message. A tool continuation can
		// have role=user but contain only tool_result blocks; in that case the
		// latest candidate must be empty instead of inheriting older human text.
		last = ""
		content := msg.Get("content")
		switch {
		case content.Type == gjson.String:
			last = content.String()
		case content.IsArray():
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					last = block.Get("text").String()
					return false
				}
				return true
			})
		}
		return true
	})
	return last
}

// buildBillingAttributionText 构造 system 数组的 billing attribution 文本。
//
// 形态对齐真实 Claude Code CLI：
//
//	x-anthropic-billing-header: cc_version=2.1.208.{fp}; cc_entrypoint=cli; cch=00000;
//
// CCH 此时仍是等长占位符。所有 body 改写完成后，
// finalizeClaudeCodeCCH 会对最终序列化字节计算 token 并原位覆盖这五个零。
//
// 此 block 不带 cache_control（与真实 CLI 一致；cache breakpoint 由后续的
// Claude Code prompt block 承担）。
func buildBillingAttributionText(body []byte, cliVersion string) (string, error) {
	if cliVersion == "" {
		return "", fmt.Errorf("cliVersion required")
	}
	fp := computeClaudeCodeFingerprint(body, cliVersion)
	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=00000;",
		cliVersion, fp,
	), nil
}
