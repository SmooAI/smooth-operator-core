package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON
}

// ImageContent is an image attachment on a user message (multimodal turns).
// URL is a `data:` URL (`data:image/png;base64,...`) or a remote `https` URL;
// the client emits it as an OpenAI `image_url` content part. Detail
// ("low"/"high"/"auto") is an optional OpenAI vision hint, omitted when empty.
// The Go sibling of Rust's `conversation::ImageContent`. Pearl th-25ce5c.
type ImageContent struct {
	URL    string
	Detail string
}

// ChatMessage is one message in the OpenAI-shaped conversation.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string // set on role=="tool" messages
	// Images are attachments on a USER message (multimodal turns), emitted as
	// OpenAI `image_url` content parts. Empty for the text-only common case,
	// which keeps the wire byte-identical to before this field existed.
	Images []ImageContent
}

// ToolSpec is a tool advertised to the model.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema
}

// ChatRequest is a single model call.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ToolSpec
	Temperature float64
	MaxTokens   int
	// Metadata is an optional opaque object forwarded as the top-level
	// `metadata` field of the OpenAI-compatible request body (LiteLLM records
	// it on spend logs, giving per-caller cost attribution at the gateway).
	// nil means the field is omitted from the wire entirely; the agent
	// normalizes an empty map to nil so unset stays byte-identical. Mirrors
	// the Rust engine's ChatRequest.metadata (with_metadata).
	Metadata map[string]any
	// ResponseFormat constrains the reply to a JSON Schema — structured output
	// (see structured.go). nil omits the field from the wire entirely, so an
	// unset format leaves the request byte-identical. Mirrors the Rust engine's
	// chat_with_format(format).
	ResponseFormat *ResponseFormat
}

// ChatResponse is the assistant's reply (content and/or tool calls).
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
	// GatewayCostUSD is the gateway's AUTHORITATIVE cost for this request, read
	// from its response header. nil means "unmeasured" — the caller falls back to
	// local ModelPricing. It is deliberately not 0: a real zero and an absent
	// header must stay distinct, or a gateway that reports no cost silently pins
	// spend at zero.
	GatewayCostUSD *float64
}

// ChatClient is the minimal OpenAI-compatible surface the agent needs. The
// GatewayClient implements it against a live endpoint; tests inject a fake.
type ChatClient interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// ChatChunk is one streamed chunk from a streaming chat completion — the standard
// OpenAI streaming chunk shape (the slice the agent reads). Content deltas
// concatenate into the assistant text; tool-call fragments are assembled by their
// Index (ID + Function.Name appear when a call first opens, Function.Arguments
// arrives in fragments). Usage is non-nil on (typically) the final chunk.
type ChatChunk struct {
	// ContentDelta is an incremental piece of assistant text ("" when this chunk
	// carries no text).
	ContentDelta string
	// ToolCallDeltas are incremental tool-call fragments in this chunk.
	ToolCallDeltas []ToolCallDelta
	// Usage, when non-nil, reports cumulative token usage (gateways send it last).
	Usage *Usage
	// CostUSD, when non-nil, is the gateway's authoritative cost for the whole
	// request, read from the response headers before the body was consumed. It
	// arrives on the FIRST chunk, not the last.
	CostUSD *float64
	// Err, when non-nil, reports that the stream FAILED mid-flight (torn body,
	// idle/overall timeout, malformed frame). It is the Go analogue of the
	// reference engine's `Result<StreamEvent>` channel item: a stream that ends
	// this way is truncated, not finished. The chunk carries no other payload and
	// is the last one; the agent aborts the turn rather than emitting StreamDone,
	// so a partial answer can never be checkpointed as a complete one.
	Err error
}

// ToolCallDelta is one tool-call fragment within a streamed chunk.
type ToolCallDelta struct {
	Index        int    // which tool call this fragment belongs to
	ID           string // set when the call first opens ("" in later fragments)
	Name         string // set when the call first opens ("" in later fragments)
	ArgsFragment string // a fragment of the JSON arguments to append
}

// StreamingChatClient is the OPTIONAL streaming surface. A ChatClient that also
// implements it can drive RunStream; the GatewayClient and MockLlmProvider both do.
// ChatStream opens a streaming model call and returns a receive-only channel of
// chunks. The channel is closed when the stream ends; a connect-time failure comes
// back as the returned error, and a MID-STREAM failure MUST arrive as a final
// chunk with Err set (see ChatChunk.Err) — closing the channel silently on a torn
// stream reports truncation as success. Implementations must also stop sending and
// release their transport once ctx is done. Production wires this to the OpenAI
// `create(..., stream=True)` surface.
type StreamingChatClient interface {
	ChatClient
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error)
}

// StreamEventKind tags a StreamEvent.
type StreamEventKind int

const (
	// StreamText is an incremental assistant content delta as it streams in.
	StreamText StreamEventKind = iota
	// StreamToolCall is a tool call the model requested, emitted once before dispatch.
	StreamToolCall
	// StreamToolResult is a tool's result, emitted after it finishes.
	StreamToolResult
	// StreamDone is the single terminal event, carrying the final AgentRunResponse.
	StreamDone
)

// StreamEvent is one event from RunStream. The Kind field selects which payload
// fields are populated, mirroring the C# RunStreamingAsync update sequence and the
// Rust reference engine's event stream:
//
//   - StreamText:       Text holds the content delta.
//   - StreamToolCall:   Name + Arguments hold the requested call.
//   - StreamToolResult: Name + Result hold a finished tool's result.
//   - StreamDone:       Response holds the final AgentRunResponse (the same value
//     Run would return for the same script). Exactly one StreamDone is emitted, last,
//     UNLESS the turn ends in an error (see RunStream's error contract).
type StreamEvent struct {
	Kind      StreamEventKind
	Text      string // StreamText
	Name      string // StreamToolCall / StreamToolResult
	Arguments string // StreamToolCall
	Result    string // StreamToolResult
	// Details carries the structured, UI-facing payload a PostCall hook
	// attached to the ToolResult (nil when none) — forwarded verbatim and
	// un-truncated on StreamToolResult, never shown to the model. Mirrors the
	// Rust engine's AgentEvent::ToolCallComplete.details.
	Details  any              // StreamToolResult
	Response AgentRunResponse // StreamDone
}

// Tool is a callable the agent may invoke.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// FuncTool wraps a function as a Tool (the AIFunctionFactory analogue).
type FuncTool struct {
	ToolName string
	Desc     string
	Params   map[string]any
	Fn       func(ctx context.Context, args map[string]any) (string, error)
}

func (t FuncTool) Name() string               { return t.ToolName }
func (t FuncTool) Description() string        { return t.Desc }
func (t FuncTool) Parameters() map[string]any { return t.Params }
func (t FuncTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return t.Fn(ctx, args)
}

// DelegateTool builds a Tool that delegates a subtask to a child SmoothAgent.
//
// A sub-agent is just a tool backed by another agent: the model calls this tool
// with a "task" argument, the child agent runs that task, and the child's final
// reply becomes the tool result — composing with the existing tool loop, no
// special wiring. The child can have its own instructions, tools, knowledge, etc.
func DelegateTool(name, description string, child *SmoothAgent) Tool {
	return FuncTool{
		ToolName: name,
		Desc:     description,
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{"type": "string", "description": "The subtask for the sub-agent to perform."},
			},
			"required": []string{"task"},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			task, _ := args["task"].(string)
			result, err := child.Run(ctx, task, nil)
			if err != nil {
				return "", err
			}
			return result.Text, nil
		},
	}
}

// AgentOptions configures a SmoothAgent turn. Mirrors the sibling cores' options.
type AgentOptions struct {
	// Extensions wires a SEP extension host into this agent — the Go sibling of
	// Rust's `Agent::with_extension_host`. Supply it with
	// `extension.NewAgentBridge(host)`. Its tools are merged into the agent's
	// tool set, and its hook lanes run at the same points Rust runs them.
	// nil (the default) leaves the loop exactly as it was before extensions.
	Extensions ExtensionHooks

	// NextUserImages attaches image(s) to the CURRENT turn's user message. A
	// host sets it when a chat turn carried image attachments; the agent emits
	// them as OpenAI `image_url` content parts on that one turn. Empty (the
	// default) leaves every text-only turn byte-identical. Mirrors Rust's
	// `AgentConfig::with_user_images`. Pearl th-25ce5c.
	NextUserImages []ImageContent

	Instructions  string
	Model         string
	MaxIterations int
	MaxTokens     int
	// ModelMaxOutput is the active model's hard output ceiling (its
	// max_output_tokens). When set, every request's MaxTokens is clamped to
	// min(MaxTokens, *ModelMaxOutput) so a budget/policy MaxTokens (which may be
	// tuned high) can never exceed what the model can physically emit — otherwise a
	// reasoning model burns its budget on reasoning and returns empty, or the
	// upstream 400s (e.g. groq-compound caps output at 8192). nil (the default)
	// leaves MaxTokens unclamped. Source it from the gateway's /model/info
	// (model_info.max_output_tokens). Mirror of the Rust engine's
	// LlmClient.with_model_ceiling (EPIC th-1cc9fa).
	ModelMaxOutput *int
	Temperature    float64
	Knowledge      Knowledge
	KnowledgeTopK  int
	// Reranker reorders retrieved hits before injection (nil = passthrough).
	Reranker Reranker
	// KnowledgeCandidateK is the pool size retrieved before reranking; when greater
	// than KnowledgeTopK, more docs are fetched, reranked, then trimmed to TopK.
	KnowledgeCandidateK int
	// Memory, if set, recalls relevant facts into context each turn.
	Memory Memory
	// MemoryTopK is how many memory entries to recall per turn (0 = default 4).
	MemoryTopK int
	Tools      []Tool
	// ParallelToolCalls, when true and an assistant turn returns >=2 tool calls,
	// dispatches them concurrently (goroutines + sync.WaitGroup) instead of
	// sequentially. Tool-result messages are still appended in the original
	// ToolCalls order, so the transcript stays deterministic regardless of
	// completion order. Default false preserves the sequential behaviour. Per-tool
	// semantics (clearance, human-gate approval, tool_search promotion, JSON
	// parsing, error handling) are unchanged — only the dispatch loop runs in parallel.
	ParallelToolCalls bool
	// DeferredTools are registered but with their schemas HIDDEN from the model.
	// When any are present, a built-in tool_search meta-tool is advertised in their
	// place; the model calls it to fuzzy-match and promote the ones it needs, which
	// then become visible + dispatchable on subsequent turns. Keeps the tool schema
	// payload small when there are many rarely-used tools. An unpromoted deferred
	// tool is NOT dispatchable.
	DeferredTools []Tool
	// MaxContextTokens is the approximate token budget for the context window.
	// Before each model call, older non-system messages are dropped (sliding
	// window) to stay under it. 0 uses the default (8000); negative disables.
	MaxContextTokens int
	// Budget, if set, stops the turn early once accumulated usage/cost hits it.
	Budget *CostBudget
	// Pricing overrides the per-model cost table (defaults to DefaultPricing).
	Pricing map[string]ModelPricing
	// CheckpointStore, with ConversationID, persists/resumes the conversation.
	CheckpointStore CheckpointStore
	// ConversationID keys the checkpoint store (required to use checkpointing).
	ConversationID string
	// Clearance, if set, gates which tools may be dispatched. A tool the clearance
	// forbids is not executed — a "tool not permitted" result is returned to the
	// model instead. Nil allows every tool (the prior behaviour).
	Clearance *Clearance
	// HumanGate, when set, is asked for approval before running any tool call for
	// which RequiresApproval returns true. A denied call is not executed; the model
	// is told it was denied and can adapt.
	HumanGate HumanGate
	// RequiresApproval reports which tool calls need human approval (e.g. writes /
	// destructive actions), given the tool name and parsed arguments. nil = none.
	// Only consulted when HumanGate is set. Example:
	//
	//	func(name string, _ map[string]any) bool { return name == "delete_record" }
	RequiresApproval func(name string, args map[string]any) bool
	// PermissionMode, when non-nil, enables the native permission engine (the Go
	// port of the Rust PermissionHook — see permission.go / permission_gate.go):
	// every tool call is classified by Decide and gated before it runs. A hard
	// circuit-breaker (rm -rf /, credential paths, pipe-to-shell, dangerous
	// domains, env dumps) is always denied; a mutating/network call Asks (routed
	// to HumanGate, or fails closed with no gate); read-only calls Allow. nil (the
	// default) disables the engine entirely — dispatch is byte-for-byte unchanged.
	// Setting DenyPolicy without PermissionMode enables the engine at
	// AutoModeAsk. The HumanGate seam doubles as the Ask approver (an
	// ApproveAlways response persists a grant to PermissionGrantsPath).
	PermissionMode *AutoMode
	// DenyPolicy is a consumer-supplied deny policy (declarative TOML rules +
	// predicates) evaluated FIRST on every gated call — a match is a
	// circuit-breaker no grant or mode can waive. nil = none. Enables the
	// permission engine even when PermissionMode is nil.
	DenyPolicy *DenyPolicy
	// PermissionGrants is the live allow-list consulted on an Ask before
	// prompting; a matching grant auto-approves silently. nil disables persistence.
	PermissionGrants *SharedGrants
	// PermissionGrantsPath is where an ApproveAlways answer persists a new grant
	// (the user-scope wonk-allow.toml). Empty degrades approve-always to approve-once.
	PermissionGrantsPath string
	// MaxRetries is the number of ADDITIONAL attempts after the first if the model
	// call returns a transient error (rate-limit, 5xx, dropped connection). 0 (the
	// default) preserves today's behaviour: a single attempt, error returned
	// immediately. Only the model call is retried — never tool execution.
	MaxRetries int
	// RetryBackoff is the base delay for exponential backoff between retries. The
	// wait before retry attempt n (1-indexed) is RetryBackoff * 2^(n-1). The zero
	// value means no real delay (retries fire immediately) — which is what tests use
	// so they don't sleep; production should set a small base such as 200ms.
	RetryBackoff time.Duration
	// Hooks are ToolHooks run in registration order around every dispatched tool
	// call: each PreCall runs before the tool (any error blocks it), each PostCall
	// runs after with a mutable *ToolResult it may redact. Nil (the default) → no
	// hooks, behaviour unchanged. Mirrors the Rust engine's ToolRegistry hook chain.
	Hooks []ToolHook
	// Metadata is forwarded verbatim as every model request's top-level
	// `metadata` object (LiteLLM spend-log attribution — e.g. an agent slug so
	// per-agent LLM spend is queryable at the gateway). A nil or empty map
	// sends nothing: the wire stays byte-identical when unset. Mirrors the
	// Rust engine's AgentConfig.with_metadata.
	Metadata map[string]any
}

// normalizeMetadata maps an empty metadata object to nil so that "unset" and
// "empty" are byte-identical on the wire (Rust parity: with_metadata filters
// empty maps to None).
func normalizeMetadata(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// AgentRunResponse is the result of a turn.
type AgentRunResponse struct {
	Text       string
	Iterations int
	ToolCalls  int
	Usage      Usage
	CostUSD    float64
	// BudgetExceeded is true if the turn stopped because the cost/token budget was hit.
	BudgetExceeded bool
}

const (
	defaultModel = "claude-haiku-4-5"
	// defaultMaxIterations / defaultMaxTokens were 8 / 512 — chat-widget sizing that
	// STARVES reasoning models (they burn the budget on reasoning_content and return
	// empty). Raised to 20 / 8192 now that the per-model output ceiling (ModelMaxOutput)
	// clamps MaxTokens to what each model can emit — a higher cap only bounds runaway
	// output; concise answers stay concise (EPIC th-1cc9fa).
	defaultMaxIterations    = 20
	defaultMaxTokens        = 8192
	defaultKnowledgeTopK    = 4
	defaultMaxContextTokens = 8000
)

// effectiveMaxTokens clamps a configured max_tokens budget to the model's hard
// output ceiling when one is known: it returns min(configured, *ceiling). A nil or
// non-positive ceiling leaves the budget unclamped. The result is never 0 for a
// positive budget — a real budget is never clamped away to nothing (some gateways
// reject max_tokens=0 or treat it as "no output"). Mirror of the Rust engine's
// LlmClient::effective_max_tokens (EPIC th-1cc9fa).
func effectiveMaxTokens(configured int, ceiling *int) int {
	if ceiling == nil || *ceiling <= 0 || *ceiling >= configured {
		return configured
	}
	return *ceiling
}

// SmoothAgent is a native, in-process agent.
type SmoothAgent struct {
	client      ChatClient
	options     AgentOptions
	toolsByName map[string]Tool
	permGate    *PermissionGate // nil unless the permission engine is enabled
}

// NewSmoothAgent constructs an agent over an OpenAI-compatible ChatClient.
func NewSmoothAgent(client ChatClient, options AgentOptions) *SmoothAgent {
	if client == nil {
		panic("core: client is required")
	}
	// Extension tools join as ORDINARY tools (already namespaced
	// `<extension>.<tool>`), so they are visible to the model, dispatched, and
	// permission-gated by exactly the same machinery as native tools — the same
	// no-special-casing property the Rust host has. Eager tools go into the
	// visible set; deferred ones stay hidden until `tool_search` promotes them.
	if options.Extensions != nil {
		options.Tools = append(options.Tools, options.Extensions.ExtensionTools()...)
		options.DeferredTools = append(options.DeferredTools, options.Extensions.ExtensionDeferredTools()...)
	}
	// NOTE: deferred tools are deliberately NOT in byName — an unpromoted
	// deferred tool must resolve to nothing until `tool_search` promotes it.
	byName := make(map[string]Tool, len(options.Tools))
	for _, t := range options.Tools {
		byName[t.Name()] = t
	}
	return &SmoothAgent{client: client, options: options, toolsByName: byName, permGate: buildPermissionGate(options)}
}

// buildPermissionGate assembles the permission gate from options, or returns nil
// when the engine is disabled (no PermissionMode and no DenyPolicy) — the
// additive no-op default that leaves dispatch unchanged.
func buildPermissionGate(o AgentOptions) *PermissionGate {
	if o.PermissionMode == nil && o.DenyPolicy == nil {
		return nil
	}
	mode := AutoModeAsk
	if o.PermissionMode != nil {
		mode = *o.PermissionMode
	}
	return &PermissionGate{
		Mode:        mode,
		Grants:      o.PermissionGrants,
		PersistPath: o.PermissionGrantsPath,
		DenyPolicy:  o.DenyPolicy,
		Approver:    o.HumanGate,
	}
}

// userTurnMessage builds this turn's user message, attaching any images the
// host set for it. A helper because BOTH run and RunStream push this message
// twice (the live `messages` slice and `turnMessages`) — all four sites have to
// agree, or a multimodal turn reaches the model without its images.
func (a *SmoothAgent) userTurnMessage(message string) ChatMessage {
	return ChatMessage{Role: "user", Content: message, Images: a.options.NextUserImages}
}

func (a *SmoothAgent) buildSystem(message string) string {
	system := a.options.Instructions

	if a.options.Memory != nil {
		topK := a.options.MemoryTopK
		if topK <= 0 {
			topK = defaultKnowledgeTopK
		}
		recalled := a.options.Memory.Recall(message, topK)
		if len(recalled) > 0 {
			lines := make([]string, len(recalled))
			for i, e := range recalled {
				lines[i] = "- " + e.Text
			}
			system = strings.TrimSpace(system + "\n\nRelevant memory (things you remember about this user/context):\n" + strings.Join(lines, "\n"))
		}
	}

	if a.options.Knowledge != nil {
		topK := a.options.KnowledgeTopK
		if topK <= 0 {
			topK = defaultKnowledgeTopK
		}
		candidateK := topK
		if a.options.KnowledgeCandidateK > candidateK {
			candidateK = a.options.KnowledgeCandidateK
		}
		hits := a.options.Knowledge.Query(message, candidateK)
		if a.options.Reranker != nil {
			hits = a.options.Reranker.Rerank(message, hits)
		}
		if len(hits) > topK {
			hits = hits[:topK]
		}
		if len(hits) > 0 {
			parts := make([]string, len(hits))
			for i, h := range hits {
				parts[i] = fmt.Sprintf("[%s] %s", h.Source, h.Content)
			}
			block := strings.Join(parts, "\n\n")
			system = strings.TrimSpace(system + "\n\nKnowledge base (ground all facts ONLY in this; if it is not here, say you don't know):\n" + block)
		}
	}
	return system
}

func (a *SmoothAgent) toolSpecs(search *ToolSearch) []ToolSpec {
	// Eager (always-visible) tools, plus — when deferred tools exist — the built-in
	// tool_search meta-tool and any deferred tools promoted so far this run.
	// Deferred-but-unpromoted tools are deliberately omitted so the model never sees
	// their schemas until it searches for them.
	visible := make([]Tool, 0, len(a.options.Tools)+1)
	visible = append(visible, a.options.Tools...)
	if search != nil && search.HasDeferred() {
		visible = append(visible, search)
		visible = append(visible, search.PromotedTools()...)
	}
	if len(visible) == 0 {
		return nil
	}
	specs := make([]ToolSpec, len(visible))
	for i, t := range visible {
		specs[i] = ToolSpec{Name: t.Name(), Description: t.Description(), Parameters: t.Parameters()}
	}
	return specs
}

// Run executes a single turn. history is prior conversation messages (multi-turn).
func (a *SmoothAgent) Run(ctx context.Context, message string, history []ChatMessage) (AgentRunResponse, error) {
	return a.run(ctx, message, history, nil)
}

// RunThread executes a single turn carried by a SmoothAgentThread: the turn is seeded
// from the thread's messages, and this turn's new user + assistant (+ tool) messages
// are appended back to the thread before returning. The thread takes precedence over
// any history as the prior context. Run (single-shot/history) keeps working unchanged.
func (a *SmoothAgent) RunThread(ctx context.Context, message string, thread *SmoothAgentThread) (AgentRunResponse, error) {
	return a.run(ctx, message, nil, thread)
}

func (a *SmoothAgent) run(ctx context.Context, message string, history []ChatMessage, thread *SmoothAgentThread) (AgentRunResponse, error) {
	a.sepDispatch(sepTurnStart, map[string]any{"agent_id": a.options.Model})

	messages := make([]ChatMessage, 0, len(history)+2)
	if system := a.buildSystem(message); system != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: system})
	}

	// Source prior conversation: the thread (if passed) wins, then the checkpoint
	// store (if configured), then the explicit history argument.
	cpStore := a.options.CheckpointStore
	cpID := a.options.ConversationID
	prior := history
	if cpStore != nil && cpID != "" {
		if loaded, ok := cpStore.Load(cpID); ok {
			prior = loaded.Messages
		}
	}
	if thread != nil {
		prior = thread.Messages()
	}
	messages = append(messages, prior...)
	messages = append(messages, a.userTurnMessage(message))

	// Track this turn's new messages (user + assistant + tool, never system) so they
	// can be appended back to the thread on exit. Slicing the live messages by index
	// would be unsafe — compaction may drop/reorder it mid-turn.
	turnMessages := []ChatMessage{a.userTurnMessage(message)}

	// Persist the conversation (sans system prompt, rebuilt each turn) on any exit,
	// and append this turn's messages to the thread.
	defer func() {
		if cpStore != nil && cpID != "" {
			nonSystem := make([]ChatMessage, 0, len(messages))
			for _, m := range messages {
				if m.Role != "system" {
					nonSystem = append(nonSystem, m)
				}
			}
			cpStore.Save(Checkpoint{ConversationID: cpID, Messages: nonSystem})
		}
		if thread != nil {
			thread.Extend(turnMessages)
		}
	}()

	model := a.options.Model
	if model == "" {
		model = defaultModel
	}
	maxIter := a.options.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}
	maxTokens := a.options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	// Clamp to the model's output ceiling (nil ⇒ unclamped) so MaxTokens never
	// exceeds what the model can physically emit (EPIC th-1cc9fa).
	maxTokens = effectiveMaxTokens(maxTokens, a.options.ModelMaxOutput)
	// Per-run promotion state for deferred tools (nil when none registered).
	var search *ToolSearch
	if len(a.options.DeferredTools) > 0 {
		search = NewToolSearch(a.options.DeferredTools)
	}
	maxContext := a.options.MaxContextTokens
	if maxContext == 0 {
		maxContext = defaultMaxContextTokens
	}

	toolCalls := 0
	lastText := ""
	var tracker CostTracker

	for iteration := 1; iteration <= maxIter; iteration++ {
		// Keep the context window within budget before each model call.
		messages = compact(messages, maxContext)
		// Recompute tool specs each iteration: a tool_search call in the previous
		// iteration may have promoted deferred tools into view.
		tools := a.toolSpecs(search)
		resp, err := a.callModel(ctx, ChatRequest{
			Model:       model,
			Messages:    messages,
			Tools:       tools,
			Temperature: a.options.Temperature,
			MaxTokens:   maxTokens,
			Metadata:    normalizeMetadata(a.options.Metadata),
		})
		if err != nil {
			return AgentRunResponse{}, fmt.Errorf("model call: %w", err)
		}
		tracker.RecordWithGatewayCost(model, resp.Usage, resp.GatewayCostUSD, a.options.Pricing)
		lastText = resp.Content

		assistantMsg := ChatMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		messages = append(messages, assistantMsg)
		turnMessages = append(turnMessages, assistantMsg)

		// Stop early if this turn has hit its token/cost budget.
		if tracker.Exceeds(a.options.Budget) {
			a.sepTurnComplete(iteration, lastText)
			return AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD, BudgetExceeded: true}, nil
		}

		if len(resp.ToolCalls) == 0 {
			a.sepTurnComplete(iteration, lastText)
			return AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}, nil
		}

		toolCalls += len(resp.ToolCalls)
		// Dispatch the tool calls — concurrently when enabled and there's more than
		// one — but always append the results in the original ToolCalls order so the
		// transcript stays deterministic. dispatchTool turns failures/denials into a
		// result string, so a panicking sibling can't abort the others.
		// SEP `tool_call` hook: fold over EVERY pending call before any of them
		// run, so an extension can veto or rewrite arguments. Vetoed calls never
		// reach dispatchTool; their veto reason becomes the tool result so the
		// model learns why. With no host configured this returns the input
		// untouched. The registry's own gates still apply afterward.
		planned, sepBlocks := a.sepToolCallPlan(ctx, resp.ToolCalls)

		results := make([]string, len(planned))
		if a.options.ParallelToolCalls && len(planned) > 1 {
			var wg sync.WaitGroup
			for i, tc := range planned {
				if reason, blocked := sepBlocks[tc.ID]; blocked {
					results[i] = sepBlockedResult(reason)
					continue
				}
				wg.Add(1)
				go func(i int, tc ToolCall) {
					defer wg.Done()
					results[i] = a.dispatchTool(ctx, tc, search)
				}(i, tc)
			}
			wg.Wait()
		} else {
			for i, tc := range planned {
				if reason, blocked := sepBlocks[tc.ID]; blocked {
					results[i] = sepBlockedResult(reason)
					continue
				}
				results[i] = a.dispatchTool(ctx, tc, search)
			}
		}
		for i, tc := range planned {
			toolMsg := ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: results[i]}
			messages = append(messages, toolMsg)
			turnMessages = append(turnMessages, toolMsg)
		}
	}

	a.sepTurnComplete(maxIter, lastText)
	return AgentRunResponse{Text: lastText, Iterations: maxIter, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}, nil
}

// RunStream streams a single turn, delivering incremental StreamEvents on the
// returned channel. It drives the SAME agentic loop as Run (system/knowledge/memory
// build, seed messages, per-iteration compaction, cost tracking, budget early-stop,
// deferred-tool specs, clearance + human-gate on dispatch, checkpoint/thread
// persistence on exit) — but calls the model in STREAMING mode and emits events as
// work happens:
//
//   - a StreamText event per non-empty content delta as it streams in;
//   - a StreamToolCall event per requested tool call, after that iteration's model
//     stream ends, BEFORE the call is dispatched;
//   - a StreamToolResult event per tool, after it finishes (in original call order
//     even when ParallelToolCalls runs them concurrently);
//   - exactly one terminal StreamDone event carrying the same AgentRunResponse Run
//     would return for the same script.
//
// Error contract (idiomatic Go): the client must implement StreamingChatClient — if
// it does not, RunStream returns a nil channel and a non-nil error synchronously and
// runs nothing. Once the turn is running, a model-call error aborts it: the channel
// is closed WITHOUT a StreamDone and the error is stored, retrievable via the
// returned *Stream's Err() after the channel drains. So a caller ranges the channel
// to completion, then checks Err(); a clean turn ends with a StreamDone and Err()==nil.
//
// That includes a stream that is TRUNCATED rather than finished — an idle or overall
// timeout, a torn connection (see ChatChunk.Err). Those abort the turn too: no
// StreamDone, non-nil Err(), and the partial assistant text is neither appended to
// the thread nor checkpointed. A half-answer must never be indistinguishable from a
// whole one.
//
// SEP extension hooks run on this path exactly as they do on Run: `turn_start` at
// the top, the `tool_call` veto/rewrite fold before every dispatch, and
// `message_end`/`turn_end` before the terminal StreamDone.
//
// A caller that stops consuming early must call Stream.Close (or cancel ctx) to
// release the turn goroutine and its model connection.
//
// NOTE: retry-with-backoff (MaxRetries/RetryBackoff) is intentionally NOT applied to
// the streaming model call — re-running it after a mid-stream failure would re-emit
// already-yielded chunks. Retry stays scoped to non-streaming Run (see callModel);
// this mirrors the C# RunStreamingAsync decision.
func (a *SmoothAgent) RunStream(ctx context.Context, message string, thread *SmoothAgentThread) (*Stream, error) {
	sc, ok := a.client.(StreamingChatClient)
	if !ok {
		return nil, fmt.Errorf("core: client does not implement StreamingChatClient (no ChatStream)")
	}

	// The turn runs under a context the Stream owns, so abandoning it (Close, or
	// cancelling the caller's ctx) unblocks every send in the loop AND tears down
	// the underlying model request. Without this the turn goroutine parks forever
	// on an unbuffered send the moment a consumer stops ranging, holding its socket.
	ctx, cancel := context.WithCancel(ctx)
	events := make(chan StreamEvent)
	stream := &Stream{events: events, cancel: cancel}
	go func() {
		// LIFO: cancel first so the model-stream producer unblocks, then close.
		defer close(events)
		defer cancel()
		if err := a.runStream(ctx, sc, message, thread, events); err != nil {
			stream.mu.Lock()
			stream.err = err
			stream.mu.Unlock()
		}
	}()
	return stream, nil
}

// Stream is the handle RunStream returns: range Events() to consume the turn's
// StreamEvents, then call Err() (after the channel drains) to see whether the turn
// aborted with a model error.
type Stream struct {
	events <-chan StreamEvent
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

// Events returns the channel of streamed events. It is closed when the turn ends.
func (s *Stream) Events() <-chan StreamEvent { return s.events }

// Close abandons the turn: the loop stops, the in-flight model request is torn
// down, and the events channel closes. It is safe to call more than once, and from
// a different goroutine than the consumer.
//
// A consumer that drains Events() to completion need not call it. A consumer that
// stops early MUST — Go channels give the sender no way to notice a receiver has
// walked away (unlike the reference's `tx.send().is_err()`), so this is that
// signal. `defer stream.Close()` is the safe habit.
func (s *Stream) Close() { s.cancel() }

// Err returns the error that aborted the turn, or nil if it completed cleanly.
// Call it only after Events() has been fully drained (the channel closed).
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// emitStream delivers one event, or gives up if the turn's context is done —
// which is what happens when the consumer abandons the stream (Stream.Close or a
// cancelled ctx). A bare `out <-` parks forever instead, since the events channel
// is unbuffered and nothing will ever receive.
func emitStream(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *SmoothAgent) runStream(ctx context.Context, sc StreamingChatClient, message string, thread *SmoothAgentThread, out chan<- StreamEvent) error {
	a.sepDispatch(sepTurnStart, map[string]any{"agent_id": a.options.Model})

	messages := make([]ChatMessage, 0, 2)
	if system := a.buildSystem(message); system != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: system})
	}

	cpStore := a.options.CheckpointStore
	cpID := a.options.ConversationID
	var prior []ChatMessage
	if cpStore != nil && cpID != "" {
		if loaded, ok := cpStore.Load(cpID); ok {
			prior = loaded.Messages
		}
	}
	if thread != nil {
		prior = thread.Messages()
	}
	messages = append(messages, prior...)
	messages = append(messages, a.userTurnMessage(message))

	turnMessages := []ChatMessage{a.userTurnMessage(message)}
	defer func() {
		if cpStore != nil && cpID != "" {
			nonSystem := make([]ChatMessage, 0, len(messages))
			for _, m := range messages {
				if m.Role != "system" {
					nonSystem = append(nonSystem, m)
				}
			}
			cpStore.Save(Checkpoint{ConversationID: cpID, Messages: nonSystem})
		}
		if thread != nil {
			thread.Extend(turnMessages)
		}
	}()

	model := a.options.Model
	if model == "" {
		model = defaultModel
	}
	maxIter := a.options.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}
	maxTokens := a.options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	// Clamp to the model's output ceiling (nil ⇒ unclamped) so MaxTokens never
	// exceeds what the model can physically emit (EPIC th-1cc9fa).
	maxTokens = effectiveMaxTokens(maxTokens, a.options.ModelMaxOutput)
	var search *ToolSearch
	if len(a.options.DeferredTools) > 0 {
		search = NewToolSearch(a.options.DeferredTools)
	}
	maxContext := a.options.MaxContextTokens
	if maxContext == 0 {
		maxContext = defaultMaxContextTokens
	}

	toolCalls := 0
	lastText := ""
	var tracker CostTracker

	for iteration := 1; iteration <= maxIter; iteration++ {
		messages = compact(messages, maxContext)
		tools := a.toolSpecs(search)

		// Stream the model call, emitting text deltas while accumulating the full
		// assistant message (content + tool calls + usage).
		chunks, err := sc.ChatStream(ctx, ChatRequest{
			Model: model, Messages: messages, Tools: tools,
			Temperature: a.options.Temperature, MaxTokens: maxTokens,
			Metadata: normalizeMetadata(a.options.Metadata),
		})
		if err != nil {
			return fmt.Errorf("model stream: %w", err)
		}
		var content strings.Builder
		partials := map[int]*ToolCall{}
		var order []int
		var usage Usage
		var gatewayCost *float64
		for chunk := range chunks {
			// A mid-stream failure aborts the turn. Falling through would append a
			// TRUNCATED assistant message to history and emit StreamDone with
			// Err()==nil — the silent data loss this contract exists to prevent.
			if chunk.Err != nil {
				return fmt.Errorf("model stream: %w", chunk.Err)
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			if chunk.CostUSD != nil {
				gatewayCost = chunk.CostUSD
			}
			if chunk.ContentDelta != "" {
				content.WriteString(chunk.ContentDelta)
				if err := emitStream(ctx, out, StreamEvent{Kind: StreamText, Text: chunk.ContentDelta}); err != nil {
					return err
				}
			}
			for _, d := range chunk.ToolCallDeltas {
				cur, seen := partials[d.Index]
				if !seen {
					cur = &ToolCall{}
					partials[d.Index] = cur
					order = append(order, d.Index)
				}
				if d.ID != "" {
					cur.ID = d.ID
				}
				if d.Name != "" {
					cur.Name = d.Name
				}
				cur.Arguments += d.ArgsFragment
			}
		}
		sort.Ints(order)
		assembled := make([]ToolCall, 0, len(order))
		for _, idx := range order {
			assembled = append(assembled, *partials[idx])
		}

		tracker.RecordWithGatewayCost(model, usage, gatewayCost, a.options.Pricing)
		lastText = content.String()

		assistantMsg := ChatMessage{Role: "assistant", Content: lastText, ToolCalls: assembled}
		messages = append(messages, assistantMsg)
		turnMessages = append(turnMessages, assistantMsg)

		if tracker.Exceeds(a.options.Budget) {
			a.sepTurnComplete(iteration, lastText)
			return emitStream(ctx, out, StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD, BudgetExceeded: true}})
		}

		if len(assembled) == 0 {
			a.sepTurnComplete(iteration, lastText)
			return emitStream(ctx, out, StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}})
		}

		toolCalls += len(assembled)
		// SEP `tool_call` hook — the SAME veto/rewrite fold Run does, for the same
		// reason: an extension that can block a write tool on the non-streaming
		// path but not the streaming one enforces nothing at all.
		planned, sepBlocks := a.sepToolCallPlan(ctx, assembled)

		// Emit a tool_call event per call (original order) BEFORE dispatch, with the
		// POST-hook arguments, so the UI shows what will actually run. A vetoed call
		// is still announced and still gets its StreamToolResult (carrying the veto
		// reason), keeping the call/result pairing intact.
		for _, tc := range planned {
			if err := emitStream(ctx, out, StreamEvent{Kind: StreamToolCall, Name: tc.Name, Arguments: tc.Arguments}); err != nil {
				return err
			}
		}
		// Reuse the SAME dispatch path as Run (clearance, human-gate, tool_search,
		// JSON parsing, error-to-string, ParallelToolCalls). Results surface in
		// original call order so the event stream stays deterministic.
		results := make([]ToolResult, len(planned))
		if a.options.ParallelToolCalls && len(planned) > 1 {
			var wg sync.WaitGroup
			for i, tc := range planned {
				if reason, blocked := sepBlocks[tc.ID]; blocked {
					results[i] = ToolResult{ToolCallID: tc.ID, Content: sepBlockedResult(reason), IsError: true}
					continue
				}
				wg.Add(1)
				go func(i int, tc ToolCall) {
					defer wg.Done()
					results[i] = a.dispatchToolResult(ctx, tc, search)
				}(i, tc)
			}
			wg.Wait()
		} else {
			for i, tc := range planned {
				if reason, blocked := sepBlocks[tc.ID]; blocked {
					results[i] = ToolResult{ToolCallID: tc.ID, Content: sepBlockedResult(reason), IsError: true}
					continue
				}
				results[i] = a.dispatchToolResult(ctx, tc, search)
			}
		}
		for i, tc := range planned {
			toolMsg := ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: results[i].Content}
			messages = append(messages, toolMsg)
			turnMessages = append(turnMessages, toolMsg)
			if err := emitStream(ctx, out, StreamEvent{Kind: StreamToolResult, Name: tc.Name, Result: results[i].Content, Details: results[i].Details}); err != nil {
				return err
			}
		}
	}

	a.sepTurnComplete(maxIter, lastText)
	return emitStream(ctx, out, StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: maxIter, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}})
}

// callModel invokes the model with bounded retry-with-exponential-backoff.
//
// On a transient error (anything the client returns — rate-limit, 5xx, dropped
// connection) the call is retried up to MaxRetries additional times, waiting
// RetryBackoff * 2^(n-1) before the n-th (1-indexed) retry. If all attempts fail the
// LAST error is returned, so the turn fails exactly as it did before retries existed.
// Only this model call is retried — tool execution is not. A zero RetryBackoff (the
// test default) means retries fire with no real sleep.
func (a *SmoothAgent) callModel(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := a.client.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt >= a.options.MaxRetries {
			return ChatResponse{}, lastErr // retries exhausted (or disabled): propagate last error
		}
		if delay := a.options.RetryBackoff * (1 << attempt); delay > 0 {
			select {
			case <-ctx.Done():
				return ChatResponse{}, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
}

func (a *SmoothAgent) dispatchTool(ctx context.Context, tc ToolCall, search *ToolSearch) string {
	return a.dispatchToolResult(ctx, tc, search).Content
}

// dispatchToolResult is dispatchTool returning the full ToolResult, so callers
// that surface results to a UI (runStream) can forward the structured Details a
// PostCall hook attached — the model itself only ever sees Content. Mirrors the
// Rust engine's AgentEvent::ToolCallComplete.details.
func (a *SmoothAgent) dispatchToolResult(ctx context.Context, tc ToolCall, search *ToolSearch) ToolResult {
	errResult := func(msg string) ToolResult {
		return ToolResult{ToolCallID: tc.ID, Content: msg, IsError: true}
	}
	// Enforce the role's tool clearance before dispatch: a forbidden tool is never
	// executed — the model is told it isn't permitted, mirroring how the loop
	// surfaces other tool errors.
	if a.options.Clearance != nil && !a.options.Clearance.IsAllowed(tc.Name) {
		return errResult(fmt.Sprintf("error: tool '%s' is not permitted for this role", tc.Name))
	}

	// Resolve the tool: eager tools first, then the built-in tool_search meta-tool,
	// then deferred tools that have been promoted. An unpromoted deferred tool
	// resolves to nothing — it's invisible until searched for.
	tool, ok := a.toolsByName[tc.Name]
	if !ok && search != nil {
		if tc.Name == search.Name() {
			tool, ok = search, true
		} else {
			tool, ok = search.ToolByName(tc.Name)
		}
	}
	if !ok {
		return errResult(fmt.Sprintf("error: unknown tool '%s'", tc.Name))
	}
	args := map[string]any{}
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return errResult(fmt.Sprintf("error: tool '%s' received invalid JSON arguments", tc.Name))
		}
	}

	// Native permission engine (opt-in via PermissionMode / DenyPolicy): classify
	// and gate the call before it runs. A deny (circuit-breaker, deny-policy, or a
	// fail-closed Ask) is fed back to the model as a result — the tool never runs.
	if a.permGate != nil {
		if err := a.permGate.Check(ctx, tc.Name, args); err != nil {
			return errResult(fmt.Sprintf("error: tool '%s' %v", tc.Name, err))
		}
	}

	// Human-in-the-loop: pause for approval before running a flagged (write/sensitive)
	// tool. A denial is fed back to the model as a result — the tool never runs.
	if a.options.HumanGate != nil && a.options.RequiresApproval != nil && a.options.RequiresApproval(tc.Name, args) {
		req := HumanApprovalRequest{ToolName: tc.Name, Arguments: args, Prompt: fmt.Sprintf("Approve calling tool '%s'?", tc.Name)}
		decision, err := a.options.HumanGate(ctx, req)
		if err != nil {
			return errResult(fmt.Sprintf("error: human gate for tool '%s' failed: %v", tc.Name, err))
		}
		if !decision.IsApproved() {
			reason := decision.Reason
			if reason == "" {
				reason = "no reason given"
			}
			return errResult(fmt.Sprintf("Denied by human: %s", reason))
		}
	}

	// Pre-call hooks run last, right before execution — any error blocks the tool
	// (the model is told, mirroring the Rust engine's "blocked by hook" result).
	for _, h := range a.options.Hooks {
		if err := h.PreCall(ctx, tc); err != nil {
			return errResult(fmt.Sprintf("blocked by hook: %v", err))
		}
	}

	out, err := tool.Execute(ctx, args)
	// Wrap the outcome so PostCall hooks can redact Content in place before it
	// reaches the model. A tool error becomes an IsError result the hooks still see.
	result := ToolResult{ToolCallID: tc.ID, Content: out}
	if err != nil {
		// Surface tool failures to the model, don't crash the turn.
		result.Content = fmt.Sprintf("error: tool '%s' failed: %v", tc.Name, err)
		result.IsError = true
	}
	// Post-call hooks may redact result.Content in place. A hook error is ignored —
	// the (possibly redacted) result still reaches the model (Rust parity).
	for _, h := range a.options.Hooks {
		_ = h.PostCall(ctx, tc, &result)
	}
	return result
}
