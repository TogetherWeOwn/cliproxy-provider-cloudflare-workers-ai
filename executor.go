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
	build := func(ctx context.Context, r pluginapi.ExecutorRequest, stream *bool) (pluginapi.HTTPRequest, error) {
		req, c, estimate, err := upstreamRequest(r, stream)
		if err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		if err = guard.admit(ctx, c, estimate); err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		return req, nil
	}
	execute := func(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
		return build(ctx, r, &r.Stream)
	}
	// Raw HTTP requests keep the stream flag of the body they carry.
	prepare := func(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
		return build(ctx, httpExecutorRequest(r), nil)
	}
	return executor{kit.Passthrough{Provider: provider, BuildRequest: execute, PrepareHTTP: prepare, ClassifyError: func(r pluginapi.HTTPResponse) error { return classifyFailure(r, time.Now()) }}}
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
func invalidModel() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_model", Message: "expected a cloudflare/@cf/ model", HTTPStatus: http.StatusBadRequest}
}

// upstreamRequest builds the chat request and, while stay_within_free is on,
// its neuron estimate. stream nil keeps the body's own stream flag.
func upstreamRequest(r pluginapi.ExecutorRequest, stream *bool) (pluginapi.HTTPRequest, credentials, float64, error) {
	c, err := credentialsFrom(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, c, 0, err
	}
	var raw []byte
	estimate := 0.0
	if c.stayWithinFree {
		raw, estimate, err = guardedRequest(r.Payload, stream)
	} else {
		raw, err = passthroughRequest(r.Payload, stream)
	}
	if err != nil {
		return pluginapi.HTTPRequest{}, c, 0, err
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: accountBase(c) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + c.token}, "Content-Type": {"application/json"}}, Body: raw}, c, estimate, nil
}

// guardedRequest decodes the client body once (see freebody.go) and returns
// the rebuilt upstream body with the estimate charged for exactly those bytes.
func guardedRequest(payload []byte, stream *bool) ([]byte, float64, error) {
	fields, err := strictObject(payload, guardedFields, "request")
	if err != nil {
		return nil, 0, err
	}
	model, ok := decodeString(fields["model"])
	if !ok || !strings.HasPrefix(model, modelPrefix) || !validModel(strings.TrimPrefix(model, modelPrefix)) {
		return nil, 0, invalidModel()
	}
	model = strings.TrimPrefix(model, modelPrefix)
	rate, priced := neuronRates[model]
	if !priced {
		return nil, 0, &pluginabi.Error{Code: "invalid_model", Message: "model has no published neuron price, so it cannot be bounded to the free allocation while stay_within_free is on", HTTPStatus: http.StatusBadRequest}
	}
	streaming := false
	if stream != nil {
		streaming = *stream
	} else if value, present := fields["stream"]; present && !isNull(value) && json.Unmarshal(value, &streaming) != nil {
		return nil, 0, guardedRequestError("stream must be a boolean")
	}
	body, limit, err := guardedBody(fields, model, streaming)
	if err != nil {
		return nil, 0, err
	}
	return body, estimateNeurons(rate, body, limit), nil
}

// passthroughRequest is the opted-out path: the client body with the model
// prefix removed and, outside raw HTTP requests, the dispatch stream flag.
func passthroughRequest(payload []byte, stream *bool) ([]byte, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil || body == nil {
		return nil, &pluginabi.Error{Code: "invalid_request", Message: "payload must be a JSON object", HTTPStatus: 400}
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) || !validModel(strings.TrimPrefix(model, modelPrefix)) {
		return nil, invalidModel()
	}
	body["model"], _ = json.Marshal(strings.TrimPrefix(model, modelPrefix))
	if stream != nil {
		body["stream"], _ = json.Marshal(*stream)
	}
	return json.Marshal(body)
}
func httpExecutorRequest(r pluginapi.ExecutorHTTPRequest) pluginapi.ExecutorRequest {
	// Do not trust raw URLs, methods, caller credentials or extra headers.
	return pluginapi.ExecutorRequest{Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata, HTTPClient: r.HTTPClient}
}
