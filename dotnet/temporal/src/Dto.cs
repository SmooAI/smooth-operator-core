using System.Text.Json;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Temporal;

/// <summary>
/// Serde data-transfer objects for the Temporal <b>activity boundary</b>. The C# sibling of the
/// Rust reference's <c>dto.rs</c> (<c>rust/smooth-operator-temporal/src/dto.rs</c>).
///
/// <para>A Temporal activity serializes its input and output. The engine's live
/// Microsoft.Extensions.AI types — <see cref="ChatMessage"/>, <see cref="ChatResponse"/>,
/// <see cref="AIFunction"/> — carry polymorphic <see cref="AIContent"/> and non-serializable tool
/// delegates, so the activities traffic these flat, System.Text.Json-friendly projections instead
/// and reconstruct the live types on each side. Everything here is plain serde with <b>no Temporal
/// dependency</b>, so it is always compiled and unit-tested regardless of whether a worker runs.</para>
/// </summary>
internal static class DtoJson
{
    /// <summary>Serializer options for the DTO boundary. Plain STJ — the DTOs are deliberately flat
    /// so they need no custom converters (the reason the boundary exists at all).</summary>
    public static readonly JsonSerializerOptions Options = new(JsonSerializerDefaults.Web);
}

/// <summary>Token usage crossing the activity boundary. Mirrors the Rust reference's <c>Usage</c>.</summary>
public sealed record UsageDto(long PromptTokens, long CompletionTokens, long TotalTokens)
{
    public static UsageDto From(UsageDetails? usage) => usage is null
        ? new UsageDto(0, 0, 0)
        : new UsageDto(usage.InputTokenCount ?? 0, usage.OutputTokenCount ?? 0, usage.TotalTokenCount ?? 0);

    public UsageDetails ToUsageDetails() => new()
    {
        InputTokenCount = PromptTokens,
        OutputTokenCount = CompletionTokens,
        TotalTokenCount = TotalTokens,
    };
}

/// <summary>A model-requested tool call, serialized. Mirrors the Rust reference's <c>ToolCall</c>.
/// Arguments are carried as a JSON element so any argument shape survives the boundary.</summary>
public sealed record FunctionCallDto(string CallId, string Name, JsonElement Arguments)
{
    public static FunctionCallDto From(FunctionCallContent call)
    {
        var args = JsonSerializer.SerializeToElement(
            call.Arguments ?? new Dictionary<string, object?>(), DtoJson.Options);
        return new FunctionCallDto(call.CallId, call.Name, args);
    }

    public FunctionCallContent ToContent()
    {
        var args = Arguments.ValueKind == JsonValueKind.Object
            ? JsonSerializer.Deserialize<Dictionary<string, object?>>(Arguments, DtoJson.Options)
            : null;
        return new FunctionCallContent(CallId, Name, args);
    }
}

/// <summary>A single conversation message, flattened. Covers the shapes the loop produces: system /
/// user / assistant text, an assistant turn that requested tool calls, a tool-result message, and
/// reasoning content. Mirrors the Rust reference's serde <c>Message</c>.</summary>
public sealed record ChatMessageDto(
    string Role,
    string? Text,
    string? AuthorName,
    IReadOnlyList<FunctionCallDto>? ToolCalls,
    string? ToolResultCallId,
    string? ToolResultContent,
    string? ReasoningContent)
{
    public static ChatMessageDto From(ChatMessage message)
    {
        var toolCalls = message.Contents.OfType<FunctionCallContent>().Select(FunctionCallDto.From).ToList();
        var result = message.Contents.OfType<FunctionResultContent>().FirstOrDefault();
        var reasoning = message.Contents.OfType<TextReasoningContent>().FirstOrDefault();
        return new ChatMessageDto(
            message.Role.Value,
            string.IsNullOrEmpty(message.Text) ? null : message.Text,
            message.AuthorName,
            toolCalls.Count > 0 ? toolCalls : null,
            result?.CallId,
            result?.Result?.ToString(),
            reasoning?.Text);
    }

    public ChatMessage ToChatMessage()
    {
        var contents = new List<AIContent>();
        if (!string.IsNullOrEmpty(ReasoningContent))
        {
            contents.Add(new TextReasoningContent(ReasoningContent));
        }
        if (!string.IsNullOrEmpty(Text))
        {
            contents.Add(new TextContent(Text));
        }
        if (ToolCalls is not null)
        {
            foreach (var call in ToolCalls)
            {
                contents.Add(call.ToContent());
            }
        }
        if (ToolResultCallId is not null)
        {
            contents.Add(new FunctionResultContent(ToolResultCallId, ToolResultContent));
        }
        return new ChatMessage(new ChatRole(Role), contents) { AuthorName = AuthorName };
    }
}

/// <summary>A tool schema offered to the model, serialized. Mirrors the Rust reference's
/// <c>ToolSchema</c>. Reconstructing a live <see cref="AIFunction"/> is impossible across the wire
/// (it owns a delegate), so the worker's tool set is resolved by <see cref="Name"/> in the
/// <c>tool_invoke</c> activity — the schema here is only what the model needs to SEE.</summary>
public sealed record ToolSchemaDto(string Name, string Description, JsonElement Parameters)
{
    public static ToolSchemaDto From(AITool tool)
    {
        var schema = tool is AIFunction fn ? fn.JsonSchema : default;
        return new ToolSchemaDto(tool.Name, tool.Description ?? string.Empty, schema);
    }
}

/// <summary>Input to the <c>model_call</c> activity: the context window + the tools the model may
/// call. Mirrors the Rust reference's <c>ModelCallInput</c>.</summary>
public sealed record ModelCallInput(IReadOnlyList<ChatMessageDto> Messages, IReadOnlyList<ToolSchemaDto> Tools);

/// <summary>Output of the <c>model_call</c> activity: a serde projection of <see cref="ChatResponse"/>.
/// Mirrors the Rust reference's <c>ModelCallOutput</c> — it carries exactly the fields the
/// orchestration and accounting read (assistant content, tool calls, reasoning, finish reason,
/// usage) and reconstructs a <see cref="ChatResponse"/> the <c>drive_turn</c> loop consumes.</summary>
public sealed record ModelCallOutput(
    string Content,
    IReadOnlyList<FunctionCallDto> ToolCalls,
    string? ReasoningContent,
    string FinishReason,
    UsageDto Usage)
{
    public static ModelCallOutput FromChatResponse(ChatResponse response)
    {
        var allContents = response.Messages.SelectMany(m => m.Contents).ToList();
        var toolCalls = allContents.OfType<FunctionCallContent>().Select(FunctionCallDto.From).ToList();
        var reasoning = allContents.OfType<TextReasoningContent>().FirstOrDefault()?.Text;
        return new ModelCallOutput(
            response.Text,
            toolCalls,
            reasoning,
            response.FinishReason?.Value ?? string.Empty,
            UsageDto.From(response.Usage));
    }

    public ChatResponse ToChatResponse()
    {
        var contents = new List<AIContent>();
        if (!string.IsNullOrEmpty(ReasoningContent))
        {
            contents.Add(new TextReasoningContent(ReasoningContent));
        }
        if (!string.IsNullOrEmpty(Content))
        {
            contents.Add(new TextContent(Content));
        }
        foreach (var call in ToolCalls)
        {
            contents.Add(call.ToContent());
        }
        var message = new ChatMessage(ChatRole.Assistant, contents);
        return new ChatResponse(message)
        {
            Usage = Usage.ToUsageDetails(),
            FinishReason = string.IsNullOrEmpty(FinishReason) ? null : new ChatFinishReason(FinishReason),
        };
    }
}

/// <summary>Input to the <c>tool_invoke</c> activity: the single tool call to execute. Mirrors the
/// Rust reference's <c>ToolInvokeInput</c>.</summary>
public sealed record ToolInvokeInput(FunctionCallDto Call);

/// <summary>Output of the <c>tool_invoke</c> activity. A tool <b>business</b> failure is reported
/// on <see cref="IsError"/> (never as a thrown activity error), mirroring the engine's
/// <c>InvokeToolAsync</c> and the Rust reference's <c>ToolResult::is_error</c>, so the model sees
/// the failure and can recover.</summary>
public sealed record ToolInvokeOutput(string CallId, string Content, bool IsError)
{
    public FunctionResultContent ToContent() => new(CallId, Content);
}

/// <summary>One message of the workflow's returned conversation, flattened for the workflow-result
/// boundary. Mirrors the Rust reference returning the full <c>Conversation</c>.</summary>
public sealed record TurnMessage(string Role, string Content, string? AuthorName)
{
    public static TurnMessage From(ChatMessage message)
    {
        var text = message.Text;
        if (string.IsNullOrEmpty(text))
        {
            var result = message.Contents.OfType<FunctionResultContent>().FirstOrDefault();
            text = result?.Result?.ToString() ?? string.Empty;
        }
        return new TurnMessage(message.Role.Value, text, message.AuthorName);
    }
}

/// <summary>The conversation an <see cref="AgentTurnWorkflow"/> returns.</summary>
public sealed record TurnConversation(IReadOnlyList<TurnMessage> Messages)
{
    /// <summary>The final assistant message text — the answer the user sees.</summary>
    public string? LastAssistantContent =>
        Messages.LastOrDefault(m => m.Role == ChatRole.Assistant.Value)?.Content;
}
