"""Native tool-call permission gate for the Python engine — the sibling of the
Rust reference engine's ``permission.rs`` (pearl th-d32ce6 / th-22bfc1 /
th-deny-policy).

:class:`PermissionHook` is a :class:`~.hooks.ToolHook` that runs the pure,
deterministic :func:`decide` classifier on every tool call and BLOCKS the call
(by raising in ``pre_call``) on a **Deny**. An **Ask** is routed to a human when
the hook has an interactive approver wired (a :class:`~.human_gate.HumanGate`) and
**fails closed** (blocks) when it does not.

The classification model is ported natively from smooth's
``smooth-bigsmooth::auto_mode`` — the security-critical core, exhaustively tested
including adversarial compound-command and credential-path inputs.

Precedence (highest first):

1. A consumer :class:`~.deny_policy.DenyPolicy` (evaluated FIRST) — a circuit-breaker
   that no stored grant waives and no :class:`AutoMode` downgrades.
2. Built-in circuit-breakers inside :func:`decide` (``rm -rf /``, ``curl | sh``,
   credential paths, dangerous domains, env dumps) — same tier.
3. Stored grants (``wonk-allow.toml``) can upgrade an ``Ask`` to silent-allow.
4. The interactive approver (human) resolves a remaining ``Ask``.
"""

from __future__ import annotations

import asyncio
import logging
import re
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import TYPE_CHECKING, Any, Optional, Union

from .hooks import ToolCall, ToolHook
from .human_gate import HumanApprovalRequest, HumanDecision, HumanGate
from .permission_grants import (
    BashGrant,
    GrantQuery,
    NetworkGrant,
    PermissionGrants,
    SharedGrants,
    ToolGrant,
    append_grant,
)

if TYPE_CHECKING:  # avoid an import cycle: deny_policy imports helpers from here.
    from .deny_policy import DenyPolicy

logger = logging.getLogger(__name__)


class AutoMode(Enum):
    """How aggressively the hook enforces. Mirrors smooth's ``AutoMode``. Selected
    via the ``SMOOTH_AUTO_MODE`` env var."""

    #: Read-only allow, mutating ask, dangerous deny. Default.
    ASK = "ask"
    #: Like ASK but filesystem-edit tools (the Write category) auto-approve instead
    #: of asking. Hard circuit-breakers still block. Mirrors Claude Code's ``acceptEdits``.
    ACCEPT_EDITS = "accept_edits"
    #: Like ASK but never asks — an unmatched verdict is a **deny** (fail-closed).
    #: The headless / CI posture (Claude Code's ``dontAsk``).
    DENY_UNMATCHED = "deny_unmatched"
    #: Allow everything **except** the hard circuit-breakers. Escape hatch
    #: equivalent to Claude Code's ``bypassPermissions`` (which keeps its breakers).
    BYPASS = "bypass"

    @staticmethod
    def from_env_value(v: str | None) -> "AutoMode":
        """Parse a ``SMOOTH_AUTO_MODE`` value. Unknown / unset → :attr:`ASK`."""
        if v is None:
            return AutoMode.ASK
        norm = v.strip().lower().replace("-", "").replace("_", "")
        if norm in ("deny", "denyunmatched", "dontask", "headless"):
            return AutoMode.DENY_UNMATCHED
        if norm in ("bypass", "bypasspermissions", "yolo"):
            return AutoMode.BYPASS
        if norm in ("acceptedits", "acceptedit", "edits"):
            return AutoMode.ACCEPT_EDITS
        return AutoMode.ASK

    @staticmethod
    def from_env() -> "AutoMode":
        """Read the mode from the process ``SMOOTH_AUTO_MODE`` environment variable."""
        import os

        return AutoMode.from_env_value(os.environ.get("SMOOTH_AUTO_MODE"))


# ── the pure verdict ──────────────────────────────────────────────
@dataclass(frozen=True)
class Allow:
    """Let the call through."""


@dataclass(frozen=True)
class Deny:
    """Block the call outright. Carries a human/LLM-readable reason."""

    reason: str


@dataclass(frozen=True)
class Ask:
    """Pause and ask a human. Carries the reason to show. With no interactive
    approver wired, the hook treats this as fail-closed."""

    reason: str


#: The verdict returned by :func:`decide`. Mirrors the Rust ``Verdict`` enum.
Verdict = Union[Allow, Deny, Ask]


# ---------------------------------------------------------------------------
# Circuit-breaker data (ported from smooth-narc::judge + auto_mode)
# ---------------------------------------------------------------------------
DANGEROUS_DOMAIN_SUFFIXES: tuple[str, ...] = (
    ".ngrok.io",
    ".ngrok-free.app",
    "etherscan.io",
    "blockchain.info",
    "binance.com",
    "pastebin.com",
    "termbin.com",
    "transfer.sh",
)

DANGEROUS_CLI_SUBSTRINGS: tuple[str, ...] = (
    "rm -rf /",
    "rm -rf ~",
    ":(){ :|:& };:",
    "mkfs",
    "dd if=/dev/zero of=/dev/",
    "> /dev/sda",
    "chmod -r 777 /",
    "| sudo sh",
    "systemctl mask",
)

SENSITIVE_PATH_SUBSTRINGS: tuple[str, ...] = (
    ".ssh/",
    ".aws/credentials",
    ".aws/config",
    ".config/gh/",
    ".config/gcloud",
    ".gnupg",
    ".kube/config",
    ".docker/config.json",
    ".npmrc",
    ".pypirc",
    ".netrc",
    "/etc/shadow",
    "id_rsa",
    "id_ed25519",
    ".smooth/providers.json",
    ".smooth/auth/",
)

SAFE_BASH_BINS: frozenset[str] = frozenset(
    {
        "ls",
        "cat",
        "head",
        "tail",
        "wc",
        "grep",
        "rg",
        "fd",
        "find",
        "echo",
        "pwd",
        "which",
        "whoami",
        "date",
        "true",
        "test",
        "dirname",
        "basename",
        "realpath",
        "stat",
        "file",
        "cksum",
        "sha256sum",
        "md5sum",
    }
)

SAFE_GIT_SUBCOMMANDS: frozenset[str] = frozenset(
    {"status", "log", "diff", "show", "branch", "remote", "rev-parse", "describe", "blame", "ls-files"}
)

GIT_LIST_ONLY_FLAGS: frozenset[str] = frozenset(
    {"-a", "-r", "-v", "-vv", "--all", "--list", "--verbose", "--show-current", "--merged", "--no-merged"}
)

NET_BASH_BINS: frozenset[str] = frozenset({"curl", "wget", "http", "https", "nc", "ncat", "telnet"})

SHELL_INTERPRETERS: frozenset[str] = frozenset({"sh", "bash", "zsh", "dash", "ksh"})

SENSITIVE_VAR_FRAGMENTS: tuple[str, ...] = (
    "secret",
    "token",
    "password",
    "passwd",
    "api_key",
    "apikey",
    "access_key",
    "credential",
    "private_key",
    "aws_",
    "ssh_",
    "session",
)

_WRAPPERS: frozenset[str] = frozenset({"timeout", "nice", "nohup", "stdbuf", "env"})
_ENV_DUMP_WRAPPERS: frozenset[str] = frozenset({"timeout", "nice", "nohup", "stdbuf"})
_FIND_ACTION_FLAGS: frozenset[str] = frozenset(
    {"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprintf", "-fls"}
)
_VAR_RE = re.compile(r"\$\{?([a-z0-9_]+)")


def domain_matches_suffix_list(domain: str, suffixes: tuple[str, ...] | list[str]) -> bool:
    """Match a domain against a suffix list (exact or subdomain), case-insensitive."""
    d = domain.lower()
    for suffix in suffixes:
        s = suffix.lower()
        if d == s or d.endswith(f".{s}") or (s.startswith(".") and d.endswith(s)):
            return True
    return False


def split_compound(command: str) -> list[str]:
    """Split a shell command line into subcommands on the operators that sequence
    independent commands: ``&&``, ``||``, ``;``, ``|``, ``&``, and newlines. Command
    / process substitution (``$(…)``, `````…````, ``<(…)``) is surfaced as its own
    segment so it can't ride in on a safe outer command."""
    # ponytail: substring split, not a real shell lexer — upgrade only if quoting
    # edge-cases (`echo "a && b"`) start mattering for policy.
    sentinel = "\x01"  # U+0001 split marker (mirrors the Rust reference)
    normalized = command.replace("&&", sentinel).replace("||", sentinel)
    if "$(" in normalized or "<(" in normalized or "`" in normalized:
        normalized = (
            normalized.replace("$(", sentinel).replace("<(", sentinel).replace("`", sentinel).replace(")", sentinel)
        )
    out: list[str] = []
    for raw in re.split(r"[;|&\n]", normalized):
        seg = raw.strip().strip("\"'").strip()
        if seg:
            out.append(seg)
    return out


def _strip_wrappers(tokens: list[str]) -> int:
    """Index of the real command after leading transparent wrappers."""
    i = 0
    while i < len(tokens) and tokens[i] in _WRAPPERS:
        i += 1
        while i < len(tokens) and (tokens[i].startswith("-") or (tokens[i][:1].isdigit())):
            i += 1
    return i


def command_bin(subcommand: str) -> str | None:
    """First meaningful token of a subcommand (after stripping wrappers)."""
    tokens = subcommand.split()
    start = _strip_wrappers(tokens)
    return tokens[start] if start < len(tokens) else None


def host_from_token(tok: str) -> str | None:
    """Pull a bare hostname out of a URL-ish or ``host:port`` token."""
    after_scheme = tok.split("://", 1)[1] if "://" in tok else tok
    after_userinfo = after_scheme.rsplit("@", 1)[1] if "@" in after_scheme else after_scheme
    host = re.split(r"[/:?#]", after_userinfo, maxsplit=1)[0].strip()
    if not host:
        return None
    if host == "localhost" or ("." in host and not host.startswith(".") and not host.endswith(".")):
        return host.lower()
    return None


def extract_hosts(subcommand: str) -> list[str]:
    """Candidate hostnames from a single (already split) net-tool subcommand. Empty
    if the binary isn't a net tool."""
    tokens = subcommand.split()
    start = _strip_wrappers(tokens)
    if start >= len(tokens) or tokens[start] not in NET_BASH_BINS:
        return []
    hosts: list[str] = []
    for t in tokens[start + 1 :]:
        if t.startswith("-"):
            continue
        h = host_from_token(t)
        if h is not None:
            hosts.append(h)
    return hosts


def _sink_bin(segment: str) -> str | None:
    """Effective binary of a pipe segment, skipping leading ``sudo`` and wrappers."""
    tokens = segment.split()
    i = _strip_wrappers(tokens)
    while i < len(tokens) and tokens[i] == "sudo":
        i += 1
        while i < len(tokens) and tokens[i].startswith("-"):
            i += 1
    return tokens[i] if i < len(tokens) else None


def _is_pipe_to_shell(command: str) -> bool:
    """Does this whole command line pipe a network fetch into a shell interpreter
    (``curl … | sh``, ``wget … | bash``)? A hard circuit-breaker regardless of host."""
    if "|" not in command:
        return False
    saw_fetch = False
    for seg in command.split("|"):
        b = _sink_bin(seg.strip())
        if b is None:
            continue
        if saw_fetch and b in SHELL_INTERPRETERS:
            return True
        if b in NET_BASH_BINS:
            saw_fetch = True
    return False


def strip_wrappers_and_sudo(subcommand: str) -> str:
    """Strip leading transparent wrappers and any leading ``sudo`` from a single
    subcommand, returning the remaining command text. Used by the deny policy so a
    rule anchored on the real binary (``aws …``) still matches ``sudo aws …`` /
    ``timeout 5 aws …``."""
    tokens = subcommand.split()
    i = _strip_wrappers(tokens)
    while i < len(tokens) and tokens[i] == "sudo":
        i += 1
        while i < len(tokens) and tokens[i].startswith("-"):
            i += 1
    return " ".join(tokens[i:])


def _references_sensitive_path(command: str) -> bool:
    """Does the command reference a sensitive credential path?"""
    lower = command.lower()
    if any(p.lower() in lower for p in SENSITIVE_PATH_SUBSTRINGS):
        return True
    # `.env` / `.envrc` / `.env.local` dotenv files are secret stores too.
    # Token-scoped so `rg "process.env" src/` isn't flagged.
    for tok in lower.split():
        t = tok.strip("\"'();")
        if t.startswith(".env") or "/.env" in t:
            return True
    return False


def _contains_sensitive_var_expansion(text: str) -> bool:
    """True if the text contains a ``$NAME`` / ``${NAME}`` expansion whose name
    matches a :data:`SENSITIVE_VAR_FRAGMENTS` fragment."""
    lower = text.lower()
    for m in _VAR_RE.finditer(lower):
        name = m.group(1)
        if name and any(f in name for f in SENSITIVE_VAR_FRAGMENTS):
            return True
    return False


def _dumps_environment(subcommand: str) -> bool:
    """Does this single subcommand reveal the process environment? Matches on intent,
    not a single binary name. Deliberately does NOT match legitimate setter forms
    (``env FOO=bar cmd``, ``export FOO=bar``, ``set -euo pipefail``)."""
    toks = subcommand.split()
    if not toks:
        return False
    lower = subcommand.lower()
    if "proc/" in lower and "/environ" in lower:
        return True
    i = 0
    while i < len(toks) and toks[i] in _ENV_DUMP_WRAPPERS:
        i += 1
        while i < len(toks) and (toks[i].startswith("-") or toks[i][:1].isdigit()):
            i += 1
    if i >= len(toks):
        return False
    b = toks[i]
    rest = toks[i + 1 :]
    if b == "printenv":
        return True
    if b == "env":
        k = 0
        while k < len(rest):
            t = rest[k]
            if t in ("-u", "-S"):
                k += 2
            elif t.startswith("-") or "=" in t or t == "-":
                k += 1
            else:
                return False  # a bare command token → setter form
        return True
    if b in ("export", "declare", "typeset"):
        return not any("=" in t for t in rest) and all(t.startswith("-") for t in rest)
    if b == "set":
        return len(rest) == 0
    if b in ("echo", "printf"):
        return _contains_sensitive_var_expansion(subcommand)
    return False


def _is_safe_readonly_bash(subcommand: str) -> bool:
    """Is this single subcommand a compiled-in safe read-only command?"""
    b = command_bin(subcommand)
    if b is None:
        return False
    if b == "find":
        return not any(t in _FIND_ACTION_FLAGS for t in subcommand.split())
    if b in SAFE_BASH_BINS:
        return True
    if b == "git":
        tokens = subcommand.split()
        start = _strip_wrappers(tokens)
        j = start + 1
        while j < len(tokens) and tokens[j].startswith("-"):
            j += 2  # `-c key=val` / `-C dir`: skip flag + value.
        if j >= len(tokens):
            return False
        sub = tokens[j]
        if sub not in SAFE_GIT_SUBCOMMANDS:
            return False
        if sub in ("branch", "remote"):
            return all(t in GIT_LIST_ONLY_FLAGS for t in tokens[j + 1 :])
        return True
    return False


def _decide_bash_subcommand(subcommand: str) -> Verdict:
    """Evaluate a single bash subcommand against the layered policy."""
    # 1. Credential-path guard — deny read AND write (exfil risk).
    if _references_sensitive_path(subcommand):
        return Deny(f"command references a sensitive credential path: {subcommand}")
    # 1b. Environment-dump guard — the process env is a secret store.
    if _dumps_environment(subcommand):
        return Deny(f"command reveals the process environment (secret exfiltration risk): {subcommand}")
    # 2. Baseline dangerous-CLI deny.
    lower = subcommand.lower()
    for needle in DANGEROUS_CLI_SUBSTRINGS:
        if needle.lower() in lower:
            return Deny(f"command matches dangerous-cli pattern: {needle}")
    # 3. Dangerous network hosts referenced by this subcommand → deny.
    hosts = extract_hosts(subcommand)
    for host in hosts:
        if domain_matches_suffix_list(host, DANGEROUS_DOMAIN_SUFFIXES):
            return Deny(f"{host} is on the dangerous-domain deny list")
    # 4. Net tool with a non-dangerous host → ask.
    if hosts:
        return Ask(f"outbound request to {hosts[0]} needs approval")
    # 5. Compiled-in safe read-only command → allow.
    if _is_safe_readonly_bash(subcommand):
        return Allow()
    # 6. Unmatched mutating command → ask.
    b = command_bin(subcommand) or ""
    return Ask(f"`{b}` is not a known-safe command")


def _decide_bash(command: str) -> Verdict:
    """Evaluate a whole (possibly compound) bash command line. Every subcommand must
    clear on its own; the strictest verdict wins (deny > ask > allow)."""
    # Whole-line dangerous-substring scan FIRST — some breakers (the fork bomb,
    # `| sudo sh`) contain the very operators split_compound divides on.
    lower_line = command.lower()
    for needle in DANGEROUS_CLI_SUBSTRINGS:
        if needle.lower() in lower_line:
            return Deny(f"command matches dangerous-cli pattern: {needle}")
    if _is_pipe_to_shell(command):
        return Deny(f"pipe-to-shell execution is blocked: {command}")
    subs = split_compound(command)
    if not subs:
        return Deny("empty command")
    pending_ask: str | None = None
    for sub in subs:
        v = _decide_bash_subcommand(sub)
        if isinstance(v, Deny):
            return v
        if isinstance(v, Ask) and pending_ask is None:
            pending_ask = v.reason
    return Ask(pending_ask) if pending_ask is not None else Allow()


class Category(Enum):
    """Category a tool falls into, derived from its name."""

    BASH = "bash"
    NETWORK = "network"
    WRITE = "write"
    SAFE = "safe"
    UNKNOWN = "unknown"


def tool_category(name: str) -> Category:
    """Classify a tool by its (possibly dotted ``<ext>.<tool>``) name."""
    bare = name.rsplit(".", 1)[-1]
    n = bare.lower()
    if n in ("bash", "shell", "shell_exec", "run_command"):
        return Category.BASH
    if "write" in n or "edit" in n or "delete" in n or "remove" in n or n == "apply_patch" or n == "create_file":
        return Category.WRITE
    if "fetch" in n or "download" in n or n.startswith("http"):
        return Category.NETWORK
    if n.startswith("read") or n.startswith("list") or n.startswith("get") or "search" in n or n in ("grep", "glob"):
        return Category.SAFE
    return Category.UNKNOWN


def _get_str(args: Any, *keys: str) -> str | None:
    if not isinstance(args, dict):
        return None
    for k in keys:
        v = args.get(k)
        if isinstance(v, str):
            return v
    return None


def _decide_inner(tool_name: str, args: Any) -> Verdict:
    cat = tool_category(tool_name)
    if cat is Category.BASH:
        cmd = (_get_str(args, "cmd", "command") or "").strip()
        if not cmd:
            return Deny("bash call with no command")
        return _decide_bash(cmd)
    if cat is Category.SAFE:
        # Read-only is not exfil-proof: the read path IS the exfil path.
        for key in ("path", "file", "dir", "directory"):
            v = _get_str(args, key)
            if v is not None and _references_sensitive_path(v):
                return Deny(f"{tool_name} targets a sensitive credential path: {v}")
        return Allow()
    if cat is Category.NETWORK:
        url = _get_str(args, "url", "host") or ""
        host = host_from_token(url) or url
        if not host:
            return Deny(f"{tool_name} call with no url/host")
        if domain_matches_suffix_list(host, DANGEROUS_DOMAIN_SUFFIXES):
            return Deny(f"{host} is on the dangerous-domain deny list")
        return Ask(f"outbound request to {host} needs approval")
    if cat is Category.WRITE:
        path = _get_str(args, "path", "file") or ""
        if _references_sensitive_path(path):
            return Deny(f"write to a sensitive credential path: {path}")
        return Ask(f"`{tool_name}` mutates the filesystem")
    return Ask(f"`{tool_name}` is not a recognised safe tool")


def decide(mode: AutoMode, tool_name: str, args: Any) -> Verdict:
    """The pure, deterministic permission decision. No async, no I/O — the
    security-critical core, tested exhaustively.

    ``args`` is the raw tool-call argument object (a dict); the relevant field is
    pulled per category (``cmd`` for bash, ``path`` for writes, ``url``/``host``
    for network)."""
    raw = _decide_inner(tool_name, args)
    # A Deny always survives every mode (hard circuit-breaker).
    if isinstance(raw, Deny):
        return raw
    if mode is AutoMode.BYPASS:
        return Allow()
    if mode is AutoMode.ACCEPT_EDITS and isinstance(raw, Ask) and tool_category(tool_name) is Category.WRITE:
        return Allow()
    if mode is AutoMode.DENY_UNMATCHED and isinstance(raw, Ask):
        return Deny(f"headless (no interactive approver): {raw.reason}")
    return raw


# ---------------------------------------------------------------------------
# Grant derivation (pearl th-22bfc1) — map an ``Ask`` to a persistable grant and
# check whether a stored grant already covers it. Never derives from a ``Deny``.
# ---------------------------------------------------------------------------
def _bash_segment_grant(sub: str) -> GrantQuery:
    hosts = extract_hosts(sub)
    if hosts:
        return NetworkGrant(hosts[0])
    return BashGrant(f"{command_bin(sub) or ''} ")


def grant_query(tool_name: str, args: Any) -> Optional[GrantQuery]:
    """The grant that "approve always" would persist for this tool call. ``None``
    when the call is not an ``Ask`` (already allowed, or a non-grantable ``Deny``)."""
    cat = tool_category(tool_name)
    if cat is Category.BASH:
        cmd = (_get_str(args, "cmd", "command") or "").strip()
        for sub in split_compound(cmd):
            v = _decide_bash_subcommand(sub)
            if isinstance(v, Ask):
                return _bash_segment_grant(sub)
            if isinstance(v, Deny):
                return None  # a deny sinks the line; nothing grantable
        return None
    if cat is Category.NETWORK:
        url = _get_str(args, "url", "host") or ""
        host = host_from_token(url) or url
        return NetworkGrant(host) if host else None
    if cat in (Category.WRITE, Category.UNKNOWN):
        return ToolGrant(tool_name)
    return None  # Category.SAFE


def _bash_segment_granted(sub: str, grants: PermissionGrants) -> bool:
    hosts = extract_hosts(sub)
    if hosts:
        return grants.matches_host(hosts[0])
    return grants.matches_bash(sub)


def covered_by_grants(grants: PermissionGrants, tool_name: str, args: Any) -> bool:
    """Is this whole tool call already covered by stored grants — so the ``Ask`` can
    be auto-approved without prompting? For compound bash, **every** asking segment
    must be granted."""
    cat = tool_category(tool_name)
    if cat is Category.BASH:
        cmd = (_get_str(args, "cmd", "command") or "").strip()
        subs = split_compound(cmd)
        if not subs:
            return False
        for sub in subs:
            v = _decide_bash_subcommand(sub)
            if isinstance(v, Deny):
                return False  # never auto-allow a deny
            if isinstance(v, Ask) and not _bash_segment_granted(sub, grants):
                return False
        return True
    if cat is Category.NETWORK:
        url = _get_str(args, "url", "host") or ""
        host = host_from_token(url) or url
        return bool(host) and grants.matches_host(host)
    if cat in (Category.WRITE, Category.UNKNOWN):
        return grants.matches_tool(tool_name)
    return False  # Category.SAFE


# ---------------------------------------------------------------------------
# The hook
# ---------------------------------------------------------------------------
class _Approval(Enum):
    ONCE = "once"
    ALWAYS = "always"


@dataclass
class _Approver:
    """Routes an ``Ask`` verdict to a human via a :class:`~.human_gate.HumanGate`.
    Fails closed (raises) on denial, timeout, or a raising gate. A ``Deny`` is a
    hard circuit-breaker and is **never** routed here."""

    gate: HumanGate
    timeout: float | None = None

    async def request(self, call: ToolCall, reason: str) -> _Approval:
        request = HumanApprovalRequest(
            tool_name=call.name,
            arguments=call.arguments,
            prompt=f"Permission: {reason}. Allow `{call.name}`?",
        )
        try:
            if self.timeout is not None:
                resp = await asyncio.wait_for(self.gate.request_approval(request), self.timeout)
            else:
                resp = await self.gate.request_approval(request)
        except asyncio.TimeoutError as e:
            raise PermissionError("permission approval timed out; failing closed") from e
        if resp.decision is HumanDecision.DENIED:
            raise PermissionError(f"user denied: {resp.reason or 'no reason given'}")
        if resp.decision is HumanDecision.APPROVED_ALWAYS:
            return _Approval.ALWAYS
        return _Approval.ONCE


class PermissionHook(ToolHook):
    """A :class:`~.hooks.ToolHook` that enforces :func:`decide` on every tool call.
    Add it FIRST on the agent's ``tool_hooks`` so it gates before other hooks (e.g.
    Narc) and before the tool executes.

    **Ask routing**: with an approver wired via :meth:`with_approver`, an ``Ask``
    prompts a human and blocks until they approve; with no approver it **fails
    closed** (raises). :attr:`AutoMode.BYPASS` / :attr:`AutoMode.ACCEPT_EDITS`
    downgrade eligible asks to allow inside :func:`decide` before they reach the
    approver. A ``Deny`` always blocks and is never routed to the human.
    """

    def __init__(self, mode: AutoMode = AutoMode.ASK) -> None:
        self._mode = mode
        self._approver: _Approver | None = None
        self._grants: SharedGrants | None = None
        self._persist_path: Path | None = None
        self._deny_policy: "DenyPolicy | None" = None

    @staticmethod
    def from_env() -> "PermissionHook":
        """Build a hook reading the mode from ``SMOOTH_AUTO_MODE`` (default ASK)."""
        return PermissionHook(AutoMode.from_env())

    @property
    def mode(self) -> AutoMode:
        return self._mode

    def with_approver(self, gate: HumanGate, timeout: float | None = None) -> "PermissionHook":
        """Wire an interactive approver. When set, an ``Ask`` verdict consults the
        gate and blocks (up to ``timeout`` seconds) on the human's answer — approve
        lets the call run, anything else (deny / timeout / raise) blocks it."""
        self._approver = _Approver(gate, timeout)
        return self

    def with_grants(self, grants: SharedGrants, persist_path: Path) -> "PermissionHook":
        """Wire the persistent allow-list (pearl th-22bfc1). ``grants`` is the live
        merged view consulted on every ``Ask`` *before* prompting — a matching grant
        auto-approves silently. ``persist_path`` is where an approve-always answer
        writes the new grant; after writing it is merged back so the very next
        identical ``Ask`` is silent too."""
        self._grants = grants
        self._persist_path = persist_path
        return self

    def with_deny_policy(self, policy: "DenyPolicy") -> "PermissionHook":
        """Attach a consumer-supplied deny policy (pearl th-deny-policy). Purely
        additive. When set, the policy is evaluated **first** in :meth:`pre_call` —
        a policy match is a hard deny (circuit-breaker tier) that no stored grant
        waives and that :attr:`AutoMode.BYPASS` / :attr:`AutoMode.ACCEPT_EDITS`
        cannot downgrade."""
        self._deny_policy = policy
        return self

    def _persist_grant(self, call: ToolCall) -> None:
        """Persist an approve-always grant to disk and merge it into the live view.
        Best-effort: a persistence failure is logged, not fatal."""
        if self._grants is None or self._persist_path is None:
            return
        query = grant_query(call.name, call.arguments)
        if query is None:
            return
        try:
            append_grant(self._persist_path, query)
            fresh = PermissionGrants.new()
            fresh.add(query)
            self._grants.merge_in(fresh)
        except Exception as e:  # noqa: BLE001 — best-effort persistence, never fatal
            logger.warning("failed to persist permission grant to %s: %s", self._persist_path, e)

    async def pre_call(self, call: ToolCall) -> None:
        # Deny policy runs FIRST — a consumer-supplied deny is a circuit-breaker that
        # wins over grants, ask, allow, and every mode (Bypass included). Never routed
        # to a human, never grantable.
        if self._deny_policy is not None:
            reason = self._deny_policy.evaluate(call)
            if reason is not None:
                raise PermissionError(f"permission denied: {reason}")

        verdict = decide(self._mode, call.name, call.arguments)
        if isinstance(verdict, Allow):
            return
        if isinstance(verdict, Deny):
            # A circuit-breaker — never routed to a human, never grantable.
            raise PermissionError(f"permission denied: {verdict.reason}")

        # Ask: consult the persisted allow-list FIRST — a stored grant auto-approves
        # silently (no prompt).
        if self._grants is not None and covered_by_grants(self._grants.snapshot(), call.name, call.arguments):
            return
        if self._approver is None:
            # Fail closed: no interactive approver wired.
            raise PermissionError(f"permission requires approval (fail-closed, no approver): {verdict.reason}")
        approval = await self._approver.request(call, verdict.reason)
        if approval is _Approval.ALWAYS:
            self._persist_grant(call)
        # approved → allow (fall through)
