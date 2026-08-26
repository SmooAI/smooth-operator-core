package core

import "testing"

// Regression for th-58db12: a streaming request MUST set
// stream_options.include_usage, or the gateway sends no usage chunk and token
// counts (hence per-turn cost) are lost. A non-streaming request must not carry it.
func TestStreamingRequestAsksForUsage(t *testing.T) {
	req := ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}

	stream := buildWireRequest(req, true)
	if stream.StreamOptions == nil || !stream.StreamOptions.IncludeUsage {
		t.Fatalf("streaming request must set stream_options.include_usage=true, got %+v", stream.StreamOptions)
	}

	nonStream := buildWireRequest(req, false)
	if nonStream.StreamOptions != nil {
		t.Fatalf("non-streaming request must omit stream_options, got %+v", nonStream.StreamOptions)
	}
}
