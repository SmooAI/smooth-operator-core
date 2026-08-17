"""Shared skip-gate + worker boilerplate for the Temporal e2e tests.

Each e2e test needs a reachable Temporal server. This mirrors the Rust reference's
per-test ephemeral dev server: prefer an already-running server named by
``TEMPORAL_ADDRESS``, else start an ephemeral local one, else self-skip cleanly
(offline / CI without the dev-server binary). ``temporalio`` itself may be absent
(the SDK is an optional extra) — importing this module then fails, and the tests
that import it are collected as skips via ``pytest.importorskip``.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
from collections.abc import AsyncIterator

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

_SERVER_START_TIMEOUT = 120


@contextlib.asynccontextmanager
async def temporal_client() -> AsyncIterator[Client]:
    """Yield a connected client, or skip the test if none can be reached.

    Uses ``TEMPORAL_ADDRESS`` when set (an existing server), otherwise an ephemeral
    local dev server. Shuts the ephemeral server down on exit.
    """
    address = os.environ.get("TEMPORAL_ADDRESS")
    if address:
        try:
            client = await asyncio.wait_for(Client.connect(address), timeout=10)
        except Exception as exc:  # noqa: BLE001 - any connect failure is a clean skip
            pytest.skip(f"no reachable Temporal server at TEMPORAL_ADDRESS={address}: {exc}")
        yield client
        return

    try:
        env = await asyncio.wait_for(WorkflowEnvironment.start_local(), timeout=_SERVER_START_TIMEOUT)
    except Exception as exc:  # noqa: BLE001 - offline / no dev-server binary is a clean skip
        pytest.skip(f"could not start an ephemeral Temporal dev server (likely offline): {exc}")

    try:
        yield env.client
    finally:
        await env.shutdown()


def worker(client: Client, task_queue: str, workflows: list, activities: list) -> Worker:
    """A worker registering the given workflows + activities on ``task_queue``.

    Use as an async context manager around the ``execute_workflow`` call; the
    worker polls in the background while the workflow runs.
    """
    return Worker(client, task_queue=task_queue, workflows=workflows, activities=activities)
