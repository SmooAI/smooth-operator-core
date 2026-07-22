using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// A hook that runs around every tool call — the in-process surveillance / redaction seam the
/// smooth-operator engines share. Mirrors the Rust reference's <c>ToolHook</c> trait
/// (<c>rust/smooth-operator-core/src/tool.rs</c>): <see cref="PreCallAsync"/> before the tool runs,
/// <see cref="PostCallAsync"/> after — with a <b>mutable</b> result so a hook can redact.
///
/// The engine speaks Microsoft.Extensions.AI types throughout, so a hook does too: the call is the
/// model's <see cref="FunctionCallContent"/> and the result is the <see cref="FunctionResultContent"/>
/// fed back to the model. Both methods default to a no-op, so a hook overrides only the phase it
/// cares about (e.g. a redactor implements only <see cref="PostCallAsync"/>).
/// </summary>
public interface IToolHook
{
    /// <summary>
    /// Called before the tool executes. <b>Throw to block the call</b> — parity with the Rust
    /// reference's <c>pre_call</c> returning <c>Err</c>. When a hook throws, the tool never runs
    /// and the model is fed a "Blocked by hook" result instead. The default impl is a no-op.
    /// </summary>
    Task PreCallAsync(FunctionCallContent call, CancellationToken cancellationToken = default) => Task.CompletedTask;

    /// <summary>
    /// Called after the tool executes, with the <b>mutable</b> <see cref="FunctionResultContent"/>.
    /// This is a redaction seam, not just an observation point: a hook may rewrite
    /// <see cref="FunctionResultContent.Result"/> (e.g. to scrub a leaked secret) and the mutation is
    /// what the caller — and the model/conversation — sees. Runs on both successful and errored tool
    /// results. A throw here is swallowed (the possibly-redacted result still reaches the caller),
    /// parity with the Rust reference logging <c>post_call</c> errors rather than surfacing them. The
    /// default impl is a no-op.
    /// </summary>
    Task PostCallAsync(FunctionCallContent call, FunctionResultContent result, CancellationToken cancellationToken = default) => Task.CompletedTask;
}
