package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

// NewGatewayClient builds a client for the given base URL + key.
func NewGatewayClient(baseURL, apiKey string) *GatewayClient {
	return &GatewayClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
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
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream,omitempty"`
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

// buildWireRequest translates a ChatRequest into the OpenAI wire shape.
func buildWireRequest(req ChatRequest, stream bool) wireRequest {
	wreq := wireRequest{Model: req.Model, Temperature: req.Temperature, MaxTokens: req.MaxTokens, Stream: stream}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
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
	return wreq
}

// Chat implements ChatClient.
func (g *GatewayClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	wreq := buildWireRequest(req, false)

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
		Content: msg.Content,
		Usage:   Usage{PromptTokens: wresp.Usage.PromptTokens, CompletionTokens: wresp.Usage.CompletionTokens},
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
func (g *GatewayClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	wreq := buildWireRequest(req, true)
	body, err := json.Marshal(wreq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/chat/completions", bytes.NewReader(body))
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

	ch := make(chan ChatChunk)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
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
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
