"""Structured output — request-side `response_format` and response-side parsing."""

from __future__ import annotations

import json
from types import SimpleNamespace

import pytest

from smooth_operator_core.structured import (
    json_schema_format,
    response_format_field,
    structured_json,
)

SCHEMA = {"type": "object", "properties": {"city": {"type": "string"}}}


def completion(content: str | None) -> SimpleNamespace:
    return SimpleNamespace(choices=[SimpleNamespace(message=SimpleNamespace(content=content))])


def test_json_schema_format_is_strict_by_default() -> None:
    fmt = json_schema_format("weather_report", SCHEMA)
    assert fmt.name == "weather_report"
    assert fmt.schema == SCHEMA
    assert fmt.strict is True


def test_response_format_field_renders_the_wire_object() -> None:
    assert response_format_field(json_schema_format("weather", SCHEMA)) == {
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": "weather", "schema": SCHEMA, "strict": True},
        }
    }


def test_unset_format_contributes_nothing() -> None:
    """The parity bar: unset leaves the request byte-identical."""
    assert response_format_field(None) == {}
    assert json.dumps({"model": "m", **response_format_field(None)}) == '{"model": "m"}'


def test_structured_json_parses_content() -> None:
    assert structured_json(completion('  {"city":"Indianapolis","high":82}  ')) == {
        "city": "Indianapolis",
        "high": 82,
    }


@pytest.mark.parametrize("content", ["", "   ", None])
def test_structured_json_rejects_empty_content(content: str | None) -> None:
    with pytest.raises(ValueError, match="empty content"):
        structured_json(completion(content))


def test_structured_json_rejects_non_json_and_quotes_it() -> None:
    with pytest.raises(ValueError, match="not valid JSON") as excinfo:
        structured_json(completion("I'm sorry, I can't do that."))
    # The offending content is quoted back, so a model that ignored the schema is
    # diagnosable from the error alone.
    assert "I'm sorry" in str(excinfo.value)


def test_structured_json_snippet_truncates_at_200_chars() -> None:
    with pytest.raises(ValueError) as excinfo:
        structured_json(completion("x" * 500))
    assert str(excinfo.value).endswith(": " + "x" * 200)
