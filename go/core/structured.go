package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Structured output — a guaranteed-JSON answer conforming to a caller-supplied
// JSON Schema. Port of the Rust reference's ResponseFormat
// (rust/smooth-operator-core/src/llm.rs, SMOODEV-1472).
//
// Wire mapping (OpenAI-compatible, e.g. the LiteLLM gateway at llm.smoo.ai):
// serialized on /chat/completions as
//
//	response_format: { "type": "json_schema",
//	                   "json_schema": { name, schema, strict } }
//
// The Rust engine also has an Anthropic-native path (no response_format field
// there, so it forces a synthetic single tool call whose input_schema IS the
// requested schema). Go's GatewayClient speaks only the OpenAI-compatible
// endpoint, so there is nothing to mirror here.

// ResponseFormat constrains the shape of the model's response to a named JSON
// Schema. Rust models this as an enum with a single JsonSchema variant; Go uses
// a struct until a second variant exists.
type ResponseFormat struct {
	// Name is a short identifier for the schema (e.g. "weather_report").
	Name string
	// Schema is the JSON Schema the response object must conform to.
	Schema map[string]any
	// Strict requests exact schema adherence (no extra keys). OpenAI/LiteLLM
	// enforce it.
	Strict bool
}

// JSONSchemaFormat builds a strict JSON-schema response format — the Go
// analogue of Rust's ResponseFormat::json_schema.
func JSONSchemaFormat(name string, schema map[string]any) *ResponseFormat {
	return &ResponseFormat{Name: name, Schema: schema, Strict: true}
}

// snippet returns the first 200 characters (not bytes) of s, matching the Rust
// reference's truncation so both engines' error messages cut at the same point.
func snippet(s string) string {
	runes := []rune(s)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}

// StructuredJSON parses the response Content as a JSON value. For a structured-
// output response this is the schema-conforming object the model produced.
//
// Returns an error if Content is empty or is not valid JSON — the error carries
// a truncated snippet of the offending content so a model that ignored the
// schema can be diagnosed. It never silently returns an empty/nil value.
func (r ChatResponse) StructuredJSON() (any, error) {
	trimmed := strings.TrimSpace(r.Content)
	if trimmed == "" {
		return nil, fmt.Errorf("structured output: model returned empty content (expected a JSON object)")
	}
	var out any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("structured output: response content was not valid JSON (%w): %s", err, snippet(trimmed))
	}
	return out, nil
}

// DeserializeJSON parses the response Content into target, which must be a
// non-nil pointer. Convenience over StructuredJSON for the common case of
// decoding straight into a typed struct.
func (r ChatResponse) DeserializeJSON(target any) error {
	trimmed := strings.TrimSpace(r.Content)
	if trimmed == "" {
		return fmt.Errorf("structured output: model returned empty content (expected JSON for the requested type)")
	}
	if err := json.Unmarshal([]byte(trimmed), target); err != nil {
		return fmt.Errorf("structured output: could not deserialize response into the requested type (%w): %s", err, snippet(trimmed))
	}
	return nil
}
