// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/ratelimit.js — per-surface rate limits (PLAN §7/§9). A single small box that
// serves untrusted content and runs an agent deploy loop is trivially DoS'd, and a
// stuck agent will hammer the deploy/MCP endpoints — so cap the hot paths.
//
// Limits are tuned per surface and overridable by env (RL_*). Keying is by client
// IP, using the same one-hop proxy policy on both inner apps in server.js. The
// default trusts a local Caddy peer; PAGES_TRUST_PROXY configures other explicit
// proxy addresses or disables forwarding for direct access. Earlier forwarded
// hops are never used. Returns JSON 429 except content reads and the password
// form, which get HTML so a human sees something sane.

const crypto = require("node:crypto");
const { isIPv6 } = require("node:net");
const { gateHeaders } = require("./csp");
const { rateLimitPage, busyPage } = require("./contentview");
const standaloneChrome = require("./standalone-chrome");

const N = (v, d) => (Number.isFinite(Number(v)) && Number(v) > 0 ? Number(v) : d);
const MIN = 60 * 1000;

// IPv6 address -> its eight 16-bit words. `ip` has already passed isIPv6, so
// the grammar here only needs to cover valid input: an optional zone id, one
// optional "::", and an optional dotted-quad tail. `dotted` records whether
// the address was WRITTEN with that tail, which is what tells the deprecated
// IPv4-compatible form (::1.2.3.4) apart from ::1 below.
function ipv6Words(ip) {
  let address = ip.split("%", 1)[0];
  let dotted = false;
  if (address.includes(".")) {
    const cut = address.lastIndexOf(":") + 1;
    const [a, b, c, d] = address.slice(cut).split(".").map(Number);
    address = `${address.slice(0, cut)}${((a << 8) | b).toString(16)}:${((c << 8) | d).toString(16)}`;
    dotted = true;
  }
  const [head, tail] = address.split("::");
  const left = head ? head.split(":") : [];
  const right = tail ? tail.split(":") : [];
  const fill = tail === undefined ? [] : new Array(8 - left.length - right.length).fill("0");
  return { words: [...left, ...fill, ...right].map((word) => parseInt(word, 16)), dotted };
}

// RFC 5952 text form: lower-case hex, no leading zeros, the longest run of two
// or more zero words (the first, on a tie) collapsed to "::".
function ipv6Text(words) {
  let best = -1;
  let bestLength = 1;
  for (let i = 0; i < 8; ) {
    if (words[i] !== 0) {
      i += 1;
      continue;
    }
    let j = i;
    while (j < 8 && words[j] === 0) j += 1;
    if (j - i > bestLength) {
      best = i;
      bestLength = j - i;
    }
    i = j;
  }
  const hex = words.map((word) => word.toString(16));
  if (best < 0) return hex.join(":");
  return `${hex.slice(0, best).join(":")}::${hex.slice(best + bestLength).join(":")}`;
}

// ipKeyGenerator — the client key for an address. IPv4 is used as is. An IPv6
// client is keyed by its /56 network, the same policy express-rate-limit 8
// applied: an attacker with an ordinary IPv6 allocation could otherwise reset
// the brute-force counter for free by moving to the next address in their own
// /64. IPv4-mapped (::ffff:a.b.c.d) and IPv4-compatible (::a.b.c.d) addresses
// key as the IPv4 address they carry, so a dual-stack socket and an IPv4 one
// share a budget.
function ipKeyGenerator(ip, ipv6Subnet = 56) {
  if (!isIPv6(ip)) return ip;
  const { words, dotted } = ipv6Words(ip);
  const zeroPrefix = words.slice(0, 5).every((word) => word === 0);
  if ((zeroPrefix && words[5] === 0xffff) || (dotted && zeroPrefix && words[5] === 0)) {
    return [words[6] >> 8, words[6] & 0xff, words[7] >> 8, words[7] & 0xff].join(".");
  }
  if (!ipv6Subnet) return ip;
  const network = words.map((word, index) => {
    const keep = Math.max(0, Math.min(16, ipv6Subnet - index * 16));
    return keep === 0 ? 0 : word & ((0xffff << (16 - keep)) & 0xffff);
  });
  return `${ipv6Text(network)}/${ipv6Subnet}`;
}

// createLimiter — a fixed-window counter per key, in process memory. Each key's
// window starts at its first hit and lasts windowMs; the hit that exceeds
// `limit` within it, and every one after, goes to `handler` instead of next().
// Every counted response carries the IETF draft-7 headers
//   RateLimit-Policy: <limit>;w=<window seconds>
//   RateLimit: limit=<limit>, remaining=<n>, reset=<seconds>
// and a refused one also gets Retry-After (a handler may override it).
// Expired windows are swept on an unref'd timer, so an address that stops
// calling costs nothing after one window and the timer never holds the
// process open. `now` is injectable for tests.
function createLimiter({ windowMs, limit, handler, keyGenerator, now = Date.now }) {
  const windows = new Map();
  const windowSeconds = Math.ceil(windowMs / 1000);
  const sweep = setInterval(() => {
    const at = now();
    for (const [key, entry] of windows) if (entry.resetAt <= at) windows.delete(key);
  }, windowMs);
  sweep.unref?.();

  async function count(req, res, next) {
    const key = keyGenerator ? await keyGenerator(req, res) : ipKeyGenerator(req.ip);
    const at = now();
    let entry = windows.get(key);
    if (!entry || entry.resetAt <= at) {
      entry = { hits: 0, resetAt: at + windowMs };
      windows.set(key, entry);
    }
    entry.hits += 1;
    const remaining = Math.max(limit - entry.hits, 0);
    const reset = Math.max(0, Math.ceil((entry.resetAt - now()) / 1000));
    if (!res.headersSent) {
      res.setHeader("RateLimit-Policy", `${limit};w=${windowSeconds}`);
      res.setHeader("RateLimit", `limit=${limit}, remaining=${remaining}, reset=${reset}`);
    }
    if (entry.hits > limit) {
      if (!res.headersSent) res.setHeader("Retry-After", String(reset));
      return handler(req, res, next);
    }
    return next();
  }

  const middleware = (req, res, next) => {
    count(req, res, next).catch(next);
  };
  middleware.resetKey = (key) => windows.delete(key);
  middleware.stop = () => clearInterval(sweep);
  Object.defineProperty(middleware, "size", { get: () => windows.size });
  return middleware;
}

function make({ windowMs, limit, handler, keyGenerator }) {
  return createLimiter({
    windowMs,
    limit,
    keyGenerator,
    handler:
      handler ||
      ((req, res) => res.status(429).json({ error: "rate limit exceeded — slow down", code: "rate_limited" })),
  });
}

// Agent surfaces: REST + MCP. Generous enough for normal deploys, low enough that a
// runaway agent can't melt the box.
const api = make({ windowMs: MIN, limit: N(process.env.RL_API_PER_MIN, 120) });
const mcp = make({
  windowMs: MIN,
  limit: N(process.env.RL_MCP_PER_MIN, 120),
  handler: (_req, res) =>
    res.status(429).json({
      jsonrpc: "2.0",
      id: null,
      error: { code: -32000, message: "Rate limit exceeded; retry later" },
    }),
});

// Content host read path (/raw + direct-serve). Higher ceiling — legitimate viewers
// reload — but still a backstop against scraping/DoS of untrusted pages.
//
// HTML, not the default JSON: every request this limiter rejects is a person or
// an iframe loading a client's dashboard, never an API client. It used to answer
// a client navigation with a raw `{"error":…}` body and none of the content
// zone's headers, so a client reloading their own page was shown a JSON blob.
const content = make({
  windowMs: MIN,
  limit: N(process.env.RL_CONTENT_PER_MIN, 240),
  // framable: this limiter also guards /raw, which the /admin preview frames.
  // A frame-ancestors 'none' rejection would answer an operator with a blank
  // preview instead of the explanation.
  handler: (req, res) => res
    .status(429)
    .set(gateHeaders({ framable: true }))
    .set("Retry-After", "60")
    .type("html")
    .send(busyPage({ slug: String(req.path || "").replace(/^\/+/, "") })),
});

// Password form: brute-force guard. Strict, longer window, HTML 429.
const password = make({
  windowMs: N(process.env.RL_PASSWORD_WINDOW_MIN, 15) * MIN,
  limit: N(process.env.RL_PASSWORD_TRIES, 20),
  handler: (req, res) => {
    // req was discarded, so the page could neither link back nor say the real
    // wait, and no Retry-After reached a client that would have honoured it.
    const minutes = N(process.env.RL_PASSWORD_WINDOW_MIN, 15);
    const slug = String(req.path || "").replace(/^\/+/, "");
    res
      .status(429)
      .set(gateHeaders())
      .set("Retry-After", String(minutes * 60))
      .type("html")
      .send(rateLimitPage({ slug, minutes }));
  },
});

// Central sign-in (PAGES_AUTH_MODE=central). Each stage has its own budget,
// so pressure on one cannot block another: a script looping /auth/login from
// a shared office or VPN address must not starve a callback that is already
// in flight, nor stop a signed-in person from signing out. HTML 429, because
// a browser is always the caller.
function signinBusy(_req, res) {
  res
    .status(429)
    .set("Retry-After", "60")
    .set("Cache-Control", "no-store")
    .type("html")
    .send(
      standaloneChrome.render({
        title: "Sign-in · Pages",
        kicker: "Sign-in",
        heading: "Too many sign-in attempts",
        bodyHtml: "<p>Wait a minute, then try again.</p>",
        assetsBase: "/shell-assets/flag",
      })
    );
}

// /auth/login: starting a handoff costs a session lookup and a sealed cookie.
const signin = make({ windowMs: MIN, limit: N(process.env.RL_SIGNIN_PER_MIN, 60), handler: signinBusy });

// /auth/callback: each one costs a server-to-server call to Auth's /token.
const signinCallback = make({ windowMs: MIN, limit: N(process.env.RL_SIGNIN_CALLBACK_PER_MIN, 60), handler: signinBusy });

// /logout: keyed by the session cookie (hashed, never stored raw) when one is
// sent, so another client on the same address cannot spend a signed-in
// person's budget. Requests without one share the address's budget. A forged
// cookie earns its sender a fresh key: one primary-key UPDATE, no more than
// the unlimited session lookup on any /admin request, plus a small counter
// that the store forgets within two windows. Accepted: an address-wide gate
// in front of it would let anyone on that address block a real sign-out.
const signout = make({
  windowMs: MIN,
  limit: N(process.env.RL_SIGNOUT_PER_MIN, 30),
  handler: signinBusy,
  keyGenerator: (req) => {
    const auth = require("./auth");
    const token = auth.central && auth.parseCookies(req.headers.cookie)[auth.central.sessionCookie];
    return token ? `session:${crypto.createHash("sha256").update(token).digest("base64url")}` : ipKeyGenerator(req.ip);
  },
});

// /auth/backchannel-logout: Auth's signed logout and access events. Its own
// budget, so agent API traffic from the same address cannot delay a
// revocation. Generous for a provisioning burst; a 429 is retried by Auth.
const backchannel = make({ windowMs: MIN, limit: N(process.env.RL_BACKCHANNEL_PER_MIN, 600) });

module.exports = {
  api,
  mcp,
  content,
  password,
  signin,
  signinCallback,
  signout,
  backchannel,
  // Exported for tests.
  createLimiter,
  ipKeyGenerator,
};
