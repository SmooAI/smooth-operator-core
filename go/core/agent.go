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

// ChatMessage is one message in the OpenAI-shaped conversation.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string // set on role=="tool" messages
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
}

// ChatResponse is the assistant's reply (content and/or tool calls).
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
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
// chunks. The channel is closed when the stream ends; any error is delivered via
// the returned error (for connect-time failures) or — for a mid-stream failure —
// stored and reported as documented by the implementation. Production wires this to
// the OpenAI `create(..., stream=True)` surface.
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
	Text      string           // StreamText
	Name      string           // StreamToolCall / StreamToolResult
	Arguments string           // StreamToolCall
	Result    string           // StreamToolResult
	Response  AgentRunResponse // StreamDone
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
	Instructions  string
	Model         string
	MaxIterations int
	MaxTokens     int
	Temperature   float64
	Knowledge     Knowledge
	KnowledgeTopK int
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
	defaultModel            = "claude-haiku-4-5"
	defaultMaxIterations    = 8
	defaultMaxTokens        = 512
	defaultKnowledgeTopK    = 4
	defaultMaxContextTokens = 8000
)

// SmoothAgent is a native, in-process agent.
type SmoothAgent struct {
	client      ChatClient
	options     AgentOptions
	toolsByName map[string]Tool
}

// NewSmoothAgent constructs an agent over an OpenAI-compatible ChatClient.
func NewSmoothAgent(client ChatClient, options AgentOptions) *SmoothAgent {
	if client == nil {
		panic("core: client is required")
	}
	byName := make(map[string]Tool, len(options.Tools))
	for _, t := range options.Tools {
		byName[t.Name()] = t
	}
	return &SmoothAgent{client: client, options: options, toolsByName: byName}
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
	messages = append(messages, ChatMessage{Role: "user", Content: message})

	// Track this turn's new messages (user + assistant + tool, never system) so they
	// can be appended back to the thread on exit. Slicing the live messages by index
	// would be unsafe — compaction may drop/reorder it mid-turn.
	turnMessages := []ChatMessage{{Role: "user", Content: message}}

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
		})
		if err != nil {
			return AgentRunResponse{}, fmt.Errorf("model call: %w", err)
		}
		tracker.Record(model, resp.Usage, a.options.Pricing)
		lastText = resp.Content

		assistantMsg := ChatMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		messages = append(messages, assistantMsg)
		turnMessages = append(turnMessages, assistantMsg)

		// Stop early if this turn has hit its token/cost budget.
		if tracker.Exceeds(a.options.Budget) {
			return AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD, BudgetExceeded: true}, nil
		}

		if len(resp.ToolCalls) == 0 {
			return AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}, nil
		}

		toolCalls += len(resp.ToolCalls)
		// Dispatch the tool calls — concurrently when enabled and there's more than
		// one — but always append the results in the original ToolCalls order so the
		// transcript stays deterministic. dispatchTool turns failures/denials into a
		// result string, so a panicking sibling can't abort the others.
		results := make([]string, len(resp.ToolCalls))
		if a.options.ParallelToolCalls && len(resp.ToolCalls) > 1 {
			var wg sync.WaitGroup
			for i, tc := range resp.ToolCalls {
				wg.Add(1)
				go func(i int, tc ToolCall) {
					defer wg.Done()
					results[i] = a.dispatchTool(ctx, tc, search)
				}(i, tc)
			}
			wg.Wait()
		} else {
			for i, tc := range resp.ToolCalls {
				results[i] = a.dispatchTool(ctx, tc, search)
			}
		}
		for i, tc := range resp.ToolCalls {
			toolMsg := ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: results[i]}
			messages = append(messages, toolMsg)
			turnMessages = append(turnMessages, toolMsg)
		}
	}

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
// NOTE: retry-with-backoff (MaxRetries/RetryBackoff) is intentionally NOT applied to
// the streaming model call — re-running it after a mid-stream failure would re-emit
// already-yielded chunks. Retry stays scoped to non-streaming Run (see callModel);
// this mirrors the C# RunStreamingAsync decision.
func (a *SmoothAgent) RunStream(ctx context.Context, message string, thread *SmoothAgentThread) (*Stream, error) {
	sc, ok := a.client.(StreamingChatClient)
	if !ok {
		return nil, fmt.Errorf("core: client does not implement StreamingChatClient (no ChatStream)")
	}

	events := make(chan StreamEvent)
	stream := &Stream{events: events}
	go func() {
		defer close(events)
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
	mu     sync.Mutex
	err    error
}

// Events returns the channel of streamed events. It is closed when the turn ends.
func (s *Stream) Events() <-chan StreamEvent { return s.events }

// Err returns the error that aborted the turn, or nil if it completed cleanly.
// Call it only after Events() has been fully drained (the channel closed).
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (a *SmoothAgent) runStream(ctx context.Context, sc StreamingChatClient, message string, thread *SmoothAgentThread, out chan<- StreamEvent) error {
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
	messages = append(messages, ChatMessage{Role: "user", Content: message})

	turnMessages := []ChatMessage{{Role: "user", Content: message}}
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
		})
		if err != nil {
			return fmt.Errorf("model stream: %w", err)
		}
		var content strings.Builder
		partials := map[int]*ToolCall{}
		var order []int
		var usage Usage
		for chunk := range chunks {
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			if chunk.ContentDelta != "" {
				content.WriteString(chunk.ContentDelta)
				out <- StreamEvent{Kind: StreamText, Text: chunk.ContentDelta}
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

		tracker.Record(model, usage, a.options.Pricing)
		lastText = content.String()

		assistantMsg := ChatMessage{Role: "assistant", Content: lastText, ToolCalls: assembled}
		messages = append(messages, assistantMsg)
		turnMessages = append(turnMessages, assistantMsg)

		if tracker.Exceeds(a.options.Budget) {
			out <- StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD, BudgetExceeded: true}}
			return nil
		}

		if len(assembled) == 0 {
			out <- StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: iteration, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}}
			return nil
		}

		toolCalls += len(assembled)
		// Emit a tool_call event per requested call (original order) BEFORE dispatch.
		for _, tc := range assembled {
			out <- StreamEvent{Kind: StreamToolCall, Name: tc.Name, Arguments: tc.Arguments}
		}
		// Reuse the SAME dispatch path as Run (clearance, human-gate, tool_search,
		// JSON parsing, error-to-string, ParallelToolCalls). Results surface in
		// original call order so the event stream stays deterministic.
		results := make([]string, len(assembled))
		if a.options.ParallelToolCalls && len(assembled) > 1 {
			var wg sync.WaitGroup
			for i, tc := range assembled {
				wg.Add(1)
				go func(i int, tc ToolCall) {
					defer wg.Done()
					results[i] = a.dispatchTool(ctx, tc, search)
				}(i, tc)
			}
			wg.Wait()
		} else {
			for i, tc := range assembled {
				results[i] = a.dispatchTool(ctx, tc, search)
			}
		}
		for i, tc := range assembled {
			toolMsg := ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: results[i]}
			messages = append(messages, toolMsg)
			turnMessages = append(turnMessages, toolMsg)
			out <- StreamEvent{Kind: StreamToolResult, Name: tc.Name, Result: results[i]}
		}
	}

	out <- StreamEvent{Kind: StreamDone, Response: AgentRunResponse{Text: lastText, Iterations: maxIter, ToolCalls: toolCalls, Usage: tracker.Usage, CostUSD: tracker.CostUSD}}
	return nil
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
	// Enforce the role's tool clearance before dispatch: a forbidden tool is never
	// executed — the model is told it isn't permitted, mirroring how the loop
	// surfaces other tool errors.
	if a.options.Clearance != nil && !a.options.Clearance.IsAllowed(tc.Name) {
		return fmt.Sprintf("error: tool '%s' is not permitted for this role", tc.Name)
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
		return fmt.Sprintf("error: unknown tool '%s'", tc.Name)
	}
	args := map[string]any{}
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: tool '%s' received invalid JSON arguments", tc.Name)
		}
	}

	// Human-in-the-loop: pause for approval before running a flagged (write/sensitive)
	// tool. A denial is fed back to the model as a result — the tool never runs.
	if a.options.HumanGate != nil && a.options.RequiresApproval != nil && a.options.RequiresApproval(tc.Name, args) {
		req := HumanApprovalRequest{ToolName: tc.Name, Arguments: args, Prompt: fmt.Sprintf("Approve calling tool '%s'?", tc.Name)}
		decision, err := a.options.HumanGate(ctx, req)
		if err != nil {
			return fmt.Sprintf("error: human gate for tool '%s' failed: %v", tc.Name, err)
		}
		if !decision.IsApproved() {
			reason := decision.Reason
			if reason == "" {
				reason = "no reason given"
			}
			return fmt.Sprintf("Denied by human: %s", reason)
		}
	}

	out, err := tool.Execute(ctx, args)
	if err != nil {
		// Surface tool failures to the model, don't crash the turn.
		return fmt.Sprintf("error: tool '%s' failed: %v", tc.Name, err)
	}
	return out
}
