package extension

// The SEP fixture-replay peer, as a self-re-exec of the test binary.
//
// TestMain checks SEP_ECHO_PEER: when set, the process acts as a dependency-free
// SEP extension (JSON-RPC 2.0 ndjson over stdio) instead of running the tests, so
// the live tests can spawn a real subprocess without building a separate binary
// or depending on node. Behavior is tuned by env vars:
//
//	SEP_ECHO_HOOK        continue (default) | block | modify | hang
//	SEP_ECHO_TOOL        echo (default)     | hang  | ui
//	SEP_ECHO_CRASH_HOOK  "1" → exit(1) on the first hook (simulate a crash)

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SEP_ECHO_PEER") == "1" {
		runEchoPeer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func peerWrite(v any) {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	_, _ = os.Stdout.Write(b)
}

func peerReply(id json.RawMessage, result any) {
	rb, _ := json.Marshal(result)
	peerWrite(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(rb)})
}

func peerReplyError(id json.RawMessage, code int, message string) {
	peerWrite(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func runEchoPeer() {
	hookMode := os.Getenv("SEP_ECHO_HOOK")
	toolMode := os.Getenv("SEP_ECHO_TOOL")
	crashHook := os.Getenv("SEP_ECHO_CRASH_HOOK") == "1"

	var pendingToolID json.RawMessage // set when awaiting our ui/request reply

	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			var f struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal([]byte(line), &f) == nil {
				handlePeerFrame(f, hookMode, toolMode, crashHook, &pendingToolID)
			}
		}
		if err != nil {
			return
		}
	}
}

func handlePeerFrame(f struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}, hookMode, toolMode string, crashHook bool, pendingToolID *json.RawMessage) {
	isRequest := len(f.ID) > 0 && f.Method != ""
	isResponse := len(f.ID) > 0 && f.Method == ""

	// A response to our own ui/request (id "ui-1"): now answer the parked tool.
	if isResponse && string(f.ID) == `"ui-1"` && *pendingToolID != nil {
		toolID := *pendingToolID
		*pendingToolID = nil
		peerReply(toolID, map[string]any{"content": "ui:" + string(f.Result)})
		return
	}

	switch f.Method {
	case MethodInitialize:
		peerReply(f.ID, map[string]any{
			"protocol_version": 1,
			"extension":        map[string]any{"name": "echo", "version": "0.1.0"},
			"registrations": map[string]any{
				"tools": []any{map[string]any{
					"name":        "say",
					"description": "Echo a phrase back.",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{"phrase": map[string]any{"type": "string"}}, "required": []string{"phrase"}},
				}},
				"commands":      []any{map[string]any{"name": "echo-cmd", "description": "Echo a slash-command back."}},
				"shortcuts":     []any{map[string]any{"key": "ctrl+e", "command": "echo-cmd"}},
				"subscriptions": []string{"turn_start", "turn_end", "message_end"},
			},
		})
	case MethodPing:
		peerReply(f.ID, map[string]any{})
	case MethodHook:
		if crashHook {
			os.Exit(1)
		}
		switch hookMode {
		case "block":
			peerReply(f.ID, map[string]any{"action": "block", "reason": "peer blocked"})
		case "modify":
			peerReply(f.ID, map[string]any{"action": "modify", "patch": map[string]any{"system_prompt": "patched"}})
		case "hang":
			// no reply — exercises the host's hook timeout.
		default:
			peerReply(f.ID, map[string]any{"action": "continue"})
		}
	case MethodToolExecute:
		switch toolMode {
		case "hang":
			// no reply — exercises the host's tool/execute timeout + $/cancel.
		case "ui":
			*pendingToolID = f.ID
			peerWrite(map[string]any{"jsonrpc": "2.0", "id": "ui-1", "method": MethodUIRequest,
				"params": map[string]any{"kind": "confirm", "prompt": "peer asks"}})
		default:
			var p struct {
				Arguments struct {
					Phrase string `json:"phrase"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(f.Params, &p)
			peerReply(f.ID, map[string]any{"content": p.Arguments.Phrase})
		}
	case MethodCommandExecute:
		var p struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(f.Params, &p)
		peerReply(f.ID, map[string]any{"content": "ran " + p.Command})
	case MethodCommandComplete:
		var p struct {
			Partial string `json:"partial"`
		}
		_ = json.Unmarshal(f.Params, &p)
		peerReply(f.ID, map[string]any{"completions": []any{map[string]any{"value": p.Partial + "-done"}}})
	case MethodShutdown:
		peerReply(f.ID, map[string]any{})
		os.Exit(0)
	case MethodEvent, MethodCancel, MethodLog:
		// fire-and-forget notifications this demo peer doesn't act on.
	default:
		if isRequest {
			peerReplyError(f.ID, CodeMethodNotFound, "method not found: "+f.Method)
		}
	}
}
