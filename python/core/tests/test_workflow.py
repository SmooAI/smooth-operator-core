"""Tests for the typed workflow graph."""

from __future__ import annotations

import pytest

from smooth_operator_core.workflow import END, Workflow, WorkflowError


async def test_linear_workflow_runs_in_order() -> None:
    """A linear 3-node graph A → B → C transforms state start→end."""

    def append(name: str):
        def node(state: list[str]) -> list[str]:
            return [*state, name]

        return node

    wf = (
        Workflow[list[str]]()
        .add_node("a", append("a"))
        .add_node("b", append("b"))
        .add_node("c", append("c"))
        .add_edge("a", "b")
        .add_edge("b", "c")
        .set_entry("a")
        .set_end("c")
    )

    assert await wf.run([]) == ["a", "b", "c"]


async def test_conditional_edge_routes_both_branches() -> None:
    """A conditional edge routes to different nodes based on state."""

    def start(state: dict[str, int]) -> dict[str, int]:
        return state

    def left(state: dict[str, int]) -> dict[str, int]:
        return {**state, "branch": -1}

    def right(state: dict[str, int]) -> dict[str, int]:
        return {**state, "branch": 1}

    def build() -> Workflow[dict[str, int]]:
        return (
            Workflow[dict[str, int]]()
            .add_node("start", start)
            .add_node("left", left)
            .add_node("right", right)
            .add_conditional_edge("start", lambda s: "right" if s["n"] > 0 else "left")
            .set_entry("start")
            .set_end("left")
            .set_end("right")
        )

    assert (await build().run({"n": 5}))["branch"] == 1
    assert (await build().run({"n": -5}))["branch"] == -1


async def test_async_node_is_awaited() -> None:
    """Nodes may be async coroutines; the runner awaits them."""

    async def add_ten(state: int) -> int:
        return state + 10

    async def double(state: int) -> int:
        return state * 2

    wf = (
        Workflow[int]()
        .add_node("add_ten", add_ten)
        .add_node("double", double)
        .add_edge("add_ten", "double")
        .set_entry("add_ten")
        .set_end("double")
    )

    assert await wf.run(5) == 30  # (5 + 10) * 2


async def test_router_can_return_end_sentinel() -> None:
    """A conditional router returning END terminates the workflow."""
    wf = (
        Workflow[int]().add_node("only", lambda s: s + 1).add_conditional_edge("only", lambda _s: END).set_entry("only")
    )
    assert await wf.run(0) == 1


async def test_node_with_no_outgoing_edge_is_implicit_end() -> None:
    """A node with no registered outgoing edge ends the workflow."""
    wf = Workflow[int]().add_node("only", lambda s: s + 1).set_entry("only")
    assert await wf.run(0) == 1


async def test_max_steps_cap_triggers_on_cycle() -> None:
    """An unbroken cycle hits the max_steps cap and raises."""
    wf = (
        Workflow[list[str]](max_steps=6)
        .add_node("a", lambda s: [*s, "a"])
        .add_node("b", lambda s: [*s, "b"])
        .add_edge("a", "b")
        .add_edge("b", "a")
        .set_entry("a")
    )
    with pytest.raises(WorkflowError, match="max_steps"):
        await wf.run([])


async def test_missing_entry_node_raises() -> None:
    """Running without an entry node raises a WorkflowError."""
    with pytest.raises(WorkflowError, match="no entry node"):
        await Workflow[int]().run(0)


async def test_unknown_entry_node_raises() -> None:
    """An entry node that was never registered raises."""
    with pytest.raises(WorkflowError, match="not found"):
        await Workflow[int]().set_entry("ghost").run(0)


async def test_edge_to_missing_node_raises() -> None:
    """An edge pointing at an unregistered node raises mid-run."""
    wf = Workflow[int]().add_node("a", lambda s: s).add_edge("a", "ghost").set_entry("a")
    with pytest.raises(WorkflowError, match="not found"):
        await wf.run(0)
