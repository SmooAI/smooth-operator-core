"""Unit tests for sub-agent delegation (delegation-as-a-tool)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, SmoothAgent, delegate_tool


def _text(content):
    return SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content=content, tool_calls=None))], usage=None
    )


def _tool_call_resp(call_id, name, arguments):
    tc = SimpleNamespace(id=call_id, function=SimpleNamespace(name=name, arguments=arguments))
    return SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content=None, tool_calls=[tc]))], usage=None
    )


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)

    async def create(self, **_kwargs):
        return self._scripted.pop(0)


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


@pytest.mark.asyncio
async def test_delegate_tool_runs_child_and_returns_its_reply():
    # The child agent answers the delegated subtask.
    child_client = FakeClient([_text("researched: 42")])
    child = SmoothAgent(child_client, AgentOptions(instructions="you are a researcher"))

    researcher = delegate_tool("researcher", "Delegate a research subtask.", child)

    # The parent calls the delegate tool, then wraps up.
    parent_client = FakeClient(
        [
            _tool_call_resp("c1", "researcher", '{"task": "find the answer"}'),
            _text("the answer is 42"),
        ]
    )
    parent = SmoothAgent(parent_client, AgentOptions(tools=[researcher]))

    result = await parent.run("delegate to the researcher")
    assert result.text == "the answer is 42"
    assert result.tool_calls == 1


@pytest.mark.asyncio
async def test_delegate_tool_schema_requires_task():
    child = SmoothAgent(FakeClient([_text("x")]), AgentOptions())
    tool = delegate_tool("helper", "help", child)
    assert tool.name == "helper"
    assert "task" in tool.parameters["properties"]
    assert tool.parameters["required"] == ["task"]
