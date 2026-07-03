"""SEP host — the Python engine's implementation of the Smooth Extension Protocol.

An extension is a long-lived subprocess speaking JSON-RPC 2.0 over ndjson on its
stdio (identical framing to MCP stdio). The canonical wire schemas live in the
``smooth-operator`` repo at ``spec/extension/``; :mod:`protocol` is this host's typed
view of that wire.

This package is **purely additive**: nothing here runs unless a caller builds an
:class:`ExtensionHost`. With no host attached the agent loop behaves exactly as
before. Layout mirrors the Rust reference engine:

- :mod:`protocol` — JSON-RPC frames + typed method params/results.
- :mod:`manifest` — ``extension.toml`` discovery, global+project merge, ``${env:VAR}``.
- :mod:`process` — one subprocess: ndjson codec, pending map, generation-guarded restart.
- :mod:`host` — :class:`ExtensionHost`: hook chaining, event fanout, tool proxies, delegate seam.
- :mod:`tool_proxy` — :class:`ExtensionTool`: an extension tool as an engine ``Tool``.
"""

from __future__ import annotations

from .host import (
    PROTOCOL_VERSION,
    DefaultHostDelegate,
    ExtensionHost,
    FoldedHook,
    HookStep,
    HookType,
    HostDelegate,
    effective_subscriptions,
    fold_hook_chain,
    validate_command_context,
)
from .manifest import (
    Capabilities,
    DiscoveredExtension,
    ExtensionManifest,
    Resources,
    RunSpec,
    Scope,
    default_global_dir,
    discover,
    project_dir,
)
from .process import (
    DefaultInboundHandler,
    ExtensionProcess,
    InboundHandler,
    SpawnSpec,
    backoff_for,
)
from .protocol import (
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

#: Canonical SEP event names the host dispatches to subscribed extensions.
TURN_START = "turn_start"
TURN_END = "turn_end"
MESSAGE_START = "message_start"
MESSAGE_UPDATE = "message_update"
MESSAGE_END = "message_end"
TOOL_EXECUTION_START = "tool_execution_start"
TOOL_EXECUTION_UPDATE = "tool_execution_update"
TOOL_EXECUTION_END = "tool_execution_end"
EVENTS_LOST = "events_lost"

__all__ = [
    "PROTOCOL_VERSION",
    "Capabilities",
    "Context",
    "DefaultHostDelegate",
    "DefaultInboundHandler",
    "DiscoveredExtension",
    "ExtensionHost",
    "ExtensionManifest",
    "ExtensionProcess",
    "ExtensionTool",
    "FoldedHook",
    "HookOutcome",
    "HookStep",
    "HookType",
    "HostDelegate",
    "HostInfo",
    "InboundHandler",
    "InitializeParams",
    "InitializeResult",
    "Resources",
    "RpcError",
    "RunSpec",
    "Scope",
    "SpawnSpec",
    "Tier",
    "WorkspaceInfo",
    "backoff_for",
    "codes",
    "default_global_dir",
    "discover",
    "effective_subscriptions",
    "fold_hook_chain",
    "method",
    "project_dir",
    "validate_command_context",
]
