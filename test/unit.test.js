"use strict";
// Unit tests for the pure, security-critical logic (no DB needed):
// signed /raw tokens and Flag render injection. Run: npm test

const { test } = require("node:test");
const assert = require("node:assert/strict");

process.env.RAW_TOKEN_SECRET = "test-secret-do-not-use-in-prod";
const rawtoken = require("../lib/rawtoken");
const render = require("../lib/render");
const versions = require("../lib/versions");

test("rawtoken: round-trips and preserves binding", () => {
  const t = rawtoken.mint({ pageId: 7, versionId: 42, purpose: "view", renderMode: "themed" });
  const c = rawtoken.verify(t);
  assert.equal(c.pid, 7);
  assert.equal(c.vid, 42);
  assert.equal(c.purpose, "view");
  assert.equal(c.mode, "themed");
  assert.ok(c.sid && c.exp);
});

test("rawtoken: rejects a tampered signature", () => {
  const t = rawtoken.mint({ pageId: 1, versionId: 1, purpose: "view", renderMode: "themed" });
  assert.equal(rawtoken.verify(t.slice(0, -3) + "AAA"), null);
});

test("rawtoken: rejects a tampered payload (purpose escalation)", () => {
  // Swap the body for {purpose:'edit'...} but keep the old signature → must fail.
  const t = rawtoken.mint({ pageId: 1, versionId: 1, purpose: "view", renderMode: "themed" });
  const sig = t.split(".")[1];
  const forgedBody = Buffer.from(JSON.stringify({
    pid: 1, vid: 1, purpose: "edit", mode: "themed", sid: "x", exp: Math.floor(Date.now() / 1000) + 300,
  })).toString("base64url");
  assert.equal(rawtoken.verify(`${forgedBody}.${sig}`), null);
});

test("rawtoken: rejects expired tokens", () => {
  const t = rawtoken.mint({ pageId: 1, versionId: 1, purpose: "view", renderMode: "themed" }, -1);
  assert.equal(rawtoken.verify(t), null);
});

test("rawtoken: rejects garbage", () => {
  for (const bad of ["", "no-dot", ".", "a.b", null, undefined]) {
    assert.equal(rawtoken.verify(bad), null);
  }
});

test("render: raw mode is byte-for-byte verbatim", () => {
  const html = "<!doctype html><html><head></head><body><script>chart()</script></body></html>";
  assert.equal(render.renderVersion({ render_mode: "raw", html }), html);
});

test("render: themed mode injects Flag into <head> without touching the body", () => {
  const html = "<!doctype html><html><head><title>x</title></head><body><canvas id=c></canvas><script>chart()</script></body></html>";
  const out = render.renderVersion({ render_mode: "themed", html, override_css: "" });
  // Flag assets injected, tagged for the source editor to strip:
  assert.match(out, /design-tokens\.css" data-flag-injected/);
  assert.match(out, /theme-controller\.js" data-flag-injected/);
  // Body preserved verbatim (charts untouched — the whole point):
  assert.match(out, /<canvas id=c><\/canvas><script>chart\(\)<\/script>/);
});

test("render: themed mode injects a client theme override", () => {
  const out = render.renderVersion({
    render_mode: "themed",
    html: "<html><head></head><body>hi</body></html>",
    override_css: ":root{--color-primary:#c8102e}",
  });
  assert.match(out, /--color-primary:#c8102e/);
  assert.match(out, /<style data-flag-injected>/);
});

test("render: themed mode synthesizes a head when there is none", () => {
  const out = render.renderVersion({ render_mode: "themed", html: "<p>bare fragment</p>", override_css: "" });
  assert.match(out, /<head>/);
  assert.match(out, /design-tokens\.css/);
  assert.match(out, /<p>bare fragment<\/p>/);
});

// ── version state machine: pure helpers (no DB) ──────────────────────────────

test("versions.sha256: stable and content-sensitive", () => {
  assert.equal(versions.sha256("hello"), versions.sha256("hello"));
  assert.notEqual(versions.sha256("hello"), versions.sha256("hello!"));
  assert.match(versions.sha256("x"), /^[0-9a-f]{64}$/);
});

test("versions.normalizeSlug: accepts flat + nested, lowercases, rejects junk", () => {
  assert.equal(versions.normalizeSlug("Omnicom"), "omnicom");
  assert.equal(versions.normalizeSlug("omnicom/q2-report"), "omnicom/q2-report");
  assert.equal(versions.normalizeSlug(" acme_co "), "acme_co");
  for (const bad of ["", "/leading", "trailing/", "has space", "bad//seg", "../etc", null]) {
    assert.throws(() => versions.normalizeSlug(bad), /slug/i, `should reject: ${JSON.stringify(bad)}`);
  }
});
