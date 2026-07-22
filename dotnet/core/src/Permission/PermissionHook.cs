using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The tool-call permission gate, ported from the Rust engine's <c>PermissionHook</c>. Consult it
/// before a tool executes: <see cref="PreCallAsync"/> returns <c>null</c> to allow, or a block reason
/// to deny. The agent turns a block reason into a tool-result error so the model can adapt.
///
/// <para>Precedence (security-critical): a <see cref="DenyPolicy"/> match runs <b>first</b> — it is a
/// circuit-breaker that wins over grants, Ask, Allow, and every <see cref="AutoMode"/> (Bypass
/// included) and is never routed to a human. Then <see cref="PermissionEngine.Decide"/> runs: a Deny
/// is a built-in circuit-breaker (never routed to a human, never grantable); an Ask consults the
/// stored grants (silent auto-approve) and, failing that, the human approver — <b>failing closed</b>
/// (block) when no approver is wired.</para>
/// </summary>
public sealed class PermissionHook
{
    private readonly AutoMode _mode;
    private readonly IHumanGate? _approver;
    private readonly SharedGrants? _grants;
    private readonly string? _persistPath;
    private readonly DenyPolicy? _denyPolicy;

    /// <summary>Build a hook. With no approver, an Ask fails closed. With no deny policy, enforcement is byte-identical to the built-in engine.</summary>
    public PermissionHook(AutoMode mode, IHumanGate? approver = null, DenyPolicy? denyPolicy = null, SharedGrants? grants = null, string? persistPath = null)
    {
        _mode = mode;
        _approver = approver;
        _denyPolicy = denyPolicy;
        _grants = grants;
        _persistPath = persistPath;
    }

    /// <summary>Build a hook reading the mode from <c>SMOOTH_AUTO_MODE</c> (default <see cref="AutoMode.Ask"/>).</summary>
    public static PermissionHook FromEnv(IHumanGate? approver = null, DenyPolicy? denyPolicy = null) =>
        new(AutoModeParser.FromEnv(), approver, denyPolicy);

    /// <summary>The mode this hook enforces.</summary>
    public AutoMode Mode => _mode;

    /// <summary>
    /// Gate a tool call. Returns <c>null</c> to allow, or a human/LLM-readable block reason to deny.
    /// </summary>
    public async Task<string?> PreCallAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
    {
        // Deny policy runs FIRST — a consumer-supplied deny is a circuit-breaker that wins over
        // grants, ask, allow, and every mode (Bypass included). Never routed to a human, never grantable.
        if (_denyPolicy is not null)
        {
            var policyReason = _denyPolicy.Evaluate(call);
            if (policyReason is not null)
            {
                return $"permission denied: {policyReason}";
            }
        }

        var args = PermissionEngine.ReadArgs(call);
        switch (PermissionEngine.Decide(_mode, call.Name, args))
        {
            case Verdict.Allow:
                return null;
            // Deny is a circuit-breaker — never routed to a human, never grantable.
            case Verdict.Deny d:
                return $"permission denied: {d.Reason}";
            case Verdict.Ask a:
                // Consult the persisted allow-list FIRST — a stored grant auto-approves silently.
                if (_grants is not null && PermissionEngine.CoveredByGrants(_grants.Snapshot(), call.Name, args))
                {
                    return null;
                }
                if (_approver is null)
                {
                    // Fail closed: no interactive approver wired.
                    return $"permission requires approval (fail-closed, no approver): {a.Reason}";
                }
                var request = new HumanApprovalRequest(call.Name, call.Arguments, $"Permission: {a.Reason}. Allow '{call.Name}'?");
                var decision = await _approver.RequestApprovalAsync(request, cancellationToken).ConfigureAwait(false);
                if (!decision.IsApproved)
                {
                    return $"user denied: {decision.Reason ?? "no reason given"}";
                }
                if (decision.Remember)
                {
                    PersistGrant(call, args);
                }
                return null;
            default:
                return null;
        }
    }

    /// <summary>
    /// Persist an approve-always grant to disk and merge it into the live view. Best-effort: a
    /// persistence failure is swallowed — the human already approved, so the call proceeds (approve-
    /// always just degrades to approve-once this run).
    /// </summary>
    private void PersistGrant(FunctionCallContent call, IReadOnlyDictionary<string, object?>? args)
    {
        if (_grants is null || _persistPath is null)
        {
            return;
        }
        var query = PermissionEngine.GrantQueryFor(call.Name, args);
        if (query is not { } q)
        {
            return; // nothing grantable (shouldn't happen for an Ask)
        }
        try
        {
            PermissionGrants.AppendGrant(_persistPath, q);
            var fresh = PermissionGrants.New();
            fresh.Add(q);
            _grants.MergeIn(fresh);
        }
        catch
        {
            // ponytail: best-effort persistence — a write failure must not fail the approved call.
        }
    }
}
