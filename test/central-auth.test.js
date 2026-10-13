// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Unit tests for lib/central-auth.js (no DB): signed back-channel tokens, the
// JWKS key resolver, the code exchange and its id_token, the login transaction, redirect
// safety and configuration validation. The Postgres store and the routes are
// covered by test/central-auth.integration.js. Run: npm test

const { test } = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");

const ca = require("../lib/central-auth");

const ISSUER = "https://auth.northwind.example";
const keyPair = () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  return { privateKey, raw: Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url") };
};
const primary = keyPair();
const now = () => Math.floor(Date.now() / 1000);

function sign(typ, claims, key = primary, headerExtra = {}) {
  const header = Buffer.from(JSON.stringify({ alg: "EdDSA", typ, kid: ca.kidForKey(key.raw), ...headerExtra })).toString("base64url");
  const body = Buffer.from(JSON.stringify(claims)).toString("base64url");
  return `${header}.${body}.${crypto.sign(null, Buffer.from(`${header}.${body}`), key.privateKey).toString("base64url")}`;
}

const base = (extra = {}) => ({ iss: ISSUER, aud: "pages", sub: "sub-1", jti: "jti-1", iat: now(), exp: now() + 120, ...extra });
const logoutClaims = (extra) => base({ events: { "http://schemas.openid.net/event/backchannel-logout": {} }, ...extra });
const accessClaims = (action, version, extra) =>
  base({ email: "Ana@Northwind.example", events: { "urn:elcanotek:event:application-access": { action, version } }, ...extra });
const opts = (keys = [primary.raw]) => ({ issuer: ISSUER, audience: "pages", keys });

test("central-auth.verifyLogoutToken: accepts Auth's logout+jwt", () => {
  const event = ca.verifyLogoutToken(sign("logout+jwt", logoutClaims()), opts());
  assert.deepEqual({ ...event, issuedAt: 0 }, { eventId: "jti-1", subject: "sub-1", issuer: ISSUER, issuedAt: 0 });
});

test("central-auth.verifyLogoutToken: refuses every malformed or foreign token", () => {
  const other = keyPair();
  const bad = {
    "wrong key": sign("logout+jwt", logoutClaims(), other),
    "kid of a trusted key, signature of another": sign("logout+jwt", logoutClaims(), other, { kid: ca.kidForKey(primary.raw) }),
    "wrong typ": sign("access+jwt", logoutClaims()),
    "wrong alg": sign("logout+jwt", logoutClaims(), primary, { alg: "none" }),
    "wrong audience": sign("logout+jwt", logoutClaims({ aud: "explorer" })),
    "audience array": sign("logout+jwt", logoutClaims({ aud: ["pages"] })),
    "wrong issuer": sign("logout+jwt", logoutClaims({ iss: "https://evil.example" })),
    expired: sign("logout+jwt", logoutClaims({ exp: now() - 61 })),
    "issued in the future": sign("logout+jwt", logoutClaims({ iat: now() + 600 })),
    "nonce present": sign("logout+jwt", logoutClaims({ nonce: "n" })),
    "no event": sign("logout+jwt", base()),
    "string exp": sign("logout+jwt", logoutClaims({ exp: String(now() + 60) })),
    "empty sub": sign("logout+jwt", logoutClaims({ sub: "" })),
  };
  for (const [name, token] of Object.entries(bad)) {
    assert.throws(() => ca.verifyLogoutToken(token, opts()), ca.CentralAuthError, name);
  }
  const good = sign("logout+jwt", logoutClaims());
  const [h, c] = good.split(".");
  assert.throws(() => ca.verifyLogoutToken(`${h}.${c}.${"A".repeat(86)}`, opts()), ca.CentralAuthError, "bad signature");
  assert.throws(() => ca.verifyLogoutToken("a.b", opts()), ca.CentralAuthError, "two segments");
  assert.throws(() => ca.verifyLogoutToken("x".repeat(20000), opts()), ca.CentralAuthError, "oversized");
  assert.throws(() => ca.verifyLogoutToken(good, opts([])), ca.CentralAuthError, "no keys");
});

test("central-auth.verifyLogoutToken: an issuer with a trailing slash still matches", () => {
  assert.equal(ca.verifyLogoutToken(sign("logout+jwt", logoutClaims({ iss: `${ISSUER}/` })), opts()).subject, "sub-1");
});

test("central-auth.verifyAccessToken: grant/revoke with a positive version, email normalized", () => {
  const grant = ca.verifyAccessToken(sign("access+jwt", accessClaims("grant", 3)), opts());
  assert.equal(grant.allowed, true);
  assert.equal(grant.version, 3);
  assert.equal(grant.email, "ana@northwind.example");
  assert.equal(ca.verifyAccessToken(sign("access+jwt", accessClaims("revoke", 4)), opts()).allowed, false);
  for (const [action, version] of [["grant", 0], ["grant", -1], ["grant", 1.5], ["grant", "2"], ["delete", 1]]) {
    assert.throws(() => ca.verifyAccessToken(sign("access+jwt", accessClaims(action, version)), opts()), ca.CentralAuthError);
  }
  assert.throws(() => ca.verifyAccessToken(sign("access+jwt", accessClaims("grant", 1, { email: "not-an-email" })), opts()));
  assert.throws(() => ca.verifyAccessToken(sign("logout+jwt", accessClaims("grant", 1)), opts()), "logout typ");
  const withSettings = (settings) =>
    sign("access+jwt", base({ email: "a@northwind.example", events: { "urn:elcanotek:event:application-access": { action: "grant", version: 1, settings } } }));
  assert.equal(ca.verifyAccessToken(withSettings({ team: "ops" }), opts()).allowed, true, "string settings accepted and ignored");
  for (const bad of [null, [], "x", { team: 1 }]) {
    assert.throws(() => ca.verifyAccessToken(withSettings(bad), opts()), ca.CentralAuthError, JSON.stringify(bad));
  }
});

test("central-auth.KeyResolver: a known kid never waits on Auth; an unknown kid fetches at most once a minute", async () => {
  const rotated = keyPair();
  let fetches = 0;
  let clock = 1_000_000;
  const fetchImpl = async (url, init) => {
    fetches++;
    assert.equal(url, `${ISSUER}/jwks.json`);
    assert.equal(init.redirect, "manual");
    return new Response(JSON.stringify({ keys: [{ kty: "OKP", crv: "Ed25519", x: rotated.raw.toString("base64url") }] }));
  };
  const resolver = new ca.KeyResolver(ISSUER, [primary.raw], { fetchImpl, clock: () => clock });
  await resolver.keysForToken(sign("logout+jwt", logoutClaims()));
  assert.equal(fetches, 0, "static key's kid → no fetch");
  const rotatedToken = sign("logout+jwt", logoutClaims(), rotated);
  const keys = await resolver.keysForToken(rotatedToken);
  assert.equal(fetches, 1);
  assert.equal(ca.verifyLogoutToken(rotatedToken, opts(keys)).subject, "sub-1", "JWKS-only key verifies");
  await resolver.keysForToken(sign("logout+jwt", logoutClaims(), keyPair()));
  assert.equal(fetches, 1, "a second unknown kid inside a minute does not refetch");
  clock += 61;
  await resolver.keysForToken(sign("logout+jwt", logoutClaims(), keyPair()));
  assert.equal(fetches, 2, "after a minute an unknown kid may refetch");
});

test("central-auth.KeyResolver: a JWKS-only key is re-checked once the cache is stale, and dropped when Auth retires it", async () => {
  const rotated = keyPair();
  let published = [rotated];
  let fetches = 0;
  let clock = 0;
  const fetchImpl = async () => {
    fetches++;
    return new Response(JSON.stringify({ keys: published.map((k) => ({ kty: "OKP", crv: "Ed25519", x: k.raw.toString("base64url") })) }));
  };
  const resolver = new ca.KeyResolver(ISSUER, [primary.raw], { fetchImpl, clock: () => clock });
  const token = sign("logout+jwt", logoutClaims(), rotated);
  await resolver.keysForToken(token);
  clock += 300;
  await resolver.keysForToken(token);
  assert.equal(fetches, 1, "inside the cache window a known remote key is not refetched");
  published = [];
  clock += 601;
  const keys = await resolver.keysForToken(token);
  assert.equal(fetches, 2, "a stale cache is refreshed even for a known kid");
  assert.throws(() => ca.verifyLogoutToken(token, opts(keys)), ca.CentralAuthError, "retired key no longer verifies");
  await resolver.keysForToken(sign("logout+jwt", logoutClaims()));
  assert.equal(fetches, 2, "a static key never waits on Auth");
});

test("central-auth.KeyResolver: a failed or hostile JWKS keeps the cached keys", async () => {
  const rotated = keyPair();
  let mode = "ok";
  let clock = 0;
  const fetchImpl = async () => {
    if (mode === "fail") throw new Error("ECONNREFUSED");
    if (mode === "huge") return new Response("x".repeat(70 * 1024));
    return new Response(JSON.stringify({ keys: [{ kty: "OKP", crv: "Ed25519", x: rotated.raw.toString("base64url") }] }));
  };
  const resolver = new ca.KeyResolver(ISSUER, [primary.raw], { fetchImpl, clock: () => clock });
  await resolver.keysForToken(sign("logout+jwt", logoutClaims(), keyPair()));
  assert.equal(resolver.known().length, 2);
  for (mode of ["fail", "huge"]) {
    clock += 61;
    await resolver.keysForToken(sign("logout+jwt", logoutClaims(), keyPair()));
    assert.equal(resolver.known().length, 2, `${mode}: cache kept`);
  }
});

// A key Auth publishes only through /jwks.json (a rotation in progress).
const rotated = keyPair();
const jwks = async () => new Response(JSON.stringify({ keys: [{ kty: "OKP", crv: "Ed25519", x: rotated.raw.toString("base64url"), kid: ca.kidForKey(rotated.raw) }] }));

function client(fetchImpl, clock = now) {
  const config = ca.loadConfig({
    AUTH_ISSUER_URL: ISSUER,
    DASHBOARD_HOST: "pages.northwind.example",
    AUTH_CLIENT_SECRET: "s".repeat(40),
    AUTH_SIGNING_PUBKEY: primary.raw.toString("base64"),
    PAGE_COOKIE_SECRET: "unit-test-secret",
  });
  const keys = new ca.KeyResolver(config.issuer, config.staticKeys, { fetchImpl: jwks, clock });
  return new ca.Client(config, { keys, fetchImpl, clock });
}

// Auth's /token answer: the claims in the clear and as a signed id_token.
const idClaims = (extra = {}) => ({
  iss: ISSUER, aud: "pages", sub: "sub-1", email: "Ana@Northwind.example", iat: now(), exp: now() + 60,
  nonce: "n1", auth_time: now(), amr: ["pwd"], acr: "urn:elcanotek:loa:1", ...extra,
});
const tokenResponse = (claims = idClaims(), idToken = sign("JWT", claims)) =>
  new Response(JSON.stringify({ ...claims, id_token: idToken }));

test("central-auth.Client.exchange: posts the code with client_secret_basic and verifies the id_token", async () => {
  let seen;
  const c = client(async (url, init) => {
    seen = { url, init };
    return tokenResponse();
  });
  const principal = await c.exchange({ code: "c1", verifier: "v1", nonce: "n1" });
  assert.deepEqual(principal, { subject: "sub-1", email: "ana@northwind.example" });
  assert.equal(seen.url, `${ISSUER}/token`);
  assert.equal(seen.init.redirect, "manual");
  assert.equal(seen.init.headers.Authorization, `Basic ${Buffer.from(`pages:${"s".repeat(40)}`).toString("base64")}`);
  const form = new URLSearchParams(seen.init.body);
  assert.equal(form.get("redirect_uri"), "https://pages.northwind.example/auth/callback");
  assert.equal(form.get("code_verifier"), "v1");
  assert.equal(form.get("grant_type"), "authorization_code");
});

test("central-auth.Client.exchange: an id_token signed by a JWKS-only key verifies", async () => {
  const c = client(async () => tokenResponse(idClaims(), sign("JWT", idClaims(), rotated)));
  assert.equal((await c.exchange({ code: "c", verifier: "v", nonce: "n1" })).subject, "sub-1");
});

test("central-auth.Client.exchange: identity comes from the signed claims, not the JSON beside them", async () => {
  const c = client(async () => new Response(JSON.stringify({ ...idClaims({ sub: "sub-evil", email: "evil@northwind.example" }), id_token: sign("JWT", idClaims()) })));
  assert.deepEqual(await c.exchange({ code: "c", verifier: "v", nonce: "n1" }), { subject: "sub-1", email: "ana@northwind.example" });
});

test("central-auth.Client.exchange: refuses answers it must not trust", async () => {
  const other = keyPair();
  const signedWith = (claims) => tokenResponse(idClaims(), sign("JWT", claims));
  const cases = {
    "nonce mismatch": () => signedWith(idClaims({ nonce: "n2" })),
    "no nonce": () => signedWith(idClaims({ nonce: undefined })),
    "other audience": () => signedWith(idClaims({ aud: "explorer" })),
    "audience array": () => signedWith(idClaims({ aud: ["pages"] })),
    "other issuer": () => signedWith(idClaims({ iss: "https://evil.example" })),
    expired: () => signedWith(idClaims({ exp: now() - 120 })),
    "issued in the future": () => signedWith(idClaims({ iat: now() + 600 })),
    "no subject": () => signedWith(idClaims({ sub: "" })),
    "bad email": () => signedWith(idClaims({ email: "nope" })),
    "missing id_token": () => new Response(JSON.stringify(idClaims())),
    "id_token not a string": () => new Response(JSON.stringify({ ...idClaims(), id_token: { a: 1 } })),
    "unknown key": () => tokenResponse(idClaims(), sign("JWT", idClaims(), other)),
    "trusted kid, other key's signature": () => tokenResponse(idClaims(), sign("JWT", idClaims(), other, { kid: ca.kidForKey(primary.raw) })),
    "wrong typ": () => tokenResponse(idClaims(), sign("logout+jwt", idClaims())),
    "alg none": () => tokenResponse(idClaims(), sign("JWT", idClaims(), primary, { alg: "none" })),
    "tampered claims": () => {
      const [h, , sig] = sign("JWT", idClaims()).split(".");
      const body = Buffer.from(JSON.stringify(idClaims({ sub: "sub-evil" }))).toString("base64url");
      return tokenResponse(idClaims(), `${h}.${body}.${sig}`);
    },
  };
  for (const [name, respond] of Object.entries(cases)) {
    const c = client(async () => respond());
    await assert.rejects(c.exchange({ code: "c", verifier: "v", nonce: "n1" }), ca.CentralAuthError, name);
  }
  await assert.rejects(
    client(async () => new Response("{}", { status: 400 })).exchange({ code: "c", verifier: "v", nonce: "n1" }),
    ca.CodeExchangeRejectedError,
    "400 is a retry"
  );
  await assert.rejects(client(async () => new Response("", { status: 302, headers: { Location: "https://evil.example" } })).exchange({ code: "c", verifier: "v", nonce: "n1" }), ca.CentralAuthError, "redirect not followed");
  await assert.rejects(client(async () => new Response("x".repeat(70 * 1024))).exchange({ code: "c", verifier: "v", nonce: "n1" }), ca.CentralAuthError, "oversized");
  await assert.rejects(client(async () => { throw new Error("ECONNREFUSED"); }).exchange({ code: "c", verifier: "v", nonce: "n1" }), ca.CentralAuthError, "unreachable");
});

test("central-auth.Client: refuses to exist without a key resolver", () => {
  assert.throws(() => new ca.Client({}), TypeError);
});

test("central-auth: login transaction is sealed, single-state, and expires", () => {
  const key = crypto.randomBytes(32);
  const tx = ca.newTransaction("/admin/northwind?x=1", 1000);
  assert.equal(tx.challenge, crypto.createHash("sha256").update(tx.verifier).digest("base64url"), "S256 challenge");
  const sealed = ca.sealTransaction(tx, key);
  assert.deepEqual(ca.openTransaction(sealed, tx.state, key, 1000), { nonce: tx.nonce, verifier: tx.verifier, next: "/admin/northwind?x=1" });
  assert.throws(() => ca.openTransaction(sealed, "other-state", key, 1000), ca.AuthTransactionError, "state mismatch");
  assert.throws(() => ca.openTransaction(sealed, tx.state, crypto.randomBytes(32), 1000), ca.AuthTransactionError, "other key");
  assert.throws(() => ca.openTransaction(sealed, tx.state, key, 1000 + 600), ca.AuthTransactionError, "expired");
  const [body, mac] = sealed.split(".");
  const tampered = Buffer.from(JSON.stringify({ ...JSON.parse(Buffer.from(body, "base64url")), next: "//evil.example" })).toString("base64url");
  assert.throws(() => ca.openTransaction(`${tampered}.${mac}`, tx.state, key, 1000), ca.AuthTransactionError, "tampered");
  assert.throws(() => ca.openTransaction(undefined, tx.state, key, 1000), ca.AuthTransactionError, "missing cookie");
});

test("central-auth.safeLocalPath: same-host paths only", () => {
  assert.equal(ca.safeLocalPath("/admin/northwind?x=1"), "/admin/northwind?x=1");
  for (const raw of ["//evil.example", "/\\evil.example", "https://evil.example/", "admin", "", "/a\u0000b", "/a\nb", `/${"a".repeat(2100)}`, null, "/%2F%2Fevil.example"]) {
    const out = ca.safeLocalPath(raw);
    assert.ok(out.startsWith("/") && !out.startsWith("//"), `${JSON.stringify(raw)} → ${out}`);
    assert.notEqual(new URL(out, "https://pages.northwind.example").origin, "https://evil.example");
  }
  assert.equal(ca.safeLocalPath("//evil.example"), "/admin");
  // Dot-segment normalization must not produce an authority reference.
  assert.equal(ca.safeLocalPath("/a/..//evil.example/x"), "/admin");
  assert.equal(ca.safeLocalPath("/%2e//evil.example/x"), "/admin");
});

test("central-auth.loadConfig: validates every setting and derives the public URL", () => {
  const env = {
    AUTH_ISSUER_URL: ISSUER,
    DASHBOARD_HOST: "pages.northwind.example",
    AUTH_CLIENT_SECRET: "s".repeat(40),
    AUTH_SIGNING_PUBKEY: primary.raw.toString("base64"),
    PAGE_COOKIE_SECRET: "unit-test-secret",
  };
  const config = ca.loadConfig(env);
  assert.equal(config.publicUrl, "https://pages.northwind.example");
  assert.equal(config.clientId, "pages");
  assert.equal(config.cookieSecure, true);
  assert.equal(config.transactionKey.length, 32);
  const broken = {
    "http issuer": { AUTH_ISSUER_URL: "http://auth.northwind.example" },
    "issuer with path": { AUTH_ISSUER_URL: `${ISSUER}/x` },
    "short secret": { AUTH_CLIENT_SECRET: "short" },
    "colon in secret": { AUTH_CLIENT_SECRET: `${"s".repeat(40)}:` },
    "no pubkey": { AUTH_SIGNING_PUBKEY: "" },
    "bad pubkey": { AUTH_SIGNING_PUBKEY: "AAAA" },
    "bad previous pubkey": { AUTH_SIGNING_PREVIOUS_PUBKEYS: "AAAA" },
    "insecure cookie without dev flag": { PAGES_AUTH_COOKIE_SECURE: "0" },
    "no cookie secret": { PAGE_COOKIE_SECRET: "" },
    "public URL on another host": { PAGES_PUBLIC_URL: "https://pages.other.example" },
    "no dashboard host": { DASHBOARD_HOST: "", PAGES_PUBLIC_URL: "https://pages.northwind.example" },
    "insecure options in production": { NODE_ENV: "production", AUTH_ISSUER_URL: "http://127.0.0.1:9000", AUTH_ALLOW_INSECURE_HTTP: "1" },
  };
  for (const [name, override] of Object.entries(broken)) {
    assert.throws(() => ca.loadConfig({ ...env, ...override }), Error, name);
  }
  const dev = ca.loadConfig({ ...env, AUTH_ISSUER_URL: "http://127.0.0.1:9000", AUTH_ALLOW_INSECURE_HTTP: "1", PAGES_AUTH_COOKIE_SECURE: "0" });
  assert.equal(dev.cookieSecure, false);
});

test("central-auth.normalizeEmail: matches Auth's shape", () => {
  assert.equal(ca.normalizeEmail("  Ana@Northwind.Example "), "ana@northwind.example");
  for (const bad of ["", "ana", "@northwind.example", "ana@", "a@b@c", "ana@.example", "ana@example.", "a b@example.com", "a\u0007@example.com"]) {
    assert.throws(() => ca.normalizeEmail(bad), ca.CentralAuthError, bad);
  }
});

test("central-auth.stripTrailingSlashes: trims only the trailing run, fast on a long one", () => {
  assert.equal(ca.stripTrailingSlashes("https://auth.example///"), "https://auth.example");
  assert.equal(ca.stripTrailingSlashes("https://auth.example"), "https://auth.example");
  assert.equal(ca.stripTrailingSlashes("///"), "");
  const started = process.hrtime.bigint();
  assert.equal(ca.stripTrailingSlashes("/".repeat(200000) + "x"), "/".repeat(200000) + "x");
  assert.ok(process.hrtime.bigint() - started < 1_000_000_000n, "linear time");
});
