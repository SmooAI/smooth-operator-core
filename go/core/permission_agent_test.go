package core

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// countingBashClient replies once with a bash tool call, then finishes. Lets a
// test observe whether the tool actually executed after the gate.
type oneToolClient struct {
	toolName string
	cmd      string
	replied  bool
}

func (c *oneToolClient) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	if !c.replied {
		c.replied = true
		return ChatResponse{ToolCalls: []ToolCall{{ID: "t1", Name: c.toolName, Arguments: `{"cmd":"` + c.cmd + `"}`}}}, nil
	}
	return ChatResponse{Content: "done"}, nil
}

func countingBashTool(runs *int32) Tool {
	return FuncTool{
		ToolName: "bash",
		Desc:     "run a command",
		Params:   map[string]any{"type": "object"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			atomic.AddInt32(runs, 1)
			return "ran", nil
		},
	}
}

func mode(m AutoMode) *AutoMode { return &m }

// The gate, wired through AgentOptions, blocks a Deny before the tool executes.
func TestAgentGateBlocksDenyBeforeExecution(t *testing.T) {
	var runs int32
	client := &oneToolClient{toolName: "bash", cmd: "rm -rf /"}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:          []Tool{countingBashTool(&runs)},
		PermissionMode: mode(AutoModeAsk),
		MaxIterations:  4,
	})
	resp, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runs) != 0 {
		t.Error("denied tool must not execute")
	}
	_ = resp
}

// A safe command is allowed and runs exactly once.
func TestAgentGateAllowsSafeCommand(t *testing.T) {
	var runs int32
	client := &oneToolClient{toolName: "bash", cmd: "ls -la"}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:          []Tool{countingBashTool(&runs)},
		PermissionMode: mode(AutoModeAsk),
		MaxIterations:  4,
	})
	if _, err := agent.Run(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runs) != 1 {
		t.Errorf("allowed tool must run once, ran %d", runs)
	}
}

// Additive no-op: with no PermissionMode/DenyPolicy, the gate is absent and a
// command that WOULD be denied under the engine runs unchanged.
func TestAgentNoPermissionOptionsIsNoop(t *testing.T) {
	var runs int32
	client := &oneToolClient{toolName: "bash", cmd: "rm -rf /"}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:         []Tool{countingBashTool(&runs)},
		MaxIterations: 4,
	})
	if agent.permGate != nil {
		t.Error("no permission options must leave permGate nil")
	}
	if _, err := agent.Run(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runs) != 1 {
		t.Error("without the engine the tool must run unchanged")
	}
}

// DenyPolicy alone enables the engine at AutoModeAsk.
func TestAgentDenyPolicyAloneEnablesEngine(t *testing.T) {
	var runs int32
	client := &oneToolClient{toolName: "bash", cmd: "terraform apply"}
	p := mustPolicyT(t, "[bash]\ndeny_patterns = [\"terraform apply\"]")
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:         []Tool{countingBashTool(&runs)},
		DenyPolicy:    p,
		MaxIterations: 4,
	})
	if agent.permGate == nil || agent.permGate.Mode != AutoModeAsk {
		t.Fatal("DenyPolicy must enable the gate at AutoModeAsk")
	}
	resp, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runs) != 0 {
		t.Error("policy-denied tool must not execute")
	}
	// The denial reason is fed back to the model as the tool result.
	_ = resp
}

func mustPolicyT(t *testing.T, toml string) *DenyPolicy {
	t.Helper()
	p, err := DenyPolicyFromTOML(toml)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(toml) == "" {
		t.Fatal("empty policy toml")
	}
	return p
}
