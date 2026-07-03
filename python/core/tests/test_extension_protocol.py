"""Unit tests for the SEP wire protocol types (frames + typed params/results)."""

from __future__ import annotations

import pytest

from smooth_operator_core.extension.protocol import (
    Context,
    HookOutcome,
    InitializeParams,
    InitializeResult,
    Message,
    RpcError,
    Tier,
    ToolExecuteParams,
    ToolExecuteResult,
    codes,
)


def test_message_classification() -> None:
    req = Message.request(1, "ping", {})
    assert req.is_request() and not req.is_notification() and not req.is_response()

    note = Message.notification("event", {})
    assert note.is_notification() and not note.is_request()

    ok = Message.success(1, {})
    assert ok.is_response() and not ok.is_request()

    err = Message.error_response(1, RpcError(codes.BLOCKED, "no"))
    assert err.is_response()


def test_request_frame_omits_result_and_error() -> None:
    d = Message.request(7, "tool/execute", {"x": 1}).to_dict()
    assert "result" not in d and "error" not in d
    assert d["jsonrpc"] == "2.0" and d["method"] == "tool/execute"


def test_notification_has_no_id() -> None:
    d = Message.notification("event", {"event": "turn_start"}).to_dict()
    assert "id" not in d


def test_message_roundtrips_all_shapes() -> None:
    for m in [
        Message.request("abc", "initialize", {}),
        Message.notification("log", {"level": "info", "message": "hi"}),
        Message.success(2, {"ok": True}),
        Message.error_response(2, RpcError(codes.CANCELLED, "cancelled")),
        Message.error_response(None, RpcError(codes.PARSE_ERROR, "bad json")),
    ]:
        assert Message.from_dict(m.to_dict()) == m


def test_initialize_params_roundtrip() -> None:
    from smooth_operator_core.extension.protocol import HostInfo, WorkspaceInfo

    p = InitializeParams(
        protocol_version=1,
        host=HostInfo("smooth-operator-core", "0.15.0"),
        workspace=WorkspaceInfo("/ws", True),
        mode="headless",
        session={"id": "s1"},
        ui_capabilities=["confirm"],
        capabilities_enabled={"tools": True},
    )
    assert InitializeParams.from_dict(p.to_dict()) == p


def test_initialize_result_roundtrip() -> None:
    r = InitializeResult.from_dict(
        {
            "protocol_version": 1,
            "extension": {"name": "echo", "version": "0.1.0"},
            "registrations": {
                "tools": [
                    {"name": "say", "description": "Echo.", "parameters": {"type": "object"}},
                ],
                "subscriptions": ["turn_start"],
            },
        }
    )
    assert r.extension.name == "echo"
    assert r.registrations.tools[0].name == "say"
    assert r.registrations.tools[0].deferred is False
    assert InitializeResult.from_dict(r.to_dict()) == r


def test_hook_outcome_variants_serialize_by_action() -> None:
    assert HookOutcome("continue").to_dict() == {"action": "continue"}
    assert HookOutcome("block", reason="nope").to_dict() == {"action": "block", "reason": "nope"}
    assert HookOutcome("block").to_dict() == {"action": "block"}
    assert HookOutcome("modify", patch={"a": 1}).to_dict() == {"action": "modify", "patch": {"a": 1}}


def test_hook_outcome_parses_from_wire() -> None:
    assert HookOutcome.from_dict({"action": "continue"}) == HookOutcome("continue")
    m = HookOutcome.from_dict({"action": "modify", "patch": {}})
    assert m.action == "modify" and m.patch == {}


def test_hook_outcome_rejects_unknown_action() -> None:
    with pytest.raises(ValueError):
        HookOutcome.from_dict({"action": "bogus"})


def test_hook_outcome_rejects_modify_without_patch() -> None:
    with pytest.raises(ValueError):
        HookOutcome.from_dict({"action": "modify"})


def test_tier_serializes_snake_case() -> None:
    assert Tier.COMMAND.value == "command"
    assert Tier.EVENT.value == "event"
    assert Context("t", Tier.COMMAND).to_dict() == {"token": "t", "tier": "command"}


def test_tool_execute_roundtrip() -> None:
    p = ToolExecuteParams("c1", "say", {"phrase": "hi"}, Context("t", Tier.COMMAND))
    assert ToolExecuteParams.from_dict(p.to_dict()) == p
    r = ToolExecuteResult("hi", is_error=False)
    assert ToolExecuteResult.from_dict(r.to_dict()) == r


def test_tool_execute_params_require_call_id() -> None:
    with pytest.raises(KeyError):
        ToolExecuteParams.from_dict({"tool": "say", "arguments": {}, "context": {"token": "t", "tier": "command"}})


def test_rpc_error_is_exception() -> None:
    e = RpcError(codes.NO_UI, "headless")
    assert "JSON-RPC error -32001: headless" in str(e)
    assert e.code == codes.NO_UI
