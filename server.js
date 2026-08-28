// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// server.js — Elcano Pages. ONE Express process, TWO registrable domains
// (see PLAN.md §3, §7):
//
//   DASHBOARD_HOST  — trusted auth zone: /view, /admin shells, /api/v1, /mcp.
//                     Reads the Elcano SSO cookie; admins are @elcanotek.com.
//   CONTENT_HOST    — separate registrable domain, COOKIELESS: /raw, /assets.
//                     Renders untrusted agent HTML, sandboxed, never touches
//                     the SSO cookie. This origin boundary is the security
//                     model — a shared parent domain would NOT be safe.
//
// Caddy terminates TLS for both names and proxies to this one process; we
// branch on the Host header. This is Phase 0: the skeleton stands up both
// hosts, wires auth + CSP, and stubs the routes Phases 1-4 fill in.

const path = require("node:path");
const express = require("express");
const helmet = require("helmet");

const auth = require("./lib/auth");
const { shellHelmetOptions, contentBaseHeaders, rawHeaders, CONTENT_ORIGIN } = require("./lib/csp");
const rawtoken = require("./lib/rawtoken");
const render = require("./lib/render");
const db = require("./lib/db");
const versions = require("./lib/versions");
const templates = require("./lib/templates");
const contentview = require("./lib/contentview");
const apiRouter = require("./lib/api");
const adminApiRouter = require("./lib/adminapi");
const adminShell = require("./lib/adminshell");
const templateShell = require("./lib/templateshell");
const portalShell = require("./lib/portalshell");
const welcomeShell = require("./lib/welcomeshell");
const errorShell = require("./lib/errorshell");
const compose = require("./lib/compose"); // DEV-only "compose with Cutlass" panel (gated)
const csrf = require("./lib/csrf");
const mcp = require("./lib/mcp");
const uploadTicket = require("./lib/uploadticket");
const limits = require("./lib/ratelimit");

const PORT = Number(process.env.PORT || 3002);
// Hostnames are configured per deployment via /etc/default/pages (set by
// bootstrap.sh). These fallbacks are just Elcano's defaults for local dev.
const DASHBOARD_HOST = (process.env.DASHBOARD_HOST || "pages.elcanotek.com").toLowerCase();
const CONTENT_HOST = (process.env.CONTENT_HOST || "elcano-pages.com").toLowerCase();

const app = express();
app.set("trust proxy", 1);
app.disable("x-powered-by");

// ── Host classification ─────────────────────────────────────────────────
// req.hostname is derived from the Host header (trust proxy set). In local
// dev both names may resolve to localhost; CONTENT_HOST_ALSO lets you treat
// an extra hostname (e.g. "localhost") as the content host for testing.
const CONTENT_ALIASES = new Set(
  [CONTENT_HOST, process.env.CONTENT_HOST_ALSO]
    .filter(Boolean)
    .map((h) => h.toLowerCase())
);
function isContentHost(req) {
  return CONTENT_ALIASES.has(String(req.hostname || "").toLowerCase());
}

// ── Routers ───────────────────────────────────────────────────────────────
const contentApp = express();
const dashboardApp = express();

// ===========================================================================
// CONTENT HOST (cookieless, untrusted render zone)
// ===========================================================================
// No helmet shell CSP here — we set the strict sandbox headers by hand so the
// `sandbox` directive binds even on a direct navigation. NEVER read cookies.
//
// contentApp is a SEPARATE Express app, so the parent app's settings never reach
// it: `x-powered-by` has to be disabled here too (the dashboard was only clean
// because helmet strips it), and the zone's header floor is applied as the first
// middleware so EVERY response carries it — including the ones no route of ours
// produces: the 301 express.static emits for a directory (its `setHeaders` hook
// runs only on the file path), a Set-Cookie redirect, /healthz, and anything the
// error handler catches. Routes that render layer rawHeaders()/gateHeaders() on
// top; express.static replaces the floor's Cache-Control with its revalidating
// default, which is what we want for a static file.
contentApp.disable("x-powered-by");
contentApp.use((_req, res, next) => {
  res.set(contentBaseHeaders());
  next();
});

contentApp.get("/healthz", (_req, res) => res.type("text").send("ok"));

// Vendored Flag tokens/fonts/icons live under public/assets and are served from
// the content host so /raw pages can reference them CSP-clean.
//
// These carry the content zone's header contract too. They are first-party
// static files, so the risk today is small — but "every response this host emits
// is hardened" is only checkable if there are no exceptions, and an exception
// that exists because a middleware was mounted with different options is
// invisible. `sandbox` is deliberately NOT among them: it would apply only to a
// direct navigation (CSP is per-document, ignored on a subresource fetch) and
// sandboxing a stylesheet document buys nothing, while the directive on a
// subresource response is pure noise for anyone reading headers.
//
// Cache-Control is left at express.static's revalidating default ON PURPOSE.
// PLAN §7 assumed content-hashed filenames (`/assets/echarts.<ver>.<hash>.js`);
// what actually shipped is stable paths like `/assets/flag/tokens/design-tokens.css`,
// which `scripts/sync-flag.sh` overwrites in place. Marking those immutable would
// pin every client to the tokens they first loaded, and a Flag sync would never
// reach them.
const ASSET_CSP =
  "default-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; base-uri 'none'; form-action 'none'";
contentApp.use(
  "/assets",
  express.static(path.join(__dirname, "public", "assets"), {
    index: false,
    setHeaders(res) {
      // The floor already set Referrer-Policy/nosniff/X-Robots-Tag. Only the CSP
      // needs widening, and only for a DIRECT navigation to an asset: a CSP on a
      // response fetched as a subresource does not govern it. That still matters
      // here because `/assets/flag/logos/*.svg` are documents, and SVG can carry
      // script — `default-src 'none'` keeps a navigated one inert while the
      // stylesheet and its @font-face resolve.
      res.setHeader("Content-Security-Policy", ASSET_CSP);
      // Cache-Control must be set EXPLICITLY, not left to express.static: `send`
      // only supplies its own when the header is absent, and the floor has
      // already set `no-store`. Leaving it would make every dashboard load
      // re-download the IBM Plex Sans woff2 files and the token sheet instead of
      // revalidating them — a real cost on a client's page, from a header nobody
      // chose. `max-age=0` is cacheable-but-revalidate: browsers keep the bytes
      // and get a 304, which is what these stable, overwritten-in-place paths
      // want. Not `immutable`: sync-flag.sh replaces them under the same URL, so
      // immutable would pin every client to whatever it first loaded.
      res.setHeader("Cache-Control", "public, max-age=0");
    },
  })
);

// /raw/<slug> — the sandboxed render target. The signed token is the ONLY
// authorization here (no cookies on this host). It binds the exact version and
// a single audience, so it cannot be replayed for another version or redeemed
// as a page credential (PLAN §6d).
// Headers (sandbox CSP etc.) are set on EVERY path, including errors.
// `*slug` (multi-segment splat) so nested slugs like northwind/q2 resolve here
// instead of falling through to the direct-serve wildcard.
contentApp.get("/raw/*slug", limits.content, async (req, res) => {
  res.set(rawHeaders());
  const slug = req.params.slug.join("/");
  const claims = rawtoken.verify(req.query.t);
  // Allow-list, not deny-list: this route renders one page version, so the only
  // purpose it accepts is "view". A "template" token names a
  // page_template_versions id — a different id space, which would only ever
  // match a page version by accident — and a "session" token is a page
  // credential, not a render credential. Stating what is accepted means a
  // purpose added later is refused here by default instead of inheriting access.
  if (!claims || claims.purpose !== "view") {
    return res.status(403).type("html").send(contentview.expiredLinkPage());
  }
  try {
    const v = await db.getRenderable(claims.vid);
    // The token's version must match what we loaded, belong to this slug, and
    // the page must not be disabled (takedown switch). pg returns BIGINT as a
    // string, so coerce before comparing to the numeric token claim.
    if (!v || Number(v.id) !== Number(claims.vid) || v.slug !== slug || v.disabled) {
      return res.status(404).type("html").send(contentview.notFoundPage());
    }
    res.type("html").send(render.renderVersion(v));
  } catch (err) {
    console.error("render error:", err.message);
    res.status(500).type("html").send(contentview.serverErrorPage());
  }
});

// /raw-template/<template_version_id> — preview a REGISTERED template revision.
// On the content host, under the same sandbox+CSP a page gets, because a template
// is the same untrusted HTML: rendering it in the dashboard origin would put agent
// markup inside the trusted auth zone. Authorization is the signed token alone
// (no cookies here), bound to this exact revision with purpose "template".
contentApp.get("/raw-template/:id", limits.content, async (req, res) => {
  res.set(rawHeaders());
  const claims = rawtoken.verify(req.query.t);
  if (!claims || claims.purpose !== "template" || Number(claims.vid) !== Number(req.params.id)) {
    return res.status(403).type("html").send(contentview.expiredLinkPage());
  }
  try {
    // Not the stored bytes: the template MATERIALIZED, so the example block is
    // gone and an example dataset (when the revision has one) is in #pages-data.
    // A preview therefore shows the design populated, and shows exactly the shape
    // a page built from it would have.
    const preview = await templates.previewBytes(claims.vid);
    // The token carries the mode the previewer asked for, so a themed template is
    // previewed the way create_page_from_template would render it by default.
    res.type("html").send(render.renderVersion({ html: preview.html, render_mode: claims.mode, override_css: "" }));
  } catch (err) {
    if (err && err.status === 404) {
      return res.status(404).type("html").send(contentview.notFoundPage());
    }
    console.error("template preview error:", err.message);
    res.status(500).type("html").send(contentview.serverErrorPage());
  }
});

// The browser asks for this unprompted on every page a partner opens. Without a
// route it fell to the slug wildcard below, where slugFromReq rejects the dot and
// a full HTML 404 is rendered and thrown away — and the tab shows a blank icon.
// Declared before the wildcard, because the wildcard would otherwise absorb it.
contentApp.get("/favicon.ico", limits.content, (_req, res) =>
  res.redirect(301, "/assets/flag/logos/elcano-mark-favicon.svg"));

// Direct-serve the live client page (PLAN §6b). elcano-pages.com/<slug> is the
// client URL: the content host owns the per-page password gate (its own cookie
// jar, never the SSO cookie) and renders the published version with the sandbox
// CSP. Registered LAST so /healthz, /assets, /raw win first. POST handles the
// password form (the only place the content host parses a body).
contentApp.get("/{*slug}", limits.content, (req, res, next) => contentview.serve(req, res).catch(next));
contentApp.post(
  "/{*slug}",
  limits.password, // brute-force guard on the per-page password
  express.urlencoded({ extended: false, limit: "1kb" }),
  (req, res, next) => contentview.unlock(req, res).catch(next)
);

// Terminal 404 for anything the routes above did not claim — in practice a
// method other than GET/POST, since both wildcards catch every path. It used to
// answer `text/plain "not found"` with none of the zone's headers, which is the
// one shape the "every response is hardened" rule cannot survive. Same body as
// the routed 404 — not the same headers: the routed path runs the read limiter,
// so it also carries RateLimit-*.
contentApp.use((_req, res) => res.status(404).set(rawHeaders()).type("html").send(contentview.notFoundPage()));

// Content-host error handler. Without one, a rejected promise fell through to
// Express's default handler: an unbranded error document with none of this
// zone's headers, and — outside NODE_ENV=production — the stack trace, on a
// public client-facing URL. Registered last so it catches every route above.
contentApp.use((err, _req, res, next) => {
  console.error("content host error:", err && err.message);
  // Once the response has started there is nothing left to render — hand back to
  // Express's finalhandler, which destroys the socket. Returning silently ends
  // neither the response nor the connection, so the client would hang until TCP
  // timeout.
  if (res.headersSent) return next(err);
  // Preserve a client-error status instead of flattening everything to 500.
  // express.static forwards 416 (unsatisfiable Range) and 412 (failed
  // precondition) for a file it already found, and the body parser forwards 413;
  // a CDN or a resumable download is entitled to those, and a 500 invites a
  // retry loop for a request that can never succeed.
  const status = Number(err && (err.status || err.statusCode));
  res
    .status(status >= 400 && status < 500 ? status : 500)
    .set(rawHeaders())
    .type("html")
    .send(contentview.serverErrorPage());
});

// ===========================================================================
// DASHBOARD HOST (trusted auth zone)
// ===========================================================================
dashboardApp.use(helmet(shellHelmetOptions(auth.AUTH_ORIGIN)));

// MCP is its own remote-protocol security boundary. Mount it BEFORE the broad
// dashboard body parsers so Host/Origin validation, rate limiting, and bearer
// auth all happen before an untrusted HTML-sized body is parsed. lib/mcp.js
// owns its strict JSON parser and JSON-RPC error mapping.
dashboardApp.use("/mcp", mcp.router);

// Rate limits on the agent surfaces (PLAN §7/§9). Registered BEFORE the broad
// body parsers — like the MCP boundary above — so an unauthenticated client
// cannot make the server parse a max-size (2 MB) body on every request before
// the per-IP limiter rejects it. Covers /api/v1 and /api/v1/admin(/compose)
// with the identical limiter and 429 shape as before; only the work order
// changes (over-quota requests are rejected pre-parse).
dashboardApp.use("/api/v1", limits.api);

// Out-of-band page-content upload. Mounted here for the same reason as /mcp
// above: it owns its own raw body parser and its own (ticket, not agent-token)
// auth, so it must sit ahead of the broad JSON/urlencoded parsers and behind
// the same per-IP limiter. See lib/uploadticket.js for why this credential is
// safe to hand to an agent's sandbox.
dashboardApp.use("/upload", limits.api, uploadTicket.router);

dashboardApp.use(express.urlencoded({ extended: false, limit: "64kb" }));
// JSON body cap = the per-version HTML ceiling (PLAN §7: HTML ≤ ~1–2 MB).
dashboardApp.use(express.json({ limit: process.env.MAX_HTML_BYTES || "2mb" }));

dashboardApp.get("/healthz", (_req, res) => res.type("text").send("ok"));

// Public shell assets (CSS/JS for the trusted /view + /admin UI).
dashboardApp.use(
  "/shell-assets",
  express.static(path.join(__dirname, "public", "shell-assets"), { index: false })
);
// The shell is Flag-themed, so serve the vendored Flag tokens/fonts/theme on the
// dashboard host too (the content host serves its own copy at /assets/flag for
// rendered pages). Same files, two zones.
dashboardApp.use(
  "/shell-assets/flag",
  express.static(path.join(__dirname, "public", "assets", "flag"), { index: false })
);

// Local dev-login (GATED). The real auth service mints the elcano_auth cookie;
// locally scripts/dev.sh mints a dev one and exposes it here so /admin works in
// a browser without auth.elcanotek.com. Mounted ONLY when PAGES_DEV_LOGIN=1 and
// a cookie is provided — never enable in production.
if (process.env.PAGES_DEV_LOGIN === "1" && process.env.DEV_ADMIN_COOKIE) {
  const cookieName = process.env.AUTH_COOKIE_NAME || "elcano_auth";
  console.warn("⚠ DEV LOGIN ENABLED: GET /__dev/login sets a local admin cookie. Do NOT set PAGES_DEV_LOGIN=1 in production.");

  // Keep auth-failure redirects on the SAME host (relative), so localhost,
  // 127.0.0.1, or a VM IP all work without bouncing to a pinned host or the
  // real auth. Runs before the /admin + /view routes (registered later), so a
  // no-session GET is intercepted here instead of by requireAdmin/requireAuth.
  dashboardApp.use((req, res, next) => {
    if (req.method === "GET" && /^\/(admin|view)\//.test(req.path) && !auth.currentSession(req)) {
      return res.redirect(302, `/__dev/login?next=${encodeURIComponent(req.originalUrl)}`);
    }
    next();
  });
  dashboardApp.get("/__dev/login", (req, res) => {
    res.setHeader("Set-Cookie", `${cookieName}=${process.env.DEV_ADMIN_COOKIE}; Path=/; HttpOnly; SameSite=Lax`);
    // Accept `next` (our own param) or `return_to` (what auth.loginRedirectURL
    // sends when requireAdmin bounces here). Always reduce to a LOCAL path so a
    // no-cookie /admin visit round-trips through here and back — never to the
    // real auth/home. Open-redirect-safe: we keep only the pathname.
    const raw =
      (typeof req.query.next === "string" && req.query.next) ||
      (typeof req.query.return_to === "string" && req.query.return_to) ||
      "/";
    let dest = "/";
    try {
      dest = raw.startsWith("/") ? raw : new URL(raw).pathname + new URL(raw).search;
    } catch {
      dest = "/";
    }
    res.redirect(302, dest);
  });
}

// Who am I — proves the SSO cookie path works end to end.
dashboardApp.get("/api/me", auth.requireAuth, (req, res) => {
  res.json({ email: req.user.email, tenant: req.user.tenant, admin: auth.isElcanoAdmin(req.user) });
});

// Client view broker (PLAN §6b). For Elcano-only pages (no password), the
// content host can't do SSO — so staff come here: we verify the Elcano session,
// mint a short broker token for the published version, and bounce to the content
// host, which exchanges it for a page-session cookie. (Password pages can be
// opened directly on the content host; this also works as a staff shortcut.)
//
// requireAdmin, not requireAuth: this route hands out a session cookie for the
// live page WITHOUT the per-page client password, so it is the one credential
// that reads every hosted dashboard. `elcano_auth` is minted by a shared SSO
// whose audience is wider than Elcano staff — that is the entire reason
// isElcanoAdmin() exists — so accepting any valid session here would let a
// signed-in outsider read every client's numbers, and skip the password gate on
// the pages that have one. PLAN §6b says "Elcano admin cookie → allow", and the
// content host's own gate copy says "Elcano staff can open it through the Pages
// dashboard". This is the check that makes that sentence true, and PLAN §6b
// now records the broker as staff-only rather than as a client entrance.
dashboardApp.get("/view/*slug", auth.requireAdmin, async (req, res, next) => {
  try {
    const { page } = await versions.getPage(req.params.slug.join("/"));
    if (!page.published_version_id) return res.status(404).type("text").send("nothing published yet");
    // purpose "session": the only token the content host will exchange for a
    // page-session cookie, and the only place it is minted. See lib/rawtoken.js.
    const token = rawtoken.mint(
      { pageId: page.id, versionId: page.published_version_id, purpose: "session", renderMode: "themed" },
      120
    );
    res.redirect(302, `${CONTENT_ORIGIN}/${page.slug}?t=${encodeURIComponent(token)}`);
  } catch (err) {
    next(err);
  }
});

// Admin landing — Elcano staff only. Flag-themed workspace index that lists,
// filters, and organizes pages through the CSRF-protected admin API, with links
// into each per-slug shell. Registered
// BEFORE /admin/*slug so "welcome" resolves to the index, not a page slug
// (createPage refuses the 'welcome' slug — versions.RESERVED_SLUG_SEGMENTS).
dashboardApp.get(["/admin", "/admin/welcome"], auth.requireAdmin, (req, res) => {
  res.type("html").send(
    welcomeShell.render(req.user.email, CONTENT_ORIGIN, { csrf: csrf.mint(req.user.email), compose: compose.enabled() })
  );
});

// DEV/TEST ONLY: "compose a page with Cutlass" — spawns the cutlass CLI which
// deploys back via /mcp. Mounted (before the admin API) ONLY when gated on
// (PAGES_COMPOSE=1 + CUTLASS_BIN/DIR). Never enabled in production.
if (compose.enabled()) {
  console.warn(`⚠ COMPOSE ENABLED [driver: ${compose.driverLabel()}]: POST /api/v1/admin/compose runs Cutlass. Dev/test only — never set PAGES_COMPOSE=1 in production.`);
  dashboardApp.use("/api/v1/admin/compose", compose.router);
}

// Admin shell — Elcano staff only. Flag-themed version list + pending review
// queue + sandboxed preview + publish/rollback/approve/reject/disable/approval/
// The template library: browse registered designs, inspect their schemas and
// reference config, preview a revision, and upload a new one with the format
// checks run before anything is written. Registered BEFORE /admin/*slug so
// "templates" resolves here rather than being read as a page slug (and
// RESERVED_SLUG_SEGMENTS stops a page from taking that slug at all).
dashboardApp.get("/admin/templates", auth.requireAdmin, (req, res) => {
  const token = csrf.mint(req.user.email);
  res.type("html").send(templateShell.render(token, req.user.email, CONTENT_ORIGIN));
});

// Partner portals: the only surface anywhere that changes which dashboards one
// client credential opens. Registered BEFORE /admin/*slug for the same reason as
// the library above, and "portals" is a reserved slug segment so no page can take
// the address.
dashboardApp.get("/admin/portals", auth.requireAdmin, (req, res) => {
  const token = csrf.mint(req.user.email);
  res.type("html").send(portalShell.render(token, req.user.email, CONTENT_ORIGIN));
});

// theme controls. The page is a bootstrap that drives the admin JSON API below.
dashboardApp.get("/admin/*slug", auth.requireAdmin, (req, res) => {
  const token = csrf.mint(req.user.email);
  res.type("html").send(adminShell.render(req.params.slug.join("/"), token, req.user.email, CONTENT_ORIGIN));
});

// Admin JSON API (cookie + CSRF) — the human mutation surface. Mounted BEFORE
// the bearer router so its /api/v1/admin/* paths win; everything else falls
// through to the agent API.
dashboardApp.use("/api/v1/admin", adminApiRouter);

// REST API (Phase 2) — bearer-authenticated agent surface. Routes every state
// change through the version state machine (lib/versions.js); see lib/api.js.
dashboardApp.use("/api/v1", apiRouter);

// Logout is owned by the auth service (it clears the shared cookie). 303 so
// the browser re-issues as GET. Mirrors home.
dashboardApp.all("/logout", (_req, res) => res.redirect(303, `${auth.AUTH_LOGIN_URL}/logout`));

// Root is not a public surface: there's nothing to show an anonymous visitor,
// and the old plain-text blurb advertised the /admin and /view paths. Bounce to
// /admin, which is requireAdmin-gated — staff land on the welcome index, everyone
// else is sent to SSO login. 302 (temporary) so the root stays a redirect.
dashboardApp.get("/", (_req, res) => res.redirect(302, "/admin"));

dashboardApp.use((_req, res) => res.status(404).type("html").send(errorShell.notFound()));

// ── Top-level vhost split ──────────────────────────────────────────────────
app.use((req, res, next) => {
  if (isContentHost(req)) return contentApp(req, res, next);
  return dashboardApp(req, res, next);
});

if (require.main === module) {
  const server = app.listen(PORT, () => {
    console.log(`Elcano Pages on :${PORT}  (dashboard=${DASHBOARD_HOST}  content=${CONTENT_HOST})`);
  });
  const shutdown = () => {
    server.close(() => db.pool.end().finally(() => process.exit(0)));
  };
  process.once("SIGTERM", shutdown);
  process.once("SIGINT", shutdown);
}

module.exports = { app, isContentHost };
