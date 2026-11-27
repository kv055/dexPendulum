package apierror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

func TestDescribeUsesOperatorMessageAndKeepsCauseAsDetail(t *testing.T) {
	cause := errors.New("robosats: GET /api/robot/ returned HTTP 403")
	err := fmt.Errorf("quote: %w", Wrap(cause, "robosats_robot_rejected", "RoboSats rejected the robot token.").WithStatus(http.StatusConflict).WithLeg("dex"))
	status, body, ref := Describe(err, http.StatusBadGateway, Options{Service: "the Taker Controller"})
	if status != http.StatusConflict || body.Error != "RoboSats rejected the robot token." || body.Code != "robosats_robot_rejected" || body.Leg != "dex" || ref != "" {
		t.Fatalf("unexpected description: %d %+v %q", status, body, ref)
	}
	if body.Detail != cause.Error() {
		t.Fatalf("detail = %q, want the cause", body.Detail)
	}
}

func TestDescribeWithholdsDatabaseCausesBehindAReference(t *testing.T) {
	err := Wrap(errors.New(`pq: duplicate key value violates unique constraint "accounts_pkey"`), "save_failed", "The account could not be saved.")
	_, body, ref := Describe(err, http.StatusInternalServerError, Options{})
	if body.Detail != "" || ref == "" || body.Ref != ref {
		t.Fatalf("database cause leaked or unreferenced: %+v %q", body, ref)
	}

	_, body, ref = Describe(errors.New("ERROR: relation \"x\" does not exist (SQLSTATE 42P01)"), http.StatusInternalServerError, Options{Service: "the Unified API"})
	if body.Code != CodeDatabase || strings.Contains(body.Error, "relation") || !strings.Contains(body.Error, ref) {
		t.Fatalf("unexpected database description: %+v", body)
	}
}

func TestDescribeClassifiesNetworkFailures(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	status, body, _ := Describe(fmt.Errorf("call controller: %w", refused), http.StatusInternalServerError, Options{Service: "the dashboard server"})
	if status != http.StatusBadGateway || body.Code != CodeUnreachable || !strings.HasPrefix(body.Error, "The dashboard server could not reach") {
		t.Fatalf("unexpected unreachable description: %d %+v", status, body)
	}

	status, body, _ = Describe(fmt.Errorf("quote: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, Options{Service: "the Unified API"})
	if status != http.StatusServiceUnavailable || body.Code != CodeTimeout {
		t.Fatalf("unexpected timeout description: %d %+v", status, body)
	}

	status, body, _ = Describe(sql.ErrNoRows, http.StatusInternalServerError, Options{})
	if status != http.StatusNotFound || body.Code != CodeNotFound {
		t.Fatalf("unexpected not-found description: %d %+v", status, body)
	}
}

func TestDescribeUnexpectedErrors(t *testing.T) {
	_, body, ref := Describe(errors.New("boom"), http.StatusInternalServerError, Options{Service: "the Taker Controller", ExposeUnexpected: true})
	if body.Code != CodeInternal || !strings.Contains(body.Error, "boom") || !strings.Contains(body.Error, ref) {
		t.Fatalf("exposed unexpected error lost its cause: %+v", body)
	}
	_, body, _ = Describe(errors.New("boom"), http.StatusInternalServerError, Options{Service: "the Unified API"})
	if strings.Contains(body.Error, "boom") || body.Ref == "" {
		t.Fatalf("hidden unexpected error leaked: %+v", body)
	}
	status, body, ref := Describe(errors.New("amount must be positive"), http.StatusBadRequest, Options{})
	if status != http.StatusBadRequest || body.Error != "amount must be positive" || ref != "" {
		t.Fatalf("client errors must pass through: %d %+v", status, body)
	}
}

func TestParseBodyUnderstandsCommonShapes(t *testing.T) {
	cases := []struct {
		name, body, reason, code string
	}{
		{"own apierror", `{"error":"RoboSats trading is turned off.","code":"robosats_disabled","detail":"x"}`, "RoboSats trading is turned off.", "robosats_disabled"},
		{"robosats bad_request", `{"bad_request":"This order has been cancelled by the maker"}`, "This order has been cancelled by the maker", ""},
		{"kraken spot", `{"error":["EOrder:Insufficient funds"]}`, "EOrder:Insufficient funds", ""},
		{"kraken futures", `{"result":"error","error":"insufficientAvailableFunds","serverTime":"2026"}`, "insufficientAvailableFunds", ""},
		{"nested", `{"error":{"message":"rate limited"}}`, "rate limited", ""},
		{"errors list", `{"errors":[{"message":"a"},{"message":"b"}]}`, "a; b", ""},
		{"plain text", "offer is not currently live\n", "offer is not currently live", ""},
		{"html", "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>", "502 Bad Gateway", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		reason, code, _ := ParseBody([]byte(tc.body))
		if reason != tc.reason || code != tc.code {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, reason, code, tc.reason, tc.code)
		}
	}
	long, _, _ := ParseBody([]byte(strings.Repeat("x", 1000)))
	if len(long) > maxReason+len("…") {
		t.Fatalf("reason not clipped: %d bytes", len(long))
	}
}

func TestHTTPErrorMessage(t *testing.T) {
	err := FromBody(409, []byte(`{"bad_request":"Order is not public"}`), "the RoboSats coordinator")
	if err.Error() != "the RoboSats coordinator returned HTTP 409: Order is not public" {
		t.Fatalf("unexpected message %q", err.Error())
	}
	if FromBody(404, nil, "mempool.space").Error() != "mempool.space returned HTTP 404" {
		t.Fatal("empty body should keep the bare status")
	}
}

func TestDescribeServiceUnavailable(t *testing.T) {
	status, body, ref := Describe(errors.New("dex release webhook is not configured"), http.StatusServiceUnavailable, Options{Service: "the Taker Controller", ExposeUnexpected: true})
	if status != http.StatusServiceUnavailable || body.Code != CodeUnavailable || ref != "" ||
		body.Error != "The Taker Controller can't do this right now: dex release webhook is not configured. Check its configuration and startup log, or try again once it has finished starting." {
		t.Fatalf("unexpected 503 description: %d %+v %q", status, body, ref)
	}
	_, body, _ = Describe(errors.New("database is not ready"), http.StatusServiceUnavailable, Options{Service: "the Taker Controller"})
	if body.Code != CodeDatabase {
		t.Fatalf("database readiness should classify as a database error: %+v", body)
	}
}
