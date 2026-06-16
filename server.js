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
const { shellHelmetOptions, rawHeaders } = require("./lib/csp");
const rawtoken = require("./lib/rawtoken");
const render = require("./lib/render");
const db = require("./lib/db");
const apiRouter = require("./lib/api");
const adminApiRouter = require("./lib/adminapi");
const adminShell = require("./lib/adminshell");
const csrf = require("./lib/csrf");
const mcp = require("./lib/mcp");

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
contentApp.get("/healthz", (_req, res) => res.type("text").send("ok"));

// Vendored, immutable rendering libs + Flag tokens/fonts live under public/assets
// and are served from the content host so /raw pages can reference them CSP-clean.
contentApp.use(
  "/assets",
  express.static(path.join(__dirname, "public", "assets"), {
    index: false,
    setHeaders(res) {
      res.setHeader("X-Content-Type-Options", "nosniff");
    },
  })
);

// /raw/<slug> — the sandboxed render target. The signed token is the ONLY
// authorization here (no cookies on this host). It binds the exact version +
// purpose, so it can't be replayed for another version or escalated to edit.
// Headers (sandbox CSP etc.) are set on EVERY path, including errors.
contentApp.get("/raw/:slug", async (req, res) => {
  res.set(rawHeaders());
  const claims = rawtoken.verify(req.query.t);
  if (!claims) return res.status(403).type("html").send("<!doctype html><title>pages</title><p>Invalid or expired link.</p>");
  try {
    const v = await db.getRenderable(claims.vid);
    // The token's version must match what we loaded, belong to this slug, and
    // the page must not be disabled (takedown switch). pg returns BIGINT as a
    // string, so coerce before comparing to the numeric token claim.
    if (!v || Number(v.id) !== Number(claims.vid) || v.slug !== req.params.slug || v.disabled) {
      return res.status(404).type("html").send("<!doctype html><title>pages</title><p>Not found.</p>");
    }
    res.type("html").send(render.renderVersion(v));
  } catch (err) {
    console.error("render error:", err.message);
    res.status(500).type("html").send("<!doctype html><title>pages</title><p>Render error.</p>");
  }
});

contentApp.use((_req, res) => res.status(404).type("text").send("not found"));

// ===========================================================================
// DASHBOARD HOST (trusted auth zone)
// ===========================================================================
dashboardApp.use(helmet(shellHelmetOptions(auth.AUTH_ORIGIN)));
dashboardApp.use(express.urlencoded({ extended: false }));
dashboardApp.use(express.json({ limit: "2mb" }));

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
  dashboardApp.get("/__dev/login", (req, res) => {
    res.setHeader("Set-Cookie", `${cookieName}=${process.env.DEV_ADMIN_COOKIE}; Path=/; HttpOnly; SameSite=Lax`);
    const next = typeof req.query.next === "string" && req.query.next.startsWith("/") ? req.query.next : "/";
    res.redirect(302, next);
  });
}

// Who am I — proves the SSO cookie path works end to end.
dashboardApp.get("/api/me", auth.requireAuth, (req, res) => {
  res.json({ email: req.user.email, tenant: req.user.tenant, admin: auth.isElcanoAdmin(req.user) });
});

// Client view — Phase 1 adds per-page password + signed /raw embedding.
dashboardApp.get("/view/:slug", (req, res) => {
  res
    .status(501)
    .type("text")
    .send(`view '${req.params.slug}' — client view arrives in Phase 1.`);
});

// Admin shell — Elcano staff only. Flag-themed version list + pending review
// queue + sandboxed preview + publish/rollback/approve/reject/disable/approval/
// theme controls. The page is a bootstrap that drives the admin JSON API below.
dashboardApp.get("/admin/:slug", auth.requireAdmin, (req, res) => {
  const token = csrf.mint(req.user.email);
  res.type("html").send(adminShell.render(req.params.slug, token, req.user.email));
});

// Admin JSON API (cookie + CSRF) — the human mutation surface. Mounted BEFORE
// the bearer router so its /api/v1/admin/* paths win; everything else falls
// through to the agent API.
dashboardApp.use("/api/v1/admin", adminApiRouter);

// REST API (Phase 2) — bearer-authenticated agent surface. Routes every state
// change through the version state machine (lib/versions.js); see lib/api.js.
dashboardApp.use("/api/v1", apiRouter);

// MCP-over-HTTP — agent-native surface (chat & cutlass). JSON-RPC 2.0 wrapping
// the SAME state machine as the REST API (lib/mcp.js); bearer-authenticated.
dashboardApp.use("/mcp", mcp.router);

// Logout is owned by the auth service (it clears the shared cookie). 303 so
// the browser re-issues as GET. Mirrors home.
dashboardApp.all("/logout", (_req, res) => res.redirect(303, `${auth.AUTH_LOGIN_URL}/logout`));

dashboardApp.get("/", (_req, res) =>
  res.type("text").send("Elcano Pages. Client dashboards live at /view/<slug>; staff admin at /admin/<slug>.")
);

dashboardApp.use((_req, res) => res.status(404).type("text").send("not found"));

// ── Top-level vhost split ──────────────────────────────────────────────────
app.use((req, res, next) => {
  if (isContentHost(req)) return contentApp(req, res, next);
  return dashboardApp(req, res, next);
});

if (require.main === module) {
  app.listen(PORT, () => {
    console.log(`Elcano Pages on :${PORT}  (dashboard=${DASHBOARD_HOST}  content=${CONTENT_HOST})`);
  });
}

module.exports = { app, isContentHost };
