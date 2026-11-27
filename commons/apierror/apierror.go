// Package apierror gives every service the same error shape for the operator:
// a plain-language message that says what happened and what to do, a stable
// code the UI can branch on, and the technical cause kept separately.
//
// Responses are JSON: {"error": message, "code": code, "detail": cause, "leg": leg}.
// "error" stays the human message so older clients that only read it keep working.
package apierror

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// Error is an error written for the operator. Message is shown as is; Err is
// the underlying cause, surfaced as technical detail and in logs.
type Error struct {
	Code    string
	Message string
	// Status is the HTTP status a handler should answer with; 0 lets the handler decide.
	Status int
	// Leg names the trade leg that failed ("spot", "hedge", "withdrawal", "dex"), if any.
	Leg string
	Err error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// New returns an operator-facing error without an underlying cause.
func New(code, message string) *Error { return &Error{Code: code, Message: message} }

// Newf is New with a formatted message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches an operator-facing message to a technical cause.
func Wrap(err error, code, message string) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// Wrapf is Wrap with a formatted message.
func Wrapf(err error, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// WithStatus sets the HTTP status a handler should use for this error.
func (e *Error) WithStatus(status int) *Error { e.Status = status; return e }

// WithLeg records which trade leg failed.
func (e *Error) WithLeg(leg string) *Error { e.Leg = leg; return e }

// As returns the outermost *Error in err's chain.
func As(err error) (*Error, bool) {
	var ae *Error
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// CodeOf returns the code of the outermost *Error in err's chain, or "".
func CodeOf(err error) string {
	if ae, ok := As(err); ok {
		return ae.Code
	}
	return ""
}

// Body is the JSON error response.
type Body struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	Leg    string `json:"leg,omitempty"`
	// Ref matches the server log line for errors whose cause is not shown.
	Ref string `json:"ref,omitempty"`
}

// Options control how Describe phrases errors that carry no operator message.
type Options struct {
	// Service names the answering service in messages, e.g. "the Taker Controller".
	Service string
	// ExposeUnexpected shows the raw cause of unexpected errors in the message.
	// Services that must not leak internals leave it false and rely on Ref.
	ExposeUnexpected bool
}

// Stable codes for failures that are classified rather than written by a handler.
const (
	CodeTimeout     = "timeout"
	CodeCancelled   = "cancelled"
	CodeUnreachable = "upstream_unreachable"
	CodeDatabase    = "database_error"
	CodeNotFound    = "not_found"
	CodeInternal    = "internal_error"
	CodeUnavailable = "service_unavailable"
)

// Describe turns any error into the status and body a handler should send.
// status is the handler's choice; an *Error with its own Status overrides it.
// logRef is non-empty when the cause is withheld and must be logged under that reference.
func Describe(err error, status int, opts Options) (int, Body, string) {
	service := opts.Service
	if service == "" {
		service = "The server"
	}
	if ae, ok := As(err); ok {
		if ae.Status != 0 {
			status = ae.Status
		}
		body := Body{Error: ae.Message, Code: ae.Code, Leg: ae.Leg}
		if ae.Err != nil {
			if isDatabase(ae.Err) {
				body.Ref = newRef()
				return status, body, body.Ref
			}
			body.Detail = ae.Err.Error()
		}
		return status, body, ""
	}
	if err == nil {
		return status, Body{Error: http.StatusText(status)}, ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return gatewayStatus(status, http.StatusGatewayTimeout), Body{
			Error:  capitalize(service) + " timed out waiting for a service it depends on. Try again in a moment; if it keeps happening, check that service on the Metrics dashboard.",
			Code:   CodeTimeout,
			Detail: err.Error(),
		}, ""
	case errors.Is(err, context.Canceled):
		return status, Body{Error: "The request was cancelled before it finished.", Code: CodeCancelled}, ""
	case IsUnreachable(err):
		return gatewayStatus(status, http.StatusBadGateway), Body{
			Error:  capitalize(service) + " could not reach a service it depends on. It may be restarting; try again in a moment.",
			Code:   CodeUnreachable,
			Detail: err.Error(),
		}, ""
	case errors.Is(err, sql.ErrNoRows):
		return http.StatusNotFound, Body{Error: "The requested record does not exist (it may have been deleted).", Code: CodeNotFound}, ""
	case isDatabase(err):
		ref := newRef()
		return status, Body{
			Error: capitalize(service) + " could not read or write its database, so the request did not complete. Try again; if it keeps failing, check the Postgres container (reference " + ref + " in the server log).",
			Code:  CodeDatabase,
			Ref:   ref,
		}, ref
	}
	if status < 500 {
		return status, Body{Error: err.Error()}, ""
	}
	if status == http.StatusServiceUnavailable {
		// Handlers answer 503 for parts of a service that are not configured or not started yet.
		if opts.ExposeUnexpected {
			return status, Body{
				Error: capitalize(service) + " can't do this right now: " + strings.TrimSuffix(err.Error(), ".") + ". Check its configuration and startup log, or try again once it has finished starting.",
				Code:  CodeUnavailable,
			}, ""
		}
		ref := newRef()
		return status, Body{
			Error: capitalize(service) + " can't handle this request right now (it may still be starting or is missing configuration). Try again in a moment; reference " + ref + " is in its log.",
			Code:  CodeUnavailable,
			Ref:   ref,
		}, ref
	}
	ref := newRef()
	if opts.ExposeUnexpected {
		return status, Body{
			Error: capitalize(service) + " hit an unexpected error: " + err.Error() + " (reference " + ref + ")",
			Code:  CodeInternal,
			Ref:   ref,
		}, ref
	}
	return status, Body{
		Error: capitalize(service) + " hit an unexpected error (reference " + ref + "). The details are in its log.",
		Code:  CodeInternal,
		Ref:   ref,
	}, ref
}

// gatewayStatus keeps a handler's specific 5xx choice but upgrades generic ones.
func gatewayStatus(status, fallback int) int {
	if status == 0 || status == http.StatusInternalServerError || status < 500 {
		return fallback
	}
	return status
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IsUnreachable reports whether err means a network peer could not be reached at all.
func IsUnreachable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "socks connect") || strings.Contains(msg, "general socks server failure") ||
		strings.Contains(msg, "host unreachable")
}

func isDatabase(err error) bool {
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "database is not ready") || strings.Contains(lower, "database unavailable") || strings.Contains(lower, "database is unavailable") {
		return true
	}
	return strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "pq: ") || strings.Contains(msg, "pgx") ||
		strings.Contains(msg, "failed to connect to `") || strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "violates foreign key") || strings.Contains(msg, "violates check constraint")
}

func newRef() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
