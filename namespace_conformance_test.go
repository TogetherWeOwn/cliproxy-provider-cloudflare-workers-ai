package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every id this plugin can emit carries the fixed "cloudflare/" namespace
// exactly once, including the second "@cf/" layer: static fallback,
// registrar, and a discovery refresh. The free-allocation spend guard is
// untouched; this test only reads the catalog.
func TestModelNamespaceConformance(t *testing.T) {
	ctx := context.Background()
	var all []pluginapi.ModelInfo
	all = append(all, fallbackModels()...)

	guard := newFreeGuard(nil)
	reg, err := catalog{}.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, reg.Models...)

	static, err := newCatalog(guard).StaticModels(ctx, pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, static.Models...)

	server := httptest.NewServer(withAnalytics(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/accounts/"+testAccount+"/ai/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if r.URL.Path != "/client/v4/accounts/"+testAccount+"/ai/models/search" {
			t.Error("invalid discovery wire request")
		}
		io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/meta/llama-3.1-8b-instruct-fp8","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
	})))
	defer server.Close()
	discovered, err := newCatalog(testGuard(server)).ModelsForAuth(ctx, pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, discovered.Models...)

	if len(all) == 0 {
		t.Fatal("no models to check")
	}
	kit.AssertModelNamespace(t, "cloudflare", all)
}
