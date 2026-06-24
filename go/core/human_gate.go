package core

import "context"

// Human-in-the-loop approval — pause before a sensitive/write tool runs.
//
// Phase-2 sibling of the C# HumanGate (dotnet/core) and the Rust engine's
// confirmation hook. When a turn is about to run a tool the caller flagged as
// needing approval, the agent consults a HumanGate first. The gate IS the pause
// point — a UI gate awaits a real person (e.g. a channel resolved when a button
// is clicked); a programmatic gate decides immediately. A denial is never
// executed; the denial reason is fed back to the model as the tool result so the
// model can adapt. With no gate configured, behavior is unchanged.

// HumanDecision is the human's verdict on a tool call that required approval.
type HumanDecision int

const (
	// HumanApproved lets the tool run.
	HumanApproved HumanDecision = iota
	// HumanDenied blocks the tool; the reason is fed back to the model.
	HumanDenied
)

// HumanApprovalRequest is sent before the agent executes a sensitive/write tool.
// Mirrors the C# HumanApprovalRequest / the Rust engine's HumanRequest::Confirm.
type HumanApprovalRequest struct {
	ToolName  string
	Arguments map[string]any
	Prompt    string
}

// HumanApprovalResponse is the answer to a HumanApprovalRequest. Mirrors the C#
// HumanApprovalResponse.
type HumanApprovalResponse struct {
	Decision HumanDecision
	Reason   string
}

// IsApproved reports whether the decision was HumanApproved.
func (r HumanApprovalResponse) IsApproved() bool { return r.Decision == HumanApproved }

// Approve builds an approval.
func Approve() HumanApprovalResponse { return HumanApprovalResponse{Decision: HumanApproved} }

// Deny builds a denial carrying a reason the model will see.
func Deny(reason string) HumanApprovalResponse {
	return HumanApprovalResponse{Decision: HumanDenied, Reason: reason}
}

// HumanGate is the human-in-the-loop seam: the agent consults it before running
// any tool that AgentOptions.RequiresApproval flags. The implementation IS the
// pause point — wire it to a UI awaiting a real person, or decide
// programmatically. Mirrors the C# IHumanGate (here a func, the idiomatic Go seam).
type HumanGate func(ctx context.Context, request HumanApprovalRequest) (HumanApprovalResponse, error)
