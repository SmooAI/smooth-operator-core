"""Serde data-transfer objects for the Temporal activity boundary.

A Temporal activity serializes its input and output. The engine's turn speaks the
OpenAI wire shape (plain ``dict``s), so — unlike the Rust reference, whose
``LlmResponse`` is deliberately *not* ``Serialize`` — Python's model call already
returns a JSON-serializable value. These DTOs give that boundary an explicit,
typed shape: ``ModelCallInput`` / ``ToolInvokeInput`` bundle the activity
arguments, and ``ModelCallOutput`` projects the fields
:func:`~smooth_operator_core.drive_turn` actually consumes from the assistant
message (``content`` + ``tool_calls``), reconstructing the wire dict on the
workflow side via :meth:`ModelCallOutput.to_assistant`.

Everything here is plain :mod:`dataclasses` over the wire dicts and imports **no**
Temporal SDK, so it is always importable and unit-tested regardless of whether
``temporalio`` is installed — mirroring how the Rust reference always compiles and
tests ``dto.rs`` even with the ``temporal`` feature off.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ModelCallInput:
    """Input to the ``model_call`` activity: the context window + visible tools.

    ``messages`` are OpenAI-wire message dicts; ``tools`` are OpenAI-wire tool
    specs (``None`` when the turn exposes no tools).
    """

    messages: list[dict[str, Any]]
    tools: list[dict[str, Any]] | None = None


@dataclass
class ModelCallOutput:
    """Output of the ``model_call`` activity: a projection of the assistant message.

    Carries exactly the two fields :func:`~smooth_operator_core.drive_turn` reads —
    ``content`` and ``tool_calls`` — so it can cross the activity boundary and be
    turned back into the assistant wire dict on the workflow side. (Python's engine
    does not surface per-call usage/cost at this seam, so — unlike the Rust
    ``ModelCallOutput`` — there are no accounting fields to preserve here yet.)
    """

    content: str = ""
    tool_calls: list[dict[str, Any]] = field(default_factory=list)

    @classmethod
    def from_assistant(cls, message: dict[str, Any]) -> ModelCallOutput:
        """Project an assistant wire dict (``{"role", "content", "tool_calls"?}``)."""
        return cls(content=message.get("content") or "", tool_calls=list(message.get("tool_calls") or []))

    def to_assistant(self) -> dict[str, Any]:
        """Reconstruct the assistant wire dict this projection came from.

        ``tool_calls`` is included only when non-empty, matching what
        :func:`~smooth_operator_core.drive_turn` appends inline.
        """
        assistant: dict[str, Any] = {"role": "assistant", "content": self.content}
        if self.tool_calls:
            assistant["tool_calls"] = self.tool_calls
        return assistant


@dataclass
class AgentTurnInput:
    """Input to ``AgentTurnWorkflow``: everything needed to seed the conversation
    and bound the loop. Serializable so it crosses the workflow-start boundary.

    ``max_iterations`` of ``0`` falls back to the engine's
    :class:`~smooth_operator_core.TurnPolicy` default. ``approval_required_tools``
    names tools the workflow blocks — durably, on an ``approve_tool`` /
    ``deny_tool`` signal — before running. ``wait_tool`` names a built-in durable
    wait: a model call to it with an integer ``seconds`` argument sleeps the
    workflow on a Temporal timer instead of dispatching a tool activity.
    """

    system_prompt: str
    user_message: str
    tools: list[dict[str, Any]] = field(default_factory=list)
    max_iterations: int = 0
    approval_required_tools: list[str] = field(default_factory=list)
    wait_tool: str | None = None


@dataclass
class ToolInvokeInput:
    """Input to the ``tool_invoke`` activity: the single wire-shape tool call to run.

    ``call`` is the OpenAI-wire ``{"id", "type", "function": {"name", "arguments"}}``
    dict the model emitted, exactly as :func:`~smooth_operator_core.drive_turn`
    hands it to an activity.
    """

    call: dict[str, Any]


@dataclass
class ToolInvokeOutput:
    """Output of the ``tool_invoke`` activity: the tool result the model sees.

    Projects :class:`smooth_operator_core.ToolResult` down to what
    :func:`~smooth_operator_core.drive_turn` appends to the transcript — the
    ``content`` string and the ``is_error`` flag. (``details`` is UI-facing and not
    part of the turn transcript, so it is dropped at this boundary.)
    """

    content: str
    is_error: bool = False
