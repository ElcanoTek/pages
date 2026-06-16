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

// Public shell assets (CSS/JS for the trusted /view + /admin UI). Phase 1+.
dashboardApp.use(
  "/shell-assets",
  express.static(path.join(__dirname, "public", "shell-assets"), { index: false })
);

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

// Admin — Elcano staff only. Phase 1 adds the version list, preview, publish,
// rollback, approve/reject queue, theme picker, and (Phase 4) source editing.
dashboardApp.get("/admin/:slug", auth.requireAdmin, (req, res) => {
  res
    .status(501)
    .type("text")
    .send(`admin '${req.params.slug}' — hello ${req.user.email}. Admin UI arrives in Phase 1.`);
});

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
