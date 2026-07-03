"""``ExtensionTool`` — a :class:`~smooth_operator_core.agent.Tool` backed by an
extension subprocess.

Registered tools appear to the agent as ordinary tools named ``<extension>.<tool>``
(the MCP convention). :meth:`execute` forwards to the extension over
``tool/execute`` and maps the reply back to a result string. The Python sibling of
the Rust reference ``tool_proxy.rs`` — satisfies the engine's structural ``Tool``
protocol (``name`` / ``description`` / ``parameters`` / ``async execute``).
"""

from __future__ import annotations

import uuid
from typing import Any

from .process import ExtensionProcess
from .protocol import Context, ToolExecuteResult, method

#: Upper bound (seconds) for a single ``tool/execute`` round-trip.
TOOL_EXECUTE_TIMEOUT = 120.0


class ExtensionTool:
    """A tool exposed by an extension. ``name`` is the dotted ``<extension>.<tool>``
    the agent/LLM sees; the bare name is what the extension receives."""

    def __init__(self, ext_name: str, reg: Any, process: ExtensionProcess, context: Context) -> None:
        self.name = f"{ext_name}.{reg.name}"
        self.description = reg.description
        self.parameters = reg.parameters
        self._bare_name = reg.name
        self._process = process
        self._context = context

    async def execute(self, arguments: dict[str, Any]) -> str:
        call_id = str(uuid.uuid4())
        params = {
            "call_id": call_id,
            "tool": self._bare_name,
            "arguments": arguments,
            "context": self._context.to_dict(),
        }
        raw = await self._process.request(method.TOOL_EXECUTE, params, TOOL_EXECUTE_TIMEOUT)
        result = ToolExecuteResult.from_dict(raw)
        if result.is_error:
            raise RuntimeError(result.content)
        # ponytail: `details` is dropped here — the engine's Tool.execute returns only
        # a string. Structured details ride tool-update/event wiring in a later phase.
        return result.content
