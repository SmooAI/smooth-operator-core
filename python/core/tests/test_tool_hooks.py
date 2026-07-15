"""Tool-call lifecycle hooks — the Python sibling of the Rust reference engine's
``ToolHook`` tests (``smooth-operator-core/src/tool.rs``).

A :class:`ToolHook`'s ``pre_call`` runs before every tool (raise to block it) and
``post_call`` runs after with a MUTABLE result — the redaction seam. These drive a
real :class:`SmoothAgent` turn on :class:`MockLlmProvider`: script a tool call, then
a final text answer, and assert the hooks fired / mutated / blocked as designed.
"""

from __future__ import annotations

import pytest

from smooth_operator_core import (
    AgentOptions,
    FunctionTool,
    MockLlmProvider,
    SmoothAgent,
    ToolCall,
    ToolHook,
    ToolResult,
)

_ECHO_PARAMS = {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}


def _echo_tool() -> FunctionTool:
    async def _run(args: dict) -> str:
        return str(args.get("text", ""))

    return FunctionTool("echo", "Echoes its input back", _ECHO_PARAMS, _run)


class SpyHook(ToolHook):
    """Records every pre/post invocation without altering behaviour."""

    def __init__(self) -> None:
        self.pre: list[ToolCall] = []
        self.post: list[tuple[ToolCall, str]] = []

    async def pre_call(self, call: ToolCall) -> None:
        self.pre.append(call)

    async def post_call(self, call: ToolCall, result: ToolResult) -> None:
        self.post.append((call, result.content))


@pytest.mark.asyncio
async def test_hook_fires_pre_and_post_around_a_tool_call():
    spy = SpyHook()
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "hi"}')
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[_echo_tool()], tool_hooks=[spy]))
    result = await agent.run("go")

    assert result.text == "done"
    # pre_call saw the call, with parsed args, BEFORE execution.
    assert [c.name for c in spy.pre] == ["echo"]
    assert spy.pre[0].arguments == {"text": "hi"}
    # post_call saw the same call AND the tool's output.
    assert [(c.name, out) for c, out in spy.post] == [("echo", "hi")]


@pytest.mark.asyncio
async def test_post_call_redaction_mutates_result_reaching_the_model():
    """A post_call hook that rewrites ``result.content`` must have its mutation
    reflected in the tool message fed back to the model (redaction seam)."""

    class RedactHook(ToolHook):
        async def post_call(self, call: ToolCall, result: ToolResult) -> None:
            result.content = result.content.replace("secret", "[REDACTED]")

    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "the secret is 42"}')
    mock.push_text("ok")

    agent = SmoothAgent(mock, AgentOptions(tools=[_echo_tool()], tool_hooks=[RedactHook()]))
    await agent.run("go")

    # The second model call carries the tool result — it must be the redacted form.
    assert mock.call_count == 2
    tool_messages = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_messages, "expected a tool result message on the follow-up call"
    assert tool_messages[-1]["content"] == "the [REDACTED] is 42"


@pytest.mark.asyncio
async def test_pre_call_raise_blocks_the_tool():
    """A pre_call that raises blocks the call: the tool never runs and the model is
    told it was blocked."""
    ran = {"executed": False}

    async def _run(args: dict) -> str:
        ran["executed"] = True
        return "should not happen"

    class BlockHook(ToolHook):
        async def pre_call(self, call: ToolCall) -> None:
            if call.name == "echo":
                raise RuntimeError("blocked by policy")

    tool = FunctionTool("echo", "", _ECHO_PARAMS, _run)
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "hi"}')
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[tool], tool_hooks=[BlockHook()]))
    await agent.run("go")

    assert ran["executed"] is False
    tool_messages = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_messages[-1]["content"] == "blocked by hook: blocked by policy"


@pytest.mark.asyncio
async def test_post_call_hook_exception_is_swallowed():
    """A post_call that raises must not crash the turn — the (unredacted) result
    still reaches the caller."""

    class BoomHook(ToolHook):
        async def post_call(self, call: ToolCall, result: ToolResult) -> None:
            raise RuntimeError("post-hook is broken")

    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "hi"}')
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[_echo_tool()], tool_hooks=[BoomHook()]))
    result = await agent.run("go")

    assert result.text == "done"
    tool_messages = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_messages[-1]["content"] == "hi"


@pytest.mark.asyncio
async def test_hooks_run_in_registration_order():
    order: list[str] = []

    class First(ToolHook):
        async def pre_call(self, call: ToolCall) -> None:
            order.append("first")

    class Second(ToolHook):
        async def pre_call(self, call: ToolCall) -> None:
            order.append("second")

    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "hi"}')
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[_echo_tool()], tool_hooks=[First(), Second()]))
    await agent.run("go")

    assert order == ["first", "second"]


@pytest.mark.asyncio
async def test_no_hooks_is_unchanged():
    """The empty-hooks default path returns the tool output verbatim."""
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text": "verbatim"}')
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[_echo_tool()]))
    await agent.run("go")

    tool_messages = [m for m in mock.last_call.messages if m.get("role") == "tool"]
    assert tool_messages[-1]["content"] == "verbatim"
