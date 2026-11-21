// Package httproute carries the matched ServeMux route pattern back out to
// middleware that wraps context-replacing layers.
//
// http.ServeMux records the matched pattern on the *http.Request it is handed
// (r.Pattern). Any middleware between an outer observer and the mux that calls
// r.WithContext (session loaders, auth gates) hands the mux a shallow copy, so
// the outer observer's r.Pattern stays empty. Capture puts a mutable slot in
// the request context; Record, called right next to the mux, fills it. The
// outer observer then reads the pattern through the slot regardless of how many
// request copies were made in between.
package httproute

import (
	"context"
	"net/http"
)

type slotKey struct{}

type slot struct{ pattern string }

// Capture returns a request whose context carries a route slot, plus a getter
// that reports the matched pattern once the inner handler has returned. The
// getter prefers the returned request's own Pattern (set when the mux sits
// directly below the caller) and falls back to the slot filled by Record.
func Capture(r *http.Request) (*http.Request, func() string) {
	s := &slot{}
	r = r.WithContext(context.WithValue(r.Context(), slotKey{}, s))
	return r, func() string {
		if r.Pattern != "" {
			return r.Pattern
		}
		return s.pattern
	}
}

// Record copies r.Pattern into the slot installed by Capture, if any. Call it
// after the ServeMux has dispatched, on the request the mux received.
func Record(r *http.Request) {
	if r.Pattern == "" {
		return
	}
	if s, ok := r.Context().Value(slotKey{}).(*slot); ok {
		s.pattern = r.Pattern
	}
}
