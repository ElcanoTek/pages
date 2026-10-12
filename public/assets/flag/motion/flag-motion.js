/* SPDX-License-Identifier: BUSL-1.1
 * Copyright (c) 2026 ElcanoTek, Inc.
 *
 * Flag Motion — live-dashboard and motion kit for Elcano Pages. Opt-in: a page
 * includes
 *
 *   <link rel="stylesheet" href="/assets/flag/motion/flag-motion.css">
 *   <script src="/assets/flag/motion/flag-motion.js" defer></script>
 *
 * and gets, declaratively (data-fm-* attributes) or from script (window.FlagMotion):
 *
 *   Dashboards  count-up numbers, scroll reveals, a freshness chip read from the
 *               page's own refresh stamps, animated themed SVG charts (line, area,
 *               bar, ranked bars, donut, sparkline) with tooltips and a data table,
 *               and provenance — click any number to see what it is, how it is
 *               computed and which source it comes from.
 *   Motion      an explainer player: scenes written as JSON, drawn on a canvas,
 *               played/scrubbed in the page, and downloaded as an MP4 (WebM where
 *               the browser cannot record MP4) — animation as code.
 *
 * Specs can point at the page's managed blocks ("data:path" / "config:path"), so
 * a chart or an explainer bound to #pages-data re-renders with every scheduled
 * refresh with no change to the page's HTML. The content sandbox forbids network
 * access (connect-src 'none'): the kit fetches nothing and needs nothing remote.
 * Full reference: docs/MOTION.md.
 */
(function (root, factory) {
  const api = factory(root);
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.FlagMotion = api;
})(typeof window !== "undefined" ? window : globalThis, function (win) {
  "use strict";

  const VERSION = "1.0.0";
  const doc = win.document;
  const SVG_NS = "http://www.w3.org/2000/svg";
  const MAX_SERIES = 6;
  const LIGHT_SERIES = ["#4a3aa7", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#2a78d6"];
  const DARK_SERIES = ["#9085e9", "#d95926", "#199e70", "#c98500", "#d55181", "#3987e5"];
  const OTHER_COLOR = "var(--fm-other)";

  // ── Formatting (pure; also exported for tests and page scripts) ─────────────
  const numberFormats = new Map();
  function nf(options) {
    const key = JSON.stringify(options);
    if (!numberFormats.has(key)) numberFormats.set(key, new Intl.NumberFormat("en-US", options));
    return numberFormats.get(key);
  }
  function parseFormat(format) {
    const match = /^([a-z]+)(\d)?$/.exec(String(format || "num"));
    return { kind: match ? match[1] : "num", decimals: match && match[2] != null ? Number(match[2]) : null };
  }
  // usd usd0 · usdc (compact $1.2M) · int · num num1 · compact · pct pct2 (fraction → %) · x (multiple)
  function formatValue(value, format) {
    if (value == null || typeof value !== "number" || !Number.isFinite(value)) return "N/A";
    const { kind, decimals } = parseFormat(format);
    const fixed = (d) => ({ minimumFractionDigits: d, maximumFractionDigits: d });
    switch (kind) {
      case "usd": return nf({ style: "currency", currency: "USD", ...fixed(decimals ?? 2) }).format(value);
      case "usdc": return nf({ style: "currency", currency: "USD", notation: "compact", maximumFractionDigits: decimals ?? 1 }).format(value);
      case "int": return nf({ maximumFractionDigits: 0 }).format(value);
      case "compact": return nf({ notation: "compact", maximumFractionDigits: decimals ?? 1 }).format(value);
      case "pct": return nf(fixed(decimals ?? 1)).format(value * 100) + "%";
      case "x": return nf(fixed(decimals ?? 2)).format(value) + "x";
      default: return nf({ minimumFractionDigits: decimals ?? 0, maximumFractionDigits: decimals ?? 2 }).format(value);
    }
  }
  const ISO_DAY = /^\d{4}-\d{2}-\d{2}$/;
  function formatLabel(label, style = "short") {
    if (typeof label === "string" && ISO_DAY.test(label)) {
      const date = new Date(label + "T00:00:00Z");
      return date.toLocaleDateString("en-US", style === "long"
        ? { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" }
        : { month: "short", day: "numeric", timeZone: "UTC" });
    }
    return String(label);
  }
  function relativeAge(ms) {
    const minutes = Math.round(ms / 60000);
    if (minutes < 1) return "just now";
    if (minutes < 60) return minutes + " min ago";
    const hours = Math.round(minutes / 60);
    if (hours < 48) return hours + " h ago";
    return Math.round(hours / 24) + " days ago";
  }
  function niceTicks(min, max, count = 4) {
    if (!Number.isFinite(min) || !Number.isFinite(max)) { min = 0; max = 1; }
    if (min === max) { max = min === 0 ? 1 : min + Math.abs(min) * 0.5; }
    const span = max - min;
    const raw = span / count;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => span / s <= count) || 10 * mag;
    const lo = Math.floor(min / step) * step;
    const hi = Math.ceil(max / step) * step;
    const ticks = [];
    for (let v = lo; v <= hi + step / 2; v += step) ticks.push(Number(v.toPrecision(12)));
    return { min: lo, max: hi, step, ticks };
  }

  // ── Small DOM helpers ───────────────────────────────────────────────────────
  function h(tag, attrs, children) {
    const el = doc.createElement(tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (value == null || value === false) continue;
      if (key === "text") el.textContent = value;
      else if (key === "style" && typeof value === "object") Object.assign(el.style, value);
      else el.setAttribute(key, value === true ? "" : String(value));
    }
    for (const child of [].concat(children || [])) if (child != null) el.append(child);
    return el;
  }
  function s(tag, attrs, parent) {
    const el = doc.createElementNS(SVG_NS, tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (value == null) continue;
      if (key === "text") el.textContent = value;
      else el.setAttribute(key, String(value));
    }
    if (parent) parent.append(el);
    return el;
  }
  const reducedMotion = () => !!(win.matchMedia && win.matchMedia("(prefers-reduced-motion: reduce)").matches);
  const ease = (p) => 1 - Math.pow(1 - p, 3);
  const easeInOut = (p) => (p < 0.5 ? 4 * p * p * p : 1 - Math.pow(-2 * p + 2, 3) / 2);
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

  function tween(duration, onFrame, onDone) {
    if (reducedMotion() || duration <= 0) { onFrame(1); if (onDone) onDone(); return () => {}; }
    let start = null, id = 0, stopped = false;
    const step = (now) => {
      if (stopped) return;
      if (start == null) start = now;
      const p = clamp((now - start) / duration, 0, 1);
      onFrame(p);
      if (p < 1) id = win.requestAnimationFrame(step);
      else if (onDone) onDone();
    };
    id = win.requestAnimationFrame(step);
    return () => { stopped = true; win.cancelAnimationFrame(id); };
  }
  function inView(el, callback, threshold = 0.2) {
    if (!("IntersectionObserver" in win)) { callback(); return; }
    const io = new win.IntersectionObserver((entries) => {
      if (entries.some(e => e.isIntersecting)) { io.disconnect(); callback(); }
    }, { threshold });
    io.observe(el);
  }
  function readJsonBlock(id) {
    const el = doc.getElementById(id);
    if (!el) return null;
    try { return JSON.parse(el.textContent); } catch { return null; }
  }
  function getPath(object, path) {
    return String(path || "").split(".").filter(Boolean).reduce((o, k) => (o == null ? undefined : o[k]), object);
  }
  // "#id" → that JSON block; "data:a.b" → #pages-data envelope.data.a.b;
  // "config:a.b" → #pages-config.a.b; anything else is parsed as inline JSON.
  function resolveSpec(source) {
    if (source == null) return null;
    if (typeof source === "object") return source;
    const text = String(source).trim();
    if (text.startsWith("#")) return readJsonBlock(text.slice(1));
    if (text.startsWith("data:")) { const env = envelope(); return env ? getPath(env.data, text.slice(5)) : null; }
    if (text.startsWith("config:")) return getPath(readJsonBlock("pages-config"), text.slice(7));
    try { return JSON.parse(text); } catch { return null; }
  }

  // ── Freshness: the page's own refresh stamps ────────────────────────────────
  function envelope() {
    const env = readJsonBlock("pages-data");
    if (!env || typeof env !== "object") return null;
    return { refreshedAt: env.refreshed_at || null, sourceAsOf: env.source_as_of || null, data: env.data };
  }
  function freshness(env = envelope(), now = Date.now()) {
    const valid = (iso) => { const t = Date.parse(iso || ""); return Number.isFinite(t) && t > 86400000 ? t : null; };
    const refreshed = env ? valid(env.refreshedAt) : null;
    const through = env ? valid(env.sourceAsOf) : null;
    if (!refreshed && !through) return { state: "none", text: "Awaiting first data", title: "" };
    const age = refreshed ? now - refreshed : Infinity;
    // The covered DAY. A page that states it (DATA.dataThrough) is the authority.
    // Otherwise source_as_of is usually an end-of-day instant in the source's own
    // timezone (2026-10-09T03:59:59Z is the end of Oct 8 in New York), so read the
    // day twelve hours earlier: right for end-of-day stamps from UTC-11 to UTC+11.
    const stated = env && env.data && typeof env.data.dataThrough === "string" && ISO_DAY.test(env.data.dataThrough) ? env.data.dataThrough : null;
    const throughDay = stated || (through ? new Date(through - 12 * 3600e3).toISOString().slice(0, 10) : null);
    const throughText = throughDay ? formatLabel(throughDay, "long") : null;
    const parts = [];
    if (refreshed) parts.push("Updated " + relativeAge(age));
    if (throughText) parts.push("data through " + throughText);
    return {
      state: age <= 36 * 3600e3 ? "live" : age > 72 * 3600e3 ? "stale" : "recent",
      updated: refreshed ? "Updated " + relativeAge(age) : null,
      through: throughText,
      text: parts.join(" · "),
      title: [refreshed ? "Refreshed " + new Date(refreshed).toISOString() : null,
        through ? "Source coverage through " + new Date(through).toISOString() : null].filter(Boolean).join("\n"),
    };
  }
  function renderFreshness(el) {
    const info = freshness();
    el.classList.add("fm-fresh");
    el.classList.toggle("fm-live", info.state === "live");
    el.classList.toggle("fm-stale", info.state === "stale");
    el.textContent = "";
    el.append(h("span", { class: "fm-dot", "aria-hidden": "true" }));
    if (info.updated) {
      el.append(h("span", { class: "fm-fresh-label", text: info.updated }));
      if (info.through) el.append(h("span", { text: "· data through " + info.through }));
    } else {
      el.append(h("span", { class: "fm-fresh-label", text: info.text }));
    }
    if (info.title) el.title = info.title;
  }

  // ── Count-up and reveal ─────────────────────────────────────────────────────
  function count(el, target, options = {}) {
    const format = options.format || el.getAttribute("data-fm-format") || "num";
    const from = options.from ?? Number(el.getAttribute("data-fm-from") || 0);
    const duration = options.duration ?? Number(el.getAttribute("data-fm-duration") || 1300);
    const final = formatValue(target, format);
    el.setAttribute("aria-label", final);
    if (!Number.isFinite(target)) { el.textContent = final; return; }
    el.textContent = formatValue(from, format);
    tween(duration, (p) => { el.textContent = formatValue(from + (target - from) * ease(p), format); },
      () => { el.textContent = final; });
  }
  function countTarget(el) {
    const attr = el.getAttribute("data-fm-count");
    if (attr !== null && attr.trim() !== "") return Number(attr);
    const text = el.textContent || "";
    const value = parseFloat(text.replace(/[^0-9.\-]/g, ""));
    return /%\s*$/.test(text.trim()) && parseFormat(el.getAttribute("data-fm-format")).kind === "pct" ? value / 100 : value;
  }

  // ── Provenance: what a number is and where it came from ─────────────────────
  function provenanceDoc() {
    return readJsonBlock("pages-provenance") || (readJsonBlock("pages-config") || {}).provenance || null;
  }
  function schemaDescription(key) {
    const schema = readJsonBlock("pages-data-schema");
    let found = null;
    (function walk(node) {
      if (found || !node || typeof node !== "object") return;
      if (node.properties && node.properties[key] && node.properties[key].description) { found = node.properties[key].description; return; }
      for (const value of Object.values(node)) walk(value);
    })(schema);
    return found;
  }
  let openPop = null;
  function closeProvenance() {
    if (!openPop) return;
    const { pop, anchor, cleanup } = openPop;
    openPop = null;
    cleanup();
    pop.remove();
    anchor.setAttribute("aria-expanded", "false");
    if (anchor.isConnected) anchor.focus({ preventScroll: true });
  }
  function openProvenance(anchor) {
    const key = anchor.getAttribute("data-fm-metric");
    if (openPop && openPop.anchor === anchor) { closeProvenance(); return; }
    closeProvenance();
    const prov = provenanceDoc() || {};
    const metric = (prov.metrics || {})[key] || {};
    const definition = metric.definition || schemaDescription(key);
    const sourceRef = metric.source;
    const source = typeof sourceRef === "string" ? (prov.sources || {})[sourceRef] || { label: sourceRef } : sourceRef || null;
    const rows = [];
    if (metric.formula) rows.push(["Formula", h("code", { text: metric.formula })]);
    if (source && (source.label || source.location)) rows.push(["Source", [source.label || "", source.location ? h("div", {}, h("code", { text: source.location })) : null]]);
    if (metric.filter) rows.push(["Scope", metric.filter]);
    if (source && source.cadence) rows.push(["Refresh", source.cadence]);
    if (metric.notes) rows.push(["Note", metric.notes]);
    const info = freshness();
    const pop = h("div", { class: "fm-pop", role: "dialog", "aria-label": (metric.label || key) + " — how this number is computed", tabindex: "-1" }, [
      h("button", { class: "fm-pop-close", type: "button", "aria-label": "Close", text: "×" }),
      h("h4", { text: metric.label || anchor.getAttribute("data-fm-label") || key }),
      h("div", { class: "fm-pop-value", text: anchor.hasAttribute("data-fm-target")
        ? formatValue(Number(anchor.getAttribute("data-fm-target")), anchor.getAttribute("data-fm-format") || "num")
        : (anchor.textContent || "").trim() }),
      definition ? h("p", { text: definition }) : h("p", { text: "No definition recorded for this number yet." }),
      rows.length ? h("dl", {}, rows.flatMap(([dt, dd]) => [h("dt", { text: dt }), h("dd", {}, dd)])) : null,
      h("div", { class: "fm-pop-fresh", text: info.text }),
    ]);
    doc.body.append(pop);
    const rect = anchor.getBoundingClientRect();
    const width = pop.offsetWidth, height = pop.offsetHeight;
    const viewportW = doc.documentElement.clientWidth;
    let left = clamp(rect.left + win.scrollX, 12 + win.scrollX, win.scrollX + viewportW - width - 12);
    let top = rect.bottom + win.scrollY + 8;
    if (rect.bottom + height + 16 > win.innerHeight && rect.top > height + 16) top = rect.top + win.scrollY - height - 8;
    pop.style.left = left + "px";
    pop.style.top = top + "px";
    anchor.setAttribute("aria-expanded", "true");
    const onKey = (e) => { if (e.key === "Escape") { e.preventDefault(); closeProvenance(); } };
    const onDown = (e) => { if (!pop.contains(e.target) && !anchor.contains(e.target)) closeProvenance(); };
    doc.addEventListener("keydown", onKey);
    doc.addEventListener("pointerdown", onDown, true);
    pop.querySelector(".fm-pop-close").addEventListener("click", closeProvenance);
    openPop = { pop, anchor, cleanup: () => { doc.removeEventListener("keydown", onKey); doc.removeEventListener("pointerdown", onDown, true); } };
    pop.focus({ preventScroll: true });
  }
  function bindMetric(el) {
    if (el.hasAttribute("data-fm-bound")) return;
    el.setAttribute("data-fm-bound", "");
    if (!el.matches("a,button,input,select,textarea") && !el.hasAttribute("tabindex")) el.tabIndex = 0;
    el.setAttribute("role", el.getAttribute("role") || "button");
    el.setAttribute("aria-haspopup", "dialog");
    el.setAttribute("aria-expanded", "false");
    el.addEventListener("click", (e) => { e.preventDefault(); openProvenance(el); });
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openProvenance(el); }
    });
  }

  // ── Charts ──────────────────────────────────────────────────────────────────
  const seriesColor = (i) => `var(--fm-s${i + 1})`;
  function normalizeChart(raw) {
    const spec = Object.assign({ type: "line", format: "num" }, raw || {});
    spec.type = String(spec.type).toLowerCase();
    if (spec.type === "donut" || spec.type === "hbar") {
      let items = spec.items;
      if (!items && spec.labels && spec.series && spec.series[0]) {
        items = spec.labels.map((label, i) => ({ label, value: spec.series[0].values[i] }));
      }
      spec.items = (items || []).filter(it => it && Number.isFinite(it.value));
    } else {
      spec.labels = (spec.labels || []).slice();
      spec.series = (spec.series || []).slice(0, MAX_SERIES).map((ser, i) => ({
        name: ser.name || `Series ${i + 1}`,
        values: (ser.values || []).map(v => (Number.isFinite(v) ? v : null)),
        color: ser.color || seriesColor(i),
      }));
    }
    spec.height = Number(spec.height) || { donut: 240, spark: 44 }[spec.type] || 260;
    return spec;
  }
  function chartRows(spec) {
    if (spec.items) {
      const total = spec.items.reduce((a, it) => a + it.value, 0);
      return { head: [spec.xLabel || "", spec.valueLabel || "Value", "Share"],
        rows: spec.items.map(it => [formatLabel(it.label, "long"), formatValue(it.value, spec.format), formatValue(total ? it.value / total : null, "pct1")]) };
    }
    return { head: [spec.xLabel || ""].concat(spec.series.map(x => x.name)),
      rows: spec.labels.map((label, i) => [formatLabel(label, "long")].concat(spec.series.map(x => formatValue(x.values[i], spec.format)))) };
  }

  function chart(el, rawSpec) {
    let spec = normalizeChart(rawSpec);
    let lastWidth = 0, revealed = false, observer = null;
    el.classList.add("fm-chart");

    function render(animate) {
      el.textContent = "";
      const isSpark = spec.type === "spark";
      const multi = spec.series && spec.series.length > 1;
      if (!isSpark && (spec.title || multi)) {
        const head = h("div", { class: "fm-chart-head" }, [spec.title ? h("h3", { class: "fm-chart-title", text: spec.title }) : h("span")]);
        if (multi) head.append(h("div", { class: "fm-legend" }, spec.series.map(ser =>
          h("span", {}, [h("i", { style: { background: ser.color } }), ser.name]))));
        el.append(head);
      }
      const plot = h("div", { class: "fm-plot" });
      el.append(plot);
      const width = Math.max(isSpark ? 60 : 260, Math.floor(plot.clientWidth || el.clientWidth || 600));
      lastWidth = width;
      const svg = s("svg", { viewBox: `0 0 ${width} ${spec.height}`, height: spec.height, role: "img",
        "aria-label": spec.title || spec.ariaLabel || "Chart" });
      plot.append(svg);
      const tip = h("div", { class: "fm-tip", role: "status", "aria-live": "polite" });
      plot.append(tip);
      const ctx = { el, plot, svg, tip, width, height: spec.height };
      ({ line: drawXY, area: drawXY, bar: drawBars, hbar: drawHBars, donut: drawDonut, spark: drawXY }[spec.type] || drawXY)(spec, ctx);
      if (!isSpark) {
        const info = freshness();
        const foot = [];
        // A source line that already states its coverage keeps it; the kit adds none.
        if (spec.source) foot.push(h("span", { text: "Source: " + spec.source + (info.through && !/\bthrough\b/i.test(spec.source) ? " · data through " + info.through : "") }));
        if (spec.freshness === true && info.state !== "none") foot.push(h("span", { text: info.text, title: info.title }));
        if (foot.length) el.append(h("div", { class: "fm-chart-foot" }, foot));
        if (spec.table !== false) {
          const { head, rows } = chartRows(spec);
          el.append(h("details", { class: "fm-table" }, [
            h("summary", { text: "Show data" }),
            h("div", { class: "fm-table-wrap" }, h("table", {}, [
              h("thead", {}, h("tr", {}, head.map(t => h("th", { text: t })))),
              h("tbody", {}, rows.map(r => h("tr", {}, r.map(t => h("td", { text: t }))))),
            ])),
          ]));
        }
      }
      if (animate && !reducedMotion()) {
        el.classList.add("fm-anim");
        el.classList.remove("fm-in");
        if (revealed) win.requestAnimationFrame(() => win.requestAnimationFrame(() => el.classList.add("fm-in")));
      } else {
        el.classList.remove("fm-anim");
      }
    }
    render(true);
    inView(el, () => {
      revealed = true;
      win.requestAnimationFrame(() => win.requestAnimationFrame(() => el.classList.add("fm-in")));
    });
    if ("ResizeObserver" in win) {
      let pending = 0;
      observer = new win.ResizeObserver(() => {
        if (pending) return;
        pending = win.requestAnimationFrame(() => {
          pending = 0;
          const w = Math.floor(el.clientWidth);
          if (w && Math.abs(w - lastWidth) > 2) render(false);
        });
      });
      observer.observe(el);
    }
    return {
      el,
      update(next) { spec = normalizeChart(next); render(false); },
      destroy() { if (observer) observer.disconnect(); el.textContent = ""; },
      get spec() { return spec; },
    };
  }

  function showTip(ctx, x, y, title, rows) {
    const { tip, width } = ctx;
    tip.textContent = "";
    tip.append(h("b", { text: title }));
    for (const row of rows) {
      tip.append(h("div", {}, [h("span", {}, [h("i", { style: { background: row.color } }), row.name]), h("strong", { text: row.value })]));
    }
    tip.classList.add("fm-on");
    const tipW = tip.offsetWidth || 160;
    tip.style.left = (x + 14 + tipW > width ? Math.max(0, x - tipW - 14) : x + 14) + "px";
    tip.style.top = Math.max(0, y) + "px";
  }
  const hideTip = (ctx) => ctx.tip.classList.remove("fm-on");

  function yAxis(spec, ctx, values, margin) {
    const finite = values.filter(Number.isFinite);
    const dataMin = finite.length ? Math.min(...finite) : 0;
    const dataMax = finite.length ? Math.max(...finite) : 1;
    const scale = niceTicks(spec.zero === false ? dataMin : Math.min(0, dataMin), Math.max(dataMax, spec.target ?? -Infinity), 4);
    const labels = scale.ticks.map(t => formatValue(t, spec.axisFormat || compactOf(spec.format)));
    margin.l = Math.max(margin.l, Math.max(...labels.map(t => t.length)) * 6.6 + 14);
    return { scale, labels };
  }
  function compactOf(format) {
    const kind = parseFormat(format).kind;
    if (kind === "usd") return "usdc";
    if (kind === "int" || kind === "num" || kind === "compact") return "compact";
    return format;
  }

  function drawXY(spec, ctx) {
    const { svg, width, height } = ctx;
    const isSpark = spec.type === "spark";
    const n = spec.labels.length || Math.max(0, ...spec.series.map(x => x.values.length));
    const values = spec.series.flatMap(x => x.values);
    const directLabels = !isSpark && spec.series.length <= 4 && spec.directLabels !== false;
    const m = isSpark ? { t: 4, r: 6, b: 4, l: 2 } : { t: 12, r: directLabels ? 70 : 16, b: 26, l: 0 };
    const axis = isSpark ? { scale: niceTicks(Math.min(...values.filter(Number.isFinite)), Math.max(...values.filter(Number.isFinite)), 2) } : yAxis(spec, ctx, values, m);
    if (isSpark) axis.scale = { min: axis.scale.min, max: axis.scale.max };
    const pw = width - m.l - m.r, ph = height - m.t - m.b;
    const x = (i) => m.l + (n <= 1 ? pw / 2 : (i * pw) / (n - 1));
    const y = (v) => m.t + ph - ((v - axis.scale.min) / (axis.scale.max - axis.scale.min || 1)) * ph;
    if (!isSpark) {
      const grid = s("g", { class: "fm-grid" }, svg);
      const axisG = s("g", { class: "fm-axis" }, svg);
      axis.scale.ticks.forEach((t, i) => {
        s("line", { x1: m.l, x2: width - m.r, y1: y(t), y2: y(t) }, grid);
        s("text", { x: m.l - 8, y: y(t) + 4, "text-anchor": "end", text: axis.labels[i] }, axisG);
      });
      const maxLabels = Math.max(2, Math.floor(pw / 74));
      const step = Math.max(1, Math.ceil(n / maxLabels));
      for (let i = 0; i < n; i++) {
        if (i % step !== 0 && i !== n - 1) continue;
        if (i !== n - 1 && n - 1 - i < step * 0.6) continue;
        s("text", { x: x(i), y: height - 6, "text-anchor": i === 0 ? "start" : i === n - 1 ? "end" : "middle", text: formatLabel(spec.labels[i]) }, axisG);
      }
      if (axis.scale.min < 0) s("line", { class: "fm-baseline", x1: m.l, x2: width - m.r, y1: y(0), y2: y(0) }, svg);
    }
    const base = y(Math.max(0, axis.scale.min));
    const ends = [];
    spec.series.forEach((ser) => {
      let d = "", area = "", open = false, first = null, last = null;
      ser.values.forEach((v, i) => {
        if (v == null) { open = false; return; }
        d += (open ? "L" : "M") + x(i).toFixed(1) + "," + y(v).toFixed(1);
        if (first == null) first = i;
        last = i;
        open = true;
      });
      if (!d) return;
      if (spec.type === "area" && first != null) {
        area = d + `L${x(last).toFixed(1)},${base.toFixed(1)}L${x(first).toFixed(1)},${base.toFixed(1)}Z`;
        s("path", { class: "fm-area", d: area, style: `fill:${ser.color}` }, svg);
      }
      const path = s("path", { class: "fm-line", d, style: `stroke:${ser.color}` }, svg);
      try { path.style.setProperty("--fm-len", Math.ceil(path.getTotalLength() + 2)); } catch { path.style.setProperty("--fm-len", 4000); }
      if (last != null) {
        s("circle", { class: "fm-dot-end", cx: x(last), cy: y(ser.values[last]), r: isSpark ? 3 : 4, style: `fill:${ser.color}` }, svg);
        if (directLabels) ends.push({ y: y(ser.values[last]), x: x(last), text: formatValue(ser.values[last], spec.format) });
      }
    });
    ends.sort((a, b) => a.y - b.y);
    for (let i = 1; i < ends.length; i++) if (ends[i].y - ends[i - 1].y < 15) ends[i].y = ends[i - 1].y + 15;
    ends.forEach(e => s("text", { class: "fm-direct", x: e.x + 9, y: e.y + 4, text: e.text }, svg));
    if (!isSpark && Number.isFinite(spec.target)) {
      s("line", { class: "fm-target", x1: m.l, x2: width - m.r, y1: y(spec.target), y2: y(spec.target) }, svg);
      s("text", { class: "fm-target-label", x: m.l + 6, y: y(spec.target) - 6, text: (spec.targetLabel || "Goal") + " " + formatValue(spec.target, spec.format) }, svg);
    }
    if (isSpark || !n) return;
    // Hover: crosshair + tooltip; arrow keys walk the points.
    const cross = s("line", { class: "fm-cross", y1: m.t, y2: m.t + ph, visibility: "hidden" }, svg);
    const marks = spec.series.map(ser => s("circle", { r: 4.5, class: "fm-dot-end", style: `fill:${ser.color}`, visibility: "hidden" }, svg));
    const hit = s("rect", { class: "fm-hit", x: m.l, y: m.t, width: pw, height: ph }, svg);
    let index = -1;
    const show = (i) => {
      index = clamp(i, 0, n - 1);
      const cx = x(index);
      cross.setAttribute("x1", cx); cross.setAttribute("x2", cx); cross.setAttribute("visibility", "visible");
      spec.series.forEach((ser, k) => {
        const v = ser.values[index];
        if (v == null) { marks[k].setAttribute("visibility", "hidden"); return; }
        marks[k].setAttribute("cx", cx); marks[k].setAttribute("cy", y(v)); marks[k].setAttribute("visibility", "visible");
      });
      showTip(ctx, cx, m.t, formatLabel(spec.labels[index], "long"),
        spec.series.map(ser => ({ name: ser.name, color: ser.color, value: formatValue(ser.values[index], spec.format) })));
    };
    const hide = () => { index = -1; cross.setAttribute("visibility", "hidden"); marks.forEach(c => c.setAttribute("visibility", "hidden")); hideTip(ctx); };
    const toIndex = (evt) => {
      const r = svg.getBoundingClientRect();
      const px = ((evt.clientX - r.left) / r.width) * width;
      return Math.round(((px - m.l) / (pw || 1)) * (n - 1));
    };
    hit.addEventListener("pointermove", (e) => show(toIndex(e)));
    hit.addEventListener("pointerdown", (e) => show(toIndex(e)));
    hit.addEventListener("pointerleave", hide);
    wireKeys(ctx, n, () => index, show, hide);
  }
  function wireKeys(ctx, n, current, show, hide) {
    ctx.plot.tabIndex = 0;
    ctx.plot.setAttribute("aria-label", (ctx.svg.getAttribute("aria-label") || "Chart") + ". Use arrow keys to read values.");
    ctx.plot.addEventListener("keydown", (e) => {
      if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
        e.preventDefault();
        const i = current();
        show(i < 0 ? (e.key === "ArrowRight" ? 0 : n - 1) : i + (e.key === "ArrowRight" ? 1 : -1));
      } else if (e.key === "Escape") hide();
    });
    ctx.plot.addEventListener("blur", hide);
  }
  function roundedTop(x, y, w, h0, r) {
    if (h0 <= 0) return "";
    r = Math.min(r, w / 2, h0);
    return `M${x},${y + h0}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h0}Z`;
  }
  function drawBars(spec, ctx) {
    const { svg, width, height } = ctx;
    const n = spec.labels.length, k = spec.series.length || 1;
    const values = spec.series.flatMap(x => x.values);
    const m = { t: 18, r: 12, b: 26, l: 0 };
    const axis = yAxis(spec, ctx, values, m);
    const pw = width - m.l - m.r, ph = height - m.t - m.b;
    const y = (v) => m.t + ph - ((v - axis.scale.min) / (axis.scale.max - axis.scale.min || 1)) * ph;
    const grid = s("g", { class: "fm-grid" }, svg), axisG = s("g", { class: "fm-axis" }, svg);
    axis.scale.ticks.forEach((t, i) => {
      s("line", { x1: m.l, x2: width - m.r, y1: y(t), y2: y(t) }, grid);
      s("text", { x: m.l - 8, y: y(t) + 4, "text-anchor": "end", text: axis.labels[i] }, axisG);
    });
    const band = pw / Math.max(1, n);
    const groupW = band * 0.72, gap = 2;
    const barW = Math.max(2, (groupW - gap * (k - 1)) / k);
    const maxLabels = Math.max(2, Math.floor(pw / 64));
    const step = Math.max(1, Math.ceil(n / maxLabels));
    const bars = [];
    let peak = { v: -Infinity, i: -1 };
    for (let i = 0; i < n; i++) {
      const gx = m.l + i * band + (band - groupW) / 2;
      spec.series.forEach((ser, j) => {
        const v = ser.values[i];
        if (v == null) return;
        const top = y(Math.max(v, 0)), bottom = y(Math.min(v, 0));
        const bar = s("path", { class: "fm-bar", d: roundedTop(gx + j * (barW + gap), top, barW, Math.max(1, bottom - top), 4), style: `fill:${ser.color};--fm-delay:${Math.min(600, i * 28)}ms` }, svg);
        bars.push({ bar, i });
        if (k === 1 && v > peak.v) peak = { v, i, x: gx + barW / 2, y: top };
      });
      if (i % step === 0 || i === n - 1) {
        if (!(i !== n - 1 && n - 1 - i < step * 0.6)) s("text", { x: m.l + i * band + band / 2, y: height - 6, "text-anchor": "middle", text: formatLabel(spec.labels[i]) }, axisG);
      }
    }
    if (k === 1 && peak.i >= 0 && spec.directLabels !== false) {
      s("text", { class: "fm-direct", x: peak.x, y: peak.y - 6, "text-anchor": "middle", text: formatValue(peak.v, spec.format) }, svg);
    }
    if (Number.isFinite(spec.target)) {
      s("line", { class: "fm-target", x1: m.l, x2: width - m.r, y1: y(spec.target), y2: y(spec.target) }, svg);
      s("text", { class: "fm-target-label", x: m.l + 6, y: y(spec.target) - 6, text: (spec.targetLabel || "Goal") + " " + formatValue(spec.target, spec.format) }, svg);
    }
    let index = -1;
    const show = (i) => {
      index = clamp(i, 0, n - 1);
      bars.forEach(b => (b.bar.style.opacity = b.i === index ? "1" : ".45"));
      const cx = m.l + index * band + band / 2;
      showTip(ctx, cx, m.t, formatLabel(spec.labels[index], "long"),
        spec.series.map(ser => ({ name: ser.name, color: ser.color, value: formatValue(ser.values[index], spec.format) })));
    };
    const hide = () => { index = -1; bars.forEach(b => (b.bar.style.opacity = "")); hideTip(ctx); };
    for (let i = 0; i < n; i++) {
      const r = s("rect", { class: "fm-hit", x: m.l + i * band, y: m.t, width: band, height: ph }, svg);
      r.addEventListener("pointerenter", () => show(i));
      r.addEventListener("pointerdown", () => show(i));
    }
    svg.addEventListener("pointerleave", hide);
    wireKeys(ctx, n, () => index, show, hide);
  }
  function drawHBars(spec, ctx) {
    const { svg, width } = ctx;
    let items = spec.items.map((it, i) => Object.assign({ order: i }, it));
    if (spec.sort !== false) items.sort((a, b) => b.value - a.value);
    const limit = spec.limit || 10;
    const hidden = items.length > limit ? items.slice(limit) : [];
    items = items.slice(0, limit);
    const rowH = 32, top = 4;
    const height = top + items.length * rowH + (hidden.length ? 22 : 4);
    svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
    svg.setAttribute("height", height);
    const total = spec.items.reduce((a, it) => a + it.value, 0);
    const labelW = Math.min(width * 0.38, Math.max(...items.map(it => formatLabel(it.label).length)) * 7.2 + 12);
    const valueW = 78;
    const max = Math.max(...items.map(it => it.value), 0) || 1;
    const barSpan = width - labelW - valueW;
    const maxChars = Math.max(6, Math.floor((labelW - 12) / 7.2));
    items.forEach((it, i) => {
      const y0 = top + i * rowH;
      const label = formatLabel(it.label);
      const text = s("text", { class: "fm-bar-label", x: 0, y: y0 + 20, text: label.length > maxChars ? label.slice(0, maxChars - 1) + "…" : label }, svg);
      s("title", { text: label }, text);
      const w = Math.max(2, (Math.max(0, it.value) / max) * barSpan);
      const color = spec.colorBy === "item" ? (it.order < MAX_SERIES ? seriesColor(it.order) : OTHER_COLOR) : (it.color || seriesColor(0));
      const r = Math.min(4, w / 2);
      s("path", { class: "fm-hbar", d: `M${labelW},${y0 + 7}H${labelW + w - r}Q${labelW + w},${y0 + 7} ${labelW + w},${y0 + 7 + r}V${y0 + 25 - r}Q${labelW + w},${y0 + 25} ${labelW + w - r},${y0 + 25}H${labelW}Z`, style: `fill:${color};--fm-delay:${i * 60}ms` }, svg);
      s("text", { class: "fm-bar-value", x: labelW + w + 8, y: y0 + 20, text: formatValue(it.value, spec.format) }, svg);
      const hit = s("rect", { class: "fm-hit", x: 0, y: y0, width, height: rowH }, svg);
      const show = () => showTip(ctx, labelW + w, y0, label, [
        { name: spec.valueLabel || "Value", color, value: formatValue(it.value, spec.format) },
        { name: "Share", color: "transparent", value: formatValue(total ? it.value / total : null, "pct1") }]);
      hit.addEventListener("pointerenter", show);
      hit.addEventListener("pointerdown", show);
    });
    svg.addEventListener("pointerleave", () => hideTip(ctx));
    if (hidden.length) {
      const rest = hidden.reduce((a, it) => a + it.value, 0);
      s("text", { class: "fm-bar-label", x: 0, y: height - 6, text: `+ ${hidden.length} more · ${formatValue(rest, spec.format)}` }, svg);
    }
  }
  function drawDonut(spec, ctx) {
    const { svg, width, height, plot } = ctx;
    let items = spec.items.slice().sort((a, b) => b.value - a.value);
    if (items.length > MAX_SERIES) {
      const rest = items.slice(MAX_SERIES - 1);
      items = items.slice(0, MAX_SERIES - 1).concat([{ label: "Other", value: rest.reduce((a, it) => a + it.value, 0), other: true }]);
    }
    const total = items.reduce((a, it) => a + Math.max(0, it.value), 0);
    const size = Math.min(height, width);
    const r = size / 2 - 14, stroke = Math.max(14, r * 0.3);
    const cx = size / 2, cy = height / 2;
    svg.setAttribute("viewBox", `0 0 ${size} ${height}`);
    svg.setAttribute("width", size);
    svg.style.width = size + "px";
    plot.classList.add("fm-donut");
    plot.style.display = "flex";
    plot.style.flexWrap = "wrap";
    plot.style.alignItems = "center";
    plot.style.gap = "18px 28px";
    const circ = 2 * Math.PI * r;
    let start = 0;
    const g = s("g", { transform: `rotate(-90 ${cx} ${cy})` }, svg);
    items.forEach((it, i) => {
      const len = total ? (Math.max(0, it.value) / total) * circ : 0;
      const color = it.other ? OTHER_COLOR : seriesColor(i);
      const seg = s("circle", { class: "fm-seg", cx, cy, r, fill: "none",
        style: `stroke:${color};stroke-width:${stroke}px;--fm-dash:${Math.max(0, len - 2).toFixed(2)};--fm-circ:${circ.toFixed(2)};--fm-delay:${i * 120}ms;stroke-dashoffset:${(-start).toFixed(2)}` }, g);
      seg.style.stroke = color;
      const show = () => showTip(ctx, cx + r * 0.6, 4, formatLabel(it.label), [
        { name: spec.valueLabel || "Value", color, value: formatValue(it.value, spec.format) },
        { name: "Share", color: "transparent", value: formatValue(total ? it.value / total : null, "pct1") }]);
      seg.addEventListener("pointerenter", show);
      seg.addEventListener("pointerdown", show);
      start += len;
    });
    svg.addEventListener("pointerleave", () => hideTip(ctx));
    s("text", { class: "fm-donut-total", x: cx, y: cy + 6, "text-anchor": "middle", text: formatValue(total, compactOf(spec.format)) }, svg);
    s("text", { class: "fm-donut-caption", x: cx, y: cy + 26, "text-anchor": "middle", text: spec.centerLabel || "Total" }, svg);
    const legend = h("div", { class: "fm-legend", style: { flexDirection: "column", gap: "8px", minWidth: "180px", flex: "1 1 180px" } },
      items.map((it, i) => h("span", { style: { justifyContent: "space-between", width: "100%" } }, [
        h("span", {}, [h("i", { style: { background: it.other ? OTHER_COLOR : seriesColor(i) } }), formatLabel(it.label)]),
        h("strong", { text: `${formatValue(it.value, spec.format)} · ${formatValue(total ? it.value / total : null, "pct0")}` }),
      ])));
    plot.append(legend);
  }

  // ── Motion: the explainer player ────────────────────────────────────────────
  const SCENE_DEFAULTS = { title: 3800, kpis: 5200, bars: 6200, line: 6400, donut: 5600, bullets: 5600, compare: 5000, closing: 4200 };
  const ASPECTS = { "16:9": [1920, 1080], "1:1": [1080, 1080], "9:16": [1080, 1920], "4:5": [1080, 1350] };
  function withAlpha(color, alpha) {
    const m = /^#([0-9a-f]{6})$/i.exec(String(color || "").trim());
    if (!m) return color;
    const n = parseInt(m[1], 16);
    return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${alpha})`;
  }
  function explainerTheme(el, spec) {
    const dark = spec.theme !== "light";
    const base = dark
      ? { bg1: "#1a0b1e", bg2: "#2e1d3c", glow: "rgba(157,167,239,0.18)", fg: "#ffffff", muted: "#beb6cd", accent: "#9da7ef", grid: "rgba(255,255,255,0.10)", panel: "rgba(255,255,255,0.05)", good: "#50d48a", bad: "#ff7b7b", series: DARK_SERIES }
      : { bg1: "#fbfaff", bg2: "#ece9f7", glow: "rgba(74,58,167,0.10)", fg: "#1a0b1e", muted: "#586f7c", accent: "#4a3aa7", grid: "rgba(26,11,30,0.10)", panel: "rgba(26,11,30,0.04)", good: "#1e7a3f", bad: "#b3261e", series: LIGHT_SERIES };
    const font = (win.getComputedStyle ? win.getComputedStyle(el).fontFamily : "") || "system-ui, sans-serif";
    return Object.assign(base, typeof spec.theme === "object" ? spec.theme : {}, { font });
  }
  function normalizeExplainer(raw) {
    const spec = Object.assign({ aspect: "16:9", fps: 30 }, raw || {});
    const [w, hgt] = ASPECTS[spec.aspect] || ASPECTS["16:9"];
    spec.width = w; spec.height = hgt;
    spec.scenes = (spec.scenes || []).filter(Boolean).map(sc => Object.assign({}, sc, { duration: Number(sc.duration) || SCENE_DEFAULTS[sc.type] || 5000 }));
    let at = 0;
    spec.scenes.forEach(sc => { sc.start = at; at += sc.duration; });
    spec.duration = at;
    return spec;
  }
  function sceneText(sc, format) {
    const out = [sc.kicker, sc.title, sc.subtitle];
    for (const it of sc.items || []) out.push(typeof it === "string" ? it : `${formatLabel(it.label)}: ${formatValue(it.value, it.format || sc.format || format)}${it.delta != null ? ` (${it.delta >= 0 ? "+" : ""}${formatValue(it.delta, "pct1")})` : ""}`);
    if (sc.a) out.push(`${sc.a.label}: ${formatValue(sc.a.value, sc.format || format)}`);
    if (sc.b) out.push(`${sc.b.label}: ${formatValue(sc.b.value, sc.format || format)}`);
    if (sc.series) for (const ser of sc.series) { const vals = ser.values.filter(Number.isFinite); if (vals.length) out.push(`${ser.name}: ${formatValue(vals[vals.length - 1], sc.format || format)} at the end`); }
    if (sc.annotate && sc.annotate.text) out.push(sc.annotate.text);
    return out.filter(Boolean).join(". ");
  }

  function explainer(el, rawSpec) {
    const spec = normalizeExplainer(rawSpec);
    const W = spec.width, H = spec.height;
    const U = Math.min(W, H) / 1080; // one design unit
    const P = 110 * U * (W > H ? 1.15 : 1);
    el.classList.add("fm-ex");
    el.textContent = "";
    const canvas = h("canvas", { width: W, height: H, role: "img", tabindex: "0", "aria-label": (spec.title || "Animated explainer") + ". Space plays or pauses; arrow keys move between scenes." });
    const play = h("button", { class: "fm-ex-btn fm-primary", type: "button", "aria-label": "Play" , text: "▶" });
    const scrub = h("input", { class: "fm-ex-scrub", type: "range", min: 0, max: spec.duration, step: 10, value: 0, "aria-label": "Position in the explainer" });
    const clock = h("span", { class: "fm-ex-time", text: "" });
    const download = h("button", { class: "fm-ex-btn", type: "button", text: "Download video" });
    const status = h("span", { class: "fm-ex-status", role: "status", "aria-live": "polite" });
    const chips = h("div", { class: "fm-ex-chips" }, spec.scenes.map((sc, i) =>
      h("button", { class: "fm-ex-chip", type: "button", "data-scene": i, text: sc.chip || sc.title || sc.kicker || `Scene ${i + 1}` })));
    const transcript = h("ol", { class: "fm-sr" }, spec.scenes.map(sc => h("li", { text: sceneText(sc, spec.format) })));
    el.append(canvas, h("div", { class: "fm-ex-bar" }, [play, scrub, clock, download, status]), chips, transcript);
    const ctx = canvas.getContext("2d");
    const theme = explainerTheme(el, spec);
    const info = freshness();
    const footnote = spec.source || null;
    const asOf = spec.asOf || (info.through && !/\bthrough\b/i.test(footnote || "") ? "Data through " + info.through : null);

    let t = 0, playing = false, raf = 0, lastNow = 0, recording = false, onEnd = null, poster = true;
    const fmtTime = (ms) => { const sec = Math.round(ms / 1000); return Math.floor(sec / 60) + ":" + String(sec % 60).padStart(2, "0"); };

    const font = (weight, size) => `${weight} ${Math.round(size * U)}px ${theme.font}`;
    // 1:1, 4:5 and 9:16 lay out in columns and rows, not one wide row.
    const narrow = W < H * 1.3;
    // The largest size (design units) at which one line fits maxW, never below 40%.
    function fit(str, weight, size, maxW) {
      let sz = size;
      ctx.font = font(weight, sz);
      while (sz > size * 0.4 && ctx.measureText(String(str)).width > maxW) { sz -= 2; ctx.font = font(weight, sz); }
      return sz;
    }
    const prog = (lt, delay, dur) => ease(clamp((lt - delay) / dur, 0, 1));
    function wrap(text, maxW, maxLines) {
      const words = String(text || "").split(/\s+/).filter(Boolean);
      const lines = [];
      let line = "";
      for (const word of words) {
        const test = line ? line + " " + word : word;
        if (ctx.measureText(test).width > maxW && line) { lines.push(line); line = word; } else line = test;
      }
      if (line) lines.push(line);
      if (maxLines && lines.length > maxLines) { lines.length = maxLines; lines[maxLines - 1] = lines[maxLines - 1].replace(/\s*\S*$/, "") + "…"; }
      return lines;
    }
    function text(str, x, y, opts = {}) {
      ctx.font = font(opts.weight || 600, opts.size || 32);
      ctx.fillStyle = opts.color || theme.fg;
      ctx.textAlign = opts.align || "left";
      ctx.textBaseline = opts.baseline || "alphabetic";
      ctx.fillText(str, x, y);
    }
    function roundRect(x, y, w, hgt, r) {
      r = Math.min(r, w / 2, hgt / 2);
      ctx.beginPath();
      ctx.moveTo(x + r, y); ctx.arcTo(x + w, y, x + w, y + hgt, r); ctx.arcTo(x + w, y + hgt, x, y + hgt, r);
      ctx.arcTo(x, y + hgt, x, y, r); ctx.arcTo(x, y, x + w, y, r); ctx.closePath();
    }
    function background() {
      const g = ctx.createLinearGradient(0, 0, W, H);
      g.addColorStop(0, theme.bg1); g.addColorStop(1, theme.bg2);
      ctx.fillStyle = g; ctx.fillRect(0, 0, W, H);
      const r = ctx.createRadialGradient(W * 0.82, H * 0.1, 0, W * 0.82, H * 0.1, Math.max(W, H) * 0.7);
      r.addColorStop(0, theme.glow); r.addColorStop(1, "rgba(0,0,0,0)");
      ctx.fillStyle = r; ctx.fillRect(0, 0, W, H);
    }
    function heading(sc, lt) {
      const a = prog(lt, 0, 650);
      ctx.save(); ctx.globalAlpha *= a; ctx.translate(0, (1 - a) * 24 * U);
      let y = P + 30 * U;
      if (sc.kicker) { text(String(sc.kicker).toUpperCase(), P, y, { size: 26, weight: 700, color: theme.accent }); y += 74 * U; }
      ctx.font = font(800, 64);
      for (const line of wrap(sc.title, W - 2 * P, 2)) { text(line, P, y, { size: 64, weight: 800 }); y += 74 * U; }
      if (sc.subtitle) { ctx.font = font(500, 32); for (const line of wrap(sc.subtitle, W - 2 * P, 2)) { text(line, P, y, { size: 32, weight: 500, color: theme.muted }); y += 42 * U; } }
      ctx.restore();
      return y + 30 * U;
    }
    const contentBottom = () => H - P - 30 * U;
    const fmtOf = (sc, it) => (it && it.format) || sc.format || spec.format || "num";

    const scenes = {
      title(sc, lt) {
        let y = H * 0.42;
        const a1 = prog(lt, 0, 600), a2 = prog(lt, 220, 800), a3 = prog(lt, 600, 800);
        ctx.save(); ctx.globalAlpha *= a1;
        if (sc.kicker) text(String(sc.kicker).toUpperCase(), P, y - 110 * U, { size: 30, weight: 700, color: theme.accent });
        ctx.fillStyle = theme.accent; ctx.fillRect(P, y - 80 * U, 120 * U * a2, 6 * U);
        ctx.restore();
        ctx.save(); ctx.globalAlpha *= a2; ctx.translate(0, (1 - a2) * 40 * U);
        ctx.font = font(800, 104);
        for (const line of wrap(sc.title, W - 2 * P, 2)) { text(line, P, y + 40 * U, { size: 104, weight: 800 }); y += 116 * U; }
        ctx.restore();
        if (sc.subtitle) {
          ctx.save(); ctx.globalAlpha *= a3; ctx.translate(0, (1 - a3) * 24 * U);
          ctx.font = font(500, 40);
          for (const line of wrap(sc.subtitle, W - 2 * P, 2)) { text(line, P, y + 50 * U, { size: 40, weight: 500, color: theme.muted }); y += 52 * U; }
          ctx.restore();
        }
      },
      closing(sc, lt) { scenes.title(sc, lt); },
      kpis(sc, lt) {
        const top = heading(sc, lt);
        const items = (sc.items || []).slice(0, 4);
        const gap = 36 * U, n = Math.max(1, items.length);
        const cols = narrow ? Math.min(2, n) : n, rows = Math.ceil(n / cols);
        const tileW = (W - 2 * P - gap * (cols - 1)) / cols;
        const tileH = clamp((contentBottom() - top - 40 * U - gap * (rows - 1)) / rows, 220 * U, 420 * U);
        items.forEach((it, i) => {
          const d = 250 + i * 180, a = prog(lt, d, 600);
          const x = P + (i % cols) * (tileW + gap), y = top + Math.floor(i / cols) * (tileH + gap) + (1 - a) * 30 * U;
          ctx.save(); ctx.globalAlpha *= a;
          ctx.fillStyle = theme.panel; roundRect(x, y, tileW, tileH, 26 * U); ctx.fill();
          ctx.fillStyle = theme.series[i % theme.series.length]; ctx.fillRect(x + 28 * U, y + 28 * U, 56 * U, 6 * U);
          const inner = tileW - 56 * U;
          text(it.label, x + 28 * U, y + 84 * U, { size: fit(it.label, 600, 28, inner), weight: 600, color: theme.muted });
          const p = prog(lt, d + 200, 1500);
          const big = fit(formatValue(it.value, fmtOf(sc, it)), 800, cols >= 4 ? 76 : 96, inner);
          text(formatValue((it.from || 0) + (it.value - (it.from || 0)) * p, fmtOf(sc, it)), x + 28 * U, y + tileH * 0.55 + big * 0.35 * U, { size: big, weight: 800 });
          if (it.delta != null) {
            const ad = prog(lt, d + 1600, 500);
            ctx.globalAlpha *= ad;
            const up = it.delta >= 0;
            const good = it.higherIsBetter === false ? !up : up;
            const deltaText = `${up ? "▲" : "▼"} ${formatValue(Math.abs(it.delta), "pct1")} ${it.deltaLabel || "vs prior"}`;
            text(deltaText, x + 28 * U, y + tileH - 40 * U, { size: fit(deltaText, 700, 30, inner), weight: 700, color: good ? theme.good : theme.bad });
          }
          ctx.restore();
        });
      },
      bars(sc, lt) {
        const top = heading(sc, lt);
        let items = (sc.items || []).slice();
        if (sc.sort !== false) items.sort((a, b) => b.value - a.value);
        items = items.slice(0, sc.limit || 8);
        const max = Math.max(...items.map(it => it.value), 0) || 1;
        const rowH = Math.min(92 * U, (contentBottom() - top) / Math.max(1, items.length));
        ctx.font = font(600, 30);
        const labelW = Math.min((W - 2 * P) * 0.32, Math.max(...items.map(it => ctx.measureText(formatLabel(it.label)).width)) + 30 * U);
        const valueW = 210 * U;
        const span = W - 2 * P - labelW - valueW;
        items.forEach((it, i) => {
          const d = 300 + i * 140, a = prog(lt, d, 900);
          const y = top + i * rowH;
          ctx.save(); ctx.globalAlpha *= prog(lt, d - 150, 400);
          ctx.font = font(600, 30);
          const label = formatLabel(it.label);
          const lines = wrap(label, labelW - 24 * U, 1);
          text(lines[0] || "", P, y + rowH * 0.58, { size: 30, weight: 600, color: i === 0 ? theme.fg : theme.muted });
          const w = Math.max(4 * U, (Math.max(0, it.value) / max) * span * a);
          ctx.save();
          ctx.fillStyle = i === 0 ? theme.accent : theme.series[0];
          if (i !== 0) ctx.globalAlpha *= 0.55;
          roundRect(P + labelW, y + rowH * 0.2, w, rowH * 0.56, 8 * U); ctx.fill();
          ctx.restore();
          text(formatValue(it.value * a, fmtOf(sc, it)), P + labelW + w + 18 * U, y + rowH * 0.6, { size: 30, weight: 800 });
          ctx.restore();
        });
      },
      line(sc, lt) {
        const top = heading(sc, lt) + 10 * U;
        const series = (sc.series || []).slice(0, 3);
        const labels = sc.labels || [];
        const n = labels.length || Math.max(0, ...series.map(x => x.values.length));
        const vals = series.flatMap(x => x.values).filter(Number.isFinite);
        const scale = niceTicks(Math.min(0, ...vals), Math.max(...vals, 0), 4);
        const left = P + 120 * U, right = W - P - 200 * U, bottom = contentBottom() - 40 * U;
        const x = (i) => left + (n <= 1 ? 0 : (i * (right - left)) / (n - 1));
        const y = (v) => bottom - ((v - scale.min) / (scale.max - scale.min || 1)) * (bottom - top);
        const ga = prog(lt, 100, 500);
        ctx.save(); ctx.globalAlpha *= ga;
        ctx.strokeStyle = theme.grid; ctx.lineWidth = 2 * U;
        for (const tk of scale.ticks) {
          ctx.beginPath(); ctx.moveTo(left, y(tk)); ctx.lineTo(right, y(tk)); ctx.stroke();
          text(formatValue(tk, compactOf(fmtOf(sc))), left - 18 * U, y(tk) + 9 * U, { size: 24, weight: 500, color: theme.muted, align: "right" });
        }
        const marks = n > 1 ? [0, Math.floor((n - 1) / 2), n - 1] : [0];
        for (const i of marks) text(formatLabel(labels[i]), x(i), bottom + 44 * U, { size: 24, weight: 500, color: theme.muted, align: i === 0 ? "left" : i === n - 1 ? "right" : "center" });
        ctx.restore();
        const p = prog(lt, 500, 2600);
        series.forEach((ser, k) => {
          const color = ser.color || (k === 0 ? theme.accent : theme.series[k % theme.series.length]);
          const pts = ser.values.map((v, i) => (Number.isFinite(v) ? [x(i), y(v)] : null)).filter(Boolean);
          if (!pts.length) return;
          const cut = left + (right - left) * p;
          ctx.save();
          ctx.beginPath(); ctx.rect(0, 0, cut + 2, H); ctx.clip();
          if (k === 0) {
            const g = ctx.createLinearGradient(0, top, 0, bottom);
            g.addColorStop(0, withAlpha(color, 0.33)); g.addColorStop(1, withAlpha(color, 0));
            ctx.beginPath(); ctx.moveTo(pts[0][0], bottom);
            pts.forEach(([px, py]) => ctx.lineTo(px, py));
            ctx.lineTo(pts[pts.length - 1][0], bottom); ctx.closePath(); ctx.fillStyle = g; ctx.fill();
          }
          ctx.beginPath(); pts.forEach(([px, py], i) => (i ? ctx.lineTo(px, py) : ctx.moveTo(px, py)));
          ctx.strokeStyle = color; ctx.lineWidth = 6 * U; ctx.lineJoin = "round"; ctx.lineCap = "round"; ctx.stroke();
          ctx.restore();
          const visible = pts.filter(([px]) => px <= cut + 1);
          const end = visible[visible.length - 1];
          if (end) {
            ctx.beginPath(); ctx.arc(end[0], end[1], 10 * U, 0, Math.PI * 2); ctx.fillStyle = color; ctx.fill();
            const idx = ser.values.map((v, i) => (Number.isFinite(v) ? i : -1)).filter(i => i >= 0)[visible.length - 1];
            text(formatValue(ser.values[idx], fmtOf(sc)), end[0] + 22 * U, end[1] + 10 * U, { size: 30, weight: 800 });
          }
        });
        if (series.length > 1) {
          series.forEach((ser, k) => {
            const color = ser.color || (k === 0 ? theme.accent : theme.series[k % theme.series.length]);
            const lx = right - 260 * U + 0, ly = top - 10 * U + k * 40 * U;
            ctx.fillStyle = color; ctx.fillRect(lx, ly - 14 * U, 26 * U, 8 * U);
            text(ser.name, lx + 38 * U, ly, { size: 24, weight: 600, color: theme.muted });
          });
        }
        if (sc.annotate && Number.isInteger(sc.annotate.index) && series[0]) {
          const a = prog(lt, 3200, 600);
          const i = sc.annotate.index, v = series[0].values[i];
          if (Number.isFinite(v) && a > 0) {
            ctx.save(); ctx.globalAlpha *= a;
            const ax = x(i), ay = y(v);
            ctx.strokeStyle = theme.fg; ctx.lineWidth = 3 * U;
            ctx.beginPath(); ctx.arc(ax, ay, 16 * U, 0, Math.PI * 2); ctx.stroke();
            ctx.font = font(700, 28);
            const lines = wrap(sc.annotate.text, 520 * U, 3);
            const bw = Math.max(...lines.map(l => ctx.measureText(l).width)) + 40 * U, bh = lines.length * 38 * U + 28 * U;
            const bx = clamp(ax - bw / 2, P, W - P - bw), by = Math.max(top, ay - bh - 40 * U);
            ctx.fillStyle = theme.panel; roundRect(bx, by, bw, bh, 16 * U); ctx.fill();
            lines.forEach((l, li) => text(l, bx + 20 * U, by + 44 * U + li * 38 * U, { size: 28, weight: 700 }));
            ctx.restore();
          }
        }
      },
      donut(sc, lt) {
        const top = heading(sc, lt);
        let items = (sc.items || []).slice().sort((a, b) => b.value - a.value);
        if (items.length > MAX_SERIES) {
          const rest = items.slice(MAX_SERIES - 1);
          items = items.slice(0, MAX_SERIES - 1).concat([{ label: "Other", value: rest.reduce((a, it) => a + it.value, 0), other: true }]);
        }
        const total = items.reduce((a, it) => a + Math.max(0, it.value), 0) || 1;
        const r = narrow ? Math.min((contentBottom() - top) * 0.3, (W - 2 * P) * 0.3) : Math.min((contentBottom() - top) / 2, (W - 2 * P) * 0.22);
        const cx = narrow ? W / 2 : P + r + 20 * U, cy = narrow ? top + r + 20 * U : top + (contentBottom() - top) / 2;
        const sweep = prog(lt, 300, 1800);
        let ang = -Math.PI / 2;
        items.forEach((it, i) => {
          const full = (Math.max(0, it.value) / total) * Math.PI * 2;
          const shown = Math.max(0, Math.min(full, Math.PI * 2 * sweep - (ang + Math.PI / 2)));
          if (shown > 0) {
            ctx.beginPath(); ctx.arc(cx, cy, r, ang, ang + Math.max(0, shown - 0.012));
            ctx.strokeStyle = it.other ? "#7d7690" : theme.series[i % theme.series.length];
            ctx.lineWidth = r * 0.34; ctx.stroke();
          }
          ang += full;
        });
        text(formatValue(total * prog(lt, 300, 1800), compactOf(fmtOf(sc))), cx, cy + 14 * U, { size: 56, weight: 800, align: "center" });
        text(sc.centerLabel || "Total", cx, cy + 58 * U, { size: 24, weight: 600, color: theme.muted, align: "center" });
        const lx = narrow ? P : cx + r + 140 * U;
        const rowH = narrow ? Math.min(60 * U, (contentBottom() - (cy + r + 60 * U)) / Math.max(1, items.length)) : 68 * U;
        items.forEach((it, i) => {
          const a = prog(lt, 900 + i * 160, 500);
          const ly = narrow ? cy + r + 90 * U + i * rowH : cy - (items.length - 1) * 34 * U + i * 68 * U;
          ctx.save(); ctx.globalAlpha *= a;
          ctx.fillStyle = it.other ? "#7d7690" : theme.series[i % theme.series.length];
          roundRect(lx, ly - 24 * U, 30 * U, 30 * U, 8 * U); ctx.fill();
          text(formatLabel(it.label), lx + 50 * U, ly, { size: 30, weight: 600 });
          text(`${formatValue(it.value, fmtOf(sc))}  ·  ${formatValue(it.value / total, "pct0")}`, W - P, ly, { size: 30, weight: 700, color: theme.muted, align: "right" });
          ctx.restore();
        });
      },
      bullets(sc, lt) {
        let y = heading(sc, lt) + 20 * U;
        const items = (sc.items || []).slice(0, 6).map(item => (typeof item === "string" ? item : item.text));
        const maxLines = narrow ? 3 : 2, room = contentBottom() - y - 20 * U;
        // The largest text size at which every takeaway fits above the footer.
        let size = narrow ? 36 : 40, laid = [];
        for (; size >= 24; size -= 2) {
          ctx.font = font(600, size);
          laid = items.map(t => wrap(t, W - 2 * P - 60 * U, maxLines));
          const height = laid.reduce((sum, lines) => sum + lines.length * (size + 10) * U + 30 * U, 0);
          if (height <= room) break;
        }
        laid.forEach((lines, i) => {
          const a = prog(lt, 400 + i * 450, 600);
          ctx.save(); ctx.globalAlpha *= a; ctx.translate((1 - a) * 30 * U, 0);
          ctx.fillStyle = theme.accent; ctx.beginPath(); ctx.arc(P + 12 * U, y - size * 0.3 * U, Math.max(6, size * 0.22) * U, 0, Math.PI * 2); ctx.fill();
          lines.forEach((l, li) => text(l, P + 50 * U, y + li * (size + 10) * U, { size, weight: 600 }));
          ctx.restore();
          y += lines.length * (size + 10) * U + 30 * U;
        });
      },
      compare(sc, lt) {
        const top = heading(sc, lt);
        const half = (W - 2 * P) / 2;
        const colW = narrow ? W - 2 * P : half - 40 * U;
        const finals = [sc.a, sc.b].map(side => (side ? formatValue(side.value || 0, fmtOf(sc, side)) : ""));
        const valueSize = Math.min(...finals.map(f => fit(f, 800, 110, colW)));
        [sc.a, sc.b].forEach((side, i) => {
          if (!side) return;
          const a = prog(lt, 300 + i * 300, 600), p = prog(lt, 400 + i * 300, 1500);
          const x = narrow ? P : P + i * half;
          const y0 = narrow ? top + i * (valueSize + 110) * U : top;
          ctx.save(); ctx.globalAlpha *= a;
          text(side.label, x, y0 + 60 * U, { size: 34, weight: 600, color: theme.muted });
          text(formatValue((side.value || 0) * p, fmtOf(sc, side)), x, y0 + (60 + valueSize * 1.05) * U, { size: valueSize, weight: 800, color: i === 1 ? theme.accent : theme.fg });
          ctx.restore();
        });
        if (sc.a && sc.b && Number.isFinite(sc.a.value) && sc.a.value !== 0) {
          const change = (sc.b.value - sc.a.value) / Math.abs(sc.a.value);
          const a = prog(lt, 2100, 600);
          ctx.save(); ctx.globalAlpha *= a;
          const up = change >= 0, good = sc.higherIsBetter === false ? !up : up;
          const deltaY = narrow ? top + 2 * (valueSize + 110) * U + 40 * U : top + (110 + valueSize * 1.05 + 90) * U;
          text(`${up ? "▲" : "▼"} ${formatValue(Math.abs(change), "pct1")} ${sc.deltaLabel || ""}`.trim(), P, deltaY, { size: 44, weight: 800, color: good ? theme.good : theme.bad });
          ctx.restore();
        }
      },
    };

    function chrome(time) {
      ctx.save();
      ctx.globalAlpha = 0.85;
      const note = [footnote, asOf].filter(Boolean).join("  ·  ");
      ctx.font = font(700, 24);
      const brandW = spec.brand ? ctx.measureText(spec.brand).width : 0;
      ctx.font = font(500, 22);
      const noteW = note ? ctx.measureText(note).width : 0;
      if (spec.brand) text(spec.brand, P, H - P * 0.5, { size: 24, weight: 700, color: theme.muted });
      if (note && brandW + noteW + 40 * U <= W - 2 * P) text(note, W - P, H - P * 0.5, { size: 22, weight: 500, color: theme.muted, align: "right" });
      else if (note) text(note, P, H - P * 0.5 - (spec.brand ? 36 * U : 0), { size: fit(note, 500, 22, W - 2 * P), weight: 500, color: theme.muted });
      ctx.restore();
      ctx.fillStyle = theme.grid; ctx.fillRect(0, H - 8 * U, W, 8 * U);
      ctx.fillStyle = theme.accent; ctx.fillRect(0, H - 8 * U, W * (spec.duration ? time / spec.duration : 0), 8 * U);
    }
    function sceneAt(time) {
      let i = spec.scenes.findIndex(sc => time < sc.start + sc.duration);
      if (i < 0) i = spec.scenes.length - 1;
      return i;
    }
    function renderAt(time) {
      t = clamp(time, 0, spec.duration);
      background();
      const i = sceneAt(t);
      const sc = spec.scenes[i];
      if (sc) {
        const lt = t - sc.start;
        const isLast = i === spec.scenes.length - 1;
        const fadeIn = i === 0 ? 1 : clamp(lt / 350, 0, 1);
        const fadeOut = isLast ? 1 : clamp((sc.duration - lt) / 380, 0, 1);
        ctx.save(); ctx.globalAlpha = Math.min(fadeIn, fadeOut);
        (scenes[sc.type] || scenes.bullets)(sc, lt);
        ctx.restore();
      }
      chrome(t);
      scrub.value = String(Math.round(t));
      clock.textContent = fmtTime(t) + " / " + fmtTime(spec.duration);
      chips.querySelectorAll(".fm-ex-chip").forEach((c, k) => c.setAttribute("aria-current", k === i ? "true" : "false"));
    }
    function loop(now) {
      if (!playing) return;
      const dt = lastNow ? now - lastNow : 0;
      lastNow = now;
      const next = t + dt;
      if (next >= spec.duration) {
        renderAt(spec.duration);
        pause();
        if (onEnd) { const cb = onEnd; onEnd = null; cb(); }
        return;
      }
      renderAt(next);
      raf = win.requestAnimationFrame(loop);
    }
    function startPlayback() {
      if (playing) return;
      if (t >= spec.duration || poster) t = 0;
      poster = false;
      playing = true; lastNow = 0;
      play.textContent = "❚❚"; play.setAttribute("aria-label", "Pause");
      raf = win.requestAnimationFrame(loop);
    }
    function pause() {
      playing = false; win.cancelAnimationFrame(raf);
      play.textContent = "▶"; play.setAttribute("aria-label", t >= spec.duration ? "Replay" : "Play");
    }
    function seek(ms) { poster = false; renderAt(ms); }

    const controls = () => [play, scrub, download].concat(Array.from(chips.querySelectorAll("button")));
    function pickMime() {
      if (typeof win.MediaRecorder === "undefined") return null;
      return ["video/mp4;codecs=avc1.42E01E", "video/mp4", "video/webm;codecs=vp9", "video/webm;codecs=vp8", "video/webm"]
        .find(m => win.MediaRecorder.isTypeSupported(m)) || null;
    }
    async function exportVideo() {
      const mime = pickMime();
      if (!mime || !canvas.captureStream) throw new Error("This browser cannot record video from the page.");
      const stream = canvas.captureStream(spec.fps);
      const recorder = new win.MediaRecorder(stream, { mimeType: mime, videoBitsPerSecond: spec.bitrate || 8_000_000 });
      const chunks = [];
      recorder.ondataavailable = (e) => { if (e.data && e.data.size) chunks.push(e.data); };
      const stopped = new Promise((resolve) => (recorder.onstop = resolve));
      recording = true;
      controls().forEach(c => (c.disabled = true));
      pause();
      renderAt(0);
      recorder.start(250);
      await new Promise((resolve) => { onEnd = resolve; startPlayback(); });
      await new Promise((resolve) => win.setTimeout(resolve, 300));
      recorder.stop();
      await stopped;
      stream.getTracks().forEach(tr => tr.stop());
      recording = false;
      controls().forEach(c => (c.disabled = false));
      const type = mime.split(";")[0];
      const blob = new Blob(chunks, { type });
      const name = (spec.filename || "explainer") + (type === "video/mp4" ? ".mp4" : ".webm");
      const url = URL.createObjectURL(blob);
      const a = h("a", { href: url, download: name });
      doc.body.append(a); a.click(); a.remove();
      win.setTimeout(() => URL.revokeObjectURL(url), 60000);
      return { blob, type, name };
    }

    play.addEventListener("click", () => (playing ? pause() : startPlayback()));
    scrub.addEventListener("input", () => { pause(); poster = false; renderAt(Number(scrub.value)); });
    chips.addEventListener("click", (e) => {
      const chip = e.target.closest(".fm-ex-chip");
      if (!chip) return;
      pause(); poster = false;
      const sc = spec.scenes[Number(chip.getAttribute("data-scene"))];
      renderAt(sc.start + Math.min(sc.duration - 1, reducedMotion() ? sc.duration - 1 : 0));
      if (!reducedMotion()) startPlayback();
    });
    canvas.addEventListener("keydown", (e) => {
      if (recording) return;
      if (e.key === " " || e.key === "k") { e.preventDefault(); playing ? pause() : startPlayback(); }
      else if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
        e.preventDefault(); pause(); poster = false;
        const i = clamp(sceneAt(t) + (e.key === "ArrowRight" ? 1 : -1), 0, spec.scenes.length - 1);
        renderAt(spec.scenes[i].start + (reducedMotion() ? spec.scenes[i].duration - 1 : 0));
      }
    });
    download.addEventListener("click", async () => {
      status.textContent = "Recording " + fmtTime(spec.duration) + " of video… keep this tab open.";
      try { const out = await exportVideo(); status.textContent = "Saved " + out.name + "."; }
      catch (err) { status.textContent = err.message; recording = false; controls().forEach(c => (c.disabled = false)); }
    });
    if (!pickMime()) download.hidden = true;

    const ready = (doc.fonts && doc.fonts.ready) ? doc.fonts.ready : Promise.resolve();
    // Until it plays, the player shows a poster: the first scene settled.
    const posterAt = () => (spec.scenes[0] ? Math.min(spec.scenes[0].duration - 1, 1800) : 0);
    ready.then(() => {
      if (poster) renderAt(posterAt());
      if (spec.autoplay !== false && !reducedMotion()) inView(el, () => { if (poster) startPlayback(); }, 0.5);
    });
    renderAt(posterAt());

    return {
      el, canvas,
      get duration() { return spec.duration; },
      get spec() { return spec; },
      play: startPlayback, pause, seek, renderAt, exportVideo,
      state: () => ({ time: t, duration: spec.duration, scene: sceneAt(t), playing, recording }),
    };
  }

  // ── Scheme: follow what the page looks like ─────────────────────────────────
  // Flag's data-theme says what the THEME is; a custom design can paint its own
  // light page under a dark theme (or the reverse). The kit reads the page's real
  // background and picks its light or dark values from that, and re-reads it when
  // the page or the theme toggle changes it.
  function parseColor(value) {
    const m = /rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)(?:\s*[,/]\s*([\d.]+%?))?/.exec(value || "");
    if (!m) return null;
    const alpha = m[4] == null ? 1 : m[4].endsWith("%") ? parseFloat(m[4]) / 100 : parseFloat(m[4]);
    return { r: +m[1], g: +m[2], b: +m[3], a: alpha };
  }
  function pageScheme() {
    for (const el of [doc.body, doc.documentElement]) {
      if (!el) continue;
      const c = parseColor(win.getComputedStyle(el).backgroundColor);
      if (c && c.a >= 0.5) {
        const lin = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
        const luminance = 0.2126 * lin(c.r) + 0.7152 * lin(c.g) + 0.0722 * lin(c.b);
        return luminance < 0.18 ? "dark" : "light";
      }
    }
    return doc.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
  }
  let schemeWatch = null;
  function applyScheme() {
    const scheme = pageScheme();
    if (doc.documentElement.getAttribute("data-fm-scheme") !== scheme) doc.documentElement.setAttribute("data-fm-scheme", scheme);
    return scheme;
  }
  function watchScheme() {
    if (schemeWatch || !("MutationObserver" in win)) return;
    let pending = 0;
    schemeWatch = new win.MutationObserver(() => {
      if (pending) return;
      pending = win.requestAnimationFrame(() => { pending = 0; applyScheme(); });
    });
    const opts = { attributes: true, attributeFilter: ["data-theme", "class", "style"] };
    schemeWatch.observe(doc.documentElement, opts);
    if (doc.body) schemeWatch.observe(doc.body, opts);
    // Theme toggles also animate the background; read it again once it settles.
    win.addEventListener("transitionend", (e) => { if (e.target === doc.body || e.target === doc.documentElement) applyScheme(); });
  }

  // ── Declarative wiring ──────────────────────────────────────────────────────
  const registry = { charts: [], explainers: [] };
  // One element can carry several features (a headline number that counts up
  // AND explains itself), so each feature remembers what it has wired on its own.
  const wired = {};
  function init(rootEl = doc) {
    if (!doc) return registry;
    applyScheme();
    watchScheme();
    doc.documentElement.classList.add("fm-ready");
    const all = (sel) => {
      const seen = wired[sel] || (wired[sel] = new WeakSet());
      return Array.from(rootEl.querySelectorAll(sel)).filter(el => !seen.has(el) && seen.add(el));
    };
    all("[data-fm-stagger]").forEach((parent) => {
      const step = Number(parent.getAttribute("data-fm-stagger")) || 80;
      Array.from(parent.children).forEach((child, i) => {
        if (!child.hasAttribute("data-fm-reveal")) child.setAttribute("data-fm-reveal", "");
        child.style.setProperty("--fm-delay", i * step + "ms");
      });
    });
    all("[data-fm-reveal]").forEach((el) => { inView(el, () => el.classList.add("fm-in"), 0.12); });
    all("[data-fm-count]").forEach((el) => {
      const target = countTarget(el);
      el.setAttribute("data-fm-target", String(target));
      const format = el.getAttribute("data-fm-format") || "num";
      const onScreen = el.getBoundingClientRect().top < (win.innerHeight || 0);
      el.textContent = formatValue(onScreen && !reducedMotion() ? Number(el.getAttribute("data-fm-from") || 0) : target, format);
      inView(el, () => count(el, target));
    });
    all("[data-fm-freshness]").forEach((el) => { renderFreshness(el); });
    all("[data-fm-metric]").forEach(bindMetric);
    all("[data-fm-chart]").forEach((el) => {
      const spec = resolveSpec(el.getAttribute("data-fm-chart") || el.getAttribute("data-fm-src"));
      if (spec) registry.charts.push(chart(el, spec));
    });
    all("[data-fm-explainer]").forEach((el) => {
      const spec = resolveSpec(el.getAttribute("data-fm-explainer") || el.getAttribute("data-fm-src"));
      if (spec) registry.explainers.push(explainer(el, spec));
    });
    return registry;
  }

  // Print and full-page capture show settled values, not half-run animations.
  function settle() {
    doc.querySelectorAll("[data-fm-reveal]").forEach(el => el.classList.add("fm-in"));
    doc.querySelectorAll(".fm-chart.fm-anim").forEach(el => el.classList.add("fm-in"));
    doc.querySelectorAll("[data-fm-target]").forEach(el => {
      el.textContent = formatValue(Number(el.getAttribute("data-fm-target")), el.getAttribute("data-fm-format") || "num");
    });
  }
  if (doc) {
    win.addEventListener("beforeprint", settle);
    if (doc.readyState === "loading") doc.addEventListener("DOMContentLoaded", () => init(), { once: true });
    else win.setTimeout(() => init(), 0);
  }

  return {
    version: VERSION, init, settle, count, chart, explainer, freshness, envelope, registry, scheme: applyScheme,
    openProvenance, closeProvenance, renderFreshness,
    format: formatValue, formatLabel, niceTicks, relativeAge,
  };
});
