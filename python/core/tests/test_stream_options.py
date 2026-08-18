"""The streaming request must ASK the gateway for usage.

The OpenAI streaming API OMITS usage unless ``stream_options.include_usage`` is set,
so a stream built without it carries no usage chunk at all. That was never the gateway
losing data — it was the gateway honouring a request that never asked, and every
character-count token estimate downstream exists only because of it.

Verified against llm.smoo.ai (LiteLLM 1.95.0, groq-gpt-oss-120b): 0 chunks carry usage
without the field, 1 carries real prompt/completion counts with it. Pearl th-5e59a5.
"""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any

import pytest

from smooth_operator_core.gateway_client import _Completions


class _RecordingRawResponse:
    def __init__(self, recorder: dict[str, Any]) -> None:
        self._recorder = recorder

    async def create(self, **body: Any) -> Any:
        self._recorder.update(body)
        return SimpleNamespace(parse=lambda: SimpleNamespace(), headers={})


def _completions_with(recorder: dict[str, Any]) -> _Completions:
    client = SimpleNamespace(
        chat=SimpleNamespace(completions=SimpleNamespace(with_raw_response=_RecordingRawResponse(recorder)))
    )
    return _Completions(client)  # type: ignore[arg-type]


@pytest.mark.asyncio
async def test_streaming_request_asks_for_usage() -> None:
    sent: dict[str, Any] = {}
    await _completions_with(sent).create(model="m", messages=[], stream=True)
    assert sent["stream_options"] == {"include_usage": True}


@pytest.mark.asyncio
async def test_non_streaming_request_omits_stream_options() -> None:
    """Meaningless without a stream, and omitting it keeps that wire byte-identical."""
    sent: dict[str, Any] = {}
    await _completions_with(sent).create(model="m", messages=[])
    assert "stream_options" not in sent


@pytest.mark.asyncio
async def test_an_explicit_stream_options_is_not_overridden() -> None:
    sent: dict[str, Any] = {}
    await _completions_with(sent).create(model="m", messages=[], stream=True, stream_options={"include_usage": False})
    assert sent["stream_options"] == {"include_usage": False}
