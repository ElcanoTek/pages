// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package ratelimit is Pages' per-surface rate limiting (PLAN §7/§9). A
// single small box that serves untrusted content and runs an agent deploy
// loop is trivially DoS'd, and a stuck agent will hammer the deploy/MCP
// endpoints — so the hot paths are capped.
//
// Limits are tuned per surface and overridable by env (RL_*). Keying is by
// client IP under the one-hop proxy policy (package clientip). Refusals are
// JSON 429s except where a person is the caller (content reads, the password
// form, central sign-in), whose HTML the owning package supplies.
//
// The limiter replaced express-rate-limit 8.7.1 in the Node server and kept
// its wire behaviour: draft-7 headers, a fixed window per key starting at the
// key's first hit, IPv6 clients keyed by /56.
package ratelimit

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/pages/internal/clientip"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/mcpproto"
	"github.com/ElcanoTek/pages/internal/mcpproto/jsjson"
)

// Options configures New.
type Options struct {
	// WindowMs is the window length in milliseconds (fractional allowed,
	// as RL_PASSWORD_WINDOW_MIN may be).
	WindowMs float64
	// Limit is the number of requests a key may make per window.
	Limit float64
	// Exceeded answers a refused request. The RateLimit headers and
	// Retry-After are already set; it may override Retry-After. nil answers
	// the default JSON 429.
	Exceeded http.Handler
	// Key returns the request's key. nil keys by IPKey(ClientIP(r), 56).
	Key func(r *http.Request) (string, error)
	// ClientIP resolves the client address for the default key; nil uses
	// the connecting peer (clientip.RemoteAddr).
	ClientIP func(r *http.Request) string
	// OnError answers a request whose Key failed (Express's next(err)); nil
	// writes a bare 500.
	OnError func(w http.ResponseWriter, r *http.Request, err error)
	// Now is the clock (tests inject one).
	Now func() time.Time
	// NoSweep disables the background sweep of expired windows (tests call
	// Sweep themselves).
	NoSweep bool
}

type window struct {
	hits    float64
	resetAt float64 // Unix milliseconds
}

// Limiter is a fixed-window counter per key, in process memory. Each key's
// window starts at its first hit and lasts WindowMs; the hit that exceeds
// Limit within it, and every one after, is refused. Every counted response
// carries the IETF draft-7 headers
//
//	RateLimit-Policy: <limit>;w=<window seconds>
//	RateLimit: limit=<limit>, remaining=<n>, reset=<seconds>
//
// and a refused one also gets Retry-After. Expired windows are swept every
// window, so an address that stops calling costs nothing after one window.
type Limiter struct {
	opts          Options
	windowSeconds float64
	mu            sync.Mutex
	windows       map[string]*window
	stop          chan struct{}
	stopOnce      sync.Once
}

// New builds a limiter and starts its sweeper.
func New(opts Options) *Limiter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	l := &Limiter{opts: opts, windowSeconds: math.Ceil(opts.WindowMs / 1000), windows: map[string]*window{}, stop: make(chan struct{})}
	if !opts.NoSweep && opts.WindowMs > 0 {
		go l.sweepLoop(time.Duration(opts.WindowMs * float64(time.Millisecond)))
	}
	return l
}

func (l *Limiter) sweepLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.Sweep()
		case <-l.stop:
			return
		}
	}
}

func (l *Limiter) nowMs() float64 { return float64(l.opts.Now().UnixMilli()) }

// Sweep forgets every expired window.
func (l *Limiter) Sweep() {
	at := l.nowMs()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, w := range l.windows {
		if w.resetAt <= at {
			delete(l.windows, k)
		}
	}
}

// Stop ends the sweeper.
func (l *Limiter) Stop() { l.stopOnce.Do(func() { close(l.stop) }) }

// ResetKey forgets one key's window.
func (l *Limiter) ResetKey(key string) {
	l.mu.Lock()
	delete(l.windows, key)
	l.mu.Unlock()
}

// Size is the number of windows held.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.windows)
}

// Limit is the configured request budget per window.
func (l *Limiter) Limit() float64 { return l.opts.Limit }

func (l *Limiter) key(r *http.Request) (string, error) {
	if l.opts.Key != nil {
		return l.opts.Key(r)
	}
	ip := ""
	if l.opts.ClientIP != nil {
		ip = l.opts.ClientIP(r)
	} else {
		ip = clientip.RemoteAddr(r)
	}
	return IPKey(ip, 56), nil
}

// Hit counts r against its key and sets the RateLimit headers on w. It
// returns false when r is over the limit (Retry-After is then set too) or
// when the key could not be computed (err is non-nil).
func (l *Limiter) Hit(w http.ResponseWriter, r *http.Request) (bool, error) {
	key, err := l.key(r)
	if err != nil {
		return false, err
	}
	at := l.nowMs()
	l.mu.Lock()
	entry := l.windows[key]
	if entry == nil || entry.resetAt <= at {
		entry = &window{resetAt: at + l.opts.WindowMs}
		l.windows[key] = entry
	}
	entry.hits++
	hits, resetAt := entry.hits, entry.resetAt
	l.mu.Unlock()

	remaining := math.Max(l.opts.Limit-hits, 0)
	reset := math.Max(0, math.Ceil((resetAt-l.nowMs())/1000))
	h := w.Header()
	h.Set("RateLimit-Policy", jsjson.FormatNumber(l.opts.Limit)+";w="+jsjson.FormatNumber(l.windowSeconds))
	h.Set("RateLimit", "limit="+jsjson.FormatNumber(l.opts.Limit)+", remaining="+jsjson.FormatNumber(remaining)+", reset="+jsjson.FormatNumber(reset))
	if hits > l.opts.Limit {
		h.Set("Retry-After", jsjson.FormatNumber(reset))
		return false, nil
	}
	return true, nil
}

// Middleware counts each request and either passes it on or answers it with
// the Exceeded handler.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := l.Hit(w, r)
		switch {
		case err != nil:
			if l.opts.OnError != nil {
				l.opts.OnError(w, r, err)
				return
			}
			slog.Error("rate limit key failed", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		case !ok:
			exceeded := l.opts.Exceeded
			if exceeded == nil {
				exceeded = DefaultExceeded
			}
			exceeded.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// IPKey is the client key for an address (express-rate-limit 8's
// ipKeyGenerator). IPv4 is used as is. An IPv6 client is keyed by its
// /subnet network: an attacker with an ordinary IPv6 allocation could
// otherwise reset a brute-force counter for free by moving to the next
// address in their own /64. IPv4-mapped (::ffff:a.b.c.d) and IPv4-compatible
// (::a.b.c.d, written dotted) addresses key as the IPv4 address they carry,
// so a dual-stack socket and an IPv4 one share a budget. subnet 0 keys the
// address itself.
func IPKey(ip string, subnet int) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() {
		return ip
	}
	raw := addr.As16()
	var words [8]uint16
	for i := range words {
		words[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
	}
	zeroPrefix := words[0] == 0 && words[1] == 0 && words[2] == 0 && words[3] == 0 && words[4] == 0
	dotted := strings.Contains(strings.SplitN(ip, "%", 2)[0], ".")
	if zeroPrefix && (words[5] == 0xFFFF || (dotted && words[5] == 0)) {
		return strconv.Itoa(int(words[6]>>8)) + "." + strconv.Itoa(int(words[6]&0xFF)) + "." +
			strconv.Itoa(int(words[7]>>8)) + "." + strconv.Itoa(int(words[7]&0xFF))
	}
	if subnet == 0 {
		return ip
	}
	var network [8]uint16
	for i, word := range words {
		keep := max(0, min(16, subnet-i*16))
		if keep > 0 {
			network[i] = word & uint16((0xFFFF<<(16-keep))&0xFFFF)
		}
	}
	return ipv6Text(network) + "/" + strconv.Itoa(subnet)
}

// ipv6Text is the RFC 5952 text form: lower-case hex, no leading zeros, the
// longest run of two or more zero words (the first, on a tie) as "::".
func ipv6Text(words [8]uint16) string {
	best, bestLen := -1, 1
	for i := 0; i < 8; {
		if words[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && words[j] == 0 {
			j++
		}
		if j-i > bestLen {
			best, bestLen = i, j-i
		}
		i = j
	}
	hex := func(ws []uint16) string {
		parts := make([]string, len(ws))
		for i, w := range ws {
			parts[i] = strconv.FormatUint(uint64(w), 16)
		}
		return strings.Join(parts, ":")
	}
	if best < 0 {
		return hex(words[:])
	}
	return hex(words[:best]) + "::" + hex(words[best+bestLen:])
}

// SessionKey is the signout limiter's key for a session cookie value: its
// hash, never the raw token.
func SessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "session:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// ── responses ──────────────────────────────────────────────────────────────

// WriteJSON answers as Express's res.status(status).json(body) did, for a
// body already serialized: JSON with charset, weak ETag, Content-Length.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	mcpproto.WriteExpressJSON(w, status, body)
}

// DefaultExceeded is the JSON 429 of the API-style limiters.
var DefaultExceeded http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusTooManyRequests, []byte(`{"error":"rate limit exceeded — slow down","code":"rate_limited"}`))
})

// MCPExceeded is the MCP endpoint's 429: a JSON-RPC error.
var MCPExceeded http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusTooManyRequests, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Rate limit exceeded; retry later"}}`))
})

// ── the named limiters ─────────────────────────────────────────────────────

const minuteMs = 60 * 1000

// envNumber is lib/ratelimit.js N(): the variable as a positive finite
// JavaScript number, else def.
func envNumber(name string, def float64) float64 {
	n := jsjson.Number(env.String(name, ""))
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return def
	}
	return n
}

// Config is what a caller supplies to a named limiter.
type Config struct {
	// ClientIP resolves the client address (clientip.Policy.ClientIP).
	ClientIP func(r *http.Request) string
	// Exceeded answers refused requests. The API-style limiters default to
	// their JSON 429; the HTML ones (content, password, sign-in) need the
	// owning package's page and fall back to the JSON only when it is nil.
	Exceeded http.Handler
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
}

func (c Config) build(windowMs, limit float64, def http.Handler) *Limiter {
	exceeded := c.Exceeded
	if exceeded == nil {
		exceeded = def
	}
	return New(Options{WindowMs: windowMs, Limit: limit, Exceeded: exceeded, ClientIP: c.ClientIP, Now: c.Now})
}

// NewAPI guards the agent REST surface: RL_API_PER_MIN (120) per minute.
// Generous enough for normal deploys, low enough that a runaway agent can't
// melt the box.
func NewAPI(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_API_PER_MIN", 120), DefaultExceeded)
}

// NewMCP guards /mcp: RL_MCP_PER_MIN (120) per minute, JSON-RPC 429.
func NewMCP(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_MCP_PER_MIN", 120), MCPExceeded)
}

// NewContent guards the content host's reads (/raw and direct serve):
// RL_CONTENT_PER_MIN (240) per minute. Higher ceiling — legitimate viewers
// reload — but still a backstop against scraping/DoS of untrusted pages.
// Every request it refuses is a person or an iframe loading a dashboard, so
// Exceeded should be the content zone's HTML busy page (contentview), with
// the framable gate headers (the limiter also guards /raw, which the /admin
// preview frames) and Retry-After: 60.
func NewContent(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_CONTENT_PER_MIN", 240), DefaultExceeded)
}

// PasswordWindowMinutes is RL_PASSWORD_WINDOW_MIN (15): the password-form
// window, which its HTML 429 states and sends as Retry-After (minutes*60).
func PasswordWindowMinutes() float64 { return envNumber("RL_PASSWORD_WINDOW_MIN", 15) }

// NewPassword is the per-page password form's brute-force guard:
// RL_PASSWORD_TRIES (20) per RL_PASSWORD_WINDOW_MIN (15) minutes. Exceeded
// should be the rate-limit page (contentview) with gate headers and
// Retry-After: PasswordWindowMinutes()*60.
func NewPassword(c Config) *Limiter {
	return c.build(PasswordWindowMinutes()*minuteMs, envNumber("RL_PASSWORD_TRIES", 20), DefaultExceeded)
}

// Central sign-in (PAGES_AUTH_MODE=central). Each stage has its own budget,
// so pressure on one cannot block another: a script looping /auth/login from
// a shared office or VPN address must not starve a callback already in
// flight, nor stop a signed-in person from signing out. Their Exceeded is the
// HTML "Too many sign-in attempts" page (Retry-After: 60, Cache-Control:
// no-store), because a browser is always the caller.

// NewSignin guards /auth/login (a session lookup and a sealed cookie each):
// RL_SIGNIN_PER_MIN (60).
func NewSignin(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_SIGNIN_PER_MIN", 60), DefaultExceeded)
}

// NewSigninCallback guards /auth/callback (a server-to-server call to Auth's
// /token each): RL_SIGNIN_CALLBACK_PER_MIN (60).
func NewSigninCallback(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_SIGNIN_CALLBACK_PER_MIN", 60), DefaultExceeded)
}

// NewSignout guards /logout: RL_SIGNOUT_PER_MIN (30). It is keyed by the
// session cookie (hashed, never stored raw) when one is sent, so another
// client on the same address cannot spend a signed-in person's budget;
// requests without one share the address's budget. A forged cookie earns its
// sender a fresh key — one primary-key UPDATE, no more than the unlimited
// session lookup on any /admin request — accepted: an address-wide gate in
// front of it would let anyone on that address block a real sign-out.
// sessionToken returns the request's session cookie value ("" when none).
func NewSignout(c Config, sessionToken func(r *http.Request) string) *Limiter {
	exceeded := c.Exceeded
	if exceeded == nil {
		exceeded = DefaultExceeded
	}
	clientIP := c.ClientIP
	return New(Options{
		WindowMs: minuteMs,
		Limit:    envNumber("RL_SIGNOUT_PER_MIN", 30),
		Exceeded: exceeded,
		Now:      c.Now,
		Key: func(r *http.Request) (string, error) {
			if sessionToken != nil {
				if token := sessionToken(r); token != "" {
					return SessionKey(token), nil
				}
			}
			ip := clientip.RemoteAddr(r)
			if clientIP != nil {
				ip = clientIP(r)
			}
			return IPKey(ip, 56), nil
		},
	})
}

// NewBackchannel guards /auth/backchannel-logout (Auth's signed logout and
// access events): RL_BACKCHANNEL_PER_MIN (600), its own budget so agent API
// traffic from the same address cannot delay a revocation. Generous for a
// provisioning burst; a 429 is retried by Auth.
func NewBackchannel(c Config) *Limiter {
	return c.build(minuteMs, envNumber("RL_BACKCHANNEL_PER_MIN", 600), DefaultExceeded)
}
