using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Configuration for a <see cref="SmoothAgent"/> run. Mirrors the Rust engine's
/// <c>AgentConfig</c>, expressed in MEAI idioms. Later phases add memory, knowledge,
/// checkpointing, cast, HITL, and cost budgets.
/// </summary>
public sealed class AgentOptions
{
    /// <summary>Display name for the agent (used in events/tracing).</summary>
    public string Name { get; set; } = "agent";

    /// <summary>
    /// System prompt prepended to the conversation. (MAF calls this "instructions".)
    /// </summary>
    public string? Instructions { get; set; }

    /// <summary>
    /// Hard cap on agentic loop iterations (LLM calls). Stops a model that keeps
    /// requesting tools from looping forever. Mirrors the Rust engine's
    /// <c>max_iterations</c>.
    /// </summary>
    public int MaxIterations { get; set; } = 8;

    /// <summary>
    /// Budget cap on the tokens the model may EMIT per turn (the request's <c>max_tokens</c>).
    /// <c>null</c> (the default) leaves it unset — the provider's own default applies, behavior
    /// unchanged. Mirrors the Rust engine's <c>LlmConfig.max_tokens</c>. Always clamped down to
    /// <see cref="ModelMaxOutputTokens"/> via <see cref="EffectiveMaxTokens"/> before it is sent.
    /// </summary>
    public int? MaxOutputTokens { get; set; }

    /// <summary>
    /// The active model's HARD output ceiling (<c>max_output_tokens</c>) when known — e.g. sourced
    /// from the LiteLLM gateway's <c>/model/info</c>. When set (&gt; 0), <see cref="MaxOutputTokens"/>
    /// is clamped to it so a budget can never exceed what the model can physically emit — otherwise a
    /// reasoning model burns its budget on reasoning and returns EMPTY, or the upstream 400s (e.g.
    /// <c>groq-compound</c> caps output at 8192 under a 32768 budget). <c>null</c> = unknown ⇒ no
    /// clamp (graceful passthrough). Mirrors the Rust engine's <c>model_max_output</c>. EPIC th-1cc9fa.
    /// </summary>
    public int? ModelMaxOutputTokens { get; set; }

    /// <summary>
    /// The <c>max_tokens</c> to actually send: <see cref="MaxOutputTokens"/> clamped down to
    /// <see cref="ModelMaxOutputTokens"/> when a ceiling is known. <c>null</c> when no budget is set
    /// (leave the request's <c>max_tokens</c> unset — passthrough). Never returns 0. Mirrors the Rust
    /// engine's <c>LlmClient.effective_max_tokens()</c>.
    /// </summary>
    public int? EffectiveMaxTokens =>
        MaxOutputTokens is not { } budget ? null
        : ModelMaxOutputTokens is { } ceiling && ceiling > 0 ? Math.Max(1, Math.Min(budget, ceiling))
        : budget;

    /// <summary>
    /// Tools available to the agent. Author them from ordinary C# methods with
    /// <c>AIFunctionFactory.Create(...)</c> — exactly as a Microsoft Agent Framework
    /// / Semantic Kernel dev already does.
    /// </summary>
    public IList<AITool> Tools { get; } = new List<AITool>();

    /// <summary>
    /// When true and an assistant turn returns ≥2 tool calls, dispatch them concurrently
    /// (<see cref="Task.WhenAll(IEnumerable{Task})"/>) instead of sequentially. The tool-result
    /// contents are still assembled in the original tool-call order, so the transcript stays
    /// deterministic regardless of completion order. Default false preserves the sequential
    /// behaviour. Per-tool semantics (clearance, human-gate approval, tool_search promotion,
    /// argument binding, error handling) are unchanged — only the dispatch loop runs in parallel.
    /// </summary>
    public bool ParallelToolCalls { get; set; }

    /// <summary>
    /// Tool-call hooks — the in-process surveillance / redaction seam. Every hook's
    /// <see cref="IToolHook.PreCallAsync"/> runs before a tool executes (a throw blocks the call) and
    /// its <see cref="IToolHook.PostCallAsync"/> runs after with the mutable result (a hook can
    /// redact it in place). Hooks fire in registration order. Mirrors the Rust reference's
    /// <c>ToolRegistry</c> hook chain (<c>add_hook</c>). Default: none.
    /// </summary>
    public IList<IToolHook> ToolHooks { get; } = new List<IToolHook>();

    /// <summary>
    /// Tools whose schemas are hidden from the model until promoted. When non-empty, the agent
    /// advertises a single <c>tool_search(query)</c> meta-tool; the model calls it to discover and
    /// promote the deferred tools it needs, keeping the visible tool set (and its token cost) small.
    /// A deferred tool isn't dispatchable until <c>tool_search</c> promotes it. Mirrors the Rust
    /// reference's deferred-tools / <c>tool_search</c> behaviour.
    /// </summary>
    public IList<AITool> DeferredTools { get; } = new List<AITool>();

    /// <summary>
    /// Soft ceiling (estimated tokens) on the conversation sent to the model. When exceeded,
    /// the <see cref="Compaction"/> strategy trims older messages before the next LLM call.
    /// Mirrors the Rust engine's <c>max_context_tokens</c>.
    /// </summary>
    public int MaxContextTokens { get; set; } = 8000;

    /// <summary>How to shrink the conversation when it exceeds <see cref="MaxContextTokens"/>.</summary>
    public CompactionStrategy Compaction { get; set; } = CompactionStrategy.SlidingWindow;

    /// <summary>
    /// Optional knowledge store. When set, the agent retrieves the top
    /// <see cref="KnowledgeTopK"/> hits for the user's message and injects them as grounding
    /// context before answering (RAG). Mirrors the Rust engine's <c>knowledge</c>.
    /// </summary>
    public IKnowledgeBase? Knowledge { get; set; }

    /// <summary>How many knowledge hits to inject per turn.</summary>
    public int KnowledgeTopK { get; set; } = 4;

    /// <summary>
    /// Optional long-/short-term memory. When set, the agent recalls the top
    /// <see cref="MemoryTopK"/> relevant memories for the user's message and injects them as
    /// context. Mirrors the Rust engine's <c>memory</c>.
    /// </summary>
    public IAgentMemory? Memory { get; set; }

    /// <summary>How many recalled memories to inject per turn.</summary>
    public int MemoryTopK { get; set; } = 4;

    /// <summary>
    /// Optional checkpoint store. When set (and a thread is in use), the agent snapshots the
    /// conversation during a run per <see cref="Checkpoint"/> so it can be resumed after a
    /// crash. Mirrors the Rust engine's <c>checkpoint_store</c>.
    /// </summary>
    public ICheckpointStore? CheckpointStore { get; set; }

    /// <summary>When to write checkpoints during a run.</summary>
    public CheckpointStrategy Checkpoint { get; set; } = CheckpointStrategy.AfterToolCall;

    /// <summary>
    /// Optional human-in-the-loop gate. When set, the agent asks it for approval before running
    /// any tool call for which <see cref="RequiresApproval"/> returns true. A denied call is not
    /// executed; the model is told it was denied and can adapt. Mirrors the Rust engine's
    /// confirmation hook.
    /// </summary>
    public IHumanGate? HumanGate { get; set; }

    /// <summary>
    /// Which tool calls need human approval (e.g. writes / destructive actions). Default: none.
    /// Example: <c>o.RequiresApproval = call =&gt; call.Name is "delete_record" or "send_email";</c>
    /// Only consulted when <see cref="HumanGate"/> is set.
    /// </summary>
    public Func<FunctionCallContent, bool>? RequiresApproval { get; set; }

    /// <summary>
    /// Optional spend ceiling. When set, the run halts (gracefully, returning what it has) as soon
    /// as accumulated cost/tokens exceed it. Mirrors the Rust engine's <c>budget</c>.
    /// </summary>
    public CostBudget? Budget { get; set; }

    /// <summary>
    /// Per-model USD pricing, keyed by model id (as reported on the response's <c>ModelId</c>),
    /// used to compute the dollar cost in <see cref="AgentRunResponse.Cost"/>. Token accounting
    /// works without it; only USD requires pricing.
    /// </summary>
    public IDictionary<string, ModelPricing> Pricing { get; } = new Dictionary<string, ModelPricing>(StringComparer.Ordinal);

    /// <summary>
    /// Number of ADDITIONAL attempts after the first if the model call throws a transient error
    /// (rate-limit, 5xx, dropped connection). <c>0</c> (the default) preserves today's behaviour:
    /// a single attempt, the error propagates immediately. Only the model call is retried — never
    /// tool execution.
    /// </summary>
    public int MaxRetries { get; set; }

    /// <summary>
    /// Base delay for exponential backoff between retries. The wait before retry attempt <c>n</c>
    /// (1-indexed) is <c>RetryBackoff * 2^(n-1)</c>. Defaults to 200ms; set to
    /// <see cref="TimeSpan.Zero"/> to retry without sleeping (used by tests).
    /// </summary>
    public TimeSpan RetryBackoff { get; set; } = TimeSpan.FromMilliseconds(200);
}
