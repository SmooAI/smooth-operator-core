"""Unit tests for the ExtensionHost orchestration — the security-critical policy
(hook fold, context-tier guard) exhaustively, plus delegate defaults and the empty
host's zero-behavior-change passthrough. No subprocess is spawned here."""

from __future__ import annotations

import pytest

from smooth_operator_core.extension.host import (
    DefaultHostDelegate,
    ExtensionHost,
    FoldedHook,
    HookStep,
    HookType,
    HostDelegate,
    HostInbound,
    effective_subscriptions,
    fold_hook_chain,
    validate_command_context,
)
from smooth_operator_core.extension.protocol import (
    HookOutcome,
    HostInfo,
    RpcError,
    Tier,
    WorkspaceInfo,
    codes,
    method,
)


def _empty_host() -> ExtensionHost:
    return ExtensionHost(HostInfo("t", "0"), WorkspaceInfo("/ws", False), "headless", [])


# ---- effective_subscriptions ----


def test_effective_subscriptions_intersects_or_passes_through() -> None:
    assert effective_subscriptions([], ["turn_start", "turn_end"]) == {"turn_start", "turn_end"}
    assert effective_subscriptions(["turn_start"], ["turn_start", "tool_call"]) == {"turn_start"}
    assert effective_subscriptions(["turn_start", "turn_end"], ["turn_end"]) == {"turn_end"}


# ---- HookType policy ----


def test_hook_type_fail_policy_and_timeout() -> None:
    assert HookType.TOOL_CALL.fail_closed()
    assert HookType.USER_BASH.fail_closed()
    assert not HookType.TOOL_RESULT.fail_closed()
    assert not HookType.MESSAGE_END.fail_closed()
    assert HookType.TOOL_CALL.default_timeout() == 60.0
    assert HookType.TOOL_RESULT.default_timeout() == 5.0
    assert HookType.from_name("before_agent_start") == HookType.BEFORE_AGENT_START
    assert HookType.from_name("nope") is None


# ---- fold_hook_chain: the security-critical policy, exhaustively ----


def test_fold_empty_chain_proceeds_unchanged() -> None:
    inp = {"tool": "rm"}
    assert fold_hook_chain(HookType.TOOL_CALL, inp, []) == FoldedHook.proceed(inp)


def test_fold_continue_keeps_value() -> None:
    steps = [HookStep.replied(HookOutcome("continue")), HookStep.replied(HookOutcome("continue"))]
    assert fold_hook_chain(HookType.TOOL_RESULT, {"a": 1}, steps) == FoldedHook.proceed({"a": 1})


def test_fold_modify_threads_patch_to_next() -> None:
    steps = [HookStep.replied(HookOutcome("modify", patch={"a": 2})), HookStep.replied(HookOutcome("continue"))]
    assert fold_hook_chain(HookType.CONTEXT, {"a": 1}, steps) == FoldedHook.proceed({"a": 2})


def test_fold_block_short_circuits() -> None:
    steps = [
        HookStep.replied(HookOutcome("block", reason="rm -rf blocked")),
        HookStep.replied(HookOutcome("modify", patch={"should": "not apply"})),
    ]
    assert fold_hook_chain(HookType.TOOL_CALL, {}, steps) == FoldedHook.block("rm -rf blocked")


def test_fold_block_without_reason_gets_default() -> None:
    steps = [HookStep.replied(HookOutcome("block"))]
    assert fold_hook_chain(HookType.USER_BASH, {}, steps) == FoldedHook.block("blocked by user_bash hook")


def test_fold_failure_is_fail_closed_for_tool_call() -> None:
    result = fold_hook_chain(HookType.TOOL_CALL, {}, [HookStep.failed()])
    assert result.blocked and "fail-closed" in result.reason


def test_fold_failure_is_fail_open_for_others() -> None:
    steps = [HookStep.failed(), HookStep.replied(HookOutcome("continue"))]
    assert fold_hook_chain(HookType.TOOL_RESULT, {"x": 9}, steps) == FoldedHook.proceed({"x": 9})


def test_fold_modify_then_failure_fail_open_keeps_patch() -> None:
    steps = [HookStep.replied(HookOutcome("modify", patch={"x": 2})), HookStep.failed()]
    assert fold_hook_chain(HookType.INPUT, {"x": 1}, steps) == FoldedHook.proceed({"x": 2})


# ---- HostDelegate defaults ----


async def test_default_delegate_ui_is_no_ui() -> None:
    with pytest.raises(RpcError) as exc:
        await DefaultHostDelegate().ui_request("ext", {"kind": "confirm"})
    assert exc.value.code == codes.NO_UI


async def test_default_delegate_exec_denied() -> None:
    with pytest.raises(RpcError) as exc:
        await DefaultHostDelegate().exec_run("ext", {"command": "ls"})
    assert exc.value.code == codes.NOT_TRUSTED


async def test_default_delegate_and_host_inbound_kv(tmp_path, monkeypatch) -> None:
    monkeypatch.setenv("SMOOTH_HOME", str(tmp_path))

    d = DefaultHostDelegate()
    assert await d.kv_get("kvtest", "missing") is None
    await d.kv_set("kvtest", "k", {"n": 1})
    assert await d.kv_get("kvtest", "k") == {"n": 1}

    inbound = HostInbound("e", DefaultHostDelegate(), _empty_host())
    assert await inbound.handle_request(method.PING, None) == {}
    await inbound.handle_request("kv/set", {"key": "a", "value": 5})
    assert await inbound.handle_request("kv/get", {"key": "a"}) == {"value": 5}
    with pytest.raises(RpcError) as exc:
        await inbound.handle_request("nope/method", None)
    assert exc.value.code == codes.METHOD_NOT_FOUND


# ---- empty host: the zero-behavior-change default ----


async def test_empty_host_hook_is_passthrough() -> None:
    host = _empty_host()
    assert host.is_empty()
    assert await host.run_hook(HookType.TOOL_CALL, {"tool": "x"}) == FoldedHook.proceed({"tool": "x"})
    assert await host.before_agent_start("prompt") == "prompt"
    assert host.tools() == []
    host.dispatch_event("turn_start", {})  # no-op, must not raise
    assert host.commands() == []
    assert host.shortcuts() == []


# ---- the command-tier deadlock guard (security-critical), exhaustively ----


def _ctx(tier: str, token: str) -> dict:
    return {"context": {"tier": tier, "token": token}, "text": "hi"}


def test_validate_command_context_accepts_current_command_tier() -> None:
    validate_command_context(_ctx("command", "epoch-4"), 4)  # no raise


def test_validate_command_context_rejects_event_tier() -> None:
    with pytest.raises(RpcError) as exc:
        validate_command_context(_ctx("event", "epoch-4"), 4)
    assert exc.value.code == codes.CONTEXT_VIOLATION


def test_validate_command_context_rejects_stale_epoch() -> None:
    with pytest.raises(RpcError) as exc:
        validate_command_context(_ctx("command", "epoch-4"), 5)
    assert exc.value.code == codes.CONTEXT_VIOLATION


def test_validate_command_context_rejects_missing_and_malformed() -> None:
    with pytest.raises(RpcError):
        validate_command_context({"text": "hi"}, 1)
    with pytest.raises(RpcError):
        validate_command_context(_ctx("command", "garbage"), 1)


class _RecordingDelegate(HostDelegate):
    def __init__(self) -> None:
        self.hits: list[str] = []

    async def session_send_message(self, ext, params):
        self.hits.append("send_message")
        return {}

    async def session_append_entry(self, ext, params):
        self.hits.append("append_entry")
        return {}


async def test_host_inbound_session_action_validates_before_delegate() -> None:
    delegate = _RecordingDelegate()
    host = _empty_host()
    host.epoch = 3
    inbound = HostInbound("e", delegate, host)

    await inbound.handle_request(method.SESSION_SEND_MESSAGE, _ctx("command", "epoch-3"))
    assert delegate.hits == ["send_message"]

    with pytest.raises(RpcError) as exc:
        await inbound.handle_request(method.SESSION_APPEND_ENTRY, _ctx("event", "epoch-3"))
    assert exc.value.code == codes.CONTEXT_VIOLATION

    host.epoch = 4  # a reload bumped 3 -> 4
    with pytest.raises(RpcError) as exc:
        await inbound.handle_request(method.SESSION_SEND_MESSAGE, _ctx("command", "epoch-3"))
    assert exc.value.code == codes.CONTEXT_VIOLATION

    assert delegate.hits == ["send_message"]  # only the one valid call reached the delegate


def test_context_token_embeds_epoch() -> None:
    host = _empty_host()
    assert host.context(Tier.COMMAND).token == "epoch-1"
    host.bump_epoch()
    assert host.context(Tier.EVENT).token == "epoch-2"
