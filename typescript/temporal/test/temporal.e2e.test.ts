/**
 * End-to-end tests against a real, ephemeral Temporal dev server — the TS sibling
 * of the Rust crate's `*_e2e.rs` (health, agent turn, durable timer, HITL signals).
 *
 * `@temporalio/testing`'s {@link TestWorkflowEnvironment} downloads (and caches)
 * the Temporal test server binary and runs it in-process — no Docker, no manually
 * installed `temporal` CLI. We stand up a worker whose workflows come from the
 * BUILT `dist/workflows.js` (so the Temporal bundler resolves real JS, not the
 * `.ts` sources) and its activities from a {@link MockLlmProvider}, then drive
 * each scenario through a workflow end to end.
 *
 * Self-skipping on two axes, so this never flakes a default CI run:
 *  1. gated behind `SMOOTH_TEMPORAL_E2E` (unset ⇒ the whole suite is skipped),
 *     mirroring the engine's `SMOOTH_AGENT_E2E`-gated live eval; and
 *  2. inside `beforeAll`, if the test server can't be downloaded/started (offline)
 *     or the package hasn't been built, it logs a skip and the specs no-op.
 *
 * Run it explicitly:
 *   pnpm --filter @smooai/smooth-operator-temporal build
 *   SMOOTH_TEMPORAL_E2E=1 pnpm --filter @smooai/smooth-operator-temporal test
 */

import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { MockLlmProvider } from '@smooai/smooth-operator-core';
import type { Tool } from '@smooai/smooth-operator-core';

import { createActivities } from '../src/activities.js';
import type { AgentTurnInput, AgentTurnResult } from '../src/dto.js';
import { AGENT_TURN_WORKFLOW_TYPE } from '../src/executor.js';
import { approveToolSignal, denyToolSignal } from '../src/signals.js';

const RUN = process.env.SMOOTH_TEMPORAL_E2E ? describe : describe.skip;

// Resolved against the BUILT output so the Temporal worker bundler sees real JS.
const WORKFLOWS_PATH = fileURLToPath(new URL('../dist/workflows.js', import.meta.url));

const HEALTH_WORKFLOW_TYPE = 'healthWorkflow';

const echoTool: Tool = {
    name: 'echo',
    description: 'Echoes input back',
    parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
    async execute(args: Record<string, unknown>): Promise<string> {
        return String(args.text ?? '');
    },
};

function lastAssistant(result: AgentTurnResult): string | undefined {
    for (let i = result.messages.length - 1; i >= 0; i--) {
        const m = result.messages[i];
        if (m.role === 'assistant' && typeof m.content === 'string' && m.content.length > 0) return m.content;
    }
    return undefined;
}

function toolMessages(result: AgentTurnResult): Array<Record<string, unknown>> {
    return result.messages.filter((m) => m.role === 'tool');
}

RUN('smooth-operator-temporal e2e', () => {
    // Types kept loose (`any`) so this test file typechecks WITHOUT a hard build
    // dependency on the Temporal SDK's type surface at check time.
    /* eslint-disable @typescript-eslint/no-explicit-any */
    let testEnv: any;
    let available = false;

    beforeAll(async () => {
        if (!existsSync(WORKFLOWS_PATH)) {
            console.warn(`SKIP: ${WORKFLOWS_PATH} missing — run \`pnpm --filter @smooai/smooth-operator-temporal build\` first.`);
            return;
        }
        try {
            const { TestWorkflowEnvironment } = await import('@temporalio/testing');
            testEnv = await TestWorkflowEnvironment.createLocal();
            available = true;
        } catch (err) {
            console.warn(`SKIP: could not start ephemeral Temporal test server (likely offline): ${String(err)}`);
        }
    }, 120_000);

    afterAll(async () => {
        await testEnv?.teardown();
    });

    async function runWorker(taskQueue: string, mock: MockLlmProvider, tools: Tool[], body: (client: any) => Promise<any>): Promise<any> {
        const { Worker } = await import('@temporalio/worker');
        const worker = await Worker.create({
            connection: testEnv.nativeConnection,
            taskQueue,
            workflowsPath: WORKFLOWS_PATH,
            activities: createActivities({ llm: mock, tools }),
        });
        return worker.runUntil(body(testEnv.client));
    }

    it('health workflow runs end to end', async () => {
        if (!available) return;
        const mock = new MockLlmProvider();
        const result = await runWorker('temporal-e2e-health', mock, [], (client) =>
            client.workflow.execute(HEALTH_WORKFLOW_TYPE, {
                taskQueue: 'temporal-e2e-health',
                workflowId: 'health-e2e-1',
                args: ['ping'],
            }),
        );
        expect(result).toBe('smooth-operator-temporal ok: ping');
    });

    it('agent turn runs a real (mocked) turn through the workflow', async () => {
        if (!available) return;
        const mock = new MockLlmProvider();
        mock.pushText('the durable answer is 42');
        const input: AgentTurnInput = {
            systemPrompt: 'You are a test agent',
            userMessage: 'what is the durable answer?',
            maxIterations: 5,
        };
        const result: AgentTurnResult = await runWorker('temporal-e2e-turn', mock, [], (client) =>
            client.workflow.execute(AGENT_TURN_WORKFLOW_TYPE, {
                taskQueue: 'temporal-e2e-turn',
                workflowId: 'agent-turn-e2e-1',
                args: [input],
            }),
        );
        expect(lastAssistant(result)).toBe('the durable answer is 42');
        expect(result.iterations).toBe(1);
        expect(mock.callCount).toBe(1);
        expect(mock.calls[0].messages.some((m) => JSON.stringify(m).includes('what is the durable answer?'))).toBe(true);
    });

    it('durable wait tool sleeps on a timer then resumes', async () => {
        if (!available) return;
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-wait', 'wait', '{"seconds":1}');
        mock.pushText('resumed after the timer');
        const input: AgentTurnInput = {
            systemPrompt: 'You are a self-pacing agent',
            userMessage: 'wait a moment, then answer',
            maxIterations: 5,
            waitTool: 'wait',
        };
        const result: AgentTurnResult = await runWorker('temporal-e2e-timer', mock, [], (client) =>
            client.workflow.execute(AGENT_TURN_WORKFLOW_TYPE, {
                taskQueue: 'temporal-e2e-timer',
                workflowId: 'durable-timer-1',
                args: [input],
            }),
        );
        const tools = toolMessages(result);
        expect(tools).toHaveLength(1);
        expect(String(tools[0].content)).toContain('durable timer');
        expect(lastAssistant(result)).toBe('resumed after the timer');
    });

    it('HITL gate approves and denies via signals', async () => {
        if (!available) return;
        const mock = new MockLlmProvider();
        // FIFO across BOTH sequential turns: approve turn, then deny turn.
        mock.pushToolCall('call-approve', 'echo', '{"text":"ran-after-approval"}');
        mock.pushText('done after approval');
        mock.pushToolCall('call-deny', 'echo', '{"text":"should-not-run"}');
        mock.pushText('done after denial');

        const [approved, denied]: [AgentTurnResult, AgentTurnResult] = await runWorker('temporal-e2e-hitl', mock, [echoTool], async (client) => {
            const mk = (id: string): AgentTurnInput => ({
                systemPrompt: 'You are a gated agent',
                userMessage: `use echo (${id})`,
                maxIterations: 5,
                approvalRequiredTools: ['echo'],
            });

            const approveHandle = await client.workflow.start(AGENT_TURN_WORKFLOW_TYPE, {
                taskQueue: 'temporal-e2e-hitl',
                workflowId: 'hitl-approve',
                args: [mk('approve')],
            });
            await approveHandle.signal(approveToolSignal, 'call-approve');
            const approvedResult: AgentTurnResult = await approveHandle.result();

            const denyHandle = await client.workflow.start(AGENT_TURN_WORKFLOW_TYPE, {
                taskQueue: 'temporal-e2e-hitl',
                workflowId: 'hitl-deny',
                args: [mk('deny')],
            });
            await denyHandle.signal(denyToolSignal, 'call-deny');
            const deniedResult: AgentTurnResult = await denyHandle.result();

            return [approvedResult, deniedResult];
        });

        const approvedTools = toolMessages(approved);
        expect(approvedTools).toHaveLength(1);
        expect(String(approvedTools[0].content)).toBe('ran-after-approval');
        expect(lastAssistant(approved)).toBe('done after approval');

        const deniedTools = toolMessages(denied);
        expect(deniedTools).toHaveLength(1);
        expect(String(deniedTools[0].content)).toContain('denied by human approval');
        expect(lastAssistant(denied)).toBe('done after denial');
    });
    /* eslint-enable @typescript-eslint/no-explicit-any */
});
