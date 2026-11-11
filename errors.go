package main

import (
	"bytes"
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
			// The daily neuron allocation is accountTag-wide (quota.go:
			// "Aggregate account-wide"), so the whole credential rests until
			// the UTC reset instead of cool-only-the-reporting-model.
			e.CredentialScoped = true
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

// streamQuotaError surfaces an account-wide quota refusal embedded in a 200
// upstream SSE event. Workers AI can signal daily neuron exhaustion inside a
// streaming response, where the HTTP-status classification above never runs.
// Only the credential-scoped 3036/daily-neuron shape is intercepted: the event
// is classified through the same 429 rules so the stream chunk error keeps the
// scoped flag, and every other event shape returns nil and passes through.
func streamQuotaError(payload []byte, now time.Time) *pluginabi.Error {
	data := streamDataValue(payload)
	if data == nil {
		return nil
	}
	e := classifyFailure(pluginapi.HTTPResponse{StatusCode: http.StatusTooManyRequests, Body: data}, now)
	if !e.CredentialScoped {
		return nil
	}
	return e
}

// streamDataValue extracts the JSON data value from one framed stream chunk:
// bare JSON for OpenAI-output framing, a data:-prefixed event for translator
// framing, or a full native SSE event whose data lines are joined. Returns nil
// when the chunk carries no data payload.
func streamDataValue(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return trimmed
	}
	var event []byte
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		v, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		v = bytes.TrimPrefix(v, []byte(" ")) // SSE permits one leading space
		event = append(event, bytes.TrimRight(v, "\r")...)
		event = append(event, '\n')
	}
	if len(event) == 0 {
		return nil
	}
	event = event[:len(event)-1]
	var compact bytes.Buffer
	if json.Compact(&compact, event) == nil {
		return compact.Bytes()
	}
	return event
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
