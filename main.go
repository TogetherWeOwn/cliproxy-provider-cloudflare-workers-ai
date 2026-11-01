package main

import (
	"context"

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
	models := newCatalog()
	return kit.New(kit.Definition{
		Metadata:  pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-cloudflare-workers-ai"},
		Configure: configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider: newAuthProvider(), ModelRegistrar: models, ModelProvider: models, Executor: newExecutor(),
			ExecutorModelScope:   pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota,
		},
		OnShutdown: func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
