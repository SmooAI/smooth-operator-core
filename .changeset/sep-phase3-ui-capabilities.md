---
"@smooai/smooth-operator-core": minor
---

SEP Phase 3 (engine) — thread `ui_capabilities` through the handshake.

`ExtensionHost::load` now takes a `ui_capabilities: Vec<String>` and forwards it
into each extension's `initialize` params, so a host declares which `ui/request`
kinds its frontend can render (`select`/`confirm`/`input`/`notify`/`set_status`/
`set_widget`/`set_title`). Extensions gate their UI on this list (the SDK's
`hasUI`); the ext→host `ui/request` seam and its headless `-32001 NoUI` default
already landed in Phase 2's `HostDelegate`. A new `SEP_ECHO_UI` mode on the
reference `sep-echo-peer` round-trips a `ui/request` confirm from inside a
`tool/execute`, echoing the negotiated caps into the prompt, exercised by the new
`sep_ui_path` integration test (answered verdict + headless NoUI).

The engine ships headless (empty caps); smooth-code and the daemon supply the
real capability set and a `HostDelegate` that renders the dialogs.
