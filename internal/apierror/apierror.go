// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package apierror is the error that carries the HTTP status an API surface
// should answer with. Business logic (versions, tokens, templates, …) returns
// these; the REST, admin and MCP layers turn them into their own error
// envelopes. Anything that is NOT an *Error is an unexpected bug: it becomes a
// 500 and its text is never shown to the caller.
package apierror

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
)

// Error is an API-facing failure.
type Error struct {
	Status  int
	Message string
	// Code is a stable machine-readable identifier ("" when none).
	Code string
	// Details is optional structured detail serialized beside the message
	// (nil when none).
	Details any
}

func (e *Error) Error() string { return e.Message }

// New builds an *Error.
func New(status int, message, code string, details any) *Error {
	return &Error{Status: status, Message: message, Code: code, Details: details}
}

// Newf builds an *Error with a formatted message and no code.
func Newf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is a 400. code and details are optional ("" / nil).
func BadRequest(msg, code string, details any) *Error { return New(400, msg, code, details) }

// Unauthorized is a 401.
func Unauthorized(msg, code string, details any) *Error { return New(401, msg, code, details) }

// Forbidden is a 403.
func Forbidden(msg, code string, details any) *Error { return New(403, msg, code, details) }

// NotFound is a 404.
func NotFound(msg, code string, details any) *Error { return New(404, msg, code, details) }

// Conflict is a 409.
func Conflict(msg, code string, details any) *Error { return New(409, msg, code, details) }

// As returns the *Error in err's chain, if any.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

var connectTimeout = regexp.MustCompile(`(?i)timeout exceeded when trying to connect|connection terminated due to connection timeout|failed to connect`)

// FromDB translates the bounded-wait Postgres failures (lock, statement and
// idle-transaction timeouts, an unreachable server) into clean 503s instead of
// opaque 500s. It returns nil for anything else: a real bug stays a 500 and is
// logged.
func FromDB(err error) *Error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "55P03": // lock_not_available: lock_timeout waiting on FOR UPDATE
			return New(503, "the page is busy with another update — retry shortly", "db_lock_timeout", nil)
		case "57014": // query_canceled: statement_timeout
			return New(503, "database query timed out — retry shortly", "db_statement_timeout", nil)
		case "57P05", "57P01", "57P03": // idle-in-transaction kill, admin shutdown, cannot connect now
			return unavailable()
		}
		return nil
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return unavailable()
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) && connectTimeout.MatchString(err.Error()) {
		return unavailable()
	}
	if connectTimeout.MatchString(err.Error()) {
		return unavailable()
	}
	return nil
}

func unavailable() *Error {
	return New(503, "database unavailable — retry shortly", "db_unavailable", nil)
}
