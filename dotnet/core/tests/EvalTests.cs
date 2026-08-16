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

    // A RATCHET, not a duplicate of the corpus. Comparing the loaded set against the file catches a
    // language that subsets or mis-parses it, but not a scenario deleted from the file itself —
    // both sides shrink together and every language stays green. This floor is what makes a
    // deletion loud. Raise it when you add scenarios; lowering it should require saying why.
    private const int MinScenarios = 15;

    private readonly ITestOutputHelper _output;

    public EvalTests(ITestOutputHelper output) => _output = output;

    private static string SupportPrompt => EvalScenarios.SupportPrompt;

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
            mean >= EvalScenarios.AggregateMeanThreshold,
            $"eval aggregate mean {mean:F2} < {EvalScenarios.AggregateMeanThreshold}; per-scenario scores = [{string.Join(", ", scores)}]");
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
            mean >= EvalScenarios.HardAggregateMeanThreshold,
            $"hard suite collapsed: mean {mean:F2} < floor {EvalScenarios.HardAggregateMeanThreshold} — a broad regression, not just one hard miss; scores = [{string.Join(", ", scores)}]");
    }

    /// <summary>
    /// The drift guard: runs OFFLINE in normal CI. Asserts the scenario set this suite would
    /// execute is exactly the set in spec/evals/scenarios.json — same count, same ids — so a
    /// language that subsets, filters or mis-parses the corpus goes red here instead of quietly
    /// running a forked suite (which is how THIS corpus drifted).
    /// </summary>
    [Fact]
    public void EvalCorpus_MatchesSharedSpec()
    {
        var fileIds = EvalScenarios.FileScenarioIds;
        var loadedIds = EvalScenarios.AllScenarios.Select(s => s.Name).ToList();

        Assert.Equal(fileIds.Count, loadedIds.Count);
        Assert.Equal(fileIds.OrderBy(x => x, StringComparer.Ordinal), loadedIds.OrderBy(x => x, StringComparer.Ordinal));
        Assert.Equal(loadedIds.Count, loadedIds.Distinct(StringComparer.Ordinal).Count());

        // The core + hard tiers must partition the corpus — no scenario silently unrun.
        Assert.Equal(loadedIds.Count, EvalScenarios.All.Count + EvalScenarios.Hard.Count);
        Assert.NotEmpty(EvalScenarios.All);

        Assert.True(
            loadedIds.Count >= MinScenarios,
            $"corpus shrank: {loadedIds.Count} scenarios < ratchet floor {MinScenarios} — a scenario was deleted from spec/evals/scenarios.json");

        // Every scenario must be runnable (docs already resolved by the loader, which throws on a
        // bad key). Catches a malformed corpus before a nightly burns gateway spend finding it.
        foreach (var scenario in EvalScenarios.AllScenarios)
        {
            Assert.NotEmpty(scenario.UserTurns);
            Assert.False(string.IsNullOrWhiteSpace(scenario.GroundTruth), $"{scenario.Name} ground truth");
            Assert.False(string.IsNullOrWhiteSpace(scenario.Rubric), $"{scenario.Name} rubric");
        }

        Assert.False(string.IsNullOrWhiteSpace(EvalScenarios.SupportPrompt));
        Assert.False(string.IsNullOrWhiteSpace(EvalScenarios.JudgeSystemPrompt));
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
