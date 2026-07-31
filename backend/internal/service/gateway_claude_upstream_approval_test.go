package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClaudeUpstreamApprovalStage_BundlesExactRequestsAndBlocksUntilApproved(t *testing.T) {
	dir := t.TempDir()
	session := &claudeUpstreamApprovalSession{
		gate: &claudeUpstreamApprovalGate{
			dir:          dir,
			token:        "approval-secret",
			pollInterval: 5 * time.Millisecond,
		},
		ctx:        context.Background(),
		approvalID: "request-1",
	}
	stage := session.NewStage()
	require.NoError(t, stage.SetExpected(3))

	type requestFixture struct {
		kind string
		body string
	}
	fixtures := []requestFixture{
		{kind: "main", body: `{"kind":"main","system":[{"text":"cch=abc12;"}]}`},
		{kind: "quota", body: `{"kind":"quota"}`},
		{kind: "title", body: `{"kind":"title"}`},
	}
	results := make(chan error, len(fixtures))
	var started sync.WaitGroup
	started.Add(len(fixtures))
	for _, fixture := range fixtures {
		fixture := fixture
		go func() {
			req, err := http.NewRequest(
				http.MethodPost,
				"https://api.anthropic.com/v1/messages",
				bytes.NewBufferString(fixture.body),
			)
			if err != nil {
				results <- err
				return
			}
			req.Header.Set("Authorization", "Bearer upstream-secret")
			req.Header.Set("x-api-key", "api-key-secret")
			req.Header.Set("anthropic-version", "2023-06-01")
			started.Done()
			results <- stage.Await(fixture.kind, req)
		}()
	}
	started.Wait()

	previewPath := filepath.Join(dir, "request-1-s01.preview.json")
	preview := waitForClaudeUpstreamApprovalPreview(t, previewPath)
	require.False(t, preview.NetworkSent)
	require.Equal(t, "request-1", preview.ApprovalID)
	require.Equal(t, "request-1-s01", preview.StageID)
	require.Equal(t, []string{"quota", "title", "main"}, []string{
		preview.Requests[0].Kind,
		preview.Requests[1].Kind,
		preview.Requests[2].Kind,
	})
	require.JSONEq(t, fixtures[0].body, string(preview.Requests[2].Body))
	require.NotEmpty(t, preview.Requests[2].BodySHA256)
	require.Equal(t, int64(len(fixtures[0].body)), preview.Requests[2].ContentLength)
	for _, request := range preview.Requests {
		require.Equal(t, "Bearer [redacted]", claudeUpstreamApprovalHeaderValue(request, "Authorization"))
		require.Equal(t, "[redacted]", claudeUpstreamApprovalHeaderValue(request, "x-api-key"))
	}

	select {
	case err := <-results:
		t.Fatalf("request passed the approval gate before approval: %v", err)
	default:
	}

	writeClaudeUpstreamApprovalMarker(t, filepath.Join(dir, "request-1-s01.approve"))
	for range fixtures {
		require.NoError(t, <-results)
	}

	var result claudeUpstreamApprovalResult
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "request-1-s01.result.json"))
		return err == nil && json.Unmarshal(data, &result) == nil
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, "approved", result.Decision)
}

func TestClaudeUpstreamApprovalStage_RejectsWithoutReleasingRequest(t *testing.T) {
	dir := t.TempDir()
	session := &claudeUpstreamApprovalSession{
		gate: &claudeUpstreamApprovalGate{
			dir:          dir,
			token:        "approval-secret",
			pollInterval: 5 * time.Millisecond,
		},
		ctx:        context.Background(),
		approvalID: "request-rejected",
	}
	stage := session.NewStage()
	require.NoError(t, stage.SetExpected(1))
	req, err := http.NewRequest(
		http.MethodPost,
		"https://api.anthropic.com/v1/messages",
		bytes.NewBufferString(`{"kind":"main"}`),
	)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- stage.Await("main", req)
	}()
	waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(dir, "request-rejected-s01.preview.json"),
	)
	writeClaudeUpstreamApprovalMarker(
		t,
		filepath.Join(dir, "request-rejected-s01.reject"),
	)
	require.ErrorIs(t, <-done, errClaudeUpstreamApprovalRejected)

	var result claudeUpstreamApprovalResult
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(filepath.Join(dir, "request-rejected-s01.result.json"))
		return readErr == nil && json.Unmarshal(data, &result) == nil
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, "rejected", result.Decision)
}

func TestGatewayServiceForward_UpstreamApprovalFirstBundleThenMainOnly(t *testing.T) {
	svc, c, account, parsed, upstream := newClaudeOAuthCompanionForwardHarness(t)
	approvalDir := t.TempDir()
	svc.claudeUpstreamApproval = &claudeUpstreamApprovalGate{
		dir:          approvalDir,
		token:        "approval-secret",
		pollInterval: 5 * time.Millisecond,
	}
	c.Request.Header.Set("conversation_id", "approval-test-conversation")
	c.Request.Header.Set(claudeUpstreamApprovalTokenHeader, "approval-secret")
	c.Request.Header.Set(claudeUpstreamApprovalIDHeader, "turn-1")

	type forwardOutcome struct {
		result *ForwardResult
		err    error
	}
	firstDone := make(chan forwardOutcome, 1)
	go func() {
		result, err := svc.Forward(context.Background(), c, account, parsed)
		firstDone <- forwardOutcome{result: result, err: err}
	}()

	firstPreview := waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(approvalDir, "turn-1-s01.preview.json"),
	)
	require.Equal(t, 0, upstream.Count(), "no request may reach the upstream before approval")
	require.Equal(t, []string{"quota", "title", "main"}, []string{
		firstPreview.Requests[0].Kind,
		firstPreview.Requests[1].Kind,
		firstPreview.Requests[2].Kind,
	})
	require.Contains(t, string(firstPreview.Requests[2].Body), "cch=")
	require.NotContains(t, string(firstPreview.Requests[2].Body), "cch=00000")
	require.Equal(
		t,
		"Bearer [redacted]",
		claudeUpstreamApprovalHeaderValue(firstPreview.Requests[2], "Authorization"),
	)
	require.Empty(
		t,
		claudeUpstreamApprovalHeaderValue(
			firstPreview.Requests[2],
			claudeUpstreamApprovalTokenHeader,
		),
		"the downstream approval token must never reach the upstream request",
	)

	writeClaudeUpstreamApprovalMarker(t, filepath.Join(approvalDir, "turn-1-s01.approve"))
	firstOutcome := <-firstDone
	require.NoError(t, firstOutcome.err)
	require.NotNil(t, firstOutcome.result)
	require.Eventually(t, func() bool { return upstream.Count() == 3 }, time.Second, 5*time.Millisecond)
	require.Equal(t, []string{"quota", "title", "main"}, []string{
		upstream.Snapshot()[0].kind,
		upstream.Snapshot()[1].kind,
		upstream.Snapshot()[2].kind,
	})
	require.Eventually(t, func() bool {
		store := svc.cache.(*companionClaimStoreForTest)
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, runtime := range store.runtimes {
			if runtime.Quota.State == claudeOAuthRuntimeActionCompleted &&
				runtime.Title.State == claudeOAuthRuntimeActionGenerated {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)

	c.Request.Header.Set(claudeUpstreamApprovalIDHeader, "turn-2")
	secondParsed := newClaudeOAuthCompanionParsedRequest(
		t,
		"claude-opus-4-8",
		true,
		"Continue from the saved answer",
	)
	secondDone := make(chan forwardOutcome, 1)
	go func() {
		result, err := svc.Forward(context.Background(), c, account, secondParsed)
		secondDone <- forwardOutcome{result: result, err: err}
	}()

	secondPreview := waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(approvalDir, "turn-2-s01.preview.json"),
	)
	require.Equal(t, 3, upstream.Count(), "the second main must also wait for approval")
	require.Len(t, secondPreview.Requests, 1)
	require.Equal(t, "main", secondPreview.Requests[0].Kind)
	require.Contains(t, string(secondPreview.Requests[0].Body), "cch=")

	writeClaudeUpstreamApprovalMarker(t, filepath.Join(approvalDir, "turn-2-s01.approve"))
	secondOutcome := <-secondDone
	require.NoError(t, secondOutcome.err)
	require.NotNil(t, secondOutcome.result)
	require.Eventually(t, func() bool { return upstream.Count() == 4 }, time.Second, 5*time.Millisecond)
}

func TestGatewayServiceForward_UpstreamApprovalStrictlyOrdersFirstBundle(t *testing.T) {
	svc, c, account, parsed, upstream := newClaudeOAuthCompanionForwardHarness(t)
	approvalDir := t.TempDir()
	svc.claudeUpstreamApproval = &claudeUpstreamApprovalGate{
		dir:          approvalDir,
		token:        "approval-secret",
		pollInterval: 5 * time.Millisecond,
	}
	c.Request.Header.Set("conversation_id", "strict-order-conversation")
	c.Request.Header.Set(claudeUpstreamApprovalTokenHeader, "approval-secret")
	c.Request.Header.Set(claudeUpstreamApprovalIDHeader, "strict-order-turn")
	c.Request.Header.Set(claudeOAuthCompanionDelayHeader, "0")

	quotaRelease := make(chan struct{})
	titleRelease := make(chan struct{})
	upstream.quotaStarted = make(chan struct{})
	upstream.quotaRelease = quotaRelease
	upstream.titleRelease = titleRelease

	var releaseQuotaOnce sync.Once
	var releaseTitleOnce sync.Once
	releaseQuota := func() {
		releaseQuotaOnce.Do(func() { close(quotaRelease) })
	}
	releaseTitle := func() {
		releaseTitleOnce.Do(func() { close(titleRelease) })
	}
	t.Cleanup(func() {
		releaseQuota()
		releaseTitle()
	})

	type forwardOutcome struct {
		result *ForwardResult
		err    error
	}
	forwardDone := make(chan forwardOutcome, 1)
	go func() {
		result, err := svc.Forward(context.Background(), c, account, parsed)
		forwardDone <- forwardOutcome{result: result, err: err}
	}()

	waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(approvalDir, "strict-order-turn-s01.preview.json"),
	)
	require.Equal(t, 0, upstream.Count(), "approval must precede every network attempt")
	writeClaudeUpstreamApprovalMarker(
		t,
		filepath.Join(approvalDir, "strict-order-turn-s01.approve"),
	)

	select {
	case <-upstream.quotaStarted:
	case <-time.After(time.Second):
		t.Fatal("quota did not start after approval")
	}
	require.Equal(t, []string{"quota"}, []string{upstream.Snapshot()[0].kind})
	select {
	case <-upstream.titleStarted:
		t.Fatal("title started before quota reached its response/error boundary")
	default:
	}
	select {
	case outcome := <-forwardDone:
		t.Fatalf("main completed while quota was blocked: result=%v err=%v", outcome.result, outcome.err)
	default:
	}

	releaseQuota()
	select {
	case <-upstream.titleStarted:
	case <-time.After(time.Second):
		t.Fatal("title did not start after quota completed")
	}
	require.Equal(t, []string{"quota", "title"}, []string{
		upstream.Snapshot()[0].kind,
		upstream.Snapshot()[1].kind,
	})
	select {
	case outcome := <-forwardDone:
		t.Fatalf("main completed while title was blocked: result=%v err=%v", outcome.result, outcome.err)
	default:
	}

	releaseTitle()
	outcome := <-forwardDone
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, []string{"quota", "title", "main"}, []string{
		upstream.Snapshot()[0].kind,
		upstream.Snapshot()[1].kind,
		upstream.Snapshot()[2].kind,
	})
}

func TestGatewayServiceForward_UpstreamApprovalHonorsDelayAndCompanionFailuresFailOpen(t *testing.T) {
	svc, c, account, parsed, upstream := newClaudeOAuthCompanionForwardHarness(t)
	approvalDir := t.TempDir()
	svc.claudeUpstreamApproval = &claudeUpstreamApprovalGate{
		dir:          approvalDir,
		token:        "approval-secret",
		pollInterval: 5 * time.Millisecond,
	}
	c.Request.Header.Set("conversation_id", "delayed-order-conversation")
	c.Request.Header.Set(claudeUpstreamApprovalTokenHeader, "approval-secret")
	c.Request.Header.Set(claudeUpstreamApprovalIDHeader, "delayed-order-turn")
	c.Request.Header.Set(claudeOAuthCompanionDelayHeader, "1")
	upstream.quotaStatus = http.StatusTooManyRequests
	upstream.titleErr = errors.New("synthetic title transport error")
	upstream.quotaStarted = make(chan struct{})
	upstream.titleStarted = make(chan struct{})

	type forwardOutcome struct {
		result *ForwardResult
		err    error
	}
	forwardDone := make(chan forwardOutcome, 1)
	go func() {
		result, err := svc.Forward(context.Background(), c, account, parsed)
		forwardDone <- forwardOutcome{result: result, err: err}
	}()

	waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(approvalDir, "delayed-order-turn-s01.preview.json"),
	)
	approvedAt := time.Now()
	writeClaudeUpstreamApprovalMarker(
		t,
		filepath.Join(approvalDir, "delayed-order-turn-s01.approve"),
	)

	select {
	case <-upstream.quotaStarted:
	case <-time.After(time.Second):
		t.Fatal("quota did not start after approval")
	}
	select {
	case <-upstream.titleStarted:
		t.Fatal("title ignored the configured quota-to-title delay")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-upstream.titleStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("title did not start after the configured delay")
	}
	require.GreaterOrEqual(t, time.Since(approvedAt), 900*time.Millisecond)

	var outcome forwardOutcome
	select {
	case outcome = <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("main remained blocked after companion failures")
	}
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, []string{"quota", "title", "main"}, []string{
		upstream.Snapshot()[0].kind,
		upstream.Snapshot()[1].kind,
		upstream.Snapshot()[2].kind,
	})
}

func TestGatewayServiceForward_UpstreamApprovalWithoutQuotaIncludesEligibleTitle(t *testing.T) {
	svc, c, account, _, upstream := newClaudeOAuthCompanionForwardHarness(t)
	approvalDir := t.TempDir()
	svc.claudeUpstreamApproval = &claudeUpstreamApprovalGate{
		dir:          approvalDir,
		token:        "approval-secret",
		pollInterval: 5 * time.Millisecond,
	}
	c.Request.Header.Set(claudeUpstreamApprovalTokenHeader, "approval-secret")
	c.Request.Header.Set(claudeUpstreamApprovalIDHeader, "sonnet-turn")
	c.Request.Header.Del(claudeOAuthSessionInitHeader)
	parsed := newClaudeOAuthCompanionParsedRequest(
		t,
		"claude-sonnet-4-6",
		true,
		"Review this request without session companions",
	)

	type forwardOutcome struct {
		result *ForwardResult
		err    error
	}
	done := make(chan forwardOutcome, 1)
	go func() {
		result, err := svc.Forward(context.Background(), c, account, parsed)
		done <- forwardOutcome{result: result, err: err}
	}()

	preview := waitForClaudeUpstreamApprovalPreview(
		t,
		filepath.Join(approvalDir, "sonnet-turn-s01.preview.json"),
	)
	require.Equal(t, 0, upstream.Count())
	require.Len(t, preview.Requests, 2)
	require.Equal(t, []string{"title", "main"}, []string{
		preview.Requests[0].Kind,
		preview.Requests[1].Kind,
	})

	writeClaudeUpstreamApprovalMarker(t, filepath.Join(approvalDir, "sonnet-turn-s01.approve"))
	outcome := <-done
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
	require.Equal(t, 2, upstream.Count())
	require.Equal(t, []string{"title", "main"}, []string{
		upstream.Snapshot()[0].kind,
		upstream.Snapshot()[1].kind,
	})
}

func waitForClaudeUpstreamApprovalPreview(
	t *testing.T,
	path string,
) claudeUpstreamApprovalPreview {
	t.Helper()
	var preview claudeUpstreamApprovalPreview
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(data, &preview) == nil
	}, time.Second, 5*time.Millisecond, "approval preview was not written: %s", path)
	return preview
}

func writeClaudeUpstreamApprovalMarker(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("approved\n"), 0o600))
}

func claudeUpstreamApprovalHeaderValue(
	request claudeUpstreamApprovalPreviewRequest,
	name string,
) string {
	for _, header := range request.Headers {
		if http.CanonicalHeaderKey(header.Name) == http.CanonicalHeaderKey(name) &&
			len(header.Values) > 0 {
			return header.Values[0]
		}
	}
	return ""
}
