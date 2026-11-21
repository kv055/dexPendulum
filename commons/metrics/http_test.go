package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"commons/tracing"

	"github.com/gorilla/websocket"
)

// TestMiddlewarePreservesHijacker guards against the regression where wrapping
// the ResponseWriter in statusRecorder hid Hijack(), turning every WebSocket
// upgrade behind the middleware into a 500.
func TestMiddlewarePreservesHijacker(t *testing.T) {
	reg := NewServiceRegistry("test")

	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{}
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		conn.Close()
	})

	srv := httptest.NewServer(reg.Middleware(mux))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):] + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial through metrics middleware failed (status %d): %v", status, err)
	}
	conn.Close()
}

// contextReplacing mimics session loaders / auth gates (scs LoadAndSave), which
// hand the next handler a shallow request copy via r.WithContext.
func contextReplacing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(r.Context()))
	})
}

// TestMiddlewareRouteLabelThroughContextReplacingLayer guards the regression
// where every request behind a session layer was labelled route="unmatched"
// because the mux set Pattern on a request copy the middleware never saw.
func TestMiddlewareRouteLabelThroughContextReplacingLayer(t *testing.T) {
	reg := NewServiceRegistry("test")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	handler := reg.Middleware(contextReplacing(tracing.RouteNamer(mux)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items/42", nil))

	if got := requestCount(t, reg, "GET /api/items/{id}"); got != 1 {
		t.Fatalf("route-labelled request count = %v, want 1", got)
	}
	if got := requestCount(t, reg, "unmatched"); got != 0 {
		t.Fatalf("unmatched request count = %v, want 0", got)
	}
}

// TestMiddlewareRouteLabelDirectMux keeps the plain wiring (mux directly below
// the middleware, as in mm-controller) labelling routes without RouteNamer.
func TestMiddlewareRouteLabelDirectMux(t *testing.T) {
	reg := NewServiceRegistry("test")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {})

	reg.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livez", nil))

	if got := requestCount(t, reg, "GET /livez"); got != 1 {
		t.Fatalf("route-labelled request count = %v, want 1", got)
	}
}

// requestCount sums http_requests_total samples carrying the given route label.
func requestCount(t *testing.T, reg *ServiceRegistry, route string) float64 {
	t.Helper()
	families, err := reg.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range families {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "route" && lp.GetValue() == route {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}
