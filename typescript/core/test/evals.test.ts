/**
 * LLM-as-judge eval suite for the TypeScript core.
 *
 * Every scenario comes from the SHARED corpus at `spec/evals/scenarios.json` —
 * nothing is defined here. See that file's `$comment` for why (the scenarios used
 * to be hand-duplicated in five languages and had already forked).
 *
 * Two tests live here:
 *
 *  - "corpus matches the shared spec" — OFFLINE, always runs. The drift guard.
 *  - the live-gateway suite — gated on `SMOOTH_AGENT_E2E=1` + `SMOOAI_GATEWAY_KEY`,
 *    so it's a no-op (never fails) without credentials:
 *
 *      SMOOAI_GATEWAY_KEY=... SMOOTH_AGENT_E2E=1 pnpm --filter @smooai/smooth-operator-core test
 */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { AgentOptions, ChatClientLike, SmoothAgent } from '../src/agent.js';
import { InMemoryKnowledge } from '../src/knowledge.js';

const GATEWAY_URL = 'https://llm.smoo.ai/v1';
const DEFAULT_MODEL = 'claude-haiku-4-5';

/**
 * A RATCHET, not a duplicate of the corpus. Comparing the loaded set against the
 * file catches a language that subsets or mis-parses it, but not a scenario
 * deleted from the file itself — both sides shrink together and every language
 * stays green. This floor is what makes a deletion loud. Raise it when you add
 * scenarios; lowering it should require saying why in the PR.
 */
const MIN_SCENARIOS = 15;

interface EvalDoc {
    content: string;
    source: string;
}

interface EvalScenario {
    id: string;
    tier: 'core' | 'hard';
    intent: string;
    kb_docs: string[];
    user_turns: string[];
    ground_truth: string;
    rubric: string;
}

interface EvalCorpus {
    support_prompt: string;
    judge_system_prompt: string;
    aggregate_mean_threshold: number;
    hard_aggregate_mean_threshold: number;
    docs: Record<string, EvalDoc>;
    scenarios: EvalScenario[];
}

/** The shared corpus, relative to this test file (typescript/core/test). */
const CORPUS_PATH = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'spec', 'evals', 'scenarios.json');

const CORPUS = JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as EvalCorpus;

/** Resolve a scenario's `kb_docs` keys into the documents to seed. */
function docsFor(scenario: EvalScenario): EvalDoc[] {
    return scenario.kb_docs.map((key) => {
        const doc = CORPUS.docs[key];
        if (!doc) throw new Error(`scenario ${scenario.id} references unknown doc "${key}"`);
        return doc;
    });
}

function parseVerdict(text: string): { score: number; reasoning?: string } {
    const match = text.match(/\{[\s\S]*\}/);
    if (!match) throw new Error(`judge did not return JSON: ${text}`);
    return JSON.parse(match[0]);
}

// The drift guard: runs offline in normal CI. A language that subsets, filters or
// mis-parses the corpus goes red here instead of quietly running a forked suite
// (which is how the .NET corpus drifted).
describe('shared eval corpus', () => {
    it('matches spec/evals/scenarios.json — same count, same ids', () => {
        const fileIds = (JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as EvalCorpus).scenarios.map((s) => s.id);
        const loadedIds = CORPUS.scenarios.map((s) => s.id);
        expect(loadedIds.length).toBe(fileIds.length);
        expect([...loadedIds].sort()).toEqual([...fileIds].sort());
        expect(new Set(loadedIds).size).toBe(loadedIds.length);
    });

    it('has not shrunk below the ratchet floor', () => {
        expect(CORPUS.scenarios.length).toBeGreaterThanOrEqual(MIN_SCENARIOS);
    });

    it('is runnable: every scenario resolves its docs and carries a rubric', () => {
        for (const scenario of CORPUS.scenarios) {
            expect(scenario.user_turns.length, `${scenario.id} turns`).toBeGreaterThan(0);
            expect(scenario.ground_truth, `${scenario.id} ground truth`).toBeTruthy();
            expect(scenario.rubric, `${scenario.id} rubric`).toBeTruthy();
            expect(() => docsFor(scenario)).not.toThrow();
        }
        expect(CORPUS.scenarios.some((s) => s.tier === 'core')).toBe(true);
        expect(CORPUS.support_prompt).toBeTruthy();
        expect(CORPUS.judge_system_prompt).toBeTruthy();
    });
});

const gated = process.env.SMOOTH_AGENT_E2E === '1' && !!process.env.SMOOAI_GATEWAY_KEY;

describe.skipIf(!gated)('TS core eval suite (live gateway)', () => {
    it('clears the aggregate-mean threshold', async () => {
        const { default: OpenAI } = await import('openai');
        const apiKey = process.env.SMOOAI_GATEWAY_KEY!;
        const judgeModel = process.env.SMOOTH_AGENT_JUDGE_MODEL || DEFAULT_MODEL;
        const client = new OpenAI({ baseURL: GATEWAY_URL, apiKey }) as unknown as ChatClientLike & {
            chat: { completions: { create(b: Record<string, unknown>): Promise<{ choices: Array<{ message: { content: string | null } }> }> } };
        };

        // Tiers are scored separately: core must clear the real bar, hard sits on a
        // lenient floor so one adversarial miss is an improvement target, not a red CI.
        const byTier: Record<string, number[]> = { core: [], hard: [] };

        for (const scenario of CORPUS.scenarios) {
            const knowledge = new InMemoryKnowledge();
            for (const doc of docsFor(scenario)) knowledge.ingest(doc.content, doc.source);
            const agent = new SmoothAgent(client, { instructions: CORPUS.support_prompt, model: DEFAULT_MODEL, knowledge } satisfies AgentOptions);

            const history: Array<Record<string, unknown>> = [];
            let reply = '';
            for (const turn of scenario.user_turns) {
                const result = await agent.run(turn, history);
                reply = result.text;
                history.push({ role: 'user', content: turn }, { role: 'assistant', content: reply });
            }

            const judgeUser = `GROUND TRUTH:\n${scenario.ground_truth}\n\nRUBRIC:\n${scenario.rubric}\n\nAGENT REPLY:\n${reply}\n\nScore it now as JSON.`;
            const verdictResp = await client.chat.completions.create({
                model: judgeModel,
                messages: [
                    { role: 'system', content: CORPUS.judge_system_prompt },
                    { role: 'user', content: judgeUser },
                ],
                temperature: 0,
                max_tokens: 300,
            });
            const verdict = parseVerdict(verdictResp.choices[0].message.content ?? '');
            (byTier[scenario.tier] ??= []).push(verdict.score);
            console.log(`[ts-eval] (${scenario.tier}) ${scenario.id}: ${verdict.score}/5 — ${verdict.reasoning ?? ''}`);
        }

        for (const [tier, threshold] of [
            ['core', CORPUS.aggregate_mean_threshold],
            ['hard', CORPUS.hard_aggregate_mean_threshold],
        ] as const) {
            const scores = byTier[tier] ?? [];
            if (scores.length === 0) continue;
            const mean = scores.reduce((a, b) => a + b, 0) / scores.length;
            console.log(`[ts-eval] ${tier} aggregate mean ${mean.toFixed(2)}/5 across ${scores.length} scenarios; scores=[${scores.join(', ')}]`);
            expect(mean, `${tier} tier`).toBeGreaterThanOrEqual(threshold);
        }
    }, 300_000);
});
