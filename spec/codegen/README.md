# Codegen — per-language type generation from `spec/`

All language clients regenerate their types from the JSON Schemas in `spec/`. The generated files are committed alongside the generator config so that CI can verify they stay in sync with the schemas.

## TypeScript — `json-schema-to-typescript`

```bash
# Install
npm install -g json-schema-to-typescript

# Generate types for the entire spec tree (run from repo root)
json2ts \
  --input  'spec/**/*.schema.json' \
  --output 'typescript/src/generated/' \
  --additionalProperties false \
  --unknownAny false \
  --style.singleQuote true

# Or per-file (useful during development)
json2ts spec/domain/session.schema.json > typescript/src/generated/domain/session.ts
```

Output: `typescript/src/generated/` — one `.ts` file per schema, with `export interface` / `export type` declarations.

## Go — `quicktype`

```bash
# Install
npm install -g quicktype

# Generate all domain types
quicktype \
  --src-lang schema \
  --lang go \
  --package protocol \
  --out go/protocol/generated.go \
  spec/domain/*.schema.json \
  spec/actions/*.schema.json \
  spec/events/*.schema.json

# Single schema
quicktype --src-lang schema --lang go --package protocol \
  spec/domain/session.schema.json --out go/protocol/session.go
```

Output: `go/protocol/` — idiomatic Go structs with `json:` tags.

## .NET — `NJsonSchema`

```bash
# Install the CLI (requires .NET 8+)
dotnet tool install --global NJsonSchema.CodeGeneration.Tool

# Generate C# classes
njsonschema2csharp \
  --input  spec/domain/session.schema.json \
  --output dotnet/SmooAgent.Protocol/Generated/Session.cs \
  --namespace SmooAgent.Protocol \
  --class-name Session

# For batch generation, iterate over all schema files:
# find spec -name '*.schema.json' | xargs -I{} sh -c \
#   'njsonschema2csharp --input {} --output dotnet/SmooAgent.Protocol/Generated/$(basename {} .schema.json).cs --namespace SmooAgent.Protocol'
```

Output: `dotnet/SmooAgent.Protocol/Generated/` — C# `partial class` / `record` files.

## Python — `datamodel-code-generator`

```bash
# Install
pip install datamodel-code-generator

# Generate Pydantic v2 models for all schemas
datamodel-codegen \
  --input spec/ \
  --input-file-type jsonschema \
  --output python/smooth_operator/generated/ \
  --output-model-type pydantic_v2.BaseModel \
  --use-annotated \
  --use-field-description \
  --reuse-model

# Or a single schema
datamodel-codegen \
  --input spec/domain/session.schema.json \
  --input-file-type jsonschema \
  --output python/smooth_operator/generated/domain/session.py \
  --output-model-type pydantic_v2.BaseModel
```

Output: `python/smooth_operator/generated/` — Pydantic v2 model files with `model_config = ConfigDict(populate_by_name=True)`.

## Rust — `typify` (schemars)

```bash
# Add to Cargo.toml under [build-dependencies]
# typify = "0.4"
# schemars = "0.8"

# In build.rs, use typify's SchematicGenerator API to emit Rust types.
# See: https://github.com/oxidecomputer/typify
```

Output: `rust/smooth-operator-protocol/src/generated.rs` — `#[derive(Debug, Clone, Serialize, Deserialize)]` structs.

## Keeping generated code in sync

Add a CI step that runs the generator and checks for uncommitted changes:

```bash
# In .github/workflows/codegen.yml:
# 1. Run the generator for every language
# 2. git diff --exit-code -- typescript/src/generated/ go/protocol/ dotnet/ python/smooth_operator/generated/ rust/smooth-operator-protocol/src/generated.rs
# If the diff is non-empty, the PR modified a schema without regenerating — fail the check.
```

## Validating against conformance fixtures

After generation, validate the `spec/conformance/fixtures.json` examples:

```bash
# Using ajv-cli (Node.js)
npx ajv-cli validate \
  -s spec/events/stream-chunk.schema.json \
  -d spec/conformance/fixtures.json#/stream_chunk_event

# Using check-jsonschema (Python)
pip install check-jsonschema
check-jsonschema --schemafile spec/events/eventual-response.schema.json \
  spec/conformance/fixtures.json
```
