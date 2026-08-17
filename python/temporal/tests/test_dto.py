"""Unit tests for the serde DTO boundary — the Python sibling of the Rust
reference's ``dto.rs`` tests. These run WITHOUT Temporal: they import only
``smooth_operator_temporal.dto`` (no ``temporalio``), proving the activity
boundary's marshalling is correct regardless of whether the SDK is installed.
"""

from __future__ import annotations

import json

from smooth_operator_temporal.dto import (
    AgentTurnInput,
    ModelCallInput,
    ModelCallOutput,
    ToolInvokeInput,
    ToolInvokeOutput,
)


def test_model_call_output_round_trips_through_assistant_dict():
    """assistant dict -> DTO -> assistant dict preserves what ``drive_turn`` reads."""
    assistant = {
        "role": "assistant",
        "content": "hello",
        "tool_calls": [
            {"id": "call-1", "type": "function", "function": {"name": "echo", "arguments": '{"text": "hi"}'}}
        ],
    }
    dto = ModelCallOutput.from_assistant(assistant)
    assert dto.content == "hello"
    assert dto.tool_calls[0]["id"] == "call-1"

    restored = dto.to_assistant()
    assert restored == assistant


def test_model_call_output_omits_empty_tool_calls():
    """A plain text reply reconstructs without a ``tool_calls`` key — matching what
    ``drive_turn`` appends inline for a no-tool response."""
    dto = ModelCallOutput.from_assistant({"role": "assistant", "content": "just text"})
    assert dto.to_assistant() == {"role": "assistant", "content": "just text"}


def test_model_call_output_defaults_missing_content_to_empty_string():
    """A drained-script assistant dict (``content=None``) projects to ``""``."""
    dto = ModelCallOutput.from_assistant({"role": "assistant", "content": None})
    assert dto.content == ""
    assert dto.tool_calls == []


def test_model_call_output_json_round_trips():
    """The DTO survives a JSON serialize/deserialize — i.e. it can actually cross a
    Temporal activity boundary."""
    dto = ModelCallOutput(content="hi", tool_calls=[{"id": "c1", "function": {"name": "echo", "arguments": "{}"}}])
    back = ModelCallOutput(**json.loads(json.dumps(dto.__dict__)))
    assert back.content == "hi"
    assert back.tool_calls[0]["function"]["name"] == "echo"


def test_activity_inputs_json_round_trip():
    """Activity inputs serialize cleanly (mirrors the Rust ``activity_inputs`` test)."""
    mci = ModelCallInput(messages=[{"role": "user", "content": "hi"}], tools=[{"name": "echo"}])
    back_mci = ModelCallInput(**json.loads(json.dumps(mci.__dict__)))
    assert back_mci.messages[0]["content"] == "hi"
    assert back_mci.tools[0]["name"] == "echo"

    tii = ToolInvokeInput(call={"id": "c1", "type": "function", "function": {"name": "echo", "arguments": "{}"}})
    back_tii = ToolInvokeInput(**json.loads(json.dumps(tii.__dict__)))
    assert back_tii.call["function"]["name"] == "echo"

    tio = ToolInvokeOutput(content="ran", is_error=False)
    back_tio = ToolInvokeOutput(**json.loads(json.dumps(tio.__dict__)))
    assert back_tio.content == "ran"
    assert back_tio.is_error is False


def test_agent_turn_input_defaults_and_round_trip():
    """The workflow-start input carries its optional gates and round-trips."""
    ati = AgentTurnInput(system_prompt="sys", user_message="hi")
    assert ati.tools == []
    assert ati.max_iterations == 0
    assert ati.approval_required_tools == []
    assert ati.wait_tool is None

    full = AgentTurnInput(
        system_prompt="sys",
        user_message="hi",
        tools=[{"name": "echo"}],
        max_iterations=5,
        approval_required_tools=["echo"],
        wait_tool="wait",
    )
    back = AgentTurnInput(**json.loads(json.dumps(full.__dict__)))
    assert back.max_iterations == 5
    assert back.approval_required_tools == ["echo"]
    assert back.wait_tool == "wait"
