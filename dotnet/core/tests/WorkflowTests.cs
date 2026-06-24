using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-3 parity tests: the typed workflow graph with static + conditional edges. Mirrors the
/// sibling engines' <c>Workflow</c> behaviour.
/// </summary>
public class WorkflowTests
{
    [Fact]
    public async Task LinearGraph_RunsNodesInOrder()
    {
        var trail = new List<string>();
        var wf = new Workflow<int>()
            .AddNode("a", s => { trail.Add("a"); return s + 1; })
            .AddNode("b", s => { trail.Add("b"); return s * 2; })
            .AddNode("c", s => { trail.Add("c"); return s - 3; })
            .AddEdge("a", "b")
            .AddEdge("b", "c")
            .SetEnd("c")
            .SetEntry("a");

        var result = await wf.RunAsync(5);

        Assert.Equal(new[] { "a", "b", "c" }, trail);
        Assert.Equal(((5 + 1) * 2) - 3, result); // 9
    }

    [Fact]
    public async Task NodeWithNoOutgoingEdge_Terminates()
    {
        var wf = new Workflow<string>()
            .AddNode("only", s => s + "!")
            .SetEntry("only");

        Assert.Equal("hi!", await wf.RunAsync("hi"));
    }

    [Fact]
    public async Task ConditionalEdge_RoutesOnState()
    {
        var wf = new Workflow<int>()
            .AddNode("start", s => s)
            .AddNode("even", s => s + 100)
            .AddNode("odd", s => s + 1)
            .AddConditionalEdge("start", s => s % 2 == 0 ? "even" : "odd")
            .SetEnd("even")
            .SetEnd("odd")
            .SetEntry("start");

        Assert.Equal(104, await wf.RunAsync(4));
        Assert.Equal(6, await wf.RunAsync(5));
    }

    [Fact]
    public async Task ConditionalEdge_EndSentinel_Terminates()
    {
        var wf = new Workflow<int>()
            .AddNode("start", s => s + 1)
            .AddConditionalEdge("start", _ => Workflow<int>.End)
            .SetEntry("start");

        Assert.Equal(8, await wf.RunAsync(7));
    }

    [Fact]
    public async Task AsyncNode_IsAwaited()
    {
        var wf = new Workflow<int>()
            .AddNode("slow", async (s, ct) => { await Task.Yield(); return s + 10; })
            .SetEntry("slow");

        Assert.Equal(11, await wf.RunAsync(1));
    }

    [Fact]
    public async Task NoEntry_Throws()
    {
        var wf = new Workflow<int>().AddNode("a", s => s);
        await Assert.ThrowsAsync<WorkflowException>(() => wf.RunAsync(0));
    }

    [Fact]
    public async Task EntryNodeMissing_Throws()
    {
        var wf = new Workflow<int>().AddNode("a", s => s).SetEntry("nope");
        await Assert.ThrowsAsync<WorkflowException>(() => wf.RunAsync(0));
    }

    [Fact]
    public async Task EdgeToMissingNode_Throws()
    {
        var wf = new Workflow<int>()
            .AddNode("a", s => s)
            .AddEdge("a", "ghost")
            .SetEntry("a");
        await Assert.ThrowsAsync<WorkflowException>(() => wf.RunAsync(0));
    }

    [Fact]
    public async Task UnbrokenCycle_ExceedsMaxSteps_Throws()
    {
        var wf = new Workflow<int>(maxSteps: 5)
            .AddNode("a", s => s + 1)
            .AddNode("b", s => s + 1)
            .AddEdge("a", "b")
            .AddEdge("b", "a")
            .SetEntry("a");
        await Assert.ThrowsAsync<WorkflowException>(() => wf.RunAsync(0));
    }
}
