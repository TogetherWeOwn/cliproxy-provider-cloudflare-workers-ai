# Cloudflare Workers AI provider for CLIProxyAPI

First-class account/token auth files, namespaced model discovery, streaming and
non-streaming OpenAI chat execution, a **daily neuron quota card** and a
**free-only hard stop** that is on by default. Uses the
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit) at `21069bf`
and the real **CLIProxyAPI v8.0.49** C-ABI loader (ABI 1, RPC schema 10). Requires CLIProxyAPI
**v8.0.49 or newer**: the kit refuses hosts older than RPC schema 10.

Multiple auth files use native routing (`round-robin`, weighted routing or
fill-first) and native per-auth/model cooldown. There is **no Scheduler** and no
background polling. Before each request is sent, the executor checks the
account's daily neuron usage. It refuses any request that could go over the
free daily allocation. If the usage reading is missing or stale, it also
refuses. See [Free-only hard stop](#free-only-hard-stop). The plugin never
changes a Workers plan, enables paid usage, or upgrades an account.

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
  "stay_within_free": true,
  "safety_margin_neurons": 500,
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
  inference-key suspension. With `stay_within_free` on, the hard stop then
  refuses inference (`quota_unverified`), because usage cannot be checked.
- `daily_free_neurons`: optional positive finite number; default **10,000**, from
  the official pricing page. This sets the card's capacity and the hard stop's
  ceiling. It does not change the upstream allowance. Do not increase it to
  invent free usage. When several auths share one account, the hard stop uses
  the **lowest** value any of them has carried since the plugin loaded.
- `stay_within_free`: optional boolean; default **true**. Turns on the free-only
  hard stop. Set it to `false` only for an account where paid usage is
  approved; the plugin then behaves like v0.1.1 (no usage check before
  dispatch, no output cap).
- `cold_start_hold_seconds`: optional number from 0 to 86,400; default **600**
  (one tally window). After the plugin instance starts (CLIProxy restart or
  plugin reload), the hard stop refuses with a retryable `rate_limit` for this
  long, because a new instance cannot see what an earlier one admitted while
  analytics still lag. Set `0` only where no earlier instance can have
  admitted requests in the last 10 minutes.
- `safety_margin_neurons`: optional positive finite number; default **500**.
  This many neurons are kept unused below `daily_free_neurons`. The margin
  absorbs estimate errors, sampled analytics and other consumers. A margin at or
  above the capacity refuses every request. When several auths share one
  account, the hard stop uses the **highest** margin any of them has carried
  since the plugin loaded. Raising the capacity or lowering the margin of a
  shared account takes effect after a plugin reload.
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
Unavailable/malformed discovery falls back to a small static catalog.

The catalog is public, not per-account availability: in a live check on
2026-10-31, 7 of 39 listed text-generation models failed a minimal chat request
for one account (403 not allowed for the account, 403 license agreement not yet
accepted, 400 non-chat input schema, 400 model has no config). Discovered and
fallback IDs are therefore **validated per account** with a one-token
non-streaming chat probe (`max_tokens: 1`; non-streaming because some models,
e.g. `@cf/meta/llama-guard-3-8b`, reject `stream: true` but serve requests
without it):

- 400, 403, 404, 410 or 422: not advertised for this account.
- 2xx: advertised. 429, 5xx, timeouts and transport errors are inconclusive and
  keep the model, so a transient problem never empties the catalog.
- At most 8 probes run at once across all discoveries, each bounded to 10 s, the whole validation to
  30 s. Definitive verdicts are cached per account/token digest for 6 h;
  inconclusive models are re-probed on the next discovery.
- Each probe uses only a few neurons. With `stay_within_free` on, each probe goes
  through the hard stop like inference (charged by the same estimate, with
  `max_tokens: 1`). A refused probe is not sent, and the model stays listed.
  A model without a published neuron price is never advertised or served while
  the hard stop is on: it is dropped from the catalog without a probe. The plugin never submits a model
  license agreement (`agree`) on the operator's behalf; accept it yourself and
  the model is advertised after the cached verdict expires or the plugin reloads.

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

With `stay_within_free: false`, the final host-translated OpenAI payload is
preserved with two exceptions: the model prefix is removed, and the stream flag
is set to match the execution method (a raw `HttpRequest` keeps the body's own
flag). With `stay_within_free` on (the default), the upstream body is
**rebuilt** from an allowlisted, text-only request shape instead (see
[Accepted request shape](#accepted-request-shape-with-the-hard-stop-on)); the
estimate is computed from exactly the rebuilt bytes. Streams
are relayed incrementally. All inference/discovery requests use the injected
**host HTTP client**. Arbitrary raw-HTTP destinations, methods and caller
credentials are not forwarded. Token counting is local and explicitly estimated.

**v0.1 is chat-only.** Cloudflare documents embeddings separately, and Responses
only for GPT-OSS, with `stream:false`. This plugin does not advertise an embedding
executor or a Responses protocol, and does not send requests to those endpoints.
Clients using other protocols must use the host's chat translation support.

## Daily neuron quota and free-only hard stop

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
Adaptive analytics can be sampled or delayed, and other consumers may use the
same account. The card only displays usage. Enforcement is done by the hard
stop below. No automatic reset is supported; UTC rollover is an upstream daily
window.

### Free-only hard stop

On Workers Paid, every neuron above the free daily allocation is billed. With
`stay_within_free` on (the default), the executor checks each request before
`Execute`, `ExecuteStream` or raw `HttpRequest` sends it. Catalog validation
probes are checked the same way. A request is sent only if:

```text
reading + tally + estimate + safety_margin_neurons < daily_free_neurons
```

- **reading**: today's (UTC) account-wide `totalNeurons`. It uses the GraphQL
  query above, sent with the plugin's own bounded analytics client (the one
  the quota card uses), **not** the host HTTP client of the inference request:
  that client would record the GraphQL request, analytics bearer token
  included, in the inference request's API log and count it as an upstream
  attempt. The read follows the same proxy policy as inference: the auth's
  `proxy_url`, else the host's global `proxy-url`. The plugin learns the
  global setting from the host's auth and model requests; until it has seen
  it, an auth without its own `proxy_url` is refused (`quota_unverified`)
  rather than read direct. An invalid proxy also fails the read. A per-call
  proxy override that another plugin passes to a host model execution is not
  visible to the plugin and does not apply to the read. Each account has one
  reading, shared by all its
  tokens. The reading is cached for at most **60 s**, and only one read per
  account runs at a time. If a refresh fails, a same-day reading up to
  **2 minutes** old is still used. A reading from before 00:00 UTC is never
  used after it, and neither is a read that crosses midnight.
- **tally**: analytics lag. So every request allowed through in the last
  **10 minutes** stays charged at its estimate, even after a newer reading may
  already include it. Counting a request twice is safe; missing one is not.
  Entries from just before midnight still count after it, which is also safe.
- **estimate**: `ceil(((body_bytes + 512) × input_rate + max_tokens × output_rate) / 1e6)`.
  `body_bytes` is the length of the rebuilt upstream body, the exact bytes
  sent; 512 input tokens cover the model's chat template; `max_tokens` is the
  integer written into that body. The rates are the model's neurons per million
  tokens from the official
  [pricing table](https://developers.cloudflare.com/workers-ai/platform/pricing/)
  (retrieved 2026-11-01). A model not in the table is refused (`invalid_model`,
  HTTP 400) and not advertised: it cannot be bounded. With text-only content
  every token is at least one byte, so the body's byte length bounds the
  prompt tokens; JSON framing only overcounts. Cached input is charged at the
  uncached rate.

#### Accepted request shape with the hard stop on

The client body is decoded **once**, with exact, case-sensitive keys, and the
upstream body is re-marshalled from those decoded values. The guard and
Cloudflare therefore read the same request: there is no second parse of the
client body in which Go's case-insensitive field matching, Unicode case folding
(the Kelvin sign `K` U+212A as `k`, long s `ſ` U+017F as `s`) or a duplicate key
could make the two disagree. Only this text-only shape is accepted; anything
else is refused before any analytics read or dispatch with `invalid_request`
(HTTP 400, not retryable, no cooldown):

- **Top-level keys**, exactly: `model`, `messages`, `max_tokens`,
  `max_completion_tokens`, `temperature`, `top_p`, `top_k`, `seed`, `stop`,
  `stream`, `stream_options` (only `include_usage`, a boolean),
  `frequency_penalty`, `presence_penalty`, `repetition_penalty`, plus `n` and
  `best_of` only with the value `1` (not forwarded). Sampling fields must be
  numbers and `stop` a string or at most 4 strings. `tools`, `tool_choice` and
  `response_format` are refused: chat templates render tool and schema JSON
  into far more tokens than its bytes (deeply nested tool parameters measured
  above 13x), so no body-length estimate bounds them. Any other key (for
  example `prompt`, `image`, `audio`, `raw`, `lora`, `logprobs`), any case
  variant (`Max_Tokens`, `max_toKens` with U+212A) and any key given twice is
  refused.
- **Messages**: an array of objects with only the keys `role` (string,
  required), `content` and `name` (string), each at most once, exact case.
  `tool_calls` and `tool_call_id` are refused for the same reason as `tools`.
- **Content**: a string, `null`, or an array of parts that are each exactly
  `{"type":"text","text":<string>}` (no other keys, no case variants).
  `image_url`, `input_audio`, `file` or any other part is refused: images and
  audio can expand to far more tokens than their bytes.
- **Output limit**: `max_tokens` and `max_completion_tokens`, when present and
  not `null`, must be integers of at least 1 (`0.0001`, `1.5`, `0`, `"5"` and
  `1e300` are refused). The rebuilt body carries one `max_tokens`: the lower of
  the two given, else 4,096; `max_completion_tokens` is not forwarded.
- The rebuilt body contains only these fields with lowercase keys: `model`
  (prefix removed), `stream` (the execution method's flag; for a raw
  `HttpRequest`, the body's boolean `stream`), `max_tokens`, the rebuilt
  `messages`, and the other allowlisted fields re-encoded from their decoded
  numbers, strings and booleans. Every byte of the rebuilt body is charged as
  input.

Callers that need another shape need an auth with `stay_within_free: false`,
which forwards the payload as before; use it only where paid usage is approved.

Refusals never reach Cloudflare:

| Condition | Plugin error | Retry hint |
| --- | --- | --- |
| `estimate + safety_margin_neurons >= daily_free_neurons` (cannot fit even at zero usage) | `request_too_large`, HTTP 400 | None; lower `max_tokens` or the prompt |
| Request shape outside the allowlist (see above) | `invalid_request`, HTTP 400 | None; send the accepted text-only shape |
| Model without a published neuron price | `invalid_model`, HTTP 400 | None; use a priced model |
| `reading + estimate + safety_margin_neurons >= daily_free_neurons` (the reading alone leaves no room) | `quota_exhausted`, HTTP 429, non-retryable | Until next 00:00 UTC |
| Within `cold_start_hold_seconds` of the plugin instance starting | `rate_limit`, HTTP 429, retryable | Until the hold ends (default at most 10 minutes) |
| Only the tally pushes the check over | `rate_limit`, HTTP 429, retryable | Until the oldest tally entry leaves the 10-minute window (at most 10 minutes) |
| No reading, failed read, reading older than 2 minutes, unknown host proxy, or read across midnight | `quota_unverified`, HTTP 503 | 60 seconds |

The `quota_exhausted` refusal keeps that auth/model in native cooldown until the
UTC reset; it is returned only when today's measured usage leaves no room for
the request. A refusal caused only by recent admissions still in the tally is a
`rate_limit` whose retry hint is at most 10 minutes, so the host cools the
auth/model down for minutes, not for the rest of the day. The plugin never
opens the gate just because analytics might be stale. Set
`stay_within_free: false` only where paid usage has been approved.

The `quota_unverified` refusal is shaped so a connector can tell it apart from
a downstream 429: HTTP 503 (never 429), a 60-second retry hint, and a
`quota_unverified:` message prefix. The prefix matters because the host renders
refusals to clients as `{"error": message}` and the plugin error code never
reaches them; match on the 503 status plus the prefix, since "refused to avoid
paid usage" alone is shared with `quota_exhausted`. The refusal cools the model
down briefly and never marks quota exhausted, so a fixable analytics-permission
problem does not sideline the account until the UTC reset.

**Overshoot bound.** Two assumptions:

1. Each request costs at most its estimate.
2. Cloudflare analytics show a request's neurons within **8 minutes** of its
   admission (the 10-minute tally window minus the 2-minute maximum reading
   age). This includes the request's run time.

If both hold, every admission keeps the measured usage plus all uncounted
admissions below `daily_free_neurons − safety_margin_neurons` (9,500 by
default). So the plugin's own traffic **never exceeds the free allocation**,
and the whole margin is spare. If the assumptions fail, let `U` be the neurons
the check could not see. Sources of `U`:

- other consumers of the same account, including a second CLIProxy instance
  (the tally is per process);
- adaptive-sampling error in analytics;
- requests that cost more than their estimate;
- requests whose neurons take longer than 8 minutes to appear in analytics;
- admissions lost to a plugin restart, only when `cold_start_hold_seconds` is
  set below the 10-minute tally window.

The paid overshoot is then at most `max(0, U − safety_margin_neurons)`. At
the published $0.011 per 1,000 neurons, the default 500-neuron margin covers
about $0.0055 of unseen usage per day. Run one CLIProxy instance per account,
or raise the margin to cover the others.

## Failure classification and host limitation

| Upstream result | Plugin error | Retry hint |
| --- | --- | --- |
| 401 | `invalid_credentials` | Non-retryable |
| 403, including model terms/paid-plan/account restriction | `entitlement_required` | Non-retryable; not automatically called bad credentials |
| 404 / 3042; 400 / 5007 | `upstream_not_found` | Non-retryable; preserve actual HTTP status |
| 429 / 3036 or daily-neuron-limit wording | `quota_exhausted` | Until next UTC reset; shorter gateway hints cannot shorten it |
| 429 / 3040 or other rate/capacity 429 | `rate_limit` | 5 seconds, or valid upstream `Retry-After` |
| 408 / 5xx | `upstream_error` | Retryable, honoring `Retry-After` |

The free-only hard stop's own refusals (`invalid_request`, `request_too_large`,
`quota_exhausted` and `rate_limit` before dispatch, `quota_unverified`) are
listed in [Free-only hard stop](#free-only-hard-stop).

Daily hints can be extended by a longer upstream `Retry-After`. Bodies, account
IDs, tokens and transport diagnostics never appear in public plugin errors.
These mappings drive **reactive native cooldown**, not plugin account selection.

The v8.0.49 RPC error adapter carries status/retry timing but **drops `retryable`
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
with `objdump -T`. It creates `cloudflare-workers-ai_0.1.6_linux_amd64.zip`
containing `cloudflare-workers-ai.so` and `LICENSE`, plus archive `checksums.txt`.
It never publishes or touches a running host. Publish both files with a matching
release; the schema-v1 `registry.json` uses native `github-release` installation.
Registry metadata is not a claim that a release has already been published.

CI checks formatting, vet, race tests, packaging/glibc, real v8.0.49 loading,
gitleaks and public-file hygiene. The `hostcompat` build tag permits only
loopback mock upstreams and is **test-only**; never release that binary.

## Pending live verification

- Validate the exact GraphQL field/filter types and Account Analytics Read
  permission for the intended account through the official live schema explorer.
- Verify current model discovery, account entitlements, chat streaming and token
  reporting on a separately authorized isolated staging CLIProxy.
- Verify native auth/quota UI and UTC reset behavior without deliberately draining
  an account. Confirm any existing paid-plan exposure before inference.
- Measure the real analytics delay for the account. Confirm it stays under the
  hard stop's 8-minute assumption before relying on the zero-overshoot bound.
- Resolve the upstream RPC transient-marker gap and update the pinned host test
  before claiming full native parity or recommending live cutover.
- Obtain an independent review and publish reviewed release assets before store
  installation; no live deployment or account validation is claimed here.
