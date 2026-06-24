import { describe, expect, it } from 'vitest';
import { END, Workflow, WorkflowError } from '../src/workflow.js';

describe('workflow', () => {
    it('runs a linear 3-node graph start→end, transforming state', async () => {
        const append = (name: string) => (state: string[]) => [...state, name];
        const wf = new Workflow<string[]>()
            .addNode('a', append('a'))
            .addNode('b', append('b'))
            .addNode('c', append('c'))
            .addEdge('a', 'b')
            .addEdge('b', 'c')
            .setEntry('a')
            .setEnd('c');

        expect(await wf.run([])).toEqual(['a', 'b', 'c']);
    });

    it('routes a conditional edge to different nodes based on state (both branches)', async () => {
        const build = () =>
            new Workflow<{ n: number; branch?: number }>()
                .addNode('start', (s) => s)
                .addNode('left', (s) => ({ ...s, branch: -1 }))
                .addNode('right', (s) => ({ ...s, branch: 1 }))
                .addConditionalEdge('start', (s) => (s.n > 0 ? 'right' : 'left'))
                .setEntry('start')
                .setEnd('left')
                .setEnd('right');

        expect((await build().run({ n: 5 })).branch).toBe(1);
        expect((await build().run({ n: -5 })).branch).toBe(-1);
    });

    it('awaits async nodes', async () => {
        const wf = new Workflow<number>()
            .addNode('addTen', async (s) => s + 10)
            .addNode('double', async (s) => s * 2)
            .addEdge('addTen', 'double')
            .setEntry('addTen')
            .setEnd('double');

        expect(await wf.run(5)).toBe(30); // (5 + 10) * 2
    });

    it('a router returning END terminates the workflow', async () => {
        const wf = new Workflow<number>()
            .addNode('only', (s) => s + 1)
            .addConditionalEdge('only', () => END)
            .setEntry('only');

        expect(await wf.run(0)).toBe(1);
    });

    it('a node with no outgoing edge is an implicit end', async () => {
        const wf = new Workflow<number>().addNode('only', (s) => s + 1).setEntry('only');
        expect(await wf.run(0)).toBe(1);
    });

    it('hits the maxSteps cap on an unbroken cycle', async () => {
        const wf = new Workflow<string[]>(6)
            .addNode('a', (s) => [...s, 'a'])
            .addNode('b', (s) => [...s, 'b'])
            .addEdge('a', 'b')
            .addEdge('b', 'a')
            .setEntry('a');

        await expect(wf.run([])).rejects.toThrow(/maxSteps/);
    });

    it('throws when no entry node is set', async () => {
        await expect(new Workflow<number>().run(0)).rejects.toThrow(WorkflowError);
    });

    it('throws when the entry node was never registered', async () => {
        await expect(new Workflow<number>().setEntry('ghost').run(0)).rejects.toThrow(/not found/);
    });

    it('throws when an edge points at a missing node', async () => {
        const wf = new Workflow<number>().addNode('a', (s) => s).addEdge('a', 'ghost').setEntry('a');
        await expect(wf.run(0)).rejects.toThrow(/not found/);
    });
});
