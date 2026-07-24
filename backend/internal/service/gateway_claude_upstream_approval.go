package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	claudeUpstreamApprovalDirEnv      = "SUB2API_UPSTREAM_APPROVAL_DIR"
	claudeUpstreamApprovalTokenEnv    = "SUB2API_UPSTREAM_APPROVAL_TOKEN"
	claudeUpstreamApprovalIDHeader    = "X-Sub2API-Upstream-Approval-ID"
	claudeUpstreamApprovalTokenHeader = "X-Sub2API-Upstream-Approval-Token"
)

var (
	claudeUpstreamApprovalIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errClaudeUpstreamApprovalRejected = errors.New("Claude upstream request was rejected before network send")
)

type claudeUpstreamApprovalGate struct {
	dir          string
	token        string
	pollInterval time.Duration
	initErr      error
}

type claudeUpstreamApprovalSession struct {
	gate       *claudeUpstreamApprovalGate
	ctx        context.Context
	approvalID string
	mu         sync.Mutex
	sequence   int
}

type claudeUpstreamApprovalStage struct {
	session *claudeUpstreamApprovalSession
	stageID string

	mu       sync.Mutex
	expected int
	requests map[string]claudeUpstreamApprovalPreviewRequest
	started  bool

	done     chan struct{}
	doneOnce sync.Once
	err      error
}

type claudeUpstreamApprovalPreview struct {
	Version     int                                    `json:"version"`
	ApprovalID  string                                 `json:"approval_id"`
	StageID     string                                 `json:"stage_id"`
	CreatedAt   string                                 `json:"created_at"`
	NetworkSent bool                                   `json:"network_sent"`
	Requests    []claudeUpstreamApprovalPreviewRequest `json:"requests"`
}

type claudeUpstreamApprovalPreviewRequest struct {
	Kind          string                                `json:"kind"`
	Method        string                                `json:"method"`
	URL           string                                `json:"url"`
	Headers       []claudeUpstreamApprovalPreviewHeader `json:"headers"`
	BodySHA256    string                                `json:"body_sha256"`
	ContentLength int64                                 `json:"content_length"`
	Body          json.RawMessage                       `json:"body"`
}

type claudeUpstreamApprovalPreviewHeader struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type claudeUpstreamApprovalResult struct {
	ApprovalID string `json:"approval_id"`
	StageID    string `json:"stage_id"`
	Decision   string `json:"decision"`
	FinishedAt string `json:"finished_at"`
	Error      string `json:"error,omitempty"`
}

func newClaudeUpstreamApprovalGateFromEnv() *claudeUpstreamApprovalGate {
	dir := strings.TrimSpace(os.Getenv(claudeUpstreamApprovalDirEnv))
	token := strings.TrimSpace(os.Getenv(claudeUpstreamApprovalTokenEnv))
	if dir == "" && token == "" {
		return nil
	}
	gate := &claudeUpstreamApprovalGate{
		dir:          dir,
		token:        token,
		pollInterval: 100 * time.Millisecond,
	}
	if dir == "" || token == "" {
		gate.initErr = fmt.Errorf("%s and %s must both be configured", claudeUpstreamApprovalDirEnv, claudeUpstreamApprovalTokenEnv)
		return gate
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		gate.initErr = fmt.Errorf("create upstream approval directory: %w", err)
		return gate
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		gate.initErr = fmt.Errorf("secure upstream approval directory: %w", err)
	}
	return gate
}

func (s *GatewayService) beginClaudeUpstreamApproval(
	c *gin.Context,
	eligible bool,
) (*claudeUpstreamApprovalSession, error) {
	if c == nil || c.Request == nil {
		return nil, nil
	}
	approvalID := strings.TrimSpace(c.GetHeader(claudeUpstreamApprovalIDHeader))
	providedToken := strings.TrimSpace(c.GetHeader(claudeUpstreamApprovalTokenHeader))
	if approvalID == "" && providedToken == "" {
		return nil, nil
	}
	if approvalID == "" || providedToken == "" {
		return nil, errors.New("upstream approval ID and token headers must both be provided")
	}
	if !eligible {
		return nil, errors.New("upstream approval is only supported for streaming Claude OAuth mimic requests")
	}
	if !claudeUpstreamApprovalIDPattern.MatchString(approvalID) {
		return nil, errors.New("invalid upstream approval ID")
	}
	if s == nil || s.claudeUpstreamApproval == nil {
		return nil, errors.New("upstream approval was requested but the server gate is disabled")
	}
	gate := s.claudeUpstreamApproval
	if gate.initErr != nil {
		return nil, gate.initErr
	}
	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(gate.token)) != 1 {
		return nil, errors.New("invalid upstream approval token")
	}
	return &claudeUpstreamApprovalSession{
		gate:       gate,
		ctx:        c.Request.Context(),
		approvalID: approvalID,
	}, nil
}

func (s *claudeUpstreamApprovalSession) NewStage() *claudeUpstreamApprovalStage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.sequence++
	sequence := s.sequence
	s.mu.Unlock()
	return &claudeUpstreamApprovalStage{
		session:  s,
		stageID:  fmt.Sprintf("%s-s%02d", s.approvalID, sequence),
		requests: make(map[string]claudeUpstreamApprovalPreviewRequest),
		done:     make(chan struct{}),
	}
}

func (s *claudeUpstreamApprovalStage) SetExpected(expected int) error {
	if s == nil {
		return nil
	}
	if expected <= 0 {
		return errors.New("upstream approval stage requires at least one request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expected != 0 && s.expected != expected {
		return fmt.Errorf("upstream approval stage expected count changed from %d to %d", s.expected, expected)
	}
	s.expected = expected
	s.startIfReadyLocked()
	return nil
}

func (s *claudeUpstreamApprovalStage) Await(kind string, req *http.Request) error {
	if s == nil {
		return nil
	}
	preview, err := buildClaudeUpstreamApprovalPreviewRequest(kind, req)
	if err != nil {
		s.finish("error", err)
		return err
	}

	s.mu.Lock()
	if _, exists := s.requests[kind]; exists {
		s.mu.Unlock()
		err := fmt.Errorf("duplicate upstream approval request kind %q", kind)
		s.finish("error", err)
		return err
	}
	s.requests[kind] = preview
	s.startIfReadyLocked()
	s.mu.Unlock()

	select {
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err
	case <-s.session.ctx.Done():
		err := s.session.ctx.Err()
		s.finish("cancelled", err)
		return err
	}
}

func (s *claudeUpstreamApprovalStage) startIfReadyLocked() {
	if s.started || s.expected <= 0 || len(s.requests) != s.expected {
		return
	}
	s.started = true
	requests := make([]claudeUpstreamApprovalPreviewRequest, 0, len(s.requests))
	for _, request := range s.requests {
		requests = append(requests, request)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		return claudeUpstreamApprovalKindOrder(requests[i].Kind) <
			claudeUpstreamApprovalKindOrder(requests[j].Kind)
	})
	go s.waitForDecision(requests)
}

func claudeUpstreamApprovalKindOrder(kind string) int {
	switch kind {
	case "quota":
		return 0
	case "title":
		return 1
	case "main":
		return 2
	default:
		return 3
	}
}

func (s *claudeUpstreamApprovalStage) waitForDecision(requests []claudeUpstreamApprovalPreviewRequest) {
	previewPath := filepath.Join(s.session.gate.dir, s.stageID+".preview.json")
	approvePath := filepath.Join(s.session.gate.dir, s.stageID+".approve")
	rejectPath := filepath.Join(s.session.gate.dir, s.stageID+".reject")
	resultPath := filepath.Join(s.session.gate.dir, s.stageID+".result.json")
	for _, path := range []string{previewPath, approvePath, rejectPath, resultPath} {
		if _, err := os.Stat(path); err == nil {
			s.finish("error", fmt.Errorf("stale upstream approval artifact already exists: %s", filepath.Base(path)))
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			s.finish("error", fmt.Errorf("inspect upstream approval artifact: %w", err))
			return
		}
	}

	preview := claudeUpstreamApprovalPreview{
		Version:     1,
		ApprovalID:  s.session.approvalID,
		StageID:     s.stageID,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		NetworkSent: false,
		Requests:    requests,
	}
	if err := writeClaudeUpstreamApprovalJSON(previewPath, preview); err != nil {
		s.finish("error", err)
		return
	}

	pollInterval := s.session.gate.pollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(rejectPath); err == nil {
			s.finish("rejected", errClaudeUpstreamApprovalRejected)
			return
		}
		if _, err := os.Stat(approvePath); err == nil {
			s.finish("approved", nil)
			return
		}
		select {
		case <-s.session.ctx.Done():
			s.finish("cancelled", s.session.ctx.Err())
			return
		case <-ticker.C:
		}
	}
}

func (s *claudeUpstreamApprovalStage) finish(decision string, err error) {
	if s == nil {
		return
	}
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		result := claudeUpstreamApprovalResult{
			ApprovalID: s.session.approvalID,
			StageID:    s.stageID,
			Decision:   decision,
			FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err != nil {
			result.Error = err.Error()
		}
		resultPath := filepath.Join(s.session.gate.dir, s.stageID+".result.json")
		if writeErr := writeClaudeUpstreamApprovalJSON(resultPath, result); writeErr != nil {
			slog.Error("failed to write upstream approval result", "stage_id", s.stageID, "error", writeErr)
		}
		close(s.done)
	})
}

func buildClaudeUpstreamApprovalPreviewRequest(
	kind string,
	req *http.Request,
) (claudeUpstreamApprovalPreviewRequest, error) {
	if req == nil || req.URL == nil {
		return claudeUpstreamApprovalPreviewRequest{}, errors.New("cannot preview an empty upstream request")
	}
	if kind == "" {
		return claudeUpstreamApprovalPreviewRequest{}, errors.New("cannot preview an upstream request without a kind")
	}
	if req.GetBody == nil {
		return claudeUpstreamApprovalPreviewRequest{}, errors.New("upstream request body cannot be replayed for approval preview")
	}
	bodyReader, err := req.GetBody()
	if err != nil {
		return claudeUpstreamApprovalPreviewRequest{}, fmt.Errorf("open upstream request body for approval preview: %w", err)
	}
	body, readErr := io.ReadAll(bodyReader)
	_ = bodyReader.Close()
	if readErr != nil {
		return claudeUpstreamApprovalPreviewRequest{}, fmt.Errorf("read upstream request body for approval preview: %w", readErr)
	}
	if !json.Valid(body) {
		return claudeUpstreamApprovalPreviewRequest{}, errors.New("upstream approval preview body is not valid JSON")
	}
	sum := sha256.Sum256(body)
	headers := make([]claudeUpstreamApprovalPreviewHeader, 0, len(req.Header))
	for _, name := range sortHeadersByWireOrder(req.Header) {
		// Keep the exact values stored under the original map key. Some Claude
		// wire-order helpers deliberately retain lowercase keys; Header.Values
		// canonicalizes its lookup and would silently omit those entries.
		values := req.Header[name]
		safeValues := make([]string, 0, len(values))
		for _, value := range values {
			safeValues = append(safeValues, safeHeaderValueForLog(name, value))
		}
		headers = append(headers, claudeUpstreamApprovalPreviewHeader{
			Name:   name,
			Values: safeValues,
		})
	}
	return claudeUpstreamApprovalPreviewRequest{
		Kind:          kind,
		Method:        req.Method,
		URL:           req.URL.String(),
		Headers:       headers,
		BodySHA256:    hex.EncodeToString(sum[:]),
		ContentLength: int64(len(body)),
		Body:          append(json.RawMessage(nil), body...),
	}, nil
}

func writeClaudeUpstreamApprovalJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".upstream-approval-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
