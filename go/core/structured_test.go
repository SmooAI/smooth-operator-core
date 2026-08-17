package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
	}
}

func TestJSONSchemaFormatIsStrictByDefault(t *testing.T) {
	f := JSONSchemaFormat("weather_report", schema())
	if f.Name != "weather_report" || !f.Strict {
		t.Fatalf("expected a strict named format, got %+v", f)
	}
}

func TestWireRequestCarriesResponseFormat(t *testing.T) {
	wreq := buildWireRequest(ChatRequest{Model: "m", ResponseFormat: JSONSchemaFormat("weather", schema())}, false)
	body, err := json.Marshal(wreq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing: %s", body)
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("type = %v", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema missing: %s", body)
	}
	if js["name"] != "weather" || js["strict"] != true {
		t.Fatalf("json_schema = %+v", js)
	}
	if _, ok := js["schema"].(map[string]any); !ok {
		t.Fatalf("schema not carried: %+v", js)
	}
}

// The parity bar: an unset format must leave the request byte-identical, not
// emit `"response_format":null`.
func TestWireRequestOmitsResponseFormatWhenUnset(t *testing.T) {
	body, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m"}, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "response_format") {
		t.Fatalf("unset format must be omitted from the wire: %s", body)
	}
}

func TestStructuredJSONParsesContent(t *testing.T) {
	resp := ChatResponse{Content: `  {"city":"Indianapolis","high":82}  `}
	value, err := resp.StructuredJSON()
	if err != nil {
		t.Fatalf("StructuredJSON: %v", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", value)
	}
	if obj["city"] != "Indianapolis" {
		t.Fatalf("city = %v", obj["city"])
	}
}

func TestStructuredJSONRejectsEmptyContent(t *testing.T) {
	_, err := ChatResponse{Content: "   "}.StructuredJSON()
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("expected an empty-content error, got %v", err)
	}
}

func TestStructuredJSONRejectsNonJSONAndQuotesIt(t *testing.T) {
	_, err := ChatResponse{Content: "I'm sorry, I can't do that."}.StructuredJSON()
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("expected an invalid-JSON error, got %v", err)
	}
	// The offending content is quoted back so a model ignoring the schema is
	// diagnosable from the error alone.
	if !strings.Contains(err.Error(), "I'm sorry") {
		t.Fatalf("error should include the content snippet: %v", err)
	}
}

func TestStructuredJSONSnippetTruncatesAt200Chars(t *testing.T) {
	_, err := ChatResponse{Content: strings.Repeat("x", 500)}.StructuredJSON()
	if err == nil {
		t.Fatal("expected an error")
	}
	// The snippet is the tail of the message; count it there rather than over the
	// whole string, which also contains encoding/json's own "invalid character 'x'".
	if !strings.HasSuffix(err.Error(), ": "+strings.Repeat("x", 200)) {
		t.Fatalf("snippet should be the trailing 200 chars: %v", err)
	}
}

func TestDeserializeJSONIntoStruct(t *testing.T) {
	var out struct {
		City string `json:"city"`
		High int    `json:"high"`
	}
	if err := (ChatResponse{Content: `{"city":"Indianapolis","high":82}`}).DeserializeJSON(&out); err != nil {
		t.Fatalf("DeserializeJSON: %v", err)
	}
	if out.City != "Indianapolis" || out.High != 82 {
		t.Fatalf("decoded = %+v", out)
	}
}

func TestDeserializeJSONReportsTypeMismatch(t *testing.T) {
	var out struct {
		High int `json:"high"`
	}
	err := (ChatResponse{Content: `{"high":"eighty-two"}`}).DeserializeJSON(&out)
	if err == nil || !strings.Contains(err.Error(), "could not deserialize") {
		t.Fatalf("expected a deserialize error, got %v", err)
	}
}

func TestDeserializeJSONRejectsEmptyContent(t *testing.T) {
	var out map[string]any
	err := (ChatResponse{Content: ""}).DeserializeJSON(&out)
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("expected an empty-content error, got %v", err)
	}
}

func TestMockRecordsTheRequestedResponseFormat(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushText(`{"city":"Indianapolis"}`)

	format := JSONSchemaFormat("weather", schema())
	if _, err := mock.Chat(context.Background(), ChatRequest{Model: "m", ResponseFormat: format}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	call, ok := mock.LastCall()
	if !ok {
		t.Fatal("no call recorded")
	}
	if call.ResponseFormat == nil || call.ResponseFormat.Name != "weather" {
		t.Fatalf("format not recorded: %+v", call.ResponseFormat)
	}
}

func TestPlainChatRecordsNoResponseFormat(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushText("hi")
	if _, err := mock.Chat(context.Background(), ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	call, _ := mock.LastCall()
	if call.ResponseFormat != nil {
		t.Fatalf("plain chat should record no format: %+v", call.ResponseFormat)
	}
}
