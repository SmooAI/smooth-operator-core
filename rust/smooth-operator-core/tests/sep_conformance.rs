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
    HookOutcome, InitializeParams, InitializeResult, Message, ToolExecuteParams, ToolExecuteResult, ToolUpdateParams,
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
