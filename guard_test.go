package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// withAnalytics answers the account GraphQL read with a fixed reading.
func withAnalytics(neurons float64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/graphql" {
			io.WriteString(w, analyticsBody(fmt.Sprintf(`[{"sum":{"totalNeurons":%v}}]`, neurons)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// analyticsFixture is a mock account: reading is in neurons, status != 0
// replaces the answer with that HTTP status.
type analyticsFixture struct {
	polls   atomic.Int32
	reading atomic.Int64
	status  atomic.Int32
	dates   sync.Map
	delay   time.Duration
}

// testGuard reads analytics from the loopback server and has seen a host
// without a proxy. Its endpoint is the server itself, so no test guard can
// reach the real API.
func testGuard(server *httptest.Server) *freeGuard {
	g := newFreeGuard(server.Client())
	g.endpoint = server.URL + "/client/v4/graphql"
	g.observeHost(pluginapi.HostConfigSummary{})
	return g
}

func (f *analyticsFixture) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/v4/graphql" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Errorf("unexpected guard request %s %s", r.Method, r.URL.Path)
		}
		var wire struct{ Variables map[string]string }
		_ = json.NewDecoder(r.Body).Decode(&wire)
		f.dates.Store(wire.Variables["date"], true)
		f.polls.Add(1)
		time.Sleep(f.delay)
		if s := f.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		body := analyticsBody(fmt.Sprintf(`[{"sum":{"totalNeurons":%d}}]`, f.reading.Load()))
		io.WriteString(w, strings.ReplaceAll(body, testAccount, wire.Variables["account"]))
	}))
	t.Cleanup(server.Close)
	return server
}
func guardAt(now *time.Time, server *httptest.Server) *freeGuard {
	g := testGuard(server)
	g.now = func() time.Time { return *now }
	return g
}
func freeCredentials(t *testing.T, metadata map[string]any) credentials {
	c, err := credentialsFrom(authJSON(testAccount, "example-key"), metadata)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func wantCode(t *testing.T, err error, code string) *pluginabi.Error {
	t.Helper()
	var e *pluginabi.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

func TestGuardThresholdIsStrictAndRetriesAtUTCMidnight(t *testing.T) {
	now := time.Date(2030, 1, 1, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		reading  int64
		estimate float64
		metadata map[string]any
		admitted bool
	}{
		{"under", 9000, 499, nil, true},
		{"at threshold", 9000, 500, nil, false},
		{"over", 9000, 501, nil, false},
		{"reading alone over", 12000, 0, nil, false},
		{"zero usage fits", 0, 9499, nil, true},
		{"custom margin under", 9000, 899, map[string]any{"safety_margin_neurons": float64(100)}, true},
		{"custom margin at", 9000, 900, map[string]any{"safety_margin_neurons": float64(100)}, false},
		{"custom capacity", 19000, 499, map[string]any{"daily_free_neurons": float64(20000)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &analyticsFixture{}
			f.reading.Store(tc.reading)
			server := f.serve(t)
			err := guardAt(&now, server).admit(context.Background(), freeCredentials(t, tc.metadata), tc.estimate)
			if tc.admitted {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			e := wantCode(t, err, "quota_exhausted")
			if e.HTTPStatus != 429 || e.Retryable || e.RetryAfterSeconds != 6*3600 {
				t.Fatalf("refusal %+v", e)
			}
		})
	}
}

func TestGuardTallyChargesAdmissionsUntilAnalyticsCatchUp(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &analyticsFixture{}
	server := f.serve(t)
	g, c := guardAt(&now, server), freeCredentials(t, nil)
	for i := 0; i < 3; i++ { // 3*3000 + 500 < 10000
		if err := g.admit(context.Background(), c, 3000); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		now = now.Add(time.Minute)
	}
	// Only the tally is over: a short retryable rate limit until the oldest
	// admission (7 minutes left) leaves the window, never a wait for midnight.
	wantTallyLimit := func(retry int) {
		t.Helper()
		e := wantCode(t, g.admit(context.Background(), c, 3000), "rate_limit")
		if e.HTTPStatus != 429 || !e.Retryable || e.RetryAfterSeconds != retry {
			t.Fatalf("tally refusal %+v, want retry %ds", e, retry)
		}
	}
	wantTallyLimit(7 * 60)
	// A fresh reading that does not yet show the requests must not erase them.
	now = now.Add(readingTTL)
	wantTallyLimit(6 * 60)
	if f.polls.Load() != 5 { // every step above outlived the 60s reading
		t.Fatalf("polls %d", f.polls.Load())
	}
	// Once the window passes, analytics are authoritative again.
	now = now.Add(tallyWindow)
	f.reading.Store(6000)
	if err := g.admit(context.Background(), c, 3000); err != nil {
		t.Fatal(err)
	}
	// 6000 + 500 + 500 < 10000 without the tally: still a rate limit, retried
	// when the fresh admission expires.
	e := wantCode(t, g.admit(context.Background(), c, 500), "rate_limit")
	if e.RetryAfterSeconds != int(tallyWindow.Seconds()) {
		t.Fatalf("retry %d", e.RetryAfterSeconds)
	}
	// The reading alone leaves no room: wait for the UTC reset.
	if e := wantCode(t, g.admit(context.Background(), c, 3500), "quota_exhausted"); e.Retryable || e.RetryAfterSeconds != 12*3600-14*60 {
		t.Fatalf("reading refusal %+v", e)
	}
}

func TestGuardUsesMostConservativeSettingsPerAccount(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &analyticsFixture{}
	f.reading.Store(8000)
	server := f.serve(t)
	g := guardAt(&now, server)
	strict := freeCredentials(t, map[string]any{"daily_free_neurons": float64(9000), "safety_margin_neurons": float64(100)})
	wide := freeCredentials(t, map[string]any{"daily_free_neurons": float64(20000), "safety_margin_neurons": float64(50)})
	cautious := freeCredentials(t, map[string]any{"daily_free_neurons": float64(20000), "safety_margin_neurons": float64(800)})
	// 8000 + 899 + 100 < 9000 under the strict auth's own settings.
	if err := g.admit(context.Background(), strict, 1); err != nil {
		t.Fatal(err)
	}
	// The permissive auth on the same account inherits the 9000 capacity.
	wantCode(t, g.admit(context.Background(), wide, 1000), "quota_exhausted")
	// 8000 + 1 + 100 + 800 < 9000; from now on the 800 margin applies to every
	// auth on the account.
	if err := g.admit(context.Background(), cautious, 100); err != nil {
		t.Fatal(err)
	}
	// 8000 + 200 + 800 >= 9000, although the wide auth's own margin would fit.
	wantCode(t, g.admit(context.Background(), wide, 200), "quota_exhausted")
	if e := wantCode(t, g.admit(context.Background(), strict, 8200), "request_too_large"); e.HTTPStatus != 400 {
		t.Fatalf("oversized for the account %+v", e)
	}
	// Another account keeps its own settings.
	other, err := credentialsFrom(authJSON(otherAccount, "example-key"), map[string]any{"daily_free_neurons": float64(20000)})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.admit(context.Background(), other, 9000); err != nil {
		t.Fatalf("settings leaked across accounts: %v", err)
	}
}

func TestGuardCachesReadingsForAtMostSixtySeconds(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &analyticsFixture{}
	server := f.serve(t)
	g, c := guardAt(&now, server), freeCredentials(t, nil)
	// Another token on the same account shares the reading and the tally.
	other := freeCredentials(t, map[string]any{"api_key": "second-key", "analytics_api_key": "example-key"})
	for _, step := range []time.Duration{0, time.Second, readingTTL - 2*time.Second} {
		now = now.Add(step)
		if err := g.admit(context.Background(), c, 1); err != nil {
			t.Fatal(err)
		}
		if err := g.admit(context.Background(), other, 1); err != nil {
			t.Fatal(err)
		}
	}
	if f.polls.Load() != 1 {
		t.Fatalf("reading not reused within 60s: %d polls", f.polls.Load())
	}
	now = now.Add(time.Second) // exactly 60s after the read started
	if err := g.admit(context.Background(), c, 1); err != nil || f.polls.Load() != 2 {
		t.Fatalf("reading reused at 60s: %v polls=%d", err, f.polls.Load())
	}
}

func TestGuardFailsClosedOnFailedOrStaleAnalytics(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, status := range []int32{403, 429, 500} {
		f := &analyticsFixture{}
		f.status.Store(status)
		server := f.serve(t)
		e := wantCode(t, guardAt(&now, server).admit(context.Background(), freeCredentials(t, nil), 1), "quota_unverified")
		if e.HTTPStatus != 503 || e.RetryAfterSeconds != 60 {
			t.Fatalf("unverified error %+v", e)
		}
	}
	for _, raw := range []string{`{`, `{"errors":[{"message":"x"}]}`, strings.ReplaceAll(analyticsBody(`[]`), testAccount, otherAccount)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
		err := guardAt(&now, server).admit(context.Background(), freeCredentials(t, nil), 1)
		server.Close()
		wantCode(t, err, "quota_unverified")
	}
	if err := newFreeGuard(nil).admit(context.Background(), freeCredentials(t, nil), 1); err == nil {
		t.Fatal("admitted without an analytics client")
	}
	if err := (*freeGuard)(nil).admit(context.Background(), freeCredentials(t, nil), 1); err == nil {
		t.Fatal("admitted without a guard")
	}

	// A failed refresh may fall back to a same-day reading up to 2 minutes old.
	f := &analyticsFixture{}
	server := f.serve(t)
	g, c := guardAt(&now, server), freeCredentials(t, nil)
	if err := g.admit(context.Background(), c, 1); err != nil {
		t.Fatal(err)
	}
	f.status.Store(500)
	now = now.Add(readingMaxAge - time.Second)
	if err := g.admit(context.Background(), c, 1); err != nil {
		t.Fatalf("recent reading rejected: %v", err)
	}
	now = now.Add(time.Second)
	wantCode(t, g.admit(context.Background(), c, 1), "quota_unverified")
	f.status.Store(0)
	if err := g.admit(context.Background(), c, 1); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
	for _, estimate := range []float64{math.Inf(1), math.NaN(), -1} {
		wantCode(t, g.admit(context.Background(), c, estimate), "quota_unverified")
	}
	// Never admissible: rejected as a client error, without a read or cooldown.
	polls := f.polls.Load()
	if e := wantCode(t, g.admit(context.Background(), c, 9500), "request_too_large"); e.HTTPStatus != 400 || f.polls.Load() != polls {
		t.Fatalf("oversized request %+v polls=%d", e, f.polls.Load())
	}
}

func TestGuardRollsOverAtUTCMidnight(t *testing.T) {
	now := time.Date(2030, 1, 1, 23, 59, 30, 0, time.UTC)
	f := &analyticsFixture{}
	f.reading.Store(9600)
	server := f.serve(t)
	g, c := guardAt(&now, server), freeCredentials(t, nil)
	if e := wantCode(t, g.admit(context.Background(), c, 1), "quota_exhausted"); e.RetryAfterSeconds != 30 {
		t.Fatalf("retry %d, want 30s to midnight", e.RetryAfterSeconds)
	}
	// 35s later is inside the 60s TTL but a new UTC day: yesterday's reading
	// must not be reused.
	now = now.Add(35 * time.Second)
	f.reading.Store(0)
	if err := g.admit(context.Background(), c, 1); err != nil || f.polls.Load() != 2 {
		t.Fatalf("new day %v polls=%d", err, f.polls.Load())
	}
	for _, day := range []string{"2030-01-01", "2030-01-02"} {
		if _, ok := f.dates.Load(day); !ok {
			t.Fatalf("no read for %s", day)
		}
	}

	// A read that starts before and ends after midnight is never trusted.
	var clock atomic.Int64
	clock.Store(time.Date(2030, 1, 1, 23, 59, 59, 0, time.UTC).Unix())
	crossing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clock.Add(2)
		io.WriteString(w, analyticsBody(`[]`))
	}))
	defer crossing.Close()
	g = testGuard(crossing)
	g.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	wantCode(t, g.admit(context.Background(), c, 1), "quota_unverified")
}

func TestGuardIsConcurrencySafeAndSingleFlight(t *testing.T) {
	f := &analyticsFixture{delay: 50 * time.Millisecond}
	server := f.serve(t)
	g, c := testGuard(server), freeCredentials(t, nil)
	var admitted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := g.admit(context.Background(), c, 100); {
			case err == nil:
				admitted.Add(1)
			default:
				var e *pluginabi.Error
				if errors.As(err, &e) && e.Code == "rate_limit" && e.Retryable {
					refused.Add(1)
				} else {
					t.Errorf("unexpected %v", err)
				}
			}
		}()
	}
	wg.Wait()
	// The k-th admission needs (k-1)*100 + 100 + 500 < 10000: k <= 94. The
	// rest are refused by the tally alone, so they are retryable rate limits.
	if admitted.Load() != 94 || refused.Load() != 106 || f.polls.Load() != 1 {
		t.Fatalf("admitted=%d refused=%d polls=%d", admitted.Load(), refused.Load(), f.polls.Load())
	}
}

func TestGuardWaiterHonoursItsOwnCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, analyticsBody(`[]`))
	}))
	defer server.Close()
	defer close(release)
	g, c := testGuard(server), freeCredentials(t, nil)
	go g.admit(context.Background(), c, 1)
	for {
		g.mu.Lock()
		l := g.accounts[c.account]
		started := l != nil && l.pending != nil
		g.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.admit(ctx, c, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter ignored cancellation: %v", err)
	}
}

func TestExecutorRefusesBeforeDispatchOnEveryPath(t *testing.T) {
	var chats atomic.Int32
	server := httptest.NewServer(withAnalytics(9900, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chats.Add(1)
		io.WriteString(w, `{"choices":[]}`)
	})))
	defer server.Close()
	ex := newExecutor(testGuard(server))
	req := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8","max_tokens":1}`), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}}
	_, a := ex.Execute(context.Background(), req)
	_, b := ex.ExecuteStream(context.Background(), req)
	_, h := ex.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Body: req.Payload, StorageJSON: req.StorageJSON, HTTPClient: req.HTTPClient})
	for _, err := range []error{a, b, h} {
		wantCode(t, err, "quota_exhausted")
	}
	if chats.Load() != 0 {
		t.Fatalf("%d requests dispatched past the guard", chats.Load())
	}
}

func TestGuardReadsAnalyticsWithoutTheHostClient(t *testing.T) {
	server := httptest.NewServer(withAnalytics(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	})))
	defer server.Close()
	host := &recordingHost{mockHostClient: mockHostClient{server}}
	f := &analyticsFixture{}
	analytics := f.serve(t)
	payload := []byte(`{"model":"cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8","max_tokens":1}`)
	if _, err := newExecutor(testGuard(analytics)).Execute(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: host}); err != nil {
		t.Fatal(err)
	}
	if got := host.urls(); f.polls.Load() != 1 || len(got) != 1 || !strings.HasSuffix(got[0], "/ai/v1/chat/completions") {
		t.Fatalf("host client saw %v, guard polls %d", got, f.polls.Load())
	}
}

type recordingHost struct {
	mockHostClient
	mu   sync.Mutex
	seen []string
}

func (h *recordingHost) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	h.seen = append(h.seen, r.URL)
	h.mu.Unlock()
	return h.mockHostClient.Do(ctx, r)
}
func (h *recordingHost) urls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// The read must never be sent direct while the host's proxy-url is unknown.
// A recording transport sees every attempted analytics request and the proxy
// the kit transport would use for it; it never dials.
type recordingTransport struct {
	mu   sync.Mutex
	seen []string
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	via := "inherit"
	if u, err := kit.QuotaProxy(r); err != nil {
		via = "invalid"
	} else if u != nil {
		via = u.String()
	}
	rt.mu.Lock()
	rt.seen = append(rt.seen, r.Method+" "+r.URL.String()+" via "+via)
	rt.mu.Unlock()
	return nil, errors.New("recording transport never dials")
}
func (rt *recordingTransport) requests() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.seen...)
}

const unroutableAnalytics = "http://127.0.0.1:1/client/v4/graphql"

func TestGuardReadFollowsAuthThenHostProxyAndNeverGuessesDirect(t *testing.T) {
	// Loopback discard ports: recorded only, never dialled.
	authProxy, hostProxy := "http://127.0.0.1:9", "http://127.0.0.2:9"
	rt := &recordingTransport{}
	g := newFreeGuard(&http.Client{Transport: rt})
	g.endpoint = unroutableAnalytics
	viaAuth := freeCredentials(t, map[string]any{"proxy_url": authProxy})
	plain := freeCredentials(t, nil)
	// Before the host's proxy-url is known, an auth without its own proxy is
	// refused without any analytics attempt, and nothing is dispatched.
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { chats.Add(1) }))
	defer server.Close()
	_, err := newExecutor(g).Execute(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"cloudflare/@cf/openai/gpt-oss-20b","max_tokens":1}`), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	wantCode(t, err, "quota_unverified")
	wantCode(t, g.admit(context.Background(), plain, 1), "quota_unverified")
	if got := rt.requests(); len(got) != 0 || chats.Load() != 0 {
		t.Fatalf("attempted %v and %d chats before the host proxy was known", got, chats.Load())
	}
	// The auth's own proxy, then the host's, then an invalid setting.
	wantCode(t, g.admit(context.Background(), viaAuth, 1), "quota_unverified")
	g.observeHost(pluginapi.HostConfigSummary{ProxyURL: hostProxy})
	wantCode(t, g.admit(context.Background(), plain, 1), "quota_unverified")
	g.observeHost(pluginapi.HostConfigSummary{ProxyURL: "not a proxy"})
	wantCode(t, g.admit(context.Background(), plain, 1), "quota_unverified")
	want := []string{"POST " + unroutableAnalytics + " via " + authProxy, "POST " + unroutableAnalytics + " via " + hostProxy, "POST " + unroutableAnalytics + " via invalid"}
	if got := rt.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("analytics attempts\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if chats.Load() != 0 {
		t.Fatal("dispatched without a verified reading")
	}
}

// The plugin's real analytics client sends the read through the chosen proxy.
func TestGuardReadUsesTheProxyWithTheRealTransport(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	proxy := func(name string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Method+" "+r.URL.String())
			mu.Unlock()
			// Never forward: the read fails closed and nothing leaves loopback.
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(s.Close)
		return s.URL
	}
	authProxy, hostProxy := proxy("auth"), proxy("host")
	client := newAnalyticsClient()
	defer client.CloseIdleConnections()
	g := newFreeGuard(client)
	g.endpoint = unroutableAnalytics
	wantCode(t, g.admit(context.Background(), freeCredentials(t, map[string]any{"proxy_url": authProxy}), 1), "quota_unverified")
	g.observeHost(pluginapi.HostConfigSummary{ProxyURL: hostProxy})
	wantCode(t, g.admit(context.Background(), freeCredentials(t, nil), 1), "quota_unverified")
	mu.Lock()
	defer mu.Unlock()
	want := "POST " + unroutableAnalytics
	if len(seen["host"]) != 1 || seen["host"][0] != want || len(seen["auth"]) != 1 || seen["auth"][0] != want {
		t.Fatalf("proxies saw %v", seen)
	}
}

func TestProbesRefusedByTheGuardAreNotSent(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(withAnalytics(9999, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/openai/gpt-oss-20b","task":{"name":"Text Generation"}},{"name":"@cf/qwen/qwen3-30b-a3b-fp8","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
			return
		}
		probes.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})))
	defer server.Close()
	out, err := newCatalog(testGuard(server)).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 2 || probes.Load() != 0 {
		t.Fatalf("models=%d probes=%d err=%v", len(out.Models), probes.Load(), err)
	}
}

func TestEstimateBoundsPromptTemplateAndOutput(t *testing.T) {
	body := []byte(`{"max_tokens":1000,"model":"@cf/openai/gpt-oss-120b","stream":false}`)
	want := math.Ceil((float64(len(body)+512)*31818 + 1000*68182) / 1e6)
	if got := estimateNeurons(neuronRates["@cf/openai/gpt-oss-120b"], body, 1000); got != want {
		t.Fatalf("estimate %v, want %v", got, want)
	}
	probe := probeBody("@cf/openai/gpt-oss-20b")
	if got := estimateNeurons(neuronRates["@cf/openai/gpt-oss-20b"], probe, 1); got != math.Ceil((float64(len(probe)+512)*18182+27273)/1e6) {
		t.Fatalf("probe estimate %v", got)
	}
}
