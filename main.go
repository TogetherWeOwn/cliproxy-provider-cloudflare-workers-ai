package main

import (
	"context"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	client := newAnalyticsClient()
	quota := newQuotaProvider("", client)
	// One guard per plugin instance: inference and catalog probes share the
	// per-account readings and admission tally. It reads analytics with the
	// plugin's own client, never the per-request host client.
	guard := newFreeGuard(client)
	guard.started = time.Now()
	models := newCatalog(guard)
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-cloudflare-workers-ai"},
		ModelNamespace: "cloudflare",
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider: newAuthProvider(guard), ModelRegistrar: models, ModelProvider: models, Executor: newExecutor(guard),
			ExecutorModelScope:   pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota,
		},
		OnShutdown: func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
