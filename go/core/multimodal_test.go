package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// Multimodal image attachments (pearl th-25ce5c), ported from the Rust
// reference. The load-bearing property is the NEGATIVE one: a turn without
// images must serialize byte-identically to before the field existed.

func marshalWire(t *testing.T, msgs []ChatMessage) string {
	t.Helper()
	raw, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m", Messages: msgs}, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestTextOnlyTurnIsByteIdenticalWithoutImages(t *testing.T) {
	got := marshalWire(t, []ChatMessage{{Role: "user", Content: "hello"}})
	if !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("text-only content must stay a plain string, got: %s", got)
	}
	if strings.Contains(got, "image_url") {
		t.Fatalf("text-only turn must not mention image_url: %s", got)
	}
}

func TestUserImagesEmitContentParts(t *testing.T) {
	got := marshalWire(t, []ChatMessage{{
		Role:    "user",
		Content: "what is this?",
		Images: []ImageContent{
			{URL: "data:image/png;base64,AAAA"},
			{URL: "https://x/y.jpg", Detail: "high"},
		},
	}})

	// Text part first, then one image_url part per image, in order.
	want := `"content":[{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},` +
		`{"type":"image_url","image_url":{"url":"https://x/y.jpg","detail":"high"}}]`
	if !strings.Contains(got, want) {
		t.Fatalf("multimodal wire shape mismatch.\nwant substring: %s\ngot: %s", want, got)
	}
	// `detail` is omitted, not null, when unset.
	if strings.Contains(got, `"detail":""`) || strings.Contains(got, `"detail":null`) {
		t.Fatalf("empty detail must be omitted: %s", got)
	}
}

func TestImagesAloneOmitTheTextPart(t *testing.T) {
	got := marshalWire(t, []ChatMessage{{
		Role:   "user",
		Images: []ImageContent{{URL: "data:image/png;base64,ZZZZ"}},
	}})
	if strings.Contains(got, `"type":"text"`) {
		t.Fatalf("empty text must not emit a text part: %s", got)
	}
	if !strings.Contains(got, `"type":"image_url"`) {
		t.Fatalf("image-only turn must still send the image: %s", got)
	}
}

func TestImagesOnlyAttachToUserMessages(t *testing.T) {
	// An assistant message carrying images (a host mistake) must not become a
	// parts array — only user turns are multimodal.
	got := marshalWire(t, []ChatMessage{{
		Role:    "assistant",
		Content: "sure",
		Images:  []ImageContent{{URL: "data:image/png;base64,AAAA"}},
	}})
	if strings.Contains(got, "image_url") {
		t.Fatalf("non-user message must not emit image parts: %s", got)
	}
}

func TestCacheControlPassesImagePartsThrough(t *testing.T) {
	// Flattening parts into a text block would silently drop the images.
	parts := []wireContentPart{
		{Type: "text", Text: "hi"},
		{Type: "image_url", ImageURL: &wireImageURL{URL: "data:image/png;base64,AAAA"}},
	}
	got, ok := wrapWithCacheControl(parts).([]wireContentPart)
	if !ok {
		t.Fatalf("image parts must pass through as parts, got %T", wrapWithCacheControl(parts))
	}
	if len(got) != 2 || got[1].ImageURL == nil {
		t.Fatalf("image part lost in cache wrap: %+v", got)
	}
}

func TestAgentAttachesNextUserImagesToTheTurn(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushText("saw it")
	agent := NewSmoothAgent(mock, AgentOptions{
		Model:          "m",
		NextUserImages: []ImageContent{{URL: "data:image/png;base64,AAAA"}},
	})

	if _, err := agent.Run(t.Context(), "what is this?", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	calls := mock.Calls()
	last := calls[len(calls)-1]
	var user *ChatMessage
	for i := range last.Messages {
		if last.Messages[i].Role == "user" {
			user = &last.Messages[i]
		}
	}
	if user == nil {
		t.Fatal("no user message reached the client")
	}
	if len(user.Images) != 1 || user.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Fatalf("NextUserImages must ride the turn's user message, got %+v", user.Images)
	}
}
