"""LangGraph-inspired typed workflow graph with conditional edges.

Phase-3 sibling of the reference engine's workflow primitive. A :class:`Workflow`
is a state machine: **nodes** transform a typed state value and **edges** — static
or **conditional** — determine the next node to execute. The runner starts at the
entry node, applies each node then follows its outgoing edge, until it reaches an
``END`` sentinel (or a node with no outgoing edge), then returns the final state.

Nodes may be sync or async; the runner awaits coroutine results. A ``max_steps``
cap bounds execution so an intentional or accidental cycle can't loop forever.

This is a standalone module — it does not touch the agent loop. The point is the
seam: a multi-step orchestration (parse → guardrails → retrieve → compose → …)
drops in as a graph of named nodes with the routing made explicit.
"""

from __future__ import annotations

import inspect
from collections.abc import Awaitable
from typing import Callable, Generic, TypeVar, Union

S = TypeVar("S")

# Sentinel a conditional router can return to signal termination.
END: str = "__end__"

# A node transforms state into a new state; it may return a value or a coroutine.
NodeFn = Callable[[S], Union[S, Awaitable[S]]]

# A conditional router inspects the current state and returns the next node name
# (or ``END`` to terminate).
Router = Callable[[S], str]


class WorkflowError(Exception):
    """Raised when a workflow is misconfigured or exceeds its step limit."""


class Workflow(Generic[S]):
    """A typed workflow graph: named nodes connected by static/conditional edges.

    Build with :meth:`add_node`, :meth:`add_edge` / :meth:`add_conditional_edge`,
    :meth:`set_entry`, and :meth:`set_end`; the builder methods return ``self`` so
    they chain. :meth:`run` executes the graph from the entry node.
    """

    def __init__(self, max_steps: int = 100) -> None:
        self._nodes: dict[str, NodeFn[S]] = {}
        # An edge is either a node name (static) or a Router (conditional). ``END``
        # marks a terminal node.
        self._edges: dict[str, Union[str, Router[S]]] = {}
        self._entry: str | None = None
        self._max_steps = max_steps

    def add_node(self, name: str, func: NodeFn[S]) -> Workflow[S]:
        """Register a node ``func`` under ``name`` (used to reference it in edges)."""
        self._nodes[name] = func
        return self

    def add_edge(self, from_node: str, to_node: str) -> Workflow[S]:
        """Add a static edge ``from_node`` → ``to_node``."""
        self._edges[from_node] = to_node
        return self

    def add_conditional_edge(self, from_node: str, router: Router[S]) -> Workflow[S]:
        """Add a conditional edge whose ``router`` picks the next node at runtime.

        The router receives the current state and returns the target node name, or
        ``END`` to terminate the workflow.
        """
        self._edges[from_node] = router
        return self

    def set_entry(self, name: str) -> Workflow[S]:
        """Set the entry node (first node to execute)."""
        self._entry = name
        return self

    def set_end(self, from_node: str) -> Workflow[S]:
        """Mark ``from_node`` as terminal — reaching it ends the workflow."""
        self._edges[from_node] = END
        return self

    async def run(self, initial_state: S) -> S:
        """Execute the workflow from the entry node, returning the final state.

        Raises :class:`WorkflowError` if no entry node was set, a referenced node
        does not exist, or the ``max_steps`` cap is exceeded (e.g. an unbroken
        cycle).
        """
        if self._entry is None:
            raise WorkflowError("workflow has no entry node — call set_entry()")
        if self._entry not in self._nodes:
            raise WorkflowError(f"entry node '{self._entry}' not found in registered nodes")

        state = initial_state
        current = self._entry

        for _ in range(self._max_steps):
            node = self._nodes.get(current)
            if node is None:
                raise WorkflowError(f"node '{current}' not found in workflow")

            result = node(state)
            state = await result if inspect.isawaitable(result) else result

            edge = self._edges.get(current)
            if edge is None:
                # No outgoing edge — implicit end.
                return state
            if edge == END:
                return state
            if callable(edge):
                target = edge(state)
                if target == END:
                    return state
                current = target
            else:
                current = edge

        raise WorkflowError(f"workflow exceeded max_steps ({self._max_steps}) — possible infinite loop")
