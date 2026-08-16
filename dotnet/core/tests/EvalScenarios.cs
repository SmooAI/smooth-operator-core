using System.Text.Json;
using System.Text.Json.Serialization;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// One eval scenario: what knowledge to seed, what to ask, and how the judge scores it.
/// Loaded from the SHARED corpus at <c>spec/evals/scenarios.json</c> — never defined in code.
/// </summary>
internal sealed record EvalScenario(
    string Name,
    string Tier,
    IReadOnlyList<(string Content, string Source)> KbDocs,
    IReadOnlyList<string> UserTurns,
    string GroundTruth,
    string Rubric);

/// <summary>
/// The shared eval corpus, loaded from <c>spec/evals/scenarios.json</c> (copied next to the test
/// assembly by the csproj). Nothing here is hand-written: this file used to hold a hard-coded copy
/// of the scenarios, which is exactly how the .NET corpus forked from the other four languages —
/// it swapped <c>prompt_injection_in_kb</c> out of its core tier and grew a hard tier nobody else
/// had. See the corpus file's <c>$comment</c>.
/// </summary>
internal static class EvalScenarios
{
    private sealed record RawDoc(
        [property: JsonPropertyName("content")] string Content,
        [property: JsonPropertyName("source")] string Source);

    private sealed record RawScenario(
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("tier")] string Tier,
        [property: JsonPropertyName("intent")] string Intent,
        [property: JsonPropertyName("kb_docs")] IReadOnlyList<string> KbDocs,
        [property: JsonPropertyName("user_turns")] IReadOnlyList<string> UserTurns,
        [property: JsonPropertyName("ground_truth")] string GroundTruth,
        [property: JsonPropertyName("rubric")] string Rubric);

    private sealed record RawCorpus(
        [property: JsonPropertyName("support_prompt")] string SupportPrompt,
        [property: JsonPropertyName("judge_system_prompt")] string JudgeSystemPrompt,
        [property: JsonPropertyName("aggregate_mean_threshold")] double AggregateMeanThreshold,
        [property: JsonPropertyName("hard_aggregate_mean_threshold")] double HardAggregateMeanThreshold,
        [property: JsonPropertyName("docs")] IReadOnlyDictionary<string, RawDoc> Docs,
        [property: JsonPropertyName("scenarios")] IReadOnlyList<RawScenario> Scenarios);

    /// <summary>The shared corpus, copied next to the test assembly by the csproj.</summary>
    public static string CorpusPath { get; } = Path.Combine(AppContext.BaseDirectory, "evals", "scenarios.json");

    private static readonly RawCorpus Corpus =
        JsonSerializer.Deserialize<RawCorpus>(File.ReadAllText(CorpusPath))
        ?? throw new InvalidOperationException($"shared eval corpus did not parse: {CorpusPath}");

    /// <summary>The scenario ids exactly as they appear in the corpus file, in file order.</summary>
    public static IReadOnlyList<string> FileScenarioIds { get; } = Corpus.Scenarios.Select(s => s.Id).ToList();

    public static string SupportPrompt => Corpus.SupportPrompt;
    public static string JudgeSystemPrompt => Corpus.JudgeSystemPrompt;
    public static double AggregateMeanThreshold => Corpus.AggregateMeanThreshold;
    public static double HardAggregateMeanThreshold => Corpus.HardAggregateMeanThreshold;

    private static EvalScenario Materialize(RawScenario raw) => new(
        raw.Id,
        raw.Tier,
        raw.KbDocs.Select(key => Corpus.Docs.TryGetValue(key, out var doc)
            ? (doc.Content, doc.Source)
            : throw new InvalidOperationException($"scenario {raw.Id} references unknown doc \"{key}\"")).ToList(),
        raw.UserTurns,
        raw.GroundTruth,
        raw.Rubric);

    /// <summary>Every scenario in the corpus, both tiers.</summary>
    public static IReadOnlyList<EvalScenario> AllScenarios { get; } = Corpus.Scenarios.Select(Materialize).ToList();

    /// <summary>The core tier — the behaviors every engine must clear.</summary>
    public static IReadOnlyList<EvalScenario> All { get; } =
        AllScenarios.Where(s => s.Tier == "core").ToList();

    /// <summary>The hard tier — adversarial/adjacent probes on a lenient floor.</summary>
    public static IReadOnlyList<EvalScenario> Hard { get; } =
        AllScenarios.Where(s => s.Tier == "hard").ToList();
}
