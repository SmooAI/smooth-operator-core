---
'@smooai/smooth-operator-core': patch
---

docs(rust,go,ts,python,dotnet): state that sub-workflows are first-class graph vertices

Follow-up to the sub-workflow primitive. The contract already held — each engine's
`sub_workflow_node` returns that engine's own node type, so `add_edge` /
`add_conditional_edge` cannot tell a sub-workflow apart from a plain node — but
nothing said so, and no test proved it.

Now documented and covered: plain nodes and sub-workflows are **interchangeable
vertices of one composite graph**. A sub-workflow works as an edge source and as
an edge target alike, including as the target of a conditional edge and as the
node a conditional router leaves (terminate sentinel included), and the nesting is
arbitrary — a sub-workflow may itself contain sub-workflows.

Each engine gains the same composite-graph test: a parent wiring
`plain --conditional--> sub-workflow --conditional--> plain`, where that
sub-workflow's own vertices are a plain node plus a further nested sub-workflow
(depth 2), all running inside one parent step.

No behavior change — tests and doc comments only.
