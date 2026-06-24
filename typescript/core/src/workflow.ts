/**
 * LangGraph-inspired typed workflow graph with conditional edges.
 *
 * Phase-3 sibling of the reference engine's workflow primitive. A `Workflow<S>`
 * is a state machine: **nodes** transform a typed state value and **edges** —
 * static or **conditional** — determine the next node to execute. The runner
 * starts at the entry node, applies each node then follows its outgoing edge,
 * until it reaches the `END` sentinel (or a node with no outgoing edge), then
 * returns the final state.
 *
 * Nodes may be sync or async; the runner awaits their results. A `maxSteps` cap
 * bounds execution so an intentional or accidental cycle can't loop forever.
 *
 * Standalone module — it does not touch the agent loop. The point is the seam: a
 * multi-step orchestration (parse → guardrails → retrieve → compose → …) drops in
 * as a graph of named nodes with the routing made explicit.
 */

/** Sentinel a conditional router can return to signal termination. */
export const END = '__end__' as const;

/** A node transforms state into a new state; may be sync or async. */
export type NodeFn<S> = (state: S) => S | Promise<S>;

/** A conditional router inspects state and returns the next node name (or `END`). */
export type Router<S> = (state: S) => string;

/** Thrown when a workflow is misconfigured or exceeds its step limit. */
export class WorkflowError extends Error {
    constructor(message: string) {
        super(message);
        this.name = 'WorkflowError';
    }
}

// An edge is either a static target node name, a conditional router, or END.
type Edge<S> = { kind: 'node'; to: string } | { kind: 'conditional'; router: Router<S> } | { kind: 'end' };

/**
 * A typed workflow graph: named nodes connected by static/conditional edges.
 *
 * Build with `addNode`, `addEdge` / `addConditionalEdge`, `setEntry`, and
 * `setEnd`; the builder methods return `this` so they chain. `run` executes the
 * graph from the entry node.
 */
export class Workflow<S> {
    private readonly nodes = new Map<string, NodeFn<S>>();
    private readonly edges = new Map<string, Edge<S>>();
    private entry: string | undefined;

    constructor(private readonly maxSteps = 100) {}

    /** Register a node `func` under `name` (used to reference it in edges). */
    addNode(name: string, func: NodeFn<S>): this {
        this.nodes.set(name, func);
        return this;
    }

    /** Add a static edge `from` → `to`. */
    addEdge(from: string, to: string): this {
        this.edges.set(from, { kind: 'node', to });
        return this;
    }

    /**
     * Add a conditional edge whose `router` picks the next node at runtime. The
     * router returns the target node name, or `END` to terminate the workflow.
     */
    addConditionalEdge(from: string, router: Router<S>): this {
        this.edges.set(from, { kind: 'conditional', router });
        return this;
    }

    /** Set the entry node (first node to execute). */
    setEntry(name: string): this {
        this.entry = name;
        return this;
    }

    /** Mark `from` as terminal — reaching it ends the workflow. */
    setEnd(from: string): this {
        this.edges.set(from, { kind: 'end' });
        return this;
    }

    /**
     * Execute the workflow from the entry node, returning the final state.
     *
     * Throws {@link WorkflowError} if no entry node was set, a referenced node
     * does not exist, or the `maxSteps` cap is exceeded (e.g. an unbroken cycle).
     */
    async run(initialState: S): Promise<S> {
        if (this.entry === undefined) {
            throw new WorkflowError('workflow has no entry node — call setEntry()');
        }
        if (!this.nodes.has(this.entry)) {
            throw new WorkflowError(`entry node '${this.entry}' not found in registered nodes`);
        }

        let state = initialState;
        let current = this.entry;

        for (let step = 0; step < this.maxSteps; step++) {
            const node = this.nodes.get(current);
            if (node === undefined) {
                throw new WorkflowError(`node '${current}' not found in workflow`);
            }

            state = await node(state);

            const edge = this.edges.get(current);
            if (edge === undefined || edge.kind === 'end') {
                // No outgoing edge, or an explicit END — terminate.
                return state;
            }
            if (edge.kind === 'conditional') {
                const target = edge.router(state);
                if (target === END) return state;
                current = target;
            } else {
                current = edge.to;
            }
        }

        throw new WorkflowError(`workflow exceeded maxSteps (${this.maxSteps}) — possible infinite loop`);
    }
}
