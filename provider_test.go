package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Only external transport is substituted. No provider endpoint can be reached.
type mockHostClient struct{ server *httptest.Server }

func (c mockHostClient) request(ctx context.Context, r pluginapi.HTTPRequest) (*http.Response, error) {
	if !strings.HasPrefix(r.URL, upstreamBase+"/") {
		return nil, fmt.Errorf("unexpected upstream")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, c.server.URL+strings.TrimPrefix(r.URL, upstreamBase), bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Headers.Clone()
	return c.server.Client().Do(req)
}
func (c mockHostClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
}
func (c mockHostClient) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer resp.Body.Close()
		for {
			buf := make([]byte, 7)
			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case chunks <- pluginapi.HTTPStreamChunk{Payload: buf[:n]}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case chunks <- pluginapi.HTTPStreamChunk{Err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Chunks: chunks}, nil
}

func TestChatExecutionStripsPrefixAndStreamsIncrementally(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/v4/accounts/"+testAccount+"/ai/v1/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("incorrect chat request")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "@cf/meta/llama-3.1-8b-instruct-fp8" || body["temperature"] != 0.25 {
			t.Error("model prefix or payload changed")
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
			}
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"id":"example","choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":5}}`)
	}))
	defer server.Close()
	ex := newExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8","stream":true,"temperature":0.25}`), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}}
	out, err := ex.Execute(ctx, req)
	if err != nil || !strings.Contains(string(out.Payload), `"content":"hello"`) {
		t.Fatalf("execute %s %v", out.Payload, err)
	}
	req.Payload = []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8","stream":false,"temperature":0.25}`)
	stream, err := ex.ExecuteStream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-stream.Chunks:
		if c.Err != nil || len(c.Payload) == 0 {
			t.Fatal("no first chunk")
		}
	case <-ctx.Done():
		t.Fatal("stream buffered")
	}
	cancel()
}
func TestRawHTTPIsPinnedAndInvalidModelsRejected(t *testing.T) {
	out, err := prepareHTTP(context.Background(), pluginapi.ExecutorHTTPRequest{URL: "https://example.invalid/escape", Method: "DELETE", Headers: http.Header{"Authorization": {"Bearer attacker"}}, Body: []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8"}`), StorageJSON: authJSON(testAccount, "example-key")})
	if err != nil || out.URL != "https://api.cloudflare.com/client/v4/accounts/"+testAccount+"/ai/v1/chat/completions" || out.Method != "POST" || out.Headers.Get("Authorization") != "Bearer example-key" {
		t.Fatal("untrusted destination/headers forwarded", err)
	}
	for _, body := range []string{`null`, `[]`, `{}`, `{"model":"@cf/meta/llama-3.1-8b-instruct-fp8"}`, `{"model":"cloudflare/"}`, `{"model":"cloudflare/cloudflare/@cf/model"}`} {
		if _, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(body), StorageJSON: authJSON(testAccount, "example-key")}); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	count, err := newExecutor().CountTokens(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"example"}`)})
	if err != nil || count.Metadata["estimated"] != true {
		t.Fatal("count must be local estimate", err)
	}
}
func TestDiscoveryUsesSearchPaginationAndNamespacesChatOnly(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/client/v4/accounts/"+testAccount+"/ai/models/search" || r.URL.Query().Get("task") != "Text Generation" || r.Header.Get("Authorization") != "Bearer example-key" || r.URL.Query().Get("per_page") != "100" {
			t.Error("invalid discovery wire request")
		}
		if r.URL.Query().Get("page") == "1" {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/meta/llama-3.1-8b-instruct-fp8","task":{"name":"Text Generation"}},{"name":"@cf/baai/bge-base-en-v1.5","task":{"name":"Text Embeddings"}}],"result_info":{"page":1,"total_pages":2}}`)
		} else {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/meta/llama-3.1-8b-instruct-fp8","task":{"name":"Text Generation"}},{"name":"@cf/qwen/qwen3-30b-a3b-fp8","task":{"name":"Text Generation"}}],"result_info":{"page":2,"total_pages":2}}`)
		}
	}))
	defer server.Close()
	out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 2 || calls.Load() != 2 {
		t.Fatalf("discovery models=%d calls=%d err=%v", len(out.Models), calls.Load(), err)
	}
	if out.Models[0].ID != "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8" || out.Models[1].ID != "cloudflare/@cf/qwen/qwen3-30b-a3b-fp8" {
		t.Fatal("incorrect model IDs")
	}
	static, _ := (catalog{}).StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	reg, _ := (catalog{}).RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if len(static.Models) < 2 || len(reg.Models) != len(static.Models) {
		t.Fatal("no static fallback")
	}
	for _, m := range reg.Models {
		if !strings.HasPrefix(m.ID, "cloudflare/@cf/") {
			t.Fatal("unprefixed static model")
		}
	}
}
func TestDiscoveryFailureFallsBackWithoutFabricatingEntitlements(t *testing.T) {
	for _, raw := range []string{`{"success":false,"errors":[{"message":"example-secret"}]}`, `{}`, `{"success":true,"result":null}`, strings.Repeat("x", (1<<20)+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
		out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
		server.Close()
		if err != nil || len(out.Models) < 2 {
			t.Fatalf("static fallback failed %v", err)
		}
	}
}
func TestErrorClassificationForStreamAndNonstream(t *testing.T) {
	now := time.Date(2030, 1, 1, 23, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		status     int
		body, code string
		retry      bool
		delay      int
	}{
		{401, `secret`, "invalid_credentials", false, 0},
		{403, `{"errors":[{"code":5016}]}`, "entitlement_required", false, 0},
		{404, `{"errors":[{"code":3042}]}`, "upstream_not_found", false, 0},
		{400, `{"errors":[{"code":5007}]}`, "upstream_not_found", false, 0},
		{429, `{"errors":[{"code":3036,"message":"You have used up your daily free allocation of 10,000 neurons."}]}`, "quota_exhausted", true, 60},
		{429, `{"error":{"message":"daily neuron limit exceeded"}}`, "quota_exhausted", true, 60},
		{429, `{"errors":[{"code":3040,"message":"Capacity temporarily exceeded, please try again."}]}`, "rate_limit", true, 5},
		{429, `{"error":"rate limited"}`, "rate_limit", true, 5},
		{503, `secret`, "upstream_error", true, 0},
		{408, `secret`, "upstream_error", true, 0},
	} {
		e := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}, now)
		if e.Code != tc.code || e.Retryable != tc.retry || e.RetryAfterSeconds != tc.delay {
			t.Fatalf("classification %d %s: %+v", tc.status, tc.code, e)
		}
		if strings.Contains(e.Message, "secret") {
			t.Fatal("body leaked")
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
		req := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8"}`), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}}
		_, a := newExecutor().Execute(context.Background(), req)
		_, b := newExecutor().ExecuteStream(context.Background(), req)
		server.Close()
		var ae, be *pluginabi.Error
		if !errors.As(a, &ae) || !errors.As(b, &be) || ae.Code != tc.code || be.Code != tc.code || ae.HTTPStatus != be.HTTPStatus {
			t.Fatalf("inconsistent errors %v %v", a, b)
		}
	}
	for _, tc := range []struct {
		body, retry string
		want        int
	}{{`{"errors":[{"code":3036}]}`, "1", 60}, {`{"errors":[{"code":3040}]}`, "12", 12}, {`{"errors":[{"code":3040}]}`, "Tue, 01 Jan 2030 23:59:10 GMT", 10}} {
		e := classifyFailure(pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(tc.body), Headers: http.Header{"Retry-After": {tc.retry}}}, now)
		if e.RetryAfterSeconds != tc.want {
			t.Fatalf("retry timing %+v", e)
		}
	}
}
