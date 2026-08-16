package extension

import (
	"context"
	"encoding/json"

	core "github.com/SmooAI/smooth-operator-core/go/core"
)

// AgentBridge adapts an ExtensionHost onto core.ExtensionHooks so a host can be
// wired into the agent loop:
//
//	host, _ := extension.Load(ctx, ...)
//	agent := core.NewSmoothAgent(client, core.AgentOptions{
//	    Extensions: extension.NewAgentBridge(host),
//	})
//
// The adapter exists because the dependency runs extension → core (an
// ExtensionTool is a core.Tool), so core cannot import this package back. Every
// method here translates between this package's types (FoldedHook,
// *ExtensionTool) and the core-expressible signatures the seam declares.
type AgentBridge struct {
	host *ExtensionHost
}

// NewAgentBridge wires host into an agent. Returns nil for a nil host, so
// `Extensions: extension.NewAgentBridge(nil)` is the same no-op as leaving the
// option unset rather than a nil-pointer panic mid-turn.
func NewAgentBridge(host *ExtensionHost) core.ExtensionHooks {
	if host == nil {
		return nil
	}
	return &AgentBridge{host: host}
}

// Compile-time proof the bridge satisfies the seam.
var _ core.ExtensionHooks = (*AgentBridge)(nil)

// ExtensionTools returns the host's eager tools as core.Tools.
func (b *AgentBridge) ExtensionTools() []core.Tool {
	return asCoreTools(b.host.Tools())
}

// ExtensionDeferredTools returns the host's deferred tools as core.Tools.
func (b *AgentBridge) ExtensionDeferredTools() []core.Tool {
	return asCoreTools(b.host.DeferredTools())
}

// asCoreTools widens []*ExtensionTool to []core.Tool. Go has no covariance for
// slices, so the copy is required rather than merely tidy.
func asCoreTools(tools []*ExtensionTool) []core.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]core.Tool, len(tools))
	for i, t := range tools {
		out[i] = t
	}
	return out
}

// RunToolCallHook folds the tool_call chain over one pending call, flattening
// FoldedHook into the (blocked, reason, patched) triple the seam speaks.
//
// A Modify outcome replaces the whole `{tool, arguments}` hook input, so the
// patched arguments are lifted back out of it. The host's cross-tool guard has
// already rejected any rewrite of a tool the extension does not own.
func (b *AgentBridge) RunToolCallHook(ctx context.Context, tool string, arguments string) (bool, string, string) {
	args := json.RawMessage(arguments)
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	folded := b.host.RunToolCallHook(ctx, tool, args)
	if folded.Blocked {
		return true, folded.Reason, ""
	}
	var patch struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(folded.Value, &patch); err != nil || len(patch.Arguments) == 0 {
		return false, "", "" // no rewrite — run the original arguments
	}
	return false, "", string(patch.Arguments)
}

// DispatchEvent fans a turn event out to subscribed extensions.
func (b *AgentBridge) DispatchEvent(event string, payload json.RawMessage) {
	b.host.DispatchEvent(event, payload)
}
