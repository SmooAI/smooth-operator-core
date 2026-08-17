"""Optional Temporal-backed durable execution backend for the smooth-operator
agent engine (ADR-030), the Python sibling of the ``smooai-smooth-operator-temporal``
Rust crate.

The serde :mod:`~smooth_operator_temporal.dto` boundary is always importable and
pulls in no Temporal SDK. The workflow/activity wiring lives in
:mod:`~smooth_operator_temporal.temporal`, which imports ``temporalio`` — install
the optional ``temporal`` extra to use it::

    pip install 'smooai-smooth-operator-temporal[temporal]'

This mirrors the Rust reference's off-by-default ``temporal`` cargo feature: a
consumer that does not opt in never pulls the Temporal SDK and stays zero-infra.
"""

from __future__ import annotations

from . import dto

__all__ = ["dto"]
