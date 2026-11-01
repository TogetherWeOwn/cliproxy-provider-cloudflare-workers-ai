package main

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/cloudflare-workers-ai-id"
	testSubmitPath   = "/v0/management/plugin-login/cloudflare-workers-ai"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

func startTestLogin(t *testing.T) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	return startTestLoginTTL(t, 0)
}

func startTestLoginTTL(t *testing.T, ttl time.Duration) (*authProvider, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	ctx := context.Background()
	// A nil guard is safe: observeHost and observeAuth ignore a nil receiver.
	p, err := newAuthProviderWithLoginTTL(nil, ttl)
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
	// The analytics key is a Secret login field: the page reads it with hidden
	// input, like the API key, while the account ID stays visible.
	if !strings.Contains(body, "read -rs PLUGIN_FIELD_ANALYTICS_API_KEY") || !strings.Contains(body, "read -r PLUGIN_FIELD_ACCOUNT_ID") {
		t.Fatalf("analytics key not read as a secret: %s", body)
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
	ctx := context.Background()
	p, _ := startTestLogin(t)
	first := loginOnce(t, p, testAccount)
	second := loginOnce(t, p, otherAccount)
	if first.FileName == "" || first.FileName == second.FileName {
		t.Fatalf("accounts share a login filename: %q vs %q", first.FileName, second.FileName)
	}
	if first.ID == second.ID {
		t.Fatalf("accounts share an auth ID: %q", first.ID)
	}
	// Both ownership entries survive: a host parse of each saved login file
	// commits under its own file name, and either identity under a third
	// filename is a duplicate. (The kit guard's maps are its own, and login
	// parses only reserve ownership, so this is asserted behaviorally.)
	for _, tc := range []struct {
		auth    pluginapi.AuthData
		account string
	}{{first, testAccount}, {second, otherAccount}} {
		if _, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: tc.auth.FileName, RawJSON: authJSON(tc.account, "example-key")}); err != nil {
			t.Fatalf("host parse of %s: %v", tc.auth.FileName, err)
		}
		if _, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: "third.json", RawJSON: authJSON(tc.account, "example-key")}); err == nil || kit.MapError(err).Code != "duplicate_auth" {
			t.Fatalf("third filename for %s = %v, want duplicate_auth", tc.account, err)
		}
	}
	// Re-login of the first identity stays idempotent, including under a
	// mixed-case spelling of the account (credentialsFields canonicalizes
	// to lowercase): same filename, no duplicate_auth, ownership unchanged.
	again := loginOnce(t, p, strings.ToUpper(testAccount))
	if again.FileName != first.FileName || again.ID != first.ID {
		t.Fatalf("re-login not idempotent: %+v vs %+v", again, first)
	}
	if _, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: first.FileName, RawJSON: authJSON(testAccount, "example-key")}); err != nil {
		t.Fatalf("host reparse after re-login: %v", err)
	}
	if _, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: "third.json", RawJSON: authJSON(testAccount, "example-key")}); err == nil || kit.MapError(err).Code != "duplicate_auth" {
		t.Fatalf("re-login churned ownership: %v", err)
	}
}

// TestLoginAbandonedSubmitReleasesOwnership mirrors the kit regression for
// TWO-908/kit#13: a key submitted through login but never polled must only be
// reserved until the login session expires. Afterwards the same account and
// token parse under a different filename instead of failing duplicate_auth
// until a plugin restart.
func TestLoginAbandonedSubmitReleasesOwnership(t *testing.T) {
	ctx := context.Background()
	p, start := startTestLoginTTL(t, 2*time.Second)
	submit := func(state string) pluginapi.ManagementResponse {
		resp, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
			Path: testSubmitPath,
			Body: []byte(`{"state":"` + state + `","api_key":"example-key","account_id":"` + testAccount + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	upload := func() error {
		_, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{FileName: "upload.json", RawJSON: authJSON(testAccount, "example-key")})
		return err
	}

	// Submit a key, then abandon the login (never poll).
	if resp := submit(start.State); resp.StatusCode != 200 {
		t.Fatalf("submit %+v", resp)
	}
	// The in-flight login still blocks a different filename.
	if err := upload(); err == nil || kit.MapError(err).Code != "duplicate_auth" {
		t.Fatalf("upload during login = %v, want duplicate_auth", err)
	}
	// The kit guard's clock is not injectable from this package, so retry the
	// upload until the abandoned session's reservation lapses instead of
	// sleeping for one fixed span; the deadline only bounds a broken guard.
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := upload()
		if err == nil {
			break
		}
		if kit.MapError(err).Code != "duplicate_auth" {
			t.Fatalf("upload after abandoned session: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandoned login still holds ownership after expiry: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Submit-time duplicate feedback still works against the committed owner.
	second, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: provider,
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/cloudflare-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp := submit(second.State); resp.StatusCode != 400 || !strings.Contains(string(resp.Body), "duplicate_auth") {
		t.Fatalf("duplicate submit %+v %s", resp, resp.Body)
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
