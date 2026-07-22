package core

// PermissionGate is the Go port of the Rust reference's PermissionHook (pearl
// th-ab0437). Rust installs it as a ToolHook on the ToolRegistry; the Go engine
// has no ToolHook seam, so the gate is consulted directly in the agent's
// dispatch path (see SmoothAgent.dispatchTool) — an opt-in, additive layer next
// to the existing Clearance / HumanGate seams.
//
// Precedence (all enforced by Check):
//   1. A consumer DenyPolicy is evaluated FIRST — a policy match is a
//      circuit-breaker that wins over grants, Ask, Allow, and every AutoMode
//      (Bypass included). Never routed to a human, never grantable.
//   2. Decide runs the built-in classifier. Deny is a circuit-breaker (never
//      routed to a human, never waivable by a grant). Allow passes.
//   3. Ask consults stored grants first (auto-approve, silent); then the human
//      approver if wired; else FAILS CLOSED (blocks).
//
// An empty gate (no DenyPolicy, no meaningful mode) is only constructed when the
// caller opts in via AgentOptions — with none set, dispatch skips the gate
// entirely and behaves byte-for-byte as before.

import (
	"context"
	"fmt"
	"log"
)

// PermissionGate enforces the permission engine on a single tool call.
type PermissionGate struct {
	// Mode is how aggressively Decide enforces.
	Mode AutoMode
	// Grants is the live merged allow-list consulted before prompting on an Ask.
	// nil disables persistence — every Ask prompts (approve-once).
	Grants *SharedGrants
	// PersistPath is where an ApprovedAlways grant is written (the user-scope
	// file). Empty means approve-always degrades to approve-once.
	PersistPath string
	// DenyPolicy is the consumer deny policy, evaluated first. nil = none.
	DenyPolicy *DenyPolicy
	// Approver routes an Ask verdict to a human. nil fails closed (Ask blocks).
	Approver HumanGate
}

// Check enforces the gate on one tool call. Returns nil to allow the call, or a
// non-nil error describing why it is blocked (the caller surfaces the message to
// the model as the tool result). name is the tool name; args are its parsed
// arguments.
func (g *PermissionGate) Check(ctx context.Context, name string, args map[string]any) error {
	// 1. Deny policy runs FIRST — a consumer deny is a circuit-breaker that wins
	// over grants, ask, allow, and every mode (Bypass included). Never routed to a
	// human, never grantable.
	if g.DenyPolicy != nil {
		if reason, denied := g.DenyPolicy.Evaluate(name, args); denied {
			return fmt.Errorf("permission denied: %s", reason)
		}
	}

	verdict := Decide(g.Mode, name, args)
	switch verdict.Kind {
	case VerdictAllow:
		return nil
	case VerdictDeny:
		// Circuit-breaker — never routed to a human, never grantable.
		return fmt.Errorf("permission denied: %s", verdict.Reason)
	default: // VerdictAsk
		// Consult the persisted allow-list FIRST — a stored grant auto-approves
		// silently (no prompt).
		if g.Grants != nil && coveredByGrants(g.Grants.Snapshot(), name, args) {
			return nil
		}
		if g.Approver == nil {
			// Fail closed: no interactive approver wired.
			return fmt.Errorf("permission requires approval (fail-closed, no approver): %s", verdict.Reason)
		}
		req := HumanApprovalRequest{
			ToolName:  name,
			Arguments: args,
			Prompt:    fmt.Sprintf("Permission: %s. Allow `%s`?", verdict.Reason, name),
		}
		resp, err := g.Approver(ctx, req)
		if err != nil {
			return fmt.Errorf("permission approval failed (failing closed): %w", err)
		}
		if !resp.IsApproved() {
			reason := resp.Reason
			if reason == "" {
				reason = "no reason given"
			}
			return fmt.Errorf("permission denied by user: %s", reason)
		}
		if resp.IsApprovedAlways() {
			g.persistGrant(name, args)
		}
		return nil
	}
}

// persistGrant writes an approve-always grant to disk and merges it into the
// live view. Best-effort: a persistence failure is logged, not fatal — the human
// already approved, so the call still proceeds (approve-always just degrades to
// approve-once this run).
func (g *PermissionGate) persistGrant(name string, args map[string]any) {
	if g.Grants == nil || g.PersistPath == "" {
		return
	}
	query, ok := grantQuery(name, args)
	if !ok {
		return // nothing grantable (shouldn't happen for an Ask)
	}
	if err := AppendGrant(g.PersistPath, query); err != nil {
		log.Printf("permission: failed to persist grant to %s: %v", g.PersistPath, err)
		return
	}
	fresh := NewPermissionGrants()
	fresh.Add(query)
	g.Grants.MergeIn(fresh)
}
