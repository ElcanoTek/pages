// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/shell"
)

// A client that declares a body and then stops sending it must not pin the
// connection: the body parsers' read deadline fires and the request is
// answered as a truncated body (400), on the dashboard fallback paths that
// reach the broad parser before any limiter as well as on the API.
func TestStalledBodyHitsReadDeadline(t *testing.T) {
	base, rate := httpx.BodyReadBase, httpx.BodyReadMinThroughput
	httpx.BodyReadBase, httpx.BodyReadMinThroughput = 300*time.Millisecond, 1<<30
	t.Cleanup(func() { httpx.BodyReadBase, httpx.BodyReadMinThroughput = base, rate })

	e := setup(t, nil)
	for _, c := range []struct {
		path, contentType, body string
	}{
		{"/nope", "application/json", shell.RequestError(http.StatusBadRequest)},
		{"/some/arbitrary/fallback", "application/x-www-form-urlencoded", shell.RequestError(http.StatusBadRequest)},
		{"/api/v1/pages", "application/json", `{"error":"invalid request","code":"bad_request"}`},
	} {
		conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		// Content-Length promises 100 bytes; only one is ever sent.
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: 100\r\n\r\n{", c.path, dashHost, c.contentType)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			t.Fatalf("%s: no response to a stalled body: %v", c.path, err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		conn.Close()
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("%s: answered after %v", c.path, elapsed)
		}
		if res.StatusCode != http.StatusBadRequest || string(b) != c.body {
			t.Fatalf("%s = %d %s", c.path, res.StatusCode, b)
		}
	}
}
