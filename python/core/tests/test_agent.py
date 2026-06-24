"""Non-network unit tests for the Python core: the agentic loop, tool calling, and
knowledge injection, driven by the reusable :class:`MockLlmProvider`. Always green
(no credentials needed) — the live-gateway behavior is covered by ``test_evals.py``.

These tests used to roll their own ad-hoc fake openai client; they now use the
shared ``MockLlmProvider`` (see ``test_llm_provider.py``) as the demonstration that
it replaces the hand-written fakes.
"""

from __future__ import annotations

import pytest

from smooth_operator_core import (
    AgentOptions,
    FunctionTool,
    InMemoryKnowledge,
    MockLlmProvider,
    SmoothAgent,
)


def test_knowledge_query_ranks_by_overlap():
    kb = InMemoryKnowledge()
    kb.ingest("The return window is 17 days from delivery.", "returns.md")
    kb.ingest("Gift wrapping costs 4.99 per item.", "wrapping.md")
    hits = kb.query("what is the return window?", top_k=1)
    assert len(hits) == 1
    assert "17 days" in hits[0].content


@pytest.mark.asyncio
async def test_text_reply_stops_after_one_call():
    mock = MockLlmProvider()
    mock.push_text("the answer is 42")
    agent = SmoothAgent(mock, AgentOptions(instructions="be helpful"))
    result = await agent.run("what is the answer?")
    assert result.text == "the answer is 42"
    assert result.iterations == 1
    assert result.tool_calls == 0


@pytest.mark.asyncio
async def test_tool_call_then_finish():
    async def echo(args):
        return args.get("text", "")

    tool = FunctionTool(
        name="echo",
        description="Echoes input back",
        parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
        func=echo,
    )
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", '{"text": "hello tools"}').push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[tool]))
    result = await agent.run("use echo")
    assert result.text == "done"
    assert result.tool_calls == 1
    # The tool result was fed back as a tool-role message before the final call.
    second_call_messages = mock.calls[1].messages
    assert any(m.get("role") == "tool" and m.get("content") == "hello tools" for m in second_call_messages)


@pytest.mark.asyncio
async def test_knowledge_is_injected_into_system_prompt():
    kb = InMemoryKnowledge()
    kb.ingest("The return window is exactly 17 days from delivery.", "returns.md")
    mock = MockLlmProvider()
    mock.push_text("17 days")
    agent = SmoothAgent(mock, AgentOptions(instructions="support agent", knowledge=kb))
    await agent.run("how many days to return?")
    system = mock.calls[0].messages[0]
    assert system["role"] == "system"
    assert "17 days" in system["content"]
