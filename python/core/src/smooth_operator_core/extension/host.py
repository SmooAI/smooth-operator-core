"""``ExtensionHost`` — orchestrates the loaded extensions: hook chaining in load
order, non-blocking event fanout, tool proxies, and the ext->host delegate seam.

Python (asyncio) sibling of the Rust reference ``host.rs``. The security-critical
part is :func:`fold_hook_chain`: how per-extension hook outcomes combine, and what
happens on timeout/crash. It is a pure function so it can be tested exhaustively
against adversarial inputs without spawning anything.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Any, Optional

from .manifest import DiscoveredExtension, Scope, default_global_dir
from .process import ExtensionProcess, InboundHandler, SpawnSpec
from .protocol import (
    Completion,
    Context,
    HookOutcome,
    HostInfo,
    InitializeParams,
    InitializeResult,
    RpcError,
    Tier,
    WorkspaceInfo,
    codes,
    method,
)
from .tool_proxy import ExtensionTool

_log = logging.getLogger("smooth_operator_core.extension")

#: The SEP protocol version this host implements.
PROTOCOL_VERSION = 1


class HookType(Enum):
    """Classifies a hook by its failure policy and default timeout."""

    TOOL_CALL = "tool_call"
    USER_BASH = "user_bash"
    TOOL_RESULT = "tool_result"
    INPUT = "input"
    BEFORE_AGENT_START = "before_agent_start"
    CONTEXT = "context"
    BEFORE_PROVIDER_REQUEST = "before_provider_request"
    MESSAGE_END = "message_end"
    SESSION_BEFORE_COMPACT = "session_before_compact"
    SESSION_BEFORE_TREE = "session_before_tree"

    def as_str(self) -> str:
        return self.value

    @classmethod
    def from_name(cls, name: str) -> Optional[HookType]:
        try:
            return cls(name)
        except ValueError:
            return None

    def fail_closed(self) -> bool:
        """Fail-closed hooks (``tool_call``, ``user_bash``) block the operation when an
        extension times out or crashes. Everything else fails open (proceeds)."""
        return self in (HookType.TOOL_CALL, HookType.USER_BASH)

    def default_timeout(self) -> float:
        """Default hook timeout (seconds): 60s for fail-closed (they gate execution),
        5s for fail-open. Manifest ``hook_timeout_ms`` overrides this."""
        return 60.0 if self.fail_closed() else 5.0


@dataclass
class HookStep:
    """One extension's reply within a hook chain, as seen by the fold. ``outcome``
    is ``None`` when the extension timed out or crashed."""

    outcome: Optional[HookOutcome]

    @classmethod
    def replied(cls, outcome: HookOutcome) -> HookStep:
        return cls(outcome)

    @classmethod
    def failed(cls) -> HookStep:
        return cls(None)

    @property
    def is_failed(self) -> bool:
        return self.outcome is None


@dataclass
class FoldedHook:
    """The folded result of a whole hook chain: proceed with a (possibly modified)
    value, or blocked with a reason."""

    blocked: bool
    value: Any = None
    reason: str = ""

    @classmethod
    def proceed(cls, value: Any) -> FoldedHook:
        return cls(blocked=False, value=value)

    @classmethod
    def block(cls, reason: str) -> FoldedHook:
        return cls(blocked=True, reason=reason)


def fold_hook_chain(hook: HookType, input_value: Any, steps: list[HookStep]) -> FoldedHook:
    """Fold a hook chain over ``input_value``, in load order. ``steps`` are the
    per-extension results in that order. The security-critical policy:

    - ``continue`` -> value unchanged, next extension sees it.
    - ``modify`` -> value replaced by the patch, next extension sees the patch.
    - ``block`` -> short-circuit; the operation is vetoed (honored for every hook).
    - failed -> for a fail-closed hook, block; for a fail-open hook, proceed
      unchanged.
    """
    current = input_value
    for step in steps:
        if step.is_failed:
            if hook.fail_closed():
                return FoldedHook.block(f"{hook.as_str()} hook failed (fail-closed)")
            continue  # fail-open: proceed with the current value.
        outcome = step.outcome
        assert outcome is not None
        if outcome.action == "continue":
            pass
        elif outcome.action == "modify":
            current = outcome.patch
        elif outcome.action == "block":
            return FoldedHook.block(outcome.reason or f"blocked by {hook.as_str()} hook")
    return FoldedHook.proceed(current)


def effective_subscriptions(declared: list[str], requested: list[str]) -> set[str]:
    """Effective event subscriptions: what the extension asked for at handshake,
    clamped to what its manifest ``[capabilities] events`` declared. An empty declared
    list means "no declared filter" -> trust the handshake as-is; a non-empty list is
    the outer bound the extension can never widen past."""
    if not declared:
        return set(requested)
    allowed = set(declared)
    return {s for s in requested if s in allowed}


def _token_epoch(token: str) -> Optional[int]:
    """Parse the epoch embedded in a context token (``epoch-<N>``). ``None`` for a
    malformed token."""
    if not token.startswith("epoch-"):
        return None
    try:
        return int(token[len("epoch-") :])
    except ValueError:
        return None


def validate_command_context(params: Any, current_epoch: int) -> None:
    """The two-tier deadlock guard: a session-mutating ext->host action is valid only
    when it presents a COMMAND-tier context whose epoch is still current. An event-tier
    context, or a stale token minted before a reload bumped the epoch, raises
    ``-32003 ContextViolation``. Kept a pure function so it can be tested
    exhaustively."""
    ctx = params.get("context") if isinstance(params, dict) else None
    tier = ctx.get("tier") if isinstance(ctx, dict) else None
    if tier != "command":
        raise RpcError(codes.CONTEXT_VIOLATION, "session action requires a command-tier context")
    token = (ctx.get("token") if isinstance(ctx, dict) else None) or ""
    epoch = _token_epoch(token)
    if epoch != current_epoch:
        raise RpcError(codes.CONTEXT_VIOLATION, "session action presented a stale context (epoch mismatch)")


# ---------------------------------------------------------------------------
# Host delegate: the ext->host seam (ui / kv / exec / session / trust).
# ---------------------------------------------------------------------------


class HostDelegate:
    """The host's side of ext->host requests. The engine ships headless defaults;
    frontends (the daemon, the servers) supply richer impls by overriding methods."""

    async def ui_request(self, ext: str, params: Any) -> Any:
        """Answer a ``ui/request``. Headless default: no UI available."""
        raise RpcError(codes.NO_UI, "no UI available (headless host)")

    async def kv_get(self, ext: str, key: str) -> Any:
        map_ = _kv_file_load(ext)
        return map_.get(key)

    async def kv_set(self, ext: str, key: str, value: Any) -> None:
        map_ = _kv_file_load(ext)
        map_[key] = value
        _kv_file_store(ext, map_)

    async def exec_run(self, ext: str, params: Any) -> Any:
        """``exec/run``. Headless default: deny (no audited permission engine here)."""
        raise RpcError(codes.NOT_TRUSTED, "exec/run is not permitted on the headless host")

    async def session_send_message(self, ext: str, params: Any) -> Any:
        raise RpcError(codes.CAPABILITY_DISABLED, "session actions are unavailable on this host")

    async def session_send_user_message(self, ext: str, params: Any) -> Any:
        raise RpcError(codes.CAPABILITY_DISABLED, "session actions are unavailable on this host")

    async def session_append_entry(self, ext: str, params: Any) -> Any:
        raise RpcError(codes.CAPABILITY_DISABLED, "session actions are unavailable on this host")

    def tool_update(self, ext: str, params: Any) -> None:
        """A ``tool/update`` progress notification streamed by an extension. Fire-and-
        forget. The headless default only traces; a frontend/daemon overrides this to
        surface progress."""
        _log.debug("extension %s: tool/update progress (dropped by headless host): %s", ext, params)


class DefaultHostDelegate(HostDelegate):
    """The engine's headless delegate: NoUI, JSON-file kv, exec denied."""


def _kv_file_path(ext: str) -> Optional[Path]:
    d = default_global_dir()
    return d / ext / "state.json" if d is not None else None


def _kv_file_load(ext: str) -> dict[str, Any]:
    path = _kv_file_path(ext)
    if path is None:
        return {}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}


def _kv_file_store(ext: str, map_: dict[str, Any]) -> None:
    path = _kv_file_path(ext)
    if path is None:
        raise RpcError(codes.INTERNAL_ERROR, "no home dir for kv store")
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(map_, indent=2), encoding="utf-8")
    except OSError as exc:
        raise RpcError(codes.INTERNAL_ERROR, f"kv write: {exc}") from exc


class HostInbound(InboundHandler):
    """Bridges the process reader's ext->host requests to the :class:`HostDelegate`.
    Reads the host's shared epoch so it can reject stale/event-tier session actions."""

    def __init__(self, ext: str, delegate: HostDelegate, host: ExtensionHost) -> None:
        self._ext = ext
        self._delegate = delegate
        self._host = host

    async def handle_request(self, method_name: str, params: Any) -> Any:
        if method_name == method.PING:
            return {}
        if method_name == method.UI_REQUEST:
            return await self._delegate.ui_request(self._ext, params)
        if method_name == method.EXEC_RUN:
            return await self._delegate.exec_run(self._ext, params)
        # Session actions are the tier-guarded set: validate the presented context
        # (command tier + current epoch) BEFORE touching the delegate.
        if method_name == method.SESSION_SEND_MESSAGE:
            validate_command_context(params, self._host.epoch)
            return await self._delegate.session_send_message(self._ext, params)
        if method_name == method.SESSION_SEND_USER_MESSAGE:
            validate_command_context(params, self._host.epoch)
            return await self._delegate.session_send_user_message(self._ext, params)
        if method_name == method.SESSION_APPEND_ENTRY:
            validate_command_context(params, self._host.epoch)
            return await self._delegate.session_append_entry(self._ext, params)
        if method_name == "kv/get":
            key = params.get("key", "") if isinstance(params, dict) else ""
            return {"value": await self._delegate.kv_get(self._ext, key)}
        if method_name == "kv/set":
            key = params.get("key", "") if isinstance(params, dict) else ""
            value = params.get("value") if isinstance(params, dict) else None
            await self._delegate.kv_set(self._ext, key, value)
            return {}
        raise RpcError(codes.METHOD_NOT_FOUND, f"method not found: {method_name}")

    def handle_notification(self, method_name: str, params: Any) -> None:
        if method_name == method.TOOL_UPDATE:
            self._delegate.tool_update(self._ext, params)


# ---------------------------------------------------------------------------
# ExtensionHost
# ---------------------------------------------------------------------------


@dataclass
class _Loaded:
    """A loaded, initialized extension. ``init`` and ``subscriptions`` are mutated in
    place by a hot reload without disturbing the stable ``process``."""

    name: str
    process: ExtensionProcess
    init: InitializeResult
    subscriptions: set[str]
    declared_events: list[str]
    hook_timeout: Optional[float]


class ExtensionHost:
    """Orchestrates the set of loaded extensions in load order."""

    def __init__(
        self,
        host: HostInfo,
        workspace: WorkspaceInfo,
        mode: str,
        ui_capabilities: list[str],
    ) -> None:
        self._extensions: list[_Loaded] = []
        self.epoch = 1
        self._host = host
        self._workspace = workspace
        self._mode = mode
        self._ui_capabilities = ui_capabilities

    @classmethod
    async def load(
        cls,
        discovered: list[DiscoveredExtension],
        host: HostInfo,
        workspace: WorkspaceInfo,
        mode: str,
        ui_capabilities: list[str],
        delegate: HostDelegate,
    ) -> tuple[ExtensionHost, list[tuple[str, str]]]:
        """Load and initialize each discovered extension. Per-extension failures
        (spawn, handshake) are tolerated and returned alongside the host. In an
        untrusted workspace, project-scoped extensions are skipped."""
        self = cls(host, workspace, mode, ui_capabilities)
        failures: list[tuple[str, str]] = []
        for ext in discovered:
            name = ext.manifest.name
            if ext.manifest.disabled:
                continue
            if ext.scope == Scope.PROJECT and not workspace.trusted:
                _log.info("extension: skipping project extension %s in untrusted workspace", name)
                continue
            try:
                loaded = await self._load_one(ext, delegate)
                self._extensions.append(loaded)
            except Exception as exc:  # noqa: BLE001 — one bad extension must not fail the rest
                _log.warning("extension: failed to load %s: %s", name, exc)
                failures.append((name, str(exc)))
        return self, failures

    async def _load_one(self, ext: DiscoveredExtension, delegate: HostDelegate) -> _Loaded:
        spec = SpawnSpec(
            command=ext.manifest.run.command,
            args=list(ext.manifest.run.args),
            env=ext.manifest.resolved_env(),
            cwd=ext.root,
        )
        handler = HostInbound(ext.manifest.name, delegate, self)
        process = await ExtensionProcess.spawn(spec, handler)
        init = await self._initialize(process)
        subscriptions = effective_subscriptions(ext.manifest.capabilities.events, init.registrations.subscriptions)
        hook_timeout = ext.manifest.hook_timeout_ms / 1000.0 if ext.manifest.hook_timeout_ms is not None else None
        return _Loaded(
            name=ext.manifest.name,
            process=process,
            init=init,
            subscriptions=subscriptions,
            declared_events=list(ext.manifest.capabilities.events),
            hook_timeout=hook_timeout,
        )

    async def _initialize(self, process: ExtensionProcess) -> InitializeResult:
        """Send the ``initialize`` handshake and parse the registrations. Shared by
        initial load and hot reload."""
        params = InitializeParams(
            protocol_version=PROTOCOL_VERSION,
            host=self._host,
            workspace=self._workspace,
            mode=self._mode,
            ui_capabilities=list(self._ui_capabilities),
        )
        raw = await process.request(method.INITIALIZE, params.to_dict(), 10.0)
        if not isinstance(raw, dict):
            raise RuntimeError("bad initialize result: not an object")
        return InitializeResult.from_dict(raw)

    def __len__(self) -> int:
        return len(self._extensions)

    def is_empty(self) -> bool:
        return not self._extensions

    def names(self) -> list[str]:
        """Names of loaded extensions, in load order."""
        return [e.name for e in self._extensions]

    def context(self, tier: Tier) -> Context:
        """A fresh dispatch context. Session-mutating actions need ``Tier.COMMAND``.
        The token embeds the current epoch so it is invalidated across reloads."""
        return Context(token=f"epoch-{self.epoch}", tier=tier)

    def bump_epoch(self) -> None:
        """Bump the epoch, invalidating every previously minted context token."""
        self.epoch += 1

    def has_subscriber(self, event: str) -> bool:
        """True if any loaded extension subscribed to ``event``."""
        return any(event in e.subscriptions for e in self._extensions)

    def dispatch_event(self, event: str, payload: Any) -> None:
        """Fire-and-forget event fanout to every subscribed extension. Non-blocking: a
        slow or dead extension never stalls the caller (it sheds on the bounded observe
        lane)."""
        if not self._extensions:
            return
        ctx = self.context(Tier.EVENT).to_dict()
        for ext in self._extensions:
            if event not in ext.subscriptions:
                continue
            ext.process.send_event(event, ctx, payload)

    async def run_hook(self, hook: HookType, input_value: Any) -> FoldedHook:
        """Run a hook across every extension in load order, folding the chain. Each
        extension sees the prior extension's patch. Fail-open/closed per
        :class:`HookType`."""
        if not self._extensions:
            return FoldedHook.proceed(input_value)
        ctx = self.context(Tier.COMMAND).to_dict()
        current = input_value
        for ext in self._extensions:
            params = {"hook": hook.as_str(), "context": ctx, "input": current}
            timeout = ext.hook_timeout if ext.hook_timeout is not None else hook.default_timeout()
            try:
                raw = await ext.process.request(method.HOOK, params, timeout)
                try:
                    outcome = HookOutcome.from_dict(raw)
                    step = HookStep.replied(outcome)
                except (ValueError, TypeError, AttributeError) as exc:
                    _log.warning("extension %s: malformed hook outcome; treating as failure (%s)", ext.name, exc)
                    step = HookStep.failed()
            except (RpcError, RuntimeError) as exc:
                _log.warning("extension %s: hook call failed (%s)", ext.name, exc)
                step = HookStep.failed()

            folded = fold_hook_chain(hook, current, [step])
            if folded.blocked:
                return folded
            current = folded.value
        return FoldedHook.proceed(current)

    async def run_tool_call_hook(self, tool: str, arguments: Any) -> FoldedHook:
        """Convenience: run the ``tool_call`` hook (fail-closed) on a pending call."""
        return await self.run_hook(HookType.TOOL_CALL, {"tool": tool, "arguments": arguments})

    async def before_agent_start(self, system_prompt: str) -> str:
        """Run the ``before_agent_start`` hook on a system prompt, returning the
        possibly-rewritten prompt. Fail-open: a blocked/failed hook leaves the prompt
        unchanged."""
        if not self._extensions:
            return system_prompt
        folded = await self.run_hook(HookType.BEFORE_AGENT_START, {"system_prompt": system_prompt})
        if folded.blocked:
            return system_prompt
        value = folded.value
        if isinstance(value, dict) and isinstance(value.get("system_prompt"), str):
            return value["system_prompt"]
        return system_prompt

    def tools(self) -> list[ExtensionTool]:
        """Tool proxies for every eager tool every extension registered. Names are
        dotted ``<ext>.<tool>``. Deferred tools are returned by :meth:`deferred_tools`."""
        return self._collect_tools(deferred=False)

    def deferred_tools(self) -> list[ExtensionTool]:
        """Deferred tool proxies."""
        return self._collect_tools(deferred=True)

    def _collect_tools(self, deferred: bool) -> list[ExtensionTool]:
        ctx = self.context(Tier.COMMAND)
        out: list[ExtensionTool] = []
        for ext in self._extensions:
            for reg in ext.init.registrations.tools:
                if reg.deferred != deferred:
                    continue
                out.append(ExtensionTool(ext.name, reg, ext.process, ctx))
        return out

    def tools_for(self, ext_name: str) -> list[ExtensionTool]:
        """Eager tool proxies for a single extension, minted at the CURRENT epoch. The
        frontend calls this after a :meth:`reload` to re-register the reloaded
        extension's tools (its old proxies carry a stale context)."""
        ctx = self.context(Tier.COMMAND)
        for ext in self._extensions:
            if ext.name != ext_name:
                continue
            return [
                ExtensionTool(ext.name, reg, ext.process, ctx)
                for reg in ext.init.registrations.tools
                if not reg.deferred
            ]
        return []

    def commands(self) -> list[tuple[str, Any]]:
        """Every registered slash-command across all extensions, paired with the owning
        extension name."""
        out: list[tuple[str, Any]] = []
        for ext in self._extensions:
            for cmd in ext.init.registrations.commands:
                out.append((ext.name, cmd))
        return out

    def shortcuts(self) -> list[tuple[str, Any]]:
        """Every keyboard shortcut across all extensions, paired with the owning
        extension name."""
        out: list[tuple[str, Any]] = []
        for ext in self._extensions:
            for sc in ext.init.registrations.shortcuts:
                out.append((ext.name, sc))
        return out

    def _command_owner(self, ext_name: Optional[str], command: str) -> Optional[ExtensionProcess]:
        for ext in self._extensions:
            if ext_name is not None and ext_name != ext.name:
                continue
            if any(c.name == command for c in ext.init.registrations.commands):
                return ext.process
        return None

    async def run_command(self, ext_name: Optional[str], command: str, arguments: Any) -> Any:
        """Dispatch a registered slash-command to its owning extension with a
        COMMAND-tier context. Raises ``-32601`` if no loaded extension registered
        ``command``."""
        process = self._command_owner(ext_name, command)
        if process is None:
            raise RpcError(codes.METHOD_NOT_FOUND, f"no extension registered command `{command}`")
        params = {"command": command, "context": self.context(Tier.COMMAND).to_dict(), "arguments": arguments}
        try:
            return await process.request(method.COMMAND_EXECUTE, params, 120.0)
        except RuntimeError as exc:
            raise RpcError(codes.INTERNAL_ERROR, f"command/execute: {exc}") from exc

    async def complete_command(self, ext_name: Optional[str], command: str, partial: str) -> list[Completion]:
        """Ask the extension that owns ``command`` for argument completions. Returns an
        empty list when the extension does not implement completion or errors
        (autocomplete is best-effort — never fail the caller's keystroke)."""
        process = self._command_owner(ext_name, command)
        if process is None:
            return []
        params = {"command": command, "context": self.context(Tier.COMMAND).to_dict(), "partial": partial}
        try:
            raw = await process.request(method.COMMAND_COMPLETE, params, 5.0)
        except (RpcError, RuntimeError):
            return []
        if not isinstance(raw, dict):
            return []
        return [Completion.from_dict(c) for c in raw.get("completions", [])]

    async def reload(self, name: str) -> None:
        """Hot-reload a single extension by name: notify it (``session_shutdown``
        reason ``reload``), bump the epoch so every context token it still holds is
        invalidated, respawn its subprocess, re-run ``initialize`` to pick up new
        registrations, then notify it (``session_start`` reason ``reload``). The caller
        re-registers the extension's tools via :meth:`tools_for`."""
        ext = next((e for e in self._extensions if e.name == name), None)
        if ext is None:
            raise RuntimeError(f"extension `{name}` is not loaded")
        reload_ctx = self.context(Tier.EVENT).to_dict()
        ext.process.send_event("session_shutdown", reload_ctx, {"reason": "reload"})

        self.bump_epoch()
        await ext.process.respawn()

        init = await self._initialize(ext.process)
        ext.subscriptions = effective_subscriptions(ext.declared_events, init.registrations.subscriptions)
        ext.init = init

        start_ctx = self.context(Tier.EVENT).to_dict()
        ext.process.send_event("session_start", start_ctx, {"reason": "reload"})

    async def shutdown_all(self) -> None:
        """Gracefully shut down every extension (5s grace each, then kill)."""
        for ext in self._extensions:
            await ext.process.shutdown(5.0)
