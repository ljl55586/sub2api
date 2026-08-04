package openai

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestCodexCLI0145BaseInstructionsForGPT56(t *testing.T) {
	prompt := CodexCLI0145BaseInstructionsForModel("gpt-5.6-sol")
	if got, want := firstLine(prompt), "You are Codex, an agent based on GPT-5. You and the user share one workspace, and your job is to collaborate with them until their goal is genuinely handled."; got != want {
		t.Fatalf("unexpected Codex CLI 0.145 prompt head: %q", got)
	}
	if got, want := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt))), "cbefa6b0bede0e332d957fca70ccacf9f12f4c0ecdf81b819e5cbe1a3b16e265"; got != want {
		t.Fatalf("unexpected Codex CLI 0.145 prompt digest: got %s want %s", got, want)
	}
	if got, want := CodexCLI0145BaseInstructionsForModel("gpt-5.5"), CodexBaseInstructionsForModel("gpt-5.5"); got != want {
		t.Fatal("non-GPT-5.6 models must retain the existing prompt selection")
	}
}

// CodexBaseInstructionsForModel 应按模型返回对应的真实 Codex base prompt。
func TestCodexBaseInstructionsForModel(t *testing.T) {
	cases := []struct {
		model    string
		wantHead string
	}{
		{"gpt-5-codex", "You are Codex, based on GPT-5"},
		{"gpt-5.3-codex", "You are Codex, based on GPT-5"},
		{"gpt-5.3-codex-spark", "You are Codex, based on GPT-5"},
		{"gpt-5.1-codex-max", "You are Codex, based on GPT-5"},
		{"gpt-5.2-codex", "You are Codex, based on GPT-5"},
		{"gpt-5.5", "You are Codex, a coding agent based on GPT-5"},
		{" GPT-5.5 ", "You are Codex, a coding agent based on GPT-5"},
		{"gpt-5.2", "You are GPT-5.2 running in the Codex CLI"},
		{"gpt-5.1", "You are GPT-5.1 running in the Codex CLI"},
		{"gpt-5", "You are Codex, a coding agent based on GPT-5"},   // 回退到最新（GPT-5.5）
		{"gpt-5.4", "You are Codex, a coding agent based on GPT-5"}, // 未单独维护 → 最新
		{"gpt-5.3", "You are Codex, a coding agent based on GPT-5"}, // 未单独维护 → 最新
		{"some-unknown-model", "You are Codex, a coding agent based on GPT-5"},
		{"", "You are Codex, a coding agent based on GPT-5"}, // 回退到最新
	}
	for _, c := range cases {
		got := strings.TrimSpace(CodexBaseInstructionsForModel(c.model))
		if got == "" {
			t.Errorf("model %q: got empty instructions", c.model)
			continue
		}
		if !strings.HasPrefix(got, c.wantHead) {
			t.Errorf("model %q: got prefix %q, want %q", c.model, firstLine(got), c.wantHead)
		}
	}
}
