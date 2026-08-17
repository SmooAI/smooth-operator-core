"""Ports the Rust reference engine's PromptCache tests (conversation.rs)."""

from __future__ import annotations

from smooth_operator_core.prompt_cache import PROMPT_CACHE_BOUNDARY, PromptCache


def test_prompt_cache_splits_at_boundary() -> None:
    c = PromptCache(f"static rules here{PROMPT_CACHE_BOUNDARY}dynamic context here")
    assert c.static_portion == "static rules here"
    assert c.dynamic_portion == "dynamic context here"


def test_prompt_cache_no_marker_treats_all_as_dynamic() -> None:
    prompt = "no marker in this prompt"
    c = PromptCache(prompt)
    assert c.static_portion == ""
    assert c.dynamic_portion == prompt


def test_full_prompt_combines_static_boundary_dynamic() -> None:
    prompt = f"You are an assistant.{PROMPT_CACHE_BOUNDARY}Project: Smooth"
    assert PromptCache(prompt).full_prompt() == prompt


def test_full_prompt_round_trips_unsplit_prompt() -> None:
    assert PromptCache("all dynamic").full_prompt() == "all dynamic"


def test_update_dynamic_only_changes_dynamic_portion() -> None:
    c = PromptCache(f"static{PROMPT_CACHE_BOUNDARY}old dynamic")
    original = c.static_hash()

    c.update_dynamic("new dynamic")

    assert c.dynamic_portion == "new dynamic"
    assert c.static_portion == "static"
    assert c.static_hash() == original, "static hash should not change"


def test_static_hash_is_deterministic() -> None:
    prompt = f"same static{PROMPT_CACHE_BOUNDARY}dynamic"
    assert PromptCache(prompt).static_hash() == PromptCache(prompt).static_hash()


def test_static_hash_changes_when_static_changes() -> None:
    a = PromptCache(f"static A{PROMPT_CACHE_BOUNDARY}dynamic")
    b = PromptCache(f"static B{PROMPT_CACHE_BOUNDARY}dynamic")
    assert a.static_hash() != b.static_hash()


def test_cached_tokens_returns_static_token_estimate() -> None:
    # "static text" is 11 chars => 11/4 + 1 = 3
    assert PromptCache(f"static text{PROMPT_CACHE_BOUNDARY}dynamic").cached_tokens() == 11 // 4 + 1
    # No marker => empty static => 0 tokens.
    assert PromptCache("all dynamic").cached_tokens() == 0
