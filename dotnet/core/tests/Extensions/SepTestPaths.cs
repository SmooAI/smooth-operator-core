namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Locates the SEP test assets (vendored fixtures + Node echo peer) copied next to the
/// test assembly.</summary>
internal static class SepTestPaths
{
    public static string SepDir { get; } = Path.Combine(AppContext.BaseDirectory, "sep");
    public static string FixturesJson { get; } = Path.Combine(SepDir, "fixtures.json");
    public static string EchoPeer { get; } = Path.Combine(SepDir, "sep-echo-peer.mjs");

    public static string? NodePath()
    {
        // PATH lookup for `node`; null when the runtime isn't installed (live tests skip).
        var paths = (Environment.GetEnvironmentVariable("PATH") ?? string.Empty).Split(Path.PathSeparator);
        foreach (var dir in paths)
        {
            var candidate = Path.Combine(dir, "node");
            if (File.Exists(candidate))
            {
                return candidate;
            }
        }
        return null;
    }
}
