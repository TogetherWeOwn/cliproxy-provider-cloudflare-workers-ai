# Migration: staging CLIProxy first, then cutover by hot config reload

This is an operator runbook, not an automated deployment. The build, test and
packaging commands in this repository never touch a running CLIProxy. Every
change below is applied through CLIProxy's **hot config reload** (the config
file watcher and auth-directory watcher), not by restarting a container.
Do not cut over while the README's pinned-host transient-429 gap is unresolved.

Why hot reload is enough: a config change is picked up by the watcher
(`internal/watcher/config_reload.go`), which re-applies plugin runtime config
(`sdk/cliproxy/service_plugins.go` -> `internal/pluginhost/host.go`
`ApplyConfig`). Enabling a plugin loads and registers it; disabling it detaches
its executor, auth provider and quota provider. Auth files are picked up by the
auth-directory watcher.

## 1. Prepare rollback and validate the artifact

1. Privately back up the target instance's config file, auth directory, plugin
   directory and plugin settings, preserving permissions. Never commit the
   backup or any credential.
2. Use a reviewed release: download `cloudflare-workers-ai_<version>_linux_amd64.zip`
   and `checksums.txt`, and verify with `sha256sum -c checksums.txt`. Never
   install a library built with the `hostcompat` tag (test-only).
3. Record today's callers and model IDs routed to Cloudflare, and how quota is
   displayed today, so the cutover can be compared.
4. Confirm the account plan. On Workers Paid, usage above 10,000 neurons/day is
   billed. The plugin's free-only hard stop (`stay_within_free`, default on)
   refuses requests that could exceed the free allocation, and refuses when
   usage cannot be verified. Read its overshoot bound in the README. Keep it on
   unless paid usage is approved.
5. With the hard stop on, check today's callers against the requests it now
   refuses with HTTP 400 `invalid_request`: image, audio or file content parts
   (only text content is accepted), a `max_tokens`/`max_completion_tokens`
   that is not an integer of at least 1, and `n`/`best_of` above 1. Move such
   callers to an auth with `stay_within_free: false` only where paid usage is
   approved. A refusal caused only by recent admissions is a retryable 429
   `rate_limit` (at most 10 minutes); only a measured reading that leaves no
   room returns `quota_exhausted` until 00:00 UTC.

## 2. Create an isolated staging instance

Run a **second CLIProxy instance** with its own config file, auth directory,
plugin directory and loopback-only port, on the same reviewed host version.

A second instance is required because the plugin's provider key is
`cloudflare-workers-ai`. If a native `openai-compatibility` entry with that
name exists in the same instance, its executor pre-empts the plugin executor
(`internal/pluginhost/adapters_executors.go`) and model claims are first-wins,
so a same-instance test would not exercise the plugin.

Illustrative staging config (all values are placeholders):

```yaml
auth-dir: <staging-auth-directory>
plugins:
  enabled: true
  dir: <staging-plugin-directory>
  store-sources:
    - https://raw.githubusercontent.com/TogetherWeOwn/cliproxy-provider-cloudflare-workers-ai/main/registry.json
  configs:
    cloudflare-workers-ai:
      enabled: true
```

Install through the native plugin store (it places the library under
`<plugin-dir>/linux/amd64/` and enables the config), or copy the verified
library into that platform directory and save the config; the watcher applies
it. Confirm registration (plugin metadata version, auth provider and quota
provider present) through the management API rather than by file presence.

## 3. Migrate credentials

Today's setup is typically one `openai-compatibility` entry named
`cloudflare-workers-ai` whose `base-url` embeds the account ID
(`.../client/v4/accounts/<account-id>/ai/v1`) with one or more `api-key`
entries, plus an optional subscription-pool `cloudflare` overlay block.

For each distinct account/token pair, write **one** private auth file (mode
`0600`) in the staging auth directory using the README schema:

- `account_id`: the 32-hex ID taken from the old `base-url`.
- `api_key`: the old `api-key` value (a Workers AI Read/Write API token).
- `analytics_api_key`: optional separate token with Account Analytics Read;
  add one if the inference token lacks that permission. Without it, the quota
  card shows an analytics error and, with `stay_within_free` on, inference is
  refused (`quota_unverified`) because usage cannot be checked.
- `daily_free_neurons`: omit to use 10,000.
- `stay_within_free` / `safety_margin_neurons`: omit to use `true` / 500.
  Auths on one account share the lowest `daily_free_neurons` and the highest
  `safety_margin_neurons` among them, so give them the same values.

Do not set `id`; identity is derived from account ID + token. Several tokens
on one account share **one** daily allocation; each card will show the same
account-wide usage. Do not print tokens into shell history, logs or tickets.

In staging, do **not** add the old `openai-compatibility` entry and do not add
the subscription-pool `cloudflare` block. This plugin registers no scheduler.

## 4. Staging acceptance

The repository tests use mocks only. These account checks need separate
authorization and are performed by an operator, not by this repository:

1. Auth parse and persistence; duplicate-file rejection; stable auth ID across
   a reload of the same file.
2. Quota card: one `daily` bucket, reset at the next `00:00:00Z`, usage matching
   the dashboard's Workers AI usage within analytics delay. If the GraphQL query
   is rejected, inspect the live schema by introspection before changing code.
3. Model list contains only `cloudflare/@cf/...` IDs; the executor sends the
   unprefixed `@cf/...` ID upstream.
4. One small non-stream and one small stream chat request; usage tokens are
   reported. Do not intentionally drain the daily allocation. If the auth or
   host uses a proxy, confirm the analytics read leaves through it and that
   the request API log of an inference request holds no GraphQL request.
5. With two auth files on different accounts, a mock or naturally occurring
   3036 cools down only the affected auth until 00:00 UTC while the sibling
   keeps serving.

## 5. Cut over (target instance, hot reload only)

1. Keep the private rollback snapshot.
2. Add the verified library to the target plugin directory (or install via the
   store) and write the auth files from step 3.
3. In **one** config save: remove the `cloudflare-workers-ai`
   `openai-compatibility` entry, remove only the subscription-pool `cloudflare`
   block, and set `plugins.configs.cloudflare-workers-ai.enabled: true`. The
   watcher applies the change; no container restart.
4. Update callers to the new IDs:

   | Before (illustrative compat alias) | After |
   | --- | --- |
   | `<old-alias>/qwen3-30b-a3b-fp8` | `cloudflare/@cf/qwen/qwen3-30b-a3b-fp8` |
   | `<old-alias>/<model>` | `cloudflare/@cf/<publisher>/<model>` |

5. Verify executor ownership, auth list, quota card and one low-volume request.

## 6. Roll back (hot reload only)

1. In one config save, set `plugins.configs.cloudflare-workers-ai.enabled: false`
   and restore the `openai-compatibility` entry and subscription-pool block
   from the snapshot. The watcher detaches the plugin and the native entry
   serves again. Restore caller model IDs.
2. Move the plugin auth files out of the auth directory if they should no
   longer be listed.
3. Note: a Go C-shared library cannot be unloaded from a running process; after
   logical disable it stays resident but receives no calls. Evicting it from
   memory only happens at the next normally scheduled restart; it is not a
   rollback step.
