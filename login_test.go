package main

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/cloudflare-workers-ai-id"
	testSubmitPath   = "/v0/management/plugin-login/cloudflare-workers-ai"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

func startTestLogin(t *testing.T) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	ctx := context.Background()
	// A nil guard is safe: observeHost and observeAuth ignore a nil receiver.
	p, err := newAuthProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Login == nil {
		t.Fatal("API-key login not wired")
	}
	reg, err := p.Login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase})
	if err != nil {
		t.Fatal(err)
	}
	var sawSubmit, sawPage bool
	for _, r := range reg.Routes {
		if r.Method == "POST" && r.Path == "plugin-login/cloudflare-workers-ai" {
			sawSubmit = true
		}
	}
	for _, r := range reg.Resources {
		if r.Path == "login" {
			sawPage = true
		}
	}
	if !sawSubmit || !sawPage {
		t.Fatalf("login routes not registered: %+v", reg)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: provider,
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/cloudflare-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	return p, start
}

func TestAPIKeyLoginWired(t *testing.T) {
	ctx := context.Background()
	p, start := startTestLogin(t)
	// The kit returns a root-relative login URL so the panel resolves it
	// against its own origin instead of the host's loopback callback base.
	if !strings.HasPrefix(start.URL, testResourceBase+"/login?state=") {
		t.Fatalf("unexpected login URL %q", start.URL)
	}
	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}
	page, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET",
		Path: testResourceBase + "/login", Query: map[string][]string{"state": {start.State}}})
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("login page %+v %v", page, err)
	}
	body := html.UnescapeString(string(page.Body))
	for _, want := range []string{"Account ID", "Analytics API key"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing prompt %q", want)
		}
	}
	// The analytics token is a secret: hidden input, like the main API key.
	// The account ID stays visible.
	for _, want := range []string{"read -rs PLUGIN_API_KEY", "read -rs PLUGIN_FIELD_ANALYTICS_API_KEY", "read -r PLUGIN_FIELD_ACCOUNT_ID"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing login field wiring %q", want)
		}
	}
	if strings.Contains(body, "read -r PLUGIN_FIELD_ANALYTICS_API_KEY") {
		t.Fatal("analytics key read with echoing input")
	}
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","account_id":"` + testAccount + `"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v %v", done, err)
	}
	var store map[string]any
	if err := json.Unmarshal(done.Auth.StorageJSON, &store); err != nil {
		t.Fatal(err)
	}
	if done.Auth.Provider != provider || store["account_id"] != testAccount || store["api_key"] != "example-key" {
		t.Fatalf("credentials lost through login: %+v", done.Auth)
	}
	other, err := newAuthProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := other.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: "ref.json", RawJSON: authJSON(testAccount, "example-key")})
	if err != nil || done.Auth.ID != ref.Auth.ID || !strings.HasPrefix(done.Auth.ID, provider+":") {
		t.Fatalf("auth ID not derived from account+token: %q vs %q (%v)", done.Auth.ID, ref.Auth.ID, err)
	}
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("consumed login redelivered: %+v %v", again, err)
	}
}

// loginOnce runs one full login (start, submit, poll) against an already
// registered provider and returns the delivered auth.
func loginOnce(t *testing.T, p *authProvider, account string) pluginapi.AuthData {
	t.Helper()
	ctx := context.Background()
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: provider,
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/cloudflare-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","account_id":"` + account + `"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit for %s: %+v %v", account, submit, err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll for %s: %+v %v", account, done, err)
	}
	return done.Auth
}

// TestAPIKeyLoginSameTokenTwoAccounts is the regression test for the kit
// filename collision: identity is account+token, so the same token under
// two accounts must mint two file names and keep both ownership entries,
// instead of the second login overwriting the first account's entry.
func TestAPIKeyLoginSameTokenTwoAccounts(t *testing.T) {
	p, _ := startTestLogin(t)
	first := loginOnce(t, p, testAccount)
	second := loginOnce(t, p, otherAccount)
	if first.FileName == "" || first.FileName == second.FileName {
		t.Fatalf("accounts share a login filename: %q vs %q", first.FileName, second.FileName)
	}
	if first.ID == second.ID {
		t.Fatalf("accounts share an auth ID: %q", first.ID)
	}
	p.mu.Lock()
	files, owners := len(p.files), len(p.owners)
	p.mu.Unlock()
	if files != 2 || owners != 2 {
		t.Fatalf("ownership entries lost: files=%d owners=%d, want 2/2", files, owners)
	}
	// Re-login of the first identity stays idempotent: same filename, no
	// duplicate_auth, no extra ownership entry.
	again := loginOnce(t, p, testAccount)
	if again.FileName != first.FileName || again.ID != first.ID {
		t.Fatalf("re-login not idempotent: %+v vs %+v", again, first)
	}
	p.mu.Lock()
	files, owners = len(p.files), len(p.owners)
	p.mu.Unlock()
	if files != 2 || owners != 2 {
		t.Fatalf("re-login churned ownership: files=%d owners=%d, want 2/2", files, owners)
	}
}

func TestAPIKeyLoginRejectsBlankAccount(t *testing.T) {
	p, start := startTestLogin(t)
	submit, err := p.Login.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","account_id":""}`)})
	if err != nil {
		t.Fatal(err)
	}
	if submit.StatusCode != 400 || !strings.Contains(string(submit.Body), "account_id") {
		t.Fatalf("blank account accepted or wrong error: %+v %s", submit, submit.Body)
	}
	if strings.Contains(string(submit.Body), "example-key") {
		t.Fatalf("rejection echoed the key: %s", submit.Body)
	}
}
