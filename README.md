# Cloudflare Workers AI provider for CLIProxyAPI

First-class account/token auth files, namespaced model discovery, streaming and
non-streaming OpenAI chat execution, and a **daily neuron quota card**. Uses the
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit) at `f24cf1f`
and the real **CLIProxyAPI v8.0.48** C-ABI loader (ABI 1, RPC schema 9).

Multiple auth files use native routing (`round-robin`, weighted routing or
fill-first) and native per-auth/model cooldown. There is **no Scheduler**, local
reservation engine, preemptive quota-based avoidance or background polling.
Quota is **display-only**, not a billing cap. The plugin never changes a Workers
plan, enables paid usage, or upgrades an account.

**Pending live verification.** All provider tests use loopback mocks. The exact
account GraphQL schema/permissions and deployed quota UI have not been verified.
The pinned host also has a verified transient-429 classification limitation
below. Do not claim full native transient/exhaustion parity or production
readiness. See [migration](docs/MIGRATION.md), [sources](docs/SOURCES.md) and
[provenance](docs/PROVENANCE.md).

## Auth file

Create one private JSON file per account/token pair in CLIProxy's auth directory,
mode `0600`. Never save actual credentials in this repository.

```json
{
  "type": "cloudflare-workers-ai",
  "account_id": "<cloudflare-account-id>",
  "api_key": "<workers-ai-api-token>",
  "analytics_api_key": "<optional-separate-analytics-read-token>",
  "daily_free_neurons": 10000,
  "label": "<account-label>",
  "weight": 1,
  "priority": 0,
  "disabled": false
}
```

- `account_id`: account-scoped 32-hex ID, normalized to lowercase.
- `api_key`: Cloudflare API **token**, not a Global API key. Inference/model
  discovery require the chosen account's Workers AI Read or Write permission.
- `analytics_api_key`: optional separate Account Analytics Read token, restricted
  to the same account. If omitted, `api_key` must carry both permissions. The
  inference API does not receive the separate analytics token. Missing analytics
  permission yields an unknown/error card, never a fabricated zero reading or
  inference-key suspension.
- `daily_free_neurons`: optional positive finite number; default **10,000**, from
  the official pricing page. This configures the card's capacity, not an upstream
  allowance or spending cap. Do not increase it to invent free usage.
- Native `prefix`, `proxy_url`, weights, priorities, label and disabled fields
  remain host-owned. An optional auth prefix does not replace the fixed namespace.

Auth ID is `cloudflare-workers-ai:` plus the first 12 hex characters of SHA-256
of `normalized_account_id + NUL + trimmed_api_key`. User-supplied IDs are rejected.
The analytics token and card-capacity changes do not alter identity. An identical
account/token pair in two files is rejected; reparsing one filename is safe.
Changing that file's token releases its old identity. For renames/deletions,
clear the plugin's in-memory ownership through a supported plugin lifecycle
before reusing an identity in another filename.

Different API tokens on **one account share the same daily allocation**. Their
cards report the same account-wide consumption; never add these capacities as
independent entitlements. Login/poll report API-key-only; refresh preserves the
record and makes no upstream call.

## Models and execution

Discovery uses the official `GET /client/v4/accounts/{account_id}/ai/models/search`
with `task=Text Generation`, `page` and `per_page=100`. Results are bounded to 20
pages, deduplicated, and filtered to supported text-generation model names.
Unavailable/malformed discovery falls back to a small static catalog. A fallback
entry is not proof of entitlement or current model availability.

Every registered/discovered model gets the fixed **`cloudflare/`** namespace.
The executor strips exactly that leading prefix, preserving the upstream's
`@cf/` namespace and organization/model path.

| Client model ID | Upstream model ID |
| --- | --- |
| `cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8` | `@cf/meta/llama-3.1-8b-instruct-fp8` |
| `cloudflare/@cf/qwen/qwen3-30b-a3b-fp8` | `@cf/qwen/qwen3-30b-a3b-fp8` |
| `cloudflare/@cf/openai/gpt-oss-120b` | `@cf/openai/gpt-oss-120b` |
| `cloudflare/@cf/openai/gpt-oss-20b` | `@cf/openai/gpt-oss-20b` |

Chat uses:

```text
POST https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1/chat/completions
Authorization: Bearer <workers-ai-api-token>
```

The final host-translated OpenAI payload is preserved except for the model
prefix and the stream flag, which is set to match the execution method. Streams
are relayed incrementally. All inference/discovery requests use the injected
**host HTTP client**. Arbitrary raw-HTTP destinations, methods and caller
credentials are not forwarded. Token counting is local and explicitly estimated.

**v0.1 is chat-only.** Cloudflare documents embeddings separately, and Responses
only for GPT-OSS, with `stream:false`. This plugin does not advertise an embedding
executor or a Responses protocol, and does not send requests to those endpoints.
Clients using other protocols must use the host's chat translation support.

## Daily neuron quota

The official free allocation is **10,000 neurons per account per day**, resetting
at **00:00 UTC**, without rollover. The capacity can be configured in the auth
file. Account-wide usage comes from a read-only GraphQL query:

```graphql
query WorkersAINeurons($account: string!, $date: Date!) {
  viewer {
    accounts(filter: {accountTag: $account}) {
      accountTag
      aiInferenceAdaptiveGroups(limit: 1, filter: {date_geq: $date, date_leq: $date}) {
        sum { totalNeurons }
      }
    }
  }
}
```

Sent via `POST https://api.cloudflare.com/client/v4/graphql`, with UTC date
variables and Bearer analytics auth. No per-model or per-token dimension narrows
this account-wide total. The query is ported from the prior provider with one
correction: `$account` is declared with Cloudflare's lowercase `string` scalar,
as in the official GraphQL examples (the port declared `String!`, which the
schema would reject). The dataset, `date_geq`/`date_leq: Date` filters and
`sum.totalNeurons: float64!` are corroborated by a public schema snapshot.
Official docs explain live schema discovery but do not publish this
field/filter combination in a static Workers AI example. **Do not mistake mock/schema-snapshot evidence for a
successful live GraphQL canary.** See the explicit evidence boundary in
[Sources](docs/SOURCES.md).

One group contains one `window: daily` bucket:

- capacity: configured daily free neurons;
- used: today's analytics `sum.totalNeurons`;
- remaining fraction: `max(0, 1 - used/capacity)`;
- reset time: next `00:00:00Z`.

The summary reports capacity, consumed neurons and `neurons_above_free`.
The last value is measured total minus the configured free capacity, floored at
zero; it is **not an invoice, proof of paid-plan status, or confirmed billed
usage**. No dollar charge or plan entitlement is fabricated.

Successful readings have the kit's 60-second per-auth/credential cache, discarded
at UTC midnight. A read crossing midnight is rejected and retried, not cached
under the new day. Missing accounts, mismatched account IDs, null/missing neuron
fields, partial GraphQL errors, oversized bodies and permission errors fail
closed for the card. An explicit empty dataset means no measured usage.

GraphQL polling has a 15-second timeout, 1 MiB body cap, no redirects, redacted
errors and no inference side effects. It goes through the auth's `proxy_url`, else the
host's global `proxy-url`, like inference. Kit v0.1.0's quota helper only supports
GET, so this provider uses an equivalent bounded client for GraphQL POST.
Adaptive analytics can be sampled/delayed and other account consumers may exist.
**This display cannot enforce free-only billing on a Workers Paid account.**
No automatic reset is supported; UTC rollover is an upstream daily window.

## Failure classification and host limitation

| Upstream result | Plugin error | Retry hint |
| --- | --- | --- |
| 401 | `invalid_credentials` | Non-retryable |
| 403, including model terms/paid-plan/account restriction | `entitlement_required` | Non-retryable; not automatically called bad credentials |
| 404 / 3042; 400 / 5007 | `upstream_not_found` | Non-retryable; preserve actual HTTP status |
| 429 / 3036 or daily-neuron-limit wording | `quota_exhausted` | Until next UTC reset; shorter gateway hints cannot shorten it |
| 429 / 3040 or other rate/capacity 429 | `rate_limit` | 5 seconds, or valid upstream `Retry-After` |
| 408 / 5xx | `upstream_error` | Retryable, honoring `Retry-After` |

Daily hints can be extended by a longer upstream `Retry-After`. Bodies, account
IDs, tokens and transport diagnostics never appear in public plugin errors.
These mappings drive **reactive native cooldown**, not plugin account selection.

The v8.0.48 RPC error adapter carries status/retry timing but **drops `retryable`
and the native transient-rate-limit marker**. Consequently a transient 3040
still sets model quota-exceeded state, although the short retry timing survives.
The real-loader test characterizes this limitation and separately proves daily
exhaustion cooldown until UTC reset plus healthy-sibling rotation. No local core
patch, status disguise or custom scheduler is supplied. Other HTTP cooldown
scope/duration, including model 404 handling, remains host policy.

## Build, test and package

Go **1.26.10**, CGO, a C compiler, `nm`, `objdump`, Python 3 and `zip` are required.
There are no `replace` directives or local sibling-module dependencies.

```sh
go vet ./...
go test -race -count=1 ./...
go -C integration/hostcompat vet ./...
go -C integration/hostcompat test -race -count=1 ./...
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o <output-dir>/cloudflare-workers-ai.so .
nm -D <output-dir>/cloudflare-workers-ai.so | grep cliproxy_plugin_init
bash scripts/package-release.sh <output-dir>
```

The package script refuses libraries needing glibc newer than **2.34**, checked
with `objdump -T`. It creates `cloudflare-workers-ai_0.1.0_linux_amd64.zip`
containing `cloudflare-workers-ai.so` and `LICENSE`, plus archive `checksums.txt`.
It never publishes or touches a running host. Publish both files with a matching
release; the schema-v1 `registry.json` uses native `github-release` installation.
Registry metadata is not a claim that a release has already been published.

CI checks formatting, vet, race tests, packaging/glibc, real v8.0.48 loading,
gitleaks and public-file hygiene. The `hostcompat` build tag permits only
loopback mock upstreams and is **test-only**; never release that binary.

## Pending live verification

- Validate the exact GraphQL field/filter types and Account Analytics Read
  permission for the intended account through the official live schema explorer.
- Verify current model discovery, account entitlements, chat streaming and token
  reporting on a separately authorized isolated staging CLIProxy.
- Verify native auth/quota UI and UTC reset behavior without deliberately draining
  an account. Confirm any existing paid-plan exposure before inference.
- Resolve the upstream RPC transient-marker gap and update the pinned host test
  before claiming full native parity or recommending live cutover.
- Obtain an independent review and publish reviewed release assets before store
  installation; no live deployment or account validation is claimed here.
