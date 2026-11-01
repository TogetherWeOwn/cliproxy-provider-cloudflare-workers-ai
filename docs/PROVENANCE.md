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

Not ported, deliberately: that module's Scheduler, durable reservation engine,
pending ledger and pay-as-you-go fence. This plugin follows native CLIProxy
provider behavior instead: multiple auth records, native routing, and reactive
cooldown from classified errors. The free-only hard stop in `guard.go` is new
code. It keeps the prior module's intent and its defaults (`stayWithinIncluded`
on, a 500-neuron safety margin, a 2-minute maximum reading age). It runs as an
executor admission check, not a scheduler. Its per-model rates are taken
directly from the official pricing table (see [SOURCES](SOURCES.md)).

The C-ABI wiring, host-compatibility harness layout, release packaging, CI and
document structure follow the sibling
[cliproxy-provider-synthetic](https://github.com/TogetherWeOwn/cliproxy-provider-synthetic)
v0.1.0 and the published
[cliproxy-provider-kit](https://github.com/TogetherWeOwn/cliproxy-provider-kit) v0.1.0 (now pinned at `de8b318`).
Auth, model discovery, executor and quota-provider code in this repository is
new. The MIT license is included at the repository root and in release archives.
