package core

import (
	"context"
	"testing"
)

func intPtr(v int) *int { return &v }

// TestEffectiveMaxTokens covers the clamp: clamp-down, passthrough, nil, and the
// never-0 guarantee — the model-output ceiling logic (EPIC th-1cc9fa).
func TestEffectiveMaxTokens(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		ceiling    *int
		want       int
	}{
		{"nil ceiling passes through", 8192, nil, 8192},
		{"ceiling below budget clamps down", 8192, intPtr(4096), 4096},
		{"ceiling above budget passes through", 512, intPtr(8192), 512},
		{"ceiling equal to budget passes through", 4096, intPtr(4096), 4096},
		{"zero ceiling is treated as unknown (unclamped)", 8192, intPtr(0), 8192},
		{"negative ceiling is treated as unknown (unclamped)", 8192, intPtr(-1), 8192},
		{"never clamps a positive budget to 0", 8192, intPtr(1), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMaxTokens(tc.configured, tc.ceiling); got != tc.want {
				t.Fatalf("effectiveMaxTokens(%d, %v) = %d, want %d", tc.configured, tc.ceiling, got, tc.want)
			}
			if got := effectiveMaxTokens(tc.configured, tc.ceiling); got == 0 && tc.configured > 0 {
				t.Fatalf("effectiveMaxTokens clamped a positive budget to 0")
			}
		})
	}
}

// TestAgentAppliesModelCeiling proves the ceiling threads all the way to the wire:
// the request the agent sends carries the clamped MaxTokens, not the raw budget.
func TestAgentAppliesModelCeiling(t *testing.T) {
	mock := NewMockLlmProvider().PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{
		MaxTokens:      8192,
		ModelMaxOutput: intPtr(2048),
	})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, ok := mock.LastCall()
	if !ok {
		t.Fatal("no model call recorded")
	}
	if call.MaxTokens != 2048 {
		t.Fatalf("request MaxTokens = %d, want clamped 2048", call.MaxTokens)
	}
}

// TestAgentNoCeilingUsesBudget confirms nil ModelMaxOutput leaves the budget alone.
func TestAgentNoCeilingUsesBudget(t *testing.T) {
	mock := NewMockLlmProvider().PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{MaxTokens: 4096})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, _ := mock.LastCall()
	if call.MaxTokens != 4096 {
		t.Fatalf("request MaxTokens = %d, want unclamped 4096", call.MaxTokens)
	}
}

// TestAgentDefaultMaxTokensRaised locks in the raised default (8192, not the old
// starvation-prone 512) when no budget is configured (EPIC th-1cc9fa).
func TestAgentDefaultMaxTokensRaised(t *testing.T) {
	mock := NewMockLlmProvider().PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call, _ := mock.LastCall()
	if call.MaxTokens != defaultMaxTokens {
		t.Fatalf("default request MaxTokens = %d, want %d", call.MaxTokens, defaultMaxTokens)
	}
	if defaultMaxTokens != 8192 {
		t.Fatalf("defaultMaxTokens = %d, want 8192", defaultMaxTokens)
	}
	if defaultMaxIterations != 20 {
		t.Fatalf("defaultMaxIterations = %d, want 20", defaultMaxIterations)
	}
}
