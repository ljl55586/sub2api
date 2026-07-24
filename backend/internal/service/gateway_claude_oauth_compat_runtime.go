package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// claudeOAuthCompatRuntime carries the P1 lifecycle through protocol adapters
// that convert OpenAI Chat Completions or Responses requests to Anthropic.
type claudeOAuthCompatRuntime struct {
	turn                       *claudeOAuthRuntimeTurn
	metadataUserID             string
	titleCandidateText         string
	metadataPassthroughEnabled bool
	finished                   bool
}

func (s *GatewayService) prepareClaudeOAuthCompatRuntime(
	ctx context.Context,
	c *gin.Context,
	parsed *ParsedRequest,
	account *Account,
	downstreamBody, anthropicBody []byte,
	systemRaw any,
	modelID string,
) ([]byte, *claudeOAuthCompatRuntime, error) {
	state := &claudeOAuthCompatRuntime{
		titleCandidateText: extractLastUserText(anthropicBody),
	}
	if s.settingService != nil {
		_, state.metadataPassthroughEnabled, _ = s.settingService.GetGatewayForwardingSettings(ctx)
	}
	if c != nil {
		c.Set(oauthMimicMetadataPassthroughContextKey, state.metadataPassthroughEnabled)
	}

	if !state.metadataPassthroughEnabled {
		// prompt_cache_key exists only on the downstream OpenAI shape. Without
		// it, use the converted Anthropic body so the first user message remains
		// the stable fallback conversation anchor.
		locatorBody := anthropicBody
		if strings.TrimSpace(gjson.GetBytes(downstreamBody, "prompt_cache_key").String()) != "" {
			locatorBody = downstreamBody
		}
		runtimeTurn, hydratedBody, runtimeErr := s.prepareClaudeOAuthRuntimeWithLocatorBody(
			ctx,
			c,
			parsed,
			account.ID,
			"",
			anthropicBody,
			locatorBody,
		)
		if runtimeErr != nil {
			logger.LegacyPrintf(
				"service.gateway",
				"Claude OAuth compatibility runtime unavailable; falling back to request-local history: account_id=%d error=%v",
				account.ID,
				runtimeErr,
			)
		} else {
			state.turn = runtimeTurn
			anthropicBody = hydratedBody
		}

		runtimeSessionID := ""
		if state.turn != nil {
			runtimeSessionID = state.turn.SessionID
		}
		// The metadata helper only needs an Anthropic body when no runtime
		// session is available and it must derive the legacy fallback session.
		metadataParsed := &ParsedRequest{Body: NewRequestBodyRef(anthropicBody)}
		if parsed != nil {
			metadataParsed.SessionContext = parsed.SessionContext
		}
		metadataUserID, err := s.buildOAuthMimicMetadataUserIDForSession(
			ctx,
			c,
			metadataParsed,
			account,
			runtimeSessionID,
		)
		if err != nil {
			s.releaseClaudeOAuthCompatRuntime(ctx, account, state)
			return nil, nil, err
		}
		state.metadataUserID = metadataUserID
		if c != nil {
			c.Set(oauthMimicMetadataFinalContextKey, true)
		}
	}

	anthropicBody = s.applyClaudeCodeOAuthMimicryToBodyWithMetadata(
		ctx,
		c,
		account,
		anthropicBody,
		systemRaw,
		modelID,
		state.metadataUserID,
	)
	return anthropicBody, state, nil
}

func (s *GatewayService) releaseClaudeOAuthCompatRuntime(ctx context.Context, account *Account, state *claudeOAuthCompatRuntime) {
	if state == nil || state.finished || state.turn == nil || !state.turn.Claimed || account == nil {
		return
	}
	s.releaseClaudeOAuthRuntimeTurn(context.WithoutCancel(ctx), account.ID, state.turn)
	state.finished = true
}

func (s *GatewayService) commitClaudeOAuthCompatRuntime(ctx context.Context, c *gin.Context, account *Account, state *claudeOAuthCompatRuntime) {
	if state == nil || state.finished || state.turn == nil || !state.turn.Claimed || account == nil {
		return
	}
	var assistantContent json.RawMessage
	if c != nil {
		if value, ok := c.Get(claudeOAuthAssistantContentContextKey); ok {
			if content, ok := value.([]byte); ok {
				assistantContent = append(json.RawMessage(nil), content...)
			}
		}
	}
	if len(bytes.TrimSpace(assistantContent)) == 0 {
		s.releaseClaudeOAuthRuntimeTurn(context.WithoutCancel(ctx), account.ID, state.turn)
	} else if err := s.commitClaudeOAuthRuntimeTurn(
		context.WithoutCancel(ctx),
		account.ID,
		state.turn,
		assistantContent,
	); err != nil {
		logger.LegacyPrintf(
			"service.gateway",
			"Claude OAuth compatibility transcript commit failed: account_id=%d error=%v",
			account.ID,
			err,
		)
	}
	state.finished = true
}

func (s *GatewayService) dispatchClaudeOAuthCompatCompanions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	state *claudeOAuthCompatRuntime,
	modelID, token, tokenType, proxyURL string,
	tlsProfile *tlsfingerprint.Profile,
) {
	if state == nil {
		return
	}
	runtimeKey := ""
	if state.turn != nil {
		runtimeKey = state.turn.RuntimeKey
	}
	s.dispatchClaudeOAuthSessionCompanions(ctx, claudeOAuthCompanionDispatchInput{
		c:                          c,
		account:                    account,
		modelID:                    modelID,
		token:                      token,
		tokenType:                  tokenType,
		reqStream:                  true,
		mimicClaudeCode:            true,
		metadataUserID:             state.metadataUserID,
		runtimeKey:                 runtimeKey,
		titleCandidateText:         state.titleCandidateText,
		metadataPassthroughEnabled: state.metadataPassthroughEnabled,
		proxyURL:                   proxyURL,
		tlsProfile:                 tlsProfile,
	})
}
