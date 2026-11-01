package main

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Free-only hard stop. On Workers Paid every neuron above the daily free
// allocation is billed, so with stay_within_free (the default) a request is
// dispatched only if
//
//	reading + tally + estimate + safety_margin < daily_free_neurons
//
// reading is today's (UTC) account-wide analytics total, re-read at most once
// per readingTTL and never trusted beyond readingMaxAge. Analytics lag, so the
// tally charges every request admitted in the last tallyWindow its estimate,
// even when a newer reading may already include it: double counting is safe,
// missing a request is not. A reading at most readingMaxAge old includes every
// request whose neurons reached analytics before it, so a request whose
// neurons appear within tallyWindow-readingMaxAge (8 minutes) of admission is
// always counted once at least. The tally is in-process; the overshoot bound
// and its assumptions are documented in the README. Only a refusal the
// reading itself forces (reading + estimate + safety_margin reaches the
// allocation) waits for the UTC reset; one the tally alone forces is a short,
// retryable rate limit that clears as admissions leave the window.
const (
	readingTTL             = 60 * time.Second
	readingMaxAge          = 2 * time.Minute
	tallyWindow            = 10 * time.Minute
	defaultMaxOutputTokens = 4096
	templateOverheadTokens = 512
)

type freeGuard struct {
	// client is the plugin's own analytics client (see read), shared with the
	// quota card.
	client httpDoer
	// endpoint overrides the analytics GraphQL URL; tests point it at
	// loopback so no test can reach the real API.
	endpoint  string
	hostProxy atomic.Pointer[string] // host proxy-url; nil until first seen
	now       func() time.Time
	mu        sync.Mutex
	accounts  map[string]*ledger
}

// One ledger per account: tokens on one account share one allocation, and the
// most conservative settings any of them carried apply to all of them.
type ledger struct {
	day      time.Time // UTC day of used
	used     float64
	readAt   time.Time // start of the read that produced used
	pending  chan struct{}
	admitted []charge
	capacity float64 // lowest daily_free_neurons seen for the account
	margin   float64 // highest safety_margin_neurons seen for the account
}
type charge struct {
	at      time.Time
	neurons float64
}

func newFreeGuard(client httpDoer) *freeGuard {
	return &freeGuard{client: client, now: time.Now, accounts: make(map[string]*ledger)}
}

// observeHost records the host's global proxy-url, which executor requests do
// not carry. Auth parse/refresh and model requests deliver it before an auth
// can serve inference.
func (g *freeGuard) observeHost(h pluginapi.HostConfigSummary) {
	if g != nil {
		proxy := strings.TrimSpace(h.ProxyURL)
		g.hostProxy.Store(&proxy)
	}
}
func freeAllowanceError(now time.Time) *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_exhausted", Message: "request could exceed the Workers AI daily free neuron allocation; refused to avoid paid usage", HTTPStatus: http.StatusTooManyRequests, RetryAfterSeconds: int(math.Ceil(nextReset(now).Sub(now).Seconds()))}
}

// tallyLimitError refuses a request that fits the measured usage but not the
// recent admissions still being charged. It clears as those entries expire, so
// it must not cool the auth/model down until midnight.
func tallyLimitError(retry time.Duration) *pluginabi.Error {
	seconds := int(math.Min(math.Max(math.Ceil(retry.Seconds()), 1), tallyWindow.Seconds()))
	return &pluginabi.Error{Code: "rate_limit", Message: "recent requests may still reach the Workers AI daily free neuron allocation; retry after they are counted", HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: seconds}
}
func requestTooLargeError() *pluginabi.Error {
	return &pluginabi.Error{Code: "request_too_large", Message: "request estimate exceeds the Workers AI daily free neuron allocation; lower max_tokens or the prompt", HTTPStatus: http.StatusBadRequest}
}
func usageUnverifiedError() *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_unverified", Message: "today's Workers AI neuron usage cannot be verified; refused to avoid paid usage", HTTPStatus: http.StatusServiceUnavailable, Retryable: true, RetryAfterSeconds: 60}
}

// admit charges estimate to the credential's account or refuses. It never
// fails open: a missing, failed, stale or cross-day reading refuses.
func (g *freeGuard) admit(ctx context.Context, c credentials, estimate float64) error {
	if !c.stayWithinFree {
		return nil
	}
	if g == nil || g.client == nil || !(estimate >= 0) || math.IsInf(estimate, 0) {
		return usageUnverifiedError()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.accounts[c.account]
	if l == nil {
		l = &ledger{capacity: c.capacity, margin: c.safetyMargin}
		g.accounts[c.account] = l
	}
	// Several auths on one account: the most permissive must not decide.
	l.capacity, l.margin = math.Min(l.capacity, c.capacity), math.Max(l.margin, c.safetyMargin)
	// A request that cannot fit even at zero usage is the request's fault; a
	// 429 would cool the auth/model down for every smaller request until reset.
	if estimate+l.margin >= l.capacity {
		return requestTooLargeError()
	}
	if now := g.now(); l.usable(now, readingTTL) {
		return l.charge(estimate, now)
	}
	if wait := l.pending; wait != nil {
		// Single flight per account: share the read already in progress.
		g.mu.Unlock()
		select {
		case <-wait:
			g.mu.Lock()
		case <-ctx.Done():
			g.mu.Lock()
			return ctx.Err()
		}
	} else {
		done := make(chan struct{})
		l.pending = done
		g.mu.Unlock()
		start := g.now()
		used, err := g.read(ctx, c, start)
		g.mu.Lock()
		// A read crossing UTC midnight belongs to neither day.
		if err == nil && utcDay(g.now()).Equal(utcDay(start)) && !start.Before(l.readAt) {
			l.day, l.used, l.readAt = utcDay(start), used, start
		}
		l.pending = nil
		close(done)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// After a failed read a same-day reading up to readingMaxAge old still
	// bounds usage; anything older fails closed.
	if now := g.now(); l.usable(now, readingMaxAge) {
		return l.charge(estimate, now)
	}
	return usageUnverifiedError()
}

// read runs without the lock. Waiters share it, so one caller's cancellation
// must not fail theirs; fetchAnalytics bounds it to 15 seconds.
//
// It uses the plugin's own client, not the executor's host HTTP client. The
// host client is bound to the inference request's context, so it would write
// this GraphQL request, analytics bearer token included, into that request's
// API log and mark it as an upstream attempt; the plugin cannot strip either
// from that context. The read still follows the proxy policy the host applies
// to inference: the auth's proxy_url, else the host's global proxy-url. Until
// the host's setting has been seen the read is refused rather than sent
// direct, and an invalid proxy fails the read; both fail closed.
func (g *freeGuard) read(ctx context.Context, c credentials, now time.Time) (used float64, err error) {
	defer func() {
		if recover() != nil {
			used, err = 0, usageUnverifiedError()
		}
	}()
	proxy := c.proxyURL
	if proxy == "" {
		host := g.hostProxy.Load()
		if host == nil {
			return 0, usageUnverifiedError()
		}
		proxy = *host
	}
	endpoint := g.endpoint
	if endpoint == "" {
		endpoint = selectedBase(upstreamBase) + "/client/v4/graphql"
	}
	return fetchAnalytics(kit.WithProxyURL(context.WithoutCancel(ctx), proxy), g.client, endpoint, c, now)
}
func (l *ledger) usable(now time.Time, maxAge time.Duration) bool {
	return !l.readAt.IsZero() && l.day.Equal(utcDay(now)) && !l.readAt.After(now) && now.Sub(l.readAt) < maxAge
}
func (l *ledger) charge(estimate float64, now time.Time) error {
	// Measured usage alone leaves no room: wait for the UTC reset.
	if l.used+estimate+l.margin >= l.capacity {
		return freeAllowanceError(now)
	}
	kept, tally, expires := l.admitted[:0], 0.0, now.Add(tallyWindow)
	for _, a := range l.admitted {
		// Entries from before midnight keep counting: conservative.
		if now.Sub(a.at) < tallyWindow {
			kept, tally = append(kept, a), tally+a.neurons
			if end := a.at.Add(tallyWindow); end.Before(expires) {
				expires = end
			}
		}
	}
	l.admitted = kept
	if l.used+tally+estimate+l.margin >= l.capacity {
		return tallyLimitError(expires.Sub(now))
	}
	l.admitted = append(l.admitted, charge{at: now, neurons: estimate})
	return nil
}

type neuronRate struct{ input, output float64 }

// Neurons per million tokens from the official LLM pricing table, retrieved
// 2026-11-01: https://developers.cloudflare.com/workers-ai/platform/pricing/
// Cached input is charged as uncached. While stay_within_free is on, a model
// missing here is neither advertised nor served: it cannot be bounded.
var neuronRates = map[string]neuronRate{
	"@cf/meta/llama-3.2-1b-instruct":               {2457, 18252},
	"@cf/meta/llama-3.2-3b-instruct":               {4625, 30475},
	"@cf/meta/llama-3.1-8b-instruct-fp8-fast":      {4119, 34868},
	"@cf/meta/llama-3.2-11b-vision-instruct":       {4410, 61493},
	"@cf/meta/llama-3.1-70b-instruct-fp8-fast":     {26668, 204805},
	"@cf/meta/llama-3.3-70b-instruct-fp8-fast":     {26668, 204805},
	"@cf/deepseek-ai/deepseek-r1-distill-qwen-32b": {45170, 443756},
	"@cf/deepseek-ai/deepseek-v4-flash-0731":       {40000, 120000},
	"@cf/deepseek-ai/deepseek-v4.1-flash-1017":     {27273, 109091},
	"@cf/deepseek-ai/deepseek-v4-pro-0813":         {120000, 360000},
	"@cf/mistral/mistral-7b-instruct-v0.1":         {10000, 17300},
	"@cf/mistralai/mistral-small-3.1-24b-instruct": {31876, 50488},
	"@cf/meta/llama-3.1-8b-instruct":               {25608, 75147},
	"@cf/meta/llama-3.1-8b-instruct-fp8":           {13778, 26128},
	"@cf/meta/llama-3.1-8b-instruct-awq":           {11161, 24215},
	"@cf/meta/llama-3-8b-instruct":                 {25608, 75147},
	"@cf/meta/llama-3-8b-instruct-awq":             {11161, 24215},
	"@cf/meta/llama-2-7b-chat-fp16":                {50505, 606061},
	"@cf/meta/llama-guard-3-8b":                    {44003, 2730},
	"@cf/meta/llama-4-scout-17b-16e-instruct":      {24545, 77273},
	"@cf/google/gemma-3-12b-it":                    {31371, 50560},
	"@cf/qwen/qwq-32b":                             {60000, 90909},
	"@cf/qwen/qwen2.5-coder-32b-instruct":          {60000, 90909},
	"@cf/qwen/qwen3-30b-a3b-fp8":                   {4625, 30475},
	"@cf/qwen/qwen3.8-27b":                         {40909, 290909},
	"@cf/openai/gpt-oss-120b":                      {31818, 68182},
	"@cf/openai/gpt-oss-20b":                       {18182, 27273},
	"@cf/aisingapore/gemma-sea-lion-v4-27b-it":     {31876, 50488},
	"@cf/ibm-granite/granite-4.0-h-micro":          {1542, 10158},
	"@cf/zai-org/glm-4.7-flash":                    {5500, 36400},
	"@cf/zai-org/glm-5.2":                          {127273, 400000},
	"@cf/zai-org/glm-5.3":                          {127273, 400000},
	"@cf/zai-org/glm-5.3-flash":                    {13636, 45455},
	"@cf/nvidia/nemotron-3-120b-a12b":              {45455, 136364},
	"@cf/moonshotai/kimi-k2.5":                     {54545, 272727},
	"@cf/moonshotai/kimi-k2.6":                     {86364, 363636},
	"@cf/moonshotai/kimi-k2.7-code":                {86364, 363636},
	"@cf/google/gemma-4-26b-a4b-it":                {9091, 27273},
	"@cf/daily-co/phonellm-mini-alpha-2":           {5455, 21818},
	"@cf/moondream/moondream3.1-9B-A2B":            {27273, 90909},
}

// estimateNeurons bounds one rebuilt upstream body (see guardedBody). Its
// content is text only, so every prompt token spans at least one body byte
// and the body length bounds prompt tokens (JSON framing only overcounts);
// templateOverheadTokens covers the model's chat template. Output is bounded
// by limit, the max_tokens written into body.
func estimateNeurons(rate neuronRate, body []byte, limit float64) float64 {
	return math.Ceil((float64(len(body)+templateOverheadTokens)*rate.input + limit*rate.output) / 1e6)
}
