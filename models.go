package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalog is the plugin's model serving for pluginapi.ModelProvider and
// pluginapi.ModelRegistrar. The kit supplies the pipeline (glob, overrides,
// namespace, single-flight and durable last-good); the plugin owns the
// paginated models/search Fetch and the per-account probe Validate hook.
// Fetch and Validate run foreground-only inside ModelsForAuth with the
// request-scoped host client.
//
// Two kit catalogs are kept apart on purpose: the fetch catalog carries no
// kit Static list, so a failed fetch with no durable last-good surfaces an
// error to ModelsForAuth, which then validates the static fallback per
// account itself (the pre-kit behavior). The static catalog serves
// RegisterModels/StaticModels, which have no auth to validate against.
type catalog struct {
	inner  *providerkit.ModelCatalog
	static *providerkit.ModelCatalog
	guard  *freeGuard
	probes chan struct{}
	cache  *verdictCache
}

func newCatalog(guard *freeGuard) catalog {
	return newCatalogWithTTL(guard, 0)
}

// newCatalogWithTTL is for TTL/last-good fixture tests. The TTL never gates
// a fetch: it only bounds the kit's freshness markers (see the kit
// ModelCatalogConfig.TTL note).
func newCatalogWithTTL(guard *freeGuard, ttl time.Duration) catalog {
	c := catalog{guard: guard, probes: make(chan struct{}, probeConcurrency), cache: newVerdictCache()}
	fetchCfg := providerkit.ModelCatalogConfig{
		Namespace: "cloudflare",
		Fetch:     c.fetch,
		Validate:  c.validateModels,
	}
	staticCfg := providerkit.ModelCatalogConfig{
		Namespace: "cloudflare",
		Static:    fallbackModels(),
	}
	if ttl > 0 {
		c.inner = providerkit.NewModelCatalogWithTTL(fetchCfg, ttl)
	} else {
		c.inner = providerkit.NewModelCatalog(fetchCfg)
	}
	c.static = providerkit.NewModelCatalog(staticCfg)
	return c
}

// fallbackModels is the small static catalog served when discovery is
// unavailable. Ids are upstream (pre-namespace); the kit catalog applies
// the cloudflare/ namespace through its uniform pipeline. Official model
// pages cited in docs/SOURCES.md. A fallback is a catalog, not an
// entitlement claim; the upstream still enforces account access.
func fallbackModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "@cf/meta/llama-3.1-8b-instruct-fp8", Object: "model", OwnedBy: provider, DisplayName: "@cf/meta/llama-3.1-8b-instruct-fp8"},
		{ID: "@cf/qwen/qwen3-30b-a3b-fp8", Object: "model", OwnedBy: provider, DisplayName: "@cf/qwen/qwen3-30b-a3b-fp8"},
		{ID: "@cf/openai/gpt-oss-120b", Object: "model", OwnedBy: provider, DisplayName: "@cf/openai/gpt-oss-120b"},
		{ID: "@cf/openai/gpt-oss-20b", Object: "model", OwnedBy: provider, DisplayName: "@cf/openai/gpt-oss-20b"},
	}
}

// RegisterModels serves the static list through the kit pipeline, for
// development-time registration. The kit stamps Provider from the plugin
// metadata name ("Cloudflare Workers AI"), but the host registers static
// catalogs under the provider id ("cloudflare-workers-ai"), so the wrapper
// rewrites it; the old hand-rolled code already returned the provider id.
func (c catalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	resp, err := c.static.RegisterModels(ctx, req)
	if err != nil {
		return pluginapi.ModelRegistrationResponse{}, err
	}
	resp.Provider = provider
	return resp, nil
}

func (c catalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	c.guard.observeHost(req.Host)
	resp, err := c.static.StaticModels(ctx, req)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	resp.Provider = provider
	return resp, nil
}

func (c catalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.ModelResponse{}, err
	}
	c.guard.observeHost(req.Host)
	// Failing on invalid credentials here (rather than serving the static
	// fallback) preserves the plugin's contract: a broken auth is an error,
	// while a well-formed auth with a failed search still gets the kit
	// fallback chain (durable last-good, then static).
	cred, err := credentialsFrom(req.StorageJSON, req.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	// The host no longer calls ParseAuth for file-loaded auths, so fold this
	// auth's account-wide limits into the shared ledger here: the host runs
	// discovery for every loaded auth, which restores fold-at-load without a
	// login card. Admission still enforces the ledger per request.
	c.guard.observeAuth(cred)
	if req.HTTPClient == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	// Every invocation fetches fresh through the kit catalog (no TTL skip);
	// a transient failure serves the kit's durable last-good internally.
	// One deadline spans fetch, validation and any fallback validation
	// below: the kit's ModelsFetchTimeout, never extended.
	// Provider is rewritten to the provider id for the same registry reason
	// as in RegisterModels above.
	dctx, cancel := context.WithTimeout(ctx, providerkit.ModelsFetchTimeout)
	defer cancel()
	resp, err := c.inner.ModelsForAuth(dctx, req)
	if err != nil {
		// An error here means no durable last-good exists for this
		// credential generation (the kit serves last-good internally
		// otherwise), so the static fallback is next, as before the kit
		// migration. A cancelled/deadline-expired call cannot probe;
		// it serves the static list, matching the kit's former
		// cancellation behavior. A live call validates the fallback
		// per account, so an account denied a static model never
		// receives it in its authenticated catalog.
		if dctx.Err() != nil {
			return pluginapi.ModelResponse{Provider: provider, Models: namespacedFallback()}, nil
		}
		models, verr := c.validate(dctx, req.HTTPClient, cred, namespacedFallback())
		if verr != nil {
			return pluginapi.ModelResponse{}, verr
		}
		resp = pluginapi.ModelResponse{Models: models}
	}
	resp.Provider = provider
	return resp, nil
}

// namespacedFallback is the static fallback list in the served namespace,
// in the pre-kit shape validate accepts (upstream id behind the prefix).
func namespacedFallback() []pluginapi.ModelInfo {
	out := fallbackModels()
	for i := range out {
		out[i].ID = modelPrefix + out[i].ID
	}
	return out
}

// fetch is the kit FetchModelsFunc: the plugin-owned paginated
// /models/search. It returns upstream ids (before the kit's namespace
// step) and enforces its own post-return parse cap (kit MaxModelsBody)
// and context cancellation; the kit's derived 15s fetch deadline
// (ModelsFetchTimeout) already bounds resolution.
func (c catalog) fetch(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	cred, err := credentialsFrom(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, err
	}
	if req.HTTPClient == nil {
		return nil, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	return discoverModels(ctx, req.HTTPClient, cred)
}

func discoverModels(ctx context.Context, client pluginapi.HostHTTPClient, c credentials) ([]pluginapi.ModelInfo, error) {
	const perPage, maxPages = 100, 20
	seen := make(map[string]bool)
	var models []pluginapi.ModelInfo
	for page := 1; page <= maxPages; page++ {
		query := url.Values{"task": {"Text Generation"}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		resp, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: accountBase(c) + "/models/search?" + query.Encode(), Headers: http.Header{"Authorization": {"Bearer " + c.token}, "Accept": {"application/json"}}})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, classifyFailure(resp, time.Now())
		}
		var wire struct {
			Success bool              `json:"success"`
			Errors  []json.RawMessage `json:"errors"`
			Result  *[]struct {
				Name string `json:"name"`
				Task struct {
					Name string `json:"name"`
				} `json:"task"`
			} `json:"result"`
			Info struct {
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		if len(resp.Body) > providerkit.MaxModelsBody || json.Unmarshal(resp.Body, &wire) != nil || !wire.Success || len(wire.Errors) != 0 || wire.Result == nil {
			return nil, &pluginabi.Error{Code: "invalid_models", Message: "Workers AI returned an invalid catalog", HTTPStatus: 502}
		}
		for _, item := range *wire.Result {
			id := strings.TrimSpace(item.Name)
			if !validModel(id) || seen[id] || item.Task.Name != "" && item.Task.Name != "Text Generation" {
				continue
			}
			seen[id] = true
			models = append(models, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, DisplayName: id})
		}
		if wire.Info.TotalPages > 0 {
			if page >= wire.Info.TotalPages {
				return markContributorBases(models, seen), nil
			}
		} else if len(*wire.Result) < perPage {
			return markContributorBases(models, seen), nil
		}
	}
	return nil, &pluginabi.Error{Code: "invalid_models", Message: "Workers AI catalog exceeds discovery page bound", HTTPStatus: 502}
}

// markContributorBases tags a "-contributor" entry whose base id is also in
// the search results. The kit drops such a base before Validate runs; the
// tag lets Validate resurrect the base when the preferred contributor is
// ineligible (see validate). Validate strips the tag before serving.
func markContributorBases(models []pluginapi.ModelInfo, seen map[string]bool) []pluginapi.ModelInfo {
	for i := range models {
		if base, ok := strings.CutSuffix(models[i].ID, "-contributor"); ok && seen[base] {
			models[i].Type = contributorBasePresent
		}
	}
	return models
}
