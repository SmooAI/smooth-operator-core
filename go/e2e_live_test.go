// Package e2e holds the live, gateway-backed end-to-end test for the Go client.
//
// Unlike the unit tests under ./protocol (which drive an in-memory mock
// transport), this test boots the REAL Rust smooth-operator-server binary,
// connects to it over a real WebSocket via the default WebSocketTransport, and
// drives real LLM turns through the live SmooAI gateway.
//
// # Gating (safe in CI / without creds)
//
// The test is a no-op unless BOTH of these are set:
//   - SMOOTH_AGENT_E2E=1
//   - SMOOAI_GATEWAY_KEY=<key>   (read from env; never printed)
//
// `go test ./...` with no env still passes — this test calls t.Skip.
//
// # Run locally (does not print the key)
//
//	export SMOOAI_GATEWAY_KEY=$(python3 -c \
//	  "import json;print(json.load(open('$HOME/.local/share/opencode/auth.json'))['smooai']['key'])")
//	export SMOOTH_AGENT_E2E=1
//	go test -run TestLive -v ./...
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SmooAI/smooth-operator-core/go/protocol"
)

const (
	// e2ePort is the fixed TCP port the spawned server binds on 127.0.0.1.
	e2ePort = "8811"
	e2eAddr = "127.0.0.1:" + e2ePort
	e2eURL  = "ws://" + e2eAddr + "/ws"

	// serverBin is the prebuilt Rust server binary. If missing, build with:
	//   cargo build -p smooai-smooth-operator-server \
	//     --bin smooth-operator-server
	// (run from rust/).
	serverBinRelHome = ".cargo/shared-target/debug/smooth-operator-server"

	// turnTimeout bounds a single live LLM turn (gateway + tool loop can be slow).
	turnTimeout = 120 * time.Second
	// bootTimeout bounds how long we wait for the server to start listening.
	bootTimeout = 30 * time.Second
)

// gate returns the gateway key, or skips the test if the E2E gates are not set.
// It NEVER logs the key value.
func gate(t *testing.T) string {
	t.Helper()
	if os.Getenv("SMOOTH_AGENT_E2E") != "1" {
		t.Skip("SMOOTH_AGENT_E2E != \"1\" — skipping live-gateway WS E2E")
	}
	key := strings.TrimSpace(os.Getenv("SMOOAI_GATEWAY_KEY"))
	if key == "" {
		t.Skip("SMOOAI_GATEWAY_KEY unset/empty — skipping live-gateway WS E2E")
	}
	return key
}

// serverBinPath resolves the absolute path to the prebuilt server binary.
func serverBinPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	path := home + "/" + serverBinRelHome
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("server binary not found at %s — build it with:\n"+
			"  cargo build -p smooai-smooth-operator-server --bin smooth-operator-server\n"+
			"(from rust/): %v", path, err)
	}
	return path
}

// bootServer spawns the Rust server with the live-E2E env and waits until it is
// accepting connections on e2eAddr. It registers cleanup that kills the process.
// The gateway key is passed only into the child env — never logged.
func bootServer(t *testing.T, key string) {
	t.Helper()
	bin := serverBinPath(t)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"SMOOTH_AGENT_PORT="+e2ePort,
		"SMOOTH_AGENT_SEED_KB=1",
		"SMOOTH_AGENT_MODEL=claude-haiku-4-5",
		"SMOOAI_GATEWAY_KEY="+key,
	)

	// Surface the server's stdout/stderr (minus the key, which the server never
	// prints) so a live run shows the "listening" line and any startup errors.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server binary: %v", err)
	}

	go pipeLines(t, "server/stdout", stdout)
	go pipeLines(t, "server/stderr", stderr)

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	// Wait for readiness by dialing the TCP port in a loop.
	deadline := time.Now().Add(bootTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", e2eAddr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Logf("[live-ws] server ready on %s", e2eAddr)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s within %s", e2eAddr, bootTimeout)
}

// pipeLines forwards a child stream line-by-line into the test log.
func pipeLines(t *testing.T, tag string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		t.Logf("[%s] %s", tag, sc.Text())
	}
}

// finalText extracts the assistant's reply text from an eventual_response. The
// runner places the reply in data.data.response.responseParts[]; fall back to a
// raw-JSON scan if the shape differs.
func finalText(t *testing.T, final protocol.EventualResponse) string {
	t.Helper()
	// final.Data.Data.Response is interface{}; re-marshal and decode the parts.
	raw, err := json.Marshal(final.Data.Data.Response)
	if err != nil {
		return ""
	}
	var shaped struct {
		ResponseParts []string `json:"responseParts"`
	}
	if err := json.Unmarshal(raw, &shaped); err == nil && len(shaped.ResponseParts) > 0 {
		return strings.Join(shaped.ResponseParts, " ")
	}
	// Fallback: the response may be a bare string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// runTurn sends one message and drains the streamed events, returning the count
// of stream_token/stream_chunk events seen and the terminal eventual_response.
func runTurn(t *testing.T, client *protocol.Client, sessionID, message string) (streamed int, reply string) {
	t.Helper()
	turn := client.SendMessage(protocol.SendMessageParams{
		SessionID: sessionID,
		Message:   message,
	})

	var tokenSample strings.Builder
	for ev := range turn.Events() {
		switch ev.Type {
		case protocol.EventStreamToken:
			streamed++
			if tokenSample.Len() < 200 {
				if tok, derr := ev.AsStreamToken(); derr == nil {
					if tok.Token != nil {
						tokenSample.WriteString(*tok.Token)
					} else {
						tokenSample.WriteString(tok.Data.Token)
					}
				}
			}
		case protocol.EventStreamChunk:
			streamed++
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()
	final, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("turn did not complete for message %q: %v", message, err)
	}
	reply = finalText(t, final)
	t.Logf("[live-ws] streamed %d token/chunk events; token sample: %q", streamed, tokenSample.String())
	t.Logf("[live-ws] reply: %q", reply)
	return streamed, reply
}

// TestLiveWSKnowledgeAndMemory boots the real Rust WS server and drives real LLM
// turns through it via the Go client over a real WebSocket:
//
//  1. knowledge grounding — the seeded "17-day return window" fact is retrieved
//     and the streamed + terminal reply contains "17".
//  2. per-session memory — "My name is Zog" then "What is my name?" recalls "Zog".
func TestLiveWSKnowledgeAndMemory(t *testing.T) {
	key := gate(t)
	bootServer(t, key)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport := protocol.NewWebSocketTransport(e2eURL, nil)
	client, err := protocol.New(protocol.Options{Transport: transport})
	if err != nil {
		t.Fatalf("construct client: %v", err)
	}
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect to %s: %v", e2eURL, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// ---- Session ----
	sess, err := client.CreateConversationSession(ctx, protocol.CreateConversationSessionParams{
		AgentID:  "e2e",
		UserName: "Zog E2E",
	})
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	if sess.SessionID == "" {
		t.Fatalf("expected a non-empty sessionId, got: %+v", sess)
	}
	t.Logf("[live-ws] session: %s", sess.SessionID)

	// ---- Turn 1: knowledge-grounded ("17"-day return window) ----
	streamed1, reply1 := runTurn(t, client, sess.SessionID,
		"What is SmooAI's return window? Search the knowledge base.")
	if streamed1 < 1 {
		t.Fatalf("expected >=1 stream_token/stream_chunk event in turn 1, got %d", streamed1)
	}
	if !strings.Contains(reply1, "17") {
		t.Fatalf("expected grounded answer to contain the retrieved 17-day fact, got: %q", reply1)
	}

	// ---- Turn 2 + 3: per-session memory ("Zog") ----
	_, reply2 := runTurn(t, client, sess.SessionID, "My name is Zog. Remember it.")
	t.Logf("[live-ws] turn 2 ack: %q", reply2)

	_, reply3 := runTurn(t, client, sess.SessionID, "What is my name?")
	if !strings.Contains(strings.ToUpper(reply3), "ZOG") {
		t.Fatalf("expected per-session memory: turn 3 should recall 'Zog', got: %q", reply3)
	}

	t.Logf("[live-ws] PASS: knowledge grounding (17) + per-session memory (Zog) verified")
}
