package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// chatRecorder is a loopback account: analytics read 0 neurons and every
// chat body is recorded byte for byte.
type chatRecorder struct {
	server *httptest.Server
	polls  atomic.Int32
	mu     sync.Mutex
	bodies [][]byte
}

func newChatRecorder(t *testing.T) *chatRecorder {
	rec := &chatRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/v4/graphql" {
			rec.polls.Add(1)
			io.WriteString(w, analyticsBody(`[]`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, raw)
		rec.mu.Unlock()
		io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}
func (rec *chatRecorder) sent() [][]byte {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([][]byte(nil), rec.bodies...)
}
func (rec *chatRecorder) request(payload string, metadata map[string]any) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Payload: []byte(payload), StorageJSON: authJSON(testAccount, "example-key"), AuthMetadata: metadata, HTTPClient: mockHostClient{rec.server}}
}

var optOut = map[string]any{"stay_within_free": false}

// The upstream body is rebuilt from the decoded request, and the tally is
// charged for exactly the bytes and max_tokens that went upstream.
func TestGuardedBodyIsRebuiltAndChargedExactly(t *testing.T) {
	const model = `"model":"cloudflare/@cf/openai/gpt-oss-20b"`
	rate := neuronRates["@cf/openai/gpt-oss-20b"]
	for _, tc := range []struct {
		fields    string
		maxTokens float64
	}{
		{``, defaultMaxOutputTokens},
		{`,"max_tokens":null,"max_completion_tokens":null`, defaultMaxOutputTokens},
		{`,"max_tokens":12.0`, 12},
		{`,"max_completion_tokens":77`, 77},
		{`,"max_tokens":10,"max_completion_tokens":77`, 10},
		{`,"max_tokens":90,"max_completion_tokens":20`, 20},
		// An escaped key decodes to the exact name; the rebuilt body carries
		// the decoded value under the plain key, so both sides read 5.
		{`,"max\u005ftokens":5`, 5},
		{`,"n":1,"best_of":1,"temperature":0.2,"top_p":0.9,"top_k":40,"seed":7,"stop":["\n"],"frequency_penalty":0.1,"presence_penalty":0.1,"repetition_penalty":1.1,"stream_options":{"include_usage":true},` +
			`"messages":[{"role":"system","content":"be brief"},{"role":"user","name":"u","content":[{"type":"text","text":"hi"},{"text":"there","type":"text"}]},{"role":"assistant","content":null},{"role":"user","content":"ok"}]`, defaultMaxOutputTokens},
	} {
		rec := newChatRecorder(t)
		g := testGuard(rec.server)
		ex := newExecutor(g)
		payload := `{` + model + tc.fields + `}`
		if _, err := ex.Execute(context.Background(), rec.request(payload, nil)); err != nil {
			t.Fatalf("%s: %v", payload, err)
		}
		if _, err := ex.ExecuteStream(context.Background(), rec.request(payload, nil)); err != nil {
			t.Fatalf("%s: stream %v", payload, err)
		}
		if _, err := ex.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Body: []byte(payload), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{rec.server}}); err != nil {
			t.Fatalf("%s: http %v", payload, err)
		}
		sent := rec.sent()
		g.mu.Lock()
		charged := append([]charge(nil), g.accounts[testAccount].admitted...)
		g.mu.Unlock()
		if len(sent) != 3 || len(charged) != 3 {
			t.Fatalf("%s: %d sent, %d charged", payload, len(sent), len(charged))
		}
		for i, raw := range sent {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			for _, dropped := range []string{"max_completion_tokens", "n", "best_of"} {
				if _, ok := body[dropped]; ok {
					t.Fatalf("%s: %s forwarded", payload, dropped)
				}
			}
			if got := string(body["max_tokens"]); got != string(mustJSON(int64(tc.maxTokens))) {
				t.Fatalf("%s: max_tokens %s, want %v", payload, got, tc.maxTokens)
			}
			if want := estimateNeurons(rate, raw, tc.maxTokens); charged[i].neurons != want {
				t.Fatalf("%s: charged %v for the body sent, want %v", payload, charged[i].neurons, want)
			}
			if wantStream := []string{"false", "true", "false"}[i]; string(body["stream"]) != wantStream {
				t.Fatalf("%s: path %d stream %s", payload, i, body["stream"])
			}
		}
		// The body sent is the one upstreamRequest built and estimated.
		stream := false
		built, _, estimate, err := upstreamRequest(rec.request(payload, nil), &stream)
		if err != nil || !bytes.Equal(built.Body, sent[0]) || estimate != charged[0].neurons {
			t.Fatalf("%s: built %s estimate %v err %v\nsent %s", payload, built.Body, estimate, err, sent[0])
		}
	}
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

// Chat templates render structured fields (tools, response_format schemas,
// tool calls) into far more tokens than their bytes, and nested scalars could
// be re-read differently upstream, so the hard stop refuses them outright.
func TestGuardRefusesStructuredFieldsTheEstimateCannotBound(t *testing.T) {
	deep := strings.Repeat("[", 800) + "1" + strings.Repeat("]", 800)
	for _, fields := range []string{
		`,"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":` + deep + `}}]`,
		`,"tool_choice":"auto"`,
		`,"response_format":{"type":"json_object"}`,
		`,"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]`,
		`,"messages":[{"role":"tool","tool_call_id":"c","content":"ok"}]`,
		`,"temperature":"0.2"`,
		`,"temperature":[0.2]`,
		`,"stop":[["\n"]]`,
		`,"stop":["a","b","c","d","e"]`,
		`,"stream_options":{"include_usage":"yes"}`,
		`,"stream_options":{"include_Usage":true}`,
	} {
		rec := newChatRecorder(t)
		ex := newExecutor(testGuard(rec.server))
		payload := `{"model":"cloudflare/@cf/meta/llama-3.3-70b-instruct-fp8-fast"` + fields + `}`
		if e := wantCode(t, execErr(ex, rec.request(payload, nil)), "invalid_request"); e.HTTPStatus != http.StatusBadRequest || e.Retryable {
			t.Fatalf("%s: refusal %+v", payload, e)
		}
		if n := len(rec.sent()); n != 0 {
			t.Fatalf("%s: %d requests sent upstream", payload, n)
		}
	}
}

func TestGuardedBodyKeepsTextPartsAndExactKeys(t *testing.T) {
	stream := false
	req, _, _, err := upstreamRequest(pluginapi.ExecutorRequest{StorageJSON: authJSON(testAccount, "example-key"), Payload: []byte(`{"model":"cloudflare/@cf/openai/gpt-oss-20b","messages":[{"role":"user","content":[{"type":"text","text":"aA"}]}]}`)}, &stream)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"max_tokens":4096,"messages":[{"content":[{"type":"text","text":"aA"}],"role":"user"}],"model":"@cf/openai/gpt-oss-20b","stream":false}`
	if string(req.Body) != want {
		t.Fatalf("rebuilt %s\nwant    %s", req.Body, want)
	}
}

// Every shape below is one Cloudflare could read differently from the guard,
// or one the estimate cannot bound. Each is refused before any analytics read
// or dispatch, and forwarded unchanged when the auth opts out.
func TestGuardedRequestRefusesShapesTheGuardCouldMisread(t *testing.T) {
	rec := newChatRecorder(t)
	ex := newExecutor(testGuard(rec.server))
	const model = `"model":"cloudflare/@cf/meta/llama-3.2-11b-vision-instruct"`
	msg := func(m string) string { return `"messages":[` + m + `]` }
	part := func(p string) string { return msg(`{"role":"user","content":[` + p + `]}`) }
	refused := []string{
		// Unicode case folding: Kelvin sign U+212A folds to k, long s U+017F to s.
		"\"max_to\u212aens\":100000", `"max_to\u212aens":100000`, "\"max_tokens\":1,\"max_to\u212aens\":100000", `"MAX_TO\u212aENS":100000`,
		"\"max_token\u017f\":100000", `"max_completion_token\u017f":100000`, "\"\u017ftream\":true", `"max_token\u017f":1,"max_to\u212aen\u017f":100000`,
		// ASCII case variants of known keys.
		`"MAX_TOKENS":100000`, `"Max_Tokens":100000`, `"max_tokens":1,"Max_tokens":100000`, `"Model":"cloudflare/@cf/new/model"`, `"Messages":[]`,
		// Duplicate keys: one decoder takes the first, another the last.
		`"max_tokens":1,"max_tokens":100000`, `"max_completion_tokens":1,"max_completion_tokens":100000`,
		`"messages":[],"messages":[{"role":"user","content":"hi"}]`, `"model":"cloudflare/@cf/openai/gpt-oss-20b"`,
		// Unknown top-level keys, including native Workers AI inputs.
		`"prompt":"hi"`, `"image":[1,2,3]`, `"audio":{"voice":"alloy"}`, `"raw":true`, `"lora":"x"`, `"logprobs":true`, `"":1`,
		// Several completions per request.
		`"n":2`, `"n":64`, `"n":"1"`, `"n":0`, `"n":null`, `"best_of":3`,
		// Limits the estimate cannot charge as sent.
		`"max_tokens":0.0001`, `"max_tokens":1.5`, `"max_tokens":0`, `"max_tokens":-5`, `"max_tokens":"many"`, `"max_tokens":"5"`, `"max_tokens":true`, `"max_tokens":1e300`,
		`"max_completion_tokens":0.5`, `"max_tokens":10,"max_completion_tokens":0.5`,
		// Message shape.
		`"messages":{"role":"user"}`, `"messages":null`, `"messages":"hi"`, msg(`"hi"`),
		msg(`{"role":"user","Content":"hi"}`), msg(`{"Role":"user","content":"hi"}`), msg(`{"role":"user","CONTENT":[{"type":"image_url","image_url":{"url":"x"}}]}`),
		msg(`{"role":"user","content":"a","content":[{"type":"image_url","image_url":{"url":"x"}}]}`), msg(`{"role":"user","role":"system","content":"a"}`),
		msg(`{"role":"user","content":"hi","image_url":"x"}`), msg("{\"role\":\"assistant\",\"content\":null,\"tool_call\u017f\":[]}"), msg(`{"role":"user","content":"a","Content":[{"type":"image_url"}]}`),
		msg(`{"content":"no role"}`), msg(`{"role":1,"content":"x"}`), msg(`{"role":"user","name":5,"content":"x"}`), msg(`{"role":"assistant","tool_calls":{"id":"c"}}`),
		msg(`{"role":"user","content":{"type":"text","text":"x"}}`), msg(`{"role":"user","content":5}`),
		// Content parts: only exactly {"type":"text","text":string}.
		part(`{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"https://example.invalid/cat.png"}}`),
		part(`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`),
		part(`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`),
		part(`{"type":"file","file":{"file_id":"f"}}`),
		part(`{"text":"no type"}`), part(`{"type":"text"}`), part(`{"Type":"text","text":"x"}`), part(`{"type":"text","Text":"x"}`),
		part(`{"TYPE":"image_url","type":"text","text":"x"}`), part(`{"type":"text","text":"x","image_url":{"url":"y"}}`),
		part(`{"type":"text","type":"image_url","text":"x"}`), part(`{"type":"image_url","type":"text","text":"x"}`), part(`{"type":"text","text":"a","text":"b"}`),
		part(`{"type":"text","text":5}`), part(`{"type":"Text","text":"x"}`), part(`"text"`), part(`null`),
	}
	for _, field := range refused {
		payload := `{` + model + `,` + field + `}`
		e := wantCode(t, execErr(ex, rec.request(payload, nil)), "invalid_request")
		if e.HTTPStatus != http.StatusBadRequest || e.Retryable {
			t.Fatalf("%s: refusal %+v", field, e)
		}
	}
	for _, payload := range []string{`{` + model + `} {}`, `{` + model + `}x`, `{` + model + `,}`, `[` + model + `]`, `null`, ``} {
		wantCode(t, execErr(ex, rec.request(payload, nil)), "invalid_request")
	}
	if n := len(rec.sent()); n != 0 || rec.polls.Load() != 0 {
		t.Fatalf("%d dispatched, %d analytics reads for refused requests", n, rec.polls.Load())
	}
	for _, field := range refused {
		payload := `{` + model + `,` + field + `}`
		if err := execErr(ex, rec.request(payload, optOut)); err != nil {
			t.Fatalf("%s: opt-out refused: %v", field, err)
		}
	}
	if n := len(rec.sent()); n != len(refused) {
		t.Fatalf("%d opted-out requests forwarded, want %d", n, len(refused))
	}
}

func execErr(ex executor, r pluginapi.ExecutorRequest) error {
	_, err := ex.Execute(context.Background(), r)
	return err
}

// A model without a published price cannot be bounded: refused while guarded,
// forwarded when the auth opts out.
func TestUnpricedModelIsRefusedWhileGuarded(t *testing.T) {
	rec := newChatRecorder(t)
	ex := newExecutor(testGuard(rec.server))
	payload := `{"model":"cloudflare/@cf/new/model","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	for _, err := range []error{execErr(ex, rec.request(payload, nil)), func() error {
		_, err := ex.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Body: []byte(payload), StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{rec.server}})
		return err
	}()} {
		if e := wantCode(t, err, "invalid_model"); e.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("unpriced refusal %+v", e)
		}
	}
	if len(rec.sent()) != 0 || rec.polls.Load() != 0 {
		t.Fatal("unpriced model reached analytics or upstream")
	}
	if err := execErr(ex, rec.request(payload, optOut)); err != nil || len(rec.sent()) != 1 {
		t.Fatalf("opt-out: %v", err)
	}
}

func TestUnpricedModelsAreNotAdvertisedWhileGuarded(t *testing.T) {
	var mu sync.Mutex
	probed := map[string]int{}
	server := httptest.NewServer(withAnalytics(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ai/models/search") {
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"name":"@cf/new/model","task":{"name":"Text Generation"}},{"name":"@cf/openai/gpt-oss-20b","task":{"name":"Text Generation"}}],"result_info":{"page":1,"total_pages":1}}`)
			return
		}
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		probed[body.Model]++
		mu.Unlock()
		io.WriteString(w, `{"object":"chat.completion","choices":[]}`)
	})))
	defer server.Close()
	cat := newCatalog(testGuard(server))
	out, err := cat.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), HTTPClient: mockHostClient{server}})
	if err != nil || ids(out.Models) != "cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("guarded catalog %s %v", ids(out.Models), err)
	}
	mu.Lock()
	if probed["@cf/new/model"] != 0 || probed["@cf/openai/gpt-oss-20b"] != 1 {
		t.Fatalf("probes %v", probed)
	}
	mu.Unlock()
	out, err = cat.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: authJSON(testAccount, "example-key"), Metadata: optOut, HTTPClient: mockHostClient{server}})
	if err != nil || ids(out.Models) != "cloudflare/@cf/new/model,cloudflare/@cf/openai/gpt-oss-20b" {
		t.Fatalf("opted-out catalog %s %v", ids(out.Models), err)
	}
	// Every static fallback model is priced, so the fallback is servable.
	for _, m := range fallbackModels() {
		if _, ok := neuronRates[strings.TrimPrefix(m.ID, modelPrefix)]; !ok {
			t.Fatalf("fallback %s has no price", m.ID)
		}
	}
}

// Forwarded numbers keep their exact literal: a seed above 2^53 must not be
// rounded through float64.
func TestGuardedBodyKeepsExactNumbers(t *testing.T) {
	stream := false
	req, _, _, err := upstreamRequest(pluginapi.ExecutorRequest{StorageJSON: authJSON(testAccount, "example-key"), Payload: []byte(`{"model":"cloudflare/@cf/openai/gpt-oss-20b","seed":9007199254740993,"temperature":2e-1,"stop":[ "a" ],"messages":[{"role":"user","content":"x"}]}`)}, &stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"seed":9007199254740993`, `"temperature":2e-1`, `"stop":["a"]`} {
		if !bytes.Contains(req.Body, []byte(want)) {
			t.Fatalf("rebuilt %s lacks %s", req.Body, want)
		}
	}
}
