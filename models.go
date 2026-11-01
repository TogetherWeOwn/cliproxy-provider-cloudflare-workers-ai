package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type catalog struct {
	cache *verdictCache
	// probes is shared by every discovery so overlapping ModelsForAuth calls
	// stay within probeConcurrency in total.
	probes chan struct{}
	// guard admits each validation probe like inference.
	guard *freeGuard
}

func newCatalog(guard *freeGuard) catalog {
	return catalog{cache: newVerdictCache(), probes: make(chan struct{}, probeConcurrency), guard: guard}
}

func modelInfo(id string) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{ID: modelPrefix + id, Object: "model", OwnedBy: provider, DisplayName: id}
}
func fallbackModels() []pluginapi.ModelInfo {
	// Official model pages cited in docs/SOURCES.md. A fallback is a catalog,
	// not an entitlement claim; the upstream still enforces account access.
	return []pluginapi.ModelInfo{modelInfo("@cf/meta/llama-3.1-8b-instruct-fp8"), modelInfo("@cf/qwen/qwen3-30b-a3b-fp8"), modelInfo("@cf/openai/gpt-oss-120b"), modelInfo("@cf/openai/gpt-oss-20b")}
}
func (catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider, Models: fallbackModels()}, nil
}
func (cat catalog) ModelsForAuth(ctx context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	c, err := credentialsFrom(r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	if r.HTTPClient == nil {
		return pluginapi.ModelResponse{}, &pluginabi.Error{Code: "host_unavailable", Message: "model discovery requires the host HTTP client", HTTPStatus: 503}
	}
	models, err := discoverModels(ctx, r.HTTPClient, c)
	if err != nil {
		if ctx.Err() != nil {
			return pluginapi.ModelResponse{}, ctx.Err()
		}
		models = fallbackModels()
	}
	// Discovered and fallback IDs alike are validated against the account.
	if models, err = cat.validate(ctx, r.HTTPClient, c, models); err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return pluginapi.ModelResponse{Provider: provider, Models: models}, nil
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
		if len(resp.Body) > 1<<20 || json.Unmarshal(resp.Body, &wire) != nil || !wire.Success || len(wire.Errors) != 0 || wire.Result == nil {
			return nil, &pluginabi.Error{Code: "invalid_models", Message: "Workers AI returned an invalid catalog", HTTPStatus: 502}
		}
		for _, item := range *wire.Result {
			id := strings.TrimSpace(item.Name)
			if !validModel(id) || seen[id] || item.Task.Name != "" && item.Task.Name != "Text Generation" {
				continue
			}
			seen[id] = true
			models = append(models, modelInfo(id))
		}
		if wire.Info.TotalPages > 0 {
			if page >= wire.Info.TotalPages {
				return models, nil
			}
		} else if len(*wire.Result) < perPage {
			return models, nil
		}
	}
	return nil, &pluginabi.Error{Code: "invalid_models", Message: "Workers AI catalog exceeds discovery page bound", HTTPStatus: 502}
}
