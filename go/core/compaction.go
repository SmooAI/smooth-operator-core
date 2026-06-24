package core

// Token-aware conversation compaction (sliding window).
//
// Phase-1 sibling of the reference engines' compaction. When a conversation's
// estimated token count exceeds a budget, drop the oldest non-system messages
// (keeping the system prompt and most recent turns) so the next model call stays
// within context. A coarse char/4 token estimate is used.
//
// Safety: the kept window never *starts* on a "tool" message (which would orphan
// a tool result whose preceding assistant tool_call was trimmed).

const charsPerToken = 4

func estimateTokens(m ChatMessage) int {
	text := m.Content
	for _, tc := range m.ToolCalls {
		text += tc.Name + tc.Arguments
	}
	t := (len(text) + charsPerToken - 1) / charsPerToken
	if t < 1 {
		return 1
	}
	return t
}

// compact returns messages trimmed to roughly maxTokens, preserving system
// messages and the most recent turns. Returns the input unchanged when already
// within budget or when maxTokens is non-positive (disabled).
func compact(messages []ChatMessage, maxTokens int) []ChatMessage {
	if maxTokens <= 0 {
		return messages
	}

	var system, rest []ChatMessage
	for _, m := range messages {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}

	systemTokens := 0
	for _, m := range system {
		systemTokens += estimateTokens(m)
	}
	total := systemTokens
	for _, m := range rest {
		total += estimateTokens(m)
	}
	if total <= maxTokens {
		return messages
	}

	budget := maxTokens - systemTokens
	// Keep a suffix of rest (most recent) that fits.
	start := len(rest)
	running := 0
	for i := len(rest) - 1; i >= 0; i-- {
		t := estimateTokens(rest[i])
		if running+t > budget && start < len(rest) {
			break
		}
		start = i
		running += t
	}
	kept := rest[start:]

	// Never start the kept window on an orphaned tool result.
	for len(kept) > 0 && kept[0].Role == "tool" {
		kept = kept[1:]
	}

	out := make([]ChatMessage, 0, len(system)+len(kept))
	out = append(out, system...)
	out = append(out, kept...)
	return out
}
