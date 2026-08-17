---
'@smooai/smooth-operator-core': minor
---

feat(rust,go,ts,python,dotnet): sub-workflows that run to completion inside one turn

The typed `Workflow` graph has always had two speeds. Standalone, `run()` executes
the whole graph. But when a workflow drives a **conversation**, the driver advances
it turn-by-turn — one node per user turn — so a five-node graph costs five turns
even when four of them are pure computation the user never needs to see.

This adds the composition primitive for the other half: a **sub-workflow node**
that wraps a child `Workflow` and runs it to completion — every node, its
conditional edges, and the `__end__` sentinel — inside ONE parent step. The
top-level graph stays turn-gated exactly as before; sub-workflows are purely
additive, and nothing about the existing turn-by-turn behavior changes.

Typed state composes across the boundary: `map_in` projects parent state into the
child's state type (they need not be the same type), `map_out` folds the child's
final state back. An error from any child node propagates out of the parent's
`run` — a failed sub-graph fails the turn, it does not silently return a partial
state. Sub-workflows nest, because a sub-workflow node is just a node: a child may
itself hold one.

Same semantics, same test set, in all five engines:

- **Rust** — `sub_workflow_node(name, child, map_in, map_out) -> FnNode<P>`
- **Go** — `SubWorkflowNode[P, C](child, mapIn, mapOut) NodeFn[P]`
- **TypeScript** — `subWorkflowNode<P, C>(child, mapIn, mapOut): NodeFn<P>`
- **Python** — `sub_workflow_node(child, map_in, map_out)`
- **.NET** — `Workflow.SubWorkflowNode<TParent, TChild>(child, mapIn, mapOut)`

It reuses each engine's existing node seam rather than adding a parallel runner, so
a sub-workflow is indistinguishable from any other node to the graph around it —
which is what makes the nesting fall out for free.
