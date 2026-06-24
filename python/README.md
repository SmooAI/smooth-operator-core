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
