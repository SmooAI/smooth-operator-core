"""Consumer-supplied **deny policy** (pearl th-deny-policy) — the Python sibling of
the Rust reference engine's ``deny_policy.rs`` and the deny-side counterpart to
:mod:`.permission_grants`.

The engine ships hardcoded circuit-breakers (``rm -rf /``, ``curl | sh``,
credential paths, dangerous domains — see :mod:`.permission`) and an allow-only
grant store that can *upgrade* an ``Ask``. Neither can express a consumer's own
"never do this" rules: "never touch the prod AWS profile", "the DB writer endpoint
is off-limits, reads go to the replica", "no writes under ``/prod``". This module
adds that missing tier.

It is **purely additive**: a :class:`~.permission.PermissionHook` with no deny
policy attached behaves exactly as before. When a policy *is* attached it is
evaluated **first**, and a match is a hard deny of the same tier as the built-in
circuit-breakers — no stored grant waives it, and :attr:`~.permission.AutoMode.BYPASS`
/ :attr:`~.permission.AutoMode.ACCEPT_EDITS` cannot downgrade it.

Two tiers:

1. **Declarative** (:class:`DenyRules`) — TOML, mirroring :mod:`.permission_grants`'
   section style::

       [tools]
       deny = ["vendor.dangerous_tool", "*.delete_prod"]

       [bash]
       deny_patterns = ["aws * --profile prod", "kubectl * --context prod"]

       [network]
       deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com"]

       [paths]
       deny = ["/prod/**", "**/secrets/**"]

2. **Predicate** (:class:`DenyPredicate`) — a subclass the consumer supplies for
   semantic checks the engine cannot parse from strings ("is this the *prod*
   account?", "writer vs replica?"). ``Some(reason)`` → deny.

Both run on every gated tool call; declarative first, then predicates. The first
match wins.
"""

from __future__ import annotations

import tomllib
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any

from .hooks import ToolCall
from .permission import (
    Category,
    domain_matches_suffix_list,
    extract_hosts,
    host_from_token,
    split_compound,
    strip_wrappers_and_sudo,
    tool_category,
)


def glob_match(pattern: str, text: str) -> bool:
    """Minimal both-ends-anchored glob: ``*`` (and any run of ``*``, so ``**`` too)
    matches any sequence of characters, including ``/``. No ``?``, no char classes —
    deny globs don't need them, and a tiny matcher stays auditable for a
    security-critical path."""
    parts = pattern.split("*")
    if len(parts) == 1:
        return pattern == text  # no wildcard → exact match
    first = parts[0]
    if not text.startswith(first):
        return False
    pos = len(first)
    last_idx = len(parts) - 1
    for i, part in enumerate(parts):
        if i == 0 or part == "":
            continue  # first segment handled; skip consecutive/trailing `*`
        if i == last_idx:
            end_start = len(text) - len(part)
            return end_start >= pos and text[pos:].endswith(part)
        idx = text.find(part, pos)
        if idx == -1:
            return False
        pos = idx + len(part)
    # Pattern ended with `*` (last part empty): the trailing run matches anything.
    return True


def _host_pattern_matches(pattern: str, host_lower: str) -> bool:
    """Match a single host deny pattern against an already-lowercased host.

    - no ``*`` → subdomain-aware suffix match (``prod.internal`` ⇒ ``api.prod.internal``).
    - ``*.suffix`` → apex + subdomains of ``suffix``.
    - mid-string ``*`` (``prod-*.rds.amazonaws.com``) → anchored glob.
    """
    p = pattern.lower()
    if "*" not in p:
        return domain_matches_suffix_list(host_lower, [p])
    if p.startswith("*."):
        bare = p[2:]
        if domain_matches_suffix_list(host_lower, [bare]):
            return True
    return glob_match(p, host_lower)


@dataclass
class DenyRules:
    """The declarative half of a :class:`DenyPolicy`: four deny lists parsed from TOML."""

    #: Reserved for forward-compatible migrations. Written as 1 by :meth:`new`.
    schema_version: int = 0
    tools_deny: set[str] = field(default_factory=set)
    bash_deny_patterns: set[str] = field(default_factory=set)
    network_deny_hosts: set[str] = field(default_factory=set)
    paths_deny: set[str] = field(default_factory=set)

    @staticmethod
    def new() -> "DenyRules":
        return DenyRules(schema_version=1)

    def is_empty(self) -> bool:
        """No rules in any section (used for the additive no-op fast path)."""
        return not (self.tools_deny or self.bash_deny_patterns or self.network_deny_hosts or self.paths_deny)

    @staticmethod
    def parse(toml_text: str) -> "DenyRules":
        """Parse from a TOML string. Missing sections default to empty."""
        data = tomllib.loads(toml_text)
        return DenyRules(
            schema_version=int(data.get("schema_version", 0) or 0),
            tools_deny=set(_str_list(data.get("tools", {}).get("deny", []))),
            bash_deny_patterns=set(_str_list(data.get("bash", {}).get("deny_patterns", []))),
            network_deny_hosts=set(_str_list(data.get("network", {}).get("deny_hosts", []))),
            paths_deny=set(_str_list(data.get("paths", {}).get("deny", []))),
        )

    def to_toml_string(self) -> str:
        """Serialize to TOML. Empty sections are omitted (matching the Rust engine)."""
        lines = [f"schema_version = {self.schema_version or 1}"]
        if self.tools_deny:
            lines += ["", "[tools]", f"deny = {_toml_array(self.tools_deny)}"]
        if self.bash_deny_patterns:
            lines += ["", "[bash]", f"deny_patterns = {_toml_array(self.bash_deny_patterns)}"]
        if self.network_deny_hosts:
            lines += ["", "[network]", f"deny_hosts = {_toml_array(self.network_deny_hosts)}"]
        if self.paths_deny:
            lines += ["", "[paths]", f"deny = {_toml_array(self.paths_deny)}"]
        return "\n".join(lines) + "\n"

    def deny_reason(self, call: ToolCall) -> str | None:
        """The first declarative rule this call matches, formatted as a deny reason."""
        # `[tools]` applies to ANY tool, whatever its category.
        for pat in sorted(self.tools_deny):
            if glob_match(pat, call.name):
                return f"denied by policy (tools): {pat}"
        args = call.arguments if isinstance(call.arguments, dict) else {}
        cat = tool_category(call.name)
        if cat is Category.BASH:
            cmd = _get_str(args, "cmd", "command")
            cmd = cmd.strip() if cmd else ""
            if not cmd:
                return None
            pat = self._bash_denied(cmd)
            if pat is not None:
                return f"denied by policy (bash): {pat}"
            # A denied host referenced by the command line is also blocked.
            for sub in split_compound(cmd):
                for host in extract_hosts(sub):
                    hp = self._host_denied(host)
                    if hp is not None:
                        return f"denied by policy (network): {hp}"
            return None
        if cat is Category.NETWORK:
            raw = _get_str(args, "url", "host") or ""
            host = host_from_token(raw) or raw
            if not host:
                return None
            hp = self._host_denied(host)
            return f"denied by policy (network): {hp}" if hp is not None else None
        if cat in (Category.WRITE, Category.SAFE):
            for key in ("path", "file", "dir", "directory"):
                v = _get_str(args, key)
                if v is None:
                    continue
                for pat in sorted(self.paths_deny):
                    if glob_match(pat, v):
                        return f"denied by policy (paths): {pat}"
            return None
        return None  # Category.UNKNOWN

    def _bash_denied(self, cmd: str) -> str | None:
        """First ``[bash]`` pattern that matches any (wrapper/sudo-stripped) subcommand."""
        subs = [strip_wrappers_and_sudo(s).lower() for s in split_compound(cmd)]
        for pat in sorted(self.bash_deny_patterns):
            lower = pat.lower()
            # A plain prefix (`"aws "`) gets an implicit trailing `*`; a pattern with
            # an explicit `*` also matches any trailing text so extra flags can't slip
            # a call past the rule.
            anchored = lower if lower.endswith("*") else f"{lower}*"
            if any(glob_match(anchored, sub) for sub in subs):
                return pat
        return None

    def _host_denied(self, host: str) -> str | None:
        """First ``[network]`` pattern that matches ``host`` (case-insensitive)."""
        h = host.lower()
        for pat in sorted(self.network_deny_hosts):
            if _host_pattern_matches(pat, h):
                return pat
        return None


@dataclass(frozen=True)
class DenyReason:
    """The reason a :class:`DenyPredicate` blocks a call. A thin wrapper over ``str``
    so the predicate contract is explicit and can grow structured fields later."""

    reason: str

    @staticmethod
    def new(reason: str) -> "DenyReason":
        return DenyReason(reason)


class DenyPredicate(ABC):
    """A consumer-supplied semantic deny check. Runs on every gated tool call; a
    non-``None`` return is a hard deny (circuit-breaker tier). Use it for the checks
    the declarative rules can't express from strings alone — resolving an AWS call to
    its account, a DB URL to writer-vs-replica, etc."""

    @abstractmethod
    def evaluate(self, call: ToolCall) -> DenyReason | None:
        """Return a :class:`DenyReason` to deny ``call``, ``None`` to let it fall
        through to the rest of the permission engine."""
        ...


class DenyPolicy:
    """Consumer-supplied deny policy: declarative rules + predicate checks. Attach to
    the gate via :meth:`~.permission.PermissionHook.with_deny_policy` or the agent's
    ``deny_policy`` option. An empty policy is a no-op."""

    def __init__(
        self,
        declarative: DenyRules | None = None,
        predicates: list[DenyPredicate] | None = None,
    ) -> None:
        self._declarative = declarative if declarative is not None else DenyRules()
        self._predicates: list[DenyPredicate] = list(predicates) if predicates else []

    @staticmethod
    def from_toml(toml_text: str) -> "DenyPolicy":
        """Build the declarative half from a TOML string. Predicates are added
        separately via :meth:`with_predicate`."""
        return DenyPolicy(declarative=DenyRules.parse(toml_text))

    def with_declarative(self, rules: DenyRules) -> "DenyPolicy":
        """Replace the declarative rules. Chainable."""
        self._declarative = rules
        return self

    def with_predicate(self, predicate: DenyPredicate) -> "DenyPolicy":
        """Add a consumer predicate. Chainable."""
        self._predicates.append(predicate)
        return self

    def is_empty(self) -> bool:
        """True when there are no rules and no predicates — nothing to deny."""
        return self._declarative.is_empty() and not self._predicates

    def evaluate(self, call: ToolCall) -> str | None:
        """The deny reason for ``call``, or ``None`` to let it fall through to the
        rest of the permission engine. Declarative rules are checked first, then
        predicates; the first match wins."""
        reason = self._declarative.deny_reason(call)
        if reason is not None:
            return reason
        for predicate in self._predicates:
            r = predicate.evaluate(call)
            if r is not None:
                return f"denied by policy (predicate): {r.reason}"
        return None


def _get_str(args: Any, *keys: str) -> str | None:
    if not isinstance(args, dict):
        return None
    for k in keys:
        v = args.get(k)
        if isinstance(v, str):
            return v
    return None


def _str_list(value: object) -> list[str]:
    if isinstance(value, list):
        return [v for v in value if isinstance(v, str)]
    return []


def _toml_escape(s: str) -> str:
    return s.replace("\\", "\\\\").replace('"', '\\"')


def _toml_array(values: set[str]) -> str:
    return "[" + ", ".join(f'"{_toml_escape(v)}"' for v in sorted(values)) + "]"
