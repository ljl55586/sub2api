package handler

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type inputTokenGuardResult struct {
	Mode                 string
	Limit                int
	Threshold            int
	EstimatedInputTokens int
	Exceeded             bool
}

func (h *GatewayHandler) evaluateInputTokenGuard(model string, body []byte) (*inputTokenGuardResult, error) {
	if h == nil || h.cfg == nil {
		return nil, nil
	}
	cfg := h.cfg.Gateway.InputTokenGuard
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode != config.TokenCountProbeModeShadow && mode != config.TokenCountProbeModeEnforce {
		return nil, nil
	}
	if cfg.MinBodyBytes > 0 && len(body) < cfg.MinBodyBytes {
		return nil, nil
	}

	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	limit := 0
	for _, configured := range cfg.ModelLimits {
		if strings.ToLower(strings.TrimSpace(configured.Model)) == normalizedModel {
			limit = configured.InputTokens
			break
		}
	}
	if limit <= 0 {
		return nil, nil
	}

	estimated, err := service.EstimateAnthropicInputTokens(body)
	if err != nil {
		return nil, fmt.Errorf("estimate input token guard request: %w", err)
	}
	tolerance := cfg.TolerancePercent
	if tolerance < 0 {
		tolerance = 0
	}
	threshold := (limit*(100+tolerance) + 99) / 100
	return &inputTokenGuardResult{
		Mode:                 mode,
		Limit:                limit,
		Threshold:            threshold,
		EstimatedInputTokens: estimated,
		Exceeded:             estimated > threshold,
	}, nil
}
