"""Prompt caching — the static/dynamic system-prompt split.

The Python port of the Rust reference engine's
``smooth_operator_core::conversation::PromptCache``.

A system prompt has two halves with very different churn rates: role instructions
and tool schemas barely change, while project context (AGENTS.md / CLAUDE.md, the
working set) changes every turn. Anthropic's prompt cache keys on a *prefix*, so
putting the volatile half first invalidates the whole thing.
:data:`PROMPT_CACHE_BOUNDARY` splits them: everything above the marker is static
and hashed once for cache-key dedup, everything below is dynamic and can be
swapped without busting the static prefix.

Feed the result to the agent as its instructions::

    cache = PromptCache(f"{rules}{PROMPT_CACHE_BOUNDARY}{project_context}")
    SmoothAgent(provider, AgentOptions(instructions=cache.full_prompt()))
"""

from __future__ import annotations

PROMPT_CACHE_BOUNDARY = "__PROMPT_CACHE_BOUNDARY__"
"""Marker splitting a system prompt into cacheable static and volatile dynamic halves."""

_FNV_OFFSET_64 = 14695981039346656037
_FNV_PRIME_64 = 1099511628211
_MASK_64 = (1 << 64) - 1


class PromptCache:
    """A system prompt split at :data:`PROMPT_CACHE_BOUNDARY`."""

    def __init__(self, prompt: str) -> None:
        """Split at the boundary marker.

        With no marker the entire prompt is treated as dynamic — nothing is claimed
        cacheable that the caller didn't mark.
        """
        idx = prompt.find(PROMPT_CACHE_BOUNDARY)
        if idx < 0:
            self.static_portion = ""
            self.dynamic_portion = prompt
        else:
            self.static_portion = prompt[:idx]
            self.dynamic_portion = prompt[idx + len(PROMPT_CACHE_BOUNDARY) :]

        self._static_hash = _hash_prompt_portion(self.static_portion)
        self._static_tokens = 0 if not self.static_portion else len(self.static_portion) // 4 + 1

    def full_prompt(self) -> str:
        """Reassemble static + boundary + dynamic.

        With no static portion the dynamic half is returned alone, so a prompt that
        was never split round-trips unchanged rather than gaining a stray marker.
        """
        if not self.static_portion:
            return self.dynamic_portion
        return f"{self.static_portion}{PROMPT_CACHE_BOUNDARY}{self.dynamic_portion}"

    def update_dynamic(self, dynamic: str) -> None:
        """Swap the dynamic half, leaving the static half and its hash untouched."""
        self.dynamic_portion = dynamic

    def static_hash(self) -> str:
        """Identify the static portion for cache-key deduplication.

        Process-local only: compared against other hashes from THIS engine, never
        sent on the wire, so it deliberately does not match the Rust reference's
        value (Rust uses ``DefaultHasher``, which is not reproducible across
        languages — or even across Rust releases). The ported contract is the
        behavior: same static text hashes the same, different static text hashes
        differently, and :meth:`update_dynamic` never changes it.
        """
        return self._static_hash

    def cached_tokens(self) -> int:
        """Estimated tokens the static portion saves on a cache hit."""
        return self._static_tokens


def _hash_prompt_portion(s: str) -> str:
    """FNV-1a (64-bit) over the UTF-8 bytes, rendered as 16 hex chars like Rust."""
    h = _FNV_OFFSET_64
    for b in s.encode("utf-8"):
        h = ((h ^ b) * _FNV_PRIME_64) & _MASK_64
    return f"{h:016x}"
