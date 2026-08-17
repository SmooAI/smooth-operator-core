"""Multimodal image attachments on user messages (pearl th-25ce5c).

All the logic lives here; the agent's body-assembly sites call :func:`user_content`
once each, so this lands alongside the other workstreams on that same line.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class ImageContent:
    """An image attachment on a user message.

    ``url`` is a ``data:`` URL (``data:image/png;base64,...``) or a remote ``https``
    URL. ``detail`` (``"low"``/``"high"``/``"auto"``) is an optional OpenAI vision
    hint, omitted from the wire when unset.
    """

    url: str
    detail: str | None = None


def user_content(text: str, images: list[ImageContent] | None = None) -> str | list[dict[str, Any]]:
    """Build a user message's wire ``content``.

    Returns an OpenAI content-parts array when the turn carries images (text part
    first — omitted when the text is empty, since images may be sent alone — then
    one ``image_url`` part per image, in order), otherwise the plain string every
    turn has always sent.

    No images ⇒ the exact input string, so every text-only turn stays
    byte-identical to before this existed.

    The ``type`` discriminator on every part is load-bearing beyond this module:
    prompt caching (``cache_control.py``) decides whether it may wrap a message's
    content by scanning parts for a ``type`` that isn't ``"text"``. Drop it and that
    guard fails open — cache_control flattens the parts into a text block and the
    images vanish silently.
    """
    if not images:
        return text

    parts: list[dict[str, Any]] = []
    if text != "":
        parts.append({"type": "text", "text": text})
    for image in images:
        image_url: dict[str, Any] = {"url": image.url}
        if image.detail is not None:
            image_url["detail"] = image.detail
        parts.append({"type": "image_url", "image_url": image_url})
    return parts
