"""Human-in-the-loop approval — pause before a sensitive/write tool runs.

Phase-2 sibling of the C# ``HumanGate`` (``dotnet/core``) and the Rust engine's
confirmation hook. When a turn is about to run a tool the caller flagged as
needing approval, the agent consults a :class:`HumanGate` first. The gate IS the
pause point — a UI gate awaits a real person (e.g. resolving a future when a
button is clicked); a programmatic gate decides immediately. A denial is never
executed; the denial reason is fed back to the model as the tool result so the
model can adapt. With no gate configured, behavior is unchanged.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import Any, Awaitable, Callable, Protocol


class HumanDecision(Enum):
    """The human's verdict on a tool call that required approval."""

    APPROVED = "approved"
    DENIED = "denied"


@dataclass(frozen=True)
class HumanApprovalRequest:
    """A request for human approval before the agent executes a sensitive/write tool.

    Mirrors the C# ``HumanApprovalRequest`` / the Rust engine's ``HumanRequest::Confirm``.
    """

    tool_name: str
    arguments: dict[str, Any]
    prompt: str


@dataclass(frozen=True)
class HumanApprovalResponse:
    """The response to a :class:`HumanApprovalRequest`. Mirrors the C# ``HumanApprovalResponse``."""

    decision: HumanDecision
    reason: str | None = None

    @property
    def is_approved(self) -> bool:
        return self.decision is HumanDecision.APPROVED

    @staticmethod
    def approve() -> "HumanApprovalResponse":
        return HumanApprovalResponse(HumanDecision.APPROVED)

    @staticmethod
    def deny(reason: str) -> "HumanApprovalResponse":
        return HumanApprovalResponse(HumanDecision.DENIED, reason)


class HumanGate(Protocol):
    """The human-in-the-loop seam: the agent consults it before running any tool
    that :func:`AgentOptions.requires_approval` flags. The implementation IS the
    pause point. Mirrors the C# ``IHumanGate``.
    """

    async def request_approval(self, request: HumanApprovalRequest) -> HumanApprovalResponse: ...


@dataclass
class DelegateHumanGate:
    """A :class:`HumanGate` backed by an async callable — handy for wiring a UI or tests."""

    handler: Callable[[HumanApprovalRequest], Awaitable[HumanApprovalResponse]]

    async def request_approval(self, request: HumanApprovalRequest) -> HumanApprovalResponse:
        return await self.handler(request)
