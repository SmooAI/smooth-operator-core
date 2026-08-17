package temporal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	core "github.com/SmooAI/smooth-operator-core/go/core"
	"go.temporal.io/sdk/workflow"
)

// Task-queue-independent identifiers used by both the workflow and its consumers.
const (
	// SignalApproveTool names the human-approval signal that unblocks a gated tool
	// call: its payload is the tool-call id to approve.
	SignalApproveTool = "approve_tool"
	// SignalDenyTool names the signal that denies a gated tool call: its payload is
	// the tool-call id to deny (the tool never runs; the model sees an error result).
	SignalDenyTool = "deny_tool"

	// activityModelCall / activityToolInvoke / activityHealthEcho are the registered
	// activity names (defaulting to the AgentTurnActivities method names). The
	// workflow schedules by name so it needs no reference to the activities struct.
	activityModelCall  = "ModelCall"
	activityToolInvoke = "ToolInvoke"
	activityHealthEcho = "HealthEcho"
)

// agentActivityTimeout is the start-to-close timeout for the model/tool activities,
// mirroring the Rust backend's 60s default. (Retry-policy tuning lands with the
// per-step retry work; the SDK default retry policy applies until then.)
const agentActivityTimeout = 60 * time.Second

// AgentTurnActivities is the side-effecting boundary of a durable agent turn: the
// model call and each tool invocation, run as Temporal activities. It delegates to
// the engine's own [core.InProcessActivities] so a durable activity runs the SAME
// side-effect code the in-process path runs inline — the durable and inline paths
// never diverge. Mirrors the Rust `AgentTurnActivities` (which calls
// `engine.llm.chat` / `engine.tools.execute`).
//
// Register it on a worker with `w.RegisterActivity(acts)`; the exported methods are
// registered under their method names (ModelCall / ToolInvoke / HealthEcho).
type AgentTurnActivities struct {
	inproc *core.InProcessActivities
}

// NewAgentTurnActivities builds the activity surface from a model provider, the
// model to call (empty ⇒ the engine default), and the tools available to it.
func NewAgentTurnActivities(llm core.LlmProvider, model string, tools []core.Tool) *AgentTurnActivities {
	return &AgentTurnActivities{inproc: core.NewInProcessActivities(llm, model, tools)}
}

// HealthEcho is a liveness probe backing the scaffold [HealthWorkflow] — it proves
// the SDK integrates end to end without touching the model or tools.
func (*AgentTurnActivities) HealthEcho(_ context.Context, message string) (string, error) {
	return fmt.Sprintf("smooth-operator-temporal ok: %s", message), nil
}

// ModelCall is the "Think" step run as a durable activity. It returns the engine's
// core.ChatResponse directly as the activity output (see dto.go for why no
// projection is needed).
func (a *AgentTurnActivities) ModelCall(ctx context.Context, input ModelCallInput) (core.ChatResponse, error) {
	return a.inproc.ModelCall(ctx, input.Messages, input.Tools)
}

// ToolInvoke is the "Act" step run as a durable activity. Tool BUSINESS failures
// come back on ToolResult.IsError (not as a returned error), matching the engine's
// dispatch — so a business failure is a completed activity whose content goes back
// to the model, while a returned error is retried as a failed activity.
func (a *AgentTurnActivities) ToolInvoke(ctx context.Context, input ToolInvokeInput) (core.ToolResult, error) {
	return a.inproc.ToolInvoke(ctx, input.Call)
}

// AgentTurnInput is the input to [AgentTurnWorkflow]: everything needed to seed the
// conversation and bound the loop. Serializable so it crosses the workflow-start
// boundary. Mirrors the Rust `AgentTurnInput`.
type AgentTurnInput struct {
	// SystemPrompt is the system prompt for the turn (omitted from the seed when empty).
	SystemPrompt string `json:"systemPrompt"`
	// UserMessage is the user message that opens the turn.
	UserMessage string `json:"userMessage"`
	// Tools are the tool specs available to the model.
	Tools []core.ToolSpec `json:"tools,omitempty"`
	// MaxIterations bounds the loop; 0 falls back to the engine's default TurnPolicy.
	MaxIterations int `json:"maxIterations,omitempty"`
	// ApprovalRequiredTools names tools that require human approval before they run.
	// When the model calls one, the workflow blocks DURABLY until an approve_tool /
	// deny_tool signal names that tool call — the block is recorded in workflow
	// history, so it survives worker restarts and can resolve arbitrarily later.
	ApprovalRequiredTools []string `json:"approvalRequiredTools,omitempty"`
	// WaitTool, if set, names a built-in DURABLE WAIT tool. A model call to it with an
	// integer `seconds` argument sleeps the workflow on a Temporal timer (a durable
	// pause that survives restarts and can span days), instead of dispatching a tool
	// activity. Empty disables the wait tool.
	WaitTool string `json:"waitTool,omitempty"`
}

// AgentTurnWorkflow runs one agent turn as a Temporal workflow. It seeds the
// conversation, then drives the engine's [core.DriveTurn] UNCHANGED over a
// workflow-side adapter that schedules the model/tool activities — so the durable
// path is the same loop as in-process. It returns the full conversation (this
// turn's assistant + tool messages appended to the seed).
//
// The human-approval ledger (approved / denied tool-call ids) is workflow state,
// populated by a signal-pump coroutine reading the approve_tool / deny_tool signal
// channels and observed by the adapter's durable gate.
func AgentTurnWorkflow(ctx workflow.Context, input AgentTurnInput) ([]core.ChatMessage, error) {
	adapter := &workflowActivities{
		ctx:              ctx,
		approved:         map[string]bool{},
		denied:           map[string]bool{},
		approvalRequired: toSet(input.ApprovalRequiredTools),
		waitTool:         input.WaitTool,
	}

	// Signal pump: fold approve_tool / deny_tool signals into the ledger as they
	// arrive. Runs on its own workflow coroutine (cooperatively scheduled with the
	// main one — no real concurrency), so map access needs no lock. The coroutine is
	// discarded when the workflow returns.
	workflow.Go(ctx, func(gctx workflow.Context) {
		approveCh := workflow.GetSignalChannel(gctx, SignalApproveTool)
		denyCh := workflow.GetSignalChannel(gctx, SignalDenyTool)
		selector := workflow.NewSelector(gctx)
		selector.AddReceive(approveCh, func(c workflow.ReceiveChannel, _ bool) {
			var id string
			c.Receive(gctx, &id)
			adapter.approved[id] = true
		})
		selector.AddReceive(denyCh, func(c workflow.ReceiveChannel, _ bool) {
			var id string
			c.Receive(gctx, &id)
			adapter.denied[id] = true
		})
		for gctx.Err() == nil {
			selector.Select(gctx)
		}
	})

	messages := make([]core.ChatMessage, 0, 2)
	if input.SystemPrompt != "" {
		messages = append(messages, core.ChatMessage{Role: "system", Content: input.SystemPrompt})
	}
	messages = append(messages, core.ChatMessage{Role: "user", Content: input.UserMessage})

	policy := core.DefaultTurnPolicy()
	if input.MaxIterations > 0 {
		policy.MaxIterations = input.MaxIterations
	}

	// The context.Context handed to DriveTurn is unused by the adapter (which holds
	// the workflow.Context); DriveTurn itself performs no I/O with it.
	return core.DriveTurn(context.Background(), adapter, messages, input.Tools, policy)
}

// HealthWorkflow is a scaffold liveness workflow — it proves the SDK integrates end
// to end and backs the ephemeral-server integration test.
func HealthWorkflow(ctx workflow.Context, message string) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Second})
	var echoed string
	if err := workflow.ExecuteActivity(ctx, activityHealthEcho, message).Get(ctx, &echoed); err != nil {
		return "", err
	}
	return echoed, nil
}

// workflowActivities makes the engine's [core.AgentActivities] surface schedule
// Temporal activities on a workflow.Context. It holds the workflow context and the
// approval/wait config + the ledger the signal pump populates.
//
// It intercepts two engine steps before they reach an activity:
//   - the DURABLE WAIT tool → a Temporal timer (workflow.Sleep), recorded in history;
//   - an approval-gated tool → a durable block on the approve/deny ledger.
//
// core.DriveTurn is unchanged; the gates live here.
type workflowActivities struct {
	ctx              workflow.Context
	approved         map[string]bool
	denied           map[string]bool
	approvalRequired map[string]bool
	waitTool         string
}

// ModelCall schedules the model_call activity and returns its response.
func (a *workflowActivities) ModelCall(_ context.Context, messages []core.ChatMessage, tools []core.ToolSpec) (core.ChatResponse, error) {
	var resp core.ChatResponse
	err := workflow.ExecuteActivity(a.activityCtx(), activityModelCall, ModelCallInput{Messages: messages, Tools: tools}).Get(a.ctx, &resp)
	if err != nil {
		return core.ChatResponse{}, fmt.Errorf("model_call activity: %w", err)
	}
	return resp, nil
}

// ToolInvoke handles the durable wait and the human-approval gate, then (for an
// ordinary tool) schedules the tool_invoke activity.
func (a *workflowActivities) ToolInvoke(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
	// Durable wait: sleep on a Temporal timer instead of dispatching an activity.
	if a.waitTool != "" && call.Name == a.waitTool {
		seconds, ok := parseWaitSeconds(call.Arguments)
		if !ok {
			return core.ToolResult{
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("wait tool '%s' requires an integer `seconds` argument", call.Name),
				IsError:    true,
			}, nil
		}
		if err := workflow.Sleep(a.ctx, time.Duration(seconds)*time.Second); err != nil {
			return core.ToolResult{}, fmt.Errorf("durable timer: %w", err)
		}
		return core.ToolResult{
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("Waited %ds (durable timer).", seconds),
			IsError:    false,
		}, nil
	}

	// Durable human-in-the-loop gate: block until an approve_tool / deny_tool signal
	// names this call id. The wait is recorded in workflow history, so it survives
	// restarts and can resolve hours later.
	if a.approvalRequired[call.Name] {
		id := call.ID
		if err := workflow.Await(a.ctx, func() bool { return a.approved[id] || a.denied[id] }); err != nil {
			return core.ToolResult{}, fmt.Errorf("approval wait: %w", err)
		}
		if a.denied[id] {
			return core.ToolResult{
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("Tool call '%s' was denied by human approval.", call.Name),
				IsError:    true,
			}, nil
		}
	}

	var result core.ToolResult
	err := workflow.ExecuteActivity(a.activityCtx(), activityToolInvoke, ToolInvokeInput{Call: call}).Get(a.ctx, &result)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("tool_invoke activity %q: %w", call.Name, err)
	}
	return result, nil
}

// activityCtx returns the workflow context with the agent activity options applied.
func (a *workflowActivities) activityCtx() workflow.Context {
	return workflow.WithActivityOptions(a.ctx, workflow.ActivityOptions{StartToCloseTimeout: agentActivityTimeout})
}

// parseWaitSeconds reads a non-negative integer `seconds` argument from a tool
// call's raw-JSON arguments. Mirrors the Rust wait tool's `arguments.get("seconds")`.
func parseWaitSeconds(arguments string) (int64, bool) {
	if arguments == "" {
		return 0, false
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return 0, false
	}
	// JSON numbers decode to float64; accept a whole, non-negative value.
	f, ok := args["seconds"].(float64)
	if !ok || f < 0 || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// toSet builds a membership set from a slice (nil-safe).
func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
