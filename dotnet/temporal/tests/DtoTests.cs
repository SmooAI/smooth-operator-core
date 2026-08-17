using System.Text.Json;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Temporal;

namespace SmooAI.SmoothOperator.Temporal.Tests;

/// <summary>
/// Unit tests of the serde DTO boundary (<c>Dto.cs</c>) — the projections the Temporal activities
/// marshal across the activity boundary. These run with <b>no Temporal server and no worker</b>,
/// mirroring the Rust reference's <c>dto.rs</c> test module one-for-one: the boundary is pure
/// System.Text.Json over flat records, and this proves a turn's data actually survives the round
/// trip an activity subjects it to.
/// </summary>
public class DtoTests
{
    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);

    private static ChatResponse SampleResponse() => new(new ChatMessage(ChatRole.Assistant, new List<AIContent>
    {
        new TextReasoningContent("because"),
        new TextContent("hello"),
        new FunctionCallContent("call-1", "echo", new Dictionary<string, object?> { ["text"] = "hi" }),
    }))
    {
        Usage = new UsageDetails { InputTokenCount = 10, OutputTokenCount = 5, TotalTokenCount = 15 },
        FinishReason = new ChatFinishReason("tool_calls"),
    };

    /// <summary>Rust: <c>model_call_output_round_trips_through_llm_response</c>. ChatResponse → DTO →
    /// ChatResponse preserves every field the orchestration and accounting read.</summary>
    [Fact]
    public void ModelCallOutput_RoundTrips_ThroughChatResponse()
    {
        var dto = ModelCallOutput.FromChatResponse(SampleResponse());
        var restored = dto.ToChatResponse();

        Assert.Equal("hello", restored.Text);
        var call = Assert.Single(restored.Messages.SelectMany(m => m.Contents).OfType<FunctionCallContent>());
        Assert.Equal("call-1", call.CallId);
        Assert.Equal("echo", call.Name);
        Assert.Equal("tool_calls", restored.FinishReason?.Value);
        Assert.Equal(15, restored.Usage?.TotalTokenCount);
        Assert.Equal("because", restored.Messages.SelectMany(m => m.Contents).OfType<TextReasoningContent>().Single().Text);
    }

    /// <summary>Rust: <c>model_call_output_serde_round_trips</c>. The DTO survives a JSON round trip —
    /// i.e. it can actually cross a Temporal activity boundary.</summary>
    [Fact]
    public void ModelCallOutput_Serde_RoundTrips()
    {
        var dto = ModelCallOutput.FromChatResponse(SampleResponse());
        var json = JsonSerializer.Serialize(dto, Json);
        var back = JsonSerializer.Deserialize<ModelCallOutput>(json, Json)!;

        Assert.Equal("hello", back.Content);
        Assert.Equal("echo", back.ToolCalls.Single().Name);
        Assert.Equal("hi", back.ToolCalls.Single().Arguments.GetProperty("text").GetString());
        Assert.Equal(10, back.Usage.PromptTokens);
        Assert.Equal("tool_calls", back.FinishReason);
    }

    /// <summary>The three message shapes the loop produces — user text, an assistant tool-call turn,
    /// and a tool-result message — survive the DTO round trip with their identifying fields intact.</summary>
    [Fact]
    public void ChatMessageDto_RoundTrips_AllShapes()
    {
        var user = ChatMessageDto.From(new ChatMessage(ChatRole.User, "what is the answer?")).ToChatMessage();
        Assert.Equal(ChatRole.User, user.Role);
        Assert.Equal("what is the answer?", user.Text);

        var assistant = ChatMessageDto.From(new ChatMessage(ChatRole.Assistant, new List<AIContent>
        {
            new FunctionCallContent("call-1", "echo", new Dictionary<string, object?> { ["text"] = "hi" }),
        })).ToChatMessage();
        Assert.Equal("call-1", assistant.Contents.OfType<FunctionCallContent>().Single().CallId);

        var toolResult = ChatMessageDto.From(
            new ChatMessage(ChatRole.Tool, new List<AIContent> { new FunctionResultContent("call-1", "hello tools") }) { AuthorName = "echo" })
            .ToChatMessage();
        Assert.Equal(ChatRole.Tool, toolResult.Role);
        Assert.Equal("echo", toolResult.AuthorName);
        var result = toolResult.Contents.OfType<FunctionResultContent>().Single();
        Assert.Equal("call-1", result.CallId);
        Assert.Contains("hello tools", result.Result?.ToString());
    }

    /// <summary>Rust: <c>activity_inputs_serde_round_trip</c>. The activity input serializes cleanly,
    /// carrying both the conversation and the tool schemas offered to the model.</summary>
    [Fact]
    public void ModelCallInput_Serde_RoundTrips()
    {
        var input = new ModelCallInput(
            new[] { ChatMessageDto.From(new ChatMessage(ChatRole.User, "hi")) },
            new[] { new ToolSchemaDto("echo", "Echoes input back", JsonSerializer.SerializeToElement(new { type = "object" }, Json)) });

        var json = JsonSerializer.Serialize(input, Json);
        var back = JsonSerializer.Deserialize<ModelCallInput>(json, Json)!;

        Assert.Equal("hi", back.Messages.Single().Text);
        Assert.Equal("echo", back.Tools.Single().Name);
        Assert.Equal("object", back.Tools.Single().Parameters.GetProperty("type").GetString());
    }

    /// <summary>A tool-invoke output reconstructs a <see cref="FunctionResultContent"/> paired to its
    /// call id, and an error result is carried on the flag (never thrown).</summary>
    [Fact]
    public void ToolInvokeOutput_ReconstructsResultContent()
    {
        var ok = new ToolInvokeOutput("call-1", "hello tools", false);
        Assert.Equal("call-1", ok.ToContent().CallId);
        Assert.Contains("hello tools", ok.ToContent().Result?.ToString());

        var err = new ToolInvokeOutput("call-2", "Error: unknown tool 'nope'", true);
        Assert.True(err.IsError);
        Assert.Contains("unknown tool", err.ToContent().Result?.ToString());
    }
}
