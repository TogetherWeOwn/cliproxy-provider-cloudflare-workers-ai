// Analytics query/parser adapted from TogetherWeOwn/cpa-plugin-subscription-pool
// at ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b,
// internal/providers/cloudflare/quota.go (MIT). Only read-only metering is ported.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const maxAnalyticsResponse = 1 << 20

// Aggregate account-wide: no model/key dimensions, no truncated groups.
// Dataset/field evidence and the unverified live schema boundary are documented
// in docs/SOURCES.md. Schema/permission/partial-data errors never imply zero.
// Deviation from the ported query: Cloudflare's schema types accountTag with
// its lowercase custom scalar "string" (official examples declare
// "$accountTag: string"), so "String!" would fail variable validation.
const neuronQuery = `query WorkersAINeurons($account: string!, $date: Date!) {
 viewer { accounts(filter: {accountTag: $account}) {
  accountTag
  aiInferenceAdaptiveGroups(limit: 1, filter: {date_geq: $date, date_leq: $date}) {
   sum { totalNeurons }
  }
 } }
}`

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func newAnalyticsClient() *http.Client {
	return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func analyticsError(code, message string) *pluginabi.Error {
	return &pluginabi.Error{Code: code, Message: message, HTTPStatus: 502}
}

// GraphQL reads use POST. Kit v0.1.0's quota HTTP helper only allows GET, so
// this bounded no-redirect helper is provider-local; inference uses host HTTP.
func fetchAnalytics(ctx context.Context, client httpDoer, endpoint string, c credentials, now time.Time) (float64, error) {
	if client == nil || endpoint == "" {
		return 0, analyticsError("analytics_unavailable", "analytics polling is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"query": neuronQuery, "variables": map[string]string{"account": c.account, "date": utcDay(now).Format("2006-01-02")}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, analyticsError("analytics_request_failed", "invalid analytics request")
	}
	req.Header.Set("Authorization", "Bearer "+c.analyticsToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, analyticsError("analytics_transport_failed", "analytics request failed")
	}
	if resp == nil || resp.Body == nil {
		return 0, analyticsError("analytics_empty_response", "analytics returned no response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Analytics authorization is independent of inference authorization.
		// A missing Analytics Read permission must not disable the inference key.
		e := analyticsError("analytics_http_error", "analytics endpoint returned an unsuccessful status; check analytics permissions")
		e.Retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
		return 0, e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnalyticsResponse+1))
	if err != nil || len(body) > maxAnalyticsResponse {
		return 0, analyticsError("analytics_response_unreadable", "analytics response unreadable or exceeds 1 MiB")
	}
	return parseAnalytics(body, c.account)
}
func parseAnalytics(raw []byte, expectedAccount string) (float64, error) {
	var envelope struct {
		Errors []json.RawMessage `json:"errors"`
		Data   *struct {
			Viewer *struct {
				Accounts []struct {
					AccountTag string `json:"accountTag"`
					Groups     *[]struct {
						Sum *struct {
							Neurons *float64 `json:"totalNeurons"`
						} `json:"sum"`
					} `json:"aiInferenceAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
	}
	bad := func() (float64, error) {
		return 0, analyticsError("analytics_invalid_data", "analytics has no complete account-wide neuron reading")
	}
	if len(raw) > maxAnalyticsResponse || json.Unmarshal(raw, &envelope) != nil || len(envelope.Errors) > 0 {
		return bad()
	}
	if envelope.Data == nil || envelope.Data.Viewer == nil || len(envelope.Data.Viewer.Accounts) != 1 {
		return bad()
	}
	account := envelope.Data.Viewer.Accounts[0]
	if account.AccountTag != expectedAccount || account.Groups == nil {
		return bad()
	}
	groups := *account.Groups
	if len(groups) == 0 {
		return 0, nil
	}
	if len(groups) != 1 || groups[0].Sum == nil || groups[0].Sum.Neurons == nil {
		return bad()
	}
	neurons := *groups[0].Sum.Neurons
	if math.IsNaN(neurons) || math.IsInf(neurons, 0) || neurons < 0 {
		return bad()
	}
	return neurons, nil
}
