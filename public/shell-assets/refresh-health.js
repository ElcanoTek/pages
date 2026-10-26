// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// refresh-health.js — how the staff screens say whether a page's scheduled
// refresh is keeping up, and the schedule presets the detail screen edits.
// Pure, so the index, the detail and the unit tests read one vocabulary.
//
// The verdict itself is the server's (internal/versions/refreshhealth.go):
// these helpers only name it. Nothing here is ever rendered on a client page
// or a partner portal.

(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.PagesRefreshHealth = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  // `kind` is a primitives.js status kind. Red for a run that failed, amber for
  // one that is late or finding nothing new; ok and unscheduled carry no alarm.
  const HEALTH = {
    blocked: { kind: "error", label: "Blocked", rank: 0, attention: true },
    overdue: { kind: "warning", label: "Overdue", rank: 1, attention: true },
    source_stale: { kind: "warning", label: "Source stuck", rank: 2, attention: true },
    ok: { kind: "live", label: "On schedule", rank: 3, attention: false },
    unscheduled: { kind: "neutral", label: "Not scheduled", rank: 4, attention: false },
  };

  function healthOf(pageOrHealth) {
    if (!pageOrHealth || typeof pageOrHealth !== "object") return null;
    return pageOrHealth.refresh_health || (pageOrHealth.health ? pageOrHealth : null);
  }

  // { kind, label } for a health block, or null when there is none.
  function describeHealth(pageOrHealth) {
    const health = healthOf(pageOrHealth);
    if (!health) return null;
    return HEALTH[health.health] || HEALTH.unscheduled;
  }

  function needsAttention(pageOrHealth) {
    const entry = describeHealth(pageOrHealth);
    return Boolean(entry && entry.attention);
  }

  // The rows a human has to look at, worst first, keeping the list's own order
  // within one state.
  function attentionRows(pages) {
    return (Array.isArray(pages) ? pages : [])
      .map((page, index) => ({ page, index }))
      .filter(({ page }) => needsAttention(page))
      .sort((a, b) => describeHealth(a.page).rank - describeHealth(b.page).rank || a.index - b.index)
      .map(({ page }) => page);
  }

  // ── no refresh set ─────────────────────────────────────────────────────────
  // Not a health. A page with no schedule is never judged, and a finished
  // campaign legitimately sits there for good. But a live page whose data is
  // more than a week old and that nobody gave a refresh schedule may also be a
  // campaign still delivering whose refresh task was never installed, so the
  // server raises a hint beside the verdict (refresh_health.unscheduled_stale,
  // internal/versions/refreshhealth.go). Grey, never counted as needing
  // attention, and worded so that "it has ended" is a fine answer. The pill
  // says "No refresh" because a pill does not wrap and the index's Last update
  // column has 104px for it ("No refresh set" measures 123); the filter that
  // lists these pages says the whole phrase.
  const NO_REFRESH = { kind: "neutral", label: "No refresh", filter: "No refresh set" };

  function unscheduledStale(pageOrHealth) {
    const health = healthOf(pageOrHealth);
    return Boolean(health && health.unscheduled_stale === true);
  }

  // The sentence the row and its hover carry; "" when there is no hint. The
  // CLI says the same words (versions.UnscheduledStaleHint).
  function unscheduledStaleHint(pageOrHealth) {
    if (!unscheduledStale(pageOrHealth)) return "";
    const days = healthOf(pageOrHealth).days_since_source;
    const age = Number.isInteger(days) ? `data ${days} ${days === 1 ? "day" : "days"} old` : "data over a week old";
    return `No refresh schedule · ${age} — set a schedule or ignore if the campaign has ended`;
  }

  // The hinted rows, the oldest data first, keeping the list's own order on a
  // tie.
  function unscheduledStaleRows(pages) {
    const age = (page) => {
      const days = healthOf(page).days_since_source;
      return Number.isInteger(days) ? days : 0;
    };
    return (Array.isArray(pages) ? pages : [])
      .map((page, index) => ({ page, index }))
      .filter(({ page }) => unscheduledStale(page))
      .sort((a, b) => age(b.page) - age(a.page) || a.index - b.index)
      .map(({ page }) => page);
  }

  // ── schedule presets ───────────────────────────────────────────────────────
  // Every time is UTC, as the server reads the cron. The presets cover what
  // refreshes are actually scheduled at; anything else is a custom cron, which
  // the server validates.

  const DAYS = [
    ["1", "Monday"], ["2", "Tuesday"], ["3", "Wednesday"], ["4", "Thursday"],
    ["5", "Friday"], ["6", "Saturday"], ["0", "Sunday"],
  ];
  const DAY_NAME = Object.fromEntries(DAYS);
  DAY_NAME["7"] = "Sunday";

  const pad = (n) => String(n).padStart(2, "0");

  // "30 14 * * 1-5" → { preset: "weekdays", time: "14:30", day: "1", cron }.
  function presetFromCron(cron) {
    const text = String(cron || "").trim().split(/\s+/).join(" ");
    if (!text) return { preset: "none", time: "09:00", day: "1", cron: "" };
    const match = /^(\d{1,2}) (\d{1,2}) \* \* (\*|1-5|[0-7])$/.exec(text);
    if (match && Number(match[1]) <= 59 && Number(match[2]) <= 23) {
      const time = `${pad(match[2])}:${pad(match[1])}`;
      if (match[3] === "*") return { preset: "daily", time, day: "1", cron: text };
      if (match[3] === "1-5") return { preset: "weekdays", time, day: "1", cron: text };
      return { preset: "weekly", time, day: match[3] === "7" ? "0" : match[3], cron: text };
    }
    return { preset: "custom", time: "09:00", day: "1", cron: text };
  }

  // The inverse: { cron } (null for "none"), or { error } in the reader's words.
  function cronFromPreset({ preset, time, day, cron } = {}) {
    if (preset === "none") return { cron: null };
    if (preset === "custom") {
      const text = String(cron || "").trim().split(/\s+/).join(" ");
      return text ? { cron: text } : { error: "Enter a cron expression, or pick a preset." };
    }
    if (!["daily", "weekdays", "weekly"].includes(preset)) return { error: "Pick a schedule." };
    const match = /^(\d{1,2}):(\d{2})$/.exec(String(time || "").trim());
    if (!match || Number(match[1]) > 23 || Number(match[2]) > 59) return { error: "Enter a time as HH:MM (UTC)." };
    const at = `${Number(match[2])} ${Number(match[1])}`;
    if (preset === "daily") return { cron: `${at} * * *` };
    if (preset === "weekdays") return { cron: `${at} * * 1-5` };
    return DAY_NAME[String(day)] ? { cron: `${at} * * ${day}` } : { error: "Pick a day of the week." };
  }

  // "Weekdays at 14:30 UTC" — what the schedule means, for a human.
  function describeSchedule(cron) {
    const parsed = presetFromCron(cron);
    switch (parsed.preset) {
      case "none": return "Not scheduled";
      case "daily": return `Daily at ${parsed.time} UTC`;
      case "weekdays": return `Weekdays at ${parsed.time} UTC`;
      case "weekly": return `${DAY_NAME[parsed.day]}s at ${parsed.time} UTC`;
      default: return `${parsed.cron} (cron, UTC)`;
    }
  }

  return {
    HEALTH, NO_REFRESH, DAYS, describeHealth, needsAttention, attentionRows,
    unscheduledStale, unscheduledStaleHint, unscheduledStaleRows,
    presetFromCron, cronFromPreset, describeSchedule,
  };
});
