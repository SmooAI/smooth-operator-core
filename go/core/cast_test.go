package core

import (
	"context"
	"strings"
	"testing"
)

// ── Clearance semantics ──────────────────────────────────────────────────────
func TestClearanceEmptyAllowsAll(t *testing.T) {
	c := AllowAllClearance()
	if !c.IsAllowed("anything") || !c.IsAllowed("other") {
		t.Fatal("empty clearance should allow all tools")
	}
}

func TestClearanceDenyEverything(t *testing.T) {
	if DenyAllClearance().IsAllowed("anything") {
		t.Fatal("DenyAll should block all tools")
	}
	// Even an allow-listed tool is blocked when deny-everything is set.
	c := NewClearance([]string{"x"}, nil, true)
	if c.IsAllowed("x") {
		t.Fatal("deny-everything should override the allow-list")
	}
}

func TestClearanceAllowListIsWhitelist(t *testing.T) {
	c := AllowClearance("read", "search")
	if !c.IsAllowed("read") || !c.IsAllowed("search") {
		t.Fatal("whitelisted tools should be allowed")
	}
	if c.IsAllowed("write") {
		t.Fatal("non-whitelisted tool should be blocked")
	}
}

func TestClearanceDenyWinsOverAllow(t *testing.T) {
	c := NewClearance([]string{"read", "write"}, []string{"write"}, false)
	if !c.IsAllowed("read") {
		t.Fatal("read should be allowed")
	}
	if c.IsAllowed("write") {
		t.Fatal("write is both allowed and denied — deny must win")
	}
}

func TestClearanceDenyListOnly(t *testing.T) {
	c := DenyClearance("delete")
	if c.IsAllowed("delete") {
		t.Fatal("denied tool should be blocked")
	}
	if !c.IsAllowed("read") {
		t.Fatal("non-denied tool should be allowed when allow-list is empty")
	}
}

// ── Cast registry ────────────────────────────────────────────────────────────
func TestCastRegistersAndFilters(t *testing.T) {
	cast := NewCast()
	lead := NewOperatorRole("lead", RoleLead, "orchestrate")
	sk := NewOperatorRole("researcher", RoleSidekick, "research")
	shadow := NewOperatorRole("critic", RoleShadow, "observe")
	shadow.Hidden = true
	cast.Register(lead).Register(sk).Register(shadow)

	if cast.Count() != 3 || cast.IsEmpty() {
		t.Fatalf("count=%d isEmpty=%v", cast.Count(), cast.IsEmpty())
	}
	if got, ok := cast.Get("researcher"); !ok || got.Name != "researcher" {
		t.Fatalf("get researcher: got=%+v ok=%v", got, ok)
	}
	if _, ok := cast.Get("missing"); ok {
		t.Fatal("missing role should not be found")
	}
	if sks := cast.Sidekicks(); len(sks) != 1 || sks[0].Name != "researcher" {
		t.Fatalf("sidekicks=%+v", sks)
	}
	visible := cast.ListVisible()
	if len(visible) != 2 {
		t.Fatalf("expected 2 visible roles, got %d", len(visible))
	}
	for _, r := range visible {
		if r.Hidden {
			t.Fatalf("hidden role leaked into visible listing: %s", r.Name)
		}
	}
}

func TestNewOperatorRoleDefaults(t *testing.T) {
	role := NewOperatorRole("lead", RoleLead, "")
	if !role.Permissions.IsAllowed("any-tool") {
		t.Fatal("default role clearance should allow all tools")
	}
	if role.MaxIterations != 8 {
		t.Fatalf("default MaxIterations = %d, want 8", role.MaxIterations)
	}
}

// ── Agent enforcement ────────────────────────────────────────────────────────
func spyTool(name string, executed *[]string) Tool {
	return FuncTool{
		ToolName: name,
		Desc:     "the " + name + " tool",
		Params:   map[string]any{"type": "object", "properties": map[string]any{}},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			*executed = append(*executed, name)
			return name + " ran", nil
		},
	}
}

func TestForbiddenToolIsNotExecuted(t *testing.T) {
	var executed []string
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "write", Arguments: "{}"}}},
		{Content: "ok, I won't write"},
	}}
	clearance := DenyClearance("write")
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{spyTool("write", &executed)}, Clearance: &clearance})
	res, err := agent.Run(context.Background(), "please write", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok, I won't write" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1 (counted)", res.ToolCalls)
	}
	if len(executed) != 0 {
		t.Fatalf("forbidden tool body ran: %+v", executed)
	}
	// The model was told the tool isn't permitted (fed back as a tool result).
	second := client.calls[1].Messages
	found := false
	for _, m := range second {
		if m.Role == "tool" && strings.Contains(m.Content, "not permitted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denial not fed back to the model; messages=%+v", second)
	}
}

func TestAllowedToolStillRuns(t *testing.T) {
	var executed []string
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "read", Arguments: "{}"}}},
		{Content: "done"},
	}}
	clearance := AllowClearance("read")
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{spyTool("read", &executed)}, Clearance: &clearance})
	res, err := agent.Run(context.Background(), "please read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || len(executed) != 1 || executed[0] != "read" {
		t.Fatalf("res=%+v executed=%+v", res, executed)
	}
}

func TestNoClearanceAllowsEveryTool(t *testing.T) {
	var executed []string
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "write", Arguments: "{}"}}},
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{spyTool("write", &executed)}}) // no clearance
	if _, err := agent.Run(context.Background(), "please write", nil); err != nil {
		t.Fatal(err)
	}
	if len(executed) != 1 || executed[0] != "write" {
		t.Fatalf("executed=%+v", executed)
	}
}
