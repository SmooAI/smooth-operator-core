// Package extension is the Go engine's implementation of SEP, the Smooth
// Extension Protocol.
//
// An extension is a long-lived subprocess speaking JSON-RPC 2.0 over ndjson on
// its stdio (identical framing to MCP stdio). The canonical wire schemas live
// in the smooth-operator repo at spec/extension/; the types here are the Go
// host's view of that wire. Field names are snake_case to match the spec.
//
// This package is purely additive: nothing here runs unless a caller builds an
// ExtensionHost and registers its tools into core.AgentOptions.Tools. With no
// host built the agent loop behaves exactly as before.
//
// Layout mirrors the Rust reference (rust/smooth-operator-core/src/extension/):
//   - protocol.go  — JSON-RPC frames + typed method params/results.
//   - manifest.go  — extension.toml discovery, global+project merge, ${env:VAR}.
//   - process.go   — one subprocess: ndjson codec, pending map, generation guard.
//   - host.go      — ExtensionHost: hook chaining, event fanout, the delegate seam.
//   - tool_proxy.go — ExtensionTool: an extension tool as a core.Tool.
package extension

import (
	"encoding/json"
	"fmt"
)

// PROTOCOL_VERSION is the SEP protocol version this host implements.
const ProtocolVersion = 1

// JSON-RPC error codes. Standard range plus the SEP extensions documented in
// spec/extension/envelope.md.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeBlocked — a hook or policy vetoed the operation.
	CodeBlocked = -32000
	// CodeNoUI — ui/request in a headless/uncapable frontend.
	CodeNoUI = -32001
	// CodeNotTrusted — extension acted beyond its granted trust.
	CodeNotTrusted = -32002
	// CodeContextViolation — command-tier action attempted from an event-tier context.
	CodeContextViolation = -32003
	// CodeCapabilityDisabled — method requires a capability the handshake did not enable.
	CodeCapabilityDisabled = -32004
	// CodeCancelled — request cancelled via $/cancel.
	CodeCancelled = -32800
)

// SEP method names, centralized so the host and tests never spell one wrong.
const (
	MethodInitialize             = "initialize"
	MethodShutdown               = "shutdown"
	MethodPing                   = "ping"
	MethodEvent                  = "event"
	MethodHook                   = "hook"
	MethodToolExecute            = "tool/execute"
	MethodToolUpdate             = "tool/update"
	MethodCommandExecute         = "command/execute"
	MethodCommandComplete        = "command/complete"
	MethodCancel                 = "$/cancel"
	MethodRegistryUpdate         = "registry/update"
	MethodToolsSetActive         = "tools/set_active"
	MethodExecRun                = "exec/run"
	MethodUIRequest              = "ui/request"
	MethodLog                    = "log"
	MethodBusPublish             = "bus/publish"
	MethodSessionSendMessage     = "session/send_message"
	MethodSessionSendUserMessage = "session/send_user_message"
	MethodSessionAppendEntry     = "session/append_entry"
)

// RpcError is a JSON-RPC error object. It implements error.
type RpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// NewRpcError builds an RpcError with no data payload.
func NewRpcError(code int, message string) *RpcError {
	return &RpcError{Code: code, Message: message}
}

func (e *RpcError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// Message is the JSON-RPC 2.0 envelope. All four frame shapes share this struct;
// which fields are present determines the shape:
//
//   - request: id + method (+ optional params)
//   - notification: method, no id
//   - success response: id + result
//   - error response: id + error
//
// omitempty keeps absent fields off the wire so a request never carries a
// result/error key. ID/Params/Result are json.RawMessage so an arbitrary
// inbound line round-trips (the id may be an int or a string).
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RpcError       `json:"error,omitempty"`
}

// NewRequest builds a request frame.
func NewRequest(id json.RawMessage, method string, params json.RawMessage) Message {
	return Message{JSONRPC: "2.0", ID: id, Method: method, Params: params}
}

// NewNotification builds a notification frame (no id, no reply expected).
func NewNotification(method string, params json.RawMessage) Message {
	return Message{JSONRPC: "2.0", Method: method, Params: params}
}

// NewSuccess builds a success response frame echoing id.
func NewSuccess(id json.RawMessage, result json.RawMessage) Message {
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return Message{JSONRPC: "2.0", ID: id, Result: result}
}

// NewErrorResponse builds an error response frame echoing id (id may be nil).
func NewErrorResponse(id json.RawMessage, err *RpcError) Message {
	return Message{JSONRPC: "2.0", ID: id, Error: err}
}

func (m *Message) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

// IsRequest reports whether this frame is a request (has both id and method).
func (m *Message) IsRequest() bool { return m.hasID() && m.Method != "" }

// IsNotification reports whether this frame is a notification (method, no id).
func (m *Message) IsNotification() bool { return !m.hasID() && m.Method != "" }

// IsResponse reports whether this frame is a response (has id, no method).
func (m *Message) IsResponse() bool { return m.hasID() && m.Method == "" }

// Tier is whether a dispatch may only observe (event) or may mutate the session
// (command). Session-mutating ext→host actions require command.
type Tier string

const (
	TierEvent   Tier = "event"
	TierCommand Tier = "command"
)

// Context is the dispatch context carried by every host→ext event/hook/tool/command.
type Context struct {
	Token string `json:"token"`
	Tier  Tier   `json:"tier"`
}

// HostInfo identifies the engine host to the extension at handshake.
type HostInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// WorkspaceInfo describes the workspace root and whether it is trusted.
type WorkspaceInfo struct {
	Root    string `json:"root"`
	Trusted bool   `json:"trusted"`
}

// SessionInfo optionally identifies the active session at handshake.
type SessionInfo struct {
	ID string `json:"id,omitempty"`
}

// InitializeParams is the host→ext handshake.
type InitializeParams struct {
	ProtocolVersion int                        `json:"protocol_version"`
	Host            HostInfo                   `json:"host"`
	Workspace       WorkspaceInfo              `json:"workspace"`
	Session         *SessionInfo               `json:"session,omitempty"`
	Mode            string                     `json:"mode"`
	UICapabilities  []string                   `json:"ui_capabilities,omitempty"`
	Flags           map[string]json.RawMessage `json:"flags,omitempty"`
	// CapabilitiesEnabled is an opaque capability map the host may advertise.
	CapabilitiesEnabled json.RawMessage `json:"capabilities_enabled,omitempty"`
}

// Validate enforces the required fields the JSON Schema marks required and that
// serde rejects in the Rust host: protocol_version must be present (>= 1).
func (p InitializeParams) Validate() error {
	if p.ProtocolVersion < 1 {
		return fmt.Errorf("initialize params: protocol_version is required and must be >= 1")
	}
	return nil
}

// ExtensionInfo identifies the extension in its handshake reply.
type ExtensionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolRegistration declares a tool the extension exposes.
type ToolRegistration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Deferred    bool            `json:"deferred,omitempty"`
}

// CommandRegistration declares a slash-command the extension exposes.
type CommandRegistration struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ShortcutRegistration binds a keyboard chord to one of the extension's commands.
// Only frontends with a key surface (the TUI) honor these.
type ShortcutRegistration struct {
	Key         string `json:"key"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
}

// Registrations is the set of things an extension registers at handshake (or
// later via registry/update).
type Registrations struct {
	Tools         []ToolRegistration     `json:"tools,omitempty"`
	Commands      []CommandRegistration  `json:"commands,omitempty"`
	Flags         []string               `json:"flags,omitempty"`
	Shortcuts     []ShortcutRegistration `json:"shortcuts,omitempty"`
	Subscriptions []string               `json:"subscriptions,omitempty"`
}

// InitializeResult is the extension's handshake reply.
type InitializeResult struct {
	ProtocolVersion int           `json:"protocol_version"`
	Extension       ExtensionInfo `json:"extension"`
	Registrations   Registrations `json:"registrations"`
}

// HookParams is the host→ext hook intercept.
type HookParams struct {
	Hook    string          `json:"hook"`
	Context Context         `json:"context"`
	Input   json.RawMessage `json:"input"`
}

// HookAction is the discriminator of a HookOutcome.
type HookAction string

const (
	ActionContinue HookAction = "continue"
	ActionBlock    HookAction = "block"
	ActionModify   HookAction = "modify"
)

// HookOutcome is an extension's reply to a hook. It marshals tagged by action:
//
//	{ "action": "continue" }
//	{ "action": "block", "reason": "..." }
//	{ "action": "modify", "patch": { ... } }
type HookOutcome struct {
	Action HookAction
	// Reason is set only for a block outcome (optional on the wire).
	Reason string
	// Patch is set only for a modify outcome (required for modify).
	Patch json.RawMessage
}

// UnmarshalJSON enforces the tagged-union constraints the Rust serde enum does:
// an unknown action is rejected, and a modify without a patch is rejected.
func (h *HookOutcome) UnmarshalJSON(b []byte) error {
	var raw struct {
		Action string          `json:"action"`
		Reason *string         `json:"reason"`
		Patch  json.RawMessage `json:"patch"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch HookAction(raw.Action) {
	case ActionContinue:
		*h = HookOutcome{Action: ActionContinue}
	case ActionBlock:
		h.Action = ActionBlock
		h.Reason = ""
		h.Patch = nil
		if raw.Reason != nil {
			h.Reason = *raw.Reason
		}
	case ActionModify:
		if len(raw.Patch) == 0 {
			return fmt.Errorf("modify hook outcome missing patch")
		}
		h.Action = ActionModify
		h.Reason = ""
		h.Patch = raw.Patch
	default:
		return fmt.Errorf("unknown hook action %q", raw.Action)
	}
	return nil
}

// MarshalJSON serializes a HookOutcome tagged by action.
func (h HookOutcome) MarshalJSON() ([]byte, error) {
	switch h.Action {
	case ActionContinue:
		return []byte(`{"action":"continue"}`), nil
	case ActionBlock:
		out := map[string]any{"action": "block"}
		if h.Reason != "" {
			out["reason"] = h.Reason
		}
		return json.Marshal(out)
	case ActionModify:
		patch := h.Patch
		if len(patch) == 0 {
			patch = json.RawMessage("null")
		}
		return json.Marshal(map[string]any{"action": "modify", "patch": patch})
	default:
		return nil, fmt.Errorf("cannot marshal hook outcome with action %q", h.Action)
	}
}

// ToolExecuteParams is the host→ext dispatch of a registered tool.
type ToolExecuteParams struct {
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Context   Context         `json:"context"`
}

// Validate enforces the required call_id the Rust host requires (serde rejects
// a missing call_id).
func (p ToolExecuteParams) Validate() error {
	if p.CallID == "" {
		return fmt.Errorf("tool/execute params: call_id is required")
	}
	return nil
}

// ToolExecuteResult is the ext→host reply to tool/execute.
type ToolExecuteResult struct {
	Content string          `json:"content"`
	IsError bool            `json:"is_error,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// ToolUpdateParams is a progress notification for an in-flight tool/execute.
type ToolUpdateParams struct {
	CallID   string          `json:"call_id"`
	Message  string          `json:"message,omitempty"`
	Progress *float64        `json:"progress,omitempty"`
	Details  json.RawMessage `json:"details,omitempty"`
}

// EventParams is a fire-and-forget lifecycle/turn event delivered to a
// subscribed extension.
type EventParams struct {
	Event string `json:"event"`
	// Seq is the per-connection monotonic sequence. Absent (a nil pointer) on the
	// out-of-band events_lost marker.
	Seq     *uint64         `json:"seq,omitempty"`
	Context Context         `json:"context"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// LogParams is a structured log line folded into host tracing.
type LogParams struct {
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Fields  json.RawMessage `json:"fields,omitempty"`
}

// CommandExecuteParams is the host→ext dispatch of a registered slash-command.
type CommandExecuteParams struct {
	Command   string          `json:"command"`
	Context   Context         `json:"context"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CommandExecuteResult is the ext→host reply to command/execute.
type CommandExecuteResult struct {
	Content string `json:"content,omitempty"`
}

// CommandCompleteParams asks an extension for argument completions.
type CommandCompleteParams struct {
	Command string  `json:"command"`
	Context Context `json:"context"`
	Partial string  `json:"partial,omitempty"`
}

// Completion is one argument-completion suggestion.
type Completion struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// CommandCompleteResult is the ext→host reply to command/complete.
type CommandCompleteResult struct {
	Completions []Completion `json:"completions,omitempty"`
}
