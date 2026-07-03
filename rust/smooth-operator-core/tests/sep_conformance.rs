//! SEP conformance replay — the Rust host's side of the shared fixture suite.
//!
//! The canonical fixtures live in the `smooth-operator` repo at
//! `spec/extension/conformance/fixtures.json` and are validated there against
//! the JSON Schemas by the TypeScript conformance test. This crate vendors a
//! copy (`tests/sep/fixtures.json`) and asserts the Rust protocol types agree
//! with it: every typed method fixture deserializes and round-trips, and the
//! `$invalid` instances that violate a Rust-enforced constraint are rejected.
//!
//! ponytail: the fixtures are a vendored copy, kept honest by this test failing
//! the moment the Rust types drift from the wire. A CI job that diffs the copy
//! against the spec repo is the eventual belt-and-suspenders; not needed to
//! catch type drift, which is what actually breaks interop.

use serde::{de::DeserializeOwned, Serialize};
use serde_json::Value;
use smooth_operator_core::extension::protocol::{
    EventParams, HookOutcome, InitializeParams, InitializeResult, Message, ProviderCompleteParams, ProviderCompleteResult, ProviderCredentials,
    ProviderDeltaParams, Registrations, SessionSetModelParams, ToolExecuteParams, ToolExecuteResult, ToolUpdateParams,
};

const FIXTURES: &str = include_str!("sep/fixtures.json");

fn fixtures() -> Value {
    serde_json::from_str(FIXTURES).expect("fixtures.json parses")
}

fn instance(all: &Value, name: &str) -> Value {
    all.get(name)
        .and_then(|f| f.get("instance"))
        .cloned()
        .unwrap_or_else(|| panic!("fixture `{name}` missing or has no instance"))
}

/// Deserialize into `T`, re-serialize, and re-deserialize — asserting the type
/// round-trips the wire value losslessly.
fn assert_roundtrip<T>(v: &Value)
where
    T: DeserializeOwned + Serialize + PartialEq + std::fmt::Debug,
{
    let a: T = serde_json::from_value(v.clone()).expect("fixture deserializes into the typed struct");
    let s = serde_json::to_string(&a).expect("serialize");
    let b: T = serde_json::from_str(&s).expect("re-deserialize");
    assert_eq!(a, b, "round-trip changed the value");
}

#[test]
fn every_valid_fixture_is_well_formed_json() {
    let all = fixtures();
    let obj = all.as_object().expect("top-level object");
    let mut count = 0;
    for (name, f) in obj {
        if name.starts_with('$') {
            continue;
        }
        assert!(f.get("$schema_ref").and_then(Value::as_str).is_some(), "{name}: missing $schema_ref");
        assert!(f.get("instance").is_some(), "{name}: missing instance");
        count += 1;
    }
    assert!(count >= 40, "expected the full SEP fixture set, found {count}");
}

#[test]
fn lifecycle_method_fixtures_roundtrip_into_typed_structs() {
    let all = fixtures();
    assert_roundtrip::<InitializeParams>(&instance(&all, "initialize_params"));
    assert_roundtrip::<InitializeResult>(&instance(&all, "initialize_result"));
    assert_roundtrip::<ToolExecuteParams>(&instance(&all, "tool_execute_params"));
    assert_roundtrip::<ToolExecuteResult>(&instance(&all, "tool_execute_result"));
    assert_roundtrip::<ToolUpdateParams>(&instance(&all, "tool_update_params"));
    assert_roundtrip::<HookOutcome>(&instance(&all, "hook_outcome_continue"));
    assert_roundtrip::<HookOutcome>(&instance(&all, "hook_outcome_block"));
    assert_roundtrip::<HookOutcome>(&instance(&all, "hook_outcome_modify"));
    // Phase 2: seq-numbered event + the out-of-band events_lost marker (no seq).
    assert_roundtrip::<EventParams>(&instance(&all, "event_params"));
    assert_roundtrip::<EventParams>(&instance(&all, "event_events_lost"));
}

/// Phase 7: the provider registration + proxied-streaming + set_model fixtures
/// deserialize into the Rust host's typed view. `ProviderCompleteResult` has no
/// `PartialEq` (ToolCall/Usage don't), so it's checked by deserialize + field
/// assertion rather than `assert_roundtrip`.
#[test]
fn provider_method_fixtures_agree_with_the_rust_types() {
    let all = fixtures();
    assert_roundtrip::<Registrations>(&instance(&all, "registrations_with_provider"));
    assert_roundtrip::<ProviderCompleteParams>(&instance(&all, "provider_complete_params"));
    assert_roundtrip::<ProviderDeltaParams>(&instance(&all, "provider_delta_params"));
    assert_roundtrip::<ProviderCredentials>(&instance(&all, "provider_credentials"));
    assert_roundtrip::<SessionSetModelParams>(&instance(&all, "session_set_model_params_provider"));

    let plain: ProviderCompleteResult = serde_json::from_value(instance(&all, "provider_complete_result")).expect("complete_result");
    assert_eq!(plain.content, "Hello there.");
    assert_eq!(plain.finish_reason, "stop");
    assert_eq!(plain.resolved_model.as_deref(), Some("corp-gpt-4o-2026"));

    let with_calls: ProviderCompleteResult = serde_json::from_value(instance(&all, "provider_complete_result_tool_calls")).expect("tool_calls result");
    assert_eq!(with_calls.tool_calls.len(), 1);
    assert_eq!(with_calls.tool_calls[0].name, "get_weather");
    assert_eq!(with_calls.finish_reason, "tool_calls");
}

/// The seq-gap hardening, asserted on the wire shape: a normal event carries a
/// `seq`; the `events_lost` marker carries a count and context but no `seq`.
#[test]
fn events_lost_marker_has_count_but_no_seq() {
    let all = fixtures();
    let normal: EventParams = serde_json::from_value(instance(&all, "event_params")).expect("event_params");
    assert!(normal.seq.is_some(), "a dispatched event is seq-numbered");

    let lost: EventParams = serde_json::from_value(instance(&all, "event_events_lost")).expect("events_lost");
    assert_eq!(lost.event, "events_lost");
    assert!(lost.seq.is_none(), "the events_lost marker is out-of-band (no seq)");
    assert_eq!(lost.payload.and_then(|p| p.get("lost").and_then(serde_json::Value::as_u64)), Some(12));
}

#[test]
fn frame_fixtures_parse_and_classify() {
    let all = fixtures();

    let req: Message = serde_json::from_value(instance(&all, "frame_request")).expect("request frame");
    assert!(req.is_request(), "frame_request should classify as a request");

    let note: Message = serde_json::from_value(instance(&all, "frame_notification")).expect("notification frame");
    assert!(note.is_notification(), "frame_notification should classify as a notification");

    let ok: Message = serde_json::from_value(instance(&all, "frame_success_response")).expect("success frame");
    assert!(ok.is_response() && ok.result.is_some());

    for name in ["frame_error_response", "error_blocked", "error_cancelled", "error_context_violation"] {
        let err: Message = serde_json::from_value(instance(&all, name)).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert!(err.error.is_some(), "{name} should carry an error object");
    }
}

#[test]
fn invalid_fixtures_that_violate_a_rust_constraint_are_rejected() {
    let all = fixtures();
    let invalid = all.get("$invalid").and_then(Value::as_array).expect("$invalid array");

    let find = |name: &str| -> Value {
        invalid
            .iter()
            .find(|e| e.get("name").and_then(Value::as_str) == Some(name))
            .and_then(|e| e.get("instance"))
            .cloned()
            .unwrap_or_else(|| panic!("invalid fixture `{name}` missing"))
    };

    // Required field missing → typed struct rejects.
    assert!(serde_json::from_value::<InitializeParams>(find("initialize_params_missing_protocol_version")).is_err());
    assert!(serde_json::from_value::<ToolExecuteParams>(find("tool_execute_params_missing_call_id")).is_err());
    // Tagged-enum constraints.
    assert!(serde_json::from_value::<HookOutcome>(find("hook_outcome_bogus_action")).is_err());
    assert!(serde_json::from_value::<HookOutcome>(find("hook_outcome_modify_missing_patch")).is_err());
    // The two frame-level invalids (`jsonrpc: "1.0"`, missing `method`) are
    // rejected by the JSON *Schema* `const`/`required`, not by the permissive
    // JSON-RPC envelope type — the spec repo's TS conformance covers those.
}
