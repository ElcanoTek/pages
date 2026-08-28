// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/rawtoken.js — signed, short-TTL access tokens for the cookieless content
// host (PLAN.md §6d, §7). The token is the ONLY thing that authorizes anything
// on that domain — there are no cookies there. HMAC-SHA256 with
// RAW_TOKEN_SECRET; constant-time verify.
//
//   token = base64url(JSON(claims)) + "." + base64url(HMAC(secret, body))
//   claims = { pid, vid, purpose, mode, sid, exp }

const crypto = require("node:crypto");

const SECRET = process.env.RAW_TOKEN_SECRET || "";
// `purpose` is the AUDIENCE of a token, and each one is consumed by exactly one
// route that allow-lists it. Keeping them disjoint is what stops a credential
// minted for a narrow job from being redeemed for a wider one:
//
//   view     — render this one page version at /raw. Minted by the /admin
//              preview endpoint for ANY version, including the published one.
//              Read-only, one version, no session.
//   template — render this one template revision at /raw-template. A template
//              revision id and a page version id are different id spaces, so a
//              token bound to one must be unusable against the other.
//   session  — exchange for a page-session cookie on the content host. Minted
//              ONLY by the dashboard's /view broker, after Elcano-staff SSO.
//              This is the one purpose that is a *page* credential rather than
//              a *render* credential, which is why it is separate: an /admin
//              preview URL is exactly the kind of link that gets pasted into a
//              chat, and it must never buy an hour of access to the live page
//              (see lib/contentview.js).
//
// "edit" used to be listed here for a Phase 4 editor that was never built. It
// was never minted or checked anywhere, so it was a purpose that only widened
// what a forged-but-valid claim could name. Removed rather than left as an
// unused door.
const PURPOSES = new Set(["view", "template", "session"]);

// Fail CLOSED in production: an empty key lets anyone forge a /raw view token.
// mint() already throws without a secret; extend that to boot time in prod.
if (!SECRET && process.env.NODE_ENV === "production") {
  throw new Error("RAW_TOKEN_SECRET is required in production (an empty key lets anyone forge /raw access tokens).");
}

function hmac(body) {
  return crypto.createHmac("sha256", SECRET).update(body).digest();
}

// mint a token. claims: { pageId, versionId, purpose, renderMode, sid? }.
function mint({ pageId, versionId, purpose, renderMode, sid }, ttlSeconds = 300) {
  if (!SECRET) throw new Error("RAW_TOKEN_SECRET is not set");
  if (!PURPOSES.has(purpose)) throw new Error(`bad purpose: ${purpose}`);
  const payload = {
    pid: pageId,
    vid: versionId,
    purpose,
    mode: renderMode,
    sid: sid || crypto.randomBytes(8).toString("hex"),
    exp: Math.floor(Date.now() / 1000) + ttlSeconds,
  };
  const body = Buffer.from(JSON.stringify(payload)).toString("base64url");
  const sig = hmac(body).toString("base64url");
  return `${body}.${sig}`;
}

// verify → claims object or null. Constant-time signature check; rejects
// expired tokens and unknown purposes.
function verify(token) {
  if (!SECRET || typeof token !== "string") return null;
  const dot = token.indexOf(".");
  if (dot < 1 || dot === token.length - 1) return null;

  const body = token.slice(0, dot);
  let given;
  try {
    given = Buffer.from(token.slice(dot + 1), "base64url");
  } catch {
    return null;
  }
  const expected = hmac(body);
  if (given.length !== expected.length || !crypto.timingSafeEqual(given, expected)) return null;

  let p;
  try {
    p = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return null;
  }
  if (!PURPOSES.has(p.purpose)) return null;
  if (typeof p.exp !== "number" || p.exp <= Math.floor(Date.now() / 1000)) return null;
  return p;
}

module.exports = { mint, verify };
