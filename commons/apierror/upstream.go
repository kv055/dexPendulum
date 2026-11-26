package apierror

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// maxReason caps how much of another service's explanation is kept.
const maxReason = 300

// HTTPError is a non-2xx answer from another service, with the reason it gave.
type HTTPError struct {
	// Service names the other side for messages, e.g. "the RoboSats coordinator".
	Service string
	Status  int
	// Reason is the explanation extracted from the response body ("" if it gave none).
	Reason string
	// Code and Detail are set when the other side answered with an apierror Body.
	Code   string
	Detail string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s returned HTTP %d", e.Service, e.Status)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// FromResponse reads up to 4 KiB of a non-2xx response body and returns the
// status together with the reason the service gave. It does not close the body.
func FromResponse(resp *http.Response, service string) *HTTPError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return FromBody(resp.StatusCode, raw, service)
}

// FromBody is FromResponse for a body that has already been read.
func FromBody(status int, raw []byte, service string) *HTTPError {
	reason, code, detail := ParseBody(raw)
	return &HTTPError{Service: service, Status: status, Reason: reason, Code: code, Detail: detail}
}

var (
	htmlTitle  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	whitespace = regexp.MustCompile(`\s+`)
)

// ParseBody extracts a human explanation from an error response body. It
// understands this project's {"error","code","detail"} shape, common API error
// envelopes (message, detail, bad_request, errors[], error_description, Kraken's
// error arrays), plain text and HTML error pages.
func ParseBody(raw []byte) (reason, code, detail string) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", "", ""
	}
	if strings.HasPrefix(text, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) == nil {
			code, _ = obj["code"].(string)
			detail, _ = obj["detail"].(string)
			if r := jsonReason(obj); r != "" {
				if r == detail {
					detail = ""
				}
				return clip(r), code, detail
			}
		}
	}
	if strings.HasPrefix(text, "[") {
		var arr []any
		if json.Unmarshal([]byte(text), &arr) == nil {
			return clip(joinReasons(arr)), "", ""
		}
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") || strings.Contains(lower, "<body") {
		if m := htmlTitle.FindStringSubmatch(text); m != nil {
			return clip(m[1]), "", ""
		}
		return "an HTML error page", "", ""
	}
	return clip(text), "", ""
}

func jsonReason(obj map[string]any) string {
	for _, key := range []string{"error", "message", "bad_request", "detail", "error_description", "errors", "msg", "reason"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				return t
			}
		case []any:
			if r := joinReasons(t); r != "" {
				return r
			}
		case map[string]any:
			if r := jsonReason(t); r != "" {
				return r
			}
		}
	}
	return ""
}

func joinReasons(items []any) string {
	var parts []string
	for _, item := range items {
		switch t := item.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				parts = append(parts, t)
			}
		case map[string]any:
			if r := jsonReason(t); r != "" {
				parts = append(parts, r)
			}
		}
	}
	return strings.Join(parts, "; ")
}

func clip(s string) string {
	s = strings.TrimSpace(whitespace.ReplaceAllString(s, " "))
	if len(s) > maxReason {
		s = strings.TrimSpace(s[:maxReason]) + "…"
	}
	return s
}
