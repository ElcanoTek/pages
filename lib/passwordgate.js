// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/passwordgate.js — progressive PER-PAGE backoff for the content host's
// password form (PLAN §7). The per-IP rate limit (lib/ratelimit.js) stops a
// single-source guessing run, but a distributed attacker gets a fresh budget
// per address. This brake is shared across every source IP: each failed
// unlock on a page delays the 401 a little more, so the total guess rate per
// page stays bounded no matter how many addresses attack (~10.8k
// guesses/day/page at the 8s ceiling, shared globally).
//
// Deliberately NOT a hard lockout: a hard per-page lock would let an attacker
// deny the page to its real viewers. A progressive delay keeps legitimate
// access cheap (one or two typos ≈ an extra half second) while making
// large-scale guessing impractical, and a successful unlock resets the page.

const db = require("./db");

const WINDOW_MINUTES = 15; // failures quiet for this long reset the counter
const BASE_DELAY_MS = 500; // delay after the first failed attempt
const MAX_DELAY_MS = 8000; // ceiling — never harsher than this per attempt

// delayForFailures — pure backoff curve: 500ms, 1s, 2s, 4s, 8s, 8s, …
function delayForFailures(failures) {
  const n = Number(failures);
  if (!Number.isFinite(n) || n <= 0) return 0;
  return Math.min(MAX_DELAY_MS, BASE_DELAY_MS * 2 ** (n - 1));
}

// recordFailure — count a failed unlock for the page (shared across all
// source IPs) and return how long the caller should delay the 401, in ms.
// The counter resets when the page has been quiet for WINDOW_MINUTES.
async function recordFailure(pageId) {
  const { rows } = await db.query(
    `INSERT INTO page_password_failures (page_id, fail_count, window_start, last_fail_at)
     VALUES ($1, 1, now(), now())
     ON CONFLICT (page_id) DO UPDATE SET
       fail_count = CASE
         WHEN page_password_failures.last_fail_at < now() - ($2 * interval '1 minute')
           THEN 1
         ELSE page_password_failures.fail_count + 1
       END,
       window_start = CASE
         WHEN page_password_failures.last_fail_at < now() - ($2 * interval '1 minute')
           THEN now()
         ELSE page_password_failures.window_start
       END,
       last_fail_at = now()
     RETURNING fail_count`,
    [pageId, WINDOW_MINUTES]
  );
  return delayForFailures(rows[0] && rows[0].fail_count);
}

// clearFailures — a successful unlock resets the page. Best-effort: a stale
// counter only means the next typo waits slightly longer, so never throw.
async function clearFailures(pageId) {
  try {
    await db.query(`DELETE FROM page_password_failures WHERE page_id = $1`, [pageId]);
  } catch (err) {
    console.error("passwordgate: failed to clear failures:", err.message);
  }
}

// ── portals ──────────────────────────────────────────────────────────────────
// Same curve, same window, same "delay the 401, never lock anyone out" rule, on
// its own counter (migrations/019). A portal password opens every dashboard in
// the portal, so it is worth more than any one page's password and gets its own
// budget rather than borrowing a member's.
//
// The counter is keyed by PORTAL, not by the URL the attempt arrived at, so that
// every door which tests a portal password charges the same budget. Once a member
// page's own form also tries the portal passwords of the portals containing it,
// a per-door counter would hand an attacker N parallel budgets against a single
// secret; the caller is expected to apply max(pageDelay, portalDelay) when one
// submission tested both kinds of credential.

async function recordPortalFailure(portalId) {
  const { rows } = await db.query(
    `INSERT INTO portal_password_failures (portal_id, fail_count, window_start, last_fail_at)
     VALUES ($1, 1, now(), now())
     ON CONFLICT (portal_id) DO UPDATE SET
       fail_count = CASE
         WHEN portal_password_failures.last_fail_at < now() - ($2 * interval '1 minute')
           THEN 1
         ELSE portal_password_failures.fail_count + 1
       END,
       window_start = CASE
         WHEN portal_password_failures.last_fail_at < now() - ($2 * interval '1 minute')
           THEN now()
         ELSE portal_password_failures.window_start
       END,
       last_fail_at = now()
     RETURNING fail_count`,
    [portalId, WINDOW_MINUTES]
  );
  return delayForFailures(rows[0] && rows[0].fail_count);
}

async function clearPortalFailures(portalId) {
  try {
    await db.query(`DELETE FROM portal_password_failures WHERE portal_id = $1`, [portalId]);
  } catch (err) {
    console.error("passwordgate: failed to clear portal failures:", err.message);
  }
}

module.exports = {
  WINDOW_MINUTES,
  BASE_DELAY_MS,
  MAX_DELAY_MS,
  delayForFailures,
  recordFailure,
  clearFailures,
  recordPortalFailure,
  clearPortalFailures,
};
