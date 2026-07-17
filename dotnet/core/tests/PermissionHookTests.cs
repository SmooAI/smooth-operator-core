using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Ports the Rust engine's <c>PermissionHook</c> tests + the deny-policy circuit-breaker precedence
/// proofs, and the agent-level additive-no-op / gating integration.
/// </summary>
public class PermissionHookTests
{
    private static FunctionCallContent Bash(string cmd) => new("c1", "bash", new Dictionary<string, object?> { ["cmd"] = cmd });

    // ── the gate, no approver ──────────────────────────────────────

    [Fact]
    public async Task HookAllowsSafeCommand()
    {
        var hook = new PermissionHook(AutoMode.Ask);
        Assert.Null(await hook.PreCallAsync(Bash("ls -la")));
    }

    [Fact]
    public async Task HookDeniesDangerousCommand()
    {
        var hook = new PermissionHook(AutoMode.Ask);
        var block = await hook.PreCallAsync(Bash("rm -rf /"));
        Assert.NotNull(block);
        Assert.Contains("permission denied", block);
    }

    [Fact]
    public async Task HookFailsClosedOnAsk()
    {
        // No interactive approver → Ask must block.
        var hook = new PermissionHook(AutoMode.Ask);
        Assert.NotNull(await hook.PreCallAsync(Bash("npm install x")));
    }

    [Fact]
    public async Task HookBypassAllowsOrdinaryAskButBlocksBreaker()
    {
        var hook = new PermissionHook(AutoMode.Bypass);
        Assert.Null(await hook.PreCallAsync(Bash("npm install x")));
        Assert.NotNull(await hook.PreCallAsync(Bash("cat ~/.ssh/id_rsa")));
    }

    // ── interactive Ask routing via IHumanGate ─────────────────────

    [Fact]
    public async Task ApproverApprovesLetsAskThrough()
    {
        var gate = new DelegateHumanGate(_ => HumanApprovalResponse.Approve());
        var hook = new PermissionHook(AutoMode.Ask, gate);
        Assert.Null(await hook.PreCallAsync(Bash("npm install x")));
    }

    [Fact]
    public async Task ApproverDeniesBlocksAsk()
    {
        var gate = new DelegateHumanGate(_ => HumanApprovalResponse.Deny("nope"));
        var hook = new PermissionHook(AutoMode.Ask, gate);
        var block = await hook.PreCallAsync(Bash("npm install x"));
        Assert.NotNull(block);
        Assert.Contains("user denied", block);
        Assert.Contains("nope", block);
    }

    [Fact]
    public async Task DenyIsNeverRoutedToHuman()
    {
        // An approver that would approve anything must NOT be able to waive a circuit-breaker.
        var consulted = false;
        var gate = new DelegateHumanGate(_ =>
        {
            consulted = true;
            return HumanApprovalResponse.Approve();
        });
        var hook = new PermissionHook(AutoMode.Ask, gate);
        var block = await hook.PreCallAsync(Bash("rm -rf /"));
        Assert.NotNull(block);
        Assert.Contains("permission denied", block);
        Assert.False(consulted, "a Deny must not prompt the human");
    }

    // ── persisted grants: auto-approve an Ask silently ─────────────

    [Fact]
    public async Task StoredGrantAutoApprovesAskWithoutPrompting()
    {
        var grants = PermissionGrants.New();
        grants.Add(GrantQuery.ForBash("npm "));
        var consulted = false;
        var gate = new DelegateHumanGate(_ =>
        {
            consulted = true;
            return HumanApprovalResponse.Deny("should not be asked");
        });
        var hook = new PermissionHook(AutoMode.Ask, gate, grants: new SharedGrants(grants));
        Assert.Null(await hook.PreCallAsync(Bash("npm install x")));
        Assert.False(consulted, "a granted Ask must not prompt");
    }

    [Fact]
    public async Task ApproveAlwaysPersistsGrant()
    {
        var dir = Directory.CreateTempSubdirectory("permhook");
        try
        {
            var path = Path.Combine(dir.FullName, "wonk-allow.toml");
            var shared = new SharedGrants(PermissionGrants.New());
            var gate = new DelegateHumanGate(_ => HumanApprovalResponse.ApproveAlways());
            var hook = new PermissionHook(AutoMode.Ask, gate, grants: shared, persistPath: path);

            Assert.Null(await hook.PreCallAsync(Bash("npm install x")));
            // The grant was persisted AND merged into the live view.
            Assert.True(shared.Snapshot().MatchesBash("npm install y"));
            Assert.True(PermissionGrants.LoadFromPath(path).MatchesBash("npm test"));

            // A second identical Ask is now silent even if the gate would deny.
            var denyGate = new DelegateHumanGate(_ => HumanApprovalResponse.Deny("x"));
            var hook2 = new PermissionHook(AutoMode.Ask, denyGate, grants: shared, persistPath: path);
            Assert.Null(await hook2.PreCallAsync(Bash("npm install z")));
        }
        finally
        {
            Directory.Delete(dir.FullName, recursive: true);
        }
    }

    // ── DENY POLICY: circuit-breaker precedence (security-critical) ─

    [Fact]
    public async Task DenyPolicyBeatsGrantAndSurvivesBypass()
    {
        var policy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\"]\n");
        var grants = PermissionGrants.New();
        grants.Add(GrantQuery.ForBash("aws ")); // grant would normally cover it

        // With the deny policy: blocked even under Bypass and even with the grant.
        var gated = new PermissionHook(AutoMode.Bypass, denyPolicy: policy, grants: new SharedGrants(grants));
        var block = await gated.PreCallAsync(Bash("aws s3 ls --profile prod"));
        Assert.NotNull(block);
        Assert.Contains("permission denied", block);
        Assert.Contains("(bash)", block);

        // Control: without the deny policy, Bypass + grant lets the very same call through — proving
        // the deny policy (not something else) is what blocks it.
        var ungated = new PermissionHook(AutoMode.Bypass, grants: new SharedGrants(grants));
        Assert.Null(await ungated.PreCallAsync(Bash("aws s3 ls --profile prod")));
    }

    [Fact]
    public async Task DenyPolicyNeverRoutedToHuman()
    {
        var consulted = false;
        var gate = new DelegateHumanGate(_ =>
        {
            consulted = true;
            return HumanApprovalResponse.Approve();
        });
        var policy = DenyPolicy.FromToml("[tools]\ndeny = [\"vendor.dangerous\"]\n");
        var hook = new PermissionHook(AutoMode.Ask, gate, denyPolicy: policy);
        var block = await hook.PreCallAsync(new FunctionCallContent("c1", "vendor.dangerous", new Dictionary<string, object?>()));
        Assert.NotNull(block);
        Assert.False(consulted, "a deny-policy match must not prompt the human");
    }

    [Fact]
    public async Task NoDenyPolicyIsAdditiveNoOp()
    {
        // A hook with no deny policy behaves exactly like the built-in engine.
        var withNone = new PermissionHook(AutoMode.Ask);
        var withEmpty = new PermissionHook(AutoMode.Ask, denyPolicy: new DenyPolicy());
        foreach (var cmd in new[] { "ls", "npm install x", "rm -rf /" })
        {
            Assert.Equal(await withNone.PreCallAsync(Bash(cmd)) is null, await withEmpty.PreCallAsync(Bash(cmd)) is null);
        }
    }

    // ── agent-level integration ────────────────────────────────────

    private static (SmoothAgent agent, Func<bool> ran) BuildBashAgent(AgentOptions options, string command)
    {
        var ran = false;
        var bash = AIFunctionFactory.Create((string cmd) =>
        {
            _ = cmd;
            ran = true;
            return "ran";
        }, "bash", "run a shell command");
        options.Tools.Add(bash);
        var mock = new MockLlmProvider()
            .PushToolCall("call-1", "bash", new Dictionary<string, object?> { ["cmd"] = command })
            .PushText("done");
        return (new SmoothAgent(mock, options), () => ran);
    }

    [Fact]
    public async Task Agent_NoPermissionOptions_RunsToolUnchanged()
    {
        // Additive no-op at the agent level: with neither PermissionMode nor DenyPolicy, the gate is
        // off and even a "dangerous" call runs exactly as before.
        var (agent, ran) = BuildBashAgent(new AgentOptions(), "rm -rf /");
        await agent.RunAsync("go");
        Assert.True(ran(), "gate off → tool executes unchanged");
    }

    [Fact]
    public async Task Agent_PermissionModeAsk_BlocksDangerousToolBeforeExecution()
    {
        var (agent, ran) = BuildBashAgent(new AgentOptions { PermissionMode = AutoMode.Ask }, "rm -rf /");
        var result = await agent.RunAsync("go");
        Assert.False(ran(), "denied tool must not execute");
        Assert.Equal("done", result.Text); // the model got the error result and moved on
    }

    [Fact]
    public async Task Agent_DenyPolicyOnly_EngagesGateAndBlocks()
    {
        // Setting only a deny policy (no PermissionMode) still engages the gate (defaulting to Ask).
        var policy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\"]\n");
        var (agent, ran) = BuildBashAgent(new AgentOptions { DenyPolicy = policy }, "aws s3 rm s3://b --profile prod");
        await agent.RunAsync("go");
        Assert.False(ran(), "deny-policy match must block execution");
    }
}
