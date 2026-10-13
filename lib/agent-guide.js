// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/agent-guide.js — the agent guide behind the get_guide MCP tool.
//
// Pages ships its own instructions for agents: docs/AGENT_WORKFLOW.md (how to
// carry a request end to end) plus the reference guides it links to. They are
// packaged with each release, so the guide an agent reads always matches the
// tools that server actually runs.
//
// Only the fixed topics below are servable. A caller names a topic and a
// section slug, never a path, so nothing else on disk is reachable. Documents
// are split on their "## " headings (outside fenced code), read once, and kept
// in memory; a release replaces the process, so there is nothing to reload.

const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");

const { badRequest, notFound } = require("./apierror");
const { version: PRODUCT_VERSION } = require("../package.json");

const DOCS_DIR = path.join(__dirname, "..", "docs");

// Order is the order of the index: the workflow first, then the references it
// links to. Every file here must be tenant-neutral (test/agent-guide.test.js).
const TOPICS = Object.freeze({
  workflow: "AGENT_WORKFLOW.md",
  authoring: "AUTHORING.md",
  templates: "TEMPLATES.md",
  data_updates: "DATA_UPDATES.md",
  motion: "MOTION.md",
});
const TOPIC_NAMES = Object.freeze(Object.keys(TOPICS));
const SECTION_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

function slugify(title) {
  return title
    .toLowerCase()
    .replace(/[`*_"'“”‘’]/g, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// parseDoc — { title, intro, sections: [{ slug, title, text }] }. A section is
// its "## " heading through the line before the next one, so "###" subsections
// stay inside their parent. Headings inside ``` fences are content, not breaks.
function parseDoc(markdown) {
  const lines = markdown.replace(/\r\n/g, "\n").split("\n");
  const title = (lines.find((line) => line.startsWith("# ")) || "").slice(2).trim();
  const intro = [];
  const sections = [];
  let current = null;
  let fenced = false;
  for (const line of lines) {
    if (/^\s*(```|~~~)/.test(line)) fenced = !fenced;
    if (!fenced && line.startsWith("## ")) {
      const heading = line.slice(3).trim();
      let slug = slugify(heading) || `section-${sections.length + 1}`;
      for (let n = 2; sections.some((s) => s.slug === slug); n++) slug = `${slugify(heading)}-${n}`;
      current = { slug, title: heading, lines: [line] };
      sections.push(current);
      continue;
    }
    (current ? current.lines : intro).push(line);
  }
  return {
    title,
    intro: intro.join("\n").trim(),
    sections: sections.map(({ slug, title: heading, lines: body }) => ({
      slug,
      title: heading,
      text: body.join("\n").trim(),
    })),
  };
}

let loaded = null;

function load() {
  if (loaded) return loaded;
  const topics = {};
  for (const [topic, file] of Object.entries(TOPICS)) {
    topics[topic] = parseDoc(fs.readFileSync(path.join(DOCS_DIR, file), "utf8"));
  }
  loaded = topics;
  return loaded;
}

const bytes = (text) => Buffer.byteLength(text, "utf8");
const sha256 = (text) => crypto.createHash("sha256").update(text, "utf8").digest("hex");

function result(topic, section, title, content, entries) {
  return {
    product_version: PRODUCT_VERSION,
    topic,
    section,
    title,
    content,
    content_sha256: sha256(content),
    entries,
  };
}

function index() {
  const topics = load();
  const entries = TOPIC_NAMES.map((topic) => ({
    topic,
    section: null,
    title: topics[topic].title,
    bytes: bytes(topics[topic].intro) + topics[topic].sections.reduce((n, s) => n + bytes(s.text), 0),
  }));
  const content = [
    "# Pages agent guide",
    "",
    "Start with the workflow. Read the section for the step you are on; the other topics are the reference it links to.",
    "Links to AGENT_WORKFLOW.md, AUTHORING.md, TEMPLATES.md, DATA_UPDATES.md and MOTION.md are the topics workflow, authoring, templates, data_updates and motion.",
    "",
    ...entries.map((e) => `- \`${e.topic}\`: ${e.title}`),
  ].join("\n");
  return result(null, null, "Pages agent guide", content, entries);
}

function topicOverview(topic) {
  const doc = load()[topic];
  const entries = doc.sections.map((s) => ({ topic, section: s.slug, title: s.title, bytes: bytes(s.text) }));
  const content = [doc.intro, "", "Sections:", ...entries.map((e) => `- \`${e.section}\`: ${e.title}`)].join("\n");
  return result(topic, null, doc.title, content, entries);
}

function sectionOf(topic, slug) {
  const doc = load()[topic];
  const found = doc.sections.find((s) => s.slug === slug);
  if (!found) {
    throw notFound(`no section "${slug}" in topic ${topic}`, "guide_section_not_found", {
      topic,
      available: doc.sections.map((s) => s.slug),
    });
  }
  const at = doc.sections.indexOf(found);
  const entries = doc.sections
    .filter((_, i) => i !== at)
    .map((s) => ({ topic, section: s.slug, title: s.title, bytes: bytes(s.text) }));
  return result(topic, found.slug, found.title, found.text, entries);
}

// get — no topic: the index; a topic: its introduction and section list; a
// topic and section: that section's text. Every result lists what can be read
// next, so an agent never has to guess a slug.
function get({ topic, section } = {}) {
  if (section !== undefined && topic === undefined) {
    throw badRequest("a section needs its topic", "guide_topic_required", { topics: [...TOPIC_NAMES] });
  }
  if (topic === undefined) return index();
  if (section === undefined) return topicOverview(topic);
  return sectionOf(topic, section);
}

// render — the text an agent reads. Hosts that pass only text content to the
// model (Fleet among them) get the Markdown itself rather than a JSON string of
// it; the structured result carries the same content plus its metadata.
// A section's text also names the topic's other sections, so a text-only host
// sees everything the structured entries offer.
function render(out) {
  const where = [out.topic, out.section].filter(Boolean).join(" / ") || "index";
  const more = out.section
    ? `\nOther sections in ${out.topic}: ${out.entries.map((e) => e.section).join(", ")}`
    : "";
  return `${out.content}\n\n---\nget_guide · ${where} · Pages ${out.product_version} · sha256 ${out.content_sha256.slice(0, 12)}${more}`;
}

module.exports = { TOPICS, TOPIC_NAMES, SECTION_RE, get, render, parseDoc, slugify };
