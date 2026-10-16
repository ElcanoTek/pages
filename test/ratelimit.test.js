// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/ratelimit.js's own fixed-window limiter. It replaced express-rate-limit
// 8.7.1 (draft-7 headers, default memory store, default IP key); the expected
// header strings and keys below were recorded from that package so the
// replacement is held to what clients and operators saw.

const test = require("node:test");
const assert = require("node:assert/strict");
const http = require("node:http");
const express = require("express");
const { createLimiter, ipKeyGenerator } = require("../lib/ratelimit");

// Minimal Express-like response double: records headers and the outcome.
function fakeRes() {
  const headers = {};
  return {
    headers,
    headersSent: false,
    setHeader(name, value) {
      headers[name.toLowerCase()] = value;
    },
  };
}

function clock(start = 1_000_000) {
  const state = { t: start };
  return { now: () => state.t, advance: (ms) => (state.t += ms) };
}

async function hit(limiter, req) {
  const res = fakeRes();
  let outcome = null;
  await new Promise((resolve) => {
    // A refusal calls the handler, which reports through res.limited.
    res.limited = () => {
      outcome = "limited";
      resolve();
    };
    limiter(req, res, (error) => {
      outcome = error ? { error } : "next";
      resolve();
    });
  });
  return { res, outcome };
}

function limiterWith(opts) {
  const c = clock();
  const limiter = createLimiter({
    windowMs: 60_000,
    limit: 3,
    handler: (_req, res) => res.limited(),
    now: c.now,
    ...opts,
  });
  return { limiter, clock: c };
}

test("ratelimit: draft-7 headers match express-rate-limit, Retry-After only when refused", async (t) => {
  const { limiter } = limiterWith();
  t.after(() => limiter.stop());
  const req = { ip: "192.0.2.10" };
  const seen = [];
  for (let i = 0; i < 5; i += 1) {
    const { res, outcome } = await hit(limiter, req);
    seen.push([outcome, res.headers.ratelimit, res.headers["ratelimit-policy"], res.headers["retry-after"]]);
  }
  assert.deepEqual(seen, [
    ["next", "limit=3, remaining=2, reset=60", "3;w=60", undefined],
    ["next", "limit=3, remaining=1, reset=60", "3;w=60", undefined],
    ["next", "limit=3, remaining=0, reset=60", "3;w=60", undefined],
    ["limited", "limit=3, remaining=0, reset=60", "3;w=60", "60"],
    ["limited", "limit=3, remaining=0, reset=60", "3;w=60", "60"],
  ]);
});

test("ratelimit: the window resets after windowMs, and reset counts down", async (t) => {
  const { limiter, clock: c } = limiterWith();
  t.after(() => limiter.stop());
  const req = { ip: "192.0.2.11" };
  for (let i = 0; i < 3; i += 1) assert.equal((await hit(limiter, req)).outcome, "next");
  c.advance(45_500);
  const refused = await hit(limiter, req);
  assert.equal(refused.outcome, "limited");
  assert.equal(refused.res.headers.ratelimit, "limit=3, remaining=0, reset=15", "seconds left, rounded up");
  assert.equal(refused.res.headers["retry-after"], "15");
  c.advance(14_500);
  const fresh = await hit(limiter, req);
  assert.equal(fresh.outcome, "next", "a new window starts at its first hit");
  assert.equal(fresh.res.headers.ratelimit, "limit=3, remaining=2, reset=60");
});

test("ratelimit: keys are independent; a custom keyGenerator and resetKey work", async (t) => {
  const { limiter } = limiterWith({ limit: 1, keyGenerator: async (req) => req.who });
  t.after(() => limiter.stop());
  assert.equal((await hit(limiter, { who: "northwind" })).outcome, "next");
  assert.equal((await hit(limiter, { who: "northwind" })).outcome, "limited");
  assert.equal((await hit(limiter, { who: "contoso" })).outcome, "next");
  limiter.resetKey("northwind");
  assert.equal((await hit(limiter, { who: "northwind" })).outcome, "next");
});

test("ratelimit: a keyGenerator error reaches next(err) instead of escaping", async (t) => {
  const failure = new Error("key lookup failed");
  const { limiter } = limiterWith({
    keyGenerator: () => {
      throw failure;
    },
  });
  t.after(() => limiter.stop());
  const { outcome } = await hit(limiter, { ip: "192.0.2.12" });
  assert.equal(outcome.error, failure);
});

test("ratelimit: headers are not set once a response has started", async (t) => {
  const { limiter } = limiterWith();
  t.after(() => limiter.stop());
  const res = fakeRes();
  res.headersSent = true;
  await new Promise((resolve) => limiter({ ip: "192.0.2.13" }, res, resolve));
  assert.deepEqual(res.headers, {});
});

test("ratelimit: expired windows are swept on an unref'd timer", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const c = clock();
  const limiter = createLimiter({ windowMs: 1000, limit: 1, handler: (_req, res) => res.limited(), now: c.now });
  t.after(() => limiter.stop());
  await hit(limiter, { ip: "192.0.2.14" });
  assert.equal((await hit(limiter, { ip: "192.0.2.14" })).outcome, "limited");
  await hit(limiter, { ip: "192.0.2.15" });
  assert.equal(limiter.size, 2);
  c.advance(500);
  t.mock.timers.tick(1000);
  assert.equal(limiter.size, 2, "windows still open are kept");
  c.advance(500);
  t.mock.timers.tick(1000);
  assert.equal(limiter.size, 0, "an address that stopped calling costs nothing after its window");
  assert.equal((await hit(limiter, { ip: "192.0.2.14" })).outcome, "next");
});

test("ratelimit: the sweep timer never holds the process open", () => {
  const { spawnSync } = require("node:child_process");
  const run = spawnSync(
    process.execPath,
    ["-e", "require('./lib/ratelimit'); setTimeout(() => {}, 10);"],
    { cwd: require("node:path").join(__dirname, ".."), timeout: 15_000, encoding: "utf8" }
  );
  assert.equal(run.status, 0, run.stderr);
});

test("ratelimit: IP keys match express-rate-limit's ipKeyGenerator (IPv6 by /56)", () => {
  // Recorded from express-rate-limit 8.7.1's ipKeyGenerator.
  const recorded = [
    ["127.0.0.1", "127.0.0.1"],
    ["::1", "::/56"],
    ["::", "::/56"],
    ["::ffff:127.0.0.1", "127.0.0.1"],
    ["::ffff:7f00:1", "127.0.0.1"],
    ["::1.2.3.4", "1.2.3.4"],
    ["::102:304", "::/56"],
    ["2001:db8::1", "2001:db8::/56"],
    ["2001:db8:1234:56ff::1", "2001:db8:1234:5600::/56"],
    ["2001:db8:1234:5700::1", "2001:db8:1234:5700::/56"],
    ["2001:DB8:1234:5678:9abc:def0:1234:5678", "2001:db8:1234:5600::/56"],
    ["fe80::1%eth0", "fe80::/56"],
    ["64:ff9b::1.2.3.4", "64:ff9b::/56"],
    [undefined, undefined],
  ];
  for (const [ip, key] of recorded) assert.equal(ipKeyGenerator(ip), key, String(ip));
  assert.equal(ipKeyGenerator("2001:db8:1234:5678::1", 64), "2001:db8:1234:5678::/64");
  assert.equal(ipKeyGenerator("2001:db8:1234:5678::1", 0), "2001:db8:1234:5678::1", "subnet 0 keys the address itself");
});

test("ratelimit: one IPv6 /56 shares a budget; the next /56 does not", async (t) => {
  const { limiter } = limiterWith({ limit: 2 });
  t.after(() => limiter.stop());
  assert.equal((await hit(limiter, { ip: "2001:db8:aa:bb00::1" })).outcome, "next");
  assert.equal((await hit(limiter, { ip: "2001:db8:aa:bbff:ffff::9" })).outcome, "next");
  assert.equal((await hit(limiter, { ip: "2001:db8:aa:bb12::7" })).outcome, "limited", "same /56, moved /64");
  assert.equal((await hit(limiter, { ip: "2001:db8:aa:bc00::1" })).outcome, "next", "neighbouring /56");
  assert.equal((await hit(limiter, { ip: "::ffff:192.0.2.20" })).outcome, "next");
  assert.equal((await hit(limiter, { ip: "192.0.2.20" })).outcome, "next");
  assert.equal((await hit(limiter, { ip: "192.0.2.20" })).outcome, "limited", "mapped and plain IPv4 share a key");
});

test("ratelimit: works as Express middleware with the real request IP", async (t) => {
  const app = express();
  const limiter = createLimiter({
    windowMs: 60_000,
    limit: 1,
    handler: (_req, res) => res.status(429).json({ code: "rate_limited" }),
  });
  t.after(() => limiter.stop());
  app.get("/", limiter, (_req, res) => res.json({ ok: true }));
  const server = http.createServer(app).listen(0, "127.0.0.1");
  t.after(() => server.close());
  await new Promise((resolve) => server.once("listening", resolve));
  const url = `http://127.0.0.1:${server.address().port}/`;
  const first = await fetch(url);
  assert.equal(first.status, 200);
  assert.equal(first.headers.get("ratelimit"), "limit=1, remaining=0, reset=60");
  const second = await fetch(url);
  assert.equal(second.status, 429);
  assert.equal(second.headers.get("retry-after"), "60");
  assert.deepEqual(await second.json(), { code: "rate_limited" });
});
