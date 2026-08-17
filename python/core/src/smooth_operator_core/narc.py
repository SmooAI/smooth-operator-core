"""Native secret-detection + prompt-injection scanning :class:`~smooth_operator_core.hooks.ToolHook`
— the Python port of the Rust reference engine's ``narc.rs`` (pearl th-5f7227).

The SEP extension host passes tool-call arguments to the extension subprocess
**unscanned** and returns the subprocess's tool-result content to the model
**verbatim**. Nothing at the extension boundary looks for leaked credentials or
prompt-injection payloads. :class:`NarcHook` closes that gap. It scans two things:

- **Secrets** — 10 credential patterns (AWS keys, private keys, JWTs/bearer
  tokens, high-entropy provider keys, …).
- **Prompt injection** — 8 patterns (instruction override, role hijack,
  jailbreak, data/URL exfiltration, …).

Division of labour with :class:`~smooth_operator_core.permission.PermissionHook`:
the permission gate already owns the *dangerous-command* / *write* /
*credential-path* circuit-breakers (``rm -rf /``, ``curl | sh``,
``~/.ssh/id_rsa``). Narc does **not** re-implement those — it is scoped to the one
thing permission does not do: **content scanning of arguments and results** for
secrets and injection. Install Narc *after* the permission hook so the
allow/ask/deny decision happens first and Narc scans the calls that clear it.

``pre_call`` (arguments) — blocks on exfiltration, alerts otherwise. A
:attr:`Severity.BLOCK` injection match (the active data/URL exfiltration signals)
raises, blocking the call before the tool runs. Lower-severity injection and any
secret in the arguments are **alerted, not blocked** — a tool argument
legitimately carrying a secret (writing a ``.env``, configuring a client) is
common enough that a hard block there would be a footgun.

``post_call`` (result) — detects, alerts, and **redacts** secrets. ``post_call``
receives a **mutable** :class:`~smooth_operator_core.hooks.ToolResult`, so a
rewrite of ``result.content`` is what downstream consumers — and the
LLM/conversation — actually see. A secret pattern in a tool result raises a
:attr:`Severity.BLOCK` alert *and* replaces the matched credential with
``[REDACTED:<pattern-name>]`` before it reaches the model. Injection patterns in
the result remain detection + :attr:`Severity.ALERT` only (surveillance) — they
can appear in legitimate content and are not rewritten.

The detection set is pinned across all five engines by the shared corpus at
``spec/narc/corpus.json`` (see ``tests/test_narc.py``).
"""

from __future__ import annotations

import json
import logging
import re
from collections.abc import Callable
from dataclasses import dataclass
from enum import IntEnum

from .hooks import ToolCall, ToolHook, ToolResult

logger = logging.getLogger(__name__)


class Severity(IntEnum):
    """Severity of a Narc finding, ordered least → most severe. A
    :attr:`BLOCK` finding in ``pre_call`` blocks the tool call. An ``IntEnum`` so
    the ordering is the comparison."""

    #: Informational — no action.
    INFO = 0
    #: Suspicious but plausibly legitimate (e.g. a secret in an argument).
    WARN = 1
    #: Strong signal worth surfacing, but not auto-blocked.
    ALERT = 2
    #: Actively harmful — blocks the call when raised in ``pre_call``.
    BLOCK = 3

    def __str__(self) -> str:
        """The shared wire label (``INFO``/``WARN``/``ALERT``/``BLOCK``)."""
        return self.name


@dataclass
class NarcAlert:
    """A single surveillance finding. Lean by design — the log record supplies the
    timestamp and correlation, so no uuid/timestamp fields are carried."""

    #: How severe the finding is.
    severity: Severity
    #: Coarse bucket: ``"injection"``, ``"secret"``, ``"secret_leak"``, ``"injection_output"``.
    category: str
    #: The named pattern that matched.
    pattern_name: str
    #: Redacted view of the matched text (never the raw secret).
    redacted: str
    #: The tool whose args/result triggered the finding.
    tool_name: str


@dataclass
class NarcFinding:
    """A pattern match: which pattern, its severity, and a redacted view."""

    #: The named pattern that matched.
    pattern_name: str
    #: The finding's severity.
    severity: Severity
    #: Redacted view of the matched text (safe to log).
    redacted: str


@dataclass(frozen=True)
class _NamedPattern:
    name: str
    severity: Severity
    regex: re.Pattern[str]


#: The 10 secret patterns. All are :attr:`Severity.WARN` in arguments (may be
#: legit) and escalate to :attr:`Severity.BLOCK` when found in a result (a leak) —
#: the caller decides which threshold to apply.
_SECRET_PATTERNS: list[_NamedPattern] = [
    _NamedPattern("AWS Access Key", Severity.WARN, re.compile(r"AKIA[0-9A-Z]{16}")),
    _NamedPattern(
        "AWS Secret Key",
        Severity.WARN,
        re.compile(r"(?i)aws[_\-]?secret[_\-]?access[_\-]?key\s*[=:]\s*[A-Za-z0-9/+=]{40}"),
    ),
    _NamedPattern("Anthropic API Key", Severity.WARN, re.compile(r"sk-ant-[A-Za-z0-9\-_]{20,}")),
    _NamedPattern("OpenAI API Key", Severity.WARN, re.compile(r"sk-[A-Za-z0-9]{20,}")),
    _NamedPattern("GitHub Token", Severity.WARN, re.compile(r"gh[posr]_[A-Za-z0-9_]{36,}")),
    _NamedPattern("Private Key", Severity.WARN, re.compile(r"-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----")),
    _NamedPattern(
        "Generic Secret",
        Severity.WARN,
        re.compile(r"""(?i)(secret|password|token|api[_\-]?key)\s*[=:]\s*["']?[A-Za-z0-9/+=\-_]{8,}"""),
    ),
    _NamedPattern("Bearer Token", Severity.WARN, re.compile(r"Bearer\s+[A-Za-z0-9\-_.~+/]+=*")),
    _NamedPattern(
        "Base64 Encoded Key",
        Severity.WARN,
        re.compile(r"(?i)(key|secret|password)\s*[=:]\s*[A-Za-z0-9+/]{32,}={0,2}"),
    ),
    _NamedPattern("Stripe Key", Severity.WARN, re.compile(r"[sr]k_(live|test)_[A-Za-z0-9]{20,}")),
]

#: The 8 injection patterns. Only the active data/URL exfiltration signals are
#: :attr:`Severity.BLOCK` (blocked in arguments); hijack/jailbreak text is
#: :attr:`Severity.ALERT` (surveilled, not blocked — it can appear in legitimate
#: content the model is authoring, e.g. a security test or documentation about
#: injection).
_INJECTION_PATTERNS: list[_NamedPattern] = [
    _NamedPattern(
        "ignore_instructions",
        Severity.ALERT,
        re.compile(r"(?i)ignore\s+(all\s+)?(previous|prior|above)\s+(instructions|prompts|rules)"),
    ),
    _NamedPattern(
        "role_hijack",
        Severity.ALERT,
        re.compile(r"(?i)(you\s+are\s+now|act\s+as|pretend\s+(to\s+be|you\s+are)|from\s+now\s+on\s+you\s+are)"),
    ),
    _NamedPattern("system_prompt", Severity.ALERT, re.compile(r"(?i)(system\s*:\s*|<\|system\|>|\[SYSTEM\])")),
    _NamedPattern(
        "jailbreak",
        Severity.ALERT,
        re.compile(r"(?i)(DAN\s+mode|developer\s+mode|do\s+anything\s+now|jailbreak)"),
    ),
    _NamedPattern(
        "base64_smuggling",
        Severity.ALERT,
        re.compile(r"(?i)(decode|eval|execute)\s+(this\s+)?(base64|encoded)"),
    ),
    _NamedPattern(
        "data_exfiltration",
        Severity.BLOCK,
        re.compile(
            r"""(?ix)
            (send|post|upload|exfiltrate|transmit|leak|push)
            \s+
            (all\s+|the\s+|our\s+|my\s+|this\s+)*
            (
                data|files?|secrets?|credentials?|keys?|tokens?|
                contents?|env\s+(vars?|file)|
                package\.json|\.env|pyproject\.toml|cargo\.toml|
                requirements\.txt|gemfile|go\.mod|composer\.json|
                \.ssh/[a-z_]+|id_rsa|\.aws/[a-z]+|\.gnupg/
            )
            \s+(to|via|at|over)
            """
        ),
    ),
    _NamedPattern(
        "url_exfiltration",
        Severity.BLOCK,
        re.compile(
            r"(?i)(send|post|upload|push|transmit|leak|exfiltrate)\b[^.\n]{1,200}\s+(to|via|at|over)\s+(https?://[\w.\-/]+)"
        ),
    ),
    _NamedPattern(
        "smell_url",
        Severity.ALERT,
        re.compile(r"(?i)https?://[\w.\-]*\b(leak|exfil|attacker|evil|tracker|c2(?:server)?|webhook\.site)\b[\w.\-/]*"),
    ),
]


def _scan(patterns: list[_NamedPattern], text: str) -> list[NarcFinding]:
    out: list[NarcFinding] = []
    for p in patterns:
        for m in p.regex.finditer(text):
            out.append(NarcFinding(pattern_name=p.name, severity=p.severity, redacted=redact_match(m.group(0))))
    return out


def scan_secrets(text: str) -> list[NarcFinding]:
    """Scan ``text`` for hardcoded secrets. Every match is redacted."""
    return _scan(_SECRET_PATTERNS, text)


def scan_injection(text: str) -> list[NarcFinding]:
    """Scan ``text`` for prompt-injection patterns. Matched text is redacted."""
    return _scan(_INJECTION_PATTERNS, text)


def has_secrets(text: str) -> bool:
    """True if ``text`` contains any secret pattern."""
    return any(p.regex.search(text) for p in _SECRET_PATTERNS)


def has_injection(text: str) -> bool:
    """True if ``text`` contains any injection pattern."""
    return any(p.regex.search(text) for p in _INJECTION_PATTERNS)


def _literal(text: str) -> Callable[[re.Match[str]], str]:
    """A ``re.sub`` replacement that is taken literally, never as a template."""
    return lambda _m: text


def redact_match(s: str) -> str:
    """Redact a matched string, showing only the first 4 and last 2 characters.
    Short matches (≤ 8 characters) are fully starred."""
    n = len(s)
    if n <= 8:
        return "*" * n
    return f"{s[:4]}{'*' * (n - 6)}**{s[-2:]}"


class NarcHook(ToolHook):
    """A :class:`~smooth_operator_core.hooks.ToolHook` that scans tool-call
    arguments and results for secrets and prompt injection. Install it via
    ``AgentOptions.tool_hooks`` alongside
    :class:`~smooth_operator_core.permission.PermissionHook`, *after* it.

    - ``pre_call`` raises on a :attr:`Severity.BLOCK` injection pattern in the
      arguments (active exfiltration); every other finding (lower-severity
      injection, any secret) is recorded as a :class:`NarcAlert` and logged, not
      blocked.
    - ``post_call`` detects secrets/injection in the result, records + logs them,
      and **redacts** leaked secrets out of the content in place so the model
      never sees the raw credential.
    """

    def __init__(self) -> None:
        self._alerts: list[NarcAlert] = []

    def alerts(self) -> list[NarcAlert]:
        """Snapshot every recorded alert."""
        return list(self._alerts)

    def alerts_above(self, min_severity: Severity) -> list[NarcAlert]:
        """Recorded alerts at or above ``min_severity``."""
        return [a for a in self._alerts if a.severity >= min_severity]

    def _record(self, alert: NarcAlert) -> None:
        level = (
            logging.ERROR
            if alert.severity is Severity.BLOCK
            else logging.WARNING
            if alert.severity is Severity.ALERT
            else logging.INFO
        )
        logger.log(
            level,
            "narc: %s finding tool=%s category=%s pattern=%s redacted=%s",
            alert.severity,
            alert.tool_name,
            alert.category,
            alert.pattern_name,
            alert.redacted,
        )
        self._alerts.append(alert)

    async def pre_call(self, call: ToolCall) -> None:
        """Scan the arguments. A Block-severity injection pattern raises (the tool
        never runs); everything else is recorded and allowed."""
        args_text = json.dumps(call.arguments)

        # Scan all first so every finding is recorded even when one of them blocks.
        block: NarcFinding | None = None
        for f in scan_injection(args_text):
            if f.severity >= Severity.BLOCK and block is None:
                block = f
            self._record(NarcAlert(f.severity, "injection", f.pattern_name, f.redacted, call.name))

        # Secrets in arguments: alert only (may be legitimate).
        for f in scan_secrets(args_text):
            self._record(NarcAlert(f.severity, "secret", f.pattern_name, f.redacted, call.name))

        if block is not None:
            raise ValueError(f"prompt-injection pattern `{block.pattern_name}` in tool arguments — blocked")

    async def post_call(self, call: ToolCall, result: ToolResult) -> None:
        """Scan the result. A secret in a result is a leak: it raises a Block alert
        AND is redacted out of ``result.content`` in place. Injection in the result
        is surveillance only — recorded, never rewritten."""
        secrets = scan_secrets(result.content)
        for f in secrets:
            self._record(NarcAlert(Severity.BLOCK, "secret_leak", f.pattern_name, f.redacted, call.name))
        if secrets:
            content = result.content
            for p in _SECRET_PATTERNS:
                # Replace via a callable so backslash/group escapes in a pattern
                # name could never be interpreted as a `\g<…>` template.
                content = p.regex.sub(_literal(f"[REDACTED:{p.name}]"), content)
            result.content = content
        # Scan the POST-redaction content — what the model will actually see.
        for f in scan_injection(result.content):
            self._record(
                NarcAlert(max(f.severity, Severity.ALERT), "injection_output", f.pattern_name, f.redacted, call.name)
            )
