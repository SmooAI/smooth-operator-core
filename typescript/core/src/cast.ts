/**
 * Multi-agent cast: roles and per-role tool-access policy.
 *
 * Phase-2 sibling of the C# reference (`dotnet/core/src/Cast.cs`) and the Rust
 * engine. A *cast* is the set of named roles a lead can dispatch to; each role has
 * a {@link RoleKind} (Lead / Sidekick / Shadow) and a {@link Clearance} that gates
 * which tools it may call.
 *
 * {@link Clearance} semantics (mirrors the reference engines):
 * - a **deny always wins** — a denied tool is never permitted;
 * - a **non-empty allow-list is a whitelist** — only listed tools are permitted;
 * - **empty allow + empty deny means "all tools"**.
 *
 * Clearance is wired into the agent loop: if `AgentOptions.clearance` forbids a
 * tool the model asked for, that tool is *not* executed — a clear "not permitted"
 * result is returned to the model instead, mirroring how the engine surfaces other
 * tool errors.
 */

/** A role's place in a multi-agent cast. Mirrors the reference engines' `RoleKind`. */
export enum RoleKind {
    /** The orchestrator that delegates to sidekicks. */
    Lead = 'lead',
    /** A focused specialist a lead can dispatch a sub-task to. */
    Sidekick = 'sidekick',
    /** A passive observer (e.g. for logging/critique); not directly dispatchable. */
    Shadow = 'shadow',
}

/**
 * Tool-access policy for a role. A deny always wins; a non-empty `allowTools` is a
 * whitelist; empty allow + empty deny means "all tools". `denyEverything` blocks
 * every tool regardless of the lists.
 */
export class Clearance {
    readonly allowTools: ReadonlySet<string>;
    readonly denyTools: ReadonlySet<string>;
    readonly denyEverything: boolean;

    constructor(options: { allowTools?: Iterable<string>; denyTools?: Iterable<string>; denyEverything?: boolean } = {}) {
        this.allowTools = new Set(options.allowTools ?? []);
        this.denyTools = new Set(options.denyTools ?? []);
        this.denyEverything = options.denyEverything ?? false;
    }

    static allowAll(): Clearance {
        return new Clearance();
    }

    static denyAll(): Clearance {
        return new Clearance({ denyEverything: true });
    }

    static allow(...tools: string[]): Clearance {
        return new Clearance({ allowTools: tools });
    }

    static deny(...tools: string[]): Clearance {
        return new Clearance({ denyTools: tools });
    }

    /** Whether `tool` is permitted under this clearance. */
    isAllowed(tool: string): boolean {
        if (this.denyEverything) return false;
        if (this.denyTools.has(tool)) return false;
        if (this.allowTools.size > 0) return this.allowTools.has(tool);
        return true;
    }
}

/**
 * A named role in the cast — its kind, instructions, tool clearance, and budget.
 * Mirrors the reference engines' `OperatorRole`.
 */
export interface OperatorRole {
    name: string;
    kind: RoleKind;
    instructions?: string;
    /** Tool-access policy (defaults to allow-all when constructed via {@link makeRole}). */
    permissions?: Clearance;
    maxIterations?: number;
    /** Hidden from listings (still dispatchable by name). */
    hidden?: boolean;
}

/** Build an {@link OperatorRole} with the reference-engine defaults applied. */
export function makeRole(
    name: string,
    kind: RoleKind,
    overrides: { instructions?: string; permissions?: Clearance; maxIterations?: number; hidden?: boolean } = {},
): Required<OperatorRole> {
    return {
        name,
        kind,
        instructions: overrides.instructions ?? '',
        permissions: overrides.permissions ?? Clearance.allowAll(),
        maxIterations: overrides.maxIterations ?? 8,
        hidden: overrides.hidden ?? false,
    };
}

/** The registered set of roles a lead can dispatch to. Mirrors the reference engines' `Cast`. */
export class Cast {
    private readonly roles = new Map<string, OperatorRole>();

    register(role: OperatorRole): this {
        this.roles.set(role.name, role);
        return this;
    }

    get(name: string): OperatorRole | undefined {
        return this.roles.get(name);
    }

    list(): OperatorRole[] {
        return [...this.roles.values()];
    }

    listVisible(): OperatorRole[] {
        return this.list().filter((r) => !r.hidden);
    }

    sidekicks(): OperatorRole[] {
        return this.list().filter((r) => r.kind === RoleKind.Sidekick);
    }

    get count(): number {
        return this.roles.size;
    }

    get isEmpty(): boolean {
        return this.roles.size === 0;
    }
}
