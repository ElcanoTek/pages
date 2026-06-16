"use strict";
// scripts/dev-auth.js — local-only Elcano auth for `scripts/dev.sh`. The real
// auth service mints the elcano_auth cookie; locally there is none. So we make a
// throwaway Ed25519 keypair (persisted in .devdata), publish its PUBLIC half as
// AUTH_SIGNING_PUBKEY (what server verifies with), and mint a long-lived admin
// cookie signed by the PRIVATE half. Output is shell-evalable.
//
//   eval "$(node scripts/dev-auth.js <devdata-dir> [email])"
//
// This is dev-only. The private seed never leaves .devdata, and the dev-login
// route that uses the cookie is gated behind PAGES_DEV_LOGIN=1 (never prod).

const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");

const dev = process.argv[2];
const email = process.argv[3] || process.env.DEV_ADMIN_EMAIL || "dev@elcanotek.com";
if (!dev) {
  console.error("usage: dev-auth.js <devdata-dir> [email]");
  process.exit(1);
}

const pemFile = path.join(dev, "auth-dev.pem");
let priv;
if (fs.existsSync(pemFile)) {
  priv = crypto.createPrivateKey(fs.readFileSync(pemFile, "utf8"));
} else {
  priv = crypto.generateKeyPairSync("ed25519").privateKey;
  fs.writeFileSync(pemFile, priv.export({ type: "pkcs8", format: "pem" }), { mode: 0o600 });
}

const pub = crypto.createPublicKey(priv);
const pubB64 = Buffer.from(pub.export({ format: "jwk" }).x, "base64url").toString("base64");

const now = Math.floor(Date.now() / 1000);
const body = Buffer.from(JSON.stringify({ email, tenant: "", iat: now, exp: now + 30 * 86400 })).toString("base64url");
const sig = crypto.sign(null, Buffer.from(body), priv).toString("base64url");
const cookie = `${body}.${sig}`;

// Emit shell assignments. Single-quoted; values are base64url/base64 so safe.
process.stdout.write(`AUTH_SIGNING_PUBKEY='${pubB64}'\n`);
process.stdout.write(`DEV_ADMIN_COOKIE='${cookie}'\n`);
process.stdout.write(`DEV_ADMIN_EMAIL='${email}'\n`);
