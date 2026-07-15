package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildOAuthMetadataUserID_RejectsMissingAccountUUID(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
	}

	account := &Account{
		ID:    123,
		Type:  AccountTypeOAuth,
		Extra: map[string]any{}, // intentionally missing account_uuid / claude_user_id
	}

	identity := AccountIdentity{DeviceID: "deadbeef"}

	_, err := svc.buildOAuthMetadataUserID(parsed, account, identity, "")
	require.ErrorIs(t, err, ErrIncompleteAccountIdentity)
}

func TestBuildOAuthMetadataUserID_UsesAccountUUIDWhenPresent(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
	}

	account := &Account{
		ID:   123,
		Type: AccountTypeOAuth,
		Extra: map[string]any{
			"account_uuid":      "acc-uuid",
			"claude_user_id":    "clientid123",
			"anthropic_user_id": "",
		},
	}

	got, err := svc.buildOAuthMetadataUserID(parsed, account, AccountIdentity{
		DeviceID:    "clientid123",
		AccountUUID: "acc-uuid",
	}, "")
	require.NoError(t, err)
	require.NotEmpty(t, got)

	// New format: user_{client}_account_{account_uuid}_session_{uuid}
	re := regexp.MustCompile(`^user_clientid123_account_acc-uuid_session_[a-f0-9-]{36}$`)
	require.True(t, re.MatchString(got), "unexpected user_id format: %s", got)
}

// TestBuildOAuthMetadataUserID_SessionIDStableAcrossTurns 验证伪装路径合成的
// metadata.user_id 在同一会话多轮请求间保持不变（session_id 稳定），贴近真实 Claude Code
// 进程级稳定的 session。账号 / 指纹 / UA 版本均相同，唯一可能变化的就是 session_id，
// 因此直接比较完整 user_id 字符串即可判定 session_id 是否稳定。
func TestBuildOAuthMetadataUserID_SessionIDStableAcrossTurns(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{ID: 777, Type: AccountTypeOAuth, Extra: map[string]any{"account_uuid": "acc-uuid"}}
	fp := &Fingerprint{ClientID: "clientid777", UserAgent: "claude-cli/2.1.161 (external, cli)"}

	mustParse := func(body string) *ParsedRequest {
		parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
		require.NoError(t, err)
		return parsed
	}

	round1 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"}]}`)
	round2 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer 1"},` +
		`{"role":"user","content":"second question"}]}`)
	round3 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer 1"},` +
		`{"role":"user","content":"second question"},` +
		`{"role":"assistant","content":"answer 2"},` +
		`{"role":"user","content":"third question"}]}`)

	identity := AccountIdentity{DeviceID: fp.ClientID, AccountUUID: "acc-uuid"}
	uaVersion := ExtractCLIVersion(fp.UserAgent)
	id1, err := svc.buildOAuthMetadataUserID(round1, account, identity, uaVersion)
	require.NoError(t, err)
	id2, err := svc.buildOAuthMetadataUserID(round2, account, identity, uaVersion)
	require.NoError(t, err)
	id3, err := svc.buildOAuthMetadataUserID(round3, account, identity, uaVersion)
	require.NoError(t, err)

	require.NotEmpty(t, id1)
	require.Equal(t, id1, id2, "session_id 应随对话增长保持不变")
	require.Equal(t, id2, id3, "session_id 应跨所有轮次保持不变")

	// 不同的首条 user 消息应派生出不同的 session_id（不同会话）。
	other := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"a completely different opener"}]}`)
	idOther, err := svc.buildOAuthMetadataUserID(other, account, identity, uaVersion)
	require.NoError(t, err)
	require.NotEqual(t, id1, idOther, "不同首条消息应派生不同 session_id")
}

func TestBuildOAuthMetadataUserID_ReplacesInvalidExistingMetadata(t *testing.T) {
	svc := &GatewayService{}
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"invalid"},"messages":[]}`)), PlatformAnthropic)
	require.NoError(t, err)

	got, err := svc.buildOAuthMetadataUserID(parsed, &Account{ID: 123}, AccountIdentity{
		DeviceID:    "device-123",
		AccountUUID: "account-123",
	}, "2.1.161")
	require.NoError(t, err)
	require.NotEmpty(t, got)
	parsedUserID := ParseMetadataUserID(got)
	require.NotNil(t, parsedUserID)
	require.Equal(t, "device-123", parsedUserID.DeviceID)
	require.Equal(t, "account-123", parsedUserID.AccountUUID)
}

func TestBuildOAuthMetadataUserID_ReplacesForeignValidMetadataOnMimicPath(t *testing.T) {
	svc := &GatewayService{}
	foreign := FormatMetadataUserID(
		"foreign-device",
		"foreign-account",
		"00000000-0000-4000-8000-000000000000",
		"2.1.161",
	)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(
		`{"model":"claude-opus-4-8","metadata":{"user_id":`+strconvQuote(foreign)+`},"messages":[{"role":"user","content":"hi"}]}`,
	)), PlatformAnthropic)
	require.NoError(t, err)

	got, err := svc.buildOAuthMetadataUserID(parsed, &Account{ID: 123}, AccountIdentity{
		DeviceID:    "device-123",
		AccountUUID: "account-123",
	}, "2.1.161")
	require.NoError(t, err)

	parsedUserID := ParseMetadataUserID(got)
	require.NotNil(t, parsedUserID)
	require.Equal(t, "device-123", parsedUserID.DeviceID)
	require.Equal(t, "account-123", parsedUserID.AccountUUID)
	require.NotEqual(t, "00000000-0000-4000-8000-000000000000", parsedUserID.SessionID)
}

func TestEnsureClaudeOAuthMetadataUserID_ReplacesInvalidExistingValue(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"invalid"},"messages":[]}`)
	valid := FormatMetadataUserID("device-123", "account-123", "session-123", "2.1.161")

	result, changed := ensureClaudeOAuthMetadataUserID(body, valid)

	require.True(t, changed)
	require.Equal(t, valid, gjson.GetBytes(result, "metadata.user_id").String())
}

func TestEnsureClaudeOAuthMetadataUserID_ReplacesForeignValidValueOnMimicPath(t *testing.T) {
	foreign := FormatMetadataUserID(
		"foreign-device",
		"foreign-account",
		"00000000-0000-4000-8000-000000000000",
		"2.1.161",
	)
	canonical := FormatMetadataUserID(
		"device-123",
		"account-123",
		"11111111-2222-4333-8444-555555555555",
		"2.1.161",
	)
	body := []byte(`{"metadata":{"user_id":` + strconvQuote(foreign) + `},"messages":[]}`)

	result, changed := ensureClaudeOAuthMetadataUserID(body, canonical)

	require.True(t, changed)
	require.Equal(t, canonical, gjson.GetBytes(result, "metadata.user_id").String())
}

func TestBuildUpstreamRequest_RealClaudeCodePreservesMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.161 (external, cli)")
	c.Request.Header.Set("X-Stainless-OS", "MacOS")
	c.Request.Header.Set("X-Claude-Code-Session-Id", "session-cli")

	cache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID:    "proxy-device",
		UserAgent:   "curl/8.4.0",
		StainlessOS: "Linux",
		UpdatedAt:   time.Now().Unix(),
	}}
	svc := &GatewayService{identityService: NewIdentityService(cache)}
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"account_uuid": "account-cli"},
	}
	original := FormatMetadataUserID("device-cli", "account-cli", "session-cli", "2.1.161")
	body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":` + strconvQuote(original) + `},"messages":[]}`)

	req, _, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body,
		"oauth-token", "oauth", "claude-sonnet-4-5", false, false,
	)

	require.NoError(t, err)
	outputBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, original, gjson.GetBytes(outputBody, "metadata.user_id").String())
	require.Equal(t, "session-cli", req.Header.Get("X-Claude-Code-Session-Id"))
	require.Equal(t, "claude-cli/2.1.161 (external, cli)", getHeaderRaw(req.Header, "User-Agent"))
	require.Equal(t, "MacOS", getHeaderRaw(req.Header, "X-Stainless-OS"))
}

func TestBuildOAuthMetadataUserIDFromBody_RejectsIncompleteAccountIdentity(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{ID: 123, Type: AccountTypeOAuth}
	fp := &Fingerprint{ClientID: "device-123", UserAgent: "claude-cli/2.1.161 (external, cli)"}

	got := svc.buildOAuthMetadataUserIDFromBody(context.Background(), account, fp, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))

	require.Empty(t, got)
}
