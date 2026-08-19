using System.Text.Json;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;
using Temporalio.Activities;
using Temporalio.Workflows;
// Both SmooAI.SmoothOperator.Core and Temporalio.Workflows export a `Workflow` type; alias the
// Temporal static entry point so the workflow-context calls are unambiguous.
using TemporalWorkflow = Temporalio.Workflows.Workflow;

namespace SmooAI.SmoothOperator.Temporal;

/// <summary>
/// The engine handles the Temporal <b>activities</b> run against: the model client backing
/// <c>model_call</c> and the tool set backing <c>tool_invoke</c>. Installed on an
/// <see cref="AgentTurnActivities"/> instance registered with the worker. The C# sibling of the Rust
/// reference's <c>EngineHandles</c> — but held as ordinary instance state rather than a process
/// global, because the .NET SDK registers activities as instance methods.
/// </summary>
public sealed class EngineHandles
{
    /// <summary>The agent the activities run against: its model client backs <c>model_call</c>, and
    /// its gated dispatch (<see cref="SmoothAgent.DispatchToolAsync"/>) backs <c>tool_invoke</c> — so
    /// the durable turn is gated by the same permission mode, deny policy, human gate and tool hooks
    /// the agent would enforce inline.</summary>
    public SmoothAgent Agent { get; }

    /// <summary>Model client backing the <c>model_call</c> activity.</summary>
    public IChatClient ChatClient => Agent.ChatClient;

    /// <summary>Tools backing the <c>tool_invoke</c> activity, indexed by name (the wire carries only
    /// a tool's <i>schema</i>, so the worker resolves the live delegate here).</summary>
    public IReadOnlyDictionary<string, AIFunction> ToolsByName { get; }

    /// <summary>Run the activities against <paramref name="agent"/> — the shape a consumer with a
    /// configured agent (permission mode, deny policy, human gate, hooks) should use, so the durable
    /// path enforces what the inline path does.</summary>
    public EngineHandles(SmoothAgent agent)
    {
        Agent = agent ?? throw new ArgumentNullException(nameof(agent));
        ToolsByName = agent.Tools.OfType<AIFunction>().ToDictionary(f => f.Name, StringComparer.Ordinal);
    }

    /// <summary>Run the activities against a bare model client and tool set — equivalent to an agent
    /// configured with those tools and nothing else, so nothing is gated.</summary>
    public EngineHandles(IChatClient chatClient, IEnumerable<AITool>? tools = null)
        : this(AgentFor(chatClient, tools))
    {
    }

    private static SmoothAgent AgentFor(IChatClient chatClient, IEnumerable<AITool>? tools)
    {
        var options = new AgentOptions();
        foreach (var tool in tools ?? Enumerable.Empty<AITool>())
        {
            options.Tools.Add(tool);
        }
        return new SmoothAgent(chatClient ?? throw new ArgumentNullException(nameof(chatClient)), options);
    }
}

/// <summary>
/// The side-effecting activities of an agent turn — the model call and each tool invocation, run as
/// durable Temporal activities (retried, memoized in workflow history). The C# sibling of the Rust
/// reference's <c>AgentTurnActivities</c>. State (the model client + tool set) comes from the
/// <see cref="EngineHandles"/> the worker constructs it with.
/// </summary>
public sealed class AgentTurnActivities
{
    private readonly EngineHandles _engine;

    public AgentTurnActivities(EngineHandles engine) => _engine = engine ?? throw new ArgumentNullException(nameof(engine));

    /// <summary>Liveness probe backing the scaffold <see cref="HealthWorkflow"/>.</summary>
    [Activity]
    public Task<string> HealthEcho(string message) => Task.FromResult($"smooth-operator-temporal ok: {message}");

    /// <summary>The model call — the <c>Think</c> step of the loop, run as a durable activity. Rebuilds
    /// the live conversation + tool set from the serde DTOs, calls the engine's <see cref="IChatClient"/>,
    /// and projects the response back onto the wire.</summary>
    [Activity]
    public async Task<ModelCallOutput> ModelCall(ModelCallInput input)
    {
        var messages = input.Messages.Select(m => m.ToChatMessage()).ToList();
        var tools = input.Tools
            .Select(s => _engine.ToolsByName.TryGetValue(s.Name, out var fn) ? fn : null)
            .Where(fn => fn is not null)
            .Cast<AITool>()
            .ToList();
        var options = tools.Count > 0 ? new ChatOptions { Tools = tools } : null;
        var response = await _engine.ChatClient.GetResponseAsync(messages, options).ConfigureAwait(false);
        return ModelCallOutput.FromChatResponse(response);
    }

    /// <summary>A single tool invocation — the <c>Act</c> step, run as a durable activity. Tool
    /// <b>business</b> failures come back on <see cref="ToolInvokeOutput.IsError"/> (not as a thrown
    /// activity error), matching the engine's <c>InProcessActivities.ToolInvokeAsync</c>.
    ///
    /// <para>Dispatch goes through <see cref="SmoothAgent.DispatchToolAsync"/> — the same gate chain
    /// the inline loop uses (permission engine, deny policy, human gate, tool hooks, extension fold)
    /// — so swapping <c>InProcessExecutor</c> for <see cref="TemporalExecutor"/> cannot widen what the
    /// agent is allowed to run. Invoking the resolved <see cref="AIFunction"/> directly here is the
    /// bug this activity exists <i>not</i> to have.</para></summary>
    [Activity]
    public async Task<ToolInvokeOutput> ToolInvoke(ToolInvokeInput input)
    {
        var call = input.Call.ToContent();
        try
        {
            var result = await _engine.Agent.DispatchToolAsync(call).ConfigureAwait(false);
            // AIFunctionFactory returns the tool result as a JsonElement; render it the way the
            // in-process path's FunctionResultContent.ToString() would — a bare string unquoted, a
            // structured value as its raw JSON — rather than re-serializing (which double-quotes strings).
            var content = result.Result switch
            {
                null => string.Empty,
                string s => s,
                JsonElement je => je.ValueKind == JsonValueKind.String ? je.GetString() ?? string.Empty : je.GetRawText(),
                var other => JsonSerializer.Serialize(other, DtoJson.Options),
            };
            return new ToolInvokeOutput(call.CallId, content, IsFailure(content));
        }
        catch (Exception ex)
        {
            // The gate chain reports business failures on the result rather than throwing, so reaching
            // here means a genuine dispatch fault. Still surfaced as a tool error so one broken tool
            // can't fail the whole durable turn.
            return new ToolInvokeOutput(call.CallId, $"Error: {ex.Message}", true);
        }
    }

    /// <summary>Whether a dispatched result is a failure the model should read as one.
    /// ponytail: prefix sniffing, because the engine encodes tool failures into the result string —
    /// there is no is-error channel on <see cref="FunctionResultContent"/>. If the engine ever grows a
    /// structured tool result (Rust's <c>ToolResult::is_error</c>), read that instead. Nothing in the
    /// loop branches on this today; it is carried for the consumer reading activity output.</summary>
    private static bool IsFailure(string content) =>
        content.StartsWith("Error:", StringComparison.Ordinal)
        || content.StartsWith("error:", StringComparison.Ordinal)
        || content.StartsWith("Denied by human:", StringComparison.Ordinal)
        || content.StartsWith("Blocked by hook:", StringComparison.Ordinal);
}

/// <summary>
/// Input to <see cref="AgentTurnWorkflow"/> — everything needed to seed the conversation and bound the
/// loop. Serializable, so it crosses the workflow-start boundary. Mirrors the Rust reference's
/// <c>AgentTurnInput</c>.
/// </summary>
/// <param name="SystemPrompt">System prompt for the turn.</param>
/// <param name="UserMessage">The user message that opens the turn.</param>
/// <param name="Tools">Tool schemas available to the model (the worker resolves live tools by name).</param>
/// <param name="MaxIterations">Iteration bound; <c>0</c> falls back to the <see cref="TurnPolicy"/> default.</param>
/// <param name="ApprovalRequiredTools">Tools that block durably on an <c>ApproveTool</c>/<c>DenyTool</c>
/// signal before they run — the durable human-in-the-loop unlock.</param>
/// <param name="WaitTool">The built-in durable-wait tool, if any: a model call to it with an integer
/// <c>seconds</c> argument sleeps the workflow on a Temporal timer instead of dispatching an activity.</param>
/// <param name="History">Prior conversation, oldest first, replayed between the system prompt and the
/// user message — the durable equivalent of the thread history <c>SmoothAgent.RunAsync</c> seeds from
/// a <see cref="SmoothAgentThread"/>. Null/empty starts a fresh conversation.</param>
public sealed record AgentTurnInput(
    string SystemPrompt,
    string UserMessage,
    IReadOnlyList<ToolSchemaDto>? Tools = null,
    int MaxIterations = 0,
    IReadOnlyList<string>? ApprovalRequiredTools = null,
    string? WaitTool = null,
    IReadOnlyList<ChatMessageDto>? History = null)
{
    /// <summary>The conversation this input opens with: system prompt, prior history, then the live
    /// user message — the same order <c>SmoothAgent.SeedConversationAsync</c> uses inline.
    /// Deterministic and I/O-free, so the workflow may call it directly (and a test may check the
    /// seeding without a Temporal server).</summary>
    public List<ChatMessage> SeedConversation()
    {
        var messages = new List<ChatMessage> { new(ChatRole.System, SystemPrompt) };
        if (History is not null)
        {
            messages.AddRange(History.Select(m => m.ToChatMessage()));
        }
        messages.Add(new ChatMessage(ChatRole.User, UserMessage));
        return messages;
    }

    /// <summary>Count of leading messages a returned <see cref="TurnConversation"/> carries over from
    /// before the turn — the system prompt plus any replayed history. Everything after them is new
    /// this turn (starting with the live user message), which is exactly what a caller appends back
    /// onto its thread, matching what the inline loop appends.</summary>
    public int CarriedCount => 1 + (History?.Count ?? 0);
}

/// <summary>
/// An agent turn as a Temporal workflow. Seeds the conversation, then drives the engine's
/// <see cref="AgentTurn.DriveTurnAsync"/> <b>unchanged</b> over a <see cref="WorkflowActivities"/>
/// adapter that schedules the model call + tool invocations as durable activities — so the durable
/// path is the same loop as the in-process path. The C# sibling of the Rust reference's
/// <c>AgentTurnWorkflow</c>.
///
/// <para>Workflow state is the human-approval ledger: the <see cref="ApproveTool"/> / <see cref="DenyTool"/>
/// signals populate it, and the adapter's durable gate observes it.</para>
/// </summary>
[Workflow]
public sealed class AgentTurnWorkflow
{
    private readonly List<string> _approved = new();
    private readonly List<string> _denied = new();

    [WorkflowRun]
    public async Task<TurnConversation> RunAsync(AgentTurnInput input)
    {
        var messages = input.SeedConversation();

        var policy = input.MaxIterations <= 0
            ? new TurnPolicy()
            : new TurnPolicy { MaxIterations = input.MaxIterations };

        var adapter = new WorkflowActivities(
            input.Tools ?? Array.Empty<ToolSchemaDto>(),
            input.ApprovalRequiredTools ?? Array.Empty<string>(),
            input.WaitTool,
            id => _approved.Contains(id),
            id => _denied.Contains(id));

        // The engine's deterministic loop, verbatim — one loop, two backends. It performs no I/O of
        // its own; every side-effect is delegated to the adapter, which turns it into a durable
        // activity / timer / signal-wait.
        await AgentTurn.DriveTurnAsync(adapter, messages, Array.Empty<AITool>(), policy);

        return new TurnConversation(messages.Select(ChatMessageDto.From).ToList());
    }

    /// <summary>Signal: a human approves the tool call with this id, unblocking the gate.</summary>
    [WorkflowSignal]
    public Task ApproveTool(string callId)
    {
        if (!_approved.Contains(callId))
        {
            _approved.Add(callId);
        }
        return Task.CompletedTask;
    }

    /// <summary>Signal: a human denies the tool call with this id; the gate returns a tool-error
    /// result instead of running it.</summary>
    [WorkflowSignal]
    public Task DenyTool(string callId)
    {
        if (!_denied.Contains(callId))
        {
            _denied.Add(callId);
        }
        return Task.CompletedTask;
    }
}

/// <summary>
/// Adapter that makes the engine's <see cref="IAgentActivities"/> surface schedule Temporal
/// activities / timers / signal-waits on the ambient <c>Workflow</c> context. The C# sibling
/// of the Rust reference's <c>WorkflowAgentActivities</c>.
///
/// <list type="bullet">
/// <item><b>model_call / tool_invoke</b> become durable activities (60s start-to-close).</item>
/// <item><b>approval-required tools</b> block durably on an <c>ApproveTool</c>/<c>DenyTool</c> signal
/// before running — the block is recorded in history, so it survives worker restarts and can resolve
/// hours later.</item>
/// <item>the <b>wait tool</b> (if configured) sleeps the workflow on a Temporal timer instead of
/// dispatching an activity — a durable pause that can span days.</item>
/// </list>
/// </summary>
internal sealed class WorkflowActivities : IAgentActivities
{
    private static ActivityOptions AgentActivityOptions => new() { StartToCloseTimeout = TimeSpan.FromSeconds(60) };

    private readonly IReadOnlyList<ToolSchemaDto> _toolSchemas;
    private readonly IReadOnlyList<string> _approvalRequired;
    private readonly string? _waitTool;
    private readonly Func<string, bool> _isApproved;
    private readonly Func<string, bool> _isDenied;

    public WorkflowActivities(
        IReadOnlyList<ToolSchemaDto> toolSchemas,
        IReadOnlyList<string> approvalRequired,
        string? waitTool,
        Func<string, bool> isApproved,
        Func<string, bool> isDenied)
    {
        _toolSchemas = toolSchemas;
        _approvalRequired = approvalRequired;
        _waitTool = waitTool;
        _isApproved = isApproved;
        _isDenied = isDenied;
    }

    public async Task<ChatResponse> ModelCallAsync(
        IReadOnlyList<ChatMessage> messages,
        IReadOnlyList<AITool> tools,
        CancellationToken cancellationToken = default)
    {
        // The live `tools` are ignored here (an AIFunction can't cross the wire); the workflow carries
        // the serializable tool SCHEMAS instead, and the activity resolves live tools by name.
        var input = new ModelCallInput(messages.Select(ChatMessageDto.From).ToList(), _toolSchemas);
        var output = await TemporalWorkflow.ExecuteActivityAsync(
            (AgentTurnActivities a) => a.ModelCall(input),
            AgentActivityOptions);
        return output.ToChatResponse();
    }

    public async Task<FunctionResultContent> ToolInvokeAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
    {
        // Durable wait: sleep on a Temporal timer instead of dispatching an activity. The timer is in
        // workflow history, so the pause survives restarts and can span days.
        if (_waitTool is not null && string.Equals(_waitTool, call.Name, StringComparison.Ordinal))
        {
            if (TryGetSeconds(call) is not { } seconds)
            {
                return new FunctionResultContent(call.CallId, $"wait tool '{call.Name}' requires an integer `seconds` argument");
            }
            await TemporalWorkflow.DelayAsync(TimeSpan.FromSeconds(seconds));
            return new FunctionResultContent(call.CallId, $"Waited {seconds}s (durable timer).");
        }

        // Durable human-in-the-loop gate: block until an ApproveTool / DenyTool signal names this call
        // id. The wait is recorded in history, so it survives restarts and can resolve hours later.
        if (_approvalRequired.Contains(call.Name, StringComparer.Ordinal))
        {
            await TemporalWorkflow.WaitConditionAsync(() => _isApproved(call.CallId) || _isDenied(call.CallId));
            if (_isDenied(call.CallId))
            {
                return new FunctionResultContent(call.CallId, $"Tool call '{call.Name}' was denied by human approval.");
            }
        }

        var output = await TemporalWorkflow.ExecuteActivityAsync(
            (AgentTurnActivities a) => a.ToolInvoke(new ToolInvokeInput(FunctionCallDto.From(call))),
            AgentActivityOptions);
        return output.ToContent();
    }

    /// <summary>Read an integer <c>seconds</c> argument off a wait-tool call, tolerating the JSON
    /// number shapes the argument can arrive as after a serde round trip.</summary>
    private static int? TryGetSeconds(FunctionCallContent call)
    {
        if (call.Arguments is null || !call.Arguments.TryGetValue("seconds", out var raw) || raw is null)
        {
            return null;
        }
        return raw switch
        {
            int i => i,
            long l => (int)l,
            double d => (int)d,
            JsonElement je when je.ValueKind == JsonValueKind.Number => je.GetInt32(),
            _ => int.TryParse(raw.ToString(), out var parsed) ? parsed : null,
        };
    }
}

/// <summary>
/// Scaffold liveness workflow — proves the SDK integrates end to end and backs the ephemeral-server
/// integration test. The C# sibling of the Rust reference's <c>HealthWorkflow</c>.
/// </summary>
[Workflow]
public sealed class HealthWorkflow
{
    [WorkflowRun]
    public Task<string> RunAsync(string message) =>
        TemporalWorkflow.ExecuteActivityAsync(
            (AgentTurnActivities a) => a.HealthEcho(message),
            new ActivityOptions { StartToCloseTimeout = TimeSpan.FromSeconds(10) });
}
