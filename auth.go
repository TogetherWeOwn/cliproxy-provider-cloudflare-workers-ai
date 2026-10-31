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

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider            = "cloudflare-workers-ai"
	version             = "0.1.0"
	modelPrefix         = "cloudflare/"
	upstreamBase        = "https://api.cloudflare.com"
	defaultDailyNeurons = 10000.0
)

var accountPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type credentials struct {
	account, token, analyticsToken string
	capacity                       float64
}

func invalidAuth() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_auth", Message: "expected an account_id, API token and optional positive daily_free_neurons", HTTPStatus: 400}
}

// Validate at every boundary, including host-metadata overlays after persistence.
func credentialsFrom(storage []byte, metadata map[string]any) (credentials, error) {
	fields, err := kit.AuthFields(storage, metadata)
	if err != nil {
		return credentials{}, err
	}
	return credentialsFields(fields)
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
	c.capacity = defaultDailyNeurons
	if value, exists := fields["daily_free_neurons"]; exists {
		switch n := value.(type) {
		case json.Number:
			c.capacity, _ = n.Float64()
		case float64:
			c.capacity = n
		default:
			return c, invalidAuth()
		}
		if c.capacity <= 0 || math.IsNaN(c.capacity) || math.IsInf(c.capacity, 0) {
			return c, invalidAuth()
		}
	}
	return c, nil
}
func validToken(token string) bool { return token != "" && !strings.ContainsAny(token, "\r\n\x00") }

// A watcher can reparse one filename. Retain only identity digests for duplicate
// checks; deleting/renaming an auth file requires a plugin lifecycle reset.
type authProvider struct {
	kit.APIKeyAuth
	mu     sync.Mutex
	files  map[string][32]byte
	owners map[[32]byte]string
}

func newAuthProvider() *authProvider {
	return &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(fields map[string]any) error {
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
	out, err := p.APIKeyAuth.ParseAuth(ctx, req)
	if err != nil || !out.Handled {
		return out, err
	}
	c, err := credentialsFrom(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
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
	if _, err := credentialsFrom(req.StorageJSON, req.Metadata); err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	return p.APIKeyAuth.RefreshAuth(ctx, req)
}
