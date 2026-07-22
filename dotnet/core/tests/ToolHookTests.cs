using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Tests for the <see cref="IToolHook"/> lifecycle — the polyglot-parity port of the Rust
/// reference's <c>ToolHook</c> trait: pre-call fires before the tool, post-call fires after with a
/// mutable result (redaction seam), pre-call throw blocks the call, and post-call failures are
/// swallowed. See <c>rust/smooth-operator-core/src/tool.rs</c>.
/// </summary>
public class ToolHookTests
{
    private static List<string> ToolResults(IList<ChatMessage> messages) =>
        messages
            .Where(m => m.Role == ChatRole.Tool)
            .SelectMany(m => m.Contents.OfType<FunctionResultContent>())
            .Select(r => r.Result?.ToString() ?? string.Empty)
            .ToList();

    /// <summary>A spy that records the pre/post calls it observed.</summary>
    private sealed class SpyHook : IToolHook
    {
        public List<string> PreCalls { get; } = new();
        public List<string> PostCalls { get; } = new();

        public Task PreCallAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
        {
            PreCalls.Add(call.Name);
            return Task.CompletedTask;
        }

        public Task PostCallAsync(FunctionCallContent call, FunctionResultContent result, CancellationToken cancellationToken = default)
        {
            PostCalls.Add($"{call.Name}={result.Result}");
            return Task.CompletedTask;
        }
    }

    [Fact]
    public async Task SpyHook_FiresPreAndPost()
    {
        var spy = new SpyHook();
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "echo", new Dictionary<string, object?> { ["text"] = "hi" })
            .PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(AIFunctionFactory.Create((string text) => text, "echo"));
        options.ToolHooks.Add(spy);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        Assert.Equal(new[] { "echo" }, spy.PreCalls);
        Assert.Equal(new[] { "echo=hi" }, spy.PostCalls);
    }

    [Fact]
    public async Task PostCallHook_RedactsResult_ReachesCaller()
    {
        // A post_call hook that rewrites the result content must have its mutation reflected in what
        // the caller (and the model) sees — the redaction seam. Parity with the Rust
        // post_call_hook_redacts_result test.
        var redactor = new DelegatePostHook((_, result) =>
        {
            var text = result.Result?.ToString() ?? string.Empty;
            result.Result = text.Replace("secret", "[REDACTED]", StringComparison.Ordinal);
        });
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "echo", new Dictionary<string, object?> { ["text"] = "the secret is here" })
            .PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(AIFunctionFactory.Create((string text) => text, "echo"));
        options.ToolHooks.Add(redactor);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        // The tool-result message sent back to the model on the second call carries the redacted text.
        var results = ToolResults(mock.Calls[1]);
        Assert.Equal(new[] { "the [REDACTED] is here" }, results);
    }

    [Fact]
    public async Task PreCallHook_Throw_BlocksTool()
    {
        // A pre_call throw blocks the tool: it never runs, and the model gets a "Blocked by hook"
        // result. Parity with the Rust hook_blocks_tool test.
        var toolRan = false;
        var blocker = new DelegatePreHook(call =>
        {
            if (call.Name == "danger")
            {
                throw new InvalidOperationException("tool is blocked by policy");
            }
        });
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "danger", new Dictionary<string, object?>())
            .PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(AIFunctionFactory.Create(() => { toolRan = true; return "ran"; }, "danger"));
        options.ToolHooks.Add(blocker);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        Assert.False(toolRan);
        var results = ToolResults(mock.Calls[1]);
        Assert.Single(results);
        Assert.Contains("Blocked by hook", results[0]);
        Assert.Contains("blocked by policy", results[0]);
    }

    [Fact]
    public async Task PostCallHook_Throw_IsSwallowed()
    {
        // A throwing post_call hook must not fail the tool result — the (possibly redacted) result
        // still reaches the caller. Parity with the Rust "post-hook Err is logged, not surfaced".
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "echo", new Dictionary<string, object?> { ["text"] = "ok" })
            .PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(AIFunctionFactory.Create((string text) => text, "echo"));
        options.ToolHooks.Add(new DelegatePostHook((_, _) => throw new InvalidOperationException("boom")));
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go"); // must not throw

        var results = ToolResults(mock.Calls[1]);
        Assert.Equal(new[] { "ok" }, results);
    }

    [Fact]
    public async Task Hooks_FireInRegistrationOrder()
    {
        var order = new List<string>();
        var first = new DelegatePreHook(_ => order.Add("first"));
        var second = new DelegatePreHook(_ => order.Add("second"));
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "echo", new Dictionary<string, object?> { ["text"] = "x" })
            .PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(AIFunctionFactory.Create((string text) => text, "echo"));
        options.ToolHooks.Add(first);
        options.ToolHooks.Add(second);
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        Assert.Equal(new[] { "first", "second" }, order);
    }

    /// <summary>A hook that runs a delegate on pre-call (and no-ops post-call via the default impl).</summary>
    private sealed class DelegatePreHook : IToolHook
    {
        private readonly Action<FunctionCallContent> _onPre;

        public DelegatePreHook(Action<FunctionCallContent> onPre) => _onPre = onPre;

        public Task PreCallAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
        {
            _onPre(call);
            return Task.CompletedTask;
        }
    }

    /// <summary>A hook that runs a delegate on post-call (and no-ops pre-call via the default impl).</summary>
    private sealed class DelegatePostHook : IToolHook
    {
        private readonly Action<FunctionCallContent, FunctionResultContent> _onPost;

        public DelegatePostHook(Action<FunctionCallContent, FunctionResultContent> onPost) => _onPost = onPost;

        public Task PostCallAsync(FunctionCallContent call, FunctionResultContent result, CancellationToken cancellationToken = default)
        {
            _onPost(call, result);
            return Task.CompletedTask;
        }
    }
}
