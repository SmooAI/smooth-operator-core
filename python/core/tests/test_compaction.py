"""Unit tests for token-aware conversation compaction."""

from __future__ import annotations

from smooth_operator_core.compaction import compact, estimate_tokens


def _msg(role: str, content: str) -> dict:
    return {"role": role, "content": content}


def test_under_budget_is_unchanged():
    msgs = [_msg("system", "sys"), _msg("user", "hi"), _msg("assistant", "hello")]
    assert compact(msgs, 8000) == msgs


def test_disabled_when_budget_non_positive():
    msgs = [_msg("user", "x" * 10_000)]
    assert compact(msgs, 0) == msgs


def test_drops_oldest_keeps_system_and_recent():
    big = "word " * 200  # ~250 tokens each
    msgs = [
        _msg("system", "you are helpful"),
        _msg("user", "OLDEST " + big),
        _msg("assistant", "old reply " + big),
        _msg("user", "MIDDLE " + big),
        _msg("assistant", "mid reply " + big),
        _msg("user", "NEWEST question"),
    ]
    out = compact(msgs, 400)
    # System is always kept.
    assert out[0]["role"] == "system"
    # The newest message survives; the oldest is dropped.
    contents = " ".join(m["content"] for m in out)
    assert "NEWEST question" in contents
    assert "OLDEST" not in contents
    # Result fits the budget.
    assert sum(estimate_tokens(m) for m in out) <= 400


def test_never_starts_window_on_orphan_tool_message():
    big = "token " * 300
    msgs = [
        _msg("system", "sys"),
        _msg("user", "q " + big),
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [{"id": "c1", "function": {"name": "t", "arguments": "{}"}}],
        },
        {"role": "tool", "tool_call_id": "c1", "content": "result " + big},
        _msg("assistant", "final answer"),
    ]
    out = compact(msgs, 200)
    # The kept non-system window must not begin with a tool message.
    non_system = [m for m in out if m["role"] != "system"]
    assert non_system, "expected at least one non-system message kept"
    assert non_system[0]["role"] != "tool"


def test_drops_every_result_of_a_trimmed_parallel_tool_group():
    # SMOODEV-3704: the cut lands inside a group of parallel tool results.
    big = "token " * 300
    msgs = [
        _msg("system", "sys"),
        _msg("user", "q"),
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [{"id": i, "function": {"name": "t", "arguments": "{}"}} for i in ("p1", "p2", "p3")],
        },
        {"role": "tool", "tool_call_id": "p1", "content": "result " + big},
        {"role": "tool", "tool_call_id": "p2", "content": "two"},
        {"role": "tool", "tool_call_id": "p3", "content": "three"},
        _msg("assistant", "final answer"),
    ]
    out = compact(msgs, 50)
    announced = {tc["id"] for m in out for tc in m.get("tool_calls", [])}
    assert all(m["tool_call_id"] in announced for m in out if m["role"] == "tool")
    assert out[-1]["content"] == "final answer"
