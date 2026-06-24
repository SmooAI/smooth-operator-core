package core

import (
	"context"
	"fmt"
)

// LangGraph-inspired typed workflow graph with conditional edges.
//
// Phase-3 sibling of the reference engine's workflow primitive. A Workflow[S] is
// a state machine: nodes transform a typed state value and edges — static or
// conditional — determine the next node to execute. The runner starts at the
// entry node, applies each node then follows its outgoing edge, until it reaches
// the END sentinel (or a node with no outgoing edge), then returns the final
// state. A maxSteps cap bounds execution so an intentional or accidental cycle
// can't loop forever.
//
// Standalone — it does not touch the agent loop. The point is the seam: a
// multi-step orchestration (parse → guardrails → retrieve → compose → …) drops in
// as a graph of named nodes with the routing made explicit.

// END is the sentinel a conditional router returns to signal termination.
const END = "__end__"

// NodeFn transforms state into a new state. It receives a context so nodes can do
// async/IO work, and may return an error to abort the run.
type NodeFn[S any] func(ctx context.Context, state S) (S, error)

// Router inspects the current state and returns the next node name (or END).
type Router[S any] func(state S) string

// edgeKind discriminates the edge variants.
type edgeKind int

const (
	edgeNode edgeKind = iota
	edgeConditional
	edgeEnd
)

type edge[S any] struct {
	kind   edgeKind
	to     string
	router Router[S]
}

// Workflow is a typed workflow graph: named nodes connected by static/conditional
// edges. Build with AddNode, AddEdge / AddConditionalEdge, SetEntry, and SetEnd;
// the builder methods return the receiver so they chain. Run executes the graph.
type Workflow[S any] struct {
	nodes    map[string]NodeFn[S]
	edges    map[string]edge[S]
	entry    string
	hasEntry bool
	maxSteps int
}

// NewWorkflow creates an empty workflow with the given max-steps cap (use a value
// <= 0 for the default of 100).
func NewWorkflow[S any](maxSteps int) *Workflow[S] {
	if maxSteps <= 0 {
		maxSteps = 100
	}
	return &Workflow[S]{
		nodes:    map[string]NodeFn[S]{},
		edges:    map[string]edge[S]{},
		maxSteps: maxSteps,
	}
}

// AddNode registers a node under name (used to reference it in edges).
func (w *Workflow[S]) AddNode(name string, fn NodeFn[S]) *Workflow[S] {
	w.nodes[name] = fn
	return w
}

// AddEdge adds a static edge from → to.
func (w *Workflow[S]) AddEdge(from, to string) *Workflow[S] {
	w.edges[from] = edge[S]{kind: edgeNode, to: to}
	return w
}

// AddConditionalEdge adds a conditional edge whose router picks the next node at
// runtime. The router returns the target node name, or END to terminate.
func (w *Workflow[S]) AddConditionalEdge(from string, router Router[S]) *Workflow[S] {
	w.edges[from] = edge[S]{kind: edgeConditional, router: router}
	return w
}

// SetEntry sets the entry node (first node to execute).
func (w *Workflow[S]) SetEntry(name string) *Workflow[S] {
	w.entry = name
	w.hasEntry = true
	return w
}

// SetEnd marks from as terminal — reaching it ends the workflow.
func (w *Workflow[S]) SetEnd(from string) *Workflow[S] {
	w.edges[from] = edge[S]{kind: edgeEnd}
	return w
}

// Run executes the workflow from the entry node, returning the final state.
//
// It returns an error if no entry node was set, a referenced node does not exist,
// a node fails, or the maxSteps cap is exceeded (e.g. an unbroken cycle).
func (w *Workflow[S]) Run(ctx context.Context, initialState S) (S, error) {
	var zero S
	if !w.hasEntry {
		return zero, fmt.Errorf("workflow has no entry node — call SetEntry()")
	}
	if _, ok := w.nodes[w.entry]; !ok {
		return zero, fmt.Errorf("entry node %q not found in registered nodes", w.entry)
	}

	state := initialState
	current := w.entry

	for step := 0; step < w.maxSteps; step++ {
		node, ok := w.nodes[current]
		if !ok {
			return zero, fmt.Errorf("node %q not found in workflow", current)
		}

		next, err := node(ctx, state)
		if err != nil {
			return zero, fmt.Errorf("node %q failed: %w", current, err)
		}
		state = next

		e, ok := w.edges[current]
		if !ok || e.kind == edgeEnd {
			// No outgoing edge, or an explicit END — terminate.
			return state, nil
		}
		if e.kind == edgeConditional {
			target := e.router(state)
			if target == END {
				return state, nil
			}
			current = target
			continue
		}
		current = e.to
	}

	return zero, fmt.Errorf("workflow exceeded maxSteps (%d) — possible infinite loop", w.maxSteps)
}
