package core

import (
	"context"
	"strings"
	"testing"
)

// Retry-with-exponential-backoff around the model call, driven by the reusable
// MockLlmProvider (it can script a transient error via PushError). RetryBackoff is
// left at its zero value so no real time is spent sleeping.

func TestRetriesThenSucceeds(t *testing.T) {
	// Errors k times then a text reply; MaxRetries >= k → the turn succeeds and the
	// model is called exactly k+1 times.
	mock := NewMockLlmProvider()
	mock.PushError("rate limited").PushError("rate limited").PushText("ok")
	agent := NewSmoothAgent(mock, AgentOptions{MaxRetries: 2})
	res, err := agent.Run(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("want text %q, got %q", "ok", res.Text)
	}
	if mock.CallCount() != 3 { // k+1 = 2 failures + 1 success
		t.Fatalf("want 3 model calls, got %d", mock.CallCount())
	}
}

func TestErrorPropagatesWhenRetriesExhausted(t *testing.T) {
	// Errors MaxRetries+1 times → the provider error propagates (the turn fails).
	mock := NewMockLlmProvider()
	mock.PushError("boom").PushError("boom")
	agent := NewSmoothAgent(mock, AgentOptions{MaxRetries: 1})
	_, err := agent.Run(context.Background(), "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want error containing %q, got %v", "boom", err)
	}
	if mock.CallCount() != 2 { // MaxRetries + 1 attempts
		t.Fatalf("want 2 model calls, got %d", mock.CallCount())
	}
}

func TestNoRetryByDefault(t *testing.T) {
	// Default MaxRetries=0 → a single error propagates immediately (one attempt).
	mock := NewMockLlmProvider()
	mock.PushError("nope").PushText("never reached")
	agent := NewSmoothAgent(mock, AgentOptions{})
	_, err := agent.Run(context.Background(), "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want error containing %q, got %v", "nope", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("want 1 model call, got %d", mock.CallCount())
	}
}
