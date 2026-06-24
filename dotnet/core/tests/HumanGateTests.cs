using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-4 parity tests: human-in-the-loop approval gating of sensitive/write tools.
/// Mirrors the Rust engine's confirmation hook — a denied tool never executes and the model
/// is told it was denied.
/// </summary>
public class HumanGateTests
{
    private static (AIFunction tool, Func<bool> fired) Destructive(string name)
    {
        var fired = false;
        var tool = AIFunctionFactory.Create(
            () =>
            {
                fired = true;
                return "done";
            },
            name,
            "a destructive action");
        return (tool, () => fired);
    }

    [Fact]
    public async Task DeniedTool_DoesNotExecute_AndModelGetsDenial()
    {
        var (tool, fired) = Destructive("delete_account");
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "delete_account", new Dictionary<string, object?>())
            .PushText("Okay, I won't delete it.");
        var options = new AgentOptions
        {
            HumanGate = new DelegateHumanGate(_ => HumanApprovalResponse.Deny("user said no")),
            RequiresApproval = call => call.Name == "delete_account",
        };
        options.Tools.Add(tool);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("delete my account");

        Assert.False(fired(), "a denied tool must not execute");
        var toolMessage = mock.Calls[1].First(m => m.Role == ChatRole.Tool);
        Assert.Contains(toolMessage.Contents.OfType<FunctionResultContent>(),
            r => r.Result is string s && s.Contains("Denied by human") && s.Contains("user said no"));
        Assert.Equal("Okay, I won't delete it.", result.Text);
    }

    [Fact]
    public async Task ApprovedTool_Executes_AndGateSawTheCall()
    {
        var (tool, fired) = Destructive("delete_account");
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "delete_account", new Dictionary<string, object?> { ["id"] = 42 })
            .PushText("Done.");
        var seen = new List<HumanApprovalRequest>();
        var options = new AgentOptions
        {
            HumanGate = new DelegateHumanGate(req =>
            {
                seen.Add(req);
                return HumanApprovalResponse.Approve();
            }),
            RequiresApproval = call => call.Name == "delete_account",
        };
        options.Tools.Add(tool);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("delete account 42");

        Assert.True(fired(), "an approved tool should execute");
        Assert.Single(seen);
        Assert.Equal("delete_account", seen[0].ToolName);
        Assert.Equal(42, seen[0].Arguments!["id"]);
    }

    [Fact]
    public async Task NonFlaggedTool_RunsWithoutConsultingGate()
    {
        var (tool, fired) = Destructive("read_status"); // not flagged for approval
        var gateConsulted = 0;
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "read_status", new Dictionary<string, object?>())
            .PushText("all good");
        var options = new AgentOptions
        {
            HumanGate = new DelegateHumanGate(_ =>
            {
                gateConsulted++;
                return HumanApprovalResponse.Approve();
            }),
            RequiresApproval = call => call.Name == "delete_account",
        };
        options.Tools.Add(tool);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("status?");

        Assert.Equal(0, gateConsulted);
        Assert.True(fired());
    }
}
