"""The SEP seam, driven end-to-end through the real agent loop.

These cover the WIRING — that the extension host is actually reached from the
loop — which is exactly what was missing: the host was published but unreachable,
with zero references from ``agent.py``.

The host's own behavior (fold policy, cross-tool guard, subprocess lifecycle) is
tested in ``test_extension_host.py`` / ``test_extension_process.py``; a fake host
keeps these deterministic and subprocess-free.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from smooth_operator_core import AgentOptions, FunctionTool, MockLlmProvider, SmoothAgent
from smooth_operator_core.extension.host import FoldedHook


class FakeHost:
    """A scripted stand-in for ExtensionHost, recording what the loop asked it."""

    def __init__(
        self,
        tools: list[Any] | None = None,
        deferred: list[Any] | None = None,
        block_tool: str | None = None,
        block_reason: str = "",
        patch_tool: str | None = None,
        patch_arguments: dict[str, Any] | None = None,
    ) -> None:
        self._tools = tools or []
        self._deferred = deferred or []
        self._block_tool = block_tool
        self._block_reason = block_reason
        self._patch_tool = patch_tool
        self._patch_arguments = patch_arguments
        self.events: list[tuple[str, dict[str, Any]]] = []
        self.hooked: list[str] = []

    def tools(self) -> list[Any]:
        return self._tools

    def deferred_tools(self) -> list[Any]:
        return self._deferred

    def dispatch_event(self, event: str, payload: dict[str, Any]) -> None:
        self.events.append((event, payload))

    async def run_tool_call_hook(self, tool: str, arguments: Any) -> FoldedHook:
        self.hooked.append(tool)
        if tool == self._block_tool:
            return FoldedHook.block(self._block_reason)
        if tool == self._patch_tool:
            return FoldedHook.proceed({"tool": tool, "arguments": self._patch_arguments})
        return FoldedHook.proceed({"tool": tool, "arguments": arguments})


def _recording_tool(name: str) -> tuple[Any, dict[str, Any]]:
    """A tool that records the arguments it was executed with."""
    seen: dict[str, Any] = {"calls": 0, "args": None}

    async def run(args: dict[str, Any]) -> str:
        seen["calls"] += 1
        seen["args"] = args
        return "ok"

    return FunctionTool(name, "records its arguments", {"type": "object"}, run), seen


@pytest.mark.asyncio
async def test_extension_tools_are_visible_and_dispatchable():
    tool, seen = _recording_tool("weather.forecast")
    host = FakeHost(tools=[tool])
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "weather.forecast", json.dumps({"city": "NYC"}))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(extensions=host))
    await agent.run("weather?")

    # Offered to the model as an ordinary tool...
    offered = [t["function"]["name"] for t in (mock.calls[0].tools or [])]
    assert "weather.forecast" in offered
    # ...and actually executed when called.
    assert seen["calls"] == 1


@pytest.mark.asyncio
async def test_deferred_extension_tools_stay_hidden_until_promoted():
    """The deferred-tool invariant must survive the merge: an unpromoted deferred
    tool is never offered to the model, and is not dispatchable."""
    hidden, _ = _recording_tool("vault.read")
    host = FakeHost(deferred=[hidden])
    mock = MockLlmProvider()
    mock.push_text("hi")

    agent = SmoothAgent(mock, AgentOptions(extensions=host))
    await agent.run("hello")

    offered = [t["function"]["name"] for t in (mock.calls[0].tools or [])]
    assert "vault.read" not in offered


@pytest.mark.asyncio
async def test_tool_call_hook_veto_blocks_execution():
    native, seen = _recording_tool("bash")
    host = FakeHost(block_tool="bash", block_reason="policy: no shell")
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", json.dumps({"command": "rm -rf /"}))
    mock.push_text("understood")

    agent = SmoothAgent(mock, AgentOptions(tools=[native], extensions=host))
    await agent.run("clean up")

    assert seen["calls"] == 0, "a vetoed tool call must never execute"
    # The model is told why, rather than seeing an empty result.
    tool_msgs = [m["content"] for m in mock.calls[1].messages if m.get("role") == "tool"]
    assert any("policy: no shell" in c for c in tool_msgs)


@pytest.mark.asyncio
async def test_tool_call_hook_modify_rewrites_arguments():
    tool, seen = _recording_tool("weather.forecast")
    host = FakeHost(tools=[tool], patch_tool="weather.forecast", patch_arguments={"city": "Boston"})
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "weather.forecast", json.dumps({"city": "NYC"}))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(extensions=host))
    await agent.run("weather?")

    assert seen["args"]["city"] == "Boston", "arguments not rewritten by the hook"


@pytest.mark.asyncio
async def test_turn_events_are_dispatched_in_rust_order():
    host = FakeHost()
    mock = MockLlmProvider()
    mock.push_text("hello there")

    agent = SmoothAgent(mock, AgentOptions(extensions=host))
    await agent.run("hi")

    assert [e for e, _ in host.events] == ["turn_start", "message_end", "turn_end"]
    payloads = dict(host.events)
    assert payloads["message_end"]["content"] == "hello there"


@pytest.mark.asyncio
async def test_end_events_fire_on_the_max_iteration_exit():
    """The easy exit to miss: a turn that never stops calling tools still has to
    emit its end events, or a subscribed extension waits forever."""
    tool, _ = _recording_tool("loop")
    host = FakeHost()
    mock = MockLlmProvider()
    for i in range(3):
        mock.push_tool_call(f"c{i}", "loop", "{}")

    agent = SmoothAgent(mock, AgentOptions(tools=[tool], extensions=host, max_iterations=3))
    await agent.run("go")

    assert [e for e, _ in host.events] == ["turn_start", "message_end", "turn_end"]


@pytest.mark.asyncio
async def test_no_extension_host_leaves_the_loop_unchanged():
    """The zero-cost default: extensions=None means no hook calls, no dispatch."""
    native, seen = _recording_tool("echo")
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", json.dumps({"x": 1}))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[native]))
    result = await agent.run("go")

    assert seen["calls"] == 1
    assert result.text == "done"
