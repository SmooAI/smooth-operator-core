"""Tests for the ``tool_search`` meta-tool and deferred-tool promotion.

Proves the Phase-3 behaviour: a deferred tool's schema is NOT sent to the model
until it's promoted; ``tool_search`` fuzzy-matches the query and promotes matches;
a promoted tool is then dispatchable; an unpromoted deferred tool is not.
"""

from __future__ import annotations

import json
from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, FunctionTool, SmoothAgent, ToolSearch


# ── fake openai client surface (mirrors test_agent.py) ───────────────────────
def _msg(content=None, tool_calls=None):
    return SimpleNamespace(content=content, tool_calls=tool_calls)


def _tool_call(call_id: str, name: str, arguments: str):
    return SimpleNamespace(id=call_id, function=SimpleNamespace(name=name, arguments=arguments))


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        message = self._scripted.pop(0)
        return SimpleNamespace(choices=[SimpleNamespace(message=message)])


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


def _func_tool(name: str, description: str) -> FunctionTool:
    async def _run(args):
        return f"ran {name}"

    return FunctionTool(name=name, description=description, parameters={"type": "object"}, func=_run)


def _spec_names(call: dict) -> list[str]:
    """Tool names advertised to the model in one recorded chat-completions call."""
    tools = call.get("tools") or []
    return [t["function"]["name"] for t in tools]


# ── unit tests on ToolSearch directly ────────────────────────────────────────
@pytest.mark.asyncio
async def test_tool_search_matches_by_name_and_promotes():
    search = ToolSearch(
        [
            _func_tool("git_status", "Show git working tree status"),
            _func_tool("git_diff", "Show git diff between commits"),
            _func_tool("http_get", "Fetch a URL via HTTP GET"),
        ]
    )
    out = json.loads(await search.execute({"query": "git"}))
    assert out["matched"] == 2
    names = {t["name"] for t in out["tools"]}
    assert names == {"git_status", "git_diff"}
    assert search.is_promoted("git_status")
    assert search.is_promoted("git_diff")
    assert not search.is_promoted("http_get")


@pytest.mark.asyncio
async def test_tool_search_matches_by_description():
    search = ToolSearch([_func_tool("http_get", "Fetch a URL via HTTP GET")])
    out = json.loads(await search.execute({"query": "url"}))  # case-insensitive
    assert out["matched"] == 1
    assert search.is_promoted("http_get")


@pytest.mark.asyncio
async def test_tool_search_no_match_promotes_nothing():
    search = ToolSearch([_func_tool("git_status", "Show git status")])
    out = json.loads(await search.execute({"query": "xyzzy"}))
    assert out["matched"] == 0
    assert out["tools"] == []
    assert not search.is_promoted("git_status")


@pytest.mark.asyncio
async def test_tool_search_empty_query_is_noop():
    search = ToolSearch([_func_tool("git_status", "Show git status")])
    out = json.loads(await search.execute({"query": "   "}))
    assert out["matched"] == 0
    assert not search.is_promoted("git_status")


# ── end-to-end through the agent loop ────────────────────────────────────────
@pytest.mark.asyncio
async def test_deferred_schema_hidden_until_promoted_then_dispatchable():
    git_status = _func_tool("git_status", "Show git working tree status")
    http_get = _func_tool("http_get", "Fetch a URL via HTTP GET")
    eager = _func_tool("echo", "Echo back")

    client = FakeClient(
        [
            # Turn 1: model searches for git tools.
            _msg(tool_calls=[_tool_call("c1", "tool_search", '{"query": "git"}')]),
            # Turn 2: model calls the now-promoted git_status tool.
            _msg(tool_calls=[_tool_call("c2", "git_status", "{}")]),
            # Turn 3: model wraps up.
            _msg(content="done"),
        ]
    )
    agent = SmoothAgent(client, AgentOptions(tools=[eager], deferred_tools=[git_status, http_get]))
    result = await agent.run("inspect the repo")
    assert result.text == "done"
    assert result.tool_calls == 2

    calls = client.chat.completions.calls
    # Turn 1: eager tool + tool_search advertised; deferred tools hidden.
    first = _spec_names(calls[0])
    assert "echo" in first
    assert "tool_search" in first
    assert "git_status" not in first
    assert "http_get" not in first
    # Turn 2: git_status promoted into view; http_get still hidden.
    second = _spec_names(calls[1])
    assert "git_status" in second
    assert "http_get" not in second
    # The promoted tool actually dispatched (ran), fed back as a tool message.
    second_msgs = calls[1]["messages"]
    assert any(m.get("role") == "tool" and m.get("content") == "ran git_status" for m in second_msgs)


@pytest.mark.asyncio
async def test_unpromoted_deferred_tool_is_not_dispatchable():
    git_status = _func_tool("git_status", "Show git working tree status")
    client = FakeClient(
        [
            # Model jumps straight to a deferred tool it was never shown — should fail.
            _msg(tool_calls=[_tool_call("c1", "git_status", "{}")]),
            _msg(content="ok"),
        ]
    )
    agent = SmoothAgent(client, AgentOptions(deferred_tools=[git_status]))
    result = await agent.run("try it")
    assert result.text == "ok"
    # The deferred-but-unpromoted call resolved to an unknown-tool error.
    tool_msgs = [m for m in client.chat.completions.calls[1]["messages"] if m.get("role") == "tool"]
    assert tool_msgs
    assert "unknown tool 'git_status'" in tool_msgs[0]["content"]


@pytest.mark.asyncio
async def test_no_deferred_tools_means_no_meta_tool():
    client = FakeClient([_msg(content="hi")])
    agent = SmoothAgent(client, AgentOptions(tools=[_func_tool("echo", "echo")]))
    await agent.run("hello")
    assert "tool_search" not in _spec_names(client.chat.completions.calls[0])
