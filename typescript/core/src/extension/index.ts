/**
 * SEP host — the engine's implementation of the Smooth Extension Protocol.
 *
 * An extension is a long-lived subprocess speaking JSON-RPC 2.0 over ndjson on its
 * stdio (identical framing to MCP stdio). The canonical wire schemas live in the
 * `smooth-operator` repo at `spec/extension/`; {@link module:protocol} is this
 * host's typed view of that wire. The TS sibling of the Rust reference host at
 * `rust/smooth-operator-core/src/extension/`.
 *
 * Purely additive: nothing here runs unless a caller builds an {@link ExtensionHost}
 * and registers its {@link ExtensionHost.tools} into an agent. With no host built,
 * the agent loop behaves exactly as before.
 */

export * from './protocol.js';
export * from './manifest.js';
export {
    backoffFor,
    DefaultInboundHandler,
    ExtensionProcess,
    type InboundHandler,
    OBSERVE_QUEUE_CAP,
    PING_IDLE_MS,
    RESTART_BACKOFFS_MS,
    type SpawnSpec,
} from './process.js';
export {
    DefaultHostDelegate,
    effectiveSubscriptions,
    ExtensionHost,
    foldHookChain,
    type FoldedHook,
    hookDefaultTimeoutMs,
    hookFailClosed,
    type HostDelegate,
    type HookStep,
    type HookType,
    hookTypeFromName,
    type LoadFailure,
    validateCommandContext,
} from './host.js';
export { ExtensionTool, TOOL_EXECUTE_TIMEOUT_MS } from './tool_proxy.js';

/**
 * Canonical SEP event names the host dispatches to subscribed extensions. A
 * stringly-typed name is the wire contract; the engine's own event producers map
 * onto these.
 */
export const events = {
    TURN_START: 'turn_start',
    TURN_END: 'turn_end',
    MESSAGE_START: 'message_start',
    MESSAGE_UPDATE: 'message_update',
    MESSAGE_END: 'message_end',
    // Names mirror pi's `tool_execution_*` so pi extensions port unchanged.
    TOOL_EXECUTION_START: 'tool_execution_start',
    TOOL_EXECUTION_UPDATE: 'tool_execution_update',
    TOOL_EXECUTION_END: 'tool_execution_end',
    /** Delivered when the bounded observe queue shed events. Carries `{lost: N}`. */
    EVENTS_LOST: 'events_lost',
} as const;
