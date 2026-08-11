package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type glmCodingUsageFetcherStub struct {
	response *GLMCodingQuotaResponse
	err      error
	calls    int
}

func (s *glmCodingUsageFetcherStub) FetchUsage(context.Context, *Account) (*GLMCodingQuotaResponse, error) {
	s.calls++
	return s.response, s.err
}

func TestAccountIsGLMCodingPlanUsageEnabled(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			glmCodingPlanUsageEnabledCredentialKey: true,
		},
	}
	require.True(t, account.IsGLMCodingPlanUsageEnabled())

	account.Platform = PlatformAnthropic
	require.True(t, account.IsGLMCodingPlanUsageEnabled())

	account.Type = AccountTypeOAuth
	require.False(t, account.IsGLMCodingPlanUsageEnabled())
	account.Type = AccountTypeAPIKey
	account.Credentials[glmCodingPlanUsageEnabledCredentialKey] = false
	require.False(t, account.IsGLMCodingPlanUsageEnabled())
}

func TestGLMCodingUsageHTTPFetcherFetchesTeamQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "2", r.URL.Query().Get("type"))
		require.Equal(t, "coding-key", r.Header.Get("Authorization"))
		require.Equal(t, "org-test", r.Header.Get("Bigmodel-Organization"))
		require.Equal(t, "proj-test", r.Header.Get("Bigmodel-Project"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"code":200,
			"msg":"ok",
			"data":{"limits":[
				{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":15000,"currentValue":470,"remaining":14530,"percentage":3,"nextResetTime":1786444965701},
				{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":66000,"currentValue":470,"remaining":65530,"percentage":1,"nextResetTime":1786935098999}
			],"level":"pro"},
			"success":true
		}`))
	}))
	defer server.Close()

	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                              "coding-key",
			bigModelOrganizationCredentialKey:      "org-test",
			bigModelProjectCredentialKey:           "proj-test",
			glmCodingPlanUsageEnabledCredentialKey: true,
		},
	}
	fetcher := &GLMCodingUsageHTTPFetcher{Endpoint: server.URL + "?type=2"}
	response, err := fetcher.FetchUsage(context.Background(), account)
	require.NoError(t, err)

	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	usage := buildGLMCodingUsageInfo(response, now)
	require.NotNil(t, usage.FiveHour)
	require.Equal(t, 3.0, usage.FiveHour.Utilization)
	require.Equal(t, int64(470), usage.FiveHour.UsedRequests)
	require.Equal(t, int64(15000), usage.FiveHour.LimitRequests)
	require.Equal(t, time.UnixMilli(1786444965701), *usage.FiveHour.ResetsAt)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 1.0, usage.SevenDay.Utilization)
	require.Equal(t, int64(66000), usage.SevenDay.LimitRequests)
}

func TestGLMCodingUsageHTTPFetcherRejectsEmptyLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"msg":"ok","data":{},"success":true}`))
	}))
	defer server.Close()

	account := &Account{Credentials: map[string]any{
		"api_key":                         "coding-key",
		bigModelOrganizationCredentialKey: "org-test",
		bigModelProjectCredentialKey:      "proj-test",
	}}
	_, err := (&GLMCodingUsageHTTPFetcher{Endpoint: server.URL}).FetchUsage(context.Background(), account)
	require.ErrorContains(t, err, "no usage windows")
}

func TestBuildGLMCodingUsageProgressCalculatesMissingPercentage(t *testing.T) {
	now := time.Now()
	progress := buildGLMCodingUsageProgress(GLMCodingQuotaLimit{
		Usage:        200,
		CurrentValue: 50,
	}, now)
	require.Equal(t, 25.0, progress.Utilization)
	require.Nil(t, progress.ResetsAt)
}

func TestAccountUsageServiceRoutesGLMAPIKeyAccountsAndCachesResult(t *testing.T) {
	percentage := 3.0
	response := &GLMCodingQuotaResponse{Code: http.StatusOK, Success: true}
	response.Data.Limits = []GLMCodingQuotaLimit{{
		Type:          "CREDIT_LIMIT",
		Unit:          3,
		Number:        5,
		Usage:         15000,
		CurrentValue:  470,
		Percentage:    &percentage,
		NextResetTime: time.Now().Add(4 * time.Hour).UnixMilli(),
	}}
	fetcher := &glmCodingUsageFetcherStub{response: response}
	repo := &snapshotUpdateAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID:       73,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				glmCodingPlanUsageEnabledCredentialKey: true,
				"api_key":                              "coding-key",
				bigModelOrganizationCredentialKey:      "org-test",
				bigModelProjectCredentialKey:           "proj-test",
			},
		}}},
		updateExtraCalls: make(chan map[string]any, 2),
	}
	service := &AccountUsageService{
		accountRepo:           repo,
		cache:                 NewUsageCache(),
		glmCodingUsageFetcher: fetcher,
	}

	first, err := service.GetUsage(context.Background(), 73)
	require.NoError(t, err)
	require.NotNil(t, first.FiveHour)
	require.Equal(t, 3.0, first.FiveHour.Utilization)
	require.Equal(t, 1, fetcher.calls)
	updates := <-repo.updateExtraCalls
	require.Contains(t, updates, glmCodingUsageSnapshotExtraKey)

	second, err := service.GetUsage(context.Background(), 73)
	require.NoError(t, err)
	require.Equal(t, 3.0, second.FiveHour.Utilization)
	require.Equal(t, 1, fetcher.calls, "second query should use the five-minute cache")

	_, err = service.GetUsage(context.Background(), 73, true)
	require.NoError(t, err)
	require.Equal(t, 2, fetcher.calls, "forced query should bypass the cache")
}

func TestGLMCodingUsageFailureKeepsLastSnapshotAndMarksAuthenticationError(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	account := &Account{Extra: map[string]any{
		glmCodingUsageSnapshotExtraKey: map[string]any{
			"source":     "active",
			"updated_at": time.Now().Add(-time.Minute).UTC(),
			"five_hour": map[string]any{
				"utilization":       27.0,
				"resets_at":         resetAt,
				"remaining_seconds": 7200,
			},
		},
	}}

	usage := glmCodingUsageFailure(account, time.Now(), &GLMCodingUsageFetchError{
		Code:    http.StatusUnauthorized,
		Message: "unauthorized",
	})
	require.Equal(t, "passive", usage.Source)
	require.NotNil(t, usage.FiveHour)
	require.Equal(t, 27.0, usage.FiveHour.Utilization)
	require.True(t, usage.NeedsReauth)
	require.Equal(t, "unauthenticated", usage.ErrorCode)
}
