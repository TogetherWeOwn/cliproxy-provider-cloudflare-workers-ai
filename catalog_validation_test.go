package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Live evidence (2026-10-31): models/search lists models the account cannot use.
// Only definitive rejections are dropped; inconclusive probes keep the model,
// catalog order is preserved and definitive verdicts are cached per account.
func TestModelsForAuthAdvertisesOnlyModelsTheAccountCanUse(t *testing.T) {
	var mu sync.Mutex
	probes := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			io.WriteString(w, `{"success":true,"errors":[],"result":[`+
				`{"name":"@cf/ok/alive","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/swiss-ai/apertus-v1.5-8b","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/meta/llama-3.2-11b-vision-instruct","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/cloudflare/clef","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/utter-project/eurollm-9b-it","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/busy/rate-limited","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/flaky/server-error","task":{"name":"Text Generation"}},`+
				`{"name":"@cf/meta/llama-guard-3-8b","task":{"name":"Text Generation"}}`+
				`],"result_info":{"page":1,"total_pages":1}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if r.Method != http.MethodPost || r.URL.Path != "/client/v4/accounts/"+testAccount+"/ai/v1/chat/completions" || json.Unmarshal(raw, &body) != nil || body["max_tokens"] != float64(1) || body["stream"] != false || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Errorf("bad probe %s %s %s", r.Method, r.URL.Path, raw)
		}
		id, _ := body["model"].(string)
		mu.Lock()
		probes[id]++
		mu.Unlock()
		switch id {
		case "@cf/swiss-ai/apertus-v1.5-8b": // 5018 account is not allowed
			w.WriteHeader(http.StatusForbidden)
		case "@cf/meta/llama-3.2-11b-vision-instruct": // license agreement required
			w.WriteHeader(http.StatusForbidden)
		case "@cf/cloudflare/clef": // 5006 not a chat input schema
			w.WriteHeader(http.StatusBadRequest)
		case "@cf/utter-project/eurollm-9b-it": // 4003 model has no config
			w.WriteHeader(http.StatusBadRequest)
		case "@cf/busy/rate-limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "@cf/flaky/server-error":
			w.WriteHeader(http.StatusInternalServerError)
		case "@cf/meta/llama-guard-3-8b": // live: 400 for stream=true, serves stream=false
			if body["stream"] == true {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"\n"},"finish_reason":"stop"}]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"length"}]}`)
		}
	}))
	defer server.Close()
	c := newCatalog()
	req := pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}}
	want := "cloudflare/@cf/ok/alive,cloudflare/@cf/busy/rate-limited,cloudflare/@cf/flaky/server-error,cloudflare/@cf/meta/llama-guard-3-8b"
	start := time.Now()
	for round := 0; round < 2; round++ {
		out, err := c.ModelsForAuth(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(out.Models); got != want {
			t.Fatalf("round %d advertised %s\nwant %s", round, got, want)
		}
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("validation not bounded")
	}
	mu.Lock()
	defer mu.Unlock()
	for id, n := range map[string]int{"@cf/ok/alive": 1, "@cf/swiss-ai/apertus-v1.5-8b": 1, "@cf/meta/llama-3.2-11b-vision-instruct": 1, "@cf/cloudflare/clef": 1, "@cf/utter-project/eurollm-9b-it": 1, "@cf/busy/rate-limited": 2, "@cf/flaky/server-error": 2, "@cf/meta/llama-guard-3-8b": 1} {
		if probes[id] != n {
			t.Errorf("%s probed %d times, want %d", id, probes[id], n)
		}
	}
}

func TestModelsForAuthHonoursCancellationDuringValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/slow/a","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := newCatalog().ModelsForAuth(ctx, pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}}); err == nil {
		t.Fatal("cancelled discovery returned a catalog")
	}
}

func TestVerdictCacheExpires(t *testing.T) {
	c := newVerdictCache()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var digest [32]byte
	c.put(digest, "m", false)
	if usable, ok := c.get(digest, "m"); !ok || usable {
		t.Fatal("fresh verdict missing")
	}
	now = now.Add(verdictTTL)
	if _, ok := c.get(digest, "m"); ok {
		t.Fatal("expired verdict reused")
	}
}

func ids(models []pluginapi.ModelInfo) string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return strings.Join(out, ",")
}
