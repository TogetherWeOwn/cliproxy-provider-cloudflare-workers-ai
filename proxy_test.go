package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAnalyticsPollUsesAuthOrHostProxy(t *testing.T) {
	recorder := func() (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var seen []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Method+" "+r.URL.String())
			mu.Unlock()
			io.WriteString(w, analyticsBody(`[{"sum":{"totalNeurons":1}}]`))
		}))
		t.Cleanup(s.Close)
		return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	}
	authProxy, authSeen := recorder()
	hostProxy, hostSeen := recorder()
	client := newAnalyticsClient()
	defer client.CloseIdleConnections()
	q := newQuotaProvider("http://cloudflare.invalid/client/v4/graphql", client)
	host := pluginapi.HostConfigSummary{ProxyURL: hostProxy.URL}
	for _, r := range []pluginapi.QuotaFetchRequest{
		{AuthID: "a", StorageJSON: authJSON(testAccount, "key-one"), Metadata: map[string]any{"proxy_url": authProxy.URL}, Host: host},
		{AuthID: "b", StorageJSON: authJSON(testAccount, "key-two"), Host: host},
	} {
		if _, err := q.FetchQuota(context.Background(), r); err != nil {
			t.Fatalf("%s: %v", r.AuthID, err)
		}
	}
	want := "POST http://cloudflare.invalid/client/v4/graphql"
	if got := authSeen(); len(got) != 1 || got[0] != want {
		t.Fatalf("auth proxy saw %v", got)
	}
	if got := hostSeen(); len(got) != 1 || got[0] != want {
		t.Fatalf("host proxy saw %v", got)
	}
}

// HTTP/2 connections are reused by destination before the proxy is chosen, so
// a transport shared across auths must not speak HTTP/2.
func TestQuotaTransportDoesNotShareConnectionsAcrossProxies(t *testing.T) {
	transport, ok := newAnalyticsClient().Transport.(*http.Transport)
	if !ok || transport.Proxy == nil || transport.Protocols == nil || transport.Protocols.HTTP2() || !transport.Protocols.HTTP1() {
		t.Fatal("quota transport must be HTTP/1.1-only with a proxy function")
	}
}
