/**
 * SEP wire protocol — JSON-RPC 2.0 frames and typed method params/results.
 *
 * SEP (the Smooth Extension Protocol) is JSON-RPC 2.0 over ndjson on an extension
 * subprocess's stdio. The canonical schemas live in the `smooth-operator` repo at
 * `spec/extension/`; the types here are the TS engine host's view of that wire —
 * the sibling of the Rust host's `extension/protocol.rs`. Field names are
 * `snake_case` because they ARE the wire.
 *
 * The author-side `@smooai/smooth-extension-sdk` ships its own copy of these
 * shapes; the host keeps them local rather than importing the SDK, which would
 * invert the dependency (the engine is the lower layer the SDK's host consumers
 * build on).
 */

/** The SEP protocol version this host implements. Effective = min(host, ext). */
export const PROTOCOL_VERSION = 1;

/** SEP method names, centralized so the host and tests never spell one wrong. */
export const method = {
    INITIALIZE: 'initialize',
    SHUTDOWN: 'shutdown',
    PING: 'ping',
    EVENT: 'event',
    HOOK: 'hook',
    TOOL_EXECUTE: 'tool/execute',
    TOOL_UPDATE: 'tool/update',
    COMMAND_EXECUTE: 'command/execute',
    COMMAND_COMPLETE: 'command/complete',
    CANCEL: '$/cancel',
    REGISTRY_UPDATE: 'registry/update',
    TOOLS_SET_ACTIVE: 'tools/set_active',
    EXEC_RUN: 'exec/run',
    UI_REQUEST: 'ui/request',
    LOG: 'log',
    BUS_PUBLISH: 'bus/publish',
    SESSION_SEND_MESSAGE: 'session/send_message',
    SESSION_SEND_USER_MESSAGE: 'session/send_user_message',
    SESSION_APPEND_ENTRY: 'session/append_entry',
} as const;

/**
 * JSON-RPC + SEP error codes (see `spec/extension/envelope.md`). Standard range
 * plus the SEP extensions.
 */
export const codes = {
    ParseError: -32700,
    InvalidRequest: -32600,
    MethodNotFound: -32601,
    InvalidParams: -32602,
    InternalError: -32603,
    /** A hook or policy vetoed the operation. */
    Blocked: -32000,
    /** `ui/request` in a headless/uncapable frontend. */
    NoUI: -32001,
    /** Extension acted beyond its granted trust. */
    NotTrusted: -32002,
    /** Command-tier action attempted from an event-tier context. */
    ContextViolation: -32003,
    /** Method requires a capability the handshake did not enable. */
    CapabilityDisabled: -32004,
    /** Request cancelled via `$/cancel`. */
    Cancelled: -32800,
} as const;

/** A JSON-RPC error object. */
export interface RpcErrorObject {
    code: number;
    message: string;
    data?: unknown;
}

/** An error carrying a JSON-RPC error code, thrown/returned for a remote error. */
export class RpcError extends Error {
    constructor(
        public readonly code: number,
        message: string,
        public readonly data?: unknown,
    ) {
        super(message);
        this.name = 'RpcError';
    }

    toObject(): RpcErrorObject {
        return this.data === undefined ? { code: this.code, message: this.message } : { code: this.code, message: this.message, data: this.data };
    }
}

/** A JSON-RPC id: an integer or a string (null only on a parse-error response). */
export type Id = number | string | null;

/**
 * The JSON-RPC 2.0 envelope. All four frame shapes share this type; which fields
 * are present determines the shape:
 * - request: `id` + `method` (+ optional `params`)
 * - notification: `method`, no `id`
 * - success response: `id` + `result`
 * - error response: `id` + `error`
 */
export interface Message {
    jsonrpc: '2.0';
    id?: Id;
    method?: string;
    params?: unknown;
    result?: unknown;
    error?: RpcErrorObject;
}

/** Build a request frame. */
export function request(id: Exclude<Id, null>, methodName: string, params: unknown): Message {
    return { jsonrpc: '2.0', id, method: methodName, params };
}

/** Build a notification frame (no id, no reply expected). */
export function notification(methodName: string, params: unknown): Message {
    return { jsonrpc: '2.0', method: methodName, params };
}

/** Build a success response frame echoing `id`. */
export function success(id: Id, result: unknown): Message {
    return { jsonrpc: '2.0', id, result };
}

/** Build an error response frame echoing `id`. */
export function errorResponse(id: Id, error: RpcErrorObject): Message {
    return { jsonrpc: '2.0', id, error };
}

/** True when this frame is a request (has both `id` and `method`). */
export function isRequest(m: Message): boolean {
    return m.id !== undefined && m.id !== null && m.method !== undefined;
}

/** True when this frame is a notification (has `method`, no `id`). */
export function isNotification(m: Message): boolean {
    return (m.id === undefined || m.id === null) && m.method !== undefined;
}

/** True when this frame is a response (has `id`, no `method`). */
export function isResponse(m: Message): boolean {
    return m.method === undefined && m.id !== undefined && m.id !== null;
}

// ---------------------------------------------------------------------------
// The two-tier dispatch context.
// ---------------------------------------------------------------------------

/**
 * Whether a dispatch may only observe (`event`) or may mutate the session
 * (`command`). Session-mutating ext→host actions require `command`.
 */
export type Tier = 'event' | 'command';

/** The dispatch context carried by every host→ext event/hook/tool/command. */
export interface Context {
    token: string;
    tier: Tier;
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

export interface HostInfo {
    name: string;
    version: string;
}

export interface WorkspaceInfo {
    root: string;
    trusted: boolean;
}

export interface InitializeParams {
    protocol_version: number;
    host: HostInfo;
    workspace: WorkspaceInfo;
    session?: { id?: string };
    mode: string;
    ui_capabilities?: string[];
    /** Parsed values for the flags the extension declares (name → value). */
    flags?: Record<string, unknown>;
    capabilities_enabled?: Record<string, unknown>;
}

export interface ToolRegistration {
    name: string;
    description: string;
    /** JSON Schema for the tool's arguments. */
    parameters: Record<string, unknown>;
    deferred?: boolean;
}

export interface CommandRegistration {
    name: string;
    description: string;
}

/** A keyboard shortcut an extension binds to one of its commands. */
export interface ShortcutRegistration {
    /** A human-typed chord, e.g. `ctrl+p`; the frontend parses it. */
    key: string;
    /** The registered command this chord invokes (no leading `/`). */
    command: string;
    description?: string;
}

export interface Registrations {
    tools?: ToolRegistration[];
    commands?: CommandRegistration[];
    flags?: string[];
    shortcuts?: ShortcutRegistration[];
    subscriptions?: string[];
}

export interface InitializeResult {
    protocol_version: number;
    extension: { name: string; version: string };
    registrations?: Registrations;
}

// ---------------------------------------------------------------------------
// hook
// ---------------------------------------------------------------------------

export interface HookParams {
    hook: string;
    context: Context;
    input: unknown;
}

/** The extension's reply to a `hook`, tagged by `action`. */
export type HookOutcome =
    | { action: 'continue' }
    | { action: 'block'; reason?: string }
    | { action: 'modify'; patch: unknown };

/**
 * Parse an untyped `hook` reply into a {@link HookOutcome}, or throw when it is
 * malformed. Mirrors the Rust host's serde-tagged decode: a `modify` without a
 * `patch`, or an unknown `action`, is rejected (the host then treats it as a
 * failed hook step).
 */
export function parseHookOutcome(value: unknown): HookOutcome {
    if (typeof value !== 'object' || value === null) throw new RpcError(codes.InvalidParams, 'hook outcome is not an object');
    const action = (value as { action?: unknown }).action;
    if (action === 'continue') return { action: 'continue' };
    if (action === 'block') {
        const reason = (value as { reason?: unknown }).reason;
        return typeof reason === 'string' ? { action: 'block', reason } : { action: 'block' };
    }
    if (action === 'modify') {
        if (!('patch' in (value as object))) throw new RpcError(codes.InvalidParams, "hook 'modify' outcome missing patch");
        return { action: 'modify', patch: (value as { patch: unknown }).patch };
    }
    throw new RpcError(codes.InvalidParams, `unknown hook action: ${String(action)}`);
}

// ---------------------------------------------------------------------------
// tool/execute + tool/update
// ---------------------------------------------------------------------------

export interface ToolExecuteParams {
    call_id: string;
    tool: string;
    arguments: unknown;
    context: Context;
}

export interface ToolExecuteResult {
    content: string;
    is_error?: boolean;
    details?: unknown;
}

export interface ToolUpdateParams {
    call_id: string;
    message?: string;
    progress?: number;
    details?: unknown;
}

// ---------------------------------------------------------------------------
// event
// ---------------------------------------------------------------------------

export interface EventParams {
    event: string;
    /** Per-connection monotonic sequence; absent on the `events_lost` marker. */
    seq?: number;
    context: Context;
    payload?: unknown;
}

// ---------------------------------------------------------------------------
// command/execute + command/complete (host→ext)
// ---------------------------------------------------------------------------

export interface CommandExecuteParams {
    command: string;
    context: Context;
    arguments?: unknown;
}

export interface CommandExecuteResult {
    content?: string;
}

export interface CommandCompleteParams {
    command: string;
    context: Context;
    partial?: string;
}

export interface Completion {
    value: string;
    description?: string;
}

export interface CommandCompleteResult {
    completions: Completion[];
}

// ---------------------------------------------------------------------------
// session/* (ext→host) — all require COMMAND tier
// ---------------------------------------------------------------------------

/** How a `session/send_user_message` is delivered relative to the current turn. */
export type DeliverAs = 'steer' | 'follow_up' | 'next_turn';

export interface SessionSendMessageParams {
    context: Context;
    text: string;
    role?: 'user' | 'assistant';
}

export interface SessionSendUserMessageParams {
    context: Context;
    text: string;
    deliver_as?: DeliverAs;
}

export interface SessionAppendEntryParams {
    context: Context;
    entry: unknown;
}
