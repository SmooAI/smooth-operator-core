"""Unit tests for the multi-agent cast: clearance semantics and agent enforcement."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import (
    AgentOptions,
    Cast,
    Clearance,
    FunctionTool,
    OperatorRole,
    RoleKind,
    SmoothAgent,
)


# ── Clearance semantics ──────────────────────────────────────────────────────
def test_empty_clearance_allows_all_tools():
    c = Clearance.allow_all()
    assert c.is_allowed("anything") is True
    assert c.is_allowed("other") is True


def test_deny_everything_blocks_all():
    c = Clearance.deny_all()
    assert c.is_allowed("anything") is False
    # Even a tool on the allow-list is blocked when deny_everything is set.
    assert Clearance(allow_tools=frozenset({"x"}), deny_everything=True).is_allowed("x") is False


def test_allow_list_is_a_whitelist():
    c = Clearance.allow("read", "search")
    assert c.is_allowed("read") is True
    assert c.is_allowed("search") is True
    assert c.is_allowed("write") is False


def test_deny_always_wins_over_allow():
    # A tool that is both allowed and denied is denied — deny wins.
    c = Clearance(allow_tools=frozenset({"read", "write"}), deny_tools=frozenset({"write"}))
    assert c.is_allowed("read") is True
    assert c.is_allowed("write") is False


def test_deny_list_with_empty_allow_blocks_only_denied():
    c = Clearance.deny("delete")
    assert c.is_allowed("delete") is False
    assert c.is_allowed("read") is True


# ── Cast registry ────────────────────────────────────────────────────────────
def test_cast_registers_and_lists_roles():
    cast = Cast()
    lead = OperatorRole(name="lead", kind=RoleKind.LEAD, instructions="orchestrate")
    sk = OperatorRole(name="researcher", kind=RoleKind.SIDEKICK, instructions="research")
    shadow = OperatorRole(name="critic", kind=RoleKind.SHADOW, instructions="observe", hidden=True)
    cast.register(lead).register(sk).register(shadow)

    assert cast.count == 3
    assert cast.is_empty is False
    assert cast.get("researcher") is sk
    assert cast.get("missing") is None
    # Sidekicks filters by kind.
    assert cast.sidekicks() == [sk]
    # Hidden roles are excluded from the visible listing but still gettable.
    visible_names = {r.name for r in cast.list_visible()}
    assert visible_names == {"lead", "researcher"}


def test_role_defaults_to_allow_all_clearance():
    role = OperatorRole(name="lead", kind=RoleKind.LEAD)
    assert role.permissions.is_allowed("any-tool") is True
    assert role.max_iterations == 8


# ── Agent enforcement ────────────────────────────────────────────────────────
def _tool_call(call_id: str, name: str, arguments: str):
    return SimpleNamespace(id=call_id, function=SimpleNamespace(name=name, arguments=arguments))


def _msg(content=None, tool_calls=None):
    return SimpleNamespace(content=content, tool_calls=tool_calls)


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        return SimpleNamespace(choices=[SimpleNamespace(message=self._scripted.pop(0))])


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


def _spy_tool(name: str, calls: list[str]):
    async def _run(args):
        calls.append(name)
        return f"{name} ran"

    return FunctionTool(
        name=name,
        description=f"the {name} tool",
        parameters={"type": "object", "properties": {}},
        func=_run,
    )


@pytest.mark.asyncio
async def test_forbidden_tool_is_not_executed():
    executed: list[str] = []
    write = _spy_tool("write", executed)
    # Model asks for the forbidden tool, then answers after seeing the denial.
    client = FakeClient(
        [
            _msg(content=None, tool_calls=[_tool_call("c1", "write", "{}")]),
            _msg(content="ok, I won't write"),
        ]
    )
    agent = SmoothAgent(client, AgentOptions(tools=[write], clearance=Clearance.deny("write")))
    result = await agent.run("please write")

    assert result.text == "ok, I won't write"
    assert result.tool_calls == 1  # the call was counted...
    assert executed == []  # ...but the tool body never ran.
    # The model was told the tool isn't permitted (fed back as a tool result).
    second_call_messages = client.chat.completions.calls[1]["messages"]
    assert any(m.get("role") == "tool" and "not permitted" in m.get("content", "") for m in second_call_messages)


@pytest.mark.asyncio
async def test_allowed_tool_still_runs_under_clearance():
    executed: list[str] = []
    read = _spy_tool("read", executed)
    client = FakeClient(
        [
            _msg(content=None, tool_calls=[_tool_call("c1", "read", "{}")]),
            _msg(content="done"),
        ]
    )
    # Whitelist allows "read"; the tool should execute normally.
    agent = SmoothAgent(client, AgentOptions(tools=[read], clearance=Clearance.allow("read")))
    result = await agent.run("please read")

    assert result.text == "done"
    assert executed == ["read"]


@pytest.mark.asyncio
async def test_no_clearance_allows_every_tool():
    executed: list[str] = []
    write = _spy_tool("write", executed)
    client = FakeClient(
        [
            _msg(content=None, tool_calls=[_tool_call("c1", "write", "{}")]),
            _msg(content="done"),
        ]
    )
    agent = SmoothAgent(client, AgentOptions(tools=[write]))  # no clearance
    await agent.run("please write")
    assert executed == ["write"]
