package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The models/search "Text Generation" list is the public catalog, not what an
// account can call: on 2026-10-31 seven of 39 listed models failed a minimal
// chat request for one account (403 "account is not allowed to access", 403
// "Model Agreement: ... you must submit the prompt 'agree'", 400 "required
// properties ... 'model,state,questions'", 400 "AI model does not have config").
// ModelsForAuth validates each candidate with a one-token chat probe and only
// advertises models the account can actually use. The plugin never submits a
// license agreement on the operator's behalf.
const (
	probeConcurrency = 8
	probeTimeout     = 10 * time.Second
	discoveryBudget  = 30 * time.Second
	verdictTTL       = 6 * time.Hour
)

func (c catalog) validate(ctx context.Context, client pluginapi.HostHTTPClient, cred credentials, candidates []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	budget, cancel := context.WithTimeout(ctx, discoveryBudget)
	defer cancel()
	digest := sha256.Sum256([]byte(cred.account + "\x00" + cred.token))
	alive := make([]bool, len(candidates))
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, m := range candidates {
		id := strings.TrimPrefix(m.ID, modelPrefix)
		if v, ok := c.cache.get(digest, id); ok {
			alive[i] = v
			continue
		}
		alive[i] = true // unknown until proven otherwise
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-budget.Done():
				return
			}
			defer func() { <-sem }()
			if usable, definitive := probeModel(budget, client, cred, id); definitive {
				alive[i] = usable
				c.cache.put(digest, id, usable)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]pluginapi.ModelInfo, 0, len(candidates))
	for i, m := range candidates {
		if alive[i] {
			out = append(out, m)
		}
	}
	return out, nil
}

// probeModel sends a one-token streaming chat request and decides on the
// response status alone; the stream is cancelled as soon as headers arrive.
// 400/422 mean the model rejects a minimal chat request; 403/404/410 mean the
// account may not use it. 2xx proves it is usable. Anything else (429, 5xx,
// timeouts) is inconclusive and keeps the model.
func probeModel(ctx context.Context, client pluginapi.HostHTTPClient, cred credentials, id string) (usable, definitive bool) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": id, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": 1, "stream": true})
	resp, err := client.DoStream(pctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: accountBase(cred) + "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + cred.token}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"}}, Body: body})
	if err != nil {
		return true, false
	}
	cancel() // only the status was needed
	if resp.Chunks != nil {
		for range resp.Chunks {
		}
	}
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return false, true
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, true
	}
	return true, false
}

// verdictCache remembers definitive per-account model verdicts so hot reloads
// do not re-probe the catalog. Keys are SHA-256 digests; no credential is kept.
type verdictCache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[[32]byte]map[string]verdict
}
type verdict struct {
	usable  bool
	expires time.Time
}

func newVerdictCache() *verdictCache {
	return &verdictCache{now: time.Now, m: make(map[[32]byte]map[string]verdict)}
}
func (c *verdictCache) get(digest [32]byte, id string) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[digest][id]
	if !ok || !c.now().Before(v.expires) {
		return false, false
	}
	return v.usable, true
}
func (c *verdictCache) put(digest [32]byte, id string, usable bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[digest] == nil {
		c.m[digest] = make(map[string]verdict)
	}
	c.m[digest][id] = verdict{usable: usable, expires: c.now().Add(verdictTTL)}
}
