using System.Runtime.CompilerServices;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The LLM call surface the agent loop depends on is MEAI's <see cref="IChatClient"/> — that
/// <i>is</i> this engine's provider seam (the C# analog of the sibling engines' <c>LlmProvider</c>
/// and the Rust reference's <c>LlmClient</c>). The real <c>OpenAIClient.AsChatClient()</c> satisfies
/// it in production; <see cref="MockLlmProvider"/> satisfies it deterministically in tests.
///
/// <see cref="MockLlmProvider"/> is the reusable, shippable record/replay double: script the
/// responses it should return (plain text, tool calls, or errors), drive your code, then assert on
/// <see cref="MockLlmProvider.Calls"/>. It mirrors the BEHAVIOR of the Rust reference's
/// <c>MockLlmClient</c> (<c>rust/smooth-operator-core/src/llm_provider.rs</c>): FIFO scripted
/// outcomes, request recording, and error injection. It serves both the blocking
/// (<see cref="IChatClient.GetResponseAsync"/>) and streaming
/// (<see cref="IChatClient.GetStreamingResponseAsync"/>) surfaces.
/// </summary>
public sealed class MockLlmProvider : IChatClient
{
    /// <summary>The model id stamped on every scripted response (for pricing/cost tests).</summary>
    public const string ModelId = "mock-model";

    private readonly Queue<Outcome> _script = new();
    private readonly List<RecordedCall> _recorded = new();

    private static UsageDetails Tokens() => new() { InputTokenCount = 10, OutputTokenCount = 5, TotalTokenCount = 15 };

    // ── scripting (fluent: each returns this) ────────────────────────────────────────────────

    /// <summary>Script a plain assistant text response (ends the loop).</summary>
    public MockLlmProvider PushText(string text)
    {
        var response = new ChatResponse(new ChatMessage(ChatRole.Assistant, text)) { Usage = Tokens(), ModelId = ModelId };
        _script.Enqueue(Outcome.Message(response));
        return this;
    }

    /// <summary>Script an assistant turn that requests a tool call (continues the loop).</summary>
    public MockLlmProvider PushToolCall(string callId, string name, IDictionary<string, object?> arguments)
    {
        var message = new ChatMessage(ChatRole.Assistant, new List<AIContent> { new FunctionCallContent(callId, name, arguments) });
        var response = new ChatResponse(message) { Usage = Tokens(), ModelId = ModelId };
        _script.Enqueue(Outcome.Message(response));
        return this;
    }

    /// <summary>Script a raw <see cref="ChatResponse"/> for the next call.</summary>
    public MockLlmProvider PushResponse(ChatResponse response)
    {
        _script.Enqueue(Outcome.Message(response));
        return this;
    }

    /// <summary>Script an error to be thrown on the next call (transient-failure / retry tests).</summary>
    public MockLlmProvider PushError(string message)
    {
        _script.Enqueue(Outcome.Error(message));
        return this;
    }

    // ── recordings ───────────────────────────────────────────────────────────────────────────

    /// <summary>The messages passed to the model on each call, in order.</summary>
    public IReadOnlyList<IList<ChatMessage>> Calls => _recorded.Select(r => r.Messages).ToList();

    /// <summary>Every request the mock has received, with the tools it was offered.</summary>
    public IReadOnlyList<RecordedCall> Recordings => _recorded;

    /// <summary>Number of requests received.</summary>
    public int CallCount => _recorded.Count;

    /// <summary>The most recent request, or null if none.</summary>
    public RecordedCall? LastCall => _recorded.Count > 0 ? _recorded[^1] : null;

    // ── the IChatClient surface ──────────────────────────────────────────────────────────────

    public Task<ChatResponse> GetResponseAsync(IEnumerable<ChatMessage> messages, ChatOptions? options = null, CancellationToken cancellationToken = default)
    {
        return Task.FromResult(Next(messages, options));
    }

    public async IAsyncEnumerable<ChatResponseUpdate> GetStreamingResponseAsync(
        IEnumerable<ChatMessage> messages,
        ChatOptions? options = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        var response = Next(messages, options);
        foreach (var update in response.ToChatResponseUpdates())
        {
            await Task.Yield();
            yield return update;
        }
    }

    public object? GetService(Type serviceType, object? serviceKey = null) => null;

    public void Dispose()
    {
    }

    private ChatResponse Next(IEnumerable<ChatMessage> messages, ChatOptions? options)
    {
        _recorded.Add(new RecordedCall(messages.ToList(), options?.Tools?.ToList()));
        if (_script.Count == 0)
        {
            throw new InvalidOperationException("MockLlmProvider: no scripted response left.");
        }
        var outcome = _script.Dequeue();
        if (outcome.IsError)
        {
            throw new InvalidOperationException(outcome.ErrorMessage!);
        }
        return outcome.Response!;
    }

    /// <summary>One request the mock received, captured for assertions.</summary>
    public sealed record RecordedCall(IList<ChatMessage> Messages, IReadOnlyList<AITool>? Tools);

    private readonly record struct Outcome(bool IsError, ChatResponse? Response, string? ErrorMessage)
    {
        public static Outcome Message(ChatResponse response) => new(false, response, null);

        public static Outcome Error(string message) => new(true, null, message);
    }
}
