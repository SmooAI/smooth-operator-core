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
/// <para>ponytail: this maps a <see cref="SmoothAgent"/> to an <see cref="AgentTurnInput"/> from the
/// agent's public config (instructions, eager tool schemas, iteration cap). Carrying prior-thread
/// history and a per-turn tool registry into the workflow is the documented open design gap
/// (ADR-030 / runner.rs TODO), deferred until the durable path needs multi-turn memory — not
/// invented here.</para>
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
        var input = BuildInput(agent, message);
        var handle = await _client.StartWorkflowAsync(
            (AgentTurnWorkflow wf) => wf.RunAsync(input),
            new WorkflowOptions(id: $"agent-turn-{Guid.NewGuid():N}", taskQueue: TaskQueue)).ConfigureAwait(false);
        var conversation = await handle.GetResultAsync().ConfigureAwait(false);
        return ToRunResponse(conversation);
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

    private AgentTurnInput BuildInput(SmoothAgent agent, string message)
    {
        var tools = agent.Tools.Select(ToolSchemaDto.From).ToList();
        return new AgentTurnInput(
            SystemPrompt: agent.Instructions ?? string.Empty,
            UserMessage: message,
            Tools: tools,
            MaxIterations: agent.MaxIterations,
            ApprovalRequiredTools: ApprovalRequiredTools,
            WaitTool: WaitTool);
    }

    private static AgentRunResponse ToRunResponse(TurnConversation conversation)
    {
        var messages = conversation.Messages
            .Select(m => new ChatMessage(new ChatRole(m.Role), m.Content) { AuthorName = m.AuthorName })
            .ToList();
        // Iterations ≈ assistant turns; token/cost accounting is per-activity in workflow history and
        // is not surfaced on the workflow result today (usage carriage is a later phase).
        var iterations = messages.Count(m => m.Role == ChatRole.Assistant);
        return new AgentRunResponse(messages, new UsageDetails(), iterations, new CostTracker());
    }
}
