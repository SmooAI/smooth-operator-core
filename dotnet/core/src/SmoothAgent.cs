using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core.Extensions;

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
    private readonly PermissionHook? _permissionHook;
    private readonly IReadOnlyList<AITool> _eagerTools;

    /// <summary>
    /// The completed turn from the most recent <see cref="RunStreamingAsync(string, SmoothAgentThread?, CancellationToken)"/>,
    /// carrying the usage, cost and budget outcome that the <c>IAsyncEnumerable&lt;ChatResponseUpdate&gt;</c>
    /// return type has nowhere to hang. This is C#'s stand-in for the terminal <c>done</c> event the
    /// sibling engines emit (Rust <c>AgentEvent::Completed</c>, Go/Python/TypeScript <c>done</c>),
    /// which carry <c>cost_usd</c> and the token totals on the stream itself.
    ///
    /// <para><b>Null until the stream is fully enumerated.</b> It is set at the end of the iterator
    /// body, so a consumer that breaks out early never sees it — and it is reset to null when a new
    /// streaming turn begins, so a stale total can't be mistaken for a fresh one.
    /// <see cref="RunAsync(string, SmoothAgentThread?, CancellationToken)"/> returns its
    /// <see cref="AgentRunResponse"/> directly and does not touch this.</para>
    ///
    /// <para>ponytail: one slot, not a per-run handle, because a <see cref="SmoothAgent"/> already
    /// cannot serve two turns at once (tool_search promotions mutate the visible tool set across
    /// iterations). If concurrent turns on one agent ever become supported, this becomes a
    /// completion callback or a per-run handle at the same time — not before.</para>
    /// </summary>
    public AgentRunResponse? LastRunResponse { get; private set; }

    public SmoothAgent(IChatClient chatClient, AgentOptions options)
    {
        _chatClient = chatClient ?? throw new ArgumentNullException(nameof(chatClient));
        _options = options ?? throw new ArgumentNullException(nameof(options));

        // SEP: an extension host's EAGER tools join the ordinary tool set — visible to the model,
        // dispatched, and permission-gated exactly like natives, no special casing. Its DEFERRED
        // tools join the tool_search pool and stay invisible until promoted.
        //
        // Merged into locals, never back onto `options`: unlike Go (where AgentOptions is a value
        // copy) AgentOptions is a shared object here, so appending to it would leak into the caller
        // and double-add on a second SmoothAgent built from the same options.
        _eagerTools = options.Extensions is null
            ? (IReadOnlyList<AITool>)options.Tools.ToList()
            : options.Tools.Concat(options.Extensions.Tools()).ToList();
        var deferred = options.Extensions is null
            ? (IReadOnlyList<AITool>)options.DeferredTools.ToList()
            : options.DeferredTools.Concat(options.Extensions.DeferredTools()).ToList();

        _functions = _eagerTools.OfType<AIFunction>().ToDictionary(f => f.Name, StringComparer.Ordinal);
        _toolSearch = deferred.Count > 0 ? new ToolSearch(deferred.OfType<AIFunction>()) : null;
        // The permission gate is opt-in: it engages only when a mode or a deny policy is configured.
        // Attaching just a deny policy still runs the full engine (defaulting to Ask). This keeps every
        // existing agent byte-identical to before. An Ask routes to the same IHumanGate seam.
        _permissionHook = options.PermissionMode is not null || options.DenyPolicy is not null
            ? new PermissionHook(options.PermissionMode ?? AutoMode.Ask, options.HumanGate, options.DenyPolicy, options.PermissionGrants, options.PermissionGrantsPersistPath)
            : null;
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
        var lastText = string.Empty;

        SepDispatch(SepTurnStart, new JsonObject { ["agent_id"] = SepAgentId });

        while (true)
        {
            iterations++;
            Compactor.Compact(working, _options.Compaction, _options.MaxContextTokens);

            // Recompute the visible tool set each iteration: tool_search promotions during the
            // previous iteration widen what the model can see/call now.
            var chatOptions = BuildChatOptions();
            var response = await CallModelAsync(working, chatOptions, cancellationToken).ConfigureAwait(false);
            Accumulate(usage, response.Usage);
            lastText = response.Text ?? string.Empty;
            cost.RecordWithGatewayCost(response.ModelId, response.Usage, ResponseGatewayCost(response), LookupPricing(response.ModelId));
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

        // Every exit above (budget blown, no more tool calls, iteration cap) breaks to here, so one
        // call covers them all — the events fire on EVERY termination, not just the happy path.
        SepTurnComplete(iterations, lastText);

        thread?.AddRange(newThisTurn);
        return new AgentRunResponse(newThisTurn, usage, iterations, cost, budgetHit);
    }


    /// <summary>
    /// The gateway's authoritative cost for a response, when the client surfaced one.
    /// <para>The engine takes an injected <see cref="IChatClient"/>, whose parsed
    /// response carries no HTTP headers — the cost lives ONLY in a response header. So
    /// this reads the documented seam: a <c>gatewayCostUsd</c> entry on the response's
    /// <see cref="ChatResponse.AdditionalProperties"/>, which a client wrapping the raw
    /// HTTP call attaches via <see cref="GatewayCost.Parse(System.Net.Http.Headers.HttpHeaders)"/>.
    /// Absent it, null — unmeasured, and the local pricing estimate is used instead of
    /// a bogus $0.</para>
    /// </summary>
    private static decimal? ResponseGatewayCost(ChatResponse response)
    {
        if (response.AdditionalProperties is not { } props)
        {
            return null;
        }
        if (!props.TryGetValue("gatewayCostUsd", out var raw) || raw is null)
        {
            return null;
        }
        return raw switch
        {
            decimal d when d > 0m => d,
            double dbl when dbl > 0 => (decimal)dbl,
            string str => GatewayCost.Parse(name => name == "x-cost-usd" ? str : null),
            _ => null,
        };
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
        var usage = new UsageDetails();
        var cost = new CostTracker();
        BudgetExceeded? budgetHit = null;
        var iterations = 0;
        var lastText = string.Empty;

        LastRunResponse = null;
        SepDispatch(SepTurnStart, new JsonObject { ["agent_id"] = SepAgentId });

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
            // The folded response carries this iteration's Usage and ModelId — the same two inputs
            // RunAsync records. Dropping them here is what left streaming turns with no usage, no
            // cost, and (worse) a silently inert AgentOptions.Budget. Pearl th-df859c.
            Accumulate(usage, response.Usage);
            cost.RecordWithGatewayCost(response.ModelId, response.Usage, ResponseGatewayCost(response), LookupPricing(response.ModelId));
            lastText = response.Text ?? string.Empty;
            working.AddRange(response.Messages);
            newThisTurn.AddRange(response.Messages);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterEachIteration, cancellationToken).ConfigureAwait(false);

            // Stop before another expensive call if the run has blown its spend ceiling — the
            // streaming counterpart of the same guard in RunAsync.
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
            // Surface the tool results into the stream (mirrors the Rust engine emitting a
            // ToolCallComplete event) so consumers can render tool-result chunks. The model's
            // tool-call request already flowed through the raw updates above.
            yield return new ChatResponseUpdate(toolMessage.Role, toolMessage.Contents);
            await MaybeCheckpointAsync(thread, newThisTurn, iterations, CheckpointStrategy.AfterToolCall, cancellationToken).ConfigureAwait(false);
        }

        // Same single-exit reasoning as RunAsync: both breaks land here, so the stream path emits
        // the identical event pair (stream parity).
        SepTurnComplete(iterations, lastText);

        thread?.AddRange(newThisTurn);
        LastRunResponse = new AgentRunResponse(newThisTurn, usage, iterations, cost, budgetHit);
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
        var tools = new List<AITool>(_eagerTools);
        if (_toolSearch is not null)
        {
            // Advertise the tool_search meta-tool plus any deferred tools promoted so far. The
            // unpromoted deferred tools stay hidden — their schemas never reach the model.
            tools.Add(_toolSearch.MetaTool);
            tools.AddRange(_toolSearch.PromotedTools());
        }

        // The clamped output budget (min(MaxOutputTokens, ModelMaxOutputTokens); null ⇒ leave
        // max_tokens unset). Keeps a budget from exceeding what the model can physically emit
        // (EPIC th-1cc9fa).
        var maxTokens = _options.EffectiveMaxTokens;
        var metadata = _options.Metadata is { Count: > 0 } m ? m : null;
        if (tools.Count == 0 && maxTokens is null && metadata is null)
        {
            return null;
        }

        var chatOptions = new ChatOptions();
        if (tools.Count > 0)
        {
            chatOptions.Tools = tools;
        }
        if (maxTokens is { } budget)
        {
            chatOptions.MaxOutputTokens = budget;
        }
        if (metadata is not null)
        {
            // Empty metadata never reaches here — unset stays byte-identical on the
            // wire (Rust parity: with_metadata filters empty maps to None).
            chatOptions.AdditionalProperties = new AdditionalPropertiesDictionary { ["metadata"] = metadata };
        }
        return chatOptions;
    }

    // ── SEP seam ────────────────────────────────────────────────────────────────
    // The C# siblings of go/core/extension_seam.go and Rust's sep_* helpers. Every
    // one is a no-op when no host is configured, so the no-extension path is
    // byte-identical to before extensions existed.

    private const string SepTurnStart = "turn_start";
    private const string SepTurnEnd = "turn_end";
    private const string SepMessageEnd = "message_end";

    /// <summary>Nil-safe event fan-out. Fire-and-forget: observe events are lossy by
    /// contract and never block the turn.</summary>
    private void SepDispatch(string @event, JsonNode payload) =>
        _options.Extensions?.DispatchEvent(@event, payload);

    /// <summary><c>agent_id</c> for the turn events. Go/Rust use their options' model id;
    /// C# keeps the model on the <see cref="IChatClient"/>, so the agent's own
    /// <see cref="AgentOptions.Name"/> is the identity here.</summary>
    private string SepAgentId => _options.Name;

    /// <summary>The pair of end-of-turn events, in the order Rust emits them:
    /// <c>message_end</c> carrying the final assistant text, then <c>turn_end</c>.</summary>
    private void SepTurnComplete(int iterations, string content)
    {
        if (_options.Extensions is null)
        {
            return;
        }
        SepDispatch(SepMessageEnd, new JsonObject { ["iteration"] = iterations, ["content"] = content });
        SepDispatch(SepTurnEnd, new JsonObject { ["agent_id"] = SepAgentId, ["iterations"] = iterations });
    }

    /// <summary>What the model sees in place of a vetoed call's result. Phrased so the model
    /// learns the call was refused and why, rather than seeing an empty result and retrying
    /// blindly. Wording is byte-identical across Rust/Go/TS/C#.</summary>
    private static string SepBlockedResult(string? reason) =>
        string.IsNullOrEmpty(reason) ? "error: blocked by extension" : $"error: blocked by extension: {reason}";

    /// <summary>
    /// Folds the <c>tool_call</c> hook over every pending call BEFORE any of them execute.
    /// Vetoed calls are returned as callId → reason (they never run); a proceed-with-patch
    /// rewrites the call's arguments in place.
    /// <para>
    /// Rewrites are already scoped by the host's cross-tool guard: an extension may only rewrite
    /// the arguments of a tool it owns, and may never redirect the call to a different tool.
    /// </para>
    /// </summary>
    private async Task<Dictionary<string, string>?> SepToolCallPlanAsync(IList<FunctionCallContent> calls)
    {
        if (_options.Extensions is null || calls.Count == 0)
        {
            return null;
        }

        Dictionary<string, string>? blocked = null;
        for (var i = 0; i < calls.Count; i++)
        {
            var call = calls[i];
            var args = JsonSerializer.SerializeToNode(call.Arguments ?? new Dictionary<string, object?>()) ?? new JsonObject();
            var folded = await _options.Extensions.RunToolCallHookAsync(call.Name, args).ConfigureAwait(false);
            switch (folded)
            {
                case FoldedHook.Blocked b:
                    blocked ??= new Dictionary<string, string>(StringComparer.Ordinal);
                    blocked[call.CallId] = b.Reason;
                    break;
                case FoldedHook.Proceed p when p.Value["arguments"] is JsonNode patched:
                    var rewritten = patched.Deserialize<Dictionary<string, object?>>();
                    if (rewritten is not null)
                    {
                        calls[i] = new FunctionCallContent(call.CallId, call.Name, rewritten);
                    }
                    break;
            }
        }
        return blocked;
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
        // SEP tool_call fold: every pending call runs the extension hook chain BEFORE any of them
        // execute. A veto means the tool never runs at all — the model sees the refusal in place of
        // a result. A proceed-with-patch rewrites the call's arguments in `planned`.
        var planned = calls.ToList();
        var sepBlocked = await SepToolCallPlanAsync(planned).ConfigureAwait(false);

        Task<FunctionResultContent> Dispatch(FunctionCallContent call) =>
            sepBlocked is not null && sepBlocked.TryGetValue(call.CallId, out var reason)
                ? Task.FromResult(new FunctionResultContent(call.CallId, SepBlockedResult(reason)))
                : InvokeToolAsync(call, cancellationToken);

        // Dispatch the tool calls — concurrently when enabled and there's more than one — but
        // always assemble the results in the original call order so the transcript stays
        // deterministic. InvokeToolAsync turns failures/denials into a result content, so a
        // single tool's failure can't cancel its siblings under Task.WhenAll.
        List<AIContent> results;
        if (_options.ParallelToolCalls && planned.Count > 1)
        {
            var tasks = planned.Select(Dispatch).ToList();
            var completed = await Task.WhenAll(tasks).ConfigureAwait(false);
            results = completed.Cast<AIContent>().ToList();
        }
        else
        {
            results = new List<AIContent>(planned.Count);
            foreach (var call in planned)
            {
                results.Add(await Dispatch(call).ConfigureAwait(false));
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

        // Permission gate (auto-mode + deny policy): classify every tool call before it runs. A deny
        // (built-in circuit-breaker or consumer deny policy) or a fail-closed Ask blocks the tool and
        // feeds the reason back to the model. An Ask that routes to the human is handled inside the
        // hook via the same IHumanGate seam. Runs before the legacy RequiresApproval gate below.
        if (_permissionHook is not null)
        {
            var blockReason = await _permissionHook.PreCallAsync(call, cancellationToken).ConfigureAwait(false);
            if (blockReason is not null)
            {
                return new FunctionResultContent(call.CallId, $"Error: {blockReason}");
            }
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

        // Pre-call hooks run before the tool. A hook that throws blocks the call — the tool never
        // runs and the model gets a "Blocked by hook" result. Parity with the Rust reference's
        // pre_call returning Err (registry short-circuits without executing the tool).
        try
        {
            foreach (var hook in _options.ToolHooks)
            {
                await hook.PreCallAsync(call, cancellationToken).ConfigureAwait(false);
            }
        }
        catch (Exception ex)
        {
            return new FunctionResultContent(call.CallId, $"Blocked by hook: {ex.Message}");
        }

        FunctionResultContent resultContent;
        try
        {
            var arguments = new AIFunctionArguments(call.Arguments);
            var result = await function.InvokeAsync(arguments, cancellationToken).ConfigureAwait(false);
            resultContent = new FunctionResultContent(call.CallId, result);
        }
        catch (Exception ex)
        {
            // A failing tool is fed back to the model as an error result, not thrown —
            // the model can recover or apologize. Mirrors the Rust ToolResult.is_error path.
            resultContent = new FunctionResultContent(call.CallId, $"Error: {ex.Message}");
        }

        // Post-call hooks run with the mutable result (redaction seam) on both success and error.
        // A hook failure is swallowed so the possibly-redacted result still reaches the caller —
        // parity with the Rust reference logging post_call errors rather than surfacing them.
        foreach (var hook in _options.ToolHooks)
        {
            try
            {
                await hook.PostCallAsync(call, resultContent, cancellationToken).ConfigureAwait(false);
            }
            catch
            {
                // ponytail: swallow post-hook failures — the (redacted) result still returns. Parity with Rust.
            }
        }

        return resultContent;
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
