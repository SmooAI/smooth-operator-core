---
"@smooai/smooth-operator-core": patch
---

fix(rust): clear the clippy 1.96 `unnecessary_sort_by` failures in `checkpoint.rs`

Both checkpoint stores sorted newest-first with `sort_by(|a, b| b.created_at.cmp(&a.created_at))`,
which clippy 1.96 rejects in favour of `sort_by_key(|c| Reverse(c.created_at))`.
Same ordering, no behavior change — but it is a hard error under `-D warnings`,
so main goes red the moment GitHub's stable runner reaches 1.96. It is invisible
today only because the clippy step is `continue-on-error`.

`cargo clippy --workspace --all-targets -- -D warnings` and the `temporal`-feature
clippy both exit 0 on 1.96 now; 643 tests still pass.
