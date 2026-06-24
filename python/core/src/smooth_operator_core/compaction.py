"""Token-aware conversation compaction (sliding window).

Phase-1 sibling of the reference engines' compaction. When a conversation's
estimated token count exceeds a budget, drop the oldest non-system messages
(keeping the system prompt and the most recent turns) so the next model call
stays within context. A coarse char/4 token estimate is used — enough to bound
context growth without a tokenizer dependency.

Safety: the kept window never *starts* on a `tool` message (which would orphan a
tool result whose preceding `assistant` tool_call was trimmed) — leading tool
messages are dropped until the window starts on a user/assistant turn.
"""

from __future__ import annotations

from typing import Any

# Rough chars-per-token; deliberately conservative so we compact a little early.
_CHARS_PER_TOKEN = 4


def estimate_tokens(message: dict[str, Any]) -> int:
    """Coarse token estimate for one OpenAI-format message."""
    content = message.get("content")
    text = content if isinstance(content, str) else ""
    # Tool calls carry JSON arguments that also cost tokens.
    for tc in message.get("tool_calls", []) or []:
        fn = tc.get("function", {}) if isinstance(tc, dict) else {}
        text += str(fn.get("name", "")) + str(fn.get("arguments", ""))
    return max(1, (len(text) + _CHARS_PER_TOKEN - 1) // _CHARS_PER_TOKEN)


def compact(messages: list[dict[str, Any]], max_tokens: int) -> list[dict[str, Any]]:
    """Return `messages` trimmed to roughly `max_tokens`, preserving system
    messages and the most recent turns. Returns the input unchanged when already
    within budget or when `max_tokens` is non-positive (disabled).
    """
    if max_tokens <= 0:
        return messages

    system = [m for m in messages if m.get("role") == "system"]
    rest = [m for m in messages if m.get("role") != "system"]

    system_tokens = sum(estimate_tokens(m) for m in system)
    budget = max_tokens - system_tokens

    total = system_tokens + sum(estimate_tokens(m) for m in rest)
    if total <= max_tokens:
        return messages

    # Keep a suffix of `rest` (most recent) that fits in the remaining budget.
    kept: list[dict[str, Any]] = []
    running = 0
    for m in reversed(rest):
        t = estimate_tokens(m)
        if running + t > budget and kept:
            break
        kept.append(m)
        running += t
    kept.reverse()

    # Never start the kept window on an orphaned tool result.
    while kept and kept[0].get("role") == "tool":
        kept.pop(0)

    return system + kept
