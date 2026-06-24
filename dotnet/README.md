# smooth-operator-core (.NET)

Native **C# / .NET** implementation of the smooth-operator agent engine — the
in-process, [`Microsoft.Extensions.AI`](https://learn.microsoft.com/dotnet/ai/microsoft-extensions-ai)-based
sibling of the Rust [`smooai-smooth-operator-core`](../rust) crate.

- Engine: [`core/src`](core/src) — published to NuGet as **`SmooAI.SmoothOperator.Core`**.
- Tests: [`core/tests`](core/tests).
- Solution: [`SmooAI.SmoothOperator.Core.slnx`](SmooAI.SmoothOperator.Core.slnx) — builds the engine + tests standalone.

```bash
dotnet test dotnet/SmooAI.SmoothOperator.Core.slnx \
  --filter "FullyQualifiedName!~EvalTests&FullyQualifiedName!~LiveE2E"
```

The `EvalTests` / `LiveE2E` suites require a live LLM gateway and are excluded by
default (and in CI). CI runs in [`.github/workflows/dotnet-checks.yml`](../.github/workflows/dotnet-checks.yml);
publishing is release-gated in [`.github/workflows/publish-nuget.yml`](../.github/workflows/publish-nuget.yml).
