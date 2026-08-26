"""Regression for th-58db12: the streaming model call must request the trailing
usage chunk (stream_options.include_usage), or the gateway sends no usage on a
streaming response and token counts (hence per-turn cost) are lost."""

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, SmoothAgent


@pytest.mark.asyncio
async def test_streaming_call_requests_include_usage() -> None:
    captured: dict = {}

    async def create(**kwargs):
        captured.update(kwargs)
        return object()  # _call_model_stream just returns the create() result

    client = SimpleNamespace(chat=SimpleNamespace(completions=SimpleNamespace(create=create)))
    agent = SmoothAgent(client, AgentOptions(model="m"))

    await agent._call_model_stream([{"role": "user", "content": "hi"}], None)

    assert captured.get("stream") is True
    assert captured.get("stream_options") == {"include_usage": True}, (
        f"streaming request must set stream_options.include_usage; got {captured.get('stream_options')!r}"
    )
