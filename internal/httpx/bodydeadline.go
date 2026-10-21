// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package httpx

import (
	"context"
	"io"
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
	eof     bool
	expired bool
}

type eofTrackingBody struct {
	io.ReadCloser
	ctl *bodyControl
}

func (b *eofTrackingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.ctl.eof = true
	}
	return n, err
}

func hasRequestBody(r *http.Request) bool {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return false
	}
	return r.ContentLength != 0 || len(r.TransferEncoding) > 0
}

// WithBodyReadControl records w's ResponseController on r's context so body
// readers deeper in the stack (which see only the request) can arm a read
// deadline on the underlying connection. When r declares a body, it also wraps
// r.Body to track EOF and arms a baseline BodyReadBase deadline immediately so
// routes that reject or ignore the body without calling a parser cannot leave
// net/http's unread-body drain unbounded. The server's root handler calls it
// with the connection's own ResponseWriter and defers BoundUnreadBody(r).
func WithBodyReadControl(w http.ResponseWriter, r *http.Request) *http.Request {
	ctl := &bodyControl{rc: http.NewResponseController(w)}
	r = r.WithContext(context.WithValue(r.Context(), bodyCtlKey{}, ctl))
	if hasRequestBody(r) {
		r.Body = &eofTrackingBody{ReadCloser: r.Body, ctl: ctl}
		_ = ctl.rc.SetReadDeadline(time.Now().Add(BodyReadBase))
	} else {
		ctl.eof = true
	}
	return r
}

// BoundUnreadBody ensures a baseline read deadline is armed when ServeHTTP
// returns on a request whose body has not reached EOF, bounding net/http's
// post-handler drain of any unread body bytes. When a body reader's deadline
// already expired, the expired deadline is left in place so the drain fails
// immediately.
func BoundUnreadBody(r *http.Request) {
	ctl, _ := r.Context().Value(bodyCtlKey{}).(*bodyControl)
	if ctl == nil || ctl.expired || ctl.eof || !hasRequestBody(r) {
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
// returns the function that clears or re-bounds it; callers defer it around
// their read. When the body was read to EOF before the deadline, disarm clears
// the read deadline so long streaming responses are unaffected. When the body
// reader stops early before EOF (for example on a 413 after limit+1 bytes,
// before WriteHeader drains unread request data), disarm restores the baseline
// BodyReadBase deadline so that drain cannot block indefinitely. Once the
// deadline has passed the clear is skipped: the body is then incomplete, and
// net/http's drain of the unread remainder must fail fast too.
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
		if !time.Now().Before(deadline) {
			ctl.expired = true
			return
		}
		if ctl.eof {
			_ = ctl.rc.SetReadDeadline(time.Time{})
		} else {
			_ = ctl.rc.SetReadDeadline(time.Now().Add(BodyReadBase))
		}
	}
}
