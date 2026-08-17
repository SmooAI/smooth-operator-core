# smooth-operator-core (Python)

The native Python implementation of the smooth-operator agent engine lives in
[`core/`](./core) and is published to PyPI as
[`smooai-smooth-operator-core`](https://pypi.org/project/smooai-smooth-operator-core/)
from this repo (see `.github/workflows/publish-pypi.yml`).

It is the Python sibling of the Rust reference engine ([`../rust/`](../rust)) and
the C# core ([`../dotnet/`](../dotnet)) — an in-process, OpenAI-compatible
agentic tool-calling loop with knowledge grounding. Pure Python; no native
bindings (PyO3 is not used).

```bash
cd core
uv sync
uv run pytest tests/ -q
```

## Optional Temporal backend

[`temporal/`](./temporal) is the optional Temporal-backed durable-execution
backend (ADR-030), published as
[`smooai-smooth-operator-temporal`](https://pypi.org/project/smooai-smooth-operator-temporal/).
It runs an agent turn as a Temporal workflow driving the engine's deterministic
`drive_turn` unchanged (crash-safe resume, durable HITL, durable timers), and is
the Python sibling of the `smooai-smooth-operator-temporal` Rust crate. The
Temporal SDK is an optional `temporal` extra; the serde DTO boundary needs no SDK.

```bash
cd temporal
uv sync
uv run pytest tests/ -q   # e2e tests self-skip when no Temporal dev server is reachable
```
