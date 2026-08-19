using System.Runtime.CompilerServices;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;
using Temporalio.Client;

namespace SmooAI.SmoothOperator.Temporal;

/// <summary>
/// A durable <see cref="IAgentExecutor"/> that runs an agent turn as an <see cref="AgentTurnWorkflow"/>
/// on Temporal instead of inline in the calling task. This is the optional durable backend the
/// engine's executor seam (ADR-030) exists to plug in: a consumer swaps
/// <see cref="InProcessExecutor"/> for this one and the turn becomes crash-safe, with durable
/// human-in-the-loop and durable timers — the engine and its callers unaware.
///
/// <para>The turn's model call and tool invocations run as Temporal <b>activities</b> against the
/// <see cref="EngineHandles"/> the <i>worker</i> was started with (not this client-side object): this
/// executor only <i>starts</i> the workflow and maps its result. A worker running
/// <see cref="AgentTurnWorkflow"/> + <see cref="AgentTurnActivities"/> on <see cref="TaskQueue"/> must
/// be up for the turn to make progress.</para>
///
/// <para>This maps a <see cref="SmoothAgent"/> to an <see cref="AgentTurnInput"/> from the agent's
/// public config (instructions, eager tool schemas, iteration cap) plus the passed thread's history,
/// and appends the turn's new messages back onto that thread — so a durable turn has the same
/// cross-turn memory an inline one does. Permission mode, deny policy, human gate and tool hooks are
/// deliberately <i>not</i> carried on the wire: they are enforced where the tool actually runs, by the
/// agent the <b>worker's</b> <see cref="EngineHandles"/> holds.</para>
///
/// <para>ponytail: <see cref="AgentOptions.Budget"/> is still not enforced on the durable path —
/// <see cref="AgentTurn.DriveTurnAsync"/> has no spend concept in any language, and neither does the
/// Rust reference's <c>drive_turn</c>. Wire it when the durable path carries usage back at all.</para>
/// </summary>
public sealed class TemporalExecutor : IAgentExecutor
{
    /// <summary>Default task queue an <see cref="AgentTurnWorkflow"/> worker polls.</summary>
    public const string DefaultTaskQueue = "smooth-operator-agent-turn";

    private readonly ITemporalClient _client;

    /// <summary>The task queue workflows are started on (a worker must poll it).</summary>
    public string TaskQueue { get; }

    /// <summary>Names of tools that require human approval before they run in the durable turn — the
    /// workflow blocks on an <c>ApproveTool</c>/<c>DenyTool</c> signal for these.</summary>
    public IReadOnlyList<string> ApprovalRequiredTools { get; }

    /// <summary>Name of the built-in durable-wait tool, if any (a model call to it sleeps the workflow
    /// on a Temporal timer). <c>null</c> disables it.</summary>
    public string? WaitTool { get; }

    public TemporalExecutor(
        ITemporalClient client,
        string taskQueue = DefaultTaskQueue,
        IReadOnlyList<string>? approvalRequiredTools = null,
        string? waitTool = null)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
        TaskQueue = taskQueue;
        ApprovalRequiredTools = approvalRequiredTools ?? Array.Empty<string>();
        WaitTool = waitTool;
    }

    /// <inheritdoc />
    public async Task<AgentRunResponse> ExecuteAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        CancellationToken cancellationToken = default)
    {
        var input = BuildInput(agent, message, thread);
        var handle = await _client.StartWorkflowAsync(
            (AgentTurnWorkflow wf) => wf.RunAsync(input),
            new WorkflowOptions(id: $"agent-turn-{Guid.NewGuid():N}", taskQueue: TaskQueue)).ConfigureAwait(false);
        var conversation = await handle.GetResultAsync().ConfigureAwait(false);

        // Everything past the carried prefix (system prompt + replayed history) is new this turn,
        // starting with the live user message — the same slice RunAsync appends inline, so a durable
        // turn leaves the thread in the state an in-process turn would have.
        var newThisTurn = conversation.Messages
            .Skip(input.CarriedCount)
            .Select(m => m.ToChatMessage())
            .ToList();
        thread?.AddRange(newThisTurn);
        return ToRunResponse(newThisTurn);
    }

    /// <inheritdoc />
    public async IAsyncEnumerable<ChatResponseUpdate> ExecuteStreamingAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        // ponytail: a Temporal workflow has no token-delta channel, so there is nothing to stream
        // incrementally — the durable turn completes, then its final answer is surfaced as one update.
        // The moment the durable path grows a streaming activity, this yields the deltas instead.
        var response = await ExecuteAsync(agent, message, thread, cancellationToken).ConfigureAwait(false);
        yield return new ChatResponseUpdate(ChatRole.Assistant, response.Text);
    }

    private AgentTurnInput BuildInput(SmoothAgent agent, string message, SmoothAgentThread? thread)
    {
        var tools = agent.Tools.Select(ToolSchemaDto.From).ToList();
        return new AgentTurnInput(
            SystemPrompt: agent.Instructions ?? string.Empty,
            UserMessage: message,
            Tools: tools,
            MaxIterations: agent.MaxIterations,
            ApprovalRequiredTools: ApprovalRequiredTools,
            WaitTool: WaitTool,
            History: thread?.Messages.Select(ChatMessageDto.From).ToList());
    }

    private static AgentRunResponse ToRunResponse(IReadOnlyList<ChatMessage> messages)
    {
        // Iterations ≈ assistant turns; token/cost accounting is per-activity in workflow history and
        // is not surfaced on the workflow result today (usage carriage is a later phase).
        var iterations = messages.Count(m => m.Role == ChatRole.Assistant);
        return new AgentRunResponse(messages, new UsageDetails(), iterations, new CostTracker());
    }
}
