// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package httpx

import (
	"context"
	"net/http"
	"time"
)

// Body read deadlines. The server sets only a header timeout (a blanket
// http.Server.ReadTimeout would cut off slow but legitimate large uploads), so
// every body reader arms a per-request read deadline scaled to the most it
// will read: BodyReadBase plus the time to move that many bytes at
// BodyReadMinThroughput, capped at BodyReadMax. A client that declares a body
// and then stalls or drips it gets a read error at the deadline, which each
// parser answers exactly as it answers a truncated body (body-parser's 400
// "request aborted"). The deadline only affects reads and is cleared once the
// body has been read, so streaming responses are unaffected.
//
// The variables are overridable so tests can use short deadlines.
var (
	BodyReadBase                = 30 * time.Second
	BodyReadMinThroughput int64 = 64 * 1024 // bytes per second
	BodyReadMax                 = 20 * time.Minute
)

type bodyCtlKey struct{}

type bodyControl struct {
	rc      *http.ResponseController
	expired bool
}

func hasRequestBody(r *http.Request) bool {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return false
	}
	return r.ContentLength != 0 || len(r.TransferEncoding) > 0
}

// WithBodyReadControl records w's ResponseController on r's context so body
// readers deeper in the stack (which see only the request) can arm a read
// deadline on the underlying connection. When r declares a body, it also arms
// a baseline BodyReadBase deadline immediately so routes that reject or ignore
// the body without calling a parser cannot leave net/http's post-handler body
// drain unbounded. The server's root handler calls it with the connection's
// own ResponseWriter and defers BoundUnreadBody(r).
func WithBodyReadControl(w http.ResponseWriter, r *http.Request) *http.Request {
	ctl := &bodyControl{rc: http.NewResponseController(w)}
	if hasRequestBody(r) {
		_ = ctl.rc.SetReadDeadline(time.Now().Add(BodyReadBase))
	}
	return r.WithContext(context.WithValue(r.Context(), bodyCtlKey{}, ctl))
}

// BoundUnreadBody ensures a baseline read deadline is armed when ServeHTTP
// returns on a request with a body, bounding net/http's post-handler drain of
// any unread body bytes (for example after a 413 early return where disarm
// cleared the read deadline, or a route that never read r.Body). When a body
// reader's deadline already expired, the expired deadline is left in place so
// the drain fails immediately.
func BoundUnreadBody(r *http.Request) {
	ctl, _ := r.Context().Value(bodyCtlKey{}).(*bodyControl)
	if ctl == nil || ctl.expired || !hasRequestBody(r) {
		return
	}
	_ = ctl.rc.SetReadDeadline(time.Now().Add(BodyReadBase))
}

// BodyReadTimeout is the read budget for a body of up to n bytes.
func BodyReadTimeout(n int64) time.Duration {
	if n < 0 {
		n = 0
	}
	d := BodyReadBase
	if BodyReadMinThroughput > 0 {
		if secs := n / BodyReadMinThroughput; secs >= int64(BodyReadMax/time.Second) {
			d = BodyReadMax
		} else {
			d += time.Duration(n) * time.Second / time.Duration(BodyReadMinThroughput)
		}
	}
	if d > BodyReadMax {
		d = BodyReadMax
	}
	return d
}

// ArmBodyReadDeadline sets a read deadline on r's connection sized for a body
// of at most limit bytes (or the declared Content-Length, when smaller) and
// returns the function that clears it again; callers defer it around their
// read. Once the deadline has passed the clear is skipped: the body is then
// incomplete, and net/http's post-handler drain of the unread remainder must
// fail fast too (so the connection is closed) rather than block on the same
// stalled client. It is a no-op when the request carries no
// ResponseController (WithBodyReadControl did not run) or the writer does not
// support deadlines (httptest.ResponseRecorder).
func ArmBodyReadDeadline(r *http.Request, limit int64) (disarm func()) {
	ctl, _ := r.Context().Value(bodyCtlKey{}).(*bodyControl)
	if ctl == nil || ctl.rc == nil {
		return func() {}
	}
	n := limit
	if r.ContentLength >= 0 && r.ContentLength < n {
		n = r.ContentLength
	}
	deadline := time.Now().Add(BodyReadTimeout(n))
	if err := ctl.rc.SetReadDeadline(deadline); err != nil {
		return func() {}
	}
	return func() {
		if time.Now().Before(deadline) {
			_ = ctl.rc.SetReadDeadline(time.Time{})
		} else {
			ctl.expired = true
		}
	}
}
