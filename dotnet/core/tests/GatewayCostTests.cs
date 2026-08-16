using System.Net;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// The gateway reports per-request cost ONLY in a response header.
///
/// The .NET engine takes an injected <see cref="IChatClient"/>, so it has no HTTP client
/// of its own to read headers from (unlike the Go engine). These cover the parser and the
/// seam the cost flows through, so a client wrapping the raw HTTP call lands a real
/// <c>TotalCostUsd</c> on the run. A real HTTP round-trip is exercised too, against a
/// local listener, to prove the parser works on actual <c>HttpResponseHeaders</c>.
/// </summary>
public class GatewayCostTests
{
    private static Func<string, string?> Lookup(Dictionary<string, string> headers) =>
        name => headers.TryGetValue(name, out var v) ? v : null;

    [Fact]
    public void PrefersMarginThenOriginalThenLegacy()
    {
        Assert.Equal(3.0e-05m, GatewayCost.Parse(Lookup(new()
        {
            ["x-litellm-response-cost-margin-amount"] = "3.0e-05",
            ["x-litellm-response-cost-original"] = "1.0e-05",
            ["x-litellm-response-cost"] = "9.0e-05",
        })));

        Assert.Equal(1.0e-05m, GatewayCost.Parse(Lookup(new()
        {
            ["x-litellm-response-cost-original"] = "1.0e-05",
            ["x-litellm-response-cost"] = "9.0e-05",
        })));

        Assert.Equal(1.47e-05m, GatewayCost.Parse(Lookup(new() { ["x-litellm-response-cost"] = "1.47e-05" })));
    }

    [Fact]
    public void ReadsTheGenericGatewayFallbacks()
    {
        Assert.Equal(0.5m, GatewayCost.Parse(Lookup(new() { ["x-response-cost"] = "0.5" })));
        Assert.Equal(0.25m, GatewayCost.Parse(Lookup(new() { ["x-cost-usd"] = "0.25" })));
    }

    // The distinction the whole fix rests on: absent and zero are BOTH "unmeasured",
    // never a recorded $0.
    [Theory]
    [InlineData("x-litellm-response-cost", "0")]
    [InlineData("x-litellm-response-cost", "-1")]
    [InlineData("x-litellm-response-cost", "not-a-number")]
    [InlineData("x-unrelated", "5")]
    public void AbsentZeroAndUnparseableAreAllUnmeasured(string name, string value)
    {
        Assert.Null(GatewayCost.Parse(Lookup(new() { [name] = value })));
    }

    [Fact]
    public void EmptyHeadersAreUnmeasured()
    {
        Assert.Null(GatewayCost.Parse(Lookup(new())));
        Assert.Null(GatewayCost.Parse((IReadOnlyDictionary<string, string?>?)null));
    }

    [Fact]
    public void ZeroMarginFallsThroughToARealOriginal()
    {
        Assert.Equal(2.5e-05m, GatewayCost.Parse(Lookup(new()
        {
            ["x-litellm-response-cost-margin-amount"] = "0",
            ["x-litellm-response-cost-original"] = "2.5e-05",
        })));
    }

    [Fact]
    public async Task ParsesRealHttpResponseHeaders()
    {
        using var listener = new HttpListener();
        var port = GetFreePort();
        listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        listener.Start();
        var serving = Task.Run(async () =>
        {
            var ctx = await listener.GetContextAsync();
            ctx.Response.AddHeader("x-litellm-response-cost", "1.47e-05");
            ctx.Response.Close();
        });

        using var http = new HttpClient();
        using var response = await http.GetAsync($"http://127.0.0.1:{port}/");
        await serving;

        Assert.Equal(1.47e-05m, GatewayCost.Parse(response.Headers));
        listener.Stop();
    }

    private static int GetFreePort()
    {
        var l = new System.Net.Sockets.TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    // ---- the tracker ----

    private static readonly ModelPricing Pricing = new(1000m, 1000m);

    private static UsageDetails Usage() => new() { InputTokenCount = 10, OutputTokenCount = 5 };

    [Fact]
    public void TrackerPrefersTheGatewayCostOverTheLocalEstimate()
    {
        var tracker = new CostTracker();
        tracker.RecordWithGatewayCost("m", Usage(), 0.25m, Pricing);
        Assert.Equal(0.25m, tracker.TotalCostUsd);
        Assert.Equal(10, tracker.TotalPromptTokens);
    }

    [Fact]
    public void TrackerFallsBackToLocalPricingWhenUnmeasured()
    {
        var tracker = new CostTracker();
        tracker.RecordWithGatewayCost("m", Usage(), null, Pricing);
        Assert.True(tracker.TotalCostUsd > 0m, "an unmeasured cost must fall back to local pricing, not record 0");
    }
}
