using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// A point-in-time snapshot of a conversation, so a turn that crashes (or a long agentic
/// loop) can be resumed instead of restarted. Mirrors the Rust engine's <c>Checkpoint</c>.
/// </summary>
public sealed record Checkpoint(
    string Id,
    string ThreadId,
    IReadOnlyList<ChatMessage> Messages,
    int Iteration,
    DateTimeOffset CreatedAt,
    IReadOnlyDictionary<string, string>? Metadata = null);

/// <summary>When the agent writes a checkpoint during a run. Mirrors the Rust <c>CheckpointStrategy</c>.</summary>
public enum CheckpointStrategy
{
    /// <summary>Never checkpoint.</summary>
    Never,

    /// <summary>Checkpoint after every LLM response (each loop iteration).</summary>
    AfterEachIteration,

    /// <summary>Checkpoint only after a tool call is executed (the expensive, side-effecting points).</summary>
    AfterToolCall,
}

/// <summary>
/// Pluggable persistence for <see cref="Checkpoint"/>s, keyed by thread. Mirrors the Rust
/// <c>CheckpointStore</c> trait. The bundled <see cref="InMemoryCheckpointStore"/> is for tests
/// and single-process use; file/SQLite/Postgres adapters arrive in later phases.
/// </summary>
public interface ICheckpointStore
{
    Task SaveAsync(Checkpoint checkpoint, CancellationToken cancellationToken = default);

    /// <summary>The most recent checkpoint for a thread, or null if none.</summary>
    Task<Checkpoint?> LoadLatestAsync(string threadId, CancellationToken cancellationToken = default);

    /// <summary>All checkpoints for a thread, oldest first.</summary>
    Task<IReadOnlyList<Checkpoint>> ListAsync(string threadId, CancellationToken cancellationToken = default);

    /// <summary>Keep only the newest <paramref name="keep"/> checkpoints for a thread; returns how many were removed.</summary>
    Task<int> PruneAsync(string threadId, int keep, CancellationToken cancellationToken = default);
}

/// <summary>In-process <see cref="ICheckpointStore"/>. The C# analog of the Rust <c>MemoryCheckpointStore</c>.</summary>
public sealed class InMemoryCheckpointStore : ICheckpointStore
{
    private readonly object _gate = new();
    private readonly List<Checkpoint> _checkpoints = new();

    public Task SaveAsync(Checkpoint checkpoint, CancellationToken cancellationToken = default)
    {
        lock (_gate)
        {
            _checkpoints.Add(checkpoint);
        }
        return Task.CompletedTask;
    }

    public Task<Checkpoint?> LoadLatestAsync(string threadId, CancellationToken cancellationToken = default)
    {
        lock (_gate)
        {
            // Insertion order is the source of truth for "latest" (deterministic even when
            // CreatedAt ties within a tick).
            for (var i = _checkpoints.Count - 1; i >= 0; i--)
            {
                if (_checkpoints[i].ThreadId == threadId)
                {
                    return Task.FromResult<Checkpoint?>(_checkpoints[i]);
                }
            }
            return Task.FromResult<Checkpoint?>(null);
        }
    }

    public Task<IReadOnlyList<Checkpoint>> ListAsync(string threadId, CancellationToken cancellationToken = default)
    {
        lock (_gate)
        {
            IReadOnlyList<Checkpoint> result = _checkpoints.Where(c => c.ThreadId == threadId).ToList();
            return Task.FromResult(result);
        }
    }

    public Task<int> PruneAsync(string threadId, int keep, CancellationToken cancellationToken = default)
    {
        lock (_gate)
        {
            var forThread = _checkpoints.Where(c => c.ThreadId == threadId).ToList();
            var removeCount = Math.Max(0, forThread.Count - keep);
            for (var i = 0; i < removeCount; i++)
            {
                _checkpoints.Remove(forThread[i]); // oldest first
            }
            return Task.FromResult(removeCount);
        }
    }
}
