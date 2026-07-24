package service

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

const claudeCodeCCHPlaceholder = "cch=00000"

var (
	claudeCodeBillingCCHFieldRe   = regexp.MustCompile(`\bcch=[^;]*;`)
	claudeCodeBillingEntrypointRe = regexp.MustCompile(`\bcc_entrypoint=[^;]*;`)
)

// finalizeClaudeCodeCCH applies Claude Code's native client-attestation step to
// exact finalized JSON bytes. It first normalizes the billing token to the
// five-byte placeholder, hashes the entire body with the profile seed, then
// replaces only those five bytes in place. The replacement cannot change
// Content-Length.
func finalizeClaudeCodeCCH(body []byte, profile claude.ClaudeCodeProfile) ([]byte, bool, error) {
	if !profile.CCH.Enabled || len(body) == 0 {
		return body, false, nil
	}

	billingIndex, billingText, ok := findClaudeCodeBillingSystemBlock(body)
	if !ok {
		// Quota requests intentionally have no system/billing block.
		return body, false, nil
	}

	placeholderText := billingText
	switch {
	case claudeCodeBillingCCHFieldRe.MatchString(placeholderText):
		placeholderText = claudeCodeBillingCCHFieldRe.ReplaceAllString(placeholderText, claudeCodeCCHPlaceholder+";")
	case claudeCodeBillingEntrypointRe.MatchString(placeholderText):
		placeholderText = claudeCodeBillingEntrypointRe.ReplaceAllStringFunc(placeholderText, func(entrypoint string) string {
			return entrypoint + " " + claudeCodeCCHPlaceholder + ";"
		})
	default:
		return nil, false, fmt.Errorf("Claude Code billing block is missing cc_entrypoint")
	}

	if placeholderText != billingText {
		next, ok := setJSONValueBytes(body, fmt.Sprintf("system.%d.text", billingIndex), placeholderText)
		if !ok {
			return nil, false, fmt.Errorf("set Claude Code CCH placeholder")
		}
		body = next
	}

	textResult := gjson.GetBytes(body, fmt.Sprintf("system.%d.text", billingIndex))
	if !textResult.Exists() || textResult.Type != gjson.String || textResult.Index < 0 {
		return nil, false, fmt.Errorf("locate Claude Code billing block")
	}
	rawStart := textResult.Index
	rawEnd := rawStart + len(textResult.Raw)
	if rawStart < 0 || rawEnd > len(body) {
		return nil, false, fmt.Errorf("locate Claude Code billing bytes")
	}
	relative := bytes.Index(body[rawStart:rawEnd], []byte(claudeCodeCCHPlaceholder))
	if relative < 0 {
		return nil, false, fmt.Errorf("locate Claude Code CCH placeholder")
	}
	valueStart := rawStart + relative + len("cch=")

	digest := xxhash.NewWithSeed(profile.CCH.Seed)
	_, _ = digest.Write(body)
	cch := fmt.Sprintf("%05x", digest.Sum64()&0xFFFFF)

	out := append([]byte(nil), body...)
	copy(out[valueStart:valueStart+5], cch)
	return out, true, nil
}

func findClaudeCodeBillingSystemBlock(body []byte) (int, string, bool) {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		return 0, "", false
	}

	index := 0
	foundIndex := 0
	foundText := ""
	found := false
	system.ForEach(func(_, block gjson.Result) bool {
		text := block.Get("text")
		if block.Get("type").String() == "text" &&
			text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header:") {
			foundIndex = index
			foundText = text.String()
			found = true
			return false
		}
		index++
		return true
	})
	return foundIndex, foundText, found
}
