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

    private static Func<List<string>, List<string>> Append(string name) =>
        state => [.. state, name];

    // Typed delegates, not method groups — generic inference can't read a method group.
    private static readonly Func<List<string>, List<string>> Identity = state => state;

    private static readonly Func<List<string>, List<string>, List<string>> TakeChild = (_, child) => child;

    /// <summary>Two tracking nodes joined by a conditional edge that skips a third.</summary>
    private static Workflow<List<string>> TwoNodeChild() =>
        new Workflow<List<string>>()
            .AddNode("child_a", Append("child_a"))
            .AddNode("child_b", Append("child_b"))
            .AddNode("child_never", Append("child_never"))
            .AddConditionalEdge("child_a", s => s.Contains("child_a") ? "child_b" : "child_never")
            .SetEntry("child_a")
            .SetEnd("child_b");

    [Fact]
    public async Task SubWorkflow_RunsToCompletion_InOneParentStep()
    {
        var wf = new Workflow<List<string>>()
            .AddNode("parent_a", Append("parent_a"))
            .AddNode("sub", Workflow.SubWorkflowNode(TwoNodeChild(), Identity, TakeChild))
            .AddNode("parent_b", Append("parent_b"))
            .AddEdge("parent_a", "sub")
            .AddEdge("sub", "parent_b")
            .SetEntry("parent_a")
            .SetEnd("parent_b");

        var result = await wf.RunAsync([]);

        Assert.Equal(new[] { "parent_a", "child_a", "child_b", "parent_b" }, result);
    }

    [Fact]
    public async Task SubWorkflow_MapsStateInAndOut()
    {
        var child = new Workflow<int>()
            .AddNode("add_ten", n => n + 10)
            .AddNode("double", n => n * 2)
            .AddEdge("add_ten", "double")
            .SetEntry("add_ten")
            .SetEnd("double");

        // Parent state is a labelled total; the child only ever sees the int.
        var wf = new Workflow<(string Label, int Total)>()
            .AddNode("math", Workflow.SubWorkflowNode(
                child,
                ((string Label, int Total) p) => p.Total,
                ((string Label, int Total) p, int total) => (p.Label + ":done", total)))
            .SetEntry("math")
            .SetEnd("math");

        var result = await wf.RunAsync(("start", 5));

        Assert.Equal(("start:done", 30), result);
    }

    [Fact]
    public async Task SubWorkflow_ChildError_PropagatesToParent()
    {
        var child = new Workflow<List<string>>()
            .AddNode("boom", (List<string> _) => throw new InvalidOperationException("child exploded"))
            .SetEntry("boom")
            .SetEnd("boom");

        var wf = new Workflow<List<string>>()
            .AddNode("parent_a", Append("parent_a"))
            .AddNode("sub", Workflow.SubWorkflowNode(child, Identity, TakeChild))
            .AddEdge("parent_a", "sub")
            .SetEntry("parent_a")
            .SetEnd("sub");

        var ex = await Assert.ThrowsAsync<InvalidOperationException>(() => wf.RunAsync([]));
        Assert.Equal("child exploded", ex.Message);
    }

    [Fact]
    public async Task SubWorkflow_NestsDeeperThanOneLevel()
    {
        var grandchild = new Workflow<List<string>>()
            .AddNode("grand_a", Append("grand_a"))
            .AddNode("grand_b", Append("grand_b"))
            .AddEdge("grand_a", "grand_b")
            .SetEntry("grand_a")
            .SetEnd("grand_b");

        var child = new Workflow<List<string>>()
            .AddNode("child_a", Append("child_a"))
            .AddNode("grand", Workflow.SubWorkflowNode(grandchild, Identity, TakeChild))
            .AddEdge("child_a", "grand")
            .SetEntry("child_a")
            .SetEnd("grand");

        var wf = new Workflow<List<string>>()
            .AddNode("parent_a", Append("parent_a"))
            .AddNode("sub", Workflow.SubWorkflowNode(child, Identity, TakeChild))
            .AddEdge("parent_a", "sub")
            .SetEntry("parent_a")
            .SetEnd("sub");

        var result = await wf.RunAsync([]);

        Assert.Equal(new[] { "parent_a", "child_a", "grand_a", "grand_b" }, result);
    }
}
