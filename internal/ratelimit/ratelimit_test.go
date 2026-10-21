// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/clientip"
	"github.com/ElcanoTek/pages/internal/mcpproto"
)

// Port of test/ratelimit.test.js. The expected header strings and keys were
// recorded from express-rate-limit 8.7.1, which the JS limiter replaced, so
// both implementations are held to what clients and operators saw.

type clock struct{ t time.Time }

func newClock() *clock                   { return &clock{t: time.UnixMilli(1_000_000)} }
func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func limiterWith(c *clock, opts Options) *Limiter {
	if opts.WindowMs == 0 {
		opts.WindowMs = 60_000
	}
	if opts.Limit == 0 {
		opts.Limit = 3
	}
	opts.Now = c.now
	opts.NoSweep = true
	return New(opts)
}

type outcome struct {
	limited bool
	headers http.Header
	err     error
}

func hit(l *Limiter, ip string) outcome {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = net.JoinHostPort(ip, "5000")
	w := httptest.NewRecorder()
	ok, err := l.Hit(w, r)
	return outcome{limited: !ok && err == nil, headers: w.Header(), err: err}
}

func TestDraft7HeadersAndRetryAfterOnlyWhenRefused(t *testing.T) {
	l := limiterWith(newClock(), Options{})
	type row struct {
		limited           bool
		rl, policy, retry string
	}
	var seen []row
	for range 5 {
		o := hit(l, "192.0.2.10")
		seen = append(seen, row{o.limited, o.headers.Get("RateLimit"), o.headers.Get("RateLimit-Policy"), o.headers.Get("Retry-After")})
	}
	want := []row{
		{false, "limit=3, remaining=2, reset=60", "3;w=60", ""},
		{false, "limit=3, remaining=1, reset=60", "3;w=60", ""},
		{false, "limit=3, remaining=0, reset=60", "3;w=60", ""},
		{true, "limit=3, remaining=0, reset=60", "3;w=60", "60"},
		{true, "limit=3, remaining=0, reset=60", "3;w=60", "60"},
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("hit %d: %+v, want %+v", i+1, seen[i], want[i])
		}
	}
}

func TestWindowResetsAndResetCountsDown(t *testing.T) {
	c := newClock()
	l := limiterWith(c, Options{})
	for range 3 {
		if hit(l, "192.0.2.11").limited {
			t.Fatal("limited under the limit")
		}
	}
	c.advance(45_500 * time.Millisecond)
	refused := hit(l, "192.0.2.11")
	if !refused.limited || refused.headers.Get("RateLimit") != "limit=3, remaining=0, reset=15" || refused.headers.Get("Retry-After") != "15" {
		t.Errorf("refused: %+v %v", refused.limited, refused.headers)
	}
	c.advance(14_500 * time.Millisecond)
	fresh := hit(l, "192.0.2.11")
	if fresh.limited || fresh.headers.Get("RateLimit") != "limit=3, remaining=2, reset=60" {
		t.Errorf("a new window starts at its first hit: %+v %v", fresh.limited, fresh.headers)
	}
}

func TestCustomKeysAndResetKey(t *testing.T) {
	l := limiterWith(newClock(), Options{Limit: 1, Key: func(r *http.Request) (string, error) { return r.Header.Get("Who"), nil }})
	try := func(who string) bool {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Who", who)
		ok, _ := l.Hit(httptest.NewRecorder(), r)
		return ok
	}
	if !try("northwind") || try("northwind") || !try("contoso") {
		t.Fatal("keys are not independent")
	}
	l.ResetKey("northwind")
	if !try("northwind") {
		t.Fatal("ResetKey did not forget the window")
	}
}

func TestKeyErrorReachesOnError(t *testing.T) {
	failure := errors.New("key lookup failed")
	var got error
	l := limiterWith(newClock(), Options{
		Key:     func(*http.Request) (string, error) { return "", failure },
		OnError: func(_ http.ResponseWriter, _ *http.Request, err error) { got = err },
	})
	h := l.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next ran") }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !errors.Is(got, failure) {
		t.Fatalf("got %v", got)
	}
}

func TestExpiredWindowsAreSwept(t *testing.T) {
	c := newClock()
	l := limiterWith(c, Options{WindowMs: 1000, Limit: 1})
	hit(l, "192.0.2.14")
	if !hit(l, "192.0.2.14").limited {
		t.Fatal("second hit not limited")
	}
	hit(l, "192.0.2.15")
	if l.Size() != 2 {
		t.Fatalf("size %d", l.Size())
	}
	c.advance(500 * time.Millisecond)
	l.Sweep()
	if l.Size() != 2 {
		t.Fatal("windows still open must be kept")
	}
	c.advance(500 * time.Millisecond)
	l.Sweep()
	if l.Size() != 0 {
		t.Fatal("an address that stopped calling costs nothing after its window")
	}
	if hit(l, "192.0.2.14").limited {
		t.Fatal("fresh window refused")
	}
}

func TestSweeperRunsAndStops(t *testing.T) {
	l := New(Options{WindowMs: 20, Limit: 1})
	defer l.Stop()
	hit(l, "192.0.2.16")
	deadline := time.Now().Add(2 * time.Second)
	for l.Size() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("sweeper never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.Stop()
	l.Stop() // idempotent
}

func TestIPKeysMatchExpressRateLimit(t *testing.T) {
	// Recorded from express-rate-limit 8.7.1's ipKeyGenerator.
	recorded := [][2]string{
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::/56"},
		{"::", "::/56"},
		{"::ffff:127.0.0.1", "127.0.0.1"},
		{"::ffff:7f00:1", "127.0.0.1"},
		{"::1.2.3.4", "1.2.3.4"},
		{"::102:304", "::/56"},
		{"2001:db8::1", "2001:db8::/56"},
		{"2001:db8:1234:56ff::1", "2001:db8:1234:5600::/56"},
		{"2001:db8:1234:5700::1", "2001:db8:1234:5700::/56"},
		{"2001:DB8:1234:5678:9abc:def0:1234:5678", "2001:db8:1234:5600::/56"},
		{"fe80::1%eth0", "fe80::/56"},
		{"64:ff9b::1.2.3.4", "64:ff9b::/56"},
		{"", ""},
	}
	for _, r := range recorded {
		if got := IPKey(r[0], 56); got != r[1] {
			t.Errorf("IPKey(%q) = %q, want %q", r[0], got, r[1])
		}
	}
	if got := IPKey("2001:db8:1234:5678::1", 64); got != "2001:db8:1234:5678::/64" {
		t.Errorf("/64: %s", got)
	}
	if got := IPKey("2001:db8:1234:5678::1", 0); got != "2001:db8:1234:5678::1" {
		t.Errorf("subnet 0 keys the address itself: %s", got)
	}
}

func TestOneIPv6Slash56SharesABudget(t *testing.T) {
	l := limiterWith(newClock(), Options{Limit: 2})
	steps := []struct {
		ip      string
		limited bool
	}{
		{"2001:db8:aa:bb00::1", false},
		{"2001:db8:aa:bbff:ffff::9", false},
		{"2001:db8:aa:bb12::7", true},  // same /56, moved /64
		{"2001:db8:aa:bc00::1", false}, // neighbouring /56
		{"::ffff:192.0.2.20", false},
		{"192.0.2.20", false},
		{"192.0.2.20", true}, // mapped and plain IPv4 share a key
	}
	for _, s := range steps {
		if got := hit(l, s.ip).limited; got != s.limited {
			t.Errorf("%s: limited=%v, want %v", s.ip, got, s.limited)
		}
	}
}

func TestWorksAsMiddlewareWithTheRealRequestIP(t *testing.T) {
	l := New(Options{WindowMs: 60_000, Limit: 1, Exceeded: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, 429, []byte(`{"code":"rate_limited"}`))
	})})
	defer l.Stop()
	srv := httptest.NewServer(l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, 200, []byte(`{"ok":true}`))
	})))
	defer srv.Close()
	first, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != 200 || first.Header.Get("RateLimit") != "limit=1, remaining=0, reset=60" {
		t.Errorf("first: %d %v", first.StatusCode, first.Header)
	}
	second, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 429 || second.Header.Get("Retry-After") != "60" || string(body) != `{"code":"rate_limited"}` {
		t.Errorf("second: %d %v %s", second.StatusCode, second.Header, body)
	}
}

func TestNamedLimitersReadTheirEnv(t *testing.T) {
	cases := []struct {
		env, value string
		build      func(Config) *Limiter
		want       float64
	}{
		{"RL_API_PER_MIN", "", NewAPI, 120},
		{"RL_API_PER_MIN", "3", NewAPI, 3},
		{"RL_API_PER_MIN", "0", NewAPI, 120},
		{"RL_API_PER_MIN", "-5", NewAPI, 120},
		{"RL_API_PER_MIN", "abc", NewAPI, 120},
		{"RL_API_PER_MIN", " 0x10 ", NewAPI, 16},
		{"RL_MCP_PER_MIN", "", NewMCP, 120},
		{"RL_CONTENT_PER_MIN", "", NewContent, 240},
		{"RL_PASSWORD_TRIES", "", NewPassword, 20},
		{"RL_SIGNIN_PER_MIN", "", NewSignin, 60},
		{"RL_SIGNIN_CALLBACK_PER_MIN", "", NewSigninCallback, 60},
		{"RL_SIGNOUT_PER_MIN", "", func(c Config) *Limiter { return NewSignout(c, nil) }, 30},
		{"RL_BACKCHANNEL_PER_MIN", "1e3", NewBackchannel, 1000},
	}
	for _, c := range cases {
		t.Setenv(c.env, c.value)
		l := c.build(Config{})
		if l.Limit() != c.want {
			t.Errorf("%s=%q: limit %v, want %v", c.env, c.value, l.Limit(), c.want)
		}
		l.Stop()
	}
	t.Setenv("RL_PASSWORD_WINDOW_MIN", "")
	t.Setenv("RL_PASSWORD_TRIES", "1")
	l := NewPassword(Config{})
	defer l.Stop()
	hit(l, "192.0.2.30")
	o := hit(l, "192.0.2.30")
	if o.headers.Get("RateLimit-Policy") != "1;w=900" || o.headers.Get("Retry-After") != "900" {
		t.Errorf("password window: %v", o.headers)
	}
	if PasswordWindowMinutes() != 15 {
		t.Error("PasswordWindowMinutes default")
	}
}

func TestDefaultRefusalBodies(t *testing.T) {
	w := httptest.NewRecorder()
	DefaultExceeded.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 429 ||
		body["code"] != "rate_limited" || body["error"] != "rate limit exceeded — slow down" ||
		w.Header().Get("Content-Type") != "application/json; charset=utf-8" || !strings.HasPrefix(w.Header().Get("ETag"), `W/"`) {
		t.Errorf("default: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = httptest.NewRecorder()
	MCPExceeded.ServeHTTP(w, httptest.NewRequest("POST", "/mcp", nil))
	if w.Code != 429 || w.Body.String() != `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Rate limit exceeded; retry later"}}` {
		t.Errorf("mcp: %d %s", w.Code, w.Body)
	}
}

func TestSignoutKeysBySessionThenAddress(t *testing.T) {
	t.Setenv("RL_SIGNOUT_PER_MIN", "1")
	l := NewSignout(Config{}, func(r *http.Request) string {
		if c, err := r.Cookie("pages_session"); err == nil {
			return c.Value
		}
		return ""
	})
	defer l.Stop()
	req := func(cookie string) bool {
		r := httptest.NewRequest("POST", "/logout", nil)
		r.RemoteAddr = "192.0.2.40:1"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "pages_session", Value: cookie})
		}
		ok, _ := l.Hit(httptest.NewRecorder(), r)
		return ok
	}
	if !req("northwind") || req("northwind") {
		t.Error("a session has its own budget")
	}
	if !req("contoso") || !req("") || req("") {
		t.Error("another session, and the address budget, are separate")
	}
	// node -e 'crypto.createHash("sha256").update("t").digest("base64url")'
	if SessionKey("t") != "session:47mKTaMaEn1L3m5DAz9muidMqw636xxw7EFAK_YnPdg" {
		t.Errorf("SessionKey: %s", SessionKey("t"))
	}
}

// From test/ratelimit.integration.js and test/proxy.integration.js, for the
// MCP surface: the limiter runs after the Host boundary and before bearer
// auth and body parsing, so an over-limit client gets the JSON-RPC 429 with
// RateLimit headers whatever it sends.
func TestMCPLimiterRunsBeforeAuthAndParsing(t *testing.T) {
	t.Setenv("DASHBOARD_ORIGIN", "http://dashboard.test")
	t.Setenv("RL_MCP_PER_MIN", "2")
	limiter := NewMCP(Config{})
	defer limiter.Stop()
	h := mcpproto.NewHandler(mcpproto.HandlerOptions{
		MaxBodyBytes: 1 << 20,
		RateLimit:    limiter.Middleware,
		Authenticate: func(*http.Request) (*http.Request, error) {
			return nil, apierror.Unauthorized("missing bearer token", "no_token", nil)
		},
	})
	send := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader("{not json"))
		r.Host = "dashboard.test"
		r.RemoteAddr = "127.0.0.2:4000"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := range 2 {
		if w := send(); w.Code != 401 {
			t.Fatalf("request %d under the limit: %d", i+1, w.Code)
		}
	}
	w := send()
	if w.Code != 429 || w.Header().Get("RateLimit") == "" || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(w.Body.String(), `"code":-32000`) {
		t.Fatalf("over the limit: %d %v %s", w.Code, w.Header(), w.Body)
	}
	// A wrong Host is refused by the boundary and costs no budget.
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Host = "evil.test"
	r.RemoteAddr = "127.0.0.3:4000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || w.Header().Get("RateLimit") != "" {
		t.Fatalf("boundary first: %d %v", w.Code, w.Header())
	}
}

// Port of test/proxy.integration.js's limiter attribution: through a
// one-hop proxy that appends the connecting address to X-Forwarded-For (and
// keeps earlier values, so hop bounding is exercised), two real clients get
// independent budgets and a spoofed X-Forwarded-For buys no extra allowance.
func TestProxyAttributionPerMode(t *testing.T) {
	for _, mode := range []string{"default", "direct", "custom"} {
		t.Run(mode, func(t *testing.T) {
			setting := ""
			switch mode {
			case "direct":
				setting = "false"
			case "custom":
				setting = "127.0.0.1/32,::1/128,198.51.100.0/24"
			}
			policy, err := clientip.Parse(setting)
			if err != nil {
				t.Fatal(err)
			}
			l := New(Options{WindowMs: 60_000, Limit: 2, ClientIP: policy.ClientIP, NoSweep: true})
			app := httptest.NewServer(l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(204)
			})))
			defer app.Close()
			target, _ := url.Parse(app.URL)
			proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target)) // appends the peer to X-Forwarded-For
			defer proxy.Close()
			entry := proxy.URL
			if mode == "direct" {
				entry = app.URL
			}
			get := func(local, base string, xff string) int {
				client := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{
					LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)},
				}).DialContext}}
				req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/raw/northwind", nil)
				if xff != "" {
					req.Header.Set("X-Forwarded-For", xff)
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				return res.StatusCode
			}
			for _, client := range []string{"127.0.0.2", "127.0.0.3"} {
				for i := range 2 {
					if got := get(client, entry, ""); got != 204 {
						t.Fatalf("%s gets allowance %d: %d", client, i+1, got)
					}
				}
				if got := get(client, entry, ""); got != 429 {
					t.Fatalf("%s third request: %d", client, got)
				}
				if got := get(client, entry, "192.0.2.10, 192.0.2.11"); got != 429 {
					t.Fatalf("earlier forwarded values created another allowance for %s", client)
				}
			}
			if mode == "custom" {
				// A direct peer outside the proxy CIDRs cannot change its attribution.
				for i, want := range []int{204, 204, 429} {
					if got := get("127.0.0.6", app.URL, "192.0.2."+string(rune('0'+i))); got != want {
						t.Fatalf("direct peer request %d: %d, want %d", i+1, got, want)
					}
				}
			}
		})
	}
}
