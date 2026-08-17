package temporal

import (
	"context"
	"os"
	"testing"
	"time"

	core "github.com/SmooAI/smooth-operator-core/go/core"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

// devServerClient returns a Temporal client for the e2e tests, or SKIPS the test
// when no Temporal is reachable — mirroring the Rust backend's self-skipping
// ephemeral-server tests (and the engine's Docker-gated Postgres tests).
//
// Two modes:
//   - TEMPORAL_ADDRESS set → dial that existing server (e.g. a testcontainer or a
//     `temporal server start-dev` a developer already has running).
//   - otherwise → start an ephemeral dev server in-process (the SDK auto-downloads
//     and caches the Temporal CLI on first run — no Docker, no manual install). If
//     that can't be downloaded/started (offline / blocked CI), the test skips.
//
// The returned client and any server are torn down via t.Cleanup.
func devServerClient(t *testing.T) client.Client {
	t.Helper()

	if addr := os.Getenv("TEMPORAL_ADDRESS"); addr != "" {
		c, err := client.Dial(client.Options{HostPort: addr})
		if err != nil {
			t.Skipf("SKIP: TEMPORAL_ADDRESS=%s set but could not connect: %v", addr, err)
		}
		t.Cleanup(c.Close)
		return c
	}

	// A generous window covers the one-time CLI binary download on first run.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	server, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{
		ClientOptions: &client.Options{},
		LogLevel:      "error",
	})
	if err != nil {
		t.Skipf("SKIP: could not start ephemeral Temporal dev server (likely offline / blocked download): %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })

	c := server.Client()
	t.Cleanup(c.Close)
	return c
}

// lastAssistantContent returns the content of the last assistant message in a
// conversation, or "" if there is none — the Go analog of the Rust
// Conversation::last_assistant_content the e2e tests assert on.
func lastAssistantContent(messages []core.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return messages[i].Content
		}
	}
	return ""
}

// toolMessages returns the tool-role messages in a conversation, in order.
func toolMessages(messages []core.ChatMessage) []core.ChatMessage {
	var out []core.ChatMessage
	for _, m := range messages {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}
