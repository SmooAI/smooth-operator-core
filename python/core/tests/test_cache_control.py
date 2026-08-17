"""Ports the Rust reference engine's cache_control gate + request-body tests (llm.rs)."""

from __future__ import annotations

from typing import Any

from smooth_operator_core.agent import AgentOptions, SmoothAgent
from smooth_operator_core.cache_control import apply_cache_control, supports_anthropic_cache_control
from smooth_operator_core.llm_provider import MockLlmProvider


def test_gate_recognizes_claude_routes() -> None:
    # Claude model id + LiteLLM gateway url → cache it.
    assert supports_anthropic_cache_control("claude-sonnet-4-20250514", "https://litellm.example.com/v1")
    # Smooth-coding alias + gateway url → cache it.
    assert supports_anthropic_cache_control("smooth-coding-claude", "https://gateway.example.com/v1")
    # Direct Anthropic API + Claude id → cache it.
    assert supports_anthropic_cache_control("claude-opus-4", "https://api.anthropic.com/v1")
    # GPT model on OpenAI → no cache control (would 400).
    assert not supports_anthropic_cache_control("gpt-4o", "https://api.openai.com/v1")
    # Gemini-compat → no cache control.
    assert not supports_anthropic_cache_control("gemini-1.5-pro", "https://generativelanguage.googleapis.com")
    # Claude id but bare OpenAI url (mis-configured) — still gated off.
    assert not supports_anthropic_cache_control("claude-3-sonnet", "https://api.openai.com/v1")
    # smooth-fast routes to Groq/Llama via the gateway — must NOT be cached.
    assert not supports_anthropic_cache_control("smooth-fast", "https://gateway.example.com/v1")


def test_gate_off_without_base_url() -> None:
    assert not supports_anthropic_cache_control("claude-opus-4", None)


def test_marks_system_last_tool_and_last_message() -> None:
    messages: list[dict[str, Any]] = [
        {"role": "system", "content": "You are smooth."},
        {"role": "user", "content": "Hi"},
    ]
    tools: list[dict[str, Any]] = [
        {"type": "function", "function": {"name": "bash", "description": "Run a command", "parameters": {}}},
        {"type": "function", "function": {"name": "file_write", "description": "Write a file", "parameters": {}}},
    ]

    apply_cache_control(messages, tools)

    assert messages[0]["content"] == [
        {"type": "text", "text": "You are smooth.", "cache_control": {"type": "ephemeral"}}
    ]
    assert "cache_control" not in tools[0]
    assert tools[1]["cache_control"] == {"type": "ephemeral"}
    assert messages[1]["content"][0]["cache_control"] == {"type": "ephemeral"}


def test_empty_content_left_alone() -> None:
    # A tool-call-only assistant message has no prose to cache.
    messages: list[dict[str, Any]] = [
        {"role": "system", "content": "sys"},
        {"role": "assistant", "content": ""},
    ]
    apply_cache_control(messages, None)
    assert messages[1]["content"] == ""


def test_multimodal_content_passes_through() -> None:
    # Flattening would silently drop the image; caching only covers text prefixes.
    parts = [
        {"type": "text", "text": "look"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,ZZZZ"}},
    ]
    messages: list[dict[str, Any]] = [
        {"role": "system", "content": "sys"},
        {"role": "user", "content": parts},
    ]
    apply_cache_control(messages, None)
    assert messages[1]["content"] == parts


def test_remarks_only_the_last_block() -> None:
    messages: list[dict[str, Any]] = [
        {"role": "system", "content": "sys"},
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "first", "cache_control": {"type": "ephemeral"}},
                {"type": "text", "text": "second"},
            ],
        },
    ]
    apply_cache_control(messages, None)
    blocks = messages[1]["content"]
    assert "cache_control" not in blocks[0]
    assert blocks[1]["cache_control"] == {"type": "ephemeral"}


async def test_agent_sends_unmarked_body_without_base_url() -> None:
    provider = MockLlmProvider()
    provider.push_text("done")
    agent = SmoothAgent(provider, AgentOptions(instructions="You are smooth.", model="claude-opus-4"))

    await agent.run("Hi")

    sent = provider.calls[0]
    assert sent.messages[0]["content"] == "You are smooth."
    assert all("cache_control" not in m for m in sent.messages)


async def test_agent_marks_body_for_claude_routing_gateway() -> None:
    provider = MockLlmProvider()
    provider.push_text("done")
    # The seam GatewayLlmProvider populates from the SDK.
    provider.api_base_url = "https://gateway.example.com/v1"  # type: ignore[attr-defined]
    agent = SmoothAgent(provider, AgentOptions(instructions="You are smooth.", model="smooth-coding-claude"))

    await agent.run("Hi")

    sent = provider.calls[0]
    assert isinstance(sent.messages[0]["content"], list)
    assert sent.messages[0]["content"][0]["cache_control"] == {"type": "ephemeral"}
