"""SEP conformance replay — the Python host's side of the shared fixture suite.

The canonical fixtures live in the ``smooth-operator`` repo at
``spec/extension/conformance/fixtures.json`` and are validated there against the
JSON Schemas by the TypeScript conformance test. This package vendors a copy
(``tests/sep/fixtures.json``) and asserts the Python protocol types agree with it:
every typed method fixture parses and round-trips, and the ``$invalid`` instances
that violate a Python-enforced constraint are rejected. The Python sibling of the
Rust ``sep_conformance.rs``.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from smooth_operator_core.extension.protocol import (
    EventParams,
    HookOutcome,
    InitializeParams,
    InitializeResult,
    Message,
    ToolExecuteParams,
    ToolExecuteResult,
    ToolUpdateParams,
)

FIXTURES = json.loads((Path(__file__).parent / "sep" / "fixtures.json").read_text(encoding="utf-8"))


def _instance(name: str) -> Any:
    f = FIXTURES.get(name)
    assert f is not None and "instance" in f, f"fixture `{name}` missing or has no instance"
    return f["instance"]


def _assert_roundtrip(cls: Any, value: Any) -> None:
    """Parse into the typed struct, re-serialize, and re-parse — asserting the type
    round-trips the wire value losslessly."""
    a = cls.from_dict(value)
    b = cls.from_dict(a.to_dict())
    assert a == b, f"round-trip changed the value for {cls.__name__}"


def test_every_valid_fixture_is_well_formed() -> None:
    count = 0
    for name, f in FIXTURES.items():
        if name.startswith("$"):
            continue
        assert isinstance(f.get("$schema_ref"), str), f"{name}: missing $schema_ref"
        assert "instance" in f, f"{name}: missing instance"
        count += 1
    assert count >= 40, f"expected the full SEP fixture set, found {count}"


def test_lifecycle_method_fixtures_roundtrip_into_typed_structs() -> None:
    _assert_roundtrip(InitializeParams, _instance("initialize_params"))
    _assert_roundtrip(InitializeResult, _instance("initialize_result"))
    _assert_roundtrip(ToolExecuteParams, _instance("tool_execute_params"))
    _assert_roundtrip(ToolExecuteResult, _instance("tool_execute_result"))
    _assert_roundtrip(ToolUpdateParams, _instance("tool_update_params"))
    _assert_roundtrip(HookOutcome, _instance("hook_outcome_continue"))
    _assert_roundtrip(HookOutcome, _instance("hook_outcome_block"))
    _assert_roundtrip(HookOutcome, _instance("hook_outcome_modify"))
    _assert_roundtrip(EventParams, _instance("event_params"))
    _assert_roundtrip(EventParams, _instance("event_events_lost"))


def test_events_lost_marker_has_count_but_no_seq() -> None:
    normal = EventParams.from_dict(_instance("event_params"))
    assert normal.seq is not None, "a dispatched event is seq-numbered"

    lost = EventParams.from_dict(_instance("event_events_lost"))
    assert lost.event == "events_lost"
    assert lost.seq is None, "the events_lost marker is out-of-band (no seq)"
    assert lost.payload["lost"] == 12


def test_frame_fixtures_parse_and_classify() -> None:
    req = Message.from_dict(_instance("frame_request"))
    assert req.is_request()

    note = Message.from_dict(_instance("frame_notification"))
    assert note.is_notification()

    ok = Message.from_dict(_instance("frame_success_response"))
    assert ok.is_response() and ok.result is not None

    for name in ["frame_error_response", "error_blocked", "error_cancelled", "error_context_violation"]:
        err = Message.from_dict(_instance(name))
        assert err.error is not None, f"{name} should carry an error object"


def test_invalid_fixtures_that_violate_a_python_constraint_are_rejected() -> None:
    invalid = {e["name"]: e["instance"] for e in FIXTURES["$invalid"]}

    # Required field missing -> typed struct rejects (KeyError).
    with pytest.raises(KeyError):
        InitializeParams.from_dict(invalid["initialize_params_missing_protocol_version"])
    with pytest.raises(KeyError):
        ToolExecuteParams.from_dict(invalid["tool_execute_params_missing_call_id"])
    # Tagged-union constraints.
    with pytest.raises(ValueError):
        HookOutcome.from_dict(invalid["hook_outcome_bogus_action"])
    with pytest.raises(ValueError):
        HookOutcome.from_dict(invalid["hook_outcome_modify_missing_patch"])
