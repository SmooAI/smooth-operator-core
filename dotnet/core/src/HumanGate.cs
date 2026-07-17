using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>The human's verdict on a tool call that required approval.</summary>
public enum HumanDecision
{
    Approved,
    Denied,
}

/// <summary>
/// A request for human approval before the agent executes a sensitive/write tool.
/// Mirrors the Rust engine's <c>HumanRequest::Confirm</c>.
/// </summary>
public sealed record HumanApprovalRequest(string ToolName, IDictionary<string, object?>? Arguments, string Prompt);

/// <summary>The response to a <see cref="HumanApprovalRequest"/>. Mirrors the Rust <c>HumanResponse</c>.</summary>
public sealed record HumanApprovalResponse(HumanDecision Decision, string? Reason = null)
{
    public bool IsApproved => Decision == HumanDecision.Approved;

    /// <summary>
    /// When approved, persist a matching grant to the allow-list so the next identical permission Ask
    /// is auto-approved without prompting (the permission gate's "approve always"). Mirrors the Rust
    /// engine's <c>HumanResponse::ApprovedAlways</c>. Ignored on a denial.
    /// </summary>
    public bool Remember { get; init; }

    public static HumanApprovalResponse Approve() => new(HumanDecision.Approved);

    /// <summary>Approve <b>and</b> remember (persist a grant so the gate stops asking).</summary>
    public static HumanApprovalResponse ApproveAlways() => new(HumanDecision.Approved) { Remember = true };

    public static HumanApprovalResponse Deny(string reason) => new(HumanDecision.Denied, reason);
}

/// <summary>
/// The human-in-the-loop seam: the agent consults it before running any tool that
/// <see cref="AgentOptions.RequiresApproval"/> flags. The implementation IS the pause point —
/// a UI gate awaits a real person (e.g. resolving a <see cref="TaskCompletionSource"/> when a
/// button is clicked); a programmatic gate decides immediately. Mirrors the Rust engine's
/// confirmation hook / human channel.
/// </summary>
public interface IHumanGate
{
    Task<HumanApprovalResponse> RequestApprovalAsync(HumanApprovalRequest request, CancellationToken cancellationToken = default);
}

/// <summary>An <see cref="IHumanGate"/> backed by a delegate — handy for wiring a UI or tests.</summary>
public sealed class DelegateHumanGate : IHumanGate
{
    private readonly Func<HumanApprovalRequest, CancellationToken, Task<HumanApprovalResponse>> _handler;

    public DelegateHumanGate(Func<HumanApprovalRequest, HumanApprovalResponse> handler)
        : this((request, _) => Task.FromResult(handler(request)))
    {
    }

    public DelegateHumanGate(Func<HumanApprovalRequest, CancellationToken, Task<HumanApprovalResponse>> handler)
    {
        _handler = handler ?? throw new ArgumentNullException(nameof(handler));
    }

    public Task<HumanApprovalResponse> RequestApprovalAsync(HumanApprovalRequest request, CancellationToken cancellationToken = default) =>
        _handler(request, cancellationToken);
}
