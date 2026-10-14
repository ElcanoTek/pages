// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/passwordvault.js — the staff-readable copy of a client password.
//
// A page or portal password is stored twice:
//
//   - password_hash (scrypt, lib/pagecookie.js) is what the content host checks
//     a client's submission against. It is one-way and stays the only thing the
//     gate reads.
//   - password_ciphertext (this module) is AES-256-GCM, so Elcano staff can show
//     the current password in the admin shell and resend it without rotating it
//     and locking out everyone else at the client.
//
// Why this is safe to have:
//   - The key is derived from PAGE_COOKIE_SECRET (HKDF, its own label), which
//     lives in the server env and never in the database, so a database dump or
//     backup alone opens nothing.
//   - Each ciphertext is bound to the password_hash it was sealed with (GCM
//     additional data). Any path that changes the hash without resealing, such
//     as an older release during a rolling deploy, leaves a copy that no longer
//     opens, and the shell says "not available" rather than showing a stale
//     password.
//   - The only reader is the admin shell (lib/adminapi.js, Elcano SSO cookie +
//     CSRF), and every reveal writes an audit row. No bearer, MCP or content-host
//     route returns a password, so an agent can never learn which password opens
//     which client's dashboards.
//
// Passwords set before this existed have only a hash. rememberOnSignIn seals the
// copy the next time a client signs in with that password, so nothing has to be
// rotated to become visible.

const crypto = require("node:crypto");
const db = require("./db");

const VERSION = "v1";
const MAX_PASSWORD_LENGTH = 512; // the KDF input bound in lib/pagecookie.js

const KEY = crypto.hkdfSync(
  "sha256",
  Buffer.from(process.env.PAGE_COOKIE_SECRET || "", "utf8"),
  Buffer.alloc(0),
  Buffer.from("pages:client-password-reveal:v1", "utf8"),
  32
);

function aad(passwordHash) {
  return Buffer.from(`pages-password:${VERSION}:${passwordHash}`, "utf8");
}

// seal — encrypt one password for the row whose password_hash is passwordHash.
// Format: v1.<iv>.<ciphertext>.<tag>, each base64url.
function seal(password, passwordHash) {
  if (typeof password !== "string" || !password || typeof passwordHash !== "string" || !passwordHash) return null;
  const iv = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv("aes-256-gcm", Buffer.from(KEY), iv);
  cipher.setAAD(aad(passwordHash));
  const body = Buffer.concat([cipher.update(password.slice(0, MAX_PASSWORD_LENGTH), "utf8"), cipher.final()]);
  return [VERSION, iv.toString("base64url"), body.toString("base64url"), cipher.getAuthTag().toString("base64url")].join(".");
}

// open — the password, or null when there is no copy, it was sealed for another
// hash, it was tampered with, or the key has changed since.
function open(sealed, passwordHash) {
  if (typeof sealed !== "string" || typeof passwordHash !== "string" || !passwordHash) return null;
  const parts = sealed.split(".");
  if (parts.length !== 4 || parts[0] !== VERSION) return null;
  try {
    const decipher = crypto.createDecipheriv("aes-256-gcm", Buffer.from(KEY), Buffer.from(parts[1], "base64url"));
    decipher.setAAD(aad(passwordHash));
    decipher.setAuthTag(Buffer.from(parts[3], "base64url"));
    return Buffer.concat([decipher.update(Buffer.from(parts[2], "base64url")), decipher.final()]).toString("utf8");
  } catch {
    return null;
  }
}

const TABLES = { page: "pages", portal: "page_portals" };

// rememberOnSignIn — after a client's password VERIFIED against passwordHash on
// the content host, keep a staff-readable copy if the row has none that opens.
// The UPDATE is conditional on the same hash, so a rotation that commits in
// between is never overwritten with the old password. Best effort: a failure
// here must never turn a correct password into an error page.
async function rememberOnSignIn(kind, id, passwordHash, password) {
  const table = TABLES[kind];
  if (!table || !passwordHash || typeof password !== "string" || !password) return;
  try {
    const plain = password.slice(0, MAX_PASSWORD_LENGTH);
    const { rows } = await db.query(
      `SELECT password_ciphertext FROM ${table} WHERE id = $1 AND password_hash = $2`,
      [id, passwordHash]
    );
    if (!rows[0] || open(rows[0].password_ciphertext, passwordHash) === plain) return;
    await db.query(
      `UPDATE ${table} SET password_ciphertext = $3 WHERE id = $1 AND password_hash = $2`,
      [id, passwordHash, seal(plain, passwordHash)]
    );
  } catch (err) {
    console.error(`passwordvault: could not keep the ${kind} password copy:`, err.message);
  }
}

module.exports = { seal, open, rememberOnSignIn };
