package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These fixture tests exercise the kit ModelCatalog pipeline through this
// plugin's fetch shape (paginated models/search) and its probe Validate
// hook. Kit-internal behavior (single-flight coalescing, copy isolation,
// late completion) is covered by the kit's own suite; the tests below
// assert only plugin-observable outcomes.

// searchCatalogFixture serves a swappable models/search response and
// answers validation probes with 200 (or the per-model status in
// probeStatus, set before serve).
type searchCatalogFixture struct {
	body        atomic.Value // string search body for the ok mode
	mode        atomic.Int32 // 0 ok, 1 upstream failure, 2 malformed
	calls       atomic.Int32
	probeStatus map[string]int // upstream model id -> probe HTTP status
	delay       time.Duration  // probe latency, for deadline fixtures
}

func (f *searchCatalogFixture) setBody(b string) { f.body.Store(b) }

func (f *searchCatalogFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(withAnalytics(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			f.calls.Add(1)
			switch f.mode.Load() {
			case 1:
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"success":false,"errors":[{"message":"boom"}]}`)
				return
			case 2:
				io.WriteString(w, `not-json`)
				return
			}
			b, _ := f.body.Load().(string)
			io.WriteString(w, b)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var probed struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &probed)
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		if status, ok := f.probeStatus[probed.Model]; ok {
			w.WriteHeader(status)
			return
		}
		io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"length"}]}`)
	})))
	t.Cleanup(server.Close)
	return server
}

func searchBody(ids ...string) string {
	var entries []string
	for _, id := range ids {
		entries = append(entries, `{"name":"`+id+`","task":{"name":"Text Generation"}}`)
	}
	return `{"success":true,"errors":[],"result":[` + strings.Join(entries, ",") + `],"result_info":{"page":1,"total_pages":1}}`
}

func catalogRequest(server *httptest.Server, metadata map[string]any) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), Metadata: metadata, HTTPClient: mockHostClient{server}}
}

const (
	staticServed = "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8,cloudflare/@cf/qwen/qwen3-30b-a3b-fp8,cloudflare/@cf/openai/gpt-oss-120b,cloudflare/@cf/openai/gpt-oss-20b"
)

// Timeline A: every invocation fetches fresh, so a changed upstream list
// converges on the next call even with a 1h kit TTL (the TTL never skips a
// fetch; it only bounds freshness markers).
func TestCatalogTimelineAFreshConvergesAndIgnoresTTL(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/meta/llama-3.1-8b-instruct-fp8", "@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	c := newCatalogWithTTL(testGuard(server), time.Hour)
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8,cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("first fetch %s %v", ids(out.Models), err)
	}
	f.setBody(searchBody("@cf/qwen/qwen3-30b-a3b-fp8"))
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/qwen/qwen3-30b-a3b-fp8" {
		t.Fatalf("fresh fetch did not converge: %s %v", ids(out.Models), err)
	}
}

// Timeline B (documented, never claimed as a guarantee): after one
// successful fetch the durable last-good serves on transient failure. The
// kit performs a single fetch per ModelsForAuth invocation (no bounded
// retry), so recovery waits for the next host event; this test does not
// assert recovery.
func TestCatalogTimelineBFailureServesLastGood(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	c := newCatalog(testGuard(server))
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("first fetch %s %v", ids(out.Models), err)
	}
	f.mode.Store(1)
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("transient failure must serve durable last-good: %s %v", ids(out.Models), err)
	}
}

// TTL expiry never erases last-good: freshness markers expire, the durable
// store persists.
func TestCatalogLastGoodSurvivesFreshnessExpiry(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	c := newCatalogWithTTL(testGuard(server), 50*time.Millisecond)
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("first fetch %s %v", ids(out.Models), err)
	}
	time.Sleep(100 * time.Millisecond)
	f.mode.Store(1)
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("expired freshness erased last-good: %s %v", ids(out.Models), err)
	}
}

// A different credential generation never inherits another generation's
// last-good: with no history of its own, a failing fetch gets the static
// fallback (validated per account; the fixture probes 200, so the full
// static list serves).
func TestCatalogCredentialGenerationIsolation(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	c := newCatalog(testGuard(server))
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("first fetch %s %v", ids(out.Models), err)
	}
	f.mode.Store(1)
	req := catalogRequest(server, probeOnly)
	req.StorageJSON = authJSON(testAccount, "other-key")
	if out, err := c.ModelsForAuth(context.Background(), req); err != nil || ids(out.Models) != staticServed {
		t.Fatalf("new generation served %s %v, want static", ids(out.Models), err)
	}
}

// Contributor dedup for unknown ids (ids with no published neuron price):
// with stay_within_free off both are probed, and the kit serves only the
// -contributor sibling. With the hard stop on, unknown ids are dropped
// without a probe.
func TestCatalogContributorDedupForUnknownIDs(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/unknown-x/model", "@cf/unknown-x/model-contributor"))
	server := f.serve(t)
	c := newCatalog(testGuard(server))
	if out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/unknown-x/model-contributor" {
		t.Fatalf("contributor dedup %s %v", ids(out.Models), err)
	}
	// Guarded (stayWithinFree on): unpriced ids are never advertised.
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, nil)); err != nil || len(out.Models) != 0 {
		t.Fatalf("guarded unknown ids %s %v", ids(out.Models), err)
	}
}

// Executor mapping: every id the catalog serves must map through the
// executor (prefix stripped exactly once) to the upstream chat endpoint.
func TestCatalogExecutorMapping(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/meta/llama-3.1-8b-instruct-fp8", "@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, nil))
	if err != nil || len(out.Models) != 2 {
		t.Fatalf("discovery %s %v", ids(out.Models), err)
	}
	cred, err := credentialsFrom(authJSON(testAccount, "example-key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Models {
		mapped, _, _, err := upstreamRequest(pluginapi.ExecutorRequest{Payload: []byte(`{"model":"` + m.ID + `"}`), StorageJSON: authJSON(testAccount, "example-key")}, nil)
		if err != nil {
			t.Fatalf("served model %s does not map: %v", m.ID, err)
		}
		if mapped.URL != accountBase(cred)+"/v1/chat/completions" || !strings.Contains(string(mapped.Body), `"model":"`+strings.TrimPrefix(m.ID, modelPrefix)+`"`) {
			t.Fatalf("mapped %s -> %s body %s", m.ID, mapped.URL, mapped.Body)
		}
	}
}

// Malformed search bodies fall back to the static list (parse failure is a
// failed fetch, never served).
func TestCatalogParseFailureFallsBackToStatic(t *testing.T) {
	f := &searchCatalogFixture{}
	f.mode.Store(2)
	server := f.serve(t)
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != staticServed {
		t.Fatalf("malformed body served %s %v, want static", ids(out.Models), err)
	}
}

// The kit stamps Provider from the plugin metadata name; the host registers
// catalogs under the provider id, so the wrappers must report the id.
func TestCatalogProviderIsProviderID(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/openai/gpt-oss-20b"))
	server := f.serve(t)
	c := newCatalog(testGuard(server))
	reg, err := c.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || reg.Provider != provider {
		t.Fatalf("RegisterModels provider %q %v", reg.Provider, err)
	}
	static, err := c.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || static.Provider != provider {
		t.Fatalf("StaticModels provider %q %v", static.Provider, err)
	}
	out, err := c.ModelsForAuth(context.Background(), catalogRequest(server, probeOnly))
	if err != nil || out.Provider != provider {
		t.Fatalf("ModelsForAuth provider %q %v", out.Provider, err)
	}
}

// Enforceable deadline: a search fetch that blocks forever does not hang
// the callback; parent cancellation propagates and the static fallback
// serves.
func TestCatalogBlockingFetchDeadline(t *testing.T) {
	server := httptest.NewServer(withAnalytics(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			<-r.Context().Done()
			return
		}
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"object":"chat.completion","choices":[]}`)
	})))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(ctx, catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != staticServed {
		t.Fatalf("blocked fetch served %s %v, want static", ids(out.Models), err)
	}
}

// The static fallback is validated per account: a fresh credential
// generation with no last-good, a failed search, and an account the
// upstream denies one static model must not receive that model.
func TestCatalogStaticFallbackIsValidatedPerAccount(t *testing.T) {
	f := &searchCatalogFixture{probeStatus: map[string]int{"@cf/openai/gpt-oss-120b": http.StatusForbidden}}
	f.mode.Store(1) // search fails
	server := f.serve(t)
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8,cloudflare/@cf/qwen/qwen3-30b-a3b-fp8,cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("denied fallback model served: %s %v", ids(out.Models), err)
	}
}

// Contributor dedup for a priced base: with the free-only guard on, the
// unpriced "-contributor" sibling is dropped without a probe, but the
// usable priced base must survive (not lose both to the kit dedup).
func TestCatalogContributorDedupKeepsUsablePricedBase(t *testing.T) {
	f := &searchCatalogFixture{}
	f.setBody(searchBody("@cf/openai/gpt-oss-20b", "@cf/openai/gpt-oss-20b-contributor"))
	server := f.serve(t)
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, nil)); err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("priced base lost: %s %v", ids(out.Models), err)
	}
}

// With probing enabled, a contributor the account cannot use is dropped
// in favor of its usable base; when both are usable the contributor still
// wins (the unknown-id fixture above covers that preference).
func TestCatalogContributorRejectionFallsBackToUsableBase(t *testing.T) {
	f := &searchCatalogFixture{probeStatus: map[string]int{"@cf/unknown-x/model-contributor": http.StatusForbidden}}
	f.setBody(searchBody("@cf/unknown-x/model", "@cf/unknown-x/model-contributor"))
	server := f.serve(t)
	if out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), catalogRequest(server, probeOnly)); err != nil || ids(out.Models) != "cloudflare/@cf/unknown-x/model" {
		t.Fatalf("rejected contributor lost the base too: %s %v", ids(out.Models), err)
	}
}

// Shared-budget semantics, scaled down: sixteen uncached probes at 200 ms
// each run in two eight-way batches, so the second batch cannot finish
// inside the call deadline. The call stays prompt and serves the static
// fallback instead of running past the deadline. At production scale the
// same shape applies: sixteen ~9 s probes need two batches under the kit's
// single 15 s ModelsFetchTimeout, and the overrun discards the fresh
// result in favor of fallback.
func TestCatalogSharedDeadlineDiscardsSlowValidation(t *testing.T) {
	var ids16 []string
	for i := 0; i < 16; i++ {
		ids16 = append(ids16, "@cf/budget/m"+string(rune('a'+i)))
	}
	f := &searchCatalogFixture{delay: 200 * time.Millisecond}
	f.setBody(searchBody(ids16...))
	server := f.serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := newCatalog(testGuard(server)).ModelsForAuth(ctx, catalogRequest(server, probeOnly))
	if err != nil {
		t.Fatalf("slow validation must serve fallback, not fail: %v", err)
	}
	if ids(out.Models) != staticServed {
		t.Fatalf("slow validation served %s, want static fallback", ids(out.Models))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("slow validation not bounded: %v", elapsed)
	}
}
