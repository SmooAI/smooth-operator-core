using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core.Extensions;

/// <summary>
/// An <see cref="AIFunction"/> backed by an extension subprocess. Registered tools appear to the
/// agent as ordinary tools named <c>&lt;extension&gt;.&lt;tool&gt;</c> (the MCP convention);
/// <see cref="InvokeCoreAsync"/> forwards to the extension over <c>tool/execute</c> and maps the
/// reply back. Mirrors the Rust engine's <c>ExtensionTool</c>. Add these to
/// <see cref="AgentOptions.Tools"/> and the engine's agentic loop calls them exactly like a native
/// tool.
/// </summary>
public sealed class ExtensionTool : AIFunction
{
    /// <summary>Upper bound for a single <c>tool/execute</c> round-trip. The engine may apply its own
    /// per-call cancellation; whichever fires first wins.</summary>
    private static readonly TimeSpan ExecuteTimeout = TimeSpan.FromSeconds(120);

    private readonly string _dottedName;
    private readonly string _bareName;
    private readonly string _description;
    private readonly JsonElement _schema;
    private readonly ExtensionProcess _process;
    private readonly Context _context;

    public ExtensionTool(string extName, ToolRegistration reg, ExtensionProcess process, Context context)
    {
        _dottedName = $"{extName}.{reg.Name}";
        _bareName = reg.Name;
        _description = reg.Description;
        _schema = JsonSerializer.SerializeToElement(reg.Parameters, SepJson.Options);
        _process = process;
        _context = context;
    }

    public override string Name => _dottedName;

    public override string Description => _description;

    public override JsonElement JsonSchema => _schema;

    protected override async ValueTask<object?> InvokeCoreAsync(AIFunctionArguments arguments, CancellationToken cancellationToken)
    {
        var argsNode = new JsonObject();
        foreach (var kv in arguments)
        {
            argsNode[kv.Key] = kv.Value is null ? null : JsonSerializer.SerializeToNode(kv.Value, SepJson.Options);
        }

        var @params = new JsonObject
        {
            ["call_id"] = Guid.NewGuid().ToString("n"),
            ["tool"] = _bareName,
            ["arguments"] = argsNode,
            ["context"] = _context.ToNode(),
        };

        var raw = await _process.RequestAsync(SepMethods.ToolExecute, @params, ExecuteTimeout, cancellationToken).ConfigureAwait(false);
        var result = raw.Deserialize<ToolExecuteResult>(SepJson.Options)
            ?? throw new InvalidOperationException("malformed tool/execute result");
        if (result.IsError)
        {
            // Surface as an exception so the engine folds it into an error tool-result (the Rust
            // ExtensionTool bails on is_error the same way). `details` is dropped here — Tool results
            // are a string; structured details ride tool/update in a later phase.
            throw new InvalidOperationException(result.Content);
        }
        return result.Content;
    }
}
