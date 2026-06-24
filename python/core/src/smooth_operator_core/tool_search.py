"""The ``tool_search`` meta-tool — promotes deferred tools on demand.

Phase-3 sibling of the Rust reference ``tool_search.rs``. Mirrors the behaviour,
not the type shapes (the Python core has no ``ToolRegistry`` — tools are a plain
list on :class:`AgentOptions`).

The idea: as a tool set grows past ~20-30 entries, every model turn pays tokens to
read schemas it isn't going to use, diluting the model's attention budget. So a
caller can register some tools as **deferred** (``AgentOptions.deferred_tools``):
their schemas are hidden from the model. Instead the agent advertises a single
built-in ``tool_search(query)`` meta-tool. When the model calls it, this fuzzy-
matches the query against the deferred tools' names + descriptions, **promotes**
the matches into the visible set (so the model can call them on subsequent turns),
and returns each match's name + description as JSON.

A deferred tool that has not been promoted is *not* dispatchable — calling it
surfaces as an unknown tool until ``tool_search`` adds it to the promoted set.
"""

from __future__ import annotations

import json
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .agent import Tool

#: The built-in meta-tool's name. Reserved — a caller's own tool may not use it
#: when deferred tools are in play.
TOOL_SEARCH_NAME = "tool_search"

#: Cap on how many deferred tools a single ``tool_search`` call may promote, so a
#: generic query like "tool" doesn't promote the entire deferred set in one shot.
MAX_MATCHES = 8

_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "query": {
            "type": "string",
            "description": "Keyword to match against deferred tool names and descriptions. Case-insensitive substring match.",
        }
    },
    "required": ["query"],
}

_DESCRIPTION = (
    "Search for additional tools by keyword. Returns matching tool schemas as JSON; "
    "matched tools become available on subsequent turns. Use when you think a tool "
    "exists for a specific task but isn't in your current tool list — e.g. "
    'tool_search(query="git") or tool_search(query="http request").'
)


class ToolSearch:
    """Drives deferred-tool promotion for one agent run.

    Holds the deferred tools (by name) and the mutable set of promoted names. The
    agent advertises :meth:`spec` to the model when there are deferred tools, runs
    :meth:`execute` when the model calls ``tool_search``, and consults
    :meth:`promoted_tools` each iteration to decide which deferred schemas are now
    visible/dispatchable.
    """

    def __init__(self, deferred: list[Tool]) -> None:
        self._deferred: dict[str, Tool] = {t.name: t for t in deferred}
        self._promoted: set[str] = set()

    @property
    def name(self) -> str:
        return TOOL_SEARCH_NAME

    @property
    def description(self) -> str:
        return _DESCRIPTION

    @property
    def parameters(self) -> dict[str, Any]:
        return _SCHEMA

    def has_deferred(self) -> bool:
        """True if any tool was registered deferred (the meta-tool is advertised only then)."""
        return bool(self._deferred)

    def is_deferred(self, name: str) -> bool:
        """True if ``name`` is a deferred tool (whether or not it has been promoted)."""
        return name in self._deferred

    def is_promoted(self, name: str) -> bool:
        """True if a deferred tool has been promoted and is now dispatchable."""
        return name in self._promoted

    def promoted_tools(self) -> list[Tool]:
        """The deferred tools that have been promoted — their schemas join the visible set."""
        return [self._deferred[n] for n in self._promoted if n in self._deferred]

    def tool_by_name(self, name: str) -> Tool | None:
        """Resolve a promoted deferred tool for dispatch. Unpromoted deferred tools are invisible."""
        if name in self._promoted:
            return self._deferred.get(name)
        return None

    def promote(self, name: str) -> bool:
        """Mark a deferred tool promoted. Returns False if no such deferred tool."""
        if name not in self._deferred:
            return False
        self._promoted.add(name)
        return True

    async def execute(self, arguments: dict[str, Any]) -> str:
        """Fuzzy-match the query, promote matches, and return their schemas as JSON.

        ``arguments`` is the parsed tool-call argument object (``{"query": "..."}``).
        """
        query = arguments.get("query")
        if not isinstance(query, str):
            return json.dumps({"matched": 0, "tools": [], "note": "missing required `query` parameter"})
        needle = query.strip().lower()
        if not needle:
            return json.dumps(
                {"matched": 0, "tools": [], "note": 'empty query — pass a keyword like "git" or "network"'}
            )

        matched = [t for t in self._deferred.values() if needle in t.name.lower() or needle in t.description.lower()]
        # Stable order (registration order) before truncation so a generic query
        # promotes a deterministic prefix.
        matched = matched[:MAX_MATCHES]

        for t in matched:
            self._promoted.add(t.name)

        tools = [{"name": t.name, "description": t.description, "parameters": t.parameters} for t in matched]
        return json.dumps({"matched": len(tools), "tools": tools})
