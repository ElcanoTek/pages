"use strict";
// lib/rawtoken.js — signed, short-TTL access tokens for the cookieless content
// host (PLAN.md §6d, §7). The token is the ONLY thing that authorizes a /raw
// render — there are no cookies on that domain. It binds the exact version and
// purpose so a `view` token can't be replayed as `edit`, nor for another
// version. HMAC-SHA256 with RAW_TOKEN_SECRET; constant-time verify.
//
//   token = base64url(JSON(claims)) + "." + base64url(HMAC(secret, body))
//   claims = { pid, vid, purpose, mode, sid, exp }

const crypto = require("node:crypto");

const SECRET = process.env.RAW_TOKEN_SECRET || "";
const PURPOSES = new Set(["view", "edit"]);

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
