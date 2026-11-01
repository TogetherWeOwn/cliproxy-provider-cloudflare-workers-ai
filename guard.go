package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

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
// and its assumptions are documented in the README.
const (
	readingTTL             = 60 * time.Second
	readingMaxAge          = 2 * time.Minute
	tallyWindow            = 10 * time.Minute
	defaultMaxOutputTokens = 4096
)

type freeGuard struct {
	endpoint string // tests only; empty selects the account API base
	now      func() time.Time
	mu       sync.Mutex
	accounts map[string]*ledger
}

// One ledger per account: tokens on one account share one allocation.
type ledger struct {
	day      time.Time // UTC day of used
	used     float64
	readAt   time.Time // start of the read that produced used
	pending  chan struct{}
	admitted []charge
}
type charge struct {
	at      time.Time
	neurons float64
}

func newFreeGuard() *freeGuard {
	return &freeGuard{now: time.Now, accounts: make(map[string]*ledger)}
}
func freeAllowanceError(now time.Time) *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_exhausted", Message: "request could exceed the Workers AI daily free neuron allocation; refused to avoid paid usage", HTTPStatus: http.StatusTooManyRequests, RetryAfterSeconds: int(math.Ceil(nextReset(now).Sub(now).Seconds()))}
}
func requestTooLargeError() *pluginabi.Error {
	return &pluginabi.Error{Code: "request_too_large", Message: "request estimate exceeds the Workers AI daily free neuron allocation; lower max_tokens or the prompt", HTTPStatus: http.StatusBadRequest}
}
func usageUnverifiedError() *pluginabi.Error {
	return &pluginabi.Error{Code: "quota_unverified", Message: "today's Workers AI neuron usage cannot be verified; refused to avoid paid usage", HTTPStatus: http.StatusServiceUnavailable, Retryable: true, RetryAfterSeconds: 60}
}

// admit charges estimate to the credential's account or refuses. It never
// fails open: a missing, failed, stale or cross-day reading refuses.
func (g *freeGuard) admit(ctx context.Context, client pluginapi.HostHTTPClient, c credentials, estimate float64) error {
	if !c.stayWithinFree {
		return nil
	}
	if g == nil || client == nil || !(estimate >= 0) || math.IsInf(estimate, 0) {
		return usageUnverifiedError()
	}
	// A request that cannot fit even at zero usage is the request's fault; a
	// 429 would cool the auth/model down for every smaller request until reset.
	if estimate+c.safetyMargin >= c.capacity {
		return requestTooLargeError()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.accounts[c.account]
	if l == nil {
		l = &ledger{}
		g.accounts[c.account] = l
	}
	if now := g.now(); l.usable(now, readingTTL) {
		return l.charge(c, estimate, now)
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
		used, err := g.read(ctx, client, c, start)
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
		return l.charge(c, estimate, now)
	}
	return usageUnverifiedError()
}

// read runs without the lock. Waiters share it, so one caller's cancellation
// must not fail theirs; fetchAnalytics bounds it to 15 seconds.
func (g *freeGuard) read(ctx context.Context, client pluginapi.HostHTTPClient, c credentials, now time.Time) (used float64, err error) {
	defer func() {
		if recover() != nil {
			used, err = 0, usageUnverifiedError()
		}
	}()
	endpoint := g.endpoint
	if endpoint == "" {
		endpoint = selectedBase(upstreamBase) + "/client/v4/graphql"
	}
	return fetchAnalytics(context.WithoutCancel(ctx), hostDoer{client}, endpoint, c, now)
}
func (l *ledger) usable(now time.Time, maxAge time.Duration) bool {
	return !l.readAt.IsZero() && l.day.Equal(utcDay(now)) && !l.readAt.After(now) && now.Sub(l.readAt) < maxAge
}
func (l *ledger) charge(c credentials, estimate float64, now time.Time) error {
	kept, tally := l.admitted[:0], 0.0
	for _, a := range l.admitted {
		// Entries from before midnight keep counting: conservative.
		if now.Sub(a.at) < tallyWindow {
			kept, tally = append(kept, a), tally+a.neurons
		}
	}
	l.admitted = kept
	if l.used+tally+estimate+c.safetyMargin >= c.capacity {
		return freeAllowanceError(now)
	}
	l.admitted = append(l.admitted, charge{at: now, neurons: estimate})
	return nil
}

// hostDoer sends the analytics read through the host HTTP client, so executor
// reads follow the same auth/global proxy policy as inference. The host strips
// credential headers on cross-origin redirects.
type hostDoer struct{ client pluginapi.HostHTTPClient }

func (h hostDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
	}
	resp, err := h.client.Do(req.Context(), pluginapi.HTTPRequest{Method: req.Method, URL: req.URL.String(), Headers: req.Header.Clone(), Body: body})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: resp.StatusCode, Header: resp.Headers, Body: io.NopCloser(bytes.NewReader(resp.Body))}, nil
}

type neuronRate struct{ input, output float64 }

// Neurons per million tokens from the official LLM pricing table, retrieved
// 2026-11-01: https://developers.cloudflare.com/workers-ai/platform/pricing/
// Cached input is charged as uncached. A model missing here is charged the
// highest input and output rate in the table.
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
var worstNeuronRate = func() (worst neuronRate) {
	for _, r := range neuronRates {
		worst.input, worst.output = math.Max(worst.input, r.input), math.Max(worst.output, r.output)
	}
	return worst
}()

// estimateNeurons bounds one upstream chat body. Every token spans at least
// one byte, so the body length bounds prompt tokens (JSON framing and images
// only overcount), and output is bounded by max_tokens/max_completion_tokens,
// whichever is larger. buildRequest always sets max_tokens in free-only mode;
// an unbounded body returns +Inf, which admit refuses.
func estimateNeurons(body []byte) float64 {
	var wire struct {
		Model               string          `json:"model"`
		MaxTokens           json.RawMessage `json:"max_tokens"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return math.Inf(1)
	}
	output := math.Max(positiveNumber(wire.MaxTokens), positiveNumber(wire.MaxCompletionTokens))
	if output == 0 {
		return math.Inf(1)
	}
	rate, ok := neuronRates[wire.Model]
	if !ok {
		rate = worstNeuronRate
	}
	return math.Ceil((float64(len(body))*rate.input + output*rate.output) / 1e6)
}
func positiveNumber(raw json.RawMessage) float64 {
	var n float64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || !(n > 0) || math.IsInf(n, 0) {
		return 0
	}
	return n
}
