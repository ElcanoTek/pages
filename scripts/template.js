// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// scripts/template.js — register page templates from FILES, without an agent.
//
//   node scripts/template.js list
//   node scripts/template.js show <name> [--revision N]
//   node scripts/template.js register <file> [--name X] [--title T] [--description D] [--note N]
//   node scripts/template.js sync [dir]        # default: the app's own templates/
//
// Two ways a template reaches Pages, and this is the one that does not involve a
// model: point it at a file on disk. `register` takes any path, so a client
// bundle can carry its own templates and an operator registers them from the
// bundle checkout; `sync` walks a directory of them, which is what a deploy runs
// against the templates/ shipped in this repo.
//
// Idempotent by construction: registration dedupes on content_sha256 against the
// NEWEST revision, so re-running sync on unchanged files creates nothing. Edit a
// template and the next sync records revision N+1 — and still moves no deployed
// page, because every page stays pinned to the revision it was built from. That
// is what makes "git push + pages update" a safe way to ship a design fix.
//
// Requires DATABASE_URL (or PG* env) matching the running server.

const fs = require("node:fs");
const path = require("node:path");
const templates = require("../lib/templates");
const { pool } = require("../lib/db");

const TEMPLATES_DIR = path.join(__dirname, "..", "templates");
const METADATA_FILE = "template.json";
const TEMPLATE_FILE = "template.html";

// The operator is the actor: these registrations are attributable to a person
// with shell access, not to an agent token.
const ACTOR = { actor: `pages-cli:${process.env.SUDO_USER || process.env.USER || "operator"}`, actorType: "system" };

function parseFlags(args) {
  const flags = {};
  const positional = [];
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];
    if (arg.startsWith("--")) {
      const key = arg.slice(2);
      const value = args[i + 1];
      if (value === undefined || value.startsWith("--")) throw new Error(`--${key} needs a value`);
      flags[key] = value;
      i += 1;
    } else {
      positional.push(arg);
    }
  }
  return { flags, positional };
}

// discover — a template is a DIRECTORY containing template.html, so it can carry
// its metadata (and later, assets) beside the document. The directory name is the
// template name unless template.json overrides it, which keeps `sync` predictable:
// what you see in the tree is what gets registered.
function discover(dir) {
  if (!fs.existsSync(dir)) return [];
  const found = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    if (!entry.isDirectory()) continue;
    const htmlPath = path.join(dir, entry.name, TEMPLATE_FILE);
    if (!fs.existsSync(htmlPath)) continue;
    found.push({ name: entry.name, htmlPath, ...readMetadata(path.join(dir, entry.name)) });
  }
  return found;
}

function readMetadata(dir) {
  const metadataPath = path.join(dir, METADATA_FILE);
  if (!fs.existsSync(metadataPath)) return {};
  let parsed;
  try {
    parsed = JSON.parse(fs.readFileSync(metadataPath, "utf8"));
  } catch (error) {
    throw new Error(`${metadataPath}: invalid JSON (${error.message})`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${metadataPath}: must contain a JSON object`);
  }
  const allowed = ["name", "title", "description", "note"];
  for (const key of Object.keys(parsed)) {
    if (!allowed.includes(key)) throw new Error(`${metadataPath}: unknown key ${JSON.stringify(key)}`);
  }
  return parsed;
}

async function registerFile({ name, htmlPath, title, description, note }) {
  const html = fs.readFileSync(htmlPath, "utf8");
  const result = await templates.register({ name, html, title, description, note, source: "cli" }, ACTOR);
  const state = result.deduped ? "unchanged" : result.created ? "created" : "new revision";
  console.log(
    `${result.template.name}: ${state} (revision ${result.revision.revision}, ${result.revision.content_sha256.slice(0, 12)})`
  );
  // Preflight errors in a template are inherited by every page built from it, so
  // they are reported loudly here rather than left in a field nobody reads.
  const problems = result.preflight && result.preflight.ok === false;
  if (problems) {
    console.error(`  ⚠ preflight found ${result.preflight.errors.length} error(s) — every page built from this inherits them:`);
    for (const error of result.preflight.errors.slice(0, 10)) {
      console.error(`      ${error.code}: ${error.message}`);
    }
  }
  return { ...result, preflightOk: !problems };
}

async function main() {
  const [cmd, ...rest] = process.argv.slice(2);
  const { flags, positional } = parseFlags(rest);

  switch (cmd) {
    case "list": {
      const { templates: rows } = await templates.list();
      if (rows.length === 0) {
        console.log("no templates registered");
        break;
      }
      for (const row of rows) {
        console.log(
          `${row.name}  revision ${row.current_revision}  ${row.page_count} page(s)` +
            `${row.title ? `  — ${row.title}` : ""}`
        );
      }
      break;
    }

    case "show": {
      const name = positional[0];
      if (!name) throw new Error("usage: template.js show <name> [--revision N]");
      const revision = flags.revision === undefined ? null : Number(flags.revision);
      const result = await templates.get(name, { revision });
      console.log(JSON.stringify(result, null, 2));
      break;
    }

    case "register": {
      const file = positional[0];
      if (!file) throw new Error("usage: template.js register <file> [--name X] [--title T] [--description D]");
      const htmlPath = path.resolve(file);
      if (!fs.existsSync(htmlPath)) throw new Error(`no such file: ${htmlPath}`);
      // Default the name from the containing directory when the file follows the
      // templates/<name>/template.html convention, else from the file stem.
      const base = path.basename(htmlPath);
      const fallback = base === TEMPLATE_FILE ? path.basename(path.dirname(htmlPath)) : base.replace(/\.html?$/i, "");
      const result = await registerFile({
        name: flags.name || fallback,
        htmlPath,
        title: flags.title,
        description: flags.description,
        note: flags.note,
      });
      if (!result.preflightOk) process.exitCode = 1;
      break;
    }

    case "sync": {
      const dir = positional[0] ? path.resolve(positional[0]) : TEMPLATES_DIR;
      const found = discover(dir);
      if (found.length === 0) {
        console.log(`no templates found under ${dir}`);
        break;
      }
      let failed = 0;
      for (const entry of found) {
        try {
          const result = await registerFile(entry);
          if (!result.preflightOk) failed += 1;
        } catch (error) {
          // One bad template must not stop the rest: a deploy that half-registers
          // is worse than one that reports exactly which file is broken.
          failed += 1;
          console.error(`${entry.name}: FAILED — ${error.message}`);
        }
      }
      console.log(`synced ${found.length - failed}/${found.length} template(s) from ${dir}`);
      if (failed > 0) process.exitCode = 1;
      break;
    }

    default:
      console.error(
        "usage: template.js list | show <name> [--revision N] | register <file> [--name X] | sync [dir]"
      );
      process.exitCode = 2;
  }
}

if (require.main === module) {
  main()
    .then(() => pool.end())
    .catch((error) => {
      console.error(error && (error.message || error));
      process.exitCode = 1;
      return pool.end();
    });
}

module.exports = { discover, readMetadata, parseFlags, TEMPLATES_DIR };
