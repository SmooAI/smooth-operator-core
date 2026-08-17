import { describe, expect, it } from 'vitest';
import { END, subWorkflowNode, Workflow, WorkflowError } from '../src/workflow.js';

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

describe('subWorkflowNode', () => {
    const append = (name: string) => (state: string[]) => [...state, name];
    const identity = (state: string[]) => state;
    const takeChild = (_parent: string[], child: string[]) => child;

    /** Two tracking nodes joined by a conditional edge that skips a third. */
    const twoNodeChild = () =>
        new Workflow<string[]>()
            .addNode('child_a', append('child_a'))
            .addNode('child_b', append('child_b'))
            .addNode('child_never', append('child_never'))
            .addConditionalEdge('child_a', (s) => (s.includes('child_a') ? 'child_b' : 'child_never'))
            .setEntry('child_a')
            .setEnd('child_b');

    it('runs the whole sub-graph — 2 nodes and a conditional edge — in one parent step', async () => {
        const wf = new Workflow<string[]>()
            .addNode('parent_a', append('parent_a'))
            .addNode('sub', subWorkflowNode(twoNodeChild(), identity, takeChild))
            .addNode('parent_b', append('parent_b'))
            .addEdge('parent_a', 'sub')
            .addEdge('sub', 'parent_b')
            .setEntry('parent_a')
            .setEnd('parent_b');

        expect(await wf.run([])).toEqual(['parent_a', 'child_a', 'child_b', 'parent_b']);
    });

    it('maps parent state into the child and folds the result back out', async () => {
        // Parent state is a labelled total; the child only ever sees the number.
        const child = new Workflow<number>()
            .addNode('add_ten', (n) => n + 10)
            .addNode('double', (n) => n * 2)
            .addEdge('add_ten', 'double')
            .setEntry('add_ten')
            .setEnd('double');

        const wf = new Workflow<{ label: string; total: number }>()
            .addNode(
                'math',
                subWorkflowNode(
                    child,
                    (p) => p.total,
                    (p, total) => ({ label: `${p.label}:done`, total }),
                ),
            )
            .setEntry('math')
            .setEnd('math');

        expect(await wf.run({ label: 'start', total: 5 })).toEqual({ label: 'start:done', total: 30 });
    });

    it('propagates a child node error to the parent run', async () => {
        const child = new Workflow<string[]>()
            .addNode('boom', () => {
                throw new Error('child exploded');
            })
            .setEntry('boom')
            .setEnd('boom');

        const wf = new Workflow<string[]>()
            .addNode('parent_a', append('parent_a'))
            .addNode('sub', subWorkflowNode(child, identity, takeChild))
            .addEdge('parent_a', 'sub')
            .setEntry('parent_a')
            .setEnd('sub');

        await expect(wf.run([])).rejects.toThrow(/child exploded/);
    });

    it('nests deeper than one level — parent → child → grandchild', async () => {
        const grandchild = new Workflow<string[]>()
            .addNode('grand_a', append('grand_a'))
            .addNode('grand_b', append('grand_b'))
            .addEdge('grand_a', 'grand_b')
            .setEntry('grand_a')
            .setEnd('grand_b');

        const child = new Workflow<string[]>()
            .addNode('child_a', append('child_a'))
            .addNode('grand', subWorkflowNode(grandchild, identity, takeChild))
            .addEdge('child_a', 'grand')
            .setEntry('child_a')
            .setEnd('grand');

        const wf = new Workflow<string[]>()
            .addNode('parent_a', append('parent_a'))
            .addNode('sub', subWorkflowNode(child, identity, takeChild))
            .addEdge('parent_a', 'sub')
            .setEntry('parent_a')
            .setEnd('sub');

        expect(await wf.run([])).toEqual(['parent_a', 'child_a', 'grand_a', 'grand_b']);
    });
});
