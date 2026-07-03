using System.Text.Json.Nodes;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>The bounded, oldest-shedding observe lane + its out-of-band <c>events_lost</c> marker.
/// Mirrors the Rust <c>observe_lane_*</c> tests.</summary>
public sealed class ObserveLaneTests
{
    private static JsonNode Ctx() => new JsonObject { ["token"] = "e", ["tier"] = "event" };

    [Fact]
    public void ShedsOldestAndMarksLoss()
    {
        var lane = new ObserveLane();
        for (var i = 0; i < ExtensionProcess.ObserveQueueCap + 3; i++)
        {
            lane.Push("turn_start", Ctx(), new JsonObject { ["n"] = i });
        }
        Assert.Equal(ExtensionProcess.ObserveQueueCap, lane.QueueCount);
        Assert.Equal(3, lane.Lost);
        Assert.Equal(ExtensionProcess.ObserveQueueCap + 3, lane.Seq);

        // First drain frame is the events_lost marker carrying the shed count, no seq.
        var marker = lane.PopForWrite()!;
        var p = marker.Params!;
        Assert.Equal("events_lost", p["event"]!.GetValue<string>());
        Assert.Equal(3, p["payload"]!["lost"]!.GetValue<long>());
        Assert.Null(p["seq"]);
        Assert.NotNull(p["context"]);
        Assert.Equal(0, lane.Lost);

        // The surviving events are the NEWEST (oldest shed): first is n=3.
        var first = lane.PopForWrite()!;
        Assert.Equal(3, first.Params!["payload"]!["n"]!.GetValue<int>());
    }

    [Fact]
    public void NoMarkerWhenNoLoss()
    {
        var lane = new ObserveLane();
        lane.Push("turn_start", Ctx(), new JsonObject());
        var f = lane.PopForWrite()!;
        Assert.Equal("turn_start", f.Params!["event"]!.GetValue<string>());
        Assert.Null(lane.PopForWrite());
    }
}
