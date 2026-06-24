namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Thrown when a <see cref="Workflow{TState}"/> is misconfigured (no entry, missing node) or
/// exceeds its step cap (an unbroken cycle). Mirrors the sibling engines' <c>WorkflowError</c>.
/// </summary>
public sealed class WorkflowException : Exception
{
    public WorkflowException(string message) : base(message)
    {
    }
}

/// <summary>
/// A LangGraph-inspired typed workflow graph with conditional edges. A <see cref="Workflow{TState}"/>
/// is a state machine: <b>nodes</b> transform a typed state value and <b>edges</b> — static or
/// conditional — determine the next node. The runner starts at the entry node, applies each node
/// then follows its outgoing edge, until it reaches a terminal node (an explicit end, or a node
/// with no outgoing edge), then returns the final state. A <c>maxSteps</c> cap bounds execution.
///
/// Standalone primitive — it does not touch the agent loop. The point is the seam: a multi-step
/// orchestration (parse → guardrails → retrieve → compose → …) drops in as a graph of named nodes
/// with the routing made explicit. Mirrors the sibling engines' <c>Workflow</c>.
/// </summary>
public sealed class Workflow<TState>
{
    /// <summary>Sentinel a conditional router returns to terminate the workflow.</summary>
    public const string End = "__end__";

    private readonly Dictionary<string, Func<TState, CancellationToken, Task<TState>>> _nodes = new(StringComparer.Ordinal);
    private readonly Dictionary<string, Edge> _edges = new(StringComparer.Ordinal);
    private readonly int _maxSteps;
    private string? _entry;

    public Workflow(int maxSteps = 100)
    {
        _maxSteps = maxSteps;
    }

    /// <summary>Register an async node under <paramref name="name"/>.</summary>
    public Workflow<TState> AddNode(string name, Func<TState, CancellationToken, Task<TState>> node)
    {
        _nodes[name] = node;
        return this;
    }

    /// <summary>Register a synchronous node under <paramref name="name"/>.</summary>
    public Workflow<TState> AddNode(string name, Func<TState, TState> node) =>
        AddNode(name, (state, _) => Task.FromResult(node(state)));

    /// <summary>Add a static edge <paramref name="from"/> → <paramref name="to"/>.</summary>
    public Workflow<TState> AddEdge(string from, string to)
    {
        _edges[from] = Edge.Static(to);
        return this;
    }

    /// <summary>
    /// Add a conditional edge whose <paramref name="router"/> picks the next node at runtime,
    /// returning the target node name or <see cref="End"/> to terminate.
    /// </summary>
    public Workflow<TState> AddConditionalEdge(string from, Func<TState, string> router)
    {
        _edges[from] = Edge.Conditional(router);
        return this;
    }

    /// <summary>Set the entry node (first to execute).</summary>
    public Workflow<TState> SetEntry(string name)
    {
        _entry = name;
        return this;
    }

    /// <summary>Mark <paramref name="from"/> terminal — reaching it ends the workflow.</summary>
    public Workflow<TState> SetEnd(string from)
    {
        _edges[from] = Edge.Terminal();
        return this;
    }

    /// <summary>
    /// Execute the workflow from the entry node, returning the final state. Throws
    /// <see cref="WorkflowException"/> if no entry was set, a referenced node does not exist, or
    /// the step cap is exceeded.
    /// </summary>
    public async Task<TState> RunAsync(TState initialState, CancellationToken cancellationToken = default)
    {
        if (_entry is null)
        {
            throw new WorkflowException("workflow has no entry node — call SetEntry()");
        }
        if (!_nodes.ContainsKey(_entry))
        {
            throw new WorkflowException($"entry node '{_entry}' not found in registered nodes");
        }

        var state = initialState;
        var current = _entry;

        for (var step = 0; step < _maxSteps; step++)
        {
            if (!_nodes.TryGetValue(current, out var node))
            {
                throw new WorkflowException($"node '{current}' not found in workflow");
            }

            state = await node(state, cancellationToken).ConfigureAwait(false);

            if (!_edges.TryGetValue(current, out var edge) || edge.Kind == EdgeKind.Terminal)
            {
                // No outgoing edge, or an explicit end — terminate.
                return state;
            }
            if (edge.Kind == EdgeKind.Conditional)
            {
                var target = edge.Router!(state);
                if (target == End)
                {
                    return state;
                }
                current = target;
            }
            else
            {
                current = edge.To!;
            }
        }

        throw new WorkflowException($"workflow exceeded maxSteps ({_maxSteps}) — possible infinite loop");
    }

    private enum EdgeKind
    {
        Static,
        Conditional,
        Terminal,
    }

    private sealed record Edge(EdgeKind Kind, string? To, Func<TState, string>? Router)
    {
        public static Edge Static(string to) => new(EdgeKind.Static, to, null);

        public static Edge Conditional(Func<TState, string> router) => new(EdgeKind.Conditional, null, router);

        public static Edge Terminal() => new(EdgeKind.Terminal, null, null);
    }
}
