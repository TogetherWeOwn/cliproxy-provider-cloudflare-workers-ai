package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// With stay_within_free on, the upstream body is never the client's bytes.
// The client body is decoded once, with exact case-sensitive keys, into an
// allowlisted text-only shape, and the upstream body is re-marshalled from
// those decoded values. The estimate is computed from the rebuilt bytes and
// the max_tokens written into them, so the guard and Cloudflare read the same
// request: there is no second reading of the client body for a decoder with
// different rules (Go's case-insensitive field matching and Unicode case
// folding, duplicate keys) to disagree with.

// Top-level fields accepted while the hard stop is on. Every forwarded value is
// re-encoded from a decoded number, string or bool, so its upstream bytes are
// exactly the bytes charged. Structured fields (tools, tool_choice,
// response_format, tool_calls) are refused: chat templates render them into far
// more tokens than their bytes, so no body-length estimate bounds them. n and
// best_of are accepted only as 1 and are not forwarded.
var guardedFields = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "top_k": true, "seed": true, "stop": true,
	"stream": true, "stream_options": true, "frequency_penalty": true, "presence_penalty": true,
	"repetition_penalty": true, "n": true, "best_of": true,
}
var numberFields = []string{"temperature", "top_p", "top_k", "seed", "frequency_penalty", "presence_penalty", "repetition_penalty"}
var messageFields = map[string]bool{"role": true, "content": true, "name": true}
var streamOptionFields = map[string]bool{"include_usage": true}

// maxStopSequences bounds the stop list; each entry is a short string.
const maxStopSequences = 4

var textPartFields = map[string]bool{"type": true, "text": true}

func guardedRequestError(message string) *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_request", Message: message + " while stay_within_free is on", HTTPStatus: http.StatusBadRequest}
}

// strictObject decodes one JSON object whose keys must be in allowed, each at
// most once, compared exactly as decoded. Trailing data is refused.
func strictObject(raw []byte, allowed map[string]bool, what string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, guardedRequestError(what + " must be a JSON object")
	}
	out := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, guardedRequestError(what + " is not valid JSON")
		}
		if !allowed[key] {
			if len(key) > 64 {
				key = key[:64]
			}
			return nil, guardedRequestError(what + " field " + strconv.QuoteToASCII(key) + " is not supported")
		}
		if _, dup := out[key]; dup {
			return nil, guardedRequestError(what + " field " + strconv.Quote(key) + " appears more than once")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, guardedRequestError(what + " is not valid JSON")
		}
		out[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, guardedRequestError(what + " is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, guardedRequestError(what + " has trailing data")
	}
	return out, nil
}

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

// decodeString accepts only a JSON string.
func decodeString(raw json.RawMessage) (string, bool) {
	var s string
	return s, len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil
}

// guardedBody turns a decoded client body into the upstream body and the
// output limit written into it. model is the upstream id (prefix removed).
func guardedBody(fields map[string]json.RawMessage, model string, stream bool) ([]byte, float64, error) {
	for _, field := range []string{"n", "best_of"} {
		if raw, ok := fields[field]; ok {
			var n float64
			if json.Unmarshal(raw, &n) != nil || n != 1 {
				return nil, 0, guardedRequestError(field + " other than 1 is not supported")
			}
		}
	}
	limit := 0.0
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		n, ok := tokenLimit(fields[field])
		if !ok {
			return nil, 0, guardedRequestError(field + " must be an integer of at least 1")
		}
		if n != 0 && (limit == 0 || n < limit) {
			limit = n
		}
	}
	if limit == 0 {
		limit = defaultMaxOutputTokens
	}
	out := make(map[string]json.RawMessage, len(fields)+2)
	out["model"], _ = json.Marshal(model)
	out["stream"], _ = json.Marshal(stream)
	out["max_tokens"], _ = json.Marshal(int64(limit))
	if raw, ok := fields["messages"]; ok {
		messages, err := guardedMessages(raw)
		if err != nil {
			return nil, 0, err
		}
		out["messages"] = messages
	}
	for _, field := range numberFields {
		if raw, ok := fields[field]; ok && !isNull(raw) {
			var n float64
			if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, 0, guardedRequestError(field + " must be a number")
			}
			out[field], _ = json.Marshal(n)
		}
	}
	if raw, ok := fields["stop"]; ok && !isNull(raw) {
		stop, err := guardedStop(raw)
		if err != nil {
			return nil, 0, err
		}
		out["stop"] = stop
	}
	if raw, ok := fields["stream_options"]; ok && !isNull(raw) {
		opts, err := strictObject(raw, streamOptionFields, "stream_options")
		if err != nil {
			return nil, 0, err
		}
		if value, present := opts["include_usage"]; present {
			if string(value) != "true" && string(value) != "false" {
				return nil, 0, guardedRequestError("stream_options include_usage must be a boolean")
			}
			out["stream_options"], _ = json.Marshal(map[string]bool{"include_usage": string(value) == "true"})
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, 0, guardedRequestError("request is not valid JSON")
	}
	return body, limit, nil
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func guardedMessages(raw json.RawMessage) (json.RawMessage, error) {
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, guardedRequestError("messages must be an array")
	}
	messages := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		fields, err := strictObject(item, messageFields, "message")
		if err != nil {
			return nil, err
		}
		message := make(map[string]json.RawMessage, len(fields))
		for _, field := range []string{"role", "name"} {
			value, present := fields[field]
			if !present && field != "role" {
				continue
			}
			s, ok := decodeString(value)
			if !ok {
				return nil, guardedRequestError("message " + field + " must be a string")
			}
			message[field], _ = json.Marshal(s)
		}
		if value, ok := fields["content"]; ok {
			content, err := guardedContent(value)
			if err != nil {
				return nil, err
			}
			message["content"] = content
		}
		messages = append(messages, message)
	}
	return json.Marshal(messages)
}

// guardedContent accepts a string, null, or text parts with exactly the keys
// type ("text") and text (a string). Image, audio and file parts can expand to
// far more tokens than their bytes, so anything else is refused.
func guardedContent(raw json.RawMessage) (json.RawMessage, error) {
	if isNull(raw) {
		return raw, nil
	}
	if s, ok := decodeString(raw); ok {
		return json.Marshal(s)
	}
	textOnly := guardedRequestError("only text message content is supported; image, audio and file parts cannot be bounded to the free allocation, and text parts must be exactly {\"type\":\"text\",\"text\":string}")
	var items []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, textOnly
	}
	parts := make([]textPart, 0, len(items))
	for _, item := range items {
		fields, err := strictObject(item, textPartFields, "content part")
		if err != nil {
			return nil, textOnly
		}
		kind, okType := decodeString(fields["type"])
		text, okText := decodeString(fields["text"])
		if !okType || kind != "text" || !okText {
			return nil, textOnly
		}
		parts = append(parts, textPart{Type: "text", Text: text})
	}
	return json.Marshal(parts)
}

// guardedStop accepts a string or an array of at most maxStopSequences strings
// and re-encodes it.
func guardedStop(raw json.RawMessage) (json.RawMessage, error) {
	if s, ok := decodeString(raw); ok {
		return json.Marshal(s)
	}
	var items []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &items) != nil || len(items) > maxStopSequences {
		return nil, guardedRequestError("stop must be a string or an array of at most 4 strings")
	}
	stops := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := decodeString(item)
		if !ok {
			return nil, guardedRequestError("stop must be a string or an array of at most 4 strings")
		}
		stops = append(stops, s)
	}
	return json.Marshal(stops)
}

// tokenLimit reads an output-token bound: absent or null is (0, true), an
// integer from 1 to 2^53 is (n, true), anything else is invalid.
func tokenLimit(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || isNull(raw) {
		return 0, true
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || !(n >= 1) || n > 1<<53 || n != math.Trunc(n) {
		return 0, false
	}
	return n, true
}
