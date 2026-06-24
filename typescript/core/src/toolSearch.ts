/**
 * The `tool_search` meta-tool — promotes deferred tools on demand.
 *
 * Phase-3 sibling of the Rust reference `tool_search.rs`. Mirrors the behaviour,
 * not the type shapes (this core has no `ToolRegistry` — tools are a plain array
 * on {@link AgentOptions}).
 *
 * As a tool set grows past ~20-30 entries, every model turn pays tokens to read
 * schemas it isn't going to use, diluting the model's attention budget. So a caller
 * can register some tools as **deferred** (`AgentOptions.deferredTools`): their
 * schemas are hidden from the model. Instead the agent advertises a single built-in
 * `tool_search(query)` meta-tool. When the model calls it, this fuzzy-matches the
 * query against the deferred tools' names + descriptions, **promotes** the matches
 * into the visible set (so the model can call them on subsequent turns), and returns
 * each match's name + description as JSON.
 *
 * A deferred tool that has not been promoted is *not* dispatchable — calling it
 * surfaces as an unknown tool until `tool_search` adds it to the promoted set.
 */

import type { Tool } from './agent.js';

/** The built-in meta-tool's name. Reserved when deferred tools are in play. */
export const TOOL_SEARCH_NAME = 'tool_search';

/**
 * Cap on how many deferred tools a single `tool_search` call may promote, so a
 * generic query like "tool" doesn't promote the entire deferred set in one shot.
 */
export const MAX_MATCHES = 8;

const SCHEMA: Record<string, unknown> = {
    type: 'object',
    properties: {
        query: {
            type: 'string',
            description: 'Keyword to match against deferred tool names and descriptions. Case-insensitive substring match.',
        },
    },
    required: ['query'],
};

const DESCRIPTION =
    'Search for additional tools by keyword. Returns matching tool schemas as JSON; ' +
    'matched tools become available on subsequent turns. Use when you think a tool ' +
    "exists for a specific task but isn't in your current tool list — e.g. " +
    'tool_search(query="git") or tool_search(query="http request").';

/**
 * Drives deferred-tool promotion for one agent run. Implements {@link Tool} so the
 * agent can advertise + dispatch it like any other tool. Holds the deferred tools
 * (by name) and the mutable set of promoted names; the agent consults
 * {@link promotedTools} each iteration to decide which deferred schemas are now
 * visible/dispatchable.
 */
export class ToolSearch implements Tool {
    readonly name = TOOL_SEARCH_NAME;
    readonly description = DESCRIPTION;
    readonly parameters = SCHEMA;

    private readonly deferredByName: Map<string, Tool>;
    private readonly promoted = new Set<string>();

    constructor(deferred: Tool[]) {
        this.deferredByName = new Map(deferred.map((t) => [t.name, t]));
    }

    /** True if any tool was registered deferred (the meta-tool is advertised only then). */
    hasDeferred(): boolean {
        return this.deferredByName.size > 0;
    }

    /** True if a deferred tool has been promoted and is now dispatchable. */
    isPromoted(name: string): boolean {
        return this.promoted.has(name);
    }

    /** The deferred tools that have been promoted — their schemas join the visible set. */
    promotedTools(): Tool[] {
        const out: Tool[] = [];
        for (const n of this.promoted) {
            const t = this.deferredByName.get(n);
            if (t) out.push(t);
        }
        return out;
    }

    /** Resolve a promoted deferred tool for dispatch. Unpromoted deferred tools are invisible. */
    toolByName(name: string): Tool | undefined {
        return this.promoted.has(name) ? this.deferredByName.get(name) : undefined;
    }

    /** Mark a deferred tool promoted. Returns false if no such deferred tool. */
    promote(name: string): boolean {
        if (!this.deferredByName.has(name)) return false;
        this.promoted.add(name);
        return true;
    }

    /** Fuzzy-match the query, promote matches, and return their schemas as JSON. */
    async execute(args: Record<string, unknown>): Promise<string> {
        const query = args.query;
        if (typeof query !== 'string') {
            return JSON.stringify({ matched: 0, tools: [], note: 'missing required `query` parameter' });
        }
        const needle = query.trim().toLowerCase();
        if (needle.length === 0) {
            return JSON.stringify({ matched: 0, tools: [], note: 'empty query — pass a keyword like "git" or "network"' });
        }

        const matched: Tool[] = [];
        for (const t of this.deferredByName.values()) {
            if (t.name.toLowerCase().includes(needle) || t.description.toLowerCase().includes(needle)) {
                matched.push(t);
                if (matched.length >= MAX_MATCHES) break;
            }
        }

        for (const t of matched) this.promoted.add(t.name);

        const tools = matched.map((t) => ({ name: t.name, description: t.description, parameters: t.parameters }));
        return JSON.stringify({ matched: tools.length, tools });
    }
}
