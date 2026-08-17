---
'@smooai/smooth-operator-core': patch
---

feat(go,ts,python,dotnet): port the project context loader (AGENTS.md / CLAUDE.md)

Rust has had `context.rs` since pearl th-5002c4 — the loader that finds a project's
agent-instruction file and hands the host a string to put in the system prompt. The
four ports never had it, so an embedder in Go/TS/Python/.NET had to hand-roll the
discovery walk (and get the precedence wrong).

All four now mirror Rust exactly:

- **Two layers, stacked.** The USER layer reads `~/.smooth/CONTEXT.md` →
  `AGENTS.md` → `CLAUDE.md` (first non-blank wins) and is prepended under a
  `## User context (~/.smooth)` heading. The PROJECT layer walks UP from the working
  directory, taking the first hit of `.smooth/CONTEXT.md` → `SMOOTH.md` → `AGENTS.md`
  → `CLAUDE.md` per directory. Either layer alone is enough — a bare CLAUDE.md with no
  user file still loads, which is the whole point of the fallback chain.
- **`## File References` resolved inline.** `- [Label](path.md#fragment) — description`
  is read relative to the context file's own directory and appended in a
  `## Resolved File References` block. A `#fragment` extracts just that markdown
  section, matched on GitHub-style heading anchors, ending at the next
  same-or-higher-level heading.
- **Absent files are a no-op.** Nothing found in either layer returns nothing
  (`nil` / `undefined` / `None` / `null`); an unreadable reference is skipped rather
  than failing the load. Behavior is unchanged for any project without these files.

Like the Rust reference this is a **standalone loader, not a hook inside the agent
loop** — the host calls it and decides what to do with the string, so no existing
agent behavior changes. Public as `LoadProjectContext` (Go),
`loadProjectContext` (TypeScript), `load_project_context` (Python) and
`ProjectContext.Load` (.NET).

Rust's 15 `context.rs` tests are ported one-for-one to each language (60 new tests):
link parsing with/without fragment and description, section-scoped reference
collection, anchor normalization, section extraction (found / to-EOF / not-found),
the four precedence cases, the walk-up, and the raw-passthrough when a file carries
no references.
