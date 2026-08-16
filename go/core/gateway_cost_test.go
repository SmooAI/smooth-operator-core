package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gateway reports per-request cost ONLY in a response header. These drive the
// real GatewayClient against a local server so the header genuinely has to survive
// the HTTP round-trip — including on the streaming path, where the headers are gone
// once the SSE body is being scanned (the bug core#102 fixed in Rust).

func TestParseGatewayCostPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    *float64
	}{
		{"margin wins over original and legacy", map[string]string{
			"x-litellm-response-cost-margin-amount": "3.0e-05",
			"x-litellm-response-cost-original":      "1.0e-05",
			"x-litellm-response-cost":               "9.0e-05",
		}, ptr(3.0e-05)},
		{"original wins over legacy", map[string]string{
			"x-litellm-response-cost-original": "1.0e-05",
			"x-litellm-response-cost":          "9.0e-05",
		}, ptr(1.0e-05)},
		{"legacy shape still read", map[string]string{
			"x-litellm-response-cost": "1.47e-05",
		}, ptr(1.47e-05)},
		{"generic fallbacks", map[string]string{"x-response-cost": "0.5"}, ptr(0.5)},
		{"generic cost-usd fallback", map[string]string{"x-cost-usd": "0.25"}, ptr(0.25)},

		// The distinction the whole fix rests on: absent and zero are BOTH
		// "unmeasured", never a recorded $0.
		{"absent -> nil", map[string]string{}, nil},
		{"zero -> nil, not 0", map[string]string{"x-litellm-response-cost": "0"}, nil},
		{"zero margin falls through to a real original", map[string]string{
			"x-litellm-response-cost-margin-amount": "0",
			"x-litellm-response-cost-original":      "2.5e-05",
		}, ptr(2.5e-05)},
		{"unparseable -> nil", map[string]string{"x-litellm-response-cost": "not-a-number"}, nil},
		{"negative -> nil", map[string]string{"x-litellm-response-cost": "-1"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.headers {
				h.Set(k, v)
			}
			got := parseGatewayCost(h)
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("want nil, got %v", *got)
			case c.want != nil && got == nil:
				t.Fatalf("want %v, got nil", *c.want)
			case c.want != nil && *got != *c.want:
				t.Fatalf("got %v, want %v", *got, *c.want)
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }

// gatewayStub serves one chat completion, optionally with a cost header, in either
// plain-JSON or SSE mode.
func gatewayStub(t *testing.T, costHeader string, streaming bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if costHeader != "" {
			w.Header().Set("x-litellm-response-cost", costHeader)
		}
		if !streaming {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestNonStreamingReadsCostHeader(t *testing.T) {
	srv := gatewayStub(t, "1.47e-05", false)
	defer srv.Close()
	client := NewGatewayClient(srv.URL, "k")

	resp, err := client.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.GatewayCostUSD == nil {
		t.Fatal("cost header did not reach ChatResponse")
	}
	if *resp.GatewayCostUSD != 1.47e-05 {
		t.Errorf("cost = %v", *resp.GatewayCostUSD)
	}
}

func TestNonStreamingNoHeaderLeavesCostNil(t *testing.T) {
	srv := gatewayStub(t, "", false)
	defer srv.Close()
	client := NewGatewayClient(srv.URL, "k")

	resp, err := client.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.GatewayCostUSD != nil {
		t.Errorf("absent header must leave cost nil (unmeasured), got %v", *resp.GatewayCostUSD)
	}
}

func TestStreamingReadsCostHeaderBeforeBody(t *testing.T) {
	// The core#102 regression: the streaming path consumed the SSE body without
	// ever reading the headers, so cost was always nil on the path every real turn
	// takes.
	srv := gatewayStub(t, "4.2e-03", true)
	defer srv.Close()
	client := NewGatewayClient(srv.URL, "k")

	chunks, err := client.ChatStream(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var cost *float64
	for chunk := range chunks {
		if chunk.CostUSD != nil {
			cost = chunk.CostUSD
		}
	}
	if cost == nil {
		t.Fatal("cost header did not survive to the stream consumer")
	}
	if *cost != 4.2e-03 {
		t.Errorf("cost = %v", *cost)
	}
}

func TestStreamingNoHeaderLeavesCostNil(t *testing.T) {
	srv := gatewayStub(t, "", true)
	defer srv.Close()
	client := NewGatewayClient(srv.URL, "k")

	chunks, err := client.ChatStream(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for chunk := range chunks {
		if chunk.CostUSD != nil {
			t.Errorf("absent header must leave cost nil, got %v", *chunk.CostUSD)
		}
	}
}

func TestGatewayCostBeatsLocalPricingInTheTurn(t *testing.T) {
	// The end of the chain: an authoritative header cost must land on the turn's
	// CostUSD instead of the local ModelPricing estimate.
	tracker := &CostTracker{}
	pricing := map[string]ModelPricing{"m": {InputPerMTok: 1000, OutputPerMTok: 1000}}
	usage := Usage{PromptTokens: 10, CompletionTokens: 5}

	tracker.RecordWithGatewayCost("m", usage, ptr(0.25), pricing)
	if tracker.CostUSD != 0.25 {
		t.Errorf("gateway cost = %v, want 0.25", tracker.CostUSD)
	}

	// nil (unmeasured) falls back to local pricing rather than recording nothing.
	local := &CostTracker{}
	local.RecordWithGatewayCost("m", usage, nil, pricing)
	if local.CostUSD == 0 {
		t.Error("a nil gateway cost must fall back to local ModelPricing, not record 0")
	}
}
