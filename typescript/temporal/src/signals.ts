/**
 * Signal definitions for the agent-turn workflow, in their own module so they are
 * safe to import from BOTH sides: the workflow (which handles them) and a client
 * (which sends them).
 *
 * {@link defineSignal} only builds a signal descriptor — it needs no workflow
 * context — so importing this file in a normal Node/client process is safe, unlike
 * importing `workflows.ts` (whose top-level `proxyActivities` throws outside a
 * workflow).
 */

import { defineSignal } from '@temporalio/workflow';

/** A human approves the tool call with this id, unblocking the durable HITL gate. */
export const approveToolSignal = defineSignal<[string]>('approveTool');
/** A human denies the tool call with this id; the gate skips it with an error result. */
export const denyToolSignal = defineSignal<[string]>('denyTool');
