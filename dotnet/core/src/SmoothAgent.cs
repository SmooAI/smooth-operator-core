using System.Runtime.CompilerServices;
using System.Text;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The smooth-operator agent engine, native C#. It drives the agentic loop over any
/// <see cref="IChatClient"/>: call the model, execute any requested tools, feed the
/// results back, and repeat until the model answers without tool calls (or the
/// iteration cap is hit). This is the in-process sibling of the Rust
/// <c>smooai-smooth-operator-core</c> <c>Agent</c>; behavioral parity is enforced by
/// the shared conformance fixtures + eval scenarios, not identical type shapes.
///
/// We own the loop deliberately (rather than delegating to MEAI's
/// <c>FunctionInvokingChatClient</c>) so later phases can layer in checkpointing,
/// HITL pause/resume, knowledge/memory injection, cost budgets, and cast/subagents
/// with smooth-operator's exact semantics.
///
/// Multi-turn conversations carry through a <see cref="SmoothAgentThread"/>; the
/// conversation is trimmed to <see cref="AgentOptions.MaxContextTokens"/> before each
/// LLM call via <see cref="AgentOptions.Compaction"/>.
/// </summary>
public sealed class SmoothAgent
{
    private readonly IChatClient _chatClient;
    private readonly AgentOptions _options;
    private readonly Dictionary<string, AIFunction> _functions;
    private readonly ToolSearch? _toolSearch;

    public SmoothAgent(IChatClient chatClient, AgentOptions options)
    {
        _chatClient = chatClient ?? throw new ArgumentNullException(nameof(chatClient));
        _options = options ?? throw new ArgumentNullException(nameof(options));
        _functions = options.Tools.OfType<AIFunction>().ToDictionary(f => f.Name, StringComparer.Ordinal);
        _toolSearch = options.DeferredTools.Count > 0 ? new ToolSearch(options.DeferredTools.OfType<AIFunction>()) : null;
    }

    /// <summary>Start a fresh conversation thread for multi-turn use. (MAF: <c>GetNewThread</c>.)</summary>
    public SmoothAgentThread GetNewThread() => new();

    /// <summary>Run a single stateless turn (no carried history).</summary>
    public Task<AgentRunResponse> RunAsync(string message, CancellationToken cancellationToken = default) =>
        RunAsync(message, null, cancellationToken);

    /// <summary>
    /// Run a turn within <paramref name="thread"/> (or stateless if null). The thread's prior
    /// messages are prepended; the new user/assistant/tool messages from this turn are appended
    /// back to it. (MAF naming: <c>RunAsync</c>.)
    /// </summary>
    public async Task<AgentRunResponse> RunAsync(string message, SmoothAgentThread? thread, CancellationToken cancellationToken = default)
    {
        var working = await SeedConversationAsync(message, thread, cancellationToken).ConfigureAwait(false);
        var newThisTurn = new List<ChatMessage> { working[^1] }; // the live user message
        var usage = new UsageDetails();
        var cost = new CostTracker();
        BudgetExceeded? budgetHit = null;
        var iterations = 0;

        while (true)
        {
            iterations++;
            Compactor.Compact(working, _options.Compaction, _options.MaxContextTokens);

            // Recompute the visible tool set each iteration: tool_search promotions during the
            // previous iteration widen what the model can see/call now.
            var chatOptions = BuildChatOptions();
            var response = await CallModelAsync(working, chatOptions, cancellationToken).ConfigureAwait(false);
            Accumulate(usage, response.Usage);
            cost.Record(response.ModelId, response.Usage, LookupPricing(response.ModelId));
            working.AddRange(response.Messages);
            newThisTurn.AddRange(response.Messages);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterEachIteration, cancellationToken).ConfigureAwait(false);

            // Stop before another expensive call if the run has blown its spend ceiling.
            if (_options.Budget is not null && cost.ExceedsBudget(_options.Budget, out budgetHit))
            {
                break;
            }

            var calls = ExtractToolCalls(response.Messages);
            if (calls.Count == 0 || iterations >= _options.MaxIterations)
            {
                break;
            }

            var toolMessage = await ExecuteToolsAsync(calls, cancellationToken).ConfigureAwait(false);
            working.Add(toolMessage);
            newThisTurn.Add(toolMessage);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterToolCall, cancellationToken).ConfigureAwait(false);
        }

        thread?.AddRange(newThisTurn);
        return new AgentRunResponse(newThisTurn, usage, iterations, cost, budgetHit);
    }

    private ModelPricing? LookupPricing(string? modelId) =>
        modelId is not null && _options.Pricing.TryGetValue(modelId, out var pricing) ? pricing : null;

    /// <summary>
    /// Invoke the model with bounded retry-with-exponential-backoff. On a transient error
    /// (anything the client throws — rate-limit, 5xx, dropped connection) the call is retried up to
    /// <see cref="AgentOptions.MaxRetries"/> additional times, waiting
    /// <c>RetryBackoff * 2^(n-1)</c> before the n-th (1-indexed) retry. If all attempts fail the
    /// LAST error propagates, so the turn fails exactly as it did before retries existed. Only this
    /// model call is retried — tool execution is not. (Retry is scoped to the non-streaming
    /// <see cref="RunAsync(string, SmoothAgentThread?, CancellationToken)"/>; streaming connect
    /// retry is out of scope — see the note in <see cref="RunStreamingAsync(string, SmoothAgentThread?, CancellationToken)"/>.)
    /// </summary>
    private async Task<ChatResponse> CallModelAsync(IList<ChatMessage> working, ChatOptions? chatOptions, CancellationToken cancellationToken)
    {
        var attempt = 0;
        while (true)
        {
            try
            {
                return await _chatClient.GetResponseAsync(working, chatOptions, cancellationToken).ConfigureAwait(false);
            }
            catch when (attempt < _options.MaxRetries)
            {
                attempt++;
                var delay = _options.RetryBackoff * Math.Pow(2, attempt - 1);
                if (delay > TimeSpan.Zero)
                {
                    await Task.Delay(delay, cancellationToken).ConfigureAwait(false);
                }
            }
        }
    }

    /// <summary>Stream a single stateless turn.</summary>
    public IAsyncEnumerable<ChatResponseUpdate> RunStreamingAsync(string message, CancellationToken cancellationToken = default) =>
        RunStreamingAsync(message, null, cancellationToken);

    /// <summary>
    /// Stream a turn within <paramref name="thread"/> (or stateless if null), yielding the
    /// model's <see cref="ChatResponseUpdate"/>s across every loop iteration. New messages are
    /// appended back to the thread when the turn completes. (MAF naming: <c>RunStreamingAsync</c>.)
    /// </summary>
    public async IAsyncEnumerable<ChatResponseUpdate> RunStreamingAsync(
        string message,
        SmoothAgentThread? thread,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        var working = await SeedConversationAsync(message, thread, cancellationToken).ConfigureAwait(false);
        var newThisTurn = new List<ChatMessage> { working[^1] };
        var iterations = 0;

        while (true)
        {
            iterations++;
            Compactor.Compact(working, _options.Compaction, _options.MaxContextTokens);

            var chatOptions = BuildChatOptions();
            // NOTE: retry-with-backoff (AgentOptions.MaxRetries/RetryBackoff) is intentionally NOT
            // applied here. Streaming yields updates to the consumer as they arrive, so re-running
            // the call after a mid-stream failure would re-emit already-yielded chunks. Retry is
            // scoped to the non-streaming RunAsync (see CallModelAsync); streaming connect retry can
            // be layered on later by retrying just the enumerator's first MoveNextAsync.
            var updates = new List<ChatResponseUpdate>();
            await foreach (var update in _chatClient.GetStreamingResponseAsync(working, chatOptions, cancellationToken).ConfigureAwait(false))
            {
                updates.Add(update);
                yield return update;
            }

            var response = updates.ToChatResponse();
            working.AddRange(response.Messages);
            newThisTurn.AddRange(response.Messages);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterEachIteration, cancellationToken).ConfigureAwait(false);

            var calls = ExtractToolCalls(response.Messages);
            if (calls.Count == 0 || iterations >= _options.MaxIterations)
            {
                break;
            }

            var toolMessage = await ExecuteToolsAsync(calls, cancellationToken).ConfigureAwait(false);
            working.Add(toolMessage);
            newThisTurn.Add(toolMessage);
            // Surface the tool results into the stream (mirrors the Rust engine emitting a
            // ToolCallComplete event) so consumers can render tool-result chunks. The model's
            // tool-call request already flowed through the raw updates above.
            yield return new ChatResponseUpdate(toolMessage.Role, toolMessage.Contents);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterToolCall, cancellationToken).ConfigureAwait(false);
        }

        thread?.AddRange(newThisTurn);
    }

    /// <summary>
    /// Reconstruct a thread from its latest checkpoint (or a fresh one with that id if there's
    /// no checkpoint / no store). The C# analog of the Rust engine's <c>resume_or_new</c>.
    /// </summary>
    public async Task<SmoothAgentThread> ResumeThreadAsync(string threadId, CancellationToken cancellationToken = default)
    {
        var thread = new SmoothAgentThread(threadId);
        if (_options.CheckpointStore is not null)
        {
            var checkpoint = await _options.CheckpointStore.LoadLatestAsync(threadId, cancellationToken).ConfigureAwait(false);
            if (checkpoint is not null)
            {
                thread.AddRange(checkpoint.Messages);
            }
        }
        return thread;
    }

    private async Task MaybeCheckpointAsync(
        SmoothAgentThread? thread,
        IReadOnlyList<ChatMessage> newThisTurn,
        int iteration,
        CheckpointStrategy trigger,
        CancellationToken cancellationToken)
    {
        if (thread is null || _options.CheckpointStore is null || _options.Checkpoint != trigger)
        {
            return;
        }

        // Snapshot the durable conversation up to this point (prior thread history + this turn
        // so far). A copy, so later compaction of the working list can't corrupt it.
        var snapshot = thread.Messages.Concat(newThisTurn).ToList();
        var checkpoint = new Checkpoint(Guid.NewGuid().ToString("n"), thread.Id, snapshot, iteration, DateTimeOffset.UtcNow);
        await _options.CheckpointStore.SaveAsync(checkpoint, cancellationToken).ConfigureAwait(false);
    }

    private async Task<List<ChatMessage>> SeedConversationAsync(string userMessage, SmoothAgentThread? thread, CancellationToken cancellationToken)
    {
        var messages = new List<ChatMessage>();
        if (!string.IsNullOrEmpty(_options.Instructions))
        {
            messages.Add(new ChatMessage(ChatRole.System, _options.Instructions));
        }
        if (thread is not null)
        {
            messages.AddRange(thread.Messages);
        }

        // Retrieve knowledge + memory for this turn and inject it as grounding context,
        // placed right before the live user message. Ephemeral — regenerated each turn,
        // never persisted into the thread.
        var context = await BuildRetrievedContextAsync(userMessage, cancellationToken).ConfigureAwait(false);
        if (context is not null)
        {
            messages.Add(context);
        }

        messages.Add(new ChatMessage(ChatRole.User, userMessage));
        return messages;
    }

    private async Task<ChatMessage?> BuildRetrievedContextAsync(string query, CancellationToken cancellationToken)
    {
        if (_options.Knowledge is null && _options.Memory is null)
        {
            return null;
        }

        var builder = new StringBuilder();

        if (_options.Knowledge is not null)
        {
            var hits = await _options.Knowledge.QueryAsync(query, _options.KnowledgeTopK, cancellationToken).ConfigureAwait(false);
            if (hits.Count > 0)
            {
                builder.AppendLine("Relevant knowledge (ground your answer in this; cite the source):");
                foreach (var hit in hits)
                {
                    builder.AppendLine($"- [{hit.Source}] {hit.Chunk}");
                }
            }
        }

        if (_options.Memory is not null)
        {
            var memories = await _options.Memory.RecallAsync(query, _options.MemoryTopK, cancellationToken).ConfigureAwait(false);
            if (memories.Count > 0)
            {
                if (builder.Length > 0)
                {
                    builder.AppendLine();
                }
                builder.AppendLine("Relevant memory:");
                foreach (var memory in memories)
                {
                    builder.AppendLine($"- {memory.Content}");
                }
            }
        }

        return builder.Length > 0 ? new ChatMessage(ChatRole.System, builder.ToString()) : null;
    }

    private ChatOptions? BuildChatOptions()
    {
        var tools = new List<AITool>(_options.Tools);
        if (_toolSearch is not null)
        {
            // Advertise the tool_search meta-tool plus any deferred tools promoted so far. The
            // unpromoted deferred tools stay hidden — their schemas never reach the model.
            tools.Add(_toolSearch.MetaTool);
            tools.AddRange(_toolSearch.PromotedTools());
        }
        return tools.Count > 0 ? new ChatOptions { Tools = tools } : null;
    }

    private static List<FunctionCallContent> ExtractToolCalls(IEnumerable<ChatMessage> messages) =>
        messages.SelectMany(m => m.Contents).OfType<FunctionCallContent>().ToList();

    /// <summary>
    /// Resolve a tool call to its function: the <c>tool_search</c> meta-tool, a regular tool, or a
    /// <i>promoted</i> deferred tool. Unpromoted deferred tools resolve to null (unknown tool).
    /// </summary>
    private AIFunction? ResolveTool(string name)
    {
        if (_toolSearch is not null && name == ToolSearch.ToolName)
        {
            return _toolSearch.MetaTool;
        }
        if (_functions.TryGetValue(name, out var function))
        {
            return function;
        }
        return _toolSearch?.ResolvePromoted(name);
    }

    private async Task<ChatMessage> ExecuteToolsAsync(IReadOnlyList<FunctionCallContent> calls, CancellationToken cancellationToken)
    {
        // Dispatch the tool calls — concurrently when enabled and there's more than one — but
        // always assemble the results in the original call order so the transcript stays
        // deterministic. InvokeToolAsync turns failures/denials into a result content, so a
        // single tool's failure can't cancel its siblings under Task.WhenAll.
        List<AIContent> results;
        if (_options.ParallelToolCalls && calls.Count > 1)
        {
            var tasks = calls.Select(call => InvokeToolAsync(call, cancellationToken)).ToList();
            var completed = await Task.WhenAll(tasks).ConfigureAwait(false);
            results = completed.Cast<AIContent>().ToList();
        }
        else
        {
            results = new List<AIContent>(calls.Count);
            foreach (var call in calls)
            {
                results.Add(await InvokeToolAsync(call, cancellationToken).ConfigureAwait(false));
            }
        }
        return new ChatMessage(ChatRole.Tool, results);
    }

    private async Task<FunctionResultContent> InvokeToolAsync(FunctionCallContent call, CancellationToken cancellationToken)
    {
        var function = ResolveTool(call.Name);
        if (function is null)
        {
            return new FunctionResultContent(call.CallId, $"Error: unknown tool '{call.Name}'");
        }

        // Human-in-the-loop: pause for approval before running a flagged (write/sensitive) tool.
        // A denial is fed back to the model as a result — the tool never runs.
        if (_options.HumanGate is not null && (_options.RequiresApproval?.Invoke(call) ?? false))
        {
            var request = new HumanApprovalRequest(call.Name, call.Arguments, $"Approve calling tool '{call.Name}'?");
            var decision = await _options.HumanGate.RequestApprovalAsync(request, cancellationToken).ConfigureAwait(false);
            if (!decision.IsApproved)
            {
                return new FunctionResultContent(call.CallId, $"Denied by human: {decision.Reason ?? "no reason given"}");
            }
        }

        try
        {
            var arguments = new AIFunctionArguments(call.Arguments);
            var result = await function.InvokeAsync(arguments, cancellationToken).ConfigureAwait(false);
            return new FunctionResultContent(call.CallId, result);
        }
        catch (Exception ex)
        {
            // A failing tool is fed back to the model as an error result, not thrown —
            // the model can recover or apologize. Mirrors the Rust ToolResult.is_error path.
            return new FunctionResultContent(call.CallId, $"Error: {ex.Message}");
        }
    }

    private static void Accumulate(UsageDetails total, UsageDetails? add)
    {
        if (add is null)
        {
            return;
        }
        total.InputTokenCount = (total.InputTokenCount ?? 0) + (add.InputTokenCount ?? 0);
        total.OutputTokenCount = (total.OutputTokenCount ?? 0) + (add.OutputTokenCount ?? 0);
        total.TotalTokenCount = (total.TotalTokenCount ?? 0) + (add.TotalTokenCount ?? 0);
    }
}
