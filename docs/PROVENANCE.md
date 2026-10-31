# Provenance

The GraphQL neuron query, its account-wide/no-dimension design and the
analytics parser cases are adapted from the MIT-licensed
[TogetherWeOwn/cpa-plugin-subscription-pool](https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool)
commit `ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`:

- `internal/providers/cloudflare/quota.go`: query and parser (ported, with the
  `$account: string!` scalar correction described in [SOURCES](SOURCES.md)).
- `internal/providers/cloudflare/quota_test.go`: parser fixture cases.
- `internal/providers/cloudflare/module.go` and `config.go`: 3036-versus-3040
  response classification and the 10,000-neuron/00:00 UTC daily window.

Not ported, deliberately: that module's admission/reservation engine, pending
in-flight neuron ledger, safety margin, pay-as-you-go guard and per-model
neuron estimates. Those implement a global Scheduler with pre-emptive quota
avoidance; this plugin follows native CLIProxy provider behavior instead
(multiple auth records, native routing, reactive cooldown from classified
errors, display-only quota card).

The C-ABI wiring, host-compatibility harness layout, release packaging, CI and
document structure follow the sibling
[cliproxy-provider-synthetic](https://github.com/TogetherWeOwn/cliproxy-provider-synthetic)
v0.1.0 and the published
[cliproxy-provider-kit](https://github.com/TogetherWeOwn/cliproxy-provider-kit) v0.1.0.
Auth, model discovery, executor and quota-provider code in this repository is
new. The MIT license is included at the repository root and in release archives.
