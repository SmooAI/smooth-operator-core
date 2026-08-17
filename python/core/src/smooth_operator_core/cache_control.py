"""Anthropic prompt-cache markers on the outbound request.

The Python port of the Rust reference's ``supports_anthropic_cache_control`` +
``apply_cache_control`` (llm.rs), and the wire half of
:class:`~.prompt_cache.PromptCache`.

Kept as a standalone module rather than inline in ``agent.py`` so the agent's
request path only needs a single call — the marking rules live here.
"""

from __future__ import annotations

from typing import Any

EPHEMERAL: dict[str, str] = {"type": "ephemeral"}
"""Anthropic's default 5-minute TTL marker."""


def supports_anthropic_cache_control(model: str | None, api_base_url: str | None) -> bool:
    """Does the configured upstream understand Anthropic-shaped ``cache_control``?

    True when the model id looks Claude-ish, or is one of the known semantic gateway
    aliases that route to Claude, AND the api base looks like a LiteLLM-style gateway
    or ``anthropic.*`` directly.

    We deliberately do NOT send these to bare OpenAI / Gemini / Groq endpoints — they
    400 on unknown extension fields. A LiteLLM gateway's
    ``cache_control_injection_points`` config is what actually forwards the markers to
    Anthropic; without that gateway-side change this is a no-op.
    """
    if not model or not api_base_url:
        return False
    m = model.lower()
    u = api_base_url.lower()
    looks_claude = "claude" in m or "sonnet" in m or "opus" in m or "haiku" in m
    # The generic ``smooth-`` prefix alone isn't enough — ``smooth-fast`` routes to a
    # Groq/Llama model, which would 400 on cache_control.
    is_claude_alias = m.startswith(("smooth-coding", "smooth-thinking", "smooth-planning", "smooth-reviewing"))
    url_is_gateway = "litellm" in u or "gateway" in u
    url_is_anthropic = "anthropic." in u
    return (looks_claude or is_claude_alias) and (url_is_gateway or url_is_anthropic)


def apply_cache_control(messages: list[dict[str, Any]], tools: list[dict[str, Any]] | None) -> None:
    """Attach ``cache_control: ephemeral`` to the strategic prefix boundaries, in place.

    1. The last system message — caches the system prompt.
    2. The last tool definition — caches the tool block + the system prefix ahead of it.
       Highest-ROI breakpoint: the tool registry is large and near-constant within a run.
    3. The last message in history — caches the running conversation, so each turn inside
       the 5-minute window pays only for the new delta.

    Marking a block caches THAT block plus everything before it, so only the last block
    of each prefix we want to reuse needs a marker.
    """
    # 1. Last system message.
    for msg in reversed(messages):
        if msg.get("role") == "system":
            msg["content"] = _wrap_with_cache_control(msg.get("content"))
            break

    # 2. Last tool — covers the whole tools array plus the system prefix.
    if tools:
        tools[-1]["cache_control"] = dict(EPHEMERAL)

    # 3. Last message, so turn-by-turn history caching extends. Skipped when the only
    #    message is the system we just marked (avoid double-marking it).
    if len(messages) > 1:
        messages[-1]["content"] = _wrap_with_cache_control(messages[-1].get("content"))


def _wrap_with_cache_control(content: Any) -> Any:
    """Rewrite string content into the single-text-block form carrying the marker.

    Empty/absent content (a tool-call-only assistant message) is returned untouched:
    there is nothing to cache on it, and the marker on the last block before the
    assistant turn already covers the prefix. Content already in list form — either
    re-marked blocks or OpenAI multimodal parts — is handled without flattening: for
    blocks the marker moves to the last one, and anything carrying a non-text part (an
    image) passes through unchanged, since flattening would silently drop the image and
    prompt caching only applies to text prefixes anyway.
    """
    if isinstance(content, str):
        if not content:
            return content
        return [{"type": "text", "text": content, "cache_control": dict(EPHEMERAL)}]
    if isinstance(content, list):
        parts: list[Any] = content
        if not parts:
            return content
        # Multimodal: leave images (and their sibling text parts) exactly as they are.
        if any(isinstance(p, dict) and p.get("type") not in (None, "text") for p in parts):
            return content
        blocks = [{k: v for k, v in p.items() if k != "cache_control"} if isinstance(p, dict) else p for p in parts]
        if isinstance(blocks[-1], dict):
            blocks[-1]["cache_control"] = dict(EPHEMERAL)
        return blocks
    return content
