using System.Diagnostics;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;
using SmooAI.SmoothOperator.Temporal;
using Temporalio.Client;
using Temporalio.Testing;
using Temporalio.Worker;

namespace SmooAI.SmoothOperator.Temporal.Tests;

/// <summary>
/// End-to-end integration tests against a real, ephemeral Temporal dev server, mirroring the Rust
/// reference's <c>tests/*_e2e.rs</c> one-for-one: a health workflow, a real agent turn, durable
/// human-in-the-loop via signals, and a durable timer. <see cref="Temporalio.Testing.WorkflowEnvironment"/>
/// auto-downloads (and caches) the Temporal dev-server binary and runs it in-process — no Docker.
///
/// <para><b>Skip-gated.</b> These run only when <c>SMOOTH_AGENT_TEMPORAL_E2E</c> is set, and even then
/// self-skip cleanly if the dev server can't be downloaded/started (offline / blocked CI) — mirroring
/// the engine's Docker-gated Postgres tests and the Rust crate's self-skipping e2e. The .NET CI
/// (<c>dotnet-checks.yml</c>) leaves the var unset, so the whole class skips there; run it locally with:
/// <code>SMOOTH_AGENT_TEMPORAL_E2E=1 dotnet test dotnet/temporal/tests</code></para>
/// </summary>
public class E2ETests
{
    private static bool Enabled =>
        Environment.GetEnvironmentVariable("SMOOTH_AGENT_TEMPORAL_E2E") is { Length: > 0 } v
        && v.Trim().ToLowerInvariant() is not ("0" or "false" or "off" or "no");

    /// <summary>Start an ephemeral local dev server, bounding the one-time binary download. On any
    /// failure (offline, blocked download, timeout) this throws <see cref="SkipException"/> so the
    /// test self-skips rather than failing — matching the Rust e2e.</summary>
    private static async Task<WorkflowEnvironment> StartEnvOrSkipAsync()
    {
        try
        {
            var start = WorkflowEnvironment.StartLocalAsync();
            var done = await Task.WhenAny(start, Task.Delay(TimeSpan.FromSeconds(120)));
            if (done != start)
            {
                throw new SkipException("SKIP: ephemeral Temporal dev server did not start within 120s (slow/blocked download).");
            }
            return await start;
        }
        catch (SkipException)
        {
            throw;
        }
        catch (Exception e)
        {
            throw new SkipException($"SKIP: could not start ephemeral Temporal dev server (likely offline): {e.Message}");
        }
    }

    private static AIFunction Echo() => AIFunctionFactory.Create((string text) => text, "echo", "Echoes input back");

    // ── health (Rust: health_workflow_e2e.rs) ────────────────────────────────────────────────

    [SkippableFact]
    public async Task HealthWorkflow_RunsEndToEnd_OnEphemeralServer()
    {
        Skip.IfNot(Enabled, "SMOOTH_AGENT_TEMPORAL_E2E not set");
        await using var env = await StartEnvOrSkipAsync();
        const string taskQueue = "smooth-operator-temporal-test";

        var engine = new EngineHandles(new MockLlmProvider());
        using var worker = new TemporalWorker(env.Client, new TemporalWorkerOptions(taskQueue)
            .AddWorkflow<HealthWorkflow>()
            .AddAllActivities(new AgentTurnActivities(engine)));

        var result = await worker.ExecuteAsync(async () =>
        {
            var handle = await env.Client.StartWorkflowAsync(
                (HealthWorkflow wf) => wf.RunAsync("ping"),
                new WorkflowOptions(id: "health-e2e-1", taskQueue: taskQueue));
            return await handle.GetResultAsync();
        });

        Assert.Equal("smooth-operator-temporal ok: ping", result);
    }

    // ── real agent turn (Rust: agent_turn_e2e.rs) ────────────────────────────────────────────

    [SkippableFact]
    public async Task AgentTurnWorkflow_RunsARealTurn_EndToEnd()
    {
        Skip.IfNot(Enabled, "SMOOTH_AGENT_TEMPORAL_E2E not set");
        await using var env = await StartEnvOrSkipAsync();
        const string taskQueue = "smooth-operator-temporal-agent-turn-test";

        var mock = new MockLlmProvider().PushText("the durable answer is 42");
        var engine = new EngineHandles(mock);
        using var worker = new TemporalWorker(env.Client, new TemporalWorkerOptions(taskQueue)
            .AddWorkflow<AgentTurnWorkflow>()
            .AddWorkflow<HealthWorkflow>()
            .AddAllActivities(new AgentTurnActivities(engine)));

        var conversation = await worker.ExecuteAsync(async () =>
        {
            var input = new AgentTurnInput("You are a test agent", "what is the durable answer?", MaxIterations: 5);
            var handle = await env.Client.StartWorkflowAsync(
                (AgentTurnWorkflow wf) => wf.RunAsync(input),
                new WorkflowOptions(id: "agent-turn-e2e-1", taskQueue: taskQueue));
            return await handle.GetResultAsync();
        });

        // The turn ran through the workflow: the mock's scripted reply is the final assistant message,
        // and the model was called exactly once (no tools).
        Assert.Equal("the durable answer is 42", conversation.LastAssistantContent);
        Assert.Equal(1, mock.CallCount);
        // The user's message reached the model through the activity boundary.
        Assert.Contains(mock.Calls[0], m => m.Text.Contains("what is the durable answer?"));
    }

    // ── durable HITL via signals (Rust: hitl_signals_e2e.rs) ─────────────────────────────────

    [SkippableFact]
    public async Task HitlGate_ApprovesAndDenies_ViaSignals()
    {
        Skip.IfNot(Enabled, "SMOOTH_AGENT_TEMPORAL_E2E not set");
        await using var env = await StartEnvOrSkipAsync();
        const string taskQueue = "smooth-operator-temporal-hitl-test";

        // One mock, scripted FIFO across BOTH (sequential) turns: approve then deny.
        var mock = new MockLlmProvider()
            .PushToolCall("call-approve", "echo", new Dictionary<string, object?> { ["text"] = "ran-after-approval" })
            .PushText("done after approval")
            .PushToolCall("call-deny", "echo", new Dictionary<string, object?> { ["text"] = "should-not-run" })
            .PushText("done after denial");
        var engine = new EngineHandles(mock, new[] { Echo() });

        using var worker = new TemporalWorker(env.Client, new TemporalWorkerOptions(taskQueue)
            .AddWorkflow<AgentTurnWorkflow>()
            .AddAllActivities(new AgentTurnActivities(engine)));

        var toolSchemas = new[] { ToolSchemaDto.From(Echo()) };
        var approveInput = new AgentTurnInput("You are a gated agent", "use echo (approve)", Tools: toolSchemas, MaxIterations: 5, ApprovalRequiredTools: new[] { "echo" });
        var denyInput = new AgentTurnInput("You are a gated agent", "use echo (deny)", Tools: toolSchemas, MaxIterations: 5, ApprovalRequiredTools: new[] { "echo" });

        var (approved, denied) = await worker.ExecuteAsync(async () =>
        {
            // Turn 1: APPROVE. The tool-call id is deterministic from the mock script; the signal
            // buffers, so the gate sees it approved when it checks.
            var approveHandle = await env.Client.StartWorkflowAsync(
                (AgentTurnWorkflow wf) => wf.RunAsync(approveInput),
                new WorkflowOptions(id: "hitl-approve", taskQueue: taskQueue));
            await approveHandle.SignalAsync(wf => wf.ApproveTool("call-approve"));
            var a = await approveHandle.GetResultAsync();

            // Turn 2: DENY.
            var denyHandle = await env.Client.StartWorkflowAsync(
                (AgentTurnWorkflow wf) => wf.RunAsync(denyInput),
                new WorkflowOptions(id: "hitl-deny", taskQueue: taskQueue));
            await denyHandle.SignalAsync(wf => wf.DenyTool("call-deny"));
            var d = await denyHandle.GetResultAsync();

            return (a, d);
        });

        // Approved turn: the gated tool actually ran, its real result is in the conversation.
        var approvedTools = approved.Messages.Where(m => m.Role == ChatRole.Tool.Value).ToList();
        var approvedTool = Assert.Single(approvedTools);
        Assert.Equal("ran-after-approval", approvedTool.Content);
        Assert.Equal("done after approval", approved.LastAssistantContent);

        // Denied turn: the tool NEVER ran — the tool message is a denial error, not the echo payload.
        var deniedTools = denied.Messages.Where(m => m.Role == ChatRole.Tool.Value).ToList();
        var deniedTool = Assert.Single(deniedTools);
        Assert.Contains("denied by human approval", deniedTool.Content);
        Assert.NotEqual("should-not-run", deniedTool.Content);
        Assert.Equal("done after denial", denied.LastAssistantContent);
    }

    // ── durable timer (Rust: durable_timer_e2e.rs) ───────────────────────────────────────────

    [SkippableFact]
    public async Task DurableWaitTool_SleepsOnATimer_ThenResumes()
    {
        Skip.IfNot(Enabled, "SMOOTH_AGENT_TEMPORAL_E2E not set");
        await using var env = await StartEnvOrSkipAsync();
        const string taskQueue = "smooth-operator-temporal-timer-test";

        // The model asks to wait 1 second, then wraps up. No tools registered — the wait is handled
        // by the workflow, not the tool registry/activity.
        var mock = new MockLlmProvider()
            .PushToolCall("call-wait", "wait", new Dictionary<string, object?> { ["seconds"] = 1 })
            .PushText("resumed after the timer");
        var engine = new EngineHandles(mock);

        using var worker = new TemporalWorker(env.Client, new TemporalWorkerOptions(taskQueue)
            .AddWorkflow<AgentTurnWorkflow>()
            .AddAllActivities(new AgentTurnActivities(engine)));

        var started = Stopwatch.StartNew();
        var conversation = await worker.ExecuteAsync(async () =>
        {
            var input = new AgentTurnInput("You are a self-pacing agent", "wait a moment, then answer", MaxIterations: 5, WaitTool: "wait");
            var handle = await env.Client.StartWorkflowAsync(
                (AgentTurnWorkflow wf) => wf.RunAsync(input),
                new WorkflowOptions(id: "durable-timer-1", taskQueue: taskQueue));
            return await handle.GetResultAsync();
        });
        started.Stop();

        var toolMsgs = conversation.Messages.Where(m => m.Role == ChatRole.Tool.Value).ToList();
        var toolMsg = Assert.Single(toolMsgs);
        Assert.Contains("durable timer", toolMsg.Content);
        Assert.Equal("resumed after the timer", conversation.LastAssistantContent);
        // It actually waited (the 1s timer elapsed), not skipped instantly.
        Assert.True(started.Elapsed >= TimeSpan.FromMilliseconds(900), $"turn returned too fast to have honored the timer: {started.Elapsed}");
    }
}
