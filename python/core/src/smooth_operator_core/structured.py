"""Structured output — a guaranteed-JSON answer conforming to a caller-supplied
JSON Schema. Port of the Rust reference's ``ResponseFormat``
(``rust/smooth-operator-core/src/llm.rs``, SMOODEV-1472).

Wire mapping (OpenAI-compatible, e.g. the LiteLLM gateway at ``llm.smoo.ai``):
serialized on ``/chat/completions`` as::

    {"response_format": {"type": "json_schema",
                         "json_schema": {"name": ..., "schema": ..., "strict": ...}}}

Usage follows this package's existing ``metadata`` idiom — a mapping you splat
into the request, so an unset format contributes nothing and the wire stays
byte-identical::

    response = await client.chat.completions.create(
        model=model,
        messages=messages,
        **response_format_field(json_schema_format("weather", schema)),
    )
    report = structured_json(response)

The Rust engine also has an Anthropic-native path (no ``response_format`` field
there, so it forces a synthetic single tool call whose ``input_schema`` IS the
requested schema). This engine talks to OpenAI-compatible endpoints only, so
there is nothing to mirror.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

__all__ = ["ResponseFormat", "json_schema_format", "response_format_field", "structured_json"]


@dataclass(frozen=True)
class ResponseFormat:
    """Constrains the model's reply to a named JSON Schema.

    Rust models this as an enum with a single ``JsonSchema`` variant; Python uses
    a dataclass until a second variant exists.
    """

    #: A short identifier for the schema, e.g. ``weather_report``.
    name: str
    #: The JSON Schema the response object must conform to.
    schema: dict[str, Any]
    #: Request exact schema adherence (no extra keys). OpenAI/LiteLLM enforce it.
    strict: bool = True


def json_schema_format(name: str, schema: dict[str, Any]) -> ResponseFormat:
    """A strict JSON-schema response format — Rust's ``ResponseFormat::json_schema``."""
    return ResponseFormat(name=name, schema=schema, strict=True)


def response_format_field(response_format: ResponseFormat | None) -> dict[str, Any]:
    """Splattable ``{"response_format": ...}`` mapping for a model request.

    ``None`` yields ``{}``, so the wire stays byte-identical when unset (Rust
    parity: the field is an ``Option`` and skipped when ``None``).
    """
    if response_format is None:
        return {}
    return {
        "response_format": {
            "type": "json_schema",
            "json_schema": {
                "name": response_format.name,
                "schema": response_format.schema,
                "strict": response_format.strict,
            },
        }
    }


def _snippet(text: str) -> str:
    """First 200 characters — matches the Rust reference's truncation."""
    return text[:200]


def structured_json(response: Any) -> Any:
    """Parse a completion's content as JSON.

    For a structured-output response this is the schema-conforming object the
    model produced.

    :raises ValueError: if the content is empty or is not valid JSON. The message
        quotes a truncated snippet of the offending content, so a model that
        ignored the schema is diagnosable from the error alone. Never silently
        returns an empty value.

    There is deliberately no ``deserialize_json``: Rust's version validates
    against a concrete type via ``serde``, and Python has no equivalent to do
    without dragging in a validation library. Parse here, validate with whatever
    the caller already uses.
    """
    content = (response.choices[0].message.content or "").strip()
    if not content:
        raise ValueError("structured output: model returned empty content (expected a JSON object)")
    try:
        return json.loads(content)
    except json.JSONDecodeError as error:
        raise ValueError(
            f"structured output: response content was not valid JSON ({error}): {_snippet(content)}"
        ) from error
