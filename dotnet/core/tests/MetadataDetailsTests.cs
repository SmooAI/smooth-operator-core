using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Parity tests for LLM request metadata (the Rust core's <c>ChatRequest.metadata</c> /
/// <c>with_metadata</c>) and structured tool details (the Rust core's
/// <c>AgentEvent::ToolCallComplete.details</c>). Metadata rides
/// <see cref="ChatOptions.AdditionalProperties"/> so an OpenAI-compat client serializes it as
/// the request's top-level <c>metadata</c> field (LiteLLM records it on spend logs). Details in
/// MEAI ride <see cref="AIContent.AdditionalProperties"/> on the
/// <see cref="FunctionResultContent"/> a post-call hook mutates.
/// </summary>
public class MetadataDetailsTests
{
    [Fact]
    public async Task Metadata_AbsentByDefault()
    {
        var mock = new MockLlmProvider().PushText("ok");
        var agent = new SmoothAgent(mock, new AgentOptions());

        await agent.RunAsync("hi");

        Assert.Null(mock.LastCall!.Metadata);
    }

    [Fact]
    public async Task Metadata_ForwardedVerbatim()
    {
        var mock = new MockLlmProvider().PushText("ok");
        var meta = new Dictionary<string, object?> { ["smooai_agent_slug"] = "support-bot", ["k"] = "v" };
        var agent = new SmoothAgent(mock, new AgentOptions { Metadata = meta });

        await agent.RunAsync("hi");

        var recorded = Assert.IsAssignableFrom<IReadOnlyDictionary<string, object?>>(mock.LastCall!.Metadata);
        Assert.Equal("support-bot", recorded["smooai_agent_slug"]);
        Assert.Equal("v", recorded["k"]);
    }

    [Fact]
    public async Task EmptyMetadata_WireIdenticalToUnset()
    {
        var mock = new MockLlmProvider().PushText("ok");
        var agent = new SmoothAgent(mock, new AgentOptions { Metadata = new Dictionary<string, object?>() });

        await agent.RunAsync("hi");

        Assert.Null(mock.LastCall!.Metadata);
    }

    /// <summary>
    /// The details seam in MEAI: a post-call <see cref="IToolHook"/> attaches structured data via
    /// <see cref="AIContent.AdditionalProperties"/> on the mutable <see cref="FunctionResultContent"/>,
    /// and the same content instance flows into the transcript — so the attached details survive
    /// where a UI can read them, without the model ever seeing them (its text stays the result string).
    /// </summary>
    [Fact]
    public async Task Details_AttachedByPostCallHook_SurviveOnFunctionResultContent()
    {
        var add = AIFunctionFactory.Create((int a, int b) => a + b, "add", "Adds two integers");
        var details = new Dictionary<string, object?> { ["traceId"] = "abc123", ["errorCount"] = 47 };
        FunctionResultContent? seen = null;

        var hook = new DetailsHook(result =>
        {
            (result.AdditionalProperties ??= new AdditionalPropertiesDictionary())["details"] = details;
            seen = result;
        });

        var mock = new MockLlmProvider()
            .PushToolCall("call-1", "add", new Dictionary<string, object?> { ["a"] = 2, ["b"] = 3 })
            .PushText("done");
        var agent = new SmoothAgent(mock, new AgentOptions { Tools = { add }, ToolHooks = { hook } });

        await agent.RunAsync("add them");

        Assert.NotNull(seen);
        var attached = Assert.IsAssignableFrom<IReadOnlyDictionary<string, object?>>(seen!.AdditionalProperties!["details"]);
        Assert.Equal("abc123", attached["traceId"]);
    }

    private sealed class DetailsHook(Action<FunctionResultContent> onResult) : IToolHook
    {
        public Task PostCallAsync(FunctionCallContent call, FunctionResultContent result, CancellationToken cancellationToken = default)
        {
            onResult(result);
            return Task.CompletedTask;
        }
    }
}
