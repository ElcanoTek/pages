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

// Routes that ignore or reject the request before any body parser runs must
// still bound net/http's post-handler drain of an unread declared body so a
// stalled keep-alive client cannot pin the connection after ServeHTTP returns.
func TestIgnoredBodyDrainHitsReadDeadline(t *testing.T) {
	base, rate := httpx.BodyReadBase, httpx.BodyReadMinThroughput
	httpx.BodyReadBase, httpx.BodyReadMinThroughput = 300*time.Millisecond, 1<<30
	t.Cleanup(func() { httpx.BodyReadBase, httpx.BodyReadMinThroughput = base, rate })

	e := setup(t, nil)
	for _, c := range []struct {
		method, host, path string
		wantStatus         int
	}{
		{"GET", dashHost, "/readyz", http.StatusOK},
		{"GET", contentHost, "/readyz", http.StatusOK},
		{"GET", dashHost, "/nope", http.StatusNotFound},
	} {
		conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		// Declare a 100-byte body on a route that never reads r.Body, and
		// send only 1 byte so net/http's post-handler keep-alive drain stalls.
		fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\nx", c.method, c.path, c.host)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			conn.Close()
			t.Fatalf("%s %s: read response: %v", c.method, c.path, err)
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != c.wantStatus {
			conn.Close()
			t.Fatalf("%s %s = %d, want %d", c.method, c.path, res.StatusCode, c.wantStatus)
		}
		// Once the unread-body drain hits BodyReadBase, net/http closes the
		// connection instead of waiting forever for the remaining 99 bytes.
		_, err = io.ReadAll(br)
		conn.Close()
		if err != nil {
			t.Fatalf("%s %s: connection did not close cleanly after drain timeout: %v", c.method, c.path, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("%s %s: connection stayed open for %v", c.method, c.path, elapsed)
		}
	}
}
