"""SEP wire protocol — JSON-RPC 2.0 frames and typed method params/results.

SEP (the Smooth Extension Protocol) is JSON-RPC 2.0 over ndjson on an extension
subprocess's stdio. The canonical schemas live in the ``smooth-operator`` repo at
``spec/extension/``; the types here are the Python host's view of that wire. Field
names are ``snake_case`` to match the spec exactly.

The Python sibling of the Rust reference ``protocol.rs``. The host works mostly in
plain dicts (the wire is JSON); the dataclasses here are the typed views the
handshake, hook, and tool paths parse into — and what the conformance suite
replays to catch drift from the wire.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional

#: The SEP protocol version this host implements.
PROTOCOL_VERSION = 1


class codes:
    """JSON-RPC error codes: the standard range plus the SEP extensions documented
    in ``spec/extension/envelope.md``."""

    PARSE_ERROR = -32700
    INVALID_REQUEST = -32600
    METHOD_NOT_FOUND = -32601
    INVALID_PARAMS = -32602
    INTERNAL_ERROR = -32603

    #: A hook or policy vetoed the operation.
    BLOCKED = -32000
    #: ``ui/request`` in a headless/uncapable frontend.
    NO_UI = -32001
    #: Extension acted beyond its granted trust.
    NOT_TRUSTED = -32002
    #: Command-tier action attempted from an event-tier context.
    CONTEXT_VIOLATION = -32003
    #: Method requires a capability the handshake did not enable.
    CAPABILITY_DISABLED = -32004
    #: Request cancelled via ``$/cancel``.
    CANCELLED = -32800


class method:
    """SEP method names, centralized so the host and tests never spell one wrong."""

    INITIALIZE = "initialize"
    SHUTDOWN = "shutdown"
    PING = "ping"
    EVENT = "event"
    HOOK = "hook"
    TOOL_EXECUTE = "tool/execute"
    TOOL_UPDATE = "tool/update"
    COMMAND_EXECUTE = "command/execute"
    COMMAND_COMPLETE = "command/complete"
    CANCEL = "$/cancel"
    REGISTRY_UPDATE = "registry/update"
    TOOLS_SET_ACTIVE = "tools/set_active"
    EXEC_RUN = "exec/run"
    UI_REQUEST = "ui/request"
    LOG = "log"
    BUS_PUBLISH = "bus/publish"
    SESSION_SEND_MESSAGE = "session/send_message"
    SESSION_SEND_USER_MESSAGE = "session/send_user_message"
    SESSION_APPEND_ENTRY = "session/append_entry"


class RpcError(Exception):
    """A JSON-RPC error object, raised across the ext<->host seam."""

    def __init__(self, code: int, message: str, data: Any | None = None) -> None:
        super().__init__(f"JSON-RPC error {code}: {message}")
        self.code = code
        self.message = message
        self.data = data

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.data is not None:
            out["data"] = self.data
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> RpcError:
        return cls(int(d["code"]), str(d["message"]), d.get("data"))

    def __eq__(self, other: object) -> bool:
        return (
            isinstance(other, RpcError)
            and other.code == self.code
            and other.message == self.message
            and other.data == self.data
        )


class Tier(str, Enum):
    """Whether a dispatch may only observe (``EVENT``) or may mutate the session
    (``COMMAND``). Session-mutating ext->host actions require ``COMMAND``."""

    EVENT = "event"
    COMMAND = "command"


@dataclass
class Context:
    """The dispatch context carried by every host->ext event/hook/tool/command."""

    token: str
    tier: Tier

    def to_dict(self) -> dict[str, Any]:
        return {"token": self.token, "tier": self.tier.value}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Context:
        return cls(token=str(d["token"]), tier=Tier(d["tier"]))


# ---------------------------------------------------------------------------
# The JSON-RPC envelope.
# ---------------------------------------------------------------------------


@dataclass
class Message:
    """The JSON-RPC 2.0 envelope. All four frame shapes share this dataclass; which
    fields are present determines the shape:

    - request: ``id`` + ``method`` (+ optional ``params``)
    - notification: ``method``, no ``id``
    - success response: ``id`` + ``result``
    - error response: ``id`` + ``error``
    """

    jsonrpc: str = "2.0"
    id: Any | None = None
    method: Optional[str] = None
    params: Any | None = None
    result: Any | None = None
    error: Optional[RpcError] = None

    @classmethod
    def request(cls, id: Any, method: str, params: Any) -> Message:
        return cls(id=id, method=method, params=params)

    @classmethod
    def notification(cls, method: str, params: Any) -> Message:
        return cls(method=method, params=params)

    @classmethod
    def success(cls, id: Any, result: Any) -> Message:
        return cls(id=id, result=result)

    @classmethod
    def error_response(cls, id: Any | None, error: RpcError) -> Message:
        return cls(id=id, error=error)

    def is_request(self) -> bool:
        return self.id is not None and self.method is not None

    def is_notification(self) -> bool:
        return self.id is None and self.method is not None

    def is_response(self) -> bool:
        return self.method is None and self.id is not None

    def to_dict(self) -> dict[str, Any]:
        # `skip_serializing_if = Option::is_none`: absent fields stay off the wire so
        # a request never carries a `result`/`error` key.
        out: dict[str, Any] = {"jsonrpc": self.jsonrpc}
        if self.id is not None:
            out["id"] = self.id
        if self.method is not None:
            out["method"] = self.method
        if self.params is not None:
            out["params"] = self.params
        if self.result is not None:
            out["result"] = self.result
        if self.error is not None:
            out["error"] = self.error.to_dict()
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Message:
        err = d.get("error")
        return cls(
            jsonrpc=str(d.get("jsonrpc", "")),
            id=d.get("id"),
            method=d.get("method"),
            params=d.get("params"),
            result=d.get("result"),
            error=RpcError.from_dict(err) if isinstance(err, dict) else None,
        )


# ---------------------------------------------------------------------------
# initialize
# ---------------------------------------------------------------------------


@dataclass
class HostInfo:
    name: str
    version: str

    def to_dict(self) -> dict[str, Any]:
        return {"name": self.name, "version": self.version}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> HostInfo:
        return cls(name=str(d["name"]), version=str(d["version"]))


@dataclass
class WorkspaceInfo:
    root: str
    trusted: bool

    def to_dict(self) -> dict[str, Any]:
        return {"root": self.root, "trusted": self.trusted}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> WorkspaceInfo:
        return cls(root=str(d["root"]), trusted=bool(d["trusted"]))


@dataclass
class InitializeParams:
    protocol_version: int
    host: HostInfo
    workspace: WorkspaceInfo
    mode: str
    session: Optional[dict[str, Any]] = None
    ui_capabilities: list[str] = field(default_factory=list)
    flags: dict[str, Any] = field(default_factory=dict)
    capabilities_enabled: Any | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "protocol_version": self.protocol_version,
            "host": self.host.to_dict(),
            "workspace": self.workspace.to_dict(),
            "mode": self.mode,
        }
        if self.session is not None:
            out["session"] = self.session
        if self.ui_capabilities:
            out["ui_capabilities"] = list(self.ui_capabilities)
        if self.flags:
            out["flags"] = dict(self.flags)
        if self.capabilities_enabled is not None:
            out["capabilities_enabled"] = self.capabilities_enabled
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> InitializeParams:
        # `protocol_version` is required — a handshake without it is malformed.
        return cls(
            protocol_version=int(d["protocol_version"]),
            host=HostInfo.from_dict(d["host"]),
            workspace=WorkspaceInfo.from_dict(d["workspace"]),
            mode=str(d["mode"]),
            session=d.get("session"),
            ui_capabilities=list(d.get("ui_capabilities", [])),
            flags=dict(d.get("flags", {})),
            capabilities_enabled=d.get("capabilities_enabled"),
        )


@dataclass
class ExtensionInfo:
    name: str
    version: str

    def to_dict(self) -> dict[str, Any]:
        return {"name": self.name, "version": self.version}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ExtensionInfo:
        return cls(name=str(d["name"]), version=str(d["version"]))


@dataclass
class ToolRegistration:
    name: str
    description: str
    parameters: dict[str, Any]
    deferred: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "description": self.description,
            "parameters": self.parameters,
            "deferred": self.deferred,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolRegistration:
        return cls(
            name=str(d["name"]),
            description=str(d["description"]),
            parameters=dict(d["parameters"]),
            deferred=bool(d.get("deferred", False)),
        )


@dataclass
class CommandRegistration:
    name: str
    description: str

    def to_dict(self) -> dict[str, Any]:
        return {"name": self.name, "description": self.description}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> CommandRegistration:
        return cls(name=str(d["name"]), description=str(d["description"]))


@dataclass
class ShortcutRegistration:
    key: str
    command: str
    description: Optional[str] = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"key": self.key, "command": self.command}
        if self.description is not None:
            out["description"] = self.description
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ShortcutRegistration:
        return cls(key=str(d["key"]), command=str(d["command"]), description=d.get("description"))


@dataclass
class Registrations:
    tools: list[ToolRegistration] = field(default_factory=list)
    commands: list[CommandRegistration] = field(default_factory=list)
    flags: list[str] = field(default_factory=list)
    shortcuts: list[ShortcutRegistration] = field(default_factory=list)
    subscriptions: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.tools:
            out["tools"] = [t.to_dict() for t in self.tools]
        if self.commands:
            out["commands"] = [c.to_dict() for c in self.commands]
        if self.flags:
            out["flags"] = list(self.flags)
        if self.shortcuts:
            out["shortcuts"] = [s.to_dict() for s in self.shortcuts]
        if self.subscriptions:
            out["subscriptions"] = list(self.subscriptions)
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Registrations:
        return cls(
            tools=[ToolRegistration.from_dict(t) for t in d.get("tools", [])],
            commands=[CommandRegistration.from_dict(c) for c in d.get("commands", [])],
            flags=list(d.get("flags", [])),
            shortcuts=[ShortcutRegistration.from_dict(s) for s in d.get("shortcuts", [])],
            subscriptions=list(d.get("subscriptions", [])),
        )


@dataclass
class InitializeResult:
    protocol_version: int
    extension: ExtensionInfo
    registrations: Registrations = field(default_factory=Registrations)

    def to_dict(self) -> dict[str, Any]:
        return {
            "protocol_version": self.protocol_version,
            "extension": self.extension.to_dict(),
            "registrations": self.registrations.to_dict(),
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> InitializeResult:
        return cls(
            protocol_version=int(d["protocol_version"]),
            extension=ExtensionInfo.from_dict(d["extension"]),
            registrations=Registrations.from_dict(d.get("registrations", {})),
        )


# ---------------------------------------------------------------------------
# hook
# ---------------------------------------------------------------------------


@dataclass
class HookOutcome:
    """An extension's reply to a ``hook``. ``action`` is one of ``continue`` /
    ``block`` / ``modify``; a ``modify`` MUST carry a ``patch``. Any other action, or
    a ``modify`` without a patch, is malformed and rejected on parse."""

    action: str
    reason: Optional[str] = None
    patch: Any | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"action": self.action}
        if self.action == "block" and self.reason is not None:
            out["reason"] = self.reason
        if self.action == "modify":
            out["patch"] = self.patch
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> HookOutcome:
        action = d.get("action")
        if action == "continue":
            return cls(action="continue")
        if action == "block":
            return cls(action="block", reason=d.get("reason"))
        if action == "modify":
            if "patch" not in d:
                raise ValueError("hook outcome `modify` requires a `patch`")
            return cls(action="modify", patch=d["patch"])
        raise ValueError(f"unknown hook outcome action: {action!r}")


# ---------------------------------------------------------------------------
# tool/execute + tool/update
# ---------------------------------------------------------------------------


@dataclass
class ToolExecuteParams:
    call_id: str
    tool: str
    arguments: Any
    context: Context

    def to_dict(self) -> dict[str, Any]:
        return {
            "call_id": self.call_id,
            "tool": self.tool,
            "arguments": self.arguments,
            "context": self.context.to_dict(),
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolExecuteParams:
        # `call_id` is required — it correlates tool/update progress + $/cancel.
        return cls(
            call_id=str(d["call_id"]),
            tool=str(d["tool"]),
            arguments=d.get("arguments"),
            context=Context.from_dict(d["context"]),
        )


@dataclass
class ToolExecuteResult:
    content: str
    is_error: bool = False
    details: Any | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"content": self.content, "is_error": self.is_error}
        if self.details is not None:
            out["details"] = self.details
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolExecuteResult:
        # `content` is required — the LLM-facing text of the tool result.
        return cls(
            content=str(d["content"]),
            is_error=bool(d.get("is_error", False)),
            details=d.get("details"),
        )


@dataclass
class ToolUpdateParams:
    call_id: str
    message: Optional[str] = None
    progress: Optional[float] = None
    details: Any | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"call_id": self.call_id}
        if self.message is not None:
            out["message"] = self.message
        if self.progress is not None:
            out["progress"] = self.progress
        if self.details is not None:
            out["details"] = self.details
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolUpdateParams:
        return cls(
            call_id=str(d["call_id"]),
            message=d.get("message"),
            progress=d.get("progress"),
            details=d.get("details"),
        )


# ---------------------------------------------------------------------------
# event
# ---------------------------------------------------------------------------


@dataclass
class EventParams:
    event: str
    context: Context
    seq: Optional[int] = None
    payload: Any | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"event": self.event, "context": self.context.to_dict()}
        if self.seq is not None:
            out["seq"] = self.seq
        if self.payload is not None:
            out["payload"] = self.payload
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> EventParams:
        return cls(
            event=str(d["event"]),
            context=Context.from_dict(d["context"]),
            seq=d.get("seq"),
            payload=d.get("payload"),
        )


@dataclass
class CommandExecuteResult:
    content: Optional[str] = None

    def to_dict(self) -> dict[str, Any]:
        return {"content": self.content} if self.content is not None else {}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> CommandExecuteResult:
        return cls(content=d.get("content"))


@dataclass
class Completion:
    value: str
    description: Optional[str] = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"value": self.value}
        if self.description is not None:
            out["description"] = self.description
        return out

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Completion:
        return cls(value=str(d["value"]), description=d.get("description"))


@dataclass
class CommandCompleteResult:
    completions: list[Completion] = field(default_factory=list)

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> CommandCompleteResult:
        return cls(completions=[Completion.from_dict(c) for c in d.get("completions", [])])
