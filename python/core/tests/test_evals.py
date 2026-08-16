"""LLM-as-judge eval suite for the Python core.

Every scenario comes from the SHARED corpus at ``spec/evals/scenarios.json`` —
nothing is defined here. See that file's ``$comment`` for why (the scenarios used
to be hand-duplicated in five languages and had already forked).

Two tests live here:

* :func:`test_eval_corpus_matches_spec` — OFFLINE, always runs. The drift guard.
* :func:`test_eval_aggregate_mean_clears_threshold` — gated on
  ``SMOOTH_AGENT_E2E=1`` + ``SMOOAI_GATEWAY_KEY``, so it skips cleanly (never
  fails) without credentials::

    SMOOAI_GATEWAY_KEY=... SMOOTH_AGENT_E2E=1 uv run pytest tests/test_evals.py -s
"""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path

import pytest

from smooth_operator_core import AgentOptions, InMemoryKnowledge, SmoothAgent

GATEWAY_URL = "https://llm.smoo.ai/v1"
DEFAULT_MODEL = "claude-haiku-4-5"

#: A RATCHET, not a duplicate of the corpus. Comparing the loaded set against the
#: file catches a language that subsets or mis-parses it, but not a scenario
#: deleted from the file itself — both sides shrink together and every language
#: stays green. This floor is what makes a deletion loud. Raise it when you add
#: scenarios; lowering it should require saying why in the PR.
MIN_SCENARIOS = 15

#: The shared corpus, relative to this test file (python/core/tests).
CORPUS_PATH = Path(__file__).resolve().parents[3] / "spec" / "evals" / "scenarios.json"


@dataclass(frozen=True)
class Scenario:
    id: str
    tier: str
    intent: str
    kb_docs: list[str]
    user_turns: list[str]
    ground_truth: str
    rubric: str


def _load_corpus() -> dict:
    return json.loads(CORPUS_PATH.read_text(encoding="utf-8"))


CORPUS = _load_corpus()
SCENARIOS: list[Scenario] = [
    Scenario(
        id=s["id"],
        tier=s["tier"],
        intent=s["intent"],
        kb_docs=s["kb_docs"],
        user_turns=s["user_turns"],
        ground_truth=s["ground_truth"],
        rubric=s["rubric"],
    )
    for s in CORPUS["scenarios"]
]


def _docs_for(scenario: Scenario) -> list[tuple[str, str]]:
    """Resolve a scenario's ``kb_docs`` keys into (content, source) pairs."""
    out: list[tuple[str, str]] = []
    for key in scenario.kb_docs:
        doc = CORPUS["docs"].get(key)
        if doc is None:
            raise KeyError(f"scenario {scenario.id} references unknown doc {key!r}")
        out.append((doc["content"], doc["source"]))
    return out


def test_eval_corpus_matches_spec() -> None:
    """The drift guard: runs OFFLINE in normal CI.

    Asserts the scenario set this suite would execute is exactly the set in
    spec/evals/scenarios.json — same count, same ids — so a language that subsets,
    filters or mis-parses the corpus goes red here instead of quietly running a
    forked suite (which is how the .NET corpus drifted).
    """
    file_ids = [s["id"] for s in _load_corpus()["scenarios"]]
    loaded_ids = [s.id for s in SCENARIOS]

    assert len(loaded_ids) == len(file_ids), f"corpus count drift: loaded {len(loaded_ids)}, spec has {len(file_ids)}"
    assert sorted(loaded_ids) == sorted(file_ids), "corpus id drift"
    assert len(set(loaded_ids)) == len(loaded_ids), "duplicate scenario ids"

    assert len(SCENARIOS) >= MIN_SCENARIOS, (
        f"corpus shrank: {len(SCENARIOS)} scenarios < ratchet floor {MIN_SCENARIOS} — "
        "a scenario was deleted from spec/evals/scenarios.json"
    )

    # Every scenario must be runnable: resolvable docs, and a non-empty prompt,
    # ground truth and rubric. Catches a malformed corpus before a nightly burns
    # gateway spend discovering it.
    for scenario in SCENARIOS:
        assert scenario.user_turns, f"{scenario.id} has no user turns"
        assert scenario.ground_truth, f"{scenario.id} has no ground truth"
        assert scenario.rubric, f"{scenario.id} has no rubric"
        _docs_for(scenario)

    assert any(s.tier == "core" for s in SCENARIOS), "corpus has no core-tier scenarios"
    assert CORPUS["support_prompt"] and CORPUS["judge_system_prompt"]


def _gateway_client(api_key: str):
    from openai import AsyncOpenAI

    return AsyncOpenAI(base_url=GATEWAY_URL, api_key=api_key)


def _parse_verdict(text: str) -> dict:
    # Tolerate markdown fences / stray prose around the JSON object.
    match = re.search(r"\{.*\}", text, re.DOTALL)
    if not match:
        raise ValueError(f"judge did not return JSON: {text!r}")
    return json.loads(match.group(0))


@pytest.mark.asyncio
async def test_eval_aggregate_mean_clears_threshold(capsys):
    if os.environ.get("SMOOTH_AGENT_E2E") != "1":
        pytest.skip("SMOOTH_AGENT_E2E != '1' — skipping live-gateway eval suite.")
    api_key = os.environ.get("SMOOAI_GATEWAY_KEY")
    if not api_key:
        pytest.skip("SMOOAI_GATEWAY_KEY unset/empty — skipping live-gateway eval suite.")

    judge_model = os.environ.get("SMOOTH_AGENT_JUDGE_MODEL") or DEFAULT_MODEL
    client = _gateway_client(api_key)

    # Tiers are scored separately: core must clear the real bar, hard sits on a
    # lenient floor so one adversarial miss is an improvement target, not a red CI.
    by_tier: dict[str, list[int]] = {"core": [], "hard": []}

    for scenario in SCENARIOS:
        knowledge = InMemoryKnowledge()
        for content, source in _docs_for(scenario):
            knowledge.ingest(content, source)
        agent = SmoothAgent(
            client,
            AgentOptions(instructions=CORPUS["support_prompt"], model=DEFAULT_MODEL, knowledge=knowledge),
        )

        history: list[dict] = []
        reply = ""
        for turn in scenario.user_turns:
            result = await agent.run(turn, history=history)
            reply = result.text
            history.append({"role": "user", "content": turn})
            history.append({"role": "assistant", "content": reply})

        judge_user = (
            f"GROUND TRUTH:\n{scenario.ground_truth}\n\nRUBRIC:\n{scenario.rubric}\n\n"
            f"AGENT REPLY:\n{reply}\n\nScore it now as JSON."
        )
        verdict_resp = await client.chat.completions.create(
            model=judge_model,
            messages=[
                {"role": "system", "content": CORPUS["judge_system_prompt"]},
                {"role": "user", "content": judge_user},
            ],
            temperature=0.0,
            max_tokens=300,
        )
        verdict = _parse_verdict(verdict_resp.choices[0].message.content or "")
        score = int(verdict["score"])
        by_tier.setdefault(scenario.tier, []).append(score)
        with capsys.disabled():
            print(f"[py-eval] ({scenario.tier}) {scenario.id}: {score}/5 — {verdict.get('reasoning', '')}")

    failures: list[str] = []
    for tier, threshold in (
        ("core", CORPUS["aggregate_mean_threshold"]),
        ("hard", CORPUS["hard_aggregate_mean_threshold"]),
    ):
        scores = by_tier.get(tier) or []
        if not scores:
            continue
        mean = sum(scores) / len(scores)
        with capsys.disabled():
            print(f"[py-eval] {tier} aggregate mean {mean:.2f}/5 across {len(scores)} scenarios; scores={scores}")
        if mean < threshold:
            failures.append(f"{tier} aggregate mean {mean:.2f} < {threshold}; scores={scores}")

    assert not failures, "; ".join(failures)
