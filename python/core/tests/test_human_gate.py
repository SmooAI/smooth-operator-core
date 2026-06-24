"""Unit tests for human-in-the-loop approval (HumanGate).

Driven by the same fake OpenAI-compatible client the other agent tests use, so no
credentials are needed. Covers the three behaviors: an approved tool executes; a
denied tool does NOT execute and its denial reason reaches the model; and with no
gate configured behavior is unchanged.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import (
    AgentOptions,
    DelegateHumanGate,
    FunctionTool,
    HumanApprovalRequest,
    HumanApprovalResponse,
    HumanDecision,
    SmoothAgent,
)


# ── a tiny fake of the openai client surface the agent uses ──────────────────
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


def _spy_tool() -> tuple[FunctionTool, list[dict]]:
    """A tool that records every invocation so a test can assert it never ran."""
    invocations: list[dict] = []

    async def _run(args):
        invocations.append(args)
        return "deleted record " + str(args.get("id", ""))

    tool = FunctionTool(
        name="delete_record",
        description="Deletes a record (destructive).",
        parameters={"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]},
        func=_run,
    )
    return tool, invocations


# ── HumanApprovalResponse helpers ────────────────────────────────────────────
def test_response_helpers():
    approved = HumanApprovalResponse.approve()
    assert approved.is_approved is True
    assert approved.decision is HumanDecision.APPROVED

    denied = HumanApprovalResponse.deny("not allowed")
    assert denied.is_approved is False
    assert denied.decision is HumanDecision.DENIED
    assert denied.reason == "not allowed"


@pytest.mark.asyncio
async def test_approved_tool_executes():
    tool, invocations = _spy_tool()
    seen: list[HumanApprovalRequest] = []

    async def approver(req: HumanApprovalRequest) -> HumanApprovalResponse:
        seen.append(req)
        return HumanApprovalResponse.approve()

    client = FakeClient(
        [
            _msg(tool_calls=[_tool_call("c1", "delete_record", '{"id": "42"}')]),
            _msg(content="done"),
        ]
    )
    agent = SmoothAgent(
        client,
        AgentOptions(
            tools=[tool],
            human_gate=DelegateHumanGate(approver),
            requires_approval=lambda name, _args: name == "delete_record",
        ),
    )
    result = await agent.run("delete record 42")

    assert result.text == "done"
    assert result.tool_calls == 1
    # The gate was consulted with the right request, and the tool actually ran.
    assert len(seen) == 1
    assert seen[0].tool_name == "delete_record"
    assert seen[0].arguments == {"id": "42"}
    assert invocations == [{"id": "42"}]
    # The successful tool result was fed back to the model.
    second_call_messages = client.chat.completions.calls[1]["messages"]
    assert any(
        m.get("role") == "tool" and "deleted record 42" in (m.get("content") or "") for m in second_call_messages
    )


@pytest.mark.asyncio
async def test_denied_tool_does_not_execute_and_reason_reaches_model():
    tool, invocations = _spy_tool()

    async def approver(_req: HumanApprovalRequest) -> HumanApprovalResponse:
        return HumanApprovalResponse.deny("policy forbids deletes")

    client = FakeClient(
        [
            _msg(tool_calls=[_tool_call("c1", "delete_record", '{"id": "42"}')]),
            _msg(content="understood, I won't delete it"),
        ]
    )
    agent = SmoothAgent(
        client,
        AgentOptions(
            tools=[tool],
            human_gate=DelegateHumanGate(approver),
            requires_approval=lambda name, _args: name == "delete_record",
        ),
    )
    result = await agent.run("delete record 42")

    # The tool never ran.
    assert invocations == []
    assert result.text == "understood, I won't delete it"
    # The denial (with reason) was fed back to the model as the tool result.
    second_call_messages = client.chat.completions.calls[1]["messages"]
    denial = next((m for m in second_call_messages if m.get("role") == "tool"), None)
    assert denial is not None
    assert "Denied by human" in denial["content"]
    assert "policy forbids deletes" in denial["content"]


@pytest.mark.asyncio
async def test_no_gate_configured_leaves_behavior_unchanged():
    tool, invocations = _spy_tool()
    client = FakeClient(
        [
            _msg(tool_calls=[_tool_call("c1", "delete_record", '{"id": "42"}')]),
            _msg(content="done"),
        ]
    )
    # No human_gate set — even though requires_approval would match, it is ignored.
    agent = SmoothAgent(
        client,
        AgentOptions(tools=[tool], requires_approval=lambda name, _args: True),
    )
    result = await agent.run("delete record 42")

    assert result.text == "done"
    assert invocations == [{"id": "42"}]


@pytest.mark.asyncio
async def test_gate_only_consults_flagged_tools():
    tool, invocations = _spy_tool()
    consulted: list[str] = []

    async def approver(req: HumanApprovalRequest) -> HumanApprovalResponse:
        consulted.append(req.tool_name)
        return HumanApprovalResponse.deny("should not be asked")

    client = FakeClient(
        [
            _msg(tool_calls=[_tool_call("c1", "delete_record", '{"id": "7"}')]),
            _msg(content="done"),
        ]
    )
    # requires_approval returns False for this tool, so the gate is never consulted
    # and the tool runs normally.
    agent = SmoothAgent(
        client,
        AgentOptions(
            tools=[tool],
            human_gate=DelegateHumanGate(approver),
            requires_approval=lambda name, _args: name == "send_email",
        ),
    )
    result = await agent.run("delete record 7")

    assert consulted == []
    assert invocations == [{"id": "7"}]
    assert result.text == "done"
