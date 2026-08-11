package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

const (
	defaultGLMCodingUsageEndpoint  = "https://bigmodel.cn/api/monitor/usage/quota/limit?type=2"
	glmCodingUsageSnapshotExtraKey = "glm_coding_usage_snapshot"
	glmCodingUsageSuccessCacheTTL  = 5 * time.Minute
	glmCodingUsageErrorCacheTTL    = time.Minute
	glmCodingUsageTimeout          = 15 * time.Second
	glmCodingUsageMaxResponseBytes = 1 << 20
)

type GLMCodingQuotaLimit struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        int      `json:"number"`
	Usage         int64    `json:"usage"`
	CurrentValue  int64    `json:"currentValue"`
	Remaining     int64    `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime int64    `json:"nextResetTime"`
}

type GLMCodingQuotaResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Limits []GLMCodingQuotaLimit `json:"limits"`
		Level  string                `json:"level"`
	} `json:"data"`
	Success bool `json:"success"`
}

type GLMCodingUsageFetchError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *GLMCodingUsageFetchError) Error() string {
	if e == nil {
		return "GLM Coding Plan usage query failed"
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "GLM Coding Plan usage query failed"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s (HTTP %d)", message, e.StatusCode)
	}
	if e.Code != 0 {
		return fmt.Sprintf("%s (code %d)", message, e.Code)
	}
	return message
}

type GLMCodingUsageFetcher interface {
	FetchUsage(ctx context.Context, account *Account) (*GLMCodingQuotaResponse, error)
}

type GLMCodingUsageHTTPFetcher struct {
	Endpoint string
}

func NewGLMCodingUsageHTTPFetcher() *GLMCodingUsageHTTPFetcher {
	return &GLMCodingUsageHTTPFetcher{Endpoint: defaultGLMCodingUsageEndpoint}
}

func (f *GLMCodingUsageHTTPFetcher) FetchUsage(ctx context.Context, account *Account) (*GLMCodingQuotaResponse, error) {
	if account == nil {
		return nil, errors.New("GLM Coding Plan account is required")
	}
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	organization := strings.TrimSpace(account.GetCredential(bigModelOrganizationCredentialKey))
	project := strings.TrimSpace(account.GetCredential(bigModelProjectCredentialKey))
	if apiKey == "" {
		return nil, errors.New("GLM Coding Plan API key is missing")
	}
	if organization == "" || project == "" {
		return nil, errors.New("GLM Coding Plan organization or project is missing")
	}

	endpoint := strings.TrimSpace(f.Endpoint)
	if endpoint == "" {
		endpoint = defaultGLMCodingUsageEndpoint
	}
	requestCtx, cancel := context.WithTimeout(ctx, glmCodingUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build GLM Coding Plan usage request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Bigmodel-Organization", organization)
	req.Header.Set("Bigmodel-Project", project)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               glmCodingUsageTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("build GLM Coding Plan usage client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request GLM Coding Plan usage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, glmCodingUsageMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GLM Coding Plan usage response: %w", err)
	}
	if len(body) > glmCodingUsageMaxResponseBytes {
		return nil, errors.New("GLM Coding Plan usage response is too large")
	}

	var result GLMCodingQuotaResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, &GLMCodingUsageFetchError{StatusCode: resp.StatusCode, Message: "invalid GLM Coding Plan usage response"}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &GLMCodingUsageFetchError{StatusCode: resp.StatusCode, Code: result.Code, Message: result.Msg}
	}
	if !result.Success || result.Code != http.StatusOK {
		return nil, &GLMCodingUsageFetchError{Code: result.Code, Message: result.Msg}
	}
	if len(result.Data.Limits) == 0 {
		return nil, errors.New("GLM Coding Plan returned no usage windows")
	}
	return &result, nil
}

func buildGLMCodingUsageInfo(response *GLMCodingQuotaResponse, now time.Time) *UsageInfo {
	usage := &UsageInfo{Source: "active", UpdatedAt: &now}
	if response == nil {
		return usage
	}
	for i := range response.Data.Limits {
		limit := response.Data.Limits[i]
		if limit.Type != "" && !strings.EqualFold(limit.Type, "CREDIT_LIMIT") {
			continue
		}
		progress := buildGLMCodingUsageProgress(limit, now)
		switch {
		case limit.Unit == 3 && limit.Number == 5:
			usage.FiveHour = progress
		case limit.Unit == 6 && limit.Number == 1:
			usage.SevenDay = progress
		}
	}
	return usage
}

func buildGLMCodingUsageProgress(limit GLMCodingQuotaLimit, now time.Time) *UsageProgress {
	utilization := float64(0)
	if limit.Percentage != nil {
		utilization = *limit.Percentage
	} else if limit.Usage > 0 {
		utilization = float64(limit.CurrentValue) / float64(limit.Usage) * 100
	}
	if utilization < 0 {
		utilization = 0
	}

	var resetsAt *time.Time
	remainingSeconds := 0
	if limit.NextResetTime > 0 {
		reset := time.UnixMilli(limit.NextResetTime)
		resetsAt = &reset
		remainingSeconds = int(reset.Sub(now).Seconds())
		if remainingSeconds < 0 {
			remainingSeconds = 0
		}
	}
	return &UsageProgress{
		Utilization:      utilization,
		ResetsAt:         resetsAt,
		RemainingSeconds: remainingSeconds,
		UsedRequests:     limit.CurrentValue,
		LimitRequests:    limit.Usage,
	}
}

func cloneGLMCodingUsageInfo(source *UsageInfo, now time.Time) *UsageInfo {
	if source == nil {
		return nil
	}
	clone := *source
	clone.FiveHour = cloneUsageProgress(source.FiveHour, now)
	clone.SevenDay = cloneUsageProgress(source.SevenDay, now)
	return &clone
}

func cloneUsageProgress(source *UsageProgress, now time.Time) *UsageProgress {
	if source == nil {
		return nil
	}
	clone := *source
	if source.ResetsAt != nil {
		reset := *source.ResetsAt
		clone.ResetsAt = &reset
		clone.RemainingSeconds = int(reset.Sub(now).Seconds())
		if clone.RemainingSeconds < 0 {
			clone.RemainingSeconds = 0
		}
	}
	return &clone
}

func persistedGLMCodingUsage(account *Account, now time.Time) *UsageInfo {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[glmCodingUsageSnapshotExtraKey]
	if !ok || raw == nil {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var usage UsageInfo
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil
	}
	if usage.FiveHour == nil && usage.SevenDay == nil {
		return nil
	}
	usage.Source = "passive"
	return cloneGLMCodingUsageInfo(&usage, now)
}

func (s *AccountUsageService) getGLMCodingPlanUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	now := time.Now()
	if account == nil {
		return glmCodingUsageFailure(nil, now, errors.New("GLM Coding Plan account is required")), nil
	}

	if !force && s.cache != nil {
		if cached, ok := s.cache.glmCodingCache.Load(account.ID); ok {
			if entry, ok := cached.(*glmCodingUsageCache); ok {
				age := now.Sub(entry.timestamp)
				if entry.err != nil && age < glmCodingUsageErrorCacheTTL {
					return glmCodingUsageFailure(account, now, entry.err), nil
				}
				if entry.usage != nil && age < glmCodingUsageSuccessCacheTTL {
					return cloneGLMCodingUsageInfo(entry.usage, now), nil
				}
			}
		}
	}

	fetch := func() (*UsageInfo, error) {
		fetcher := s.glmCodingUsageFetcher
		if fetcher == nil {
			fetcher = NewGLMCodingUsageHTTPFetcher()
		}
		response, err := fetcher.FetchUsage(ctx, account)
		if err != nil {
			return nil, err
		}
		usage := buildGLMCodingUsageInfo(response, time.Now())
		if usage.FiveHour == nil && usage.SevenDay == nil {
			return nil, errors.New("GLM Coding Plan response did not contain supported 5h or 7d windows")
		}
		return usage, nil
	}

	var usage *UsageInfo
	var err error
	if s.cache != nil {
		result, flightErr, _ := s.cache.glmCodingFlight.Do(fmt.Sprintf("glm-coding:%d", account.ID), func() (any, error) {
			return fetch()
		})
		err = flightErr
		if result != nil {
			usage, _ = result.(*UsageInfo)
		}
	} else {
		usage, err = fetch()
	}

	if err != nil {
		if s.cache != nil {
			s.cache.glmCodingCache.Store(account.ID, &glmCodingUsageCache{err: err, timestamp: time.Now()})
		}
		return glmCodingUsageFailure(account, now, err), nil
	}
	if s.cache != nil {
		s.cache.glmCodingCache.Store(account.ID, &glmCodingUsageCache{usage: usage, timestamp: time.Now()})
	}
	if s.accountRepo != nil {
		if persistErr := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{glmCodingUsageSnapshotExtraKey: usage}); persistErr != nil {
			slog.Warn("persist_glm_coding_usage_failed", "account_id", account.ID, "error", persistErr)
		}
	}
	return cloneGLMCodingUsageInfo(usage, time.Now()), nil
}

func glmCodingUsageFailure(account *Account, now time.Time, err error) *UsageInfo {
	usage := persistedGLMCodingUsage(account, now)
	if usage == nil {
		usage = &UsageInfo{UpdatedAt: &now}
	}
	usage.Error = err.Error()
	var fetchErr *GLMCodingUsageFetchError
	if errors.As(err, &fetchErr) &&
		(fetchErr.StatusCode == http.StatusUnauthorized ||
			fetchErr.StatusCode == http.StatusForbidden ||
			fetchErr.Code == http.StatusUnauthorized ||
			fetchErr.Code == http.StatusForbidden) {
		usage.ErrorCode = "unauthenticated"
		usage.NeedsReauth = true
	} else {
		usage.ErrorCode = "network_error"
	}
	return usage
}
