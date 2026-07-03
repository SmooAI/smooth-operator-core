package extension

// ExtensionHost — orchestrates the loaded extensions: hook chaining in load
// order, non-blocking event fanout, tool proxies, and the ext→host delegate seam.
//
// The security-critical part is FoldHookChain: how per-extension hook outcomes
// combine, and what happens on timeout/crash. It is a pure function so it can be
// tested exhaustively against adversarial inputs without spawning anything.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HookType classifies a hook by its failure policy and default timeout.
type HookType int

const (
	HookToolCall HookType = iota
	HookUserBash
	HookToolResult
	HookInput
	HookBeforeAgentStart
	HookContext
	HookBeforeProviderRequest
	HookMessageEnd
	HookSessionBeforeCompact
	HookSessionBeforeTree
)

var hookNames = map[HookType]string{
	HookToolCall:              "tool_call",
	HookUserBash:              "user_bash",
	HookToolResult:            "tool_result",
	HookInput:                 "input",
	HookBeforeAgentStart:      "before_agent_start",
	HookContext:               "context",
	HookBeforeProviderRequest: "before_provider_request",
	HookMessageEnd:            "message_end",
	HookSessionBeforeCompact:  "session_before_compact",
	HookSessionBeforeTree:     "session_before_tree",
}

// String returns the wire name of the hook.
func (h HookType) String() string { return hookNames[h] }

// HookTypeFromName maps a wire name to its HookType.
func HookTypeFromName(name string) (HookType, bool) {
	for k, v := range hookNames {
		if v == name {
			return k, true
		}
	}
	return 0, false
}

// FailClosed reports whether this hook blocks the operation when an extension
// times out or crashes. tool_call and user_bash gate execution; everything else
// fails open (proceeds).
func (h HookType) FailClosed() bool {
	return h == HookToolCall || h == HookUserBash
}

// DefaultTimeout is 60s for fail-closed hooks (they gate execution), 5s for
// fail-open. Manifest hook_timeout_ms overrides this.
func (h HookType) DefaultTimeout() time.Duration {
	if h.FailClosed() {
		return 60 * time.Second
	}
	return 5 * time.Second
}

// hookStep is one extension's reply within a hook chain, as seen by the fold.
type hookStep struct {
	// outcome is the extension's reply; nil means it timed out or crashed (failed).
	outcome *HookOutcome
}

func repliedStep(o HookOutcome) hookStep { return hookStep{outcome: &o} }
func failedStep() hookStep               { return hookStep{outcome: nil} }

// FoldedHook is the folded result of a whole hook chain.
type FoldedHook struct {
	// Blocked is true when the operation was vetoed; Reason carries why.
	Blocked bool
	Reason  string
	// Value is the (possibly modified) input to proceed with (valid when !Blocked).
	Value json.RawMessage
}

// FoldHookChain folds a hook chain over input, in load order. steps are the
// per-extension results in that order. This is the security-critical policy:
//
//   - continue → value unchanged, next extension sees it.
//   - modify   → value replaced by the patch, next extension sees the patch.
//   - block    → short-circuit; the operation is vetoed (honored for every hook).
//   - failed   → for a fail-closed hook, block; for a fail-open hook, proceed.
func FoldHookChain(hook HookType, input json.RawMessage, steps []hookStep) FoldedHook {
	current := input
	for _, step := range steps {
		if step.outcome == nil {
			if hook.FailClosed() {
				return FoldedHook{Blocked: true, Reason: fmt.Sprintf("%s hook failed (fail-closed)", hook)}
			}
			continue // fail-open: proceed with the current value.
		}
		switch step.outcome.Action {
		case ActionContinue:
			// value unchanged.
		case ActionModify:
			current = step.outcome.Patch
		case ActionBlock:
			reason := step.outcome.Reason
			if reason == "" {
				reason = fmt.Sprintf("blocked by %s hook", hook)
			}
			return FoldedHook{Blocked: true, Reason: reason}
		}
	}
	return FoldedHook{Value: current}
}

// EffectiveSubscriptions is what the extension asked for at handshake, clamped to
// what its manifest [capabilities] events declared. An empty declared list means
// "no declared filter" → trust the handshake as-is; a non-empty list is the outer
// bound the extension can never widen past.
func EffectiveSubscriptions(declared, requested []string) map[string]struct{} {
	out := map[string]struct{}{}
	if len(declared) == 0 {
		for _, s := range requested {
			out[s] = struct{}{}
		}
		return out
	}
	allow := map[string]struct{}{}
	for _, d := range declared {
		allow[d] = struct{}{}
	}
	for _, s := range requested {
		if _, ok := allow[s]; ok {
			out[s] = struct{}{}
		}
	}
	return out
}

// tokenEpoch parses the epoch embedded in a context token minted by
// ExtensionHost.Context (epoch-<N>). Returns ok=false for a malformed token.
func tokenEpoch(token string) (uint64, bool) {
	rest, ok := strings.CutPrefix(token, "epoch-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// validateCommandContext is the two-tier deadlock guard: a session-mutating
// ext→host action is valid only when it presents a COMMAND-tier context whose
// epoch is still current. An event-tier context, or a stale token minted before a
// reload bumped the epoch, is rejected with ContextViolation. Kept a pure
// function so it can be tested exhaustively.
func validateCommandContext(params json.RawMessage, currentEpoch uint64) *RpcError {
	var p struct {
		Context struct {
			Tier  string `json:"tier"`
			Token string `json:"token"`
		} `json:"context"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Context.Tier != "command" {
		return NewRpcError(CodeContextViolation, "session action requires a command-tier context")
	}
	if e, ok := tokenEpoch(p.Context.Token); !ok || e != currentEpoch {
		return NewRpcError(CodeContextViolation, "session action presented a stale context (epoch mismatch)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Host delegate: the ext→host seam (ui / kv / exec / session / trust).
// ---------------------------------------------------------------------------

// HostDelegate is the host's side of ext→host requests. Embed DefaultHostDelegate
// to inherit the headless defaults and override only what a richer frontend needs.
type HostDelegate interface {
	UIRequest(ext string, params json.RawMessage) (json.RawMessage, *RpcError)
	KVGet(ext, key string) (json.RawMessage, *RpcError)
	KVSet(ext, key string, value json.RawMessage) *RpcError
	ExecRun(ext string, params json.RawMessage) (json.RawMessage, *RpcError)
	SessionSendMessage(ext string, params json.RawMessage) (json.RawMessage, *RpcError)
	SessionSendUserMessage(ext string, params json.RawMessage) (json.RawMessage, *RpcError)
	SessionAppendEntry(ext string, params json.RawMessage) (json.RawMessage, *RpcError)
	ToolUpdate(ext string, params json.RawMessage)
}

// DefaultHostDelegate is the engine's headless delegate: NoUI, JSON-file kv, exec
// denied, session actions unavailable.
type DefaultHostDelegate struct{}

func (DefaultHostDelegate) UIRequest(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return nil, NewRpcError(CodeNoUI, "no UI available (headless host)")
}

func (DefaultHostDelegate) KVGet(ext, key string) (json.RawMessage, *RpcError) {
	m := kvFileLoad(ext)
	if v, ok := m[key]; ok {
		return v, nil
	}
	return json.RawMessage("null"), nil
}

func (DefaultHostDelegate) KVSet(ext, key string, value json.RawMessage) *RpcError {
	m := kvFileLoad(ext)
	if value == nil {
		value = json.RawMessage("null")
	}
	m[key] = value
	return kvFileStore(ext, m)
}

func (DefaultHostDelegate) ExecRun(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return nil, NewRpcError(CodeNotTrusted, "exec/run is not permitted on the headless host")
}

func (DefaultHostDelegate) SessionSendMessage(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return nil, NewRpcError(CodeCapabilityDisabled, "session actions are unavailable on this host")
}

func (DefaultHostDelegate) SessionSendUserMessage(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return nil, NewRpcError(CodeCapabilityDisabled, "session actions are unavailable on this host")
}

func (DefaultHostDelegate) SessionAppendEntry(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return nil, NewRpcError(CodeCapabilityDisabled, "session actions are unavailable on this host")
}

func (DefaultHostDelegate) ToolUpdate(string, json.RawMessage) {}

// kvFilePath is the per-extension kv state file:
// $SMOOTH_HOME/extensions/<name>/state.json (or ~/.smooth/extensions/<name>/state.json).
func kvFilePath(ext string) string {
	dir := DefaultGlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ext, "state.json")
}

func kvFileLoad(ext string) map[string]json.RawMessage {
	path := kvFilePath(ext)
	if path == "" {
		return map[string]json.RawMessage{}
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(text, &m); err != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

func kvFileStore(ext string, m map[string]json.RawMessage) *RpcError {
	path := kvFilePath(ext)
	if path == "" {
		return NewRpcError(CodeInternalError, "no home dir for kv store")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return NewRpcError(CodeInternalError, "kv mkdir: "+err.Error())
	}
	text, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return NewRpcError(CodeInternalError, "kv serialize: "+err.Error())
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		return NewRpcError(CodeInternalError, "kv write: "+err.Error())
	}
	return nil
}

// hostInbound bridges the process reader's ext→host requests to the HostDelegate.
// It holds the host's shared epoch so it can reject stale/event-tier session actions.
type hostInbound struct {
	ext      string
	delegate HostDelegate
	epoch    *atomic.Uint64
}

func (h *hostInbound) HandleRequest(method string, params json.RawMessage) (json.RawMessage, *RpcError) {
	switch method {
	case MethodPing:
		return json.RawMessage("{}"), nil
	case MethodUIRequest:
		return h.delegate.UIRequest(h.ext, params)
	case MethodExecRun:
		return h.delegate.ExecRun(h.ext, params)
	case MethodSessionSendMessage:
		if err := validateCommandContext(params, h.epoch.Load()); err != nil {
			return nil, err
		}
		return h.delegate.SessionSendMessage(h.ext, params)
	case MethodSessionSendUserMessage:
		if err := validateCommandContext(params, h.epoch.Load()); err != nil {
			return nil, err
		}
		return h.delegate.SessionSendUserMessage(h.ext, params)
	case MethodSessionAppendEntry:
		if err := validateCommandContext(params, h.epoch.Load()); err != nil {
			return nil, err
		}
		return h.delegate.SessionAppendEntry(h.ext, params)
	case "kv/get":
		var p struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(params, &p)
		v, err := h.delegate.KVGet(h.ext, p.Key)
		if err != nil {
			return nil, err
		}
		out, _ := json.Marshal(map[string]json.RawMessage{"value": v})
		return out, nil
	case "kv/set":
		var p struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal(params, &p)
		if err := h.delegate.KVSet(h.ext, p.Key, p.Value); err != nil {
			return nil, err
		}
		return json.RawMessage("{}"), nil
	default:
		return nil, NewRpcError(CodeMethodNotFound, "method not found: "+method)
	}
}

func (h *hostInbound) HandleNotification(method string, params json.RawMessage) {
	if method == MethodToolUpdate {
		h.delegate.ToolUpdate(h.ext, params)
	}
}

// ---------------------------------------------------------------------------
// ExtensionHost
// ---------------------------------------------------------------------------

// loaded is a loaded, initialized extension. init and subscriptions are guarded
// so a hot Reload can swap in the freshly re-initialized registrations without
// disturbing the stable process handle.
type loaded struct {
	name    string
	process *ExtensionProcess

	mu            sync.RWMutex
	init          InitializeResult
	subscriptions map[string]struct{}

	// declaredEvents is the manifest's declared event allow-list — the clamp
	// subscriptions can never widen past, re-applied on reload.
	declaredEvents []string
	// hookTimeout overrides the per-hook default (0 = use the default).
	hookTimeout time.Duration
}

// LoadFailure records an extension that failed to load (spawn/handshake).
type LoadFailure struct {
	Name string
	Err  string
}

// ExtensionHost orchestrates the set of loaded extensions in load order.
type ExtensionHost struct {
	extensions []*loaded
	epoch      *atomic.Uint64

	host           HostInfo
	workspace      WorkspaceInfo
	mode           string
	uiCapabilities []string
}

// NewEmptyHost returns a host with no extensions — the zero-cost default when no
// extensions are configured. Every hook is a passthrough.
func NewEmptyHost() *ExtensionHost {
	e := &atomic.Uint64{}
	e.Store(1)
	return &ExtensionHost{
		epoch:     e,
		host:      HostInfo{Name: "smooth-operator-core", Version: "0.0.0"},
		workspace: WorkspaceInfo{},
		mode:      "headless",
	}
}

// Load loads and initializes each discovered extension. Per-extension failures
// (spawn, handshake) are tolerated and returned alongside the host. In an
// untrusted workspace, project-scoped extensions are skipped.
func Load(ctx context.Context, discovered []DiscoveredExtension, host HostInfo, workspace WorkspaceInfo, mode string, uiCapabilities []string, delegate HostDelegate) (*ExtensionHost, []LoadFailure) {
	if delegate == nil {
		delegate = DefaultHostDelegate{}
	}
	epoch := &atomic.Uint64{}
	epoch.Store(1)
	h := &ExtensionHost{
		epoch:          epoch,
		host:           host,
		workspace:      workspace,
		mode:           mode,
		uiCapabilities: uiCapabilities,
	}
	var failures []LoadFailure
	for _, ext := range discovered {
		if ext.Manifest.Disabled {
			continue
		}
		if ext.Scope == ScopeProject && !workspace.Trusted {
			continue
		}
		l, err := h.loadOne(ctx, ext, delegate)
		if err != nil {
			failures = append(failures, LoadFailure{Name: ext.Manifest.Name, Err: err.Error()})
			continue
		}
		h.extensions = append(h.extensions, l)
	}
	return h, failures
}

func (h *ExtensionHost) loadOne(ctx context.Context, ext DiscoveredExtension, delegate HostDelegate) (*loaded, error) {
	spec := SpawnSpec{
		Command: ext.Manifest.Run.Command,
		Args:    ext.Manifest.Run.Args,
		Env:     ext.Manifest.ResolvedEnv(),
		Cwd:     ext.Root,
	}
	handler := &hostInbound{ext: ext.Manifest.Name, delegate: delegate, epoch: h.epoch}
	process, err := Spawn(spec, handler)
	if err != nil {
		return nil, err
	}
	init, err := h.initialize(ctx, process)
	if err != nil {
		process.Close()
		return nil, err
	}
	var hookTimeout time.Duration
	if ext.Manifest.HookTimeoutMS > 0 {
		hookTimeout = time.Duration(ext.Manifest.HookTimeoutMS) * time.Millisecond
	}
	return &loaded{
		name:           ext.Manifest.Name,
		process:        process,
		init:           init,
		subscriptions:  EffectiveSubscriptions(ext.Manifest.Capabilities.Events, init.Registrations.Subscriptions),
		declaredEvents: ext.Manifest.Capabilities.Events,
		hookTimeout:    hookTimeout,
	}, nil
}

// initialize sends the initialize handshake and parses the registrations.
func (h *ExtensionHost) initialize(ctx context.Context, process *ExtensionProcess) (InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Host:            h.host,
		Workspace:       h.workspace,
		Mode:            h.mode,
		UICapabilities:  h.uiCapabilities,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return InitializeResult{}, err
	}
	reply, err := process.Request(ctx, MethodInitialize, raw, 10*time.Second)
	if err != nil {
		return InitializeResult{}, fmt.Errorf("initialize: %w", err)
	}
	var result InitializeResult
	if err := json.Unmarshal(reply, &result); err != nil {
		return InitializeResult{}, fmt.Errorf("bad initialize result: %w", err)
	}
	return result, nil
}

// Len is the number of successfully loaded extensions.
func (h *ExtensionHost) Len() int { return len(h.extensions) }

// IsEmpty reports whether no extensions loaded.
func (h *ExtensionHost) IsEmpty() bool { return len(h.extensions) == 0 }

// Names returns the loaded extension names, in load order.
func (h *ExtensionHost) Names() []string {
	out := make([]string, len(h.extensions))
	for i, e := range h.extensions {
		out[i] = e.name
	}
	return out
}

// Context builds a fresh dispatch context. Session-mutating actions need
// TierCommand. The token embeds the current epoch so it is invalidated across reloads.
func (h *ExtensionHost) Context(tier Tier) Context {
	return Context{Token: "epoch-" + strconv.FormatUint(h.epoch.Load(), 10), Tier: tier}
}

func (h *ExtensionHost) contextRaw(tier Tier) json.RawMessage {
	raw, _ := json.Marshal(h.Context(tier))
	return raw
}

// BumpEpoch bumps the epoch, invalidating every previously minted context token.
func (h *ExtensionHost) BumpEpoch() { h.epoch.Add(1) }

// HasSubscriber reports whether any loaded extension subscribed to event.
func (h *ExtensionHost) HasSubscriber(event string) bool {
	for _, e := range h.extensions {
		e.mu.RLock()
		_, ok := e.subscriptions[event]
		e.mu.RUnlock()
		if ok {
			return true
		}
	}
	return false
}

// DispatchEvent is a fire-and-forget event fanout to every subscribed extension.
// Non-blocking: a slow or dead extension never stalls the caller.
func (h *ExtensionHost) DispatchEvent(event string, payload json.RawMessage) {
	if len(h.extensions) == 0 {
		return
	}
	ctx := h.contextRaw(TierEvent)
	for _, e := range h.extensions {
		e.mu.RLock()
		_, ok := e.subscriptions[event]
		e.mu.RUnlock()
		if !ok {
			continue
		}
		e.process.SendEvent(event, ctx, payload)
	}
}

// RunHook runs a hook across every extension in load order, folding the chain.
// Each extension sees the prior extension's patch. Fail-open/closed per HookType.
func (h *ExtensionHost) RunHook(ctx context.Context, hook HookType, input json.RawMessage) FoldedHook {
	if len(h.extensions) == 0 {
		return FoldedHook{Value: input}
	}
	current := input
	cmdCtx := h.Context(TierCommand)
	for _, e := range h.extensions {
		params, _ := json.Marshal(map[string]any{"hook": hook.String(), "context": cmdCtx, "input": current})
		timeout := e.hookTimeout
		if timeout == 0 {
			timeout = hook.DefaultTimeout()
		}
		var step hookStep
		reply, err := e.process.Request(ctx, MethodHook, params, timeout)
		if err != nil {
			step = failedStep()
		} else {
			var outcome HookOutcome
			if err := json.Unmarshal(reply, &outcome); err != nil {
				step = failedStep()
			} else {
				step = repliedStep(outcome)
			}
		}
		folded := FoldHookChain(hook, current, []hookStep{step})
		if folded.Blocked {
			return folded
		}
		current = folded.Value
	}
	return FoldedHook{Value: current}
}

// RunToolCallHook runs the tool_call hook (fail-closed) on a pending call.
func (h *ExtensionHost) RunToolCallHook(ctx context.Context, tool string, arguments json.RawMessage) FoldedHook {
	if len(arguments) == 0 {
		arguments = json.RawMessage("null")
	}
	input, _ := json.Marshal(map[string]json.RawMessage{"tool": mustJSON(tool), "arguments": arguments})
	return h.RunHook(ctx, HookToolCall, input)
}

// BeforeAgentStart runs the before_agent_start hook on a system prompt, returning
// the possibly-rewritten prompt. Fail-open: a blocked/failed hook leaves it unchanged.
func (h *ExtensionHost) BeforeAgentStart(ctx context.Context, systemPrompt string) string {
	if len(h.extensions) == 0 {
		return systemPrompt
	}
	input, _ := json.Marshal(map[string]string{"system_prompt": systemPrompt})
	folded := h.RunHook(ctx, HookBeforeAgentStart, input)
	if folded.Blocked {
		return systemPrompt
	}
	var out struct {
		SystemPrompt *string `json:"system_prompt"`
	}
	if err := json.Unmarshal(folded.Value, &out); err == nil && out.SystemPrompt != nil {
		return *out.SystemPrompt
	}
	return systemPrompt
}

// Tools returns tool proxies for every eager tool every extension registered.
// Names are dotted <ext>.<tool>. Deferred tools are returned by DeferredTools.
func (h *ExtensionHost) Tools() []*ExtensionTool { return h.collectTools(false) }

// DeferredTools returns deferred tool proxies.
func (h *ExtensionHost) DeferredTools() []*ExtensionTool { return h.collectTools(true) }

func (h *ExtensionHost) collectTools(deferred bool) []*ExtensionTool {
	ctx := h.Context(TierCommand)
	var out []*ExtensionTool
	for _, e := range h.extensions {
		e.mu.RLock()
		regs := e.init.Registrations.Tools
		e.mu.RUnlock()
		for i := range regs {
			if regs[i].Deferred != deferred {
				continue
			}
			out = append(out, NewExtensionTool(e.name, regs[i], e.process, ctx))
		}
	}
	return out
}

// ToolsFor returns eager tool proxies for a single extension, minted at the
// CURRENT epoch. The frontend calls this after a Reload to re-register the
// reloaded extension's tools (old proxies carry a stale context).
func (h *ExtensionHost) ToolsFor(extName string) []*ExtensionTool {
	ctx := h.Context(TierCommand)
	for _, e := range h.extensions {
		if e.name != extName {
			continue
		}
		e.mu.RLock()
		regs := e.init.Registrations.Tools
		e.mu.RUnlock()
		var out []*ExtensionTool
		for i := range regs {
			if regs[i].Deferred {
				continue
			}
			out = append(out, NewExtensionTool(e.name, regs[i], e.process, ctx))
		}
		return out
	}
	return nil
}

// OwnedCommand pairs a registered command with the extension that owns it.
type OwnedCommand struct {
	Extension string
	Command   CommandRegistration
}

// Commands returns every registered slash-command across all extensions.
func (h *ExtensionHost) Commands() []OwnedCommand {
	var out []OwnedCommand
	for _, e := range h.extensions {
		e.mu.RLock()
		cmds := e.init.Registrations.Commands
		e.mu.RUnlock()
		for _, c := range cmds {
			out = append(out, OwnedCommand{Extension: e.name, Command: c})
		}
	}
	return out
}

// OwnedShortcut pairs a keyboard shortcut with the extension that owns it.
type OwnedShortcut struct {
	Extension string
	Shortcut  ShortcutRegistration
}

// Shortcuts returns every keyboard shortcut across all extensions.
func (h *ExtensionHost) Shortcuts() []OwnedShortcut {
	var out []OwnedShortcut
	for _, e := range h.extensions {
		e.mu.RLock()
		scs := e.init.Registrations.Shortcuts
		e.mu.RUnlock()
		for _, s := range scs {
			out = append(out, OwnedShortcut{Extension: e.name, Shortcut: s})
		}
	}
	return out
}

// commandOwner finds the extension process that registered command (optionally
// scoped to a specific extension name).
func (h *ExtensionHost) commandOwner(extName, command string) *ExtensionProcess {
	for _, e := range h.extensions {
		if extName != "" && extName != e.name {
			continue
		}
		e.mu.RLock()
		cmds := e.init.Registrations.Commands
		e.mu.RUnlock()
		for _, c := range cmds {
			if c.Name == command {
				return e.process
			}
		}
	}
	return nil
}

// RunCommand dispatches a registered slash-command to its owning extension with a
// COMMAND-tier context. Pass extName to disambiguate a command registered by more
// than one extension; "" picks the first match in load order.
func (h *ExtensionHost) RunCommand(ctx context.Context, extName, command string, arguments json.RawMessage) (CommandExecuteResult, *RpcError) {
	process := h.commandOwner(extName, command)
	if process == nil {
		return CommandExecuteResult{}, NewRpcError(CodeMethodNotFound, "no extension registered command `"+command+"`")
	}
	params, _ := json.Marshal(map[string]any{"command": command, "context": h.Context(TierCommand), "arguments": arguments})
	raw, err := process.Request(ctx, MethodCommandExecute, params, 120*time.Second)
	if err != nil {
		return CommandExecuteResult{}, NewRpcError(CodeInternalError, "command/execute: "+err.Error())
	}
	var result CommandExecuteResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CommandExecuteResult{}, NewRpcError(CodeInternalError, "bad command/execute result: "+err.Error())
	}
	return result, nil
}

// CompleteCommand asks the extension that owns command for argument completions
// given the partial text typed so far. Best-effort: returns nil on any error.
func (h *ExtensionHost) CompleteCommand(ctx context.Context, extName, command, partial string) []Completion {
	process := h.commandOwner(extName, command)
	if process == nil {
		return nil
	}
	params, _ := json.Marshal(map[string]any{"command": command, "context": h.Context(TierCommand), "partial": partial})
	raw, err := process.Request(ctx, MethodCommandComplete, params, 5*time.Second)
	if err != nil {
		return nil
	}
	var result CommandCompleteResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result.Completions
}

// Reload hot-reloads a single extension by name: notify it (session_shutdown
// reason reload), bump the epoch so every context token it still holds is
// invalidated, respawn its subprocess, re-run initialize to pick up its new
// registrations, then notify it (session_start reason reload). The caller
// re-registers the extension's tools via ToolsFor.
func (h *ExtensionHost) Reload(ctx context.Context, name string) error {
	var ext *loaded
	for _, e := range h.extensions {
		if e.name == name {
			ext = e
			break
		}
	}
	if ext == nil {
		return fmt.Errorf("extension `%s` is not loaded", name)
	}
	ext.process.SendEvent("session_shutdown", h.contextRaw(TierEvent), json.RawMessage(`{"reason":"reload"}`))

	// Fence: any context token minted before this point is now stale.
	h.BumpEpoch()
	if err := ext.process.Respawn(); err != nil {
		return err
	}
	init, err := h.initialize(ctx, ext.process)
	if err != nil {
		return err
	}
	subs := EffectiveSubscriptions(ext.declaredEvents, init.Registrations.Subscriptions)
	ext.mu.Lock()
	ext.init = init
	ext.subscriptions = subs
	ext.mu.Unlock()

	ext.process.SendEvent("session_start", h.contextRaw(TierEvent), json.RawMessage(`{"reason":"reload"}`))
	return nil
}

// ShutdownAll gracefully shuts down every extension (5s grace each, then SIGKILL).
func (h *ExtensionHost) ShutdownAll(ctx context.Context) {
	for _, e := range h.extensions {
		e.process.Shutdown(ctx, 5*time.Second)
	}
}

// mustJSON marshals a value to json.RawMessage, panicking only on an
// impossible-to-fail input (a string). Used for building small fixed inputs.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
