using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>Ports the Rust reference engine's PromptCache tests (conversation.rs).</summary>
public class PromptCacheTests
{
    [Fact]
    public void SplitsAtBoundary()
    {
        var c = new PromptCache($"static rules here{PromptCache.Boundary}dynamic context here");
        Assert.Equal("static rules here", c.StaticPortion);
        Assert.Equal("dynamic context here", c.DynamicPortion);
    }

    [Fact]
    public void NoMarkerTreatsAllAsDynamic()
    {
        const string prompt = "no marker in this prompt";
        var c = new PromptCache(prompt);
        Assert.Equal(string.Empty, c.StaticPortion);
        Assert.Equal(prompt, c.DynamicPortion);
    }

    [Fact]
    public void FullPromptCombinesStaticBoundaryDynamic()
    {
        var prompt = $"You are an assistant.{PromptCache.Boundary}Project: Smooth";
        Assert.Equal(prompt, new PromptCache(prompt).FullPrompt());
    }

    [Fact]
    public void FullPromptRoundTripsUnsplitPrompt()
    {
        Assert.Equal("all dynamic", new PromptCache("all dynamic").FullPrompt());
    }

    [Fact]
    public void UpdateDynamicOnlyChangesDynamicPortion()
    {
        var c = new PromptCache($"static{PromptCache.Boundary}old dynamic");
        var original = c.StaticHash();

        c.UpdateDynamic("new dynamic");

        Assert.Equal("new dynamic", c.DynamicPortion);
        Assert.Equal("static", c.StaticPortion);
        Assert.Equal(original, c.StaticHash());
    }

    [Fact]
    public void StaticHashIsDeterministic()
    {
        var prompt = $"same static{PromptCache.Boundary}dynamic";
        Assert.Equal(new PromptCache(prompt).StaticHash(), new PromptCache(prompt).StaticHash());
    }

    [Fact]
    public void StaticHashChangesWhenStaticChanges()
    {
        var a = new PromptCache($"static A{PromptCache.Boundary}dynamic");
        var b = new PromptCache($"static B{PromptCache.Boundary}dynamic");
        Assert.NotEqual(a.StaticHash(), b.StaticHash());
    }

    [Fact]
    public void CachedTokensReturnsStaticTokenEstimate()
    {
        // "static text" is 11 chars => 11/4 + 1 = 3
        Assert.Equal((11 / 4) + 1, new PromptCache($"static text{PromptCache.Boundary}dynamic").CachedTokens());
        // No marker => empty static => 0 tokens.
        Assert.Equal(0, new PromptCache("all dynamic").CachedTokens());
    }
}
