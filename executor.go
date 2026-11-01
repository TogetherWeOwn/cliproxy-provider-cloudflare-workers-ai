package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type executor struct{ kit.Passthrough }

// Every dispatch path (Execute, ExecuteStream, HttpRequest) builds its upstream
// request here, so the free-only guard admits each one before it is sent.
func newExecutor(guard *freeGuard) executor {
	build := func(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
		req, c, err := upstreamRequest(r)
		if err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		if err = guard.admit(ctx, r.HTTPClient, c, estimateNeurons(req.Body)); err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		return req, nil
	}
	prepare := func(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
		return build(ctx, httpExecutorRequest(r))
	}
	return executor{kit.Passthrough{Provider: provider, BuildRequest: build, PrepareHTTP: prepare, ClassifyError: func(r pluginapi.HTTPResponse) error { return classifyFailure(r, time.Now()) }}}
}
func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	r.Stream = false
	return e.Passthrough.Execute(ctx, r)
}
func (e executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	return e.Passthrough.ExecuteStream(ctx, r)
}
func accountBase(c credentials) string {
	return selectedBase(upstreamBase) + "/client/v4/accounts/" + c.account + "/ai"
}
func validModel(id string) bool {
	return strings.HasPrefix(id, "@cf/") && len(id) > len("@cf/") && !strings.ContainsAny(id, " \r\n\t?#\\")
}
func upstreamRequest(r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, credentials, error) {
	c, err := credentialsFrom(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, c, err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &body) != nil || body == nil {
		return pluginapi.HTTPRequest{}, c, &pluginabi.Error{Code: "invalid_request", Message: "payload must be a JSON object", HTTPStatus: 400}
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) || !validModel(strings.TrimPrefix(model, modelPrefix)) {
		return pluginapi.HTTPRequest{}, c, &pluginabi.Error{Code: "invalid_model", Message: "expected a cloudflare/@cf/ model", HTTPStatus: 400}
	}
	body["model"], _ = json.Marshal(strings.TrimPrefix(model, modelPrefix))
	body["stream"], _ = json.Marshal(r.Stream)
	// The free-only estimate needs an output bound; without a positive
	// max_tokens, cap output at max_completion_tokens or the default.
	if c.stayWithinFree && positiveNumber(body["max_tokens"]) == 0 {
		limit := positiveNumber(body["max_completion_tokens"])
		if limit == 0 {
			limit = defaultMaxOutputTokens
		}
		body["max_tokens"], _ = json.Marshal(limit)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.HTTPRequest{}, c, err
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: accountBase(c) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + c.token}, "Content-Type": {"application/json"}}, Body: raw}, c, nil
}
func httpExecutorRequest(r pluginapi.ExecutorHTTPRequest) pluginapi.ExecutorRequest {
	// Do not trust raw URLs, methods, caller credentials or extra headers.
	var input struct{ Stream bool }
	_ = json.Unmarshal(r.Body, &input)
	return pluginapi.ExecutorRequest{Payload: r.Body, Stream: input.Stream, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata, HTTPClient: r.HTTPClient}
}
