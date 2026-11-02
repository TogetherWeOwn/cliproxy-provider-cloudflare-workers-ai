package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The management panel titles tiles with the plugin metadata name, so it must
// be human-readable. The provider id used for namespaces, auth and quota stays
// the raw id.
func TestPluginMetadataNameIsHumanReadable(t *testing.T) {
	if provider != "cloudflare-workers-ai" {
		t.Fatalf("provider id changed to %q", provider)
	}
	r, err := makePlugin(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown(context.Background())
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(r.Call(context.Background(), "plugin.register", []byte(`{"schema_version":10}`)), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	var result struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Metadata.Name != "Cloudflare Workers AI" {
		t.Fatalf("metadata name = %q, want %q", result.Metadata.Name, "Cloudflare Workers AI")
	}
}
