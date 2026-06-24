"""Multi-agent cast: roles and per-role tool-access policy.

Phase-2 sibling of the C# reference (``dotnet/core/src/Cast.cs``) and the Rust
engine. A *cast* is the set of named roles a lead can dispatch to; each role has a
:class:`RoleKind` (Lead / Sidekick / Shadow) and a :class:`Clearance` that gates
which tools it may call.

:class:`Clearance` semantics (mirrors the reference engines):

* a **deny always wins** — a denied tool is never permitted;
* a **non-empty allow-list is a whitelist** — only listed tools are permitted;
* **empty allow + empty deny means "all tools"**.

Clearance is wired into the agent loop: if ``AgentOptions.clearance`` forbids a
tool the model asked for, that tool is *not* executed — a clear "not permitted"
result is returned to the model instead, mirroring how the engine surfaces other
tool errors.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class RoleKind(Enum):
    """A role's place in a multi-agent cast. Mirrors the reference engines' ``RoleKind``."""

    #: The orchestrator that delegates to sidekicks.
    LEAD = "lead"
    #: A focused specialist a lead can dispatch a sub-task to.
    SIDEKICK = "sidekick"
    #: A passive observer (e.g. for logging/critique); not directly dispatchable.
    SHADOW = "shadow"


@dataclass(frozen=True)
class Clearance:
    """Tool-access policy for a role.

    A deny always wins; a non-empty ``allow_tools`` is a whitelist; empty allow +
    empty deny means "all tools". ``deny_everything`` blocks every tool regardless.
    """

    allow_tools: frozenset[str] = field(default_factory=frozenset)
    deny_tools: frozenset[str] = field(default_factory=frozenset)
    #: Block every tool regardless of the lists.
    deny_everything: bool = False

    @staticmethod
    def allow_all() -> "Clearance":
        return Clearance()

    @staticmethod
    def deny_all() -> "Clearance":
        return Clearance(deny_everything=True)

    @staticmethod
    def allow(*tools: str) -> "Clearance":
        return Clearance(allow_tools=frozenset(tools))

    @staticmethod
    def deny(*tools: str) -> "Clearance":
        return Clearance(deny_tools=frozenset(tools))

    def is_allowed(self, tool: str) -> bool:
        """Whether ``tool`` is permitted under this clearance."""
        if self.deny_everything:
            return False
        if tool in self.deny_tools:
            return False
        if self.allow_tools:
            return tool in self.allow_tools
        return True


@dataclass(frozen=True)
class OperatorRole:
    """A named role in the cast — its kind, instructions, tool clearance, and budget.

    Mirrors the reference engines' ``OperatorRole``.
    """

    name: str
    kind: RoleKind
    instructions: str = ""
    permissions: Clearance = field(default_factory=Clearance.allow_all)
    max_iterations: int = 8
    #: Hidden from listings (still dispatchable by name).
    hidden: bool = False


class Cast:
    """The registered set of roles a lead can dispatch to.

    Mirrors the reference engines' ``Cast``.
    """

    def __init__(self) -> None:
        self._roles: dict[str, OperatorRole] = {}

    def register(self, role: OperatorRole) -> "Cast":
        self._roles[role.name] = role
        return self

    def get(self, name: str) -> OperatorRole | None:
        return self._roles.get(name)

    def list(self) -> list[OperatorRole]:
        return list(self._roles.values())

    def list_visible(self) -> list[OperatorRole]:
        return [r for r in self._roles.values() if not r.hidden]

    def sidekicks(self) -> list[OperatorRole]:
        return [r for r in self._roles.values() if r.kind is RoleKind.SIDEKICK]

    @property
    def count(self) -> int:
        return len(self._roles)

    @property
    def is_empty(self) -> bool:
        return not self._roles
