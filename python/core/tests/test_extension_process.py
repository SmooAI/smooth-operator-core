"""Live-subprocess tests for ExtensionProcess — spawn / handshake / request /
timeout / respawn (generation guard), plus the bounded observe lane's shed +
events_lost behaviour. Drives the dependency-free Python echo peer in tests/sep/."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from smooth_operator_core.extension.process import (
    OBSERVE_QUEUE_CAP,
    DefaultInboundHandler,
    ExtensionProcess,
    ObserveLane,
    SpawnSpec,
    backoff_for,
)
from smooth_operator_core.extension.protocol import method

ECHO_PEER = Path(__file__).parent / "sep" / "echo_peer.py"


def _echo_spec() -> SpawnSpec:
    return SpawnSpec(command=sys.executable, args=[str(ECHO_PEER)])


def test_backoff_schedule() -> None:
    assert backoff_for(0) == 1.0
    assert backoff_for(1) == 5.0
    assert backoff_for(2) == 25.0
    assert backoff_for(3) is None


def test_observe_lane_sheds_oldest_and_marks_loss() -> None:
    lane = ObserveLane()
    ctx = {"token": "e", "tier": "event"}
    for i in range(OBSERVE_QUEUE_CAP + 3):
        lane.push("turn_start", ctx, {"n": i})

    marker = lane.pop_for_write()
    assert marker is not None
    p = marker.params
    assert p["event"] == "events_lost"
    assert p["payload"]["lost"] == 3
    assert "seq" not in p  # out-of-band marker has no seq
    assert p["context"] is not None

    first = lane.pop_for_write()
    assert first is not None
    assert first.params["payload"]["n"] == 3  # oldest (0,1,2) shed


def test_observe_lane_no_marker_when_no_loss() -> None:
    lane = ObserveLane()
    lane.push("turn_start", {"token": "e", "tier": "event"}, {})
    f = lane.pop_for_write()
    assert f is not None and f.params["event"] == "turn_start"
    assert lane.pop_for_write() is None


async def test_default_handler_answers_ping_only() -> None:
    h = DefaultInboundHandler()
    assert await h.handle_request(method.PING, None) == {}
    from smooth_operator_core.extension.protocol import RpcError

    with pytest.raises(RpcError):
        await h.handle_request("session/send_message", None)


async def test_spawn_handshake_and_tool_execute() -> None:
    proc = await ExtensionProcess.spawn(_echo_spec(), DefaultInboundHandler())
    try:
        init = await proc.request(method.INITIALIZE, {"protocol_version": 1}, 5.0)
        assert init["extension"]["name"] == "echo"
        assert init["registrations"]["tools"][0]["name"] == "say"

        assert await proc.request(method.PING, {}, 5.0) == {}

        result = await proc.request(
            method.TOOL_EXECUTE,
            {
                "call_id": "c1",
                "tool": "say",
                "arguments": {"phrase": "hello"},
                "context": {"token": "e", "tier": "command"},
            },
            5.0,
        )
        assert result["content"] == "hello"
    finally:
        await proc.shutdown(2.0)


async def test_request_times_out_against_silent_peer() -> None:
    # A peer that consumes stdin and never replies -> the request must time out.
    spec = SpawnSpec(command=sys.executable, args=["-c", "import sys\nfor _ in sys.stdin: pass"])
    proc = await ExtensionProcess.spawn(spec, DefaultInboundHandler())
    try:
        with pytest.raises(RuntimeError, match="timed out"):
            await proc.request(method.PING, {}, 0.3)
    finally:
        await proc.shutdown(0.5)


async def test_respawn_bumps_generation_and_recovers() -> None:
    proc = await ExtensionProcess.spawn(_echo_spec(), DefaultInboundHandler())
    try:
        await proc.request(method.INITIALIZE, {"protocol_version": 1}, 5.0)
        gen_before = proc.generation
        await proc.respawn()
        assert proc.generation == gen_before + 1
        assert proc.is_alive
        # The fresh child handshakes and serves again.
        init = await proc.request(method.INITIALIZE, {"protocol_version": 1}, 5.0)
        assert init["extension"]["name"] == "echo"
    finally:
        await proc.shutdown(2.0)


async def test_request_on_dead_process_raises() -> None:
    proc = await ExtensionProcess.spawn(_echo_spec(), DefaultInboundHandler())
    await proc.shutdown(2.0)
    with pytest.raises(RuntimeError, match="not alive"):
        await proc.request(method.PING, {}, 1.0)
