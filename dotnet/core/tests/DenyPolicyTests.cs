using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>Ports the Rust engine's <c>deny_policy.rs</c> tests, including the adversarial cases.</summary>
public class DenyPolicyTests
{
    private static FunctionCallContent Call(string name, IDictionary<string, object?>? args = null) => new("c1", name, args);

    private static FunctionCallContent BashCall(string cmd) => Call("bash", new Dictionary<string, object?> { ["cmd"] = cmd });

    // ── glob matcher ───────────────────────────────────────────────

    [Fact]
    public void GlobExactAndWildcards()
    {
        Assert.True(DenyPolicy.GlobMatch("exact", "exact"));
        Assert.False(DenyPolicy.GlobMatch("exact", "exacts"));
        Assert.True(DenyPolicy.GlobMatch("vendor.*", "vendor.delete"));
        Assert.False(DenyPolicy.GlobMatch("vendor.*", "other.delete"));
        Assert.True(DenyPolicy.GlobMatch("*.delete", "vendor.delete"));
        Assert.False(DenyPolicy.GlobMatch("*.delete", "vendor.deleted"));
        Assert.True(DenyPolicy.GlobMatch("a*c", "abc"));
        Assert.True(DenyPolicy.GlobMatch("a*c", "ac"));
        Assert.False(DenyPolicy.GlobMatch("a*c", "ab"));
        Assert.True(DenyPolicy.GlobMatch("/prod/**", "/prod/secrets/db.txt"));
        Assert.False(DenyPolicy.GlobMatch("/prod/**", "/staging/x"));
        Assert.True(DenyPolicy.GlobMatch("**/secrets/**", "/a/b/secrets/c/d"));
        Assert.False(DenyPolicy.GlobMatch("**/secrets/**", "/a/b/c"));
    }

    // ── declarative: tools ─────────────────────────────────────────

    [Fact]
    public void ToolsSectionDeniesMatchAllowsNonmatch()
    {
        var policy = DenyPolicy.FromToml("[tools]\ndeny = [\"vendor.dangerous_tool\", \"*.delete_prod\"]\n");
        Assert.NotNull(policy.Evaluate(Call("vendor.dangerous_tool")));
        Assert.NotNull(policy.Evaluate(Call("svc.delete_prod")));
        Assert.Null(policy.Evaluate(Call("vendor.safe_tool")));
    }

    // ── declarative: bash ──────────────────────────────────────────

    [Fact]
    public void BashSectionDeniesMatchAllowsNonmatch()
    {
        var policy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\", \"terraform apply\"]\n");
        Assert.NotNull(policy.Evaluate(BashCall("aws s3 ls --profile prod")));
        Assert.NotNull(policy.Evaluate(BashCall("terraform apply -auto-approve")));
        Assert.Null(policy.Evaluate(BashCall("aws s3 ls --profile dev")));
        Assert.Null(policy.Evaluate(BashCall("aws s3 ls")));
    }

    [Fact]
    public void BashPrefixWordBoundary()
    {
        var policy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws \"]\n");
        Assert.NotNull(policy.Evaluate(BashCall("aws s3 ls")));
        Assert.Null(policy.Evaluate(BashCall("awslocal s3 ls")));
    }

    [Fact]
    public void BashDenySurvivesSudoAndCompoundAndExtraFlags()
    {
        var policy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\"]\n");
        Assert.NotNull(policy.Evaluate(BashCall("sudo aws s3 rm s3://b --profile prod")));
        Assert.NotNull(policy.Evaluate(BashCall("ls && aws s3 ls --profile prod")));
        Assert.NotNull(policy.Evaluate(BashCall("aws s3 ls --profile prod --region us-east-1")));
        Assert.NotNull(policy.Evaluate(BashCall("timeout 5 aws s3 ls --profile prod")));
    }

    // ── declarative: network ───────────────────────────────────────

    [Fact]
    public void NetworkSectionDeniesSuffixAndGlob()
    {
        var policy = DenyPolicy.FromToml("[network]\ndeny_hosts = [\"*.prod.internal\", \"prod-*.rds.amazonaws.com\", \"secrets.example.com\"]\n");
        Assert.NotNull(policy.Evaluate(Call("web_fetch", new Dictionary<string, object?> { ["url"] = "https://api.prod.internal/x" })));
        Assert.NotNull(policy.Evaluate(Call("web_fetch", new Dictionary<string, object?> { ["url"] = "https://prod.internal/" })));
        Assert.NotNull(policy.Evaluate(Call("web_fetch", new Dictionary<string, object?> { ["url"] = "https://prod-db1.rds.amazonaws.com" })));
        Assert.NotNull(policy.Evaluate(Call("web_fetch", new Dictionary<string, object?> { ["host"] = "api.secrets.example.com" })));
        Assert.Null(policy.Evaluate(Call("web_fetch", new Dictionary<string, object?> { ["url"] = "https://staging.internal/x" })));
        Assert.NotNull(policy.Evaluate(BashCall("curl https://api.prod.internal/health")));
    }

    // ── declarative: paths ─────────────────────────────────────────

    [Fact]
    public void PathsSectionDeniesWriteAndRead()
    {
        var policy = DenyPolicy.FromToml("[paths]\ndeny = [\"/prod/**\", \"**/secrets/**\"]\n");
        Assert.NotNull(policy.Evaluate(Call("file_write", new Dictionary<string, object?> { ["path"] = "/prod/config.yaml" })));
        Assert.NotNull(policy.Evaluate(Call("read_file", new Dictionary<string, object?> { ["path"] = "/app/secrets/db.env" })));
        Assert.NotNull(policy.Evaluate(Call("list_dir", new Dictionary<string, object?> { ["dir"] = "/prod/data" })));
        Assert.Null(policy.Evaluate(Call("file_write", new Dictionary<string, object?> { ["path"] = "/app/src/main.rs" })));
    }

    // ── predicate tier ─────────────────────────────────────────────

    private sealed class ProdAccountPredicate : IDenyPredicate
    {
        public DenyReason? Evaluate(FunctionCallContent call)
        {
            var cmd = call.Arguments is not null && call.Arguments.TryGetValue("cmd", out var v) ? v as string : null;
            return cmd is not null && cmd.Contains("999999999999")
                ? DenyReason.New("resolved to the prod AWS account")
                : null;
        }
    }

    [Fact]
    public void PredicateSomeDeniesNoneFallsThrough()
    {
        var policy = new DenyPolicy().WithPredicate(new ProdAccountPredicate());
        var denied = policy.Evaluate(BashCall("aws s3 ls --profile acct-999999999999"));
        Assert.Contains("prod AWS account", denied);
        Assert.Null(policy.Evaluate(BashCall("aws s3 ls --profile acct-111")));
    }

    // ── empty policy = no-op ───────────────────────────────────────

    [Fact]
    public void EmptyPolicyDeniesNothing()
    {
        var policy = new DenyPolicy();
        Assert.True(policy.IsEmpty());
        Assert.Null(policy.Evaluate(BashCall("rm -rf /prod")));
        Assert.Null(policy.Evaluate(Call("file_write", new Dictionary<string, object?> { ["path"] = "/prod/x" })));
        Assert.Null(policy.Evaluate(Call("vendor.anything")));
    }

    // ── TOML round-trip ────────────────────────────────────────────

    [Fact]
    public void TomlRoundTrip()
    {
        var rules = DenyRules.New();
        rules.Tools.Deny.Add("vendor.dangerous_tool");
        rules.Bash.DenyPatterns.Add("aws * --profile prod");
        rules.Network.DenyHosts.Add("*.prod.internal");
        rules.Paths.Deny.Add("/prod/**");
        var text = rules.ToTomlString();
        var back = DenyRules.Parse(text);
        Assert.Equal(rules.Tools.Deny, back.Tools.Deny);
        Assert.Equal(rules.Bash.DenyPatterns, back.Bash.DenyPatterns);
        Assert.Equal(rules.Network.DenyHosts, back.Network.DenyHosts);
        Assert.Equal(rules.Paths.Deny, back.Paths.Deny);
    }

    [Fact]
    public void EmptyRulesParseAndAreEmpty()
    {
        Assert.True(DenyRules.Parse("").IsEmpty());
        Assert.True(DenyRules.Parse("schema_version = 1").IsEmpty());
    }

    // ── precedence: declarative before predicate ───────────────────

    private sealed class AlwaysDeny : IDenyPredicate
    {
        public DenyReason? Evaluate(FunctionCallContent call) => DenyReason.New("predicate always denies");
    }

    [Fact]
    public void DeclarativeReasonWinsOverPredicate()
    {
        var policy = DenyPolicy.FromToml("[tools]\ndeny = [\"vendor.tool\"]\n").WithPredicate(new AlwaysDeny());
        Assert.Contains("(tools)", policy.Evaluate(Call("vendor.tool")));
        Assert.Contains("(predicate)", policy.Evaluate(Call("other.tool")));
    }
}
