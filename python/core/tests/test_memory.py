"""Unit tests for long-term memory."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, InMemoryMemory, SmoothAgent
from smooth_operator_core.memory import (
    RECALL_FRESHNESS_NOTE,
    RECALL_HEADER,
    MemoryEntry,
    MemoryType,
    relevance_score,
    render_recall_block,
)


def test_remember_and_recall_by_overlap():
    mem = InMemoryMemory()
    mem.remember("The user's name is Dana.")
    mem.remember("The user prefers metric units.")
    mem.remember("Gift wrapping costs 4.99.")
    recalled = mem.recall("what units does the user prefer?", top_k=1)
    assert len(recalled) == 1
    assert "metric" in recalled[0].text


def test_recall_returns_nothing_on_no_overlap():
    mem = InMemoryMemory()
    mem.remember("The sky is blue.")
    assert mem.recall("quarterly revenue forecast", top_k=4) == []


def test_blank_memory_is_ignored():
    mem = InMemoryMemory()
    mem.remember("   ")
    assert mem.recall("anything", top_k=4) == []


def _resp(content):
    return SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content=content, tool_calls=None))], usage=None
    )


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)
        self.calls: list[list] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs["messages"])
        return self._scripted.pop(0)


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


@pytest.mark.asyncio
async def test_recalled_memory_is_injected_into_system_prompt():
    mem = InMemoryMemory()
    mem.remember("The user's name is Dana.")
    mem.remember("Unrelated trivia about penguins.")
    client = FakeClient([_resp("Hi Dana!")])
    agent = SmoothAgent(client, AgentOptions(instructions="support", memory=mem))
    await agent.run("do you remember my name?")
    system = client.chat.completions.calls[0][0]["content"]
    assert RECALL_HEADER in system
    assert "Dana" in system
    # The unrelated entry has no overlap with the query, so it is not recalled.
    assert "penguins" not in system


# ── cross-language recall contract (th-ffaeae) ───────────────────────────────
#
# The block below is reproduced byte-for-byte by the Rust reference and the C#,
# Go and TypeScript siblings. These tests mirror
# rust/smooth-operator-core/src/memory.rs so a drift shows up here, not months
# later when someone tries to write a shared conformance scenario.


def test_recall_block_is_pinned_across_languages():
    entry = MemoryEntry(text="brent prefers execution over questions", memory_type=MemoryType.USER, relevance=0.5)
    assert render_recall_block([entry]) == (
        "[Recalled memories]\n- (User, relevance=0.50): brent prefers execution over questions\n"
    )


def test_recall_block_adds_freshness_note_only_when_time_sensitive():
    """A Project/Reference memory names something in a moving codebase, so the
    model is told to verify it. A User/Feedback one describes the person and does
    not go stale — emitting the note there would train the model to skip it."""
    project = MemoryEntry(text="the retry lives in fetch.rs", memory_type=MemoryType.PROJECT, relevance=1.0)
    block = render_recall_block([project])
    assert block is not None
    assert block.startswith(f"{RECALL_HEADER}\n{RECALL_FRESHNESS_NOTE}\n")
    assert block.endswith("- (Project, relevance=1.00): the retry lives in fetch.rs\n")

    durable = MemoryEntry(text="prefers dark mode", memory_type=MemoryType.USER, relevance=1.0)
    assert "Note:" not in (render_recall_block([durable]) or "")


def test_empty_recall_renders_no_block():
    """A bare header would spend context telling the model it remembered nothing."""
    assert render_recall_block([]) is None


def test_punctuation_does_not_defeat_a_match():
    """Scoring used to split on whitespace only, so "do you remember my name?"
    scored 0 against "the user's name is Dana" — the trailing '?' made `name?`
    fail — and the memory was silently never recalled."""
    assert relevance_score("do you remember my name?", "The user's name is Dana.") > 0.0
    assert relevance_score("watchlist!", "the watchlist lives here") == pytest.approx(1.0)


def test_relevance_is_a_fraction_of_query_words():
    """Normalised to 0-1 so it is comparable between entries AND between
    languages — a raw overlap count is neither, and it is rendered into the
    prompt. Two of four query tokens hit."""
    assert relevance_score("watchlist on marvin today", "the watchlist lives on smoo-hub") == pytest.approx(0.5)
    assert relevance_score("", "anything") == 0.0


def test_recall_populates_relevance_and_type():
    mem = InMemoryMemory()
    mem.remember("the watchlist lives on smoo-hub", MemoryType.PROJECT)
    hits = mem.recall("watchlist on marvin today")
    assert len(hits) == 1
    assert hits[0].relevance == pytest.approx(0.5)
    assert hits[0].memory_type is MemoryType.PROJECT
