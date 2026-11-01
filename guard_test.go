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

func (f *analyticsFixture) serve(t *testing.T) (*httptest.Server, pluginapi.HostHTTPClient) {
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
		io.WriteString(w, analyticsBody(fmt.Sprintf(`[{"sum":{"totalNeurons":%d}}]`, f.reading.Load())))
	}))
	t.Cleanup(server.Close)
	return server, mockHostClient{server}
}
func guardAt(now *time.Time) *freeGuard {
	g := newFreeGuard()
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
			_, client := f.serve(t)
			err := guardAt(&now).admit(context.Background(), client, freeCredentials(t, tc.metadata), tc.estimate)
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
	_, client := f.serve(t)
	g, c := guardAt(&now), freeCredentials(t, nil)
	for i := 0; i < 3; i++ { // 3*3000 + 500 < 10000
		if err := g.admit(context.Background(), client, c, 3000); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
	}
	wantCode(t, g.admit(context.Background(), client, c, 3000), "quota_exhausted")
	// A fresh reading that does not yet show the requests must not erase them.
	now = now.Add(readingTTL)
	wantCode(t, g.admit(context.Background(), client, c, 3000), "quota_exhausted")
	if f.polls.Load() != 2 {
		t.Fatalf("polls %d", f.polls.Load())
	}
	// Once the window passes, analytics are authoritative again.
	now = now.Add(tallyWindow - readingTTL)
	f.reading.Store(6000)
	if err := g.admit(context.Background(), client, c, 3000); err != nil {
		t.Fatal(err)
	}
	wantCode(t, g.admit(context.Background(), client, c, 500), "quota_exhausted")
}

func TestGuardCachesReadingsForAtMostSixtySeconds(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f := &analyticsFixture{}
	_, client := f.serve(t)
	g, c := guardAt(&now), freeCredentials(t, nil)
	// Another token on the same account shares the reading and the tally.
	other := freeCredentials(t, map[string]any{"api_key": "second-key", "analytics_api_key": "example-key"})
	for _, step := range []time.Duration{0, time.Second, readingTTL - 2*time.Second} {
		now = now.Add(step)
		if err := g.admit(context.Background(), client, c, 1); err != nil {
			t.Fatal(err)
		}
		if err := g.admit(context.Background(), client, other, 1); err != nil {
			t.Fatal(err)
		}
	}
	if f.polls.Load() != 1 {
		t.Fatalf("reading not reused within 60s: %d polls", f.polls.Load())
	}
	now = now.Add(time.Second) // exactly 60s after the read started
	if err := g.admit(context.Background(), client, c, 1); err != nil || f.polls.Load() != 2 {
		t.Fatalf("reading reused at 60s: %v polls=%d", err, f.polls.Load())
	}
}

func TestGuardFailsClosedOnFailedOrStaleAnalytics(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, status := range []int32{403, 429, 500} {
		f := &analyticsFixture{}
		f.status.Store(status)
		_, client := f.serve(t)
		e := wantCode(t, guardAt(&now).admit(context.Background(), client, freeCredentials(t, nil), 1), "quota_unverified")
		if e.HTTPStatus != 503 || e.RetryAfterSeconds != 60 {
			t.Fatalf("unverified error %+v", e)
		}
	}
	for _, raw := range []string{`{`, `{"errors":[{"message":"x"}]}`, strings.ReplaceAll(analyticsBody(`[]`), testAccount, otherAccount)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
		err := guardAt(&now).admit(context.Background(), mockHostClient{server}, freeCredentials(t, nil), 1)
		server.Close()
		wantCode(t, err, "quota_unverified")
	}
	if err := newFreeGuard().admit(context.Background(), nil, freeCredentials(t, nil), 1); err == nil {
		t.Fatal("admitted without a host HTTP client")
	}
	if err := (*freeGuard)(nil).admit(context.Background(), mockHostClient{}, freeCredentials(t, nil), 1); err == nil {
		t.Fatal("admitted without a guard")
	}

	// A failed refresh may fall back to a same-day reading up to 2 minutes old.
	f := &analyticsFixture{}
	_, client := f.serve(t)
	g, c := guardAt(&now), freeCredentials(t, nil)
	if err := g.admit(context.Background(), client, c, 1); err != nil {
		t.Fatal(err)
	}
	f.status.Store(500)
	now = now.Add(readingMaxAge - time.Second)
	if err := g.admit(context.Background(), client, c, 1); err != nil {
		t.Fatalf("recent reading rejected: %v", err)
	}
	now = now.Add(time.Second)
	wantCode(t, g.admit(context.Background(), client, c, 1), "quota_unverified")
	f.status.Store(0)
	if err := g.admit(context.Background(), client, c, 1); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
	for _, estimate := range []float64{math.Inf(1), math.NaN(), -1} {
		wantCode(t, g.admit(context.Background(), client, c, estimate), "quota_unverified")
	}
	// Never admissible: rejected as a client error, without a read or cooldown.
	polls := f.polls.Load()
	if e := wantCode(t, g.admit(context.Background(), client, c, 9500), "request_too_large"); e.HTTPStatus != 400 || f.polls.Load() != polls {
		t.Fatalf("oversized request %+v polls=%d", e, f.polls.Load())
	}
}

func TestGuardRollsOverAtUTCMidnight(t *testing.T) {
	now := time.Date(2030, 1, 1, 23, 59, 30, 0, time.UTC)
	f := &analyticsFixture{}
	f.reading.Store(9600)
	_, client := f.serve(t)
	g, c := guardAt(&now), freeCredentials(t, nil)
	if e := wantCode(t, g.admit(context.Background(), client, c, 1), "quota_exhausted"); e.RetryAfterSeconds != 30 {
		t.Fatalf("retry %d, want 30s to midnight", e.RetryAfterSeconds)
	}
	// 35s later is inside the 60s TTL but a new UTC day: yesterday's reading
	// must not be reused.
	now = now.Add(35 * time.Second)
	f.reading.Store(0)
	if err := g.admit(context.Background(), client, c, 1); err != nil || f.polls.Load() != 2 {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clock.Add(2)
		io.WriteString(w, analyticsBody(`[]`))
	}))
	defer server.Close()
	g = newFreeGuard()
	g.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	wantCode(t, g.admit(context.Background(), mockHostClient{server}, c, 1), "quota_unverified")
}

func TestGuardIsConcurrencySafeAndSingleFlight(t *testing.T) {
	f := &analyticsFixture{delay: 50 * time.Millisecond}
	_, client := f.serve(t)
	g, c := newFreeGuard(), freeCredentials(t, nil)
	var admitted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := g.admit(context.Background(), client, c, 100); {
			case err == nil:
				admitted.Add(1)
			default:
				var e *pluginabi.Error
				if errors.As(err, &e) && e.Code == "quota_exhausted" {
					refused.Add(1)
				} else {
					t.Errorf("unexpected %v", err)
				}
			}
		}()
	}
	wg.Wait()
	// The k-th admission needs (k-1)*100 + 100 + 500 < 10000: k <= 94.
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
	g, c := newFreeGuard(), freeCredentials(t, nil)
	go g.admit(context.Background(), mockHostClient{server}, c, 1)
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
	if err := g.admit(ctx, mockHostClient{server}, c, 1); !errors.Is(err, context.DeadlineExceeded) {
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
	ex := newExecutor(newFreeGuard())
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

func TestExecutorBoundsOutputAndBypassesWhenOptedOut(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var graphql atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/graphql" {
			graphql.Add(1)
			io.WriteString(w, analyticsBody(`[]`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer server.Close()
	ex := newExecutor(newFreeGuard())
	run := func(payload string, metadata map[string]any) map[string]any {
		t.Helper()
		mu.Lock()
		bodies = nil
		mu.Unlock()
		_, err := ex.Execute(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(payload), StorageJSON: authJSON(testAccount, "example-key"), AuthMetadata: metadata, HTTPClient: mockHostClient{server}})
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) != 1 {
			t.Fatalf("%d upstream requests", len(bodies))
		}
		return bodies[0]
	}
	const model = `"model":"cloudflare/@cf/openai/gpt-oss-20b"`
	for _, tc := range []struct {
		payload string
		want    any
	}{
		{`{` + model + `}`, float64(defaultMaxOutputTokens)},
		{`{` + model + `,"max_tokens":"many"}`, float64(defaultMaxOutputTokens)},
		{`{` + model + `,"max_completion_tokens":77}`, float64(77)},
		{`{` + model + `,"max_tokens":10,"max_completion_tokens":77}`, float64(10)},
	} {
		if got := run(tc.payload, nil)["max_tokens"]; got != tc.want {
			t.Fatalf("%s: max_tokens %v, want %v", tc.payload, got, tc.want)
		}
	}
	if graphql.Load() != 1 {
		t.Fatalf("guarded reads %d, want one cached read", graphql.Load())
	}
	// Opted out: today's behaviour, no analytics read and no output cap.
	body := run(`{`+model+`}`, map[string]any{"stay_within_free": false})
	if _, set := body["max_tokens"]; set || graphql.Load() != 1 {
		t.Fatalf("opt-out still guarded: %v reads=%d", body, graphql.Load())
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/graphql" {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer failing.Close()
	if _, err := newExecutor(newFreeGuard()).Execute(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{` + model + `}`), StorageJSON: authJSON(testAccount, "example-key"), AuthMetadata: map[string]any{"stay_within_free": false}, HTTPClient: mockHostClient{failing}}); err != nil {
		t.Fatalf("opt-out depends on analytics: %v", err)
	}
}

func TestProbesRefusedByTheGuardAreNotSent(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(withAnalytics(9999, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/org/a","task":{"name":"Text Generation"}},{"name":"@cf/org/b","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
			return
		}
		probes.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})))
	defer server.Close()
	out, err := newCatalog(newFreeGuard()).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 2 || probes.Load() != 0 {
		t.Fatalf("models=%d probes=%d err=%v", len(out.Models), probes.Load(), err)
	}
}

func TestEstimateBoundsPromptAndOutput(t *testing.T) {
	body := []byte(`{"model":"@cf/openai/gpt-oss-120b","max_tokens":1000}`)
	want := math.Ceil((float64(len(body))*31818 + 1000*68182) / 1e6)
	if got := estimateNeurons(body); got != want {
		t.Fatalf("known model %v, want %v", got, want)
	}
	unknown := []byte(`{"model":"@cf/new/model","max_completion_tokens":1000}`)
	if worstNeuronRate.input != 127273 || worstNeuronRate.output != 606061 {
		t.Fatalf("worst rate %+v", worstNeuronRate)
	}
	if got := estimateNeurons(unknown); got != math.Ceil((float64(len(unknown))*127273+1000*606061)/1e6) {
		t.Fatalf("unknown model %v", got)
	}
	for _, raw := range []string{`{"model":"@cf/openai/gpt-oss-120b"}`, `{"model":"@cf/openai/gpt-oss-120b","max_tokens":0}`, `{"model":"@cf/openai/gpt-oss-120b","max_tokens":-5}`, `[`} {
		if !math.IsInf(estimateNeurons([]byte(raw)), 1) {
			t.Fatalf("unbounded %s was estimated", raw)
		}
	}
	probe := probeBody("@cf/new/model")
	if got := estimateNeurons(probe); got != math.Ceil((float64(len(probe))*127273+606061)/1e6) || got > 20 {
		t.Fatalf("probe estimate %v", got)
	}
}
