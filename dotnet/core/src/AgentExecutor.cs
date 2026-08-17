using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// A backend that executes a <see cref="SmoothAgent"/> turn — the seam that decides
/// <i>where and how</i> a turn runs, while the agent remains the unit of orchestration
/// (instructions, tools, loop policy, compaction, checkpointing). The C# sibling of the
/// Rust reference's <c>AgentExecutor</c> trait
/// (<c>rust/smooth-operator-core/src/executor.rs</c>, ADR-030 / SMOODEV-1972).
///
/// <para>Today there is one implementation, <see cref="InProcessExecutor"/>, which drives the
/// turn directly in the calling task — identical behavior to calling
/// <see cref="SmoothAgent.RunAsync(string, SmoothAgentThread?, CancellationToken)"/> yourself.
/// It carries no dependencies and needs no infrastructure: the zero-infra, OSS default.</para>
///
/// <para>The point of the interface is to let an <i>optional</i> durable backend plug in without
/// the engine or its consumers knowing. Such a backend models the turn as a durable workflow and
/// runs the agent's side-effects (model calls, tool invocations, retrieval, persistence) as
/// activities, giving crash-safe resume, durable human-in-the-loop via signals, and durable
/// timers. The in-process executor runs those same side-effects inline; this boundary is what lets
/// the two be swapped at the edge.</para>
///
/// <para>Both methods take the agent as a <b>parameter</b> so an executor never owns it — it
/// borrows the agent for the duration of the turn, exactly like calling
/// <see cref="SmoothAgent.RunAsync(string, SmoothAgentThread?, CancellationToken)"/> directly.</para>
/// </summary>
// TODO(ADR-030): a Temporal-backed executor belongs in a SEPARATE NuGet package
// (e.g. SmooAI.SmoothOperator.Temporal), mirroring the Rust reference's separate
// `smooth-operator-temporal` crate behind an off-by-default `temporal` cargo feature. Keeping it
// out of this package is what keeps the published core zero-infra: no `Temporalio` dependency, no
// worker/namespace configuration, nothing to run. That package implements this interface plus
// <see cref="IAgentActivities"/> against a workflow context and calls
// <see cref="AgentTurn.DriveTurnAsync"/> unchanged.
public interface IAgentExecutor
{
    /// <summary>
    /// Run a single turn for <paramref name="message"/> and return the turn's result.
    /// Behaviorally equivalent to
    /// <see cref="SmoothAgent.RunAsync(string, SmoothAgentThread?, CancellationToken)"/> for the
    /// in-process backend. Any fatal error from the underlying turn (model call, tool dispatch,
    /// or — for durable backends — the workflow engine) propagates.
    /// </summary>
    Task<AgentRunResponse> ExecuteAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Run a single turn, yielding the model's <see cref="ChatResponseUpdate"/>s as they arrive.
    /// Behaviorally equivalent to
    /// <see cref="SmoothAgent.RunStreamingAsync(string, SmoothAgentThread?, CancellationToken)"/>
    /// for the in-process backend.
    /// </summary>
    IAsyncEnumerable<ChatResponseUpdate> ExecuteStreamingAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        CancellationToken cancellationToken = default);
}

/// <summary>
/// The default, zero-infra executor: it drives the turn inline in the calling task by delegating
/// straight to <see cref="SmoothAgent.RunAsync(string, SmoothAgentThread?, CancellationToken)"/> /
/// <see cref="SmoothAgent.RunStreamingAsync(string, SmoothAgentThread?, CancellationToken)"/>.
///
/// <para>This is a verbatim pass-through — introducing the seam changes no behavior. It is the
/// executor used unless a consumer explicitly opts into a durable backend.</para>
/// </summary>
public sealed class InProcessExecutor : IAgentExecutor
{
    /// <inheritdoc />
    public Task<AgentRunResponse> ExecuteAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        CancellationToken cancellationToken = default) =>
        agent.RunAsync(message, thread, cancellationToken);

    /// <inheritdoc />
    public IAsyncEnumerable<ChatResponseUpdate> ExecuteStreamingAsync(
        SmoothAgent agent,
        string message,
        SmoothAgentThread? thread = null,
        CancellationToken cancellationToken = default) =>
        agent.RunStreamingAsync(message, thread, cancellationToken);
}

/// <summary>
/// The <i>side-effecting</i> boundary of an agent turn: the model call and each tool invocation.
/// These are exactly the steps a durable backend runs as Temporal <b>activities</b> (retried,
/// memoized in workflow history); the in-process backend runs them inline. Mirrors the Rust
/// reference's <c>AgentActivities</c> trait (<c>rust/smooth-operator-core/src/activities.rs</c>,
/// ADR-030 / SMOODEV-1974).
///
/// <para>Inputs and outputs are plain, serializable values (message lists, a
/// <see cref="ChatResponse"/>, a <see cref="FunctionCallContent"/>, a
/// <see cref="FunctionResultContent"/>) rather than live engine objects, so an implementation may
/// marshal them across an activity boundary unchanged.</para>
/// </summary>
public interface IAgentActivities
{
    /// <summary>
    /// Invoke the model with the given context window and the tools available this turn.
    /// A fatal model-call error (network, upstream rejection) propagates; a durable backend turns
    /// transient failures into activity retries before they surface here.
    /// </summary>
    Task<ChatResponse> ModelCallAsync(
        IReadOnlyList<ChatMessage> messages,
        IReadOnlyList<AITool> tools,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Execute a single tool call and return its result.
    ///
    /// <para>Tool <b>business</b> failures — the tool threw, was denied, or does not exist — are
    /// reported <i>on the returned result</i> (an error string in
    /// <see cref="FunctionResultContent.Result"/>), never as a thrown exception, so the model can
    /// see the failure and recover. This mirrors the Rust reference's <c>ToolResult::is_error</c>
    /// and the engine's own <c>InvokeToolAsync</c>. Only a fatal <i>dispatch</i> error throws.</para>
    /// </summary>
    Task<FunctionResultContent> ToolInvokeAsync(FunctionCallContent call, CancellationToken cancellationToken = default);
}

/// <summary>
/// Loop policy for <see cref="AgentTurn.DriveTurnAsync"/>: the bound that keeps the orchestration
/// terminating. Mirrors <see cref="AgentOptions.MaxIterations"/>.
/// </summary>
public sealed record TurnPolicy
{
    /// <summary>
    /// Maximum model-call iterations before the turn stops unconditionally. Defaults to this
    /// engine's own loop bound (<see cref="AgentOptions.MaxIterations"/> = 8); the Rust reference's
    /// <c>TurnPolicy</c> defaults to 50 because its <c>AgentConfig</c> does — each language keeps
    /// its own engine's default so the seam never changes the loop bound a consumer already gets.
    /// </summary>
    public int MaxIterations { get; init; } = new AgentOptions().MaxIterations;
}

/// <summary>
/// The <i>deterministic</i> half of a turn: the loop that decides "call the model → if it asked for
/// tools, run them and loop → else stop". The C# sibling of the Rust reference's <c>drive_turn</c>
/// (ADR-030 / SMOODEV-1974).
///
/// <para><b>One loop, two backends</b> — which is what keeps a durable path from diverging from the
/// inline path: a Temporal workflow runs this very method, and the in-process executor runs it too.
/// All I/O is delegated to <see cref="IAgentActivities"/>.</para>
/// </summary>
public static class AgentTurn
{
    /// <summary>
    /// Drive one agent turn deterministically over the supplied activity surface, appending to
    /// <paramref name="messages"/> in place.
    ///
    /// <para><paramref name="messages"/> must already be seeded with the system instructions, any
    /// prior turns, and the current user message — exactly the state the engine's loop holds when it
    /// enters. On return it carries the appended assistant/tool messages.</para>
    ///
    /// <para>This method performs <b>no I/O, no wall-clock reads, and no randomness</b> of its own,
    /// so it is replay-safe: it is valid Temporal workflow code as well as inline code. Running out
    /// of iterations mid-tool-chain returns what has accumulated so far rather than throwing,
    /// matching the engine's post-loop behavior.</para>
    /// </summary>
    public static async Task DriveTurnAsync(
        IAgentActivities activities,
        IList<ChatMessage> messages,
        IReadOnlyList<AITool> tools,
        TurnPolicy policy,
        CancellationToken cancellationToken = default)
    {
        for (var iteration = 1; iteration <= policy.MaxIterations; iteration++)
        {
            // Observe: snapshot the context window as an owned list so the call can cross an
            // activity boundary without aliasing the live conversation.
            var context = messages.ToList();

            // NOTE: no ConfigureAwait(false) here (nor on the tool await below). This method is
            // documented as valid Temporal workflow code, and a durable backend runs it inside the
            // workflow scheduler — ConfigureAwait(false) would post the continuation off that
            // single-threaded scheduler and break determinism (the workflow would hang mid-turn). The
            // in-process path has no captured context to marshal back to, so dropping it costs nothing.
            var response = await activities.ModelCallAsync(context, tools, cancellationToken);

            // Append the assistant turn under exactly the Rust push condition: non-empty content,
            // OR tool calls, OR reasoning content. An empty message carrying none of the three is
            // dropped rather than polluting the transcript.
            foreach (var produced in response.Messages)
            {
                if (!string.IsNullOrEmpty(produced.Text)
                    || produced.Contents.OfType<FunctionCallContent>().Any()
                    || produced.Contents.OfType<TextReasoningContent>().Any())
                {
                    messages.Add(produced);
                }
            }

            var calls = response.Messages.SelectMany(m => m.Contents).OfType<FunctionCallContent>().ToList();
            if (calls.Count == 0)
            {
                return;
            }

            // Act: run each tool in order and append its result, paired to the call by id (on the
            // result content) and by name (on the message's author) so the model — and a replaying
            // workflow — can match results back to calls.
            foreach (var call in calls)
            {
                var result = await activities.ToolInvokeAsync(call, cancellationToken); // no ConfigureAwait — see the model await above (workflow determinism)
                messages.Add(new ChatMessage(ChatRole.Tool, new List<AIContent> { result }) { AuthorName = call.Name });
            }
        }
    }
}

/// <summary>
/// The default, zero-infra <see cref="IAgentActivities"/>: it runs the model call through this
/// engine's provider seam (<see cref="IChatClient"/>) and tool calls through the supplied tool set,
/// inline in the calling task.
/// </summary>
public sealed class InProcessActivities : IAgentActivities
{
    private readonly IChatClient _chatClient;
    private readonly Dictionary<string, AIFunction> _functions;

    /// <summary>Build an in-process activity surface from a model client and a tool set.</summary>
    public InProcessActivities(IChatClient chatClient, IEnumerable<AITool>? tools = null)
    {
        _chatClient = chatClient ?? throw new ArgumentNullException(nameof(chatClient));
        _functions = (tools ?? Enumerable.Empty<AITool>()).OfType<AIFunction>().ToDictionary(f => f.Name, StringComparer.Ordinal);
    }

    /// <inheritdoc />
    public Task<ChatResponse> ModelCallAsync(
        IReadOnlyList<ChatMessage> messages,
        IReadOnlyList<AITool> tools,
        CancellationToken cancellationToken = default)
    {
        var options = tools.Count > 0 ? new ChatOptions { Tools = tools.ToList() } : null;
        return _chatClient.GetResponseAsync(messages, options, cancellationToken);
    }

    /// <inheritdoc />
    public async Task<FunctionResultContent> ToolInvokeAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
    {
        if (!_functions.TryGetValue(call.Name, out var function))
        {
            return new FunctionResultContent(call.CallId, $"Error: unknown tool '{call.Name}'");
        }

        try
        {
            var result = await function.InvokeAsync(new AIFunctionArguments(call.Arguments), cancellationToken).ConfigureAwait(false);
            return new FunctionResultContent(call.CallId, result);
        }
        catch (Exception ex)
        {
            // Business failure, not a dispatch failure: reported on the result so the model can
            // recover. Only an unrecoverable dispatch error would throw out of here.
            return new FunctionResultContent(call.CallId, $"Error: {ex.Message}");
        }
    }
}
