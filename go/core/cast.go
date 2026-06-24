package core

// Multi-agent cast: roles and per-role tool-access policy.
//
// Phase-2 sibling of the C# reference (dotnet/core/src/Cast.cs) and the Rust
// engine. A cast is the set of named roles a lead can dispatch to; each role has
// a RoleKind (Lead / Sidekick / Shadow) and a Clearance that gates which tools it
// may call.
//
// Clearance semantics (mirrors the reference engines):
//   - a deny always wins — a denied tool is never permitted;
//   - a non-empty allow-list is a whitelist — only listed tools are permitted;
//   - empty allow + empty deny means "all tools".
//
// Clearance is wired into the agent loop: if AgentOptions.Clearance forbids a tool
// the model asked for, that tool is not executed — a clear "not permitted" result
// is returned to the model instead, mirroring how the engine surfaces other tool
// errors.

// RoleKind is a role's place in a multi-agent cast.
type RoleKind int

const (
	// RoleLead is the orchestrator that delegates to sidekicks.
	RoleLead RoleKind = iota
	// RoleSidekick is a focused specialist a lead can dispatch a sub-task to.
	RoleSidekick
	// RoleShadow is a passive observer (e.g. for logging/critique); not directly dispatchable.
	RoleShadow
)

// Clearance is a tool-access policy for a role. A deny always wins; a non-empty
// AllowTools is a whitelist; empty allow + empty deny means "all tools".
// DenyEverything blocks every tool regardless of the lists.
type Clearance struct {
	allow          map[string]struct{}
	deny           map[string]struct{}
	denyEverything bool
}

func toSet(tools []string) map[string]struct{} {
	if len(tools) == 0 {
		return nil
	}
	s := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		s[t] = struct{}{}
	}
	return s
}

// AllowAllClearance permits every tool (the zero-value default).
func AllowAllClearance() Clearance { return Clearance{} }

// DenyAllClearance blocks every tool.
func DenyAllClearance() Clearance { return Clearance{denyEverything: true} }

// AllowClearance whitelists exactly the named tools.
func AllowClearance(tools ...string) Clearance { return Clearance{allow: toSet(tools)} }

// DenyClearance blocks the named tools (everything else allowed).
func DenyClearance(tools ...string) Clearance { return Clearance{deny: toSet(tools)} }

// NewClearance builds a clearance from explicit allow/deny lists and the
// deny-everything flag.
func NewClearance(allow, deny []string, denyEverything bool) Clearance {
	return Clearance{allow: toSet(allow), deny: toSet(deny), denyEverything: denyEverything}
}

// IsAllowed reports whether tool is permitted under this clearance.
func (c Clearance) IsAllowed(tool string) bool {
	if c.denyEverything {
		return false
	}
	if _, denied := c.deny[tool]; denied {
		return false
	}
	if len(c.allow) > 0 {
		_, ok := c.allow[tool]
		return ok
	}
	return true
}

// OperatorRole is a named role in the cast — its kind, instructions, tool
// clearance, and iteration budget. Mirrors the reference engines' OperatorRole.
type OperatorRole struct {
	Name          string
	Kind          RoleKind
	Instructions  string
	Permissions   Clearance
	MaxIterations int
	// Hidden roles are omitted from ListVisible (still dispatchable by name).
	Hidden bool
}

// NewOperatorRole builds a role with the reference-engine defaults applied
// (allow-all clearance, 8 iterations).
func NewOperatorRole(name string, kind RoleKind, instructions string) OperatorRole {
	return OperatorRole{
		Name:          name,
		Kind:          kind,
		Instructions:  instructions,
		Permissions:   AllowAllClearance(),
		MaxIterations: 8,
	}
}

// Cast is the registered set of roles a lead can dispatch to. Mirrors the
// reference engines' Cast.
type Cast struct {
	roles map[string]OperatorRole
	order []string // preserves registration order for stable listing
}

// NewCast returns an empty cast.
func NewCast() *Cast {
	return &Cast{roles: map[string]OperatorRole{}}
}

// Register adds (or replaces) a role and returns the cast for chaining.
func (c *Cast) Register(role OperatorRole) *Cast {
	if _, exists := c.roles[role.Name]; !exists {
		c.order = append(c.order, role.Name)
	}
	c.roles[role.Name] = role
	return c
}

// Get returns the role by name and whether it was found.
func (c *Cast) Get(name string) (OperatorRole, bool) {
	r, ok := c.roles[name]
	return r, ok
}

// List returns all roles in registration order.
func (c *Cast) List() []OperatorRole {
	out := make([]OperatorRole, 0, len(c.order))
	for _, name := range c.order {
		out = append(out, c.roles[name])
	}
	return out
}

// ListVisible returns the non-hidden roles in registration order.
func (c *Cast) ListVisible() []OperatorRole {
	out := make([]OperatorRole, 0, len(c.order))
	for _, name := range c.order {
		if r := c.roles[name]; !r.Hidden {
			out = append(out, r)
		}
	}
	return out
}

// Sidekicks returns the sidekick roles in registration order.
func (c *Cast) Sidekicks() []OperatorRole {
	out := make([]OperatorRole, 0, len(c.order))
	for _, name := range c.order {
		if r := c.roles[name]; r.Kind == RoleSidekick {
			out = append(out, r)
		}
	}
	return out
}

// Count is the number of registered roles.
func (c *Cast) Count() int { return len(c.roles) }

// IsEmpty reports whether no roles are registered.
func (c *Cast) IsEmpty() bool { return len(c.roles) == 0 }
