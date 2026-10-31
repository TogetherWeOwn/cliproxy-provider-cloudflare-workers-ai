// Parser cases adapted from cpa-plugin-subscription-pool at
// ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b, internal/providers/cloudflare/quota_test.go (MIT).
package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func analyticsBody(rows string) string {
	return `{"data":{"viewer":{"accounts":[{"accountTag":"` + testAccount + `","aiInferenceAdaptiveGroups":` + rows + `}]}}}`
}
func TestAnalyticsParserRequiresCompleteMatchingAccountData(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
		bad  bool
	}{
		{analyticsBody(`[{"count":99999,"sum":{"totalNeurons":1234.5}}]`), 1234.5, false},
		{analyticsBody(`[{"sum":{"totalNeurons":0}}]`), 0, false},
		{analyticsBody(`[]`), 0, false},
		{analyticsBody(`[{"sum":{"totalNeurons":1}},{"sum":{"totalNeurons":2}}]`), 0, true},
		{analyticsBody(`[{"sum":{"totalNeurons":null}}]`), 0, true},
		{analyticsBody(`[{"count":1}]`), 0, true},
		{analyticsBody(`[{"sum":{"totalNeurons":-1}}]`), 0, true},
		{analyticsBody(`[{"sum":{"totalNeurons":1e999}}]`), 0, true},
		{analyticsBody(`null`), 0, true},
		{`{"data":{"viewer":{"accounts":[]}}}`, 0, true},
		{strings.ReplaceAll(analyticsBody(`[]`), testAccount, otherAccount), 0, true},
		{`{"errors":[{"message":"example-secret"}],"data":{"viewer":{"accounts":[]}}}`, 0, true},
		{`{`, 0, true},
	} {
		got, err := parseAnalytics([]byte(tc.raw), testAccount)
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("got %v %v bad=%v", got, err, tc.bad)
		}
		if err != nil && strings.Contains(err.Error(), "example-secret") {
			t.Fatal("body leaked")
		}
	}
}
func TestDailyQuotaUsesAccountWideUTCNeuronsAndSeparateAnalyticsToken(t *testing.T) {
	var polls atomic.Int32
	now := time.Date(2030, 1, 1, 23, 59, 50, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/client/v4/graphql" || r.Header.Get("Authorization") != "Bearer analytics-key" {
			t.Error("wrong GraphQL auth or endpoint")
		}
		var body struct {
			Query     string
			Variables map[string]string
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Variables["account"] != testAccount || body.Variables["date"] != "2030-01-01" {
			t.Error("wrong account or UTC date")
		}
		for _, part := range []string{"aiInferenceAdaptiveGroups", "totalNeurons", "date_geq", "date_leq", "$account: string!", "$date: Date!"} {
			if !strings.Contains(body.Query, part) {
				t.Errorf("query missing %s", part)
			}
		}
		if strings.Contains(body.Query, "dimensions") || strings.Contains(body.Query, "modelId") {
			t.Error("quota must not filter/group by model")
		}
		io.WriteString(w, analyticsBody(`[{"sum":{"totalNeurons":12500}}]`))
	}))
	defer server.Close()
	q := newQuotaProvider(server.URL+"/client/v4/graphql", server.Client())
	q.now = func() time.Time { return now }
	req := pluginapi.QuotaFetchRequest{Provider: "cloudflare-workers-ai", AuthID: "example-account", StorageJSON: authJSON(testAccount, "example-key"), Metadata: map[string]any{"analytics_api_key": "analytics-key"}}
	for i := 0; i < 2; i++ {
		out, err := q.FetchQuota(context.Background(), req)
		if err != nil || len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 1 {
			t.Fatalf("quota %v %v", out, err)
		}
		b := out.Groups[0].Buckets[0]
		if b.Window != "daily" || b.RemainingFraction != 0 || b.ResetTime != "2030-01-02T00:00:00Z" {
			t.Fatalf("bad bucket %+v", b)
		}
		metrics := map[string]float64{}
		for _, m := range out.Summary {
			metrics[m.Key] = m.Value
		}
		if metrics["daily_free_neurons"] != 10000 || metrics["neurons_used_today"] != 12500 || metrics["neurons_above_free"] != 2500 {
			t.Fatalf("bad summary %v", metrics)
		}
	}
	if polls.Load() != 1 {
		t.Fatal("missing 60s cache")
	}
	req.Metadata["daily_free_neurons"] = float64(25000)
	out, err := q.FetchQuota(context.Background(), req)
	if err != nil || out.Groups[0].Buckets[0].RemainingFraction != .5 || polls.Load() != 2 {
		t.Fatal("capacity change cached incorrectly", err)
	}
	desc, err := q.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil || desc.SupportsReset || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != "cloudflare-workers-ai" {
		t.Fatal("quota capability mismatch")
	}
	if _, err := q.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{}); err == nil {
		t.Fatal("offered reset")
	}
}
func TestDailyQuotaInvalidatesCacheAtUTCMidnight(t *testing.T) {
	now := time.Date(2030, 1, 1, 23, 59, 50, 0, time.UTC)
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		io.WriteString(w, analyticsBody(`[{"sum":{"totalNeurons":2500}}]`))
	}))
	defer server.Close()
	q := newQuotaProvider(server.URL, server.Client())
	q.now = func() time.Time { return now }
	req := pluginapi.QuotaFetchRequest{AuthID: "account", StorageJSON: authJSON(testAccount, "example-key")}
	a, err := q.FetchQuota(context.Background(), req)
	if err != nil || a.Groups[0].Buckets[0].ResetTime != "2030-01-02T00:00:00Z" {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	b, err := q.FetchQuota(context.Background(), req)
	if err != nil || polls.Load() != 2 || b.Groups[0].Buckets[0].ResetTime != "2030-01-03T00:00:00Z" {
		t.Fatal("previous day cached past reset", err)
	}
}
func TestAnalyticsReadCrossingMidnightIsNotCached(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2030, 1, 1, 23, 59, 59, 0, time.UTC).Unix())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { clock.Add(2); io.WriteString(w, analyticsBody(`[]`)) }))
	defer server.Close()
	q := newQuotaProvider(server.URL, server.Client())
	q.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	if _, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "account", StorageJSON: authJSON(testAccount, "example-key")}); err == nil {
		t.Fatal("cross-day response accepted")
	}
}
func TestQuotaErrorsAreBoundedRedactedAndNeverFabricateZero(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, path := range []string{"/denied", "/huge", "/partial", "/redirect"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch path {
			case "/denied":
				w.WriteHeader(403)
				io.WriteString(w, "example-secret")
			case "/huge":
				io.WriteString(w, strings.Repeat("x", (1<<20)+1))
			case "/partial":
				io.WriteString(w, `{"errors":[{"message":"example-secret"}]}`)
			case "/redirect":
				http.Redirect(w, r, target.URL, 302)
			}
		}))
		client := newAnalyticsClient()
		q := newQuotaProvider(server.URL+path, client)
		out, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "account", StorageJSON: authJSON(testAccount, "example-key")})
		client.CloseIdleConnections()
		server.Close()
		if err == nil || len(out.Groups) != 0 || strings.Contains(err.Error(), "example-secret") {
			t.Fatalf("unsafe analytics error %v", err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("analytics token followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newQuotaProvider("", nil).FetchQuota(ctx, pluginapi.QuotaFetchRequest{}); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, used := range []float64{math.NaN(), math.Inf(1), -1} {
		if _, err := normalizeQuota(used, 10000, time.Now()); err == nil {
			t.Fatal("invalid usage accepted")
		}
	}
}
