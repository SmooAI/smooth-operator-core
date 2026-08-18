package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// The OpenAI streaming API OMITS usage unless it is explicitly requested, so a stream
// built without `stream_options.include_usage` carries no usage chunk at all. That was
// never the gateway losing data — it was the gateway honouring a request that never
// asked. Verified against llm.smoo.ai (LiteLLM 1.95.0, groq-gpt-oss-120b): 0 chunks
// carry usage without the field, 1 carries real prompt/completion counts with it.
// Pearl th-5e59a5.
func TestStreamRequestAsksForUsage(t *testing.T) {
	streamed, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m"}, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(streamed), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("a streaming request must ask for usage, or none is sent: %s", streamed)
	}

	// Meaningless on a non-streaming request, and omitting it keeps that wire
	// byte-identical to a client without the field.
	plain, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "stream_options") {
		t.Fatalf("non-streaming request must not carry stream_options: %s", plain)
	}
}
