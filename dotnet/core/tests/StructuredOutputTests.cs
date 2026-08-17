using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// The response side of structured output: reading back the JSON a
/// <see cref="ChatOptions.ResponseFormat"/> asked for. The request side is covered by
/// <see cref="GatewayChatClientTests"/>, against a real socket.
/// </summary>
public class StructuredOutputTests
{
    private static ChatResponse Response(string text) => new(new ChatMessage(ChatRole.Assistant, text));

    private sealed record WeatherReport(string City, int High);

    [Fact]
    public void StructuredJsonParsesTheContent()
    {
        var json = Response("""  {"city":"Indianapolis","high":82}  """).StructuredJson();

        Assert.Equal("Indianapolis", json["city"]!.GetValue<string>());
        Assert.Equal(82, json["high"]!.GetValue<int>());
    }

    [Fact]
    public void StructuredJsonRejectsEmptyContent()
    {
        var error = Assert.Throws<InvalidOperationException>(() => Response("   ").StructuredJson());
        Assert.Contains("empty content", error.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void StructuredJsonRejectsNonJsonAndQuotesIt()
    {
        var error = Assert.Throws<InvalidOperationException>(() => Response("I'm sorry, I can't do that.").StructuredJson());

        Assert.Contains("not valid JSON", error.Message, StringComparison.Ordinal);
        // The offending content is quoted back, so a model that ignored the schema is
        // diagnosable from the error alone.
        Assert.Contains("I'm sorry", error.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void StructuredJsonTruncatesTheSnippetAt200Characters()
    {
        var error = Assert.Throws<InvalidOperationException>(() => Response(new string('x', 500)).StructuredJson());

        Assert.EndsWith(": " + new string('x', 200), error.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void DeserializeJsonIntoARecord()
    {
        var report = Response("""{"city":"Indianapolis","high":82}""").DeserializeJson<WeatherReport>();

        Assert.Equal("Indianapolis", report.City);
        Assert.Equal(82, report.High);
    }

    [Fact]
    public void DeserializeJsonReportsATypeMismatch()
    {
        var error = Assert.Throws<InvalidOperationException>(
            () => Response("""{"city":"Indianapolis","high":"eighty-two"}""").DeserializeJson<WeatherReport>());

        Assert.Contains("could not deserialize", error.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void DeserializeJsonRejectsEmptyContent()
    {
        var error = Assert.Throws<InvalidOperationException>(() => Response("").DeserializeJson<WeatherReport>());

        Assert.Contains("empty content", error.Message, StringComparison.Ordinal);
    }
}
