using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Produces the <c>send_sidekick</c> tool a lead agent uses to delegate a sub-task to a named
/// <see cref="OperatorRole"/> from a <see cref="Cast"/>. The dispatched sidekick runs as its own
/// <see cref="SmoothAgent"/> with the role's instructions and a clearance-filtered slice of the
/// parent's tools; only its final message bubbles back to the lead — its working transcript stays
/// isolated. Mirrors the Rust engine's <c>DispatchSubagentTool</c> and maps onto Microsoft Agent
/// Framework's handoff pattern.
/// </summary>
public sealed class SubagentDispatcher
{
    /// <summary>The tool name the lead's model calls to delegate.</summary>
    public const string ToolName = "send_sidekick";

    private readonly IChatClient _chatClient;
    private readonly Cast _cast;
    private readonly IReadOnlyList<AITool> _parentTools;

    public SubagentDispatcher(IChatClient chatClient, Cast cast, IReadOnlyList<AITool>? parentTools = null)
    {
        _chatClient = chatClient ?? throw new ArgumentNullException(nameof(chatClient));
        _cast = cast ?? throw new ArgumentNullException(nameof(cast));
        _parentTools = parentTools ?? Array.Empty<AITool>();
    }

    /// <summary>The dispatch tool to add to a lead agent's <see cref="AgentOptions.Tools"/>.</summary>
    public AIFunction AsTool() => AIFunctionFactory.Create(
        DispatchAsync,
        ToolName,
        "Delegate a sub-task to a named sidekick agent (a focused specialist with scoped tools) and return its result.");

    private async Task<string> DispatchAsync(string role, string task, CancellationToken cancellationToken)
    {
        var operatorRole = _cast.Get(role);
        if (operatorRole is null)
        {
            var available = string.Join(", ", _cast.Sidekicks().Select(r => r.Name));
            return $"Error: no sidekick named '{role}'. Available sidekicks: {available}";
        }

        // The sidekick only sees the tools its clearance permits — a denied tool isn't even in
        // its registry, so it cannot call it.
        var allowedTools = _parentTools
            .OfType<AIFunction>()
            .Where(tool => operatorRole.Permissions.Allows(tool.Name))
            .Cast<AITool>()
            .ToList();

        var options = new AgentOptions
        {
            Name = operatorRole.Name,
            Instructions = operatorRole.Instructions,
            MaxIterations = operatorRole.MaxIterations,
        };
        foreach (var tool in allowedTools)
        {
            options.Tools.Add(tool);
        }

        var sidekick = new SmoothAgent(_chatClient, options);
        var result = await sidekick.RunAsync(task, cancellationToken).ConfigureAwait(false);

        // Only the summary crosses back to the lead; the sidekick's transcript is isolated.
        return result.Text;
    }
}
