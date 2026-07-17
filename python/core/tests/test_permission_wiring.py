"""AgentOptions ``permission_mode`` / ``deny_policy`` wiring — the auto-prepended
PermissionHook gates tool calls end-to-end on a real :class:`SmoothAgent` turn."""

from __future__ import annotations

import pytest

from smooth_operator_core import (
    AgentOptions,
    AutoMode,
    DenyPolicy,
    FunctionTool,
    MockLlmProvider,
    SmoothAgent,
)

_BASH_PARAMS = {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}


def _bash_tool(runs: list[str]) -> FunctionTool:
    async def _run(args: dict) -> str:
        runs.append(str(args.get("cmd", "")))
        return "ran"

    return FunctionTool("bash", "run a shell command", _BASH_PARAMS, _run)


@pytest.mark.asyncio
async def test_no_permission_mode_is_additive_noop():
    """Neither field set ⇒ no gating: even a dangerous command reaches the tool
    (behavior unchanged from before this feature)."""
    runs: list[str] = []
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", '{"cmd": "rm -rf /"}')
    mock.push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[_bash_tool(runs)]))
    await agent.run("go")
    assert runs == ["rm -rf /"]  # ungated


@pytest.mark.asyncio
async def test_permission_mode_denies_dangerous_command():
    runs: list[str] = []
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", '{"cmd": "rm -rf /"}')
    mock.push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[_bash_tool(runs)], permission_mode=AutoMode.BYPASS))
    await agent.run("go")
    assert runs == [], "the circuit-breaker must block the tool before it runs"
    tool_msgs = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_msgs and "blocked by hook" in tool_msgs[-1]["content"]


@pytest.mark.asyncio
async def test_permission_mode_bypass_allows_ordinary_command():
    runs: list[str] = []
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", '{"cmd": "npm install left-pad"}')
    mock.push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[_bash_tool(runs)], permission_mode=AutoMode.BYPASS))
    await agent.run("go")
    assert runs == ["npm install left-pad"]


@pytest.mark.asyncio
async def test_deny_policy_only_activates_bypass_gate():
    """A deny policy with NO explicit mode still enforces the policy (and the
    built-in breakers) while allowing everything else — the implicit BYPASS gate."""
    runs: list[str] = []
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", '{"cmd": "terraform apply -auto-approve"}')
    mock.push_text("done")
    policy = DenyPolicy.from_toml('[bash]\ndeny_patterns = ["terraform apply"]')
    agent = SmoothAgent(mock, AgentOptions(tools=[_bash_tool(runs)], deny_policy=policy))
    await agent.run("go")
    assert runs == [], "the deny policy must block terraform apply"
    tool_msgs = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_msgs and "denied by policy" in tool_msgs[-1]["content"]


@pytest.mark.asyncio
async def test_deny_policy_only_allows_unrelated_command():
    runs: list[str] = []
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "bash", '{"cmd": "terraform plan"}')
    mock.push_text("done")
    policy = DenyPolicy.from_toml('[bash]\ndeny_patterns = ["terraform apply"]')
    agent = SmoothAgent(mock, AgentOptions(tools=[_bash_tool(runs)], deny_policy=policy))
    await agent.run("go")
    assert runs == ["terraform plan"]
