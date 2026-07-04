---
'@smooai/smooth-operator-core': patch
---

SEP security fix (th-f0e020): scope what an extension `tool_call` **Modify** can
do. The `tool_call` hook fires over every pending call the model made — native
tools (`bash`, `file-write`) included — and a `Modify` verdict was applied
verbatim as a full `{tool, arguments}` replacement with no validation. So
enabling ANY extension let its hook silently rewrite the arguments of a bash /
file-write call — or redirect the call to a different tool — with zero
oversight.

The fold driver (`ExtensionHost::run_hook`) now guards every `tool_call` Modify:

- The `tool` field is immutable across a hook — a Modify that renames the tool
  is rejected (redirecting call A to a different tool is never legitimate).
- An extension may only rewrite the arguments of a tool it **owns**
  (namespaced `<ext>.<tool>`). A Modify targeting a native tool or another
  extension's tool is rejected.

Rejected Modifies are downgraded to `Continue` (the original call is preserved)
and logged as a security warning. **Blocking is unaffected** — an extension can
still `Block` any call, native or not; only silent mutation is scoped. Continue,
Block, fail-closed timeout semantics, and Modify of the extension's own tool args
are all unchanged. Exhaustive adversarial unit tests cover tool-rename,
native-tool rewrite, foreign-extension rewrite, and the legitimate own-tool
cases.
