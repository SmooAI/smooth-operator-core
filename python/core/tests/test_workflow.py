"""Tests for the typed workflow graph."""

from __future__ import annotations

import pytest

from smooth_operator_core.workflow import END, Workflow, WorkflowError, sub_workflow_node


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


def _append(name: str):
    def node(state: list[str]) -> list[str]:
        return [*state, name]

    return node


def _two_node_child() -> Workflow[list[str]]:
    """Two tracking nodes joined by a conditional edge that skips a third."""
    return (
        Workflow[list[str]]()
        .add_node("child_a", _append("child_a"))
        .add_node("child_b", _append("child_b"))
        .add_node("child_never", _append("child_never"))
        .add_conditional_edge("child_a", lambda s: "child_b" if "child_a" in s else "child_never")
        .set_entry("child_a")
        .set_end("child_b")
    )


def _identity(state: list[str]) -> list[str]:
    return state


def _take_child(_parent: list[str], child: list[str]) -> list[str]:
    return child


async def test_sub_workflow_runs_to_completion_in_one_step() -> None:
    """A sub-workflow's 2 nodes + conditional edge all run inside one parent step."""
    wf = (
        Workflow[list[str]]()
        .add_node("parent_a", _append("parent_a"))
        .add_node("sub", sub_workflow_node(_two_node_child(), _identity, _take_child))
        .add_node("parent_b", _append("parent_b"))
        .add_edge("parent_a", "sub")
        .add_edge("sub", "parent_b")
        .set_entry("parent_a")
        .set_end("parent_b")
    )

    assert await wf.run([]) == ["parent_a", "child_a", "child_b", "parent_b"]


async def test_sub_workflow_state_maps_in_and_out() -> None:
    """Parent and child hold different state types — map in, map out."""
    child = (
        Workflow[int]()
        .add_node("add_ten", lambda n: n + 10)
        .add_node("double", lambda n: n * 2)
        .add_edge("add_ten", "double")
        .set_entry("add_ten")
        .set_end("double")
    )

    # Parent state is a labelled total; the child only ever sees the int.
    wf = (
        Workflow[tuple[str, int]]()
        .add_node(
            "math",
            sub_workflow_node(
                child,
                lambda p: p[1],
                lambda p, total: (f"{p[0]}:done", total),
            ),
        )
        .set_entry("math")
        .set_end("math")
    )

    assert await wf.run(("start", 5)) == ("start:done", 30)


async def test_sub_workflow_error_propagates_to_parent() -> None:
    """A failing child node aborts the parent run."""

    def boom(_state: list[str]) -> list[str]:
        raise RuntimeError("child exploded")

    child = Workflow[list[str]]().add_node("boom", boom).set_entry("boom").set_end("boom")

    wf = (
        Workflow[list[str]]()
        .add_node("parent_a", _append("parent_a"))
        .add_node("sub", sub_workflow_node(child, _identity, _take_child))
        .add_edge("parent_a", "sub")
        .set_entry("parent_a")
        .set_end("sub")
    )

    with pytest.raises(RuntimeError, match="child exploded"):
        await wf.run([])


async def test_sub_workflow_nesting_depth_two() -> None:
    """Nesting deeper than one level — parent → child → grandchild."""
    grandchild = (
        Workflow[list[str]]()
        .add_node("grand_a", _append("grand_a"))
        .add_node("grand_b", _append("grand_b"))
        .add_edge("grand_a", "grand_b")
        .set_entry("grand_a")
        .set_end("grand_b")
    )

    child = (
        Workflow[list[str]]()
        .add_node("child_a", _append("child_a"))
        .add_node("grand", sub_workflow_node(grandchild, _identity, _take_child))
        .add_edge("child_a", "grand")
        .set_entry("child_a")
        .set_end("grand")
    )

    wf = (
        Workflow[list[str]]()
        .add_node("parent_a", _append("parent_a"))
        .add_node("sub", sub_workflow_node(child, _identity, _take_child))
        .add_edge("parent_a", "sub")
        .set_entry("parent_a")
        .set_end("sub")
    )

    assert await wf.run([]) == ["parent_a", "child_a", "grand_a", "grand_b"]
