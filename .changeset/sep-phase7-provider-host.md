---
'@smooai/smooth-operator-core': minor
---

SEP Phase 7 (engine) — registerProvider: declarative provider registration,
OAuth round-trips, proxied streaming, and `session/set_model`.

Extensions can now contribute LLM providers to the host. The engine gains:

- **Declarative provider registration** — `ProviderRegistration` (name, base_url,
  api_key_env, oauth flag, models) rides the `initialize` handshake registrations
  and `registry/update`. `ExtensionHost::providers()` surfaces the merged set so a
  host can present extension providers in its model surface.
- **Proxied streaming** — `ExtensionLlmProvider` implements the engine's
  `LlmProvider` trait, so an extension-registered provider is a drop-in for the
  native `LlmClient` at the agent-loop seam. The host sends `provider/complete`;
  the extension streams `provider/delta` notifications (serialized `StreamEvent`s)
  keyed by a `request_id`, then replies with the final result. Deltas are routed
  by a shared `ProviderStreams` registry and terminated cleanly when the request
  resolves; ordering (deltas before the terminal `Done`) rides the process's
  single ordered reader.
- **OAuth round-trips** — `ExtensionHost::provider_oauth_login` /
  `provider_oauth_refresh` send `provider/oauth_login` / `provider/oauth_refresh`
  to the owning extension, which drives any user interaction back over the
  existing `ui/*` surface and returns a `ProviderCredentials` bundle.
- **`session/set_model`** — a new tier-guarded (command-tier + current-epoch)
  `HostDelegate::session_set_model`, carrying an optional `provider` and
  `thinking` level, so an extension can switch the active model to an
  extension-registered provider/model. Plus a `model_select` SEP event name.

Additive: nothing runs unless a host attaches an `ExtensionHost`. The reference
`sep-echo-peer` gains a `SEP_ECHO_PROVIDER` mode exercising the whole path live.
