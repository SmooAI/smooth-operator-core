using System.Text.Json;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core.Tests;

internal sealed record Verdict(int Score, bool Pass, string Reasoning);

/// <summary>
/// LLM-as-judge scoring, ported from the Rust eval harness: a separate (skeptical) model grades
/// the agent's reply 1–5 against the scenario's rubric + ground-truth and returns strict JSON.
/// </summary>
internal static class EvalJudge
{
    private const string SystemPrompt =
        "You are a strict, fair evaluator of an AI customer-support agent. " +
        "You grade the agent's REPLY against the rubric and the ground-truth facts. " +
        "Be skeptical: a confident answer that invents facts not in the ground truth must score low, " +
        "and appropriately admitting 'I don't know' when the ground truth lacks the answer must score high. " +
        "Respond with ONLY a single JSON object, no prose, no markdown fences, exactly: " +
        "{\"score\": <integer 1-5>, \"pass\": <true|false>, \"reasoning\": \"<one or two sentences>\"}.";

    public static async Task<Verdict> JudgeAsync(IChatClient judge, EvalScenario scenario, string agentReply, CancellationToken cancellationToken = default)
    {
        var user =
            $"RUBRIC (what to check):\n{scenario.Rubric}\n\n" +
            $"GROUND-TRUTH FACTS (the only facts that are true here):\n{scenario.GroundTruth}\n\n" +
            $"The user asked: {scenario.UserTurns[^1]}\n" +
            $"The agent replied:\n{agentReply}\n\n" +
            "Score 1-5 per the rubric. Return ONLY the JSON object.";

        var messages = new List<ChatMessage>
        {
            new(ChatRole.System, SystemPrompt),
            new(ChatRole.User, user),
        };

        var response = await judge.GetResponseAsync(messages, cancellationToken: cancellationToken).ConfigureAwait(false);
        return Parse(response.Text);
    }

    /// <summary>Parse the verdict, tolerating stray prose/markdown around the JSON object.</summary>
    internal static Verdict Parse(string text)
    {
        var start = text.IndexOf('{');
        var end = text.LastIndexOf('}');
        var json = start >= 0 && end > start ? text[start..(end + 1)] : text;

        using var doc = JsonDocument.Parse(json);
        var root = doc.RootElement;
        var score = root.GetProperty("score").GetInt32();
        var pass = root.TryGetProperty("pass", out var p) && p.ValueKind == JsonValueKind.True;
        var reasoning = root.TryGetProperty("reasoning", out var r) ? r.GetString() ?? string.Empty : string.Empty;
        return new Verdict(score, pass, reasoning);
    }
}
