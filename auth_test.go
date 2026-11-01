package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var testAccount = strings.Repeat("a", 32)
var otherAccount = strings.Repeat("b", 32)

func authJSON(account, key string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "cloudflare-workers-ai", "account_id": account, "api_key": key})
	return raw
}
func TestAuthStableAccountAndTokenIdentity(t *testing.T) {
	p := newAuthProvider()
	parse := func(file, account, key string) (pluginapi.AuthParseResponse, error) {
		return p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{FileName: file, RawJSON: authJSON(account, key)})
	}
	a, err := parse("first.json", testAccount, "example-key")
	if err != nil || !a.Handled || !strings.HasPrefix(a.Auth.ID, "cloudflare-workers-ai:") {
		t.Fatalf("auth %v %v", a.Handled, err)
	}
	b, err := parse("first.json", testAccount, " example-key ")
	if err != nil || a.Auth.ID != b.Auth.ID {
		t.Fatal("unstable identity", err)
	}
	if _, err := parse("duplicate.json", testAccount, "example-key"); err == nil {
		t.Fatal("duplicate accepted")
	}
	c, err := parse("other.json", otherAccount, "example-key")
	if err != nil || c.Auth.ID == a.Auth.ID {
		t.Fatal("account absent from identity", err)
	}
	if _, err := parse("first.json", testAccount, "replacement-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := parse("duplicate.json", testAccount, "example-key"); err != nil {
		t.Fatal("old identity not released", err)
	}
	var saved map[string]any
	if json.Unmarshal(a.Auth.StorageJSON, &saved) != nil || saved["account_id"] != testAccount || saved["api_key"] != "example-key" {
		t.Fatal("bad storage")
	}
	refreshed, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: a.Auth.ID, StorageJSON: a.Auth.StorageJSON, Metadata: a.Auth.Metadata})
	if err != nil || string(refreshed.Auth.StorageJSON) != string(a.Auth.StorageJSON) {
		t.Fatal("refresh changed credentials", err)
	}
	if _, err = p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("offered OAuth")
	}
	if _, err = p.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{}); err == nil {
		t.Fatal("polled OAuth")
	}
}
func TestAuthValidationAndMetadataPrecedence(t *testing.T) {
	p := newAuthProvider()
	for _, raw := range []string{
		`{}`, `{"type":"cloudflare-workers-ai","api_key":"example-key"}`, `{"type":"cloudflare-workers-ai","account_id":"../escape","api_key":"example-key"}`,
		string(authJSON(testAccount, "")),
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"id":"custom"}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"analytics_api_key":false}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"daily_free_neurons":0}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"daily_free_neurons":"10000"}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"disabled":"false"}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"stay_within_free":"false"}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"stay_within_free":0}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"stay_within_free":null}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"safety_margin_neurons":0}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"safety_margin_neurons":-500}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"safety_margin_neurons":"500"}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"safety_margin_neurons":null}`,
		strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"safety_margin_neurons":1e999}`,
	} {
		out, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{FileName: "invalid.json", RawJSON: []byte(raw)})
		if raw == `{}` {
			if err != nil || out.Handled {
				t.Fatal("claimed unrelated auth")
			}
			continue
		}
		if err == nil {
			t.Fatalf("accepted invalid auth %s", raw)
		}
		if strings.Contains(err.Error(), "example-key") {
			t.Fatal("secret leaked")
		}
	}
	cred, err := credentialsFrom(authJSON(testAccount, "example-key"), map[string]any{"account_id": otherAccount, "api_key": "override-key", "analytics_api_key": "analytics-key", "daily_free_neurons": float64(25000)})
	if err != nil || cred.account != otherAccount || cred.token != "override-key" || cred.analyticsToken != "analytics-key" || cred.capacity != 25000 {
		t.Fatalf("metadata precedence failed %v", err)
	}
	cred, err = credentialsFrom(authJSON(testAccount, "example-key"), nil)
	if err != nil || cred.capacity != 10000 || cred.analyticsToken != "example-key" || !cred.stayWithinFree || cred.safetyMargin != 500 {
		t.Fatal("wrong defaults", err)
	}
	valid := strings.TrimSuffix(string(authJSON(testAccount, "example-key")), "}") + `,"stay_within_free":false,"safety_margin_neurons":250.5}`
	out, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{FileName: "opt-out.json", RawJSON: []byte(valid)})
	if err != nil || !out.Handled {
		t.Fatal("rejected valid guard fields", err)
	}
	cred, err = credentialsFrom(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil || cred.stayWithinFree || cred.safetyMargin != 250.5 {
		t.Fatalf("guard fields not persisted %+v %v", cred.stayWithinFree, err)
	}
	cred, err = credentialsFrom(out.Auth.StorageJSON, map[string]any{"stay_within_free": true, "safety_margin_neurons": float64(900)})
	if err != nil || !cred.stayWithinFree || cred.safetyMargin != 900 {
		t.Fatal("metadata overlay ignored for guard fields", err)
	}
	if _, err = credentialsFrom(out.Auth.StorageJSON, map[string]any{"stay_within_free": "no"}); err == nil {
		t.Fatal("invalid metadata overlay accepted")
	}
}
