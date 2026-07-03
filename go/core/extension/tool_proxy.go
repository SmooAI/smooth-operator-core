package extension

// ExtensionTool — a tool backed by an extension subprocess.
//
// Registered tools appear to the agent as ordinary tools named <extension>.<tool>
// (the MCP convention). Execute forwards to the extension over tool/execute and
// maps the reply back. ExtensionTool structurally satisfies core.Tool
// (Name/Description/Parameters/Execute) so the server appends it into
// core.AgentOptions.Tools without this package importing core.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// toolExecuteTimeout is the upper bound for a single tool/execute round-trip.
const toolExecuteTimeout = 120 * time.Second

// ExtensionTool is a tool exposed by an extension.
type ExtensionTool struct {
	// dottedName is <extension>.<tool> — what the agent/LLM sees.
	dottedName string
	// bareName is the tool name sent to the extension.
	bareName    string
	description string
	parameters  map[string]any
	process     *ExtensionProcess
	toolCtx     Context
}

// NewExtensionTool builds a proxy for one registered tool.
func NewExtensionTool(extName string, reg ToolRegistration, process *ExtensionProcess, toolCtx Context) *ExtensionTool {
	var params map[string]any
	if len(reg.Parameters) > 0 {
		_ = json.Unmarshal(reg.Parameters, &params)
	}
	return &ExtensionTool{
		dottedName:  extName + "." + reg.Name,
		bareName:    reg.Name,
		description: reg.Description,
		parameters:  params,
		process:     process,
		toolCtx:     toolCtx,
	}
}

// Name returns the dotted <extension>.<tool> name.
func (t *ExtensionTool) Name() string { return t.dottedName }

// Description returns the tool's description.
func (t *ExtensionTool) Description() string { return t.description }

// Parameters returns the tool's JSON-schema parameters as a map.
func (t *ExtensionTool) Parameters() map[string]any { return t.parameters }

// Execute forwards the call to the extension over tool/execute and returns the
// content, or an error when the extension reports is_error or the call fails.
func (t *ExtensionTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	arguments, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if args == nil {
		arguments = json.RawMessage("{}")
	}
	params, err := json.Marshal(ToolExecuteParams{
		CallID:    uuid.NewString(),
		Tool:      t.bareName,
		Arguments: arguments,
		Context:   t.toolCtx,
	})
	if err != nil {
		return "", err
	}
	raw, err := t.process.Request(ctx, MethodToolExecute, params, toolExecuteTimeout)
	if err != nil {
		return "", err
	}
	var result ToolExecuteResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", errors.New("malformed tool/execute result: " + err.Error())
	}
	if result.IsError {
		return "", errors.New(result.Content)
	}
	// ponytail: details is dropped — Execute returns only a string. Structured
	// details ride ToolCallUpdate wiring in a later phase; the field is parsed
	// into ToolExecuteResult already for when that lands.
	return result.Content, nil
}

// IsConcurrentSafe reports false: extensions run in their own process with a
// per-extension ordered stream, so the registry serializes their tools
// (conservative until an extension opts in). Present for parity with the Rust
// Tool trait; core.Tool does not require it, so it is advisory.
func (t *ExtensionTool) IsConcurrentSafe() bool { return false }
