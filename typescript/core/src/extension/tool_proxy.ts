/**
 * `ExtensionTool` — a core {@link Tool} backed by an extension subprocess. The TS
 * sibling of the Rust host's `extension/tool_proxy.rs`.
 *
 * Registered tools appear to the agent as ordinary tools named `<extension>.<tool>`
 * (the MCP convention). `execute` forwards to the extension over `tool/execute` and
 * maps the reply back to a result string.
 */

import { randomUUID } from 'node:crypto';
import type { Tool } from '../agent.js';
import type { ExtensionProcess } from './process.js';
import { type Context, method, type ToolExecuteResult } from './protocol.js';

/**
 * Upper bound (ms) for a single `tool/execute` round-trip. The agent applies its
 * own per-tool timeout too; whichever is shorter wins in practice.
 */
export const TOOL_EXECUTE_TIMEOUT_MS = 120_000;

/** A tool exposed by an extension, adapted to the engine's {@link Tool} seam. */
export class ExtensionTool implements Tool {
    /** `<extension>.<tool>` — what the agent/LLM sees. */
    readonly name: string;
    readonly description: string;
    readonly parameters: Record<string, unknown>;
    /** Bare tool name sent to the extension. */
    private readonly bareName: string;

    constructor(
        extName: string,
        reg: { name: string; description: string; parameters: Record<string, unknown> },
        private readonly process: ExtensionProcess,
        private readonly context: Context,
    ) {
        this.name = `${extName}.${reg.name}`;
        this.bareName = reg.name;
        this.description = reg.description;
        this.parameters = reg.parameters;
    }

    async execute(args: Record<string, unknown>): Promise<string> {
        const callId = randomUUID();
        const params = { call_id: callId, tool: this.bareName, arguments: args, context: this.context };
        const raw = (await this.process.request(method.TOOL_EXECUTE, params, TOOL_EXECUTE_TIMEOUT_MS)) as ToolExecuteResult;
        if (typeof raw?.content !== 'string') throw new Error('malformed tool/execute result: missing content');
        if (raw.is_error) throw new Error(raw.content);
        // ponytail: `details` is dropped here — Tool.execute returns only a string.
        // Surfacing structured details rides on tool-update/event wiring in a later
        // phase; the field is preserved on ToolExecuteResult already.
        return raw.content;
    }
}
