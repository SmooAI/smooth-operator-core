/**
 * Human-in-the-loop approval — pause before a sensitive/write tool runs.
 *
 * Phase-2 sibling of the C# `HumanGate` (`dotnet/core`) and the Rust engine's
 * confirmation hook. When a turn is about to run a tool the caller flagged as
 * needing approval, the agent consults a {@link HumanGate} first. The gate IS the
 * pause point — a UI gate awaits a real person (e.g. resolving a promise when a
 * button is clicked); a programmatic gate decides immediately. A denial is never
 * executed; the denial reason is fed back to the model as the tool result so the
 * model can adapt. With no gate configured, behavior is unchanged.
 */

/** The human's verdict on a tool call that required approval. */
export enum HumanDecision {
    Approved = 'approved',
    Denied = 'denied',
}

/**
 * A request for human approval before the agent executes a sensitive/write tool.
 * Mirrors the C# `HumanApprovalRequest` / the Rust engine's `HumanRequest::Confirm`.
 */
export interface HumanApprovalRequest {
    toolName: string;
    arguments: Record<string, unknown>;
    prompt: string;
}

/** The response to a {@link HumanApprovalRequest}. Mirrors the C# `HumanApprovalResponse`. */
export interface HumanApprovalResponse {
    decision: HumanDecision;
    reason?: string;
}

/** True when the decision is {@link HumanDecision.Approved}. */
export function isApproved(response: HumanApprovalResponse): boolean {
    return response.decision === HumanDecision.Approved;
}

/** Build an approval. */
export function approve(): HumanApprovalResponse {
    return { decision: HumanDecision.Approved };
}

/** Build a denial carrying a reason the model will see. */
export function deny(reason: string): HumanApprovalResponse {
    return { decision: HumanDecision.Denied, reason };
}

/**
 * The human-in-the-loop seam: the agent consults it before running any tool that
 * {@link AgentOptions.requiresApproval} flags. The implementation IS the pause
 * point — wire it to a UI awaiting a real person, or decide programmatically.
 * Mirrors the C# `IHumanGate` (here just an async function, the idiomatic TS seam).
 */
export type HumanGate = (request: HumanApprovalRequest) => Promise<HumanApprovalResponse>;
