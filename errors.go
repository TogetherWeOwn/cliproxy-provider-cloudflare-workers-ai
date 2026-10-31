package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
func nextReset(t time.Time) time.Time { return utcDay(t).AddDate(0, 0, 1) }

// Official error table: https://developers.cloudflare.com/workers-ai/platform/errors/
// 3036 is daily neuron exhaustion; 3040 is transient capacity, not exhaustion.
// Response classification parallels the MIT subscription-pool cloudflare module
// at ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b; no scheduler/admission code is ported.
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	e := &pluginabi.Error{Code: "upstream_error", Message: "Workers AI request failed", HTTPStatus: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Code, e.Message = "invalid_credentials", "Workers AI rejected the credential"
	case http.StatusForbidden:
		e.Code, e.Message = "entitlement_required", "Workers AI denied access to the account or model"
	case http.StatusNotFound:
		e.Code, e.Message = "upstream_not_found", "Workers AI endpoint or model was not found"
	case http.StatusTooManyRequests:
		e.Retryable = true
		body := strings.ToLower(string(resp.Body))
		if hasErrorCode(resp.Body, 3036) || (strings.Contains(body, "daily") && strings.Contains(body, "neuron")) {
			e.Code, e.Message = "quota_exhausted", "Workers AI daily free neuron allocation exhausted"
			e.RetryAfterSeconds = int(math.Ceil(nextReset(now).Sub(now).Seconds()))
		} else {
			e.Code, e.Message, e.RetryAfterSeconds = "rate_limit", "Workers AI rate or capacity limit reached", 5
		}
		// Daily exhaustion cannot be retried before the UTC reset even if a
		// generic gateway Retry-After is shorter. A longer server hint is safe.
		if n := kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now); n > 0 && (e.Code != "quota_exhausted" || n > e.RetryAfterSeconds) {
			e.RetryAfterSeconds = n
		}
	default:
		if resp.StatusCode == 400 && hasErrorCode(resp.Body, 5007) {
			e.Code, e.Message = "upstream_not_found", "Workers AI model was not found"
		}
		e.Retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
		if e.Retryable {
			e.RetryAfterSeconds = kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now)
		}
	}
	return e
}
func hasErrorCode(raw []byte, wanted int) bool {
	var wire struct {
		Errors []struct {
			Code json.RawMessage `json:"code"`
		} `json:"errors"`
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return false
	}
	matches := func(code json.RawMessage) bool {
		if string(code) == strconv.Itoa(wanted) {
			return true
		}
		var s string
		return json.Unmarshal(code, &s) == nil && s == strconv.Itoa(wanted)
	}
	if matches(wire.Error.Code) {
		return true
	}
	for _, e := range wire.Errors {
		if matches(e.Code) {
			return true
		}
	}
	return false
}
