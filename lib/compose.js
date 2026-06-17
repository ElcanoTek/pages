"use strict";
// lib/compose.js — DEV/TEST ONLY: an in-admin "compose a page with Cutlass" panel.
//
// This deliberately crosses the clean separation (PLAN: pages hosts; agents
// author — the real prod create flow lives in moc.elcanotek.com / chat). It
// exists for local testing of the text→agent→page loop and is GATED: only mounted
// when PAGES_COMPOSE=1, and admin-cookie + CSRF authenticated. Never enable in prod.
//
// Two pluggable drivers (COMPOSE_DRIVER), same in-memory job + log shape so the
// browser panel (welcome.js) is identical either way:
//
//   spawn (default, dev) — spawn the local `cutlass` CLI (array args, NO shell →
//     a prompt can't inject a command), cwd=CUTLASS_DIR so it reads its own .env
//     (OpenRouter key + PAGES_MCP_*). It connects back to THIS server's /mcp and
//     deploys+publishes. Needs CUTLASS_BIN + CUTLASS_DIR.
//
//   moc (prod-style) — POST {MOC_URL}/tasks with X-API-Key (a scoped create-task
//     key), then poll {MOC_URL}/tasks/{id} + /logs/{id}. A cutlass fleet NODE picks
//     the task up and deploys to pages' MCP. pages never holds an OpenRouter key.
//     Needs MOC_URL + MOC_API_KEY. (Node→pages MCP registration is a MOC/node
//     concern, configured there — not here.)

const express = require("express");
const { spawn } = require("node:child_process");
const { requireAdminJSON, requireCsrf } = require("./csrf");

const DRIVER = (process.env.COMPOSE_DRIVER || "spawn").toLowerCase();
const CUTLASS_BIN = process.env.CUTLASS_BIN || "";
const CUTLASS_DIR = process.env.CUTLASS_DIR || "";
const MOC_URL = (process.env.MOC_URL || "").replace(/\/+$/, "");
const MOC_API_KEY = process.env.MOC_API_KEY || "";
const MOC_TARGET = process.env.MOC_TARGET_NODE_NAME || "";
const MOC_MODEL = process.env.MOC_MODEL || "";
const MOC_FALLBACK = process.env.MOC_FALLBACK_MODEL || "";
const MAX_MS = Number(process.env.COMPOSE_TIMEOUT_MS || 420000); // hard cap ~7 min
const POLL_MS = Number(process.env.COMPOSE_POLL_MS || 3000);
const SLUG_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const LOG_CAP = 400; // keep the last N lines per job
// eslint-disable-next-line no-control-regex
const ANSI = /\x1b\[[0-9;]*m/g;
// eslint-disable-next-line no-control-regex
const CTRL = /[\x00-\x08\x0b-\x1f\x7f]/g; // C0/C1 ctrl chars; keep \n (split) + \t

function enabled() {
  if (process.env.PAGES_COMPOSE !== "1") return false;
  if (DRIVER === "moc") return !!MOC_URL && !!MOC_API_KEY;
  return !!CUTLASS_BIN && !!CUTLASS_DIR; // spawn
}
function driverLabel() {
  return DRIVER === "moc" ? `moc (${MOC_URL})` : `spawn (${CUTLASS_BIN})`;
}

function clean(s) {
  return String(s).replace(ANSI, "").replace(CTRL, "");
}

const jobs = new Map();
let seq = 0;

// Append streamed text (spawn driver) line-by-line, sanitized + capped.
function pushLog(job, chunk) {
  for (const line of clean(chunk).split(/\r?\n/)) {
    if (line === "") continue;
    job.log.push(line);
    if (job.log.length > LOG_CAP) job.log.shift();
  }
}
// Replace the whole log (moc driver re-renders from the LogSession each poll).
function setLog(job, lines) {
  job.log = lines.map(clean).slice(-LOG_CAP);
}

function buildTask({ prompt, slug, title }) {
  return [
    "Use the pages MCP server to create and publish a client dashboard page.",
    `Page slug: ${slug}`,
    `Page title: ${title || slug}`,
    "",
    "Write self-contained, themed HTML using the Flag design system (semantic CSS",
    "tokens/classes already served by the host — do NOT pull external CDNs).",
    "Page requirements from the requester:",
    prompt,
    "",
    'Steps: call deploy_page with { slug, title, html, render_mode: "themed",',
    "publish: true }. Then call page_urls and report the resulting URLs. Do not ask",
    "follow-up questions — make reasonable choices and finish.",
  ].join("\n");
}

// ── driver: spawn the local cutlass CLI ──────────────────────────────────────
function startSpawn(job, task) {
  let child;
  try {
    child = spawn(CUTLASS_BIN, ["-task", task], { cwd: CUTLASS_DIR, env: process.env });
  } catch (err) {
    job.status = "error";
    pushLog(job, "failed to spawn cutlass: " + err.message);
    return;
  }
  pushLog(job, `▸ cutlass ${CUTLASS_BIN} (cwd ${CUTLASS_DIR})`);
  child.stdout.on("data", (d) => pushLog(job, d));
  child.stderr.on("data", (d) => pushLog(job, d));
  child.on("error", (err) => { job.status = "error"; pushLog(job, "spawn error: " + err.message); });
  const killer = setTimeout(() => {
    pushLog(job, `✗ timed out after ${Math.round(MAX_MS / 1000)}s — killing`);
    job.status = "error";
    child.kill("SIGKILL");
  }, MAX_MS);
  child.on("close", (code) => {
    clearTimeout(killer);
    job.code = code;
    if (job.status === "running") job.status = code === 0 ? "done" : "error";
    pushLog(job, `▸ cutlass exited (code ${code})`);
  });
}

// ── driver: dispatch via the MOC task API, then poll ─────────────────────────
const MOC_TERMINAL_OK = new Set(["success"]);
const MOC_TERMINAL_BAD = new Set(["error", "cancelled"]);

async function mocFetch(path, init) {
  const r = await fetch(`${MOC_URL}${path}`, {
    ...init,
    headers: { "X-API-Key": MOC_API_KEY, "Content-Type": "application/json", Accept: "application/json", ...(init && init.headers) },
  });
  const text = await r.text();
  let body;
  try { body = text ? JSON.parse(text) : {}; } catch { body = { raw: text }; }
  if (!r.ok) throw new Error(`MOC ${path} → ${r.status} ${body.error || body.message || text.slice(0, 200)}`);
  return body;
}

function renderLogSession(ls) {
  const lines = [];
  if (ls.title) lines.push(`title: ${ls.title}`);
  for (const m of ls.messages || []) {
    const c = clean(m.content || "").trim();
    if (c) lines.push(`[${m.role || "?"}] ${c}`);
  }
  if (ls.cost != null) lines.push(`cost: $${ls.cost} (${ls.prompt_tokens || 0} in / ${ls.completion_tokens || 0} out)`);
  return lines;
}

function startMoc(job, task) {
  (async () => {
    let taskId;
    try {
      const created = await mocFetch("/tasks", {
        method: "POST",
        body: JSON.stringify({
          prompt: task,
          ...(MOC_MODEL ? { model: MOC_MODEL } : {}),
          ...(MOC_FALLBACK ? { fallback_model: MOC_FALLBACK } : {}),
          ...(MOC_TARGET ? { target_node_name: MOC_TARGET } : {}),
        }),
      });
      taskId = created.id;
      job.mocTaskId = taskId;
      pushLog(job, `▸ MOC task ${taskId} created (status ${created.status || "pending"})`);
    } catch (err) {
      job.status = "error";
      pushLog(job, "MOC create failed: " + err.message);
      return;
    }

    const deadline = Date.now() + MAX_MS;
    for (;;) {
      if (Date.now() > deadline) {
        job.status = "error";
        pushLog(job, `✗ timed out after ${Math.round(MAX_MS / 1000)}s waiting on MOC`);
        return;
      }
      await new Promise((r) => setTimeout(r, POLL_MS));
      let t;
      try {
        t = await mocFetch(`/tasks/${taskId}`);
      } catch (err) {
        pushLog(job, "poll error (will retry): " + err.message);
        continue;
      }
      // Refresh the log from the session (best-effort; ignore log fetch errors).
      try {
        const ls = await mocFetch(`/logs/${taskId}`);
        const lines = renderLogSession(ls);
        if (lines.length) setLog(job, [`▸ MOC task ${taskId}`, ...lines]);
      } catch { /* logs may not exist yet */ }

      const st = (t.status || "").toLowerCase();
      if (MOC_TERMINAL_OK.has(st)) {
        job.status = "done";
        if (t.result) pushLog(job, "result: " + clean(t.result));
        pushLog(job, `▸ MOC task ${taskId} succeeded`);
        return;
      }
      if (MOC_TERMINAL_BAD.has(st)) {
        job.status = "error";
        if (t.error_message) pushLog(job, "error: " + clean(t.error_message));
        pushLog(job, `▸ MOC task ${taskId} ${st}`);
        return;
      }
      // else still pending/assigned/leased/running/analyzing → keep polling
    }
  })().catch((err) => {
    job.status = "error";
    pushLog(job, "moc driver crashed: " + (err && err.message));
  });
}

const router = express.Router();
router.use(requireAdminJSON);

// POST /compose — start a run via the configured driver. Admin cookie + CSRF.
router.post("/", requireCsrf, (req, res) => {
  const b = req.body || {};
  const prompt = typeof b.prompt === "string" ? b.prompt.trim() : "";
  const slug = (typeof b.slug === "string" ? b.slug : "").trim().toLowerCase();
  const title = typeof b.title === "string" ? b.title.trim().slice(0, 200) : "";

  if (!prompt) return res.status(400).json({ error: "prompt is required", code: "no_prompt" });
  // Generous cap: a pasted MOC protocol YAML is tens of KB. Still bounded.
  if (prompt.length > 200000) return res.status(400).json({ error: "prompt too long (200k max)", code: "too_long" });
  if (!SLUG_RE.test(slug)) return res.status(400).json({ error: "slug must be lowercase letters/digits with single hyphens", code: "bad_slug" });

  const id = String(++seq);
  const job = { id, driver: DRIVER, status: "running", code: null, slug, title, startedAt: Date.now(), log: [] };
  jobs.set(id, job);

  const task = buildTask({ prompt, slug, title });
  if (DRIVER === "moc") startMoc(job, task);
  else startSpawn(job, task);

  return res.status(202).json({ jobId: id, driver: DRIVER });
});

// GET /compose/:id — poll job status + log tail (the panel reads this).
router.get("/:id", (req, res) => {
  const job = jobs.get(req.params.id);
  if (!job) return res.status(404).json({ error: "no such job", code: "no_job" });
  res.json({ status: job.status, code: job.code, slug: job.slug, title: job.title, driver: job.driver, log: job.log.join("\n") });
});

module.exports = { router, enabled, driverLabel };
