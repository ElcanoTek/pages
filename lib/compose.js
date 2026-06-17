"use strict";
// lib/compose.js — DEV/TEST ONLY: an in-admin "compose a page with Cutlass" panel.
//
// This deliberately crosses the clean separation (PLAN: pages hosts; agents
// author). It exists for local testing of the text→agent→page loop. It is GATED:
// only mounted when PAGES_COMPOSE=1 AND CUTLASS_BIN/CUTLASS_DIR are set, and it is
// admin-cookie + CSRF authenticated. NEVER enable in production — it spawns a CLI
// that holds an OpenRouter key.
//
// Flow: POST /compose {prompt, slug, title?} → spawn `cutlass -task "<wrapped>"`
// (array args, NO shell → the prompt can't inject a command) with cwd=CUTLASS_DIR
// so cutlass reads its own .env (OpenRouter key + PAGES_MCP_URL/TOKEN). Cutlass
// connects back to THIS server's /mcp and deploys+publishes the page. We stream its
// log into an in-memory job the UI polls via GET /compose/:id.

const express = require("express");
const { spawn } = require("node:child_process");
const { requireAdminJSON, requireCsrf } = require("./csrf");

const CUTLASS_BIN = process.env.CUTLASS_BIN || "";
const CUTLASS_DIR = process.env.CUTLASS_DIR || "";
const MAX_MS = Number(process.env.COMPOSE_TIMEOUT_MS || 420000); // hard kill after ~7 min
const SLUG_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const LOG_CAP = 400; // keep the last N lines per job

function enabled() {
  return process.env.PAGES_COMPOSE === "1" && !!CUTLASS_BIN && !!CUTLASS_DIR;
}

const jobs = new Map();
let seq = 0;
// eslint-disable-next-line no-control-regex
const ANSI = /\[[0-9;]*m/g;

function pushLog(job, chunk) {
  // ANSI strips color escapes (incl. ESC); the second strip removes any other
  // C0/C1 control chars (CR, etc.) so the job JSON stays valid — otherwise the
  // browser poll's JSON.parse in welcome.js would throw. Keeps \n (split) + \t.
  // eslint-disable-next-line no-control-regex
  const text = String(chunk).replace(ANSI, "").replace(/[\x00-\x08\x0b-\x1f\x7f]/g, "");
  for (const line of text.split(/\r?\n/)) {
    if (line === "") continue;
    job.log.push(line);
    if (job.log.length > LOG_CAP) job.log.shift();
  }
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

const router = express.Router();
router.use(requireAdminJSON);

// POST /compose — start a Cutlass run. Admin cookie + CSRF.
router.post("/", requireCsrf, (req, res) => {
  const b = req.body || {};
  const prompt = typeof b.prompt === "string" ? b.prompt.trim() : "";
  const slug = (typeof b.slug === "string" ? b.slug : "").trim().toLowerCase();
  const title = typeof b.title === "string" ? b.title.trim().slice(0, 200) : "";

  if (!prompt) return res.status(400).json({ error: "prompt is required", code: "no_prompt" });
  if (prompt.length > 8000) return res.status(400).json({ error: "prompt too long (8k max)", code: "too_long" });
  if (!SLUG_RE.test(slug)) return res.status(400).json({ error: "slug must be lowercase letters/digits with single hyphens", code: "bad_slug" });

  const id = String(++seq);
  const job = { id, status: "running", code: null, slug, title, startedAt: Date.now(), log: [] };
  jobs.set(id, job);

  const task = buildTask({ prompt, slug, title });
  let child;
  try {
    // Array args → cutlass receives `task` as a single argv element; no shell
    // interpolation, so a malicious prompt cannot break out into a command.
    // No -v: keep the high-level progress (tool calls, steps, cost) without the
    // token-by-token stream flooding the log.
    child = spawn(CUTLASS_BIN, ["-task", task], { cwd: CUTLASS_DIR, env: process.env });
  } catch (err) {
    job.status = "error";
    pushLog(job, "failed to spawn cutlass: " + err.message);
    return res.status(202).json({ jobId: id });
  }

  pushLog(job, `▸ cutlass ${CUTLASS_BIN} (cwd ${CUTLASS_DIR})`);
  child.stdout.on("data", (d) => pushLog(job, d));
  child.stderr.on("data", (d) => pushLog(job, d));
  child.on("error", (err) => {
    job.status = "error";
    pushLog(job, "spawn error: " + err.message);
  });
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

  return res.status(202).json({ jobId: id });
});

// GET /compose/:id — poll job status + log tail.
router.get("/:id", (req, res) => {
  const job = jobs.get(req.params.id);
  if (!job) return res.status(404).json({ error: "no such job", code: "no_job" });
  res.json({
    status: job.status,
    code: job.code,
    slug: job.slug,
    title: job.title,
    log: job.log.join("\n"),
  });
});

module.exports = { router, enabled };
