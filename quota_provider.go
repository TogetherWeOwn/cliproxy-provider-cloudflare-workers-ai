package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaProvider struct {
	endpoint string
	client   httpDoer
	now      func() time.Time
	mu       sync.Mutex
	day      time.Time
	cache    *kit.CachedQuota
}

func newQuotaProvider(endpoint string, client httpDoer) *quotaProvider {
	return &quotaProvider{endpoint: endpoint, client: client, now: time.Now}
}
func (q *quotaProvider) Identifier() string { return provider }
func (q *quotaProvider) current() *kit.CachedQuota {
	q.mu.Lock()
	defer q.mu.Unlock()
	day := utcDay(q.now())
	// Kit TTL is 60s, but a daily card must not survive midnight even when
	// fetched a few seconds before reset. Old in-flight reads fail below.
	if q.cache == nil || !q.day.Equal(day) {
		q.day = day
		q.cache = kit.NewCachedQuota(provider, "Cloudflare Workers AI", q.fetch)
	}
	return q.cache
}
func (q *quotaProvider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	return q.current().FetchQuota(ctx, req)
}
func (q *quotaProvider) DescribeQuota(ctx context.Context, req pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return q.current().DescribeQuota(ctx, req)
}
func (q *quotaProvider) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return q.current().ResetQuota(ctx, req)
}
func (q *quotaProvider) fetch(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	c, err := credentialsFrom(req.StorageJSON, req.Metadata)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	now := q.now()
	endpoint := q.endpoint
	if endpoint == "" {
		endpoint = selectedBase(upstreamBase) + "/client/v4/graphql"
	}
	used, err := fetchAnalytics(ctx, q.client, endpoint, c, now)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	if !utcDay(q.now()).Equal(utcDay(now)) {
		return pluginapi.QuotaFetchResponse{}, analyticsError("analytics_day_changed", "UTC day changed during analytics read; retry")
	}
	return normalizeQuota(used, c.capacity, now)
}
func normalizeQuota(used, capacity float64, now time.Time) (pluginapi.QuotaFetchResponse, error) {
	if used < 0 || capacity <= 0 || math.IsNaN(used) || math.IsInf(used, 0) || math.IsNaN(capacity) || math.IsInf(capacity, 0) {
		return pluginapi.QuotaFetchResponse{}, analyticsError("analytics_invalid_data", "invalid neuron usage or daily capacity")
	}
	return pluginapi.QuotaFetchResponse{
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Daily free neurons", Buckets: []pluginapi.QuotaBucket{{
			Window: "daily", RemainingFraction: math.Max(0, 1-used/capacity), ResetTime: nextReset(now).Format(time.RFC3339),
			Description: fmt.Sprintf("Account-wide analytics: %.6g of %.6g free neurons used today (UTC); delayed/sampled, display-only", used, capacity),
		}}}},
		Summary: []pluginapi.QuotaMetric{
			{Key: "daily_free_neurons", Label: "Daily free allocation", Value: capacity, Unit: "neurons", Format: "number"},
			{Key: "neurons_used_today", Label: "Neurons used today", Value: used, Unit: "neurons", Format: "number"},
			{Key: "neurons_above_free", Label: "Neurons above free allocation (not an invoice)", Value: math.Max(0, used-capacity), Unit: "neurons", Format: "number"},
		},
	}, nil
}

var _ pluginapi.QuotaProvider = (*quotaProvider)(nil)
