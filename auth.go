package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider            = "cloudflare-workers-ai"
	version             = "0.1.2"
	modelPrefix         = "cloudflare/"
	upstreamBase        = "https://api.cloudflare.com"
	defaultDailyNeurons = 10000.0
	defaultSafetyMargin = 500.0
)

var accountPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type credentials struct {
	account, token, analyticsToken string
	capacity                       float64
	// The free-only hard stop is on unless an auth explicitly opts out.
	stayWithinFree bool
	safetyMargin   float64
	// coldStartHold: after the plugin instance starts, refuse (retryably) for
	// this long, because a new instance cannot see what an earlier one
	// admitted while analytics still lag. Default one tally window.
	coldStartHold time.Duration
	// proxyURL is the auth's own proxy_url (metadata over storage), empty when
	// unset; the guard's analytics read falls back to the host's proxy-url.
	proxyURL string
}

func invalidAuth() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_auth", Message: "expected an account_id, API token, optional positive daily_free_neurons and safety_margin_neurons, optional non-negative cold_start_hold_seconds, and optional boolean stay_within_free", HTTPStatus: 400}
}

// Validate at every boundary, including host-metadata overlays after persistence.
func credentialsFrom(storage []byte, metadata map[string]any) (credentials, error) {
	fields, err := kit.AuthFields(storage, metadata)
	if err != nil {
		return credentials{}, err
	}
	c, err := credentialsFields(fields)
	c.proxyURL = kit.AuthProxyURL(storage, metadata, "")
	return c, err
}
func credentialsFields(fields map[string]any) (credentials, error) {
	var c credentials
	c.account, _ = fields["account_id"].(string)
	c.account = strings.ToLower(strings.TrimSpace(c.account))
	c.token, _ = fields["api_key"].(string)
	c.token = strings.TrimSpace(c.token)
	if !accountPattern.MatchString(c.account) || !validToken(c.token) {
		return c, invalidAuth()
	}
	c.analyticsToken = c.token
	if value, exists := fields["analytics_api_key"]; exists {
		token, ok := value.(string)
		if !ok || !validToken(strings.TrimSpace(token)) {
			return c, invalidAuth()
		}
		c.analyticsToken = strings.TrimSpace(token)
	}
	var ok bool
	if c.capacity, ok = positiveField(fields, "daily_free_neurons", defaultDailyNeurons); !ok {
		return c, invalidAuth()
	}
	if c.safetyMargin, ok = positiveField(fields, "safety_margin_neurons", defaultSafetyMargin); !ok {
		return c, invalidAuth()
	}
	hold, ok := nonNegativeField(fields, "cold_start_hold_seconds", tallyWindow.Seconds())
	if !ok {
		return c, invalidAuth()
	}
	c.coldStartHold = time.Duration(hold * float64(time.Second))
	c.stayWithinFree = true
	if value, exists := fields["stay_within_free"]; exists {
		if c.stayWithinFree, ok = value.(bool); !ok {
			return c, invalidAuth()
		}
	}
	return c, nil
}
func positiveField(fields map[string]any, name string, fallback float64) (float64, bool) {
	n, ok := numberField(fields, name, fallback)
	return n, ok && n > 0
}
func nonNegativeField(fields map[string]any, name string, fallback float64) (float64, bool) {
	n, ok := numberField(fields, name, fallback)
	return n, ok && n >= 0
}
func numberField(fields map[string]any, name string, fallback float64) (float64, bool) {
	value, exists := fields[name]
	if !exists {
		return fallback, true
	}
	var n float64
	switch v := value.(type) {
	case json.Number:
		var err error
		if n, err = v.Float64(); err != nil {
			return 0, false
		}
	case float64:
		n = v
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}
func validToken(token string) bool { return token != "" && !strings.ContainsAny(token, "\r\n\x00") }

// A watcher can reparse one filename. Retain only identity digests for duplicate
// checks; deleting/renaming an auth file requires a plugin lifecycle reset.
type authProvider struct {
	kit.APIKeyAuth
	guard  *freeGuard
	mu     sync.Mutex
	files  map[string][32]byte
	owners map[[32]byte]string
}

// guard learns the host's global proxy-url from each parse/refresh request.
func newAuthProvider(guard *freeGuard) *authProvider {
	return &authProvider{guard: guard, APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(fields map[string]any) error {
		if _, ok := fields["id"]; ok {
			return invalidAuth()
		}
		c, err := credentialsFields(fields)
		if err == nil {
			fields["account_id"] = c.account
			fields["api_key"] = c.token
		}
		return err
	}}, files: make(map[string][32]byte), owners: make(map[[32]byte]string)}
}
func (p *authProvider) ParseAuth(ctx context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	p.guard.observeHost(req.Host)
	out, err := p.APIKeyAuth.ParseAuth(ctx, req)
	if err != nil || !out.Handled {
		return out, err
	}
	c, err := credentialsFrom(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	p.guard.observeAuth(c)
	digest := sha256.Sum256([]byte(c.account + "\x00" + c.token))
	out.Auth.ID = provider + ":" + hex.EncodeToString(digest[:6])
	p.mu.Lock()
	defer p.mu.Unlock()
	if file, ok := p.owners[digest]; ok && file != req.FileName {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "duplicate_auth", Message: "account and token already configured in another auth file", HTTPStatus: 400}
	}
	if old, ok := p.files[req.FileName]; ok && old != digest {
		delete(p.owners, old)
	}
	p.files[req.FileName], p.owners[digest] = digest, req.FileName
	return out, nil
}
func (p *authProvider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	p.guard.observeHost(req.Host)
	c, err := credentialsFrom(req.StorageJSON, req.Metadata)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	p.guard.observeAuth(c)
	return p.APIKeyAuth.RefreshAuth(ctx, req)
}
