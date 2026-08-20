package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GatewayClient is an OpenAI-compatible ChatClient over HTTP — the Go analogue
// of the other cores' OpenAI SDK client. Points at any /chat/completions
// endpoint (e.g. the SmooAI gateway) with a Bearer API key.
//
// (Phase 0 uses net/http directly, mirroring how the sibling cores' OpenAI SDKs
// do their own HTTP. Adopting @smooai/fetch's Go client is a tracked follow-up.)
type GatewayClient struct {
	BaseURL    string // e.g. https://llm.smoo.ai/v1
	APIKey     string
	HTTPClient *http.Client

	// idleTimeout overrides chunkIdleTimeout on the streaming path. Unexported and
	// zero by default — it exists so tests can exercise the watchdog without
	// waiting a minute, not as a knob to tune per call site.
	idleTimeout time.Duration
}

// requestTimeout is the OVERALL deadline for one model call, mirroring the Rust
// reference's `.timeout(600s)` on its reqwest client (llm.rs). It is deliberately
// generous: Go's http.Client.Timeout covers reading the response BODY, so on the
// streaming path it is a total-duration ceiling on the whole SSE stream, not a
// "server went silent" detector. The 60s value this replaced silently guillotined
// any reasoning/multi-tool turn that ran longer.
//
// Liveness is enforced separately by chunkIdleTimeout in ChatStream — that is the
// timeout that should normally fire, and it fires as an ERROR.
//
// Connect time is bounded by http.DefaultTransport's 30s dialer, matching the
// reference's `.connect_timeout(30s)`.
const requestTimeout = 600 * time.Second

// chunkIdleTimeout aborts a stream that has gone silent — no bytes for this long.
// Mirrors the Rust reference's CHUNK_IDLE_TIMEOUT (llm.rs). It measures the gap
// between reads, so a legitimately slow-but-progressing stream never trips it.
const chunkIdleTimeout = 60 * time.Second

// errStreamIdle is the cancellation cause the idle watchdog stamps on the stream
// context, so the read error it provokes can be reported as what it actually is.
var errStreamIdle = errors.New("stream idle timeout")

// NewGatewayClient builds a client for the given base URL + key.
func NewGatewayClient(baseURL, apiKey string) *GatewayClient {
	return &GatewayClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: requestTimeout},
	}
}

// gatewayCostHeaders are the response headers the LLM gateway reports per-request
// cost in, in PRECEDENCE order. Mirrors the Rust reference's parseGatewayCost
// candidate list exactly (rust/smooth-operator-core/src/llm.rs).
//
// LiteLLM splits cost across a few headers: `-margin-amount` is what the caller
// actually pays (includes the gateway's configured markup), `-original` is the raw
// upstream cost, and the bare `x-litellm-response-cost` is the legacy shape older
// versions emit. The last two are generic fallbacks for other OpenAI-compatible
// gateways that echo a cost header.
var gatewayCostHeaders = []string{
	"x-litellm-response-cost-margin-amount",
	"x-litellm-response-cost-original",
	"x-litellm-response-cost",
	"x-response-cost",
	"x-cost-usd",
}

// parseGatewayCost reads the gateway's authoritative per-request cost from a
// response's headers, taking the FIRST NON-ZERO candidate.
//
// Returns nil when every candidate is absent OR reports zero. That distinction is
// the whole point: nil means "unmeasured", so the caller falls back to local
// ModelPricing, whereas locking in 0 would pin cost at zero for the rest of the
// dispatch. LiteLLM's config on llm.smoo.ai currently reports 0 for `smooth-*`
// aliases on every response, so taking a zero at face value silently zeroes real
// spend.
func parseGatewayCost(h http.Header) *float64 {
	for _, name := range gatewayCostHeaders {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		cost, err := strconv.ParseFloat(v, 64)
		if err != nil || cost <= 0 {
			continue
		}
		return &cost
	}
	return nil
}

// ── wire shapes (OpenAI /chat/completions) ──────────────────────────────────

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Either a plain string (the default every OpenAI-compat upstream accepts)
	// or a []wireTextBlock when Anthropic prompt caching marks this message.
	// Rust models this as an untagged enum; in Go `any` marshals both shapes,
	// and a string in an `any` field is byte-identical to a string field, so
	// the wire is unchanged whenever caching is off.
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireContentPart is one part of an OpenAI multimodal `content` array. It
// marshals as `{"type":"text","text":...}` or
// `{"type":"image_url","image_url":{...}}` — the standard shape every model we
// route vision to (gemini-flash, gpt-4o, mimo-vl) speaks. Mirrors Rust's
// `ContentPart`. Pearl th-25ce5c.
type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

// wireImageURL is the `image_url` object inside an image content part. URL is a
// `data:`/`https` URL; Detail is the optional OpenAI vision hint.
type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// messageContent returns a message's wire `content`: an OpenAI content-parts
// array when a USER message carries images (text part first — omitted when the
// text is empty, since images may be sent alone — then one image_url part per
// image), otherwise the plain string every turn has always sent. Text-only
// turns are byte-identical to before images existed.
func messageContent(m ChatMessage) any {
	if m.Role != "user" || len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]wireContentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, wireContentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, wireContentPart{
			Type:     "image_url",
			ImageURL: &wireImageURL{URL: img.URL, Detail: img.Detail},
		})
	}
	return parts
}

// wireCacheControl is the Anthropic-shaped marker. `{"type":"ephemeral"}` is
// the default 5-minute TTL — what the agent loop wants, since system + tools +
// recent history get reused turn after turn within one dispatch.
type wireCacheControl struct {
	Type string `json:"type"`
}

func ephemeralCacheControl() *wireCacheControl { return &wireCacheControl{Type: "ephemeral"} }

// wireTextBlock is one block of an Anthropic-style content array. A
// cache_control on a block caches THAT block plus everything before it.
type wireTextBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
	// Set on the LAST tool only: Anthropic then caches the whole tool-definitions
	// block plus the system prefix ahead of it. Highest-ROI breakpoint, since the
	// tool registry is large and near-constant within a run.
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream,omitempty"`
	// Top-level OpenAI-compat `metadata` object — LiteLLM records it on spend
	// logs. omitempty keeps the wire byte-identical when unset (Rust parity).
	Metadata map[string]any `json:"metadata,omitempty"`
	// Structured output. Pointer + omitempty so an unset format leaves the
	// request byte-identical to before (Rust parity).
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
	// The OpenAI streaming API OMITS usage unless it is explicitly requested; the
	// missing usage chunk was never the gateway losing data, it was the gateway
	// honouring a request that never asked. Verified against llm.smoo.ai (LiteLLM
	// 1.95.0): 0 chunks carry usage without this, 1 carries real prompt/completion
	// counts with it. Only sent when streaming — it is meaningless otherwise, and
	// omitempty keeps a non-streaming request byte-identical. Pearl th-5e59a5.
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
}

// wireStreamOptions carries `stream_options.include_usage`, which makes the gateway
// emit a final usage-only chunk. See wireRequest.StreamOptions.
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireResponseFormat is the OpenAI-compatible `response_format` object:
// `{"type":"json_schema","json_schema":{name,schema,strict}}`.
type wireResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

// wireStreamChunk is one OpenAI streaming chunk (`data: {...}` SSE payload).
type wireStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// supportsAnthropicCacheControl decides whether the configured upstream
// understands Anthropic-shaped `cache_control` markers. We send them when the
// model id looks Claude-ish, or is one of the known semantic gateway aliases
// that route to Claude, AND the api base looks like a LiteLLM-style gateway or
// anthropic.* directly.
//
// We deliberately do NOT send cache_control to bare OpenAI / Gemini / Groq
// endpoints — they 400 on unknown extension fields. A LiteLLM gateway's
// `cache_control_injection_points` config is what actually passes the markers
// through to Anthropic; without that gateway-side change this is a no-op.
func supportsAnthropicCacheControl(model, apiURL string) bool {
	m := strings.ToLower(model)
	u := strings.ToLower(apiURL)
	looksClaude := strings.Contains(m, "claude") || strings.Contains(m, "sonnet") || strings.Contains(m, "opus") || strings.Contains(m, "haiku")
	// The generic `smooth-` prefix alone isn't enough — `smooth-fast` routes to
	// a Groq/Llama model, which would 400 on cache_control.
	isClaudeAlias := strings.HasPrefix(m, "smooth-coding") ||
		strings.HasPrefix(m, "smooth-thinking") ||
		strings.HasPrefix(m, "smooth-planning") ||
		strings.HasPrefix(m, "smooth-reviewing")
	urlIsGateway := strings.Contains(u, "litellm") || strings.Contains(u, "gateway")
	urlIsAnthropic := strings.Contains(u, "anthropic.")
	return (looksClaude || isClaudeAlias) && (urlIsGateway || urlIsAnthropic)
}

// applyCacheControl attaches `cache_control: ephemeral` to the strategic prefix
// boundaries so Anthropic's prompt cache covers what changes least:
//
//  1. The (last) system message — caches the system prompt.
//  2. The last tool definition — caches the tool block + system prefix ahead of it.
//  3. The last message in history — caches the running conversation, so each turn
//     within the 5-minute window pays only for the new delta.
//
// Marking a block caches THAT block plus everything before it, so only the last
// block of each prefix we want to reuse needs a marker.
func applyCacheControl(messages []wireMessage, tools []wireTool) {
	// 1. Last system message.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "system" {
			messages[i].Content = wrapWithCacheControl(messages[i].Content)
			break
		}
	}

	// 2. Last tool — covers the whole tools array plus the system prefix.
	if len(tools) > 0 {
		tools[len(tools)-1].CacheControl = ephemeralCacheControl()
	}

	// 3. Last message, so turn-by-turn history caching extends. Skipped when the
	//    only message is the system we just marked (avoid double-marking it).
	if len(messages) > 1 {
		last := len(messages) - 1
		messages[last].Content = wrapWithCacheControl(messages[last].Content)
	}
}

// wrapWithCacheControl rewrites string content into the single-text-block form
// carrying `cache_control: ephemeral`. Empty content (a tool-call-only assistant
// message) is left alone: there's nothing to cache on it, and the marker on the
// last block before the assistant turn already covers the prefix. Content that
// is already in block form is re-marked on its last block.
//
// Multimodal content-parts arrays pass through UNTOUCHED: flattening them into
// a text block would silently drop the images, and prompt caching only applies
// to text prefixes anyway. Mirrors Rust's `wrap_with_cache_control`.
func wrapWithCacheControl(existing any) any {
	switch c := existing.(type) {
	case []wireContentPart:
		return c
	case string:
		if c == "" {
			return c
		}
		return []wireTextBlock{{Type: "text", Text: c, CacheControl: ephemeralCacheControl()}}
	case []wireTextBlock:
		blocks := make([]wireTextBlock, len(c))
		for i, b := range c {
			blocks[i] = wireTextBlock{Type: b.Type, Text: b.Text}
		}
		if len(blocks) > 0 {
			blocks[len(blocks)-1].CacheControl = ephemeralCacheControl()
		}
		return blocks
	default:
		return existing
	}
}

// buildWireRequest translates a ChatRequest into the OpenAI wire shape.
//
// apiURL is optional and only used to gate Anthropic prompt-cache markers: with
// no URL (or an upstream that doesn't support them) the request is byte-identical
// to what it was before caching existed.
func buildWireRequest(req ChatRequest, stream bool, apiURL ...string) wireRequest {
	wreq := wireRequest{Model: req.Model, Temperature: req.Temperature, MaxTokens: req.MaxTokens, Stream: stream, Metadata: normalizeMetadata(req.Metadata)}
	if stream {
		wreq.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: messageContent(m), ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var w wireToolCall
			w.ID = tc.ID
			w.Type = "function"
			w.Function.Name = tc.Name
			w.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, w)
		}
		wreq.Messages = append(wreq.Messages, wm)
	}
	for _, t := range req.Tools {
		var w wireTool
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.Parameters
		wreq.Tools = append(wreq.Tools, w)
	}
	if f := req.ResponseFormat; f != nil {
		wreq.ResponseFormat = &wireResponseFormat{
			Type:       "json_schema",
			JSONSchema: wireJSONSchema{Name: f.Name, Schema: f.Schema, Strict: f.Strict},
		}
	}
	if len(apiURL) > 0 && supportsAnthropicCacheControl(req.Model, apiURL[0]) {
		applyCacheControl(wreq.Messages, wreq.Tools)
	}
	return wreq
}

// Chat implements ChatClient.
func (g *GatewayClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	wreq := buildWireRequest(req, false, g.BaseURL)

	body, err := json.Marshal(wreq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.APIKey)

	resp, err := g.HTTPClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, fmt.Errorf("gateway %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var wresp wireResponse
	if err := json.Unmarshal(respBody, &wresp); err != nil {
		return ChatResponse{}, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(wresp.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("gateway returned no choices")
	}
	msg := wresp.Choices[0].Message
	out := ChatResponse{
		Content:        msg.Content,
		Usage:          Usage{PromptTokens: wresp.Usage.PromptTokens, CompletionTokens: wresp.Usage.CompletionTokens},
		GatewayCostUSD: parseGatewayCost(resp.Header),
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out, nil
}

// ChatStream implements StreamingChatClient: it opens an SSE streaming completion
// and translates each `data: {...}` line into a ChatChunk on the returned channel.
// Connect-time failures (request build / HTTP / non-2xx) come back as the error;
// the channel is closed when the SSE stream ends (`data: [DONE]` or EOF).
//
// A stream that ends any way OTHER than cleanly — idle timeout, the overall
// requestTimeout expiring, a mid-stream connection reset, an over-long SSE line —
// delivers a final ChatChunk carrying Err before the channel closes. It NEVER
// closes as if the model had finished: a truncated stream is an error, because a
// half-sentence that looks like a clean completion gets appended to history and
// checkpointed as the assistant turn with nothing recording the loss.
//
// Cancelling ctx tears down the request and releases the socket; the goroutine
// always exits, so an abandoning consumer leaks neither it nor the connection.
func (g *GatewayClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	wreq := buildWireRequest(req, true, g.BaseURL)
	body, err := json.Marshal(wreq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// The idle watchdog needs to be able to abort the in-flight body read, which
	// means cancelling the request's OWN context. WithCancelCause lets the
	// resulting read error be reported as the idle timeout it really is rather
	// than a bare "context canceled".
	streamCtx, cancelStream := context.WithCancelCause(ctx)
	streaming := false
	defer func() {
		// Every early return below abandons the request; only the goroutine path
		// takes ownership of cancelStream.
		if !streaming {
			cancelStream(context.Canceled)
		}
	}()
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, g.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := g.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("gateway %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Capture the cost off the response BEFORE handing the body to the scanner.
	// Not because consuming the body destroys the headers — Go's resp.Header is a
	// plain map and stays readable afterward. The real hazard core#102 fixed is
	// narrower: a streaming API hands you only a stream unless you deliberately
	// keep hold of the response, so the mistake is never capturing it at all
	// (Rust's bytes_stream() literally consumes the Response, which is why the
	// reference orders it this way). Capturing up front makes that impossible.
	streamCost := parseGatewayCost(resp.Header)

	ch := make(chan ChatChunk)
	streaming = true
	go func() {
		defer close(ch)
		defer cancelStream(context.Canceled)
		defer resp.Body.Close()

		// send blocks until the consumer takes the chunk OR the stream context is
		// done. The bare `ch <-` this replaced wedged the goroutine forever (still
		// holding the socket) whenever a consumer broke out of the range early.
		// Mirrors the reference's `tx.send(..).is_err() -> return`.
		send := func(c ChatChunk) bool {
			select {
			case ch <- c:
				return true
			case <-streamCtx.Done():
				return false
			}
		}

		// Cost rides the first chunk so the consumer folds it even if the stream
		// errors before any usage arrives.
		if streamCost != nil && !send(ChatChunk{CostUSD: streamCost}) {
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		// 8MB line ceiling: a single SSE `data:` line carrying a large structured
		// output or tool-call argument blob can exceed the old 1MB. Overrunning it
		// is now at least loud (bufio.ErrTooLong via scanner.Err) instead of a
		// silent truncation, but the ceiling should not be hit in normal use.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

		// Idle watchdog: cancelling streamCtx aborts the blocked body read, which
		// surfaces below as scanner.Err. Reset immediately before each Scan so the
		// deadline covers only time spent waiting on the SERVER — a consumer that
		// is slow to take chunks must not be mistaken for a stalled stream.
		idleAfter := g.idleTimeout
		if idleAfter <= 0 {
			idleAfter = chunkIdleTimeout
		}
		idle := time.AfterFunc(idleAfter, func() { cancelStream(errStreamIdle) })
		defer idle.Stop()

		for {
			// Arm around the Scan ONLY. Leaving it armed through the send below
			// would put a slow CONSUMER on the same clock as a stalled server and
			// abort a perfectly healthy stream.
			idle.Reset(idleAfter)
			ok := scanner.Scan()
			idle.Stop()
			if !ok {
				break
			}
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				return
			}
			var wc wireStreamChunk
			if err := json.Unmarshal([]byte(data), &wc); err != nil {
				continue // skip malformed/keep-alive payloads
			}
			chunk := ChatChunk{}
			if len(wc.Choices) > 0 {
				d := wc.Choices[0].Delta
				chunk.ContentDelta = d.Content
				for _, tc := range d.ToolCalls {
					chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, ToolCallDelta{
						Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, ArgsFragment: tc.Function.Arguments,
					})
				}
			}
			if wc.Usage != nil {
				chunk.Usage = &Usage{PromptTokens: wc.Usage.PromptTokens, CompletionTokens: wc.Usage.CompletionTokens}
			}
			if !send(chunk) {
				break // abandoned, or the watchdog fired — classified below
			}
		}

		// ONE terminal check covering every way out of the loop except the clean
		// `[DONE]` above, which returns before reaching here. Not checking
		// scanner.Err is what made a torn body, a reset connection, an expired
		// requestTimeout and an over-long line all look like a clean finish.
		err := scanner.Err()
		// The watchdog reports a stall by cancelling streamCtx, so it can surface
		// either as a read error OR as a failed send (when it fires in the window
		// between a successful Scan and its send). Both are the same stall, and
		// classifying by cause catches both — an early version returned on the
		// failed send and silently truncated in exactly that race.
		if cause := context.Cause(streamCtx); errors.Is(cause, errStreamIdle) {
			err = fmt.Errorf("stream idle timeout: no data for %s", idleAfter)
		}
		if err != nil {
			// Deliberately NOT `send`: selecting on streamCtx here would race the
			// error away, since the watchdog signals by cancelling it. The CALLER's
			// ctx is the honest liveness signal — if that is done the consumer is
			// gone and there is nobody left to tell.
			select {
			case ch <- ChatChunk{Err: fmt.Errorf("stream read error: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}
