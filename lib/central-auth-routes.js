// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/central-auth-routes.js — the dashboard host's sign-in surface in
// PAGES_AUTH_MODE=central. Mounted by server.js only in that mode, ahead of
// the broad body parsers (the back-channel receiver owns a small one).
//
//   GET  /auth/login               start the handoff (state, nonce, PKCE)
//   GET  /auth/callback            finish it, mint the Pages session
//   POST /auth/backchannel-logout  Auth's signed logout and access events
//   GET  /logout                   end this session, then Auth's RP logout
//   GET  /signed-out               public landing after sign-out
//
// See lib/central-auth.js for the contract and the session conventions.

const express = require("express");

const auth = require("./auth");
const centralAuth = require("./central-auth");
const standaloneChrome = require("./standalone-chrome");
const limits = require("./ratelimit");

const router = express.Router();
const runtime = auth.central;

function cookie(name, value, { maxAge } = {}) {
  const parts = [`${name}=${value}`, "Path=/", "HttpOnly", "SameSite=Lax"];
  if (runtime.config.cookieSecure) parts.push("Secure");
  if (maxAge !== undefined) parts.push(`Max-Age=${maxAge}`);
  return parts.join("; ");
}

const clearCookie = (name) => cookie(name, "", { maxAge: 0 });

function page(res, status, { title, heading, body, cta }) {
  res
    .status(status)
    .set("Cache-Control", "no-store")
    .type("html")
    .send(
      standaloneChrome.render({
        title: `${title} · Pages`,
        kicker: title,
        heading,
        bodyHtml: `<p>${body}</p>`,
        assetsBase: "/shell-assets/flag",
        cta,
      })
    );
}

const retry = { href: "/auth/login", label: "Sign in again" };

function noAccess(res) {
  page(res, 403, {
    title: "No access",
    heading: "Your account does not have access to Pages",
    body: "You are signed in, but this account has not been granted Pages. Ask an administrator to grant it in the account console.",
    cta: { href: `${runtime.config.issuer}/account`, label: "Your account" },
  });
}

router.get("/auth/login", async (req, res) => {
  const next = centralAuth.safeLocalPath(typeof req.query.next === "string" ? req.query.next : "/admin");
  if (await auth.resolveSession(req)) return res.redirect(303, next);
  const tx = centralAuth.newTransaction(next);
  res
    .set("Cache-Control", "no-store")
    .set("Set-Cookie", cookie(runtime.loginCookie, centralAuth.sealTransaction(tx, runtime.config.transactionKey), {
      maxAge: centralAuth.LOGIN_TRANSACTION_SECONDS,
    }))
    .redirect(303, runtime.client.authorizationUrl({ state: tx.state, nonce: tx.nonce, codeChallenge: tx.challenge }));
});

router.get("/auth/callback", async (req, res) => {
  // The transaction is single-use whatever happens next.
  res.set("Set-Cookie", clearCookie(runtime.loginCookie)).set("Referrer-Policy", "no-referrer");
  const { code, state, error, iss } = req.query;
  if (error === "access_denied") return noAccess(res);
  // Auth names itself in the response (RFC 9207); a different issuer means
  // the response is not from the Auth this deployment trusts.
  const wrongIssuer = iss !== undefined && (typeof iss !== "string" || iss.replace(/\/+$/, "") !== runtime.config.issuer);
  if (error || wrongIssuer || typeof code !== "string" || !code || typeof state !== "string" || !state) {
    return page(res, 400, { title: "Sign-in", heading: "Sign-in was not completed", body: "Start again from the sign-in page.", cta: retry });
  }
  try {
    const sealed = auth.parseCookies(req.headers.cookie)[runtime.loginCookie];
    const tx = centralAuth.openTransaction(sealed, state, runtime.config.transactionKey);
    const exchangeStartedMs = Date.now();
    const principal = await runtime.client.exchange({ code, verifier: tx.verifier, nonce: tx.nonce });
    const token = await centralAuth.store.createSession(principal.subject, principal.email, { exchangeStartedMs });
    res.append("Set-Cookie", cookie(runtime.sessionCookie, token));
    return res.redirect(303, tx.next);
  } catch (err) {
    if (err instanceof centralAuth.AccessDeniedError) return noAccess(res);
    if (err instanceof centralAuth.AuthTransactionError || err instanceof centralAuth.CodeExchangeRejectedError) {
      return page(res, 400, { title: "Sign-in", heading: "Sign-in expired", body: "That sign-in link is no longer valid. Try again.", cta: retry });
    }
    if (err instanceof centralAuth.CentralAuthError) {
      console.error("central auth: code exchange failed:", err.message);
      return page(res, 502, {
        title: "Sign-in",
        heading: "The sign-in service is unavailable",
        body: "Pages could not reach the account service. Try again shortly.",
        cta: retry,
      });
    }
    throw err;
  }
});

// Auth's server-to-server channel. Exactly one form field: logout_token (end
// the subject's sessions) or access_token (grant/revoke this application).
// 204 once durable; 400 for a token that will never verify; 503 for a
// failure worth retrying (Auth retries every non-2xx with backoff).
router.post(
  "/auth/backchannel-logout",
  limits.api,
  express.urlencoded({ extended: false, limit: "20kb", parameterLimit: 4 }),
  async (req, res) => {
    res.set("Cache-Control", "no-store");
    const body = req.body || {};
    const fields = Object.keys(body);
    if (fields.length !== 1 || !["logout_token", "access_token"].includes(fields[0]) || typeof body[fields[0]] !== "string" || !body[fields[0]]) {
      return res.status(400).json({ error: "invalid_request" });
    }
    const logoutToken = fields[0] === "logout_token" ? body.logout_token : "";
    const accessToken = fields[0] === "access_token" ? body.access_token : "";
    const raw = logoutToken || accessToken;
    const opts = {
      issuer: runtime.config.issuer,
      audience: runtime.config.clientId,
      keys: await runtime.keys.keysForToken(raw),
    };
    let event;
    try {
      event = logoutToken ? centralAuth.verifyLogoutToken(raw, opts) : centralAuth.verifyAccessToken(raw, opts);
    } catch {
      return res.status(400).json({ error: "invalid_token" });
    }
    try {
      if (logoutToken) await centralAuth.store.consumeLogoutEvent(event);
      else await centralAuth.store.applyAccessEvent(event);
    } catch (err) {
      console.error("central auth: back-channel event not applied:", err.message);
      return res.status(503).json({ error: "temporarily_unavailable" });
    }
    return res.status(204).end();
  }
);

// Sign-out ends this session and hands the browser to Auth's RP-initiated
// logout, which ends the central session and fans a back-channel logout out
// to every application. Ending only the local session would be undone by the
// next silent sign-in. A link, not a form, so the shell stays unchanged. A
// request that a browser marks as cross-site (Fetch Metadata) or that carries
// a foreign Origin is ignored. A client sending neither header can still be
// made to sign out: a nuisance, not a takeover, and the same one Auth's own
// GET /logout?client_id accepts.
router.all("/logout", async (req, res) => {
  const site = String(req.headers["sec-fetch-site"] || "");
  const origin = req.headers.origin;
  if ((site && site !== "same-origin" && site !== "none") || (origin && origin !== runtime.config.publicUrl)) {
    return res.redirect(303, "/admin");
  }
  const token = auth.parseCookies(req.headers.cookie)[runtime.sessionCookie];
  if (token) await centralAuth.store.revokeSession(token);
  res
    .set("Cache-Control", "no-store")
    .set("Set-Cookie", [clearCookie(runtime.sessionCookie), clearCookie(runtime.loginCookie)])
    .redirect(303, runtime.client.logoutUrl());
});

// Registered with Auth as the post-logout landing. Public and identity-free:
// bouncing to /admin would start a silent sign-in at once.
router.get("/signed-out", (_req, res) => {
  page(res, 200, { title: "Signed out", heading: "You are signed out of Pages", body: "Sign in again to manage dashboards.", cta: retry });
});

module.exports = router;
