using System.ClientModel;
using Microsoft.Extensions.AI;
using OpenAI;
using SmooAI.SmoothOperator.Core;
using Xunit.Abstractions;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-7: the C# core is held to the SAME quality bar as the Rust reference — it runs the five
/// shared eval scenarios against the live gateway and an LLM judge, and must clear an aggregate
/// mean of ≥ 4.0. Gated on SMOOTH_AGENT_E2E=1 + SMOOAI_GATEWAY_KEY, so it skips cleanly (never
/// fails) without credentials — exactly like the protocol client's LiveE2ETests.
/// </summary>
public class EvalTests
{
    private const string GatewayUrl = "https://llm.smoo.ai/v1";
    private const string DefaultModel = "claude-haiku-4-5";
    private const double AggregateMeanThreshold = 4.0;

    // Lenient floor for the hard suite: a single hard scenario scoring 1–2 should not redden the
    // suite, but a broad collapse (most failing) should — the improvement dashboard, mirroring the
    // Rust extended_judge.
    private const double HardAggregateMeanFloor = 3.0;

    private readonly ITestOutputHelper _output;

    public EvalTests(ITestOutputHelper output) => _output = output;

    private const string SupportPrompt =
        "You are SmooAI's customer support agent. Answer using ONLY the knowledge provided to you. " +
        "If the knowledge does not contain the answer, clearly say you don't have that information — " +
        "never invent facts, names, or policies. Be concise and courteous.";

    private static IChatClient Gateway(string apiKey, string model) =>
        new OpenAIClient(new ApiKeyCredential(apiKey), new OpenAIClientOptions { Endpoint = new Uri(GatewayUrl) })
            .GetChatClient(model)
            .AsIChatClient();

    [SkippableFact]
    public async Task Evals_AggregateMean_ClearsThreshold()
    {
        Skip.IfNot(
            Environment.GetEnvironmentVariable("SMOOTH_AGENT_E2E") == "1",
            "SMOOTH_AGENT_E2E != \"1\" — skipping live-gateway eval suite.");

        var apiKey = Environment.GetEnvironmentVariable("SMOOAI_GATEWAY_KEY");
        Skip.If(string.IsNullOrWhiteSpace(apiKey), "SMOOAI_GATEWAY_KEY unset/empty — skipping live-gateway eval suite.");

        var judgeModel = Environment.GetEnvironmentVariable("SMOOTH_AGENT_JUDGE_MODEL") ?? DefaultModel;
        using var agentClient = Gateway(apiKey!, DefaultModel);
        using var judgeClient = Gateway(apiKey!, judgeModel);

        var scores = new List<int>();
        foreach (var scenario in EvalScenarios.All)
        {
            var knowledge = new InMemoryKnowledgeBase();
            foreach (var (content, source) in scenario.KbDocs)
            {
                await knowledge.IngestAsync(new KnowledgeDocument(source, content, source));
            }

            var agent = new SmoothAgent(agentClient, new AgentOptions { Instructions = SupportPrompt, Knowledge = knowledge });
            var thread = agent.GetNewThread();

            AgentRunResponse? last = null;
            foreach (var turn in scenario.UserTurns)
            {
                last = await agent.RunAsync(turn, thread);
            }

            var verdict = await EvalJudge.JudgeAsync(judgeClient, scenario, last!.Text);
            scores.Add(verdict.Score);
        }

        var mean = scores.Average();
        Assert.True(
            mean >= AggregateMeanThreshold,
            $"eval aggregate mean {mean:F2} < {AggregateMeanThreshold}; per-scenario scores = [{string.Join(", ", scores)}]");
    }

    /// <summary>
    /// The harder, adversarial + developer-experience suite (<see cref="EvalScenarios.Hard"/>),
    /// ported from the Rust <c>extended_judge</c>. Asserts only a lenient floor so a single hard
    /// miss surfaces as an improvement target (printed) without reddening CI, while a broad collapse
    /// still fails. Prefer a stronger judge here: SMOOTH_AGENT_JUDGE_MODEL=claude-sonnet-4-5.
    /// </summary>
    [SkippableFact]
    public async Task Evals_Hard_AggregateMean_ClearsFloor()
    {
        Skip.IfNot(
            Environment.GetEnvironmentVariable("SMOOTH_AGENT_E2E") == "1",
            "SMOOTH_AGENT_E2E != \"1\" — skipping live-gateway hard-eval suite.");

        var apiKey = Environment.GetEnvironmentVariable("SMOOAI_GATEWAY_KEY");
        Skip.If(string.IsNullOrWhiteSpace(apiKey), "SMOOAI_GATEWAY_KEY unset/empty — skipping live-gateway hard-eval suite.");

        var judgeModel = Environment.GetEnvironmentVariable("SMOOTH_AGENT_JUDGE_MODEL") ?? DefaultModel;
        using var agentClient = Gateway(apiKey!, DefaultModel);
        using var judgeClient = Gateway(apiKey!, judgeModel);

        var scores = new List<int>();
        var misses = new List<string>();
        foreach (var scenario in EvalScenarios.Hard)
        {
            var knowledge = new InMemoryKnowledgeBase();
            foreach (var (content, source) in scenario.KbDocs)
            {
                await knowledge.IngestAsync(new KnowledgeDocument(source, content, source));
            }

            var agent = new SmoothAgent(agentClient, new AgentOptions { Instructions = SupportPrompt, Knowledge = knowledge });
            var thread = agent.GetNewThread();

            AgentRunResponse? last = null;
            foreach (var turn in scenario.UserTurns)
            {
                last = await agent.RunAsync(turn, thread);
            }

            var verdict = await EvalJudge.JudgeAsync(judgeClient, scenario, last!.Text);
            scores.Add(verdict.Score);
            _output.WriteLine($"[hard] {scenario.Name}: {verdict.Score}/5 — {verdict.Reasoning}");
            if (verdict.Score < 4)
            {
                misses.Add($"{scenario.Name} ({verdict.Score}/5): {verdict.Reasoning}");
            }
        }

        var mean = scores.Average();
        _output.WriteLine($"[hard] aggregate mean {mean:F2}/5 across {scores.Count} scenarios; " +
            (misses.Count == 0 ? "all met threshold 🎉 — consider raising the bar." : $"{misses.Count} improvement target(s): {string.Join(" | ", misses)}"));

        Assert.True(
            mean >= HardAggregateMeanFloor,
            $"hard suite collapsed: mean {mean:F2} < floor {HardAggregateMeanFloor} — a broad regression, not just one hard miss; scores = [{string.Join(", ", scores)}]");
    }

    // Always-on (no network): the judge JSON parser tolerates stray prose / markdown fences.
    [Fact]
    public void Judge_Parse_ToleratesMarkdownFences()
    {
        var verdict = EvalJudge.Parse("```json\n{\"score\": 5, \"pass\": true, \"reasoning\": \"grounded\"}\n```");
        Assert.Equal(5, verdict.Score);
        Assert.True(verdict.Pass);
        Assert.Equal("grounded", verdict.Reasoning);
    }
}
