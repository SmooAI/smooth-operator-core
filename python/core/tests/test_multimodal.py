"""Multimodal image attachments (pearl th-25ce5c), ported from the Rust reference.

The load-bearing property is the NEGATIVE one: a turn without images must be
byte-identical to before the field existed.
"""

from __future__ import annotations

import json

from smooth_operator_core.cache_control import apply_cache_control
from smooth_operator_core.multimodal import ImageContent, user_content


def test_no_images_returns_the_plain_string() -> None:
    assert user_content("hello") == "hello"
    assert user_content("hello", []) == "hello"


def test_text_part_then_one_image_part_per_image_in_order() -> None:
    assert user_content(
        "what is this?",
        [ImageContent("data:image/png;base64,AAAA"), ImageContent("https://x/y.jpg", "high")],
    ) == [
        {"type": "text", "text": "what is this?"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
        {"type": "image_url", "image_url": {"url": "https://x/y.jpg", "detail": "high"}},
    ]


def test_images_alone_omit_the_text_part() -> None:
    assert user_content("", [ImageContent("data:image/png;base64,ZZZZ")]) == [
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,ZZZZ"}},
    ]


def test_detail_is_omitted_when_unset() -> None:
    parts = user_content("hi", [ImageContent("https://x/y.jpg")])
    assert isinstance(parts, list)
    assert list(parts[1]["image_url"].keys()) == ["url"]


def test_vision_turn_through_a_claude_route_still_carries_the_image() -> None:
    """cache_control marks the LAST message, which in a vision turn IS the
    image-bearing one. Flattening it into a text block drops the images silently."""
    messages = [
        {"role": "system", "content": "be helpful"},
        {
            "role": "user",
            "content": user_content("what is this?", [ImageContent("data:image/png;base64,AAAA")]),
        },
    ]
    apply_cache_control(messages, None)

    content = messages[1]["content"]
    assert isinstance(content, list)
    assert any(p["type"] == "image_url" for p in content)
    # Passed through untouched — no marker smuggled onto an image part.
    assert all("cache_control" not in p for p in content)


def test_text_only_turn_still_caches_on_the_same_route() -> None:
    messages = [
        {"role": "system", "content": "be helpful"},
        {"role": "user", "content": user_content("no images here")},
    ]
    apply_cache_control(messages, None)
    # Sanity: the guard is scoped to multimodal content, it didn't disable caching.
    assert "cache_control" in json.dumps(messages)
