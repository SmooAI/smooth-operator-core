using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// A conversation thread that carries message history across <c>SmoothAgent.RunAsync</c>
/// calls — the engine's analog of the Rust core's persisted <c>Conversation</c>, named after
/// Microsoft Agent Framework's <c>AgentThread</c>. Hold one per user conversation and pass it
/// to each run; the agent appends the new user/assistant/tool messages to it so the next turn
/// has the full context. The system prompt is supplied per-run from
/// <see cref="AgentOptions.Instructions"/> and is never stored here.
/// </summary>
public sealed class SmoothAgentThread
{
    private readonly List<ChatMessage> _messages = new();

    /// <summary>Create a thread. Pass an existing id to resume one (e.g. from a checkpoint).</summary>
    public SmoothAgentThread(string? id = null)
    {
        Id = string.IsNullOrEmpty(id) ? Guid.NewGuid().ToString("n") : id;
    }

    /// <summary>Stable id for this conversation — the key checkpoints are stored under.</summary>
    public string Id { get; }

    /// <summary>The accumulated history, oldest first (no system prompt).</summary>
    public IReadOnlyList<ChatMessage> Messages => _messages;

    /// <summary>Number of messages currently in the thread.</summary>
    public int Count => _messages.Count;

    /// <summary>Append a message to the thread.</summary>
    public void Add(ChatMessage message) => _messages.Add(message);

    /// <summary>Append a range of messages to the thread.</summary>
    public void AddRange(IEnumerable<ChatMessage> messages) => _messages.AddRange(messages);

    internal List<ChatMessage> Mutable => _messages;
}
