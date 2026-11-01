//go:build cgo && (linux || darwin || freebsd)

// Harness layout adapted from cliproxy-provider-synthetic v0.1.0 (MIT).
package hostcompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

const provider = "cloudflare-workers-ai"
const model = "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8"

var account = strings.Repeat("a", 32)

// exhausted is a second mock account whose analytics already show 9,900 of
// 10,000 free neurons, so the free-only guard must refuse before dispatch.
var exhausted = strings.Repeat("c", 32)

// busy is a third mock account at 6,000 neurons: one large request fits, and
// the next is refused by the in-process tally alone.
var busy = strings.Repeat("d", 32)

// unverified is a fourth mock account whose analytics permission was revoked:
// usage cannot be checked, so the guard must refuse as quota_unverified (503),
// never as a downstream-style 429, and must not mark the model quota-exceeded.
var unverified = strings.Repeat("e", 32)

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type models struct{}

func (models) RegisterClient(string, string, []*registry.ModelInfo) {}
func (models) UnregisterClient(string)                              {}
func (models) GetModelProviders(string) []string                    { return nil }
func authJSON(key string) []byte {
	// cold_start_hold_seconds 0: the plugin instance starts inside the test, so
	// the default hold would refuse every request it makes.
	raw, _ := json.Marshal(map[string]any{"type": provider, "account_id": account, "api_key": key, "analytics_api_key": "analytics-key", "label": "Example", "cold_start_hold_seconds": 0})
	return raw
}

// fixture reads mock directives from the first message. They travel in the
// content because the free-only guard forwards only allowlisted fields.
type fixture struct {
	status                 int
	daily, firstOnly, gate bool
}

func fixtureOf(body map[string]any) (fx fixture) {
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return fx
	}
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].(string)
	words := strings.Fields(content)
	if len(words) == 0 || words[0] != "fixture" {
		return fx
	}
	for _, word := range words[1:] {
		switch {
		case strings.HasPrefix(word, "status="):
			fx.status, _ = strconv.Atoi(strings.TrimPrefix(word, "status="))
		case word == "daily":
			fx.daily = true
		case word == "first_only":
			fx.firstOnly = true
		case word == "gate":
			fx.gate = true
		}
	}
	return fx
}

// Real v8.0.49 dlopen, C-ABI registration, RPC dispatch, auth persistence and
// executor adapters. The only provider network target is an httptest server.
func TestRealHostLoader(t *testing.T) {
	var polls atomic.Int32
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/v4/graphql":
			polls.Add(1)
			if r.Method != "POST" {
				t.Error("bad analytics request")
			}
			if r.Header.Get("Authorization") != "Bearer analytics-key" {
				// Revoked or missing Analytics Read permission: usage cannot
				// be checked. The guard must refuse inference as
				// quota_unverified, never as a 429.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var wire struct {
				Query     string
				Variables map[string]string
			}
			if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.Variables["account"] != account && wire.Variables["account"] != exhausted && wire.Variables["account"] != busy || !strings.Contains(wire.Query, "totalNeurons") {
				t.Error("bad GraphQL query")
			}
			used := "7500"
			switch wire.Variables["account"] {
			case exhausted:
				used = "9900"
			case busy:
				used = "6000"
			}
			io.WriteString(w, `{"data":{"viewer":{"accounts":[{"accountTag":"`+wire.Variables["account"]+`","aiInferenceAdaptiveGroups":[{"sum":{"totalNeurons":`+used+`}}]}]}}}`)
		case "/client/v4/accounts/" + account + "/ai/models/search":
			if r.Header.Get("Authorization") != "Bearer example-key" || r.URL.Query().Get("task") != "Text Generation" {
				t.Error("bad discovery request")
			}
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/meta/llama-3.1-8b-instruct-fp8","task":{"name":"Text Generation"}},{"name":"@cf/qwen/qwen3-30b-a3b-fp8","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
		case "/client/v4/accounts/" + account + "/ai/v1/chat/completions":
			token := r.Header.Get("Authorization")
			if token != "Bearer example-key" && token != "Bearer second-key" {
				t.Error("bad inference auth")
			}
			var body map[string]any
			// Discovery validates each catalog model with a one-token chat probe.
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "@cf/meta/llama-3.1-8b-instruct-fp8" && !(body["model"] == "@cf/qwen/qwen3-30b-a3b-fp8" && body["max_tokens"] == float64(1)) {
				t.Error("prefix not stripped")
				w.WriteHeader(400)
				return
			}
			fx := fixtureOf(body)
			if fx.status != 0 && !(fx.firstOnly && token == "Bearer second-key") {
				if fx.status == 429 {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(fx.status)
				if fx.daily {
					io.WriteString(w, `{"errors":[{"code":3036,"message":"daily neuron allocation exhausted"}]}`)
				} else if fx.status == 429 {
					io.WriteString(w, `{"errors":[{"code":3040,"message":"Capacity temporarily exceeded"}]}`)
				} else {
					io.WriteString(w, `{"error":"fixture failure"}`)
				}
				return
			}
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
				w.(http.Flusher).Flush()
				if fx.gate {
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chat-example","object":"chat.completion","model":"@cf/meta/llama-3.1-8b-instruct-fp8","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		case "/client/v4/accounts/" + busy + "/ai/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chat-busy","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		case "/client/v4/accounts/" + exhausted + "/ai/v1/chat/completions":
			t.Error("request dispatched past the free-only guard")
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected provider path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib := filepath.Join(arch, provider+ext)
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", lib, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", err, out)
	}
	if out, err := exec.Command("nm", "-D", lib).CombinedOutput(); err != nil || !strings.Contains(string(out), " T cliproxy_plugin_init") {
		t.Fatalf("entry point %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_base: "+server.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{provider: {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = logging.WithResponseHeadersHolder(ctx)
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded(provider) || !host.PluginRegistered(provider) || !host.HasAuthProvider(provider) || !host.HasQuotaProvider(provider) {
		t.Fatalf("registration %+v", host.RegisteredPlugins())
	}
	info := host.RegisteredPlugins()
	if len(info) != 1 || !info[0].SupportsQuota || info[0].Metadata.Version != "0.1.6" {
		t.Fatalf("metadata %+v", info)
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "example.json", RawJSON: authJSON("example-key")})
	if err != nil || !handled || auth == nil {
		t.Fatalf("auth %v %v", handled, err)
	}
	if _, _, err = host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "duplicate.json", RawJSON: authJSON("example-key")}); err == nil {
		t.Fatal("duplicate accepted")
	}
	file := filepath.Join(cfg.AuthDir, "example.json")
	if err = auth.Storage.SaveTokenToFile(file); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(saved, &fields) != nil || fields["type"] != provider || fields["account_id"] != account || fields["label"] != "Example" {
		t.Fatal("storage merge failed")
	}
	quotaReq := pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: provider, StorageJSON: saved}
	for i := 0; i < 2; i++ {
		quota, handled, err := host.FetchQuota(ctx, quotaReq)
		if err != nil || !handled || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != .25 || quota.Groups[0].Buckets[0].Window != "daily" || polls.Load() != 1 {
			t.Fatalf("quota %+v %v polls=%d", quota, err, polls.Load())
		}
	}
	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	static := host.ModelsForProvider(provider)
	if len(static) < 2 {
		t.Fatal("no static fallback")
	}
	for _, m := range static {
		if !strings.HasPrefix(m.ID, "cloudflare/") {
			t.Fatal("unprefixed static model")
		}
	}
	found := host.ModelsForAuth(ctx, auth)
	if !found.Handled || len(found.Models) != 2 {
		t.Fatalf("discovery %+v", found)
	}
	for _, m := range found.Models {
		if !strings.HasPrefix(m.ID, "cloudflare/") {
			t.Fatal("unprefixed discovered model")
		}
	}
	ex, ok := m[provider]
	if !ok {
		t.Fatal("no executor")
	}
	opts := coreexec.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI}
	req := coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`)}
	resp, err := ex.Execute(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(resp.Payload), `"content":"hello"`) || !strings.Contains(string(resp.Payload), `"total_tokens":5`) {
		t.Fatalf("execute %s %v", resp.Payload, err)
	}
	req.Payload = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"fixture gate"}]}`)
	opts.Stream = true
	stream, err := ex.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var chunks strings.Builder
	select {
	case c := <-stream.Chunks:
		if c.Err != nil || len(c.Payload) == 0 {
			t.Fatalf("first chunk %+v", c)
		}
		chunks.Write(c.Payload)
		close(gate)
	case <-ctx.Done():
		t.Fatal("stream buffered")
	}
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		chunks.Write(c.Payload)
	}
	if !strings.Contains(chunks.String(), "[DONE]") {
		t.Fatal("stream truncated")
	}
	opts.Stream = false
	for _, status := range []int{401, 403, 404, 429, 503} {
		raw, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": fmt.Sprintf("fixture status=%d", status)}}})
		req.Payload = raw
		_, err = ex.Execute(ctx, auth, req, opts)
		e, ok := err.(interface{ StatusCode() int })
		if !ok || e.StatusCode() != status {
			t.Fatalf("status %d: %T %v", status, err, err)
		}
		if status == 429 {
			hint, ok := err.(interface{ RetryAfter() *time.Duration })
			if !ok || hint.RetryAfter() == nil || *hint.RetryAfter() != 7*time.Second {
				t.Fatal("rate Retry-After lost")
			}
		}
	}
	t.Run("daily exhaustion rotates only failed auth until UTC reset", func(t *testing.T) {
		second, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "second.json", RawJSON: authJSON("second-key")})
		if err != nil || !handled || second == nil {
			t.Fatal(err)
		}
		mgr := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		mgr.SetRetryConfig(0, 0, 2)
		mgr.RegisterExecutor(ex)
		first := auth.Clone()
		first.ID = "first-auth"
		first.Status = coreauth.StatusActive
		first.Attributes = map[string]string{"type": "api_key", "priority": "10"}
		second = second.Clone()
		second.ID = "second-auth"
		second.Status = coreauth.StatusActive
		for _, a := range []*coreauth.Auth{first, second} {
			if _, err = mgr.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		}
		before := time.Now().UTC()
		reset := time.Date(before.Year(), before.Month(), before.Day()+1, 0, 0, 0, 0, time.UTC)
		call := coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"fixture status=429 daily first_only"}]}`)}
		if _, err = mgr.Execute(ctx, []string{provider}, call, opts); err != nil {
			t.Fatalf("sibling rotation %v", err)
		}
		failed, _ := mgr.GetByID(first.ID)
		healthy, _ := mgr.GetByID(second.ID)
		state := failed.ModelStates[model]
		if state == nil || !state.Quota.Exceeded || state.NextRetryAfter.Before(reset) || state.NextRetryAfter.After(reset.Add(3*time.Second)) {
			t.Fatalf("UTC reset hint lost %+v", state)
		}
		if healthy.Unavailable || healthy.Quota.Exceeded {
			t.Fatal("healthy sibling impaired")
		}
	})
	t.Run("free-only guard refuses before dispatch until UTC reset", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"type": provider, "account_id": exhausted, "api_key": "example-key", "analytics_api_key": "analytics-key", "cold_start_hold_seconds": 0})
		spent, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "exhausted.json", RawJSON: raw})
		if err != nil || !handled || spent == nil {
			t.Fatal(err)
		}
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		spent = spent.Clone()
		spent.ID = "exhausted-auth"
		spent.Status = coreauth.StatusActive
		if _, err = mgr.Register(ctx, spent); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(spent.ID, spent.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(spent.ID) })
		before := time.Now().UTC()
		reset := time.Date(before.Year(), before.Month(), before.Day()+1, 0, 0, 0, 0, time.UTC)
		_, err = mgr.Execute(ctx, []string{provider}, coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`)}, opts)
		if e, ok := err.(interface{ StatusCode() int }); !ok || e.StatusCode() != 429 {
			t.Fatalf("guard refusal %T %v", err, err)
		}
		got, _ := mgr.GetByID(spent.ID)
		state := got.ModelStates[model]
		if state == nil || !state.Quota.Exceeded || state.NextRetryAfter.Before(reset) || state.NextRetryAfter.After(reset.Add(3*time.Second)) {
			t.Fatalf("guard refusal lost the UTC reset hint %+v", state)
		}
	})
	t.Run("tally-only guard refusal cools down minutes, not until UTC reset", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"type": provider, "account_id": busy, "api_key": "example-key", "analytics_api_key": "analytics-key", "cold_start_hold_seconds": 0})
		pending, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "busy.json", RawJSON: raw})
		if err != nil || !handled || pending == nil {
			t.Fatal(err)
		}
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		pending = pending.Clone()
		pending.ID = "busy-auth"
		pending.Status = coreauth.StatusActive
		if _, err = mgr.Register(ctx, pending); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(pending.ID, pending.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(pending.ID) })
		// About 2,615 neurons each: 6000 + 2615 + 500 fits, a second one only
		// fails because the first is still in the tally.
		call := coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","max_tokens":100000,"messages":[{"role":"user","content":"hello"}]}`)}
		if _, err = mgr.Execute(ctx, []string{provider}, call, opts); err != nil {
			t.Fatalf("first request %v", err)
		}
		before := time.Now()
		_, err = mgr.Execute(ctx, []string{provider}, call, opts)
		if e, ok := err.(interface{ StatusCode() int }); !ok || e.StatusCode() != 429 {
			t.Fatalf("tally refusal %T %v", err, err)
		}
		if hint, ok := err.(interface{ RetryAfter() *time.Duration }); !ok || hint.RetryAfter() == nil || *hint.RetryAfter() <= 0 || *hint.RetryAfter() > 10*time.Minute {
			t.Fatalf("tally refusal retry hint %v", err)
		}
		got, _ := mgr.GetByID(pending.ID)
		state := got.ModelStates[model]
		if state == nil || !state.NextRetryAfter.After(before) || state.NextRetryAfter.After(before.Add(10*time.Minute+3*time.Second)) {
			t.Fatalf("tally refusal locked the model too long %+v", state)
		}
	})
	t.Run("unverified guard refusal stays a 503, never a 429", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"type": provider, "account_id": unverified, "api_key": "example-key", "analytics_api_key": "revoked-key", "cold_start_hold_seconds": 0})
		blind, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: provider, FileName: "unverified.json", RawJSON: raw})
		if err != nil || !handled || blind == nil {
			t.Fatal(err)
		}
		blindFile := filepath.Join(cfg.AuthDir, "unverified.json")
		if err = blind.Storage.SaveTokenToFile(blindFile); err != nil {
			t.Fatal(err)
		}
		blindSaved, err := os.ReadFile(blindFile)
		if err != nil {
			t.Fatal(err)
		}
		quotaAuthID := blind.ID
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		blind = blind.Clone()
		blind.ID = "unverified-auth"
		blind.Status = coreauth.StatusActive
		if _, err = mgr.Register(ctx, blind); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(blind.ID, blind.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(blind.ID) })
		_, err = mgr.Execute(ctx, []string{provider}, coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`)}, opts)
		e, ok := err.(interface{ StatusCode() int })
		if !ok || e.StatusCode() != http.StatusServiceUnavailable {
			t.Fatalf("guard refusal %T %v", err, err)
		}
		// The host reframes the plugin's 60-second hint as a live cooldown
		// deadline, so only a range can be asserted, not exact equality.
		if hint, ok := err.(interface{ RetryAfter() *time.Duration }); !ok || hint.RetryAfter() == nil || *hint.RetryAfter() <= 0 || *hint.RetryAfter() > 60*time.Second {
			t.Fatalf("guard refusal retry hint %v", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "quota_unverified") || !strings.Contains(msg, "cannot be verified") {
			t.Fatalf("guard refusal lost its label %q", msg)
		}
		// A 503 guard refusal cools the model down briefly; unlike a 429 it
		// must never mark quota exhausted (that state waits for the UTC reset
		// and would sideline the account over a fixable permission problem).
		got, _ := mgr.GetByID(blind.ID)
		if state := got.ModelStates[model]; state != nil && state.Quota.Exceeded {
			t.Fatalf("guard refusal marked quota exhausted %+v", state)
		}
		// The quota card fails closed too, so quota sync never reports this
		// account as healthy while inference refuses.
		if _, _, err = host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{AuthID: quotaAuthID, Provider: provider, StorageJSON: blindSaved}); err == nil {
			t.Fatal("quota fetch succeeded despite revoked analytics permission")
		}
	})
	t.Run("pinned host transient 429 marker limitation", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, nil, nil)
		mgr.SetRetryConfig(0, 0, 1)
		mgr.RegisterExecutor(ex)
		one := auth.Clone()
		one.ID = "transient-auth"
		one.Status = coreauth.StatusActive
		if _, err = mgr.Register(ctx, one); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(one.ID, one.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(one.ID) })
		_, err = mgr.Execute(ctx, []string{provider}, coreexec.Request{Model: model, Format: translator.FormatOpenAI, Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"fixture status=429"}]}`)}, opts)
		if err == nil {
			t.Fatal("rate failure disappeared")
		}
		got, _ := mgr.GetByID(one.ID)
		state := got.ModelStates[model]
		if state == nil || !state.Quota.Exceeded {
			t.Fatalf("host changed; revisit documented RPC limitation %+v", state)
		}
		t.Log("v8.0.49 RPC drops the transient marker; rate/capacity 429 is marked model quota-exceeded")
	})
}
func TestRegistryUsesNativeStoreSchema(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := pluginstore.ParseRegistry(raw)
	if err != nil || reg.SchemaVersion != 1 || len(reg.Plugins) != 1 || reg.Plugins[0].ID != provider || reg.Plugins[0].Version != "0.1.6" || pluginstore.PluginInstallType(reg.Plugins[0]) != "github-release" {
		t.Fatalf("registry %+v %v", reg, err)
	}
}
