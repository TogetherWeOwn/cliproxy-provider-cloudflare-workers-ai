# Sources for every wire detail

Retrieved 2026-10-31. Official Cloudflare documentation is the authority for
every endpoint, header, limit and error code below. Where official docs are
silent, the row says so and names the weaker evidence actually used. The wire
rows below were not established by calling a live Cloudflare endpoint, and all
repository tests use loopback `httptest` mocks. The only live observations are
the dated catalog-availability measurements in the README ("Models and
execution", 2026-10-31, one account, via a staging host): they motivate the
per-account validation probe and are not guarantees.

## Inference

| Wire detail | Implementation | Source |
| --- | --- | --- |
| Base URL `https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1` | `accountBase` + `/v1/...` in `executor.go` | [OpenAI compatible API endpoints](https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/) |
| `POST /v1/chat/completions`, `Authorization: Bearer <api_token>`, `Content-Type: application/json` | `buildRequest` | same page (cURL example) |
| Chat completions is supported by "most" text-generation models, not all | discovery keeps text-generation models; an unsupported model surfaces the upstream error | same page |
| `/v1/responses` exists **only for GPT-OSS models** and **only `stream: false`** | not used in v0.1; Responses clients are translated by the host to chat | same page |
| `/v1/embeddings` exists | not exposed in v0.1 (chat executor only) | same page |
| `options.rejectIfBusy` turns busy requests away with 3040 | passed through unchanged if a client sends it | same page; [errors](https://developers.cloudflare.com/workers-ai/platform/errors/) |
| Model IDs use the `@cf/<publisher>/<model>` form | `validModel` requires `@cf/`; plugin prepends `cloudflare/` and strips exactly that prefix | [Models catalog](https://developers.cloudflare.com/workers-ai/models/) |
| Static fallback IDs | `@cf/meta/llama-3.1-8b-instruct-fp8`, `@cf/qwen/qwen3-30b-a3b-fp8`, `@cf/openai/gpt-oss-120b`, `@cf/openai/gpt-oss-20b` (no deprecation notice on any page at retrieval) | [llama-3.1-8b-instruct-fp8](https://developers.cloudflare.com/workers-ai/models/llama-3.1-8b-instruct-fp8/), [qwen3-30b-a3b-fp8](https://developers.cloudflare.com/workers-ai/models/qwen3-30b-a3b-fp8/), [gpt-oss-120b](https://developers.cloudflare.com/workers-ai/models/gpt-oss-120b/), [gpt-oss-20b](https://developers.cloudflare.com/workers-ai/models/gpt-oss-20b/) |
| Default text-generation rate limit 300 requests/minute; Workers-Paid-only models 20/minute per account per model | informational; a rate-limit 429 is classified as transient | [Limits](https://developers.cloudflare.com/workers-ai/platform/limits/) |

The plain `@cf/meta/llama-3.1-8b-instruct` page returned 404 at retrieval, so
the fallback uses the documented `-fp8` variant.

## Model discovery

| Wire detail | Implementation | Source |
| --- | --- | --- |
| `GET /client/v4/accounts/{account_id}/ai/models/search` | `discoverModels` in `models.go` | [Models: search (API reference)](https://developers.cloudflare.com/api/resources/ai/subresources/models/methods/list/) |
| Query parameters `task`, `page`, `per_page` (also `author`, `search`, `source`, `hide_experimental`, `include_deprecated`, `format`) | sends `task=Text Generation`, `page`, `per_page=100` | same page |
| Token needs **Workers AI Read** or **Workers AI Write** | README auth-file section | same page |
| Envelope `{success, errors, messages, result}` | requires `success: true`, empty `errors`, non-null `result` | same page |
| `result[]` item fields (`name`, `task.name`) and `result_info.total_pages` | **Not specified** by the reference (items typed "unknown"). The parser reads `name`/`task.name` as returned by the API's model objects, stops at `total_pages` when present, otherwise at a short page, and is bounded to 20 pages. Any mismatch falls back to the static catalog. | Pending live verification |

## Daily neuron quota

| Wire detail | Implementation | Source |
| --- | --- | --- |
| Free allocation **10,000 neurons per day** | default `daily_free_neurons` = 10000, overridable per auth file | [Pricing](https://developers.cloudflare.com/workers-ai/platform/pricing/) |
| "All limits reset daily at **00:00 UTC**" | bucket `resetTime` = next `00:00:00Z`; cache dropped at UTC midnight | same page |
| Usage above the free allocation is billed at $0.011 per 1,000 neurons on Workers Paid; Workers Free cannot exceed it | `neurons_above_free` summary metric (neurons, not dollars; not an invoice) | same page |
| GraphQL endpoint `POST https://api.cloudflare.com/client/v4/graphql`, `Authorization: Bearer <token>`, JSON body `{query, variables}` | `fetchAnalytics` in `quota.go` | [Querying Workers metrics with GraphQL](https://developers.cloudflare.com/analytics/graphql-api/tutorials/querying-workers-metrics/) |
| `viewer { accounts(filter: {accountTag: $accountTag}) { ... } }`, with `accountTag` declared as the lowercase custom scalar `string` | query declares `$account: string!` | same page |
| Analytics token permission: **Account > Account Analytics > Read** | optional `analytics_api_key`; inference key is never required to hold it | [Configure an Analytics API token](https://developers.cloudflare.com/analytics/graphql-api/getting-started/authentication/api-token-auth/) |
| Live schema can be inspected with introspection | operator verification step | [Introspection](https://developers.cloudflare.com/analytics/graphql-api/features/discovery/introspection/) |
| Dataset `aiInferenceAdaptiveGroups` ("Aggregated AI Inference logs with adaptive sampling"), filter fields `date_geq: Date` / `date_leq: Date`, `sum.totalNeurons: float64!` | the exact selection in `neuronQuery` | **No official page publishes this field/filter combination.** Corroborated by a public third-party snapshot of the live schema: [Cloudflare GraphQL schema docs](https://pages.johnspurlock.com/graphql-schema-docs/cloudflare.html) (`AccountAiInferenceAdaptiveGroupsFilter_InputObject`, `AccountAiInferenceAdaptiveGroupsSum`). Pending live verification. |
| Adaptive datasets are sampled | card description says "delayed and sampled"; the hard stop's safety margin absorbs sampling error | dataset description in the schema snapshot above |
| Per-model LLM rates in neurons per million input/output tokens (retrieved 2026-11-01) | `neuronRates` in `guard.go`; with the hard stop on, a model missing from the table is refused and not advertised | [Pricing](https://developers.cloudflare.com/workers-ai/platform/pricing/) |

### Deviation from the ported query

The prior provider (see [PROVENANCE](PROVENANCE.md)) declared `$account: String!`.
Cloudflare's schema types `accountTag` (and every text field in the snapshot)
with the custom scalar `string`; the built-in `String` does not appear in the
snapshot at all, and the official tutorial declares `$accountTag: string`.
A `String!` variable in a `string!` position fails GraphQL variable validation,
so this plugin declares `$account: string!`. The `$date: Date!` declaration
matches the schema's `Date` scalar used by `date_geq`/`date_leq`.

## Error codes

All codes from [Workers AI errors](https://developers.cloudflare.com/workers-ai/platform/errors/):

| Code | HTTP | Official meaning | Plugin classification |
| --- | --- | --- | --- |
| 3036 | 429 | Account limited: "You have used up your daily free allocation of 10,000 neurons." | `quota_exhausted`, retry after next 00:00 UTC |
| 3040 | 429 | Out of capacity: "Capacity temporarily exceeded, please try again." (also `rejectIfBusy`) | `rate_limit`, retryable, 5 s or upstream `Retry-After` |
| 3042 | 404 | Invalid model ID | `upstream_not_found` |
| 5007 | 400 | No such model or task | `upstream_not_found` (status 400 preserved) |
| 5016, 5018, 3041, 3023, 5035 | 403 | Model terms, private model, account blocked, Workers Paid required | `entitlement_required`; never treated as a bad credential |
| 3007, 3008 | 408 | Timeout / aborted | `upstream_error`, retryable |

401 (`invalid_credentials`) and 5xx (`upstream_error`, retryable) follow
general HTTP semantics; the errors page has no Workers-AI-specific rows for
them. A 429 without code 3036 and without "daily"+"neuron" wording is treated
as transient, because the documented rate limits and code 3040 are not daily
exhaustion. `Retry-After` parsing (delta-seconds or HTTP-date) follows
[RFC 9110 section 10.2.3](https://www.rfc-editor.org/rfc/rfc9110#section-10.2.3).

## Host contract (CLIProxyAPI v8.0.48)

- `sdk/pluginabi/types.go`: ABI 1 / RPC schema 9 envelopes, `retry_after_seconds`.
- `internal/pluginhost/rpc_client.go`: RPC-to-host error adaptation (drops the
  transient marker; characterized in `integration/hostcompat/compat_test.go`).
- `sdk/cliproxy/auth/conductor_cooldown.go`: native per-auth/model cooldown.
- `internal/pluginhost/adapters_executors.go`: executor ownership; a native
  `openai-compatibility` executor for the same provider pre-empts the plugin.
- `internal/watcher/config_reload.go`, `sdk/cliproxy/service_plugins.go` and
  `internal/pluginhost/host.go` (`ApplyConfig`): hot reload of plugin config.
- `internal/pluginstore/registry.go`, `internal/pluginstore/github.go`:
  `registry.json` schema 1 and `<id>_<version>_<os>_<arch>.zip` + `checksums.txt`.
- Native model prefix precedent: `internal/registry/devin_models.go` registers
  `devin/` IDs and `internal/runtime/executor/helps/devin_models.go` strips them.
