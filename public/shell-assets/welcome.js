"use strict";
// welcome.js — drives the admin landing page. Read-only: fetches the page index
// from GET /api/v1/admin/pages and renders a Flag-themed grid of clickable cards
// (one per page), each linking to its per-slug admin shell (/admin/<slug>). It
// never mutates anything, so it carries no CSRF token.

(function () {
  // Read bootstrap from the JSON data island (inline scripts are CSP-blocked).
  const boot = JSON.parse(document.getElementById("pages-bootstrap")?.textContent || "{}");
  const { csrf } = boot;
  const app = document.getElementById("app");
  let composeRef = null; // set by composePanel(); lets a card's "Revise" drive the panel

  const el = (tag, attrs = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") n.className = v;
      else if (k === "html") n.innerHTML = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else if (v != null) n.setAttribute(k, v);
    }
    for (const kid of kids) if (kid != null) n.append(kid.nodeType ? kid : document.createTextNode(kid));
    return n;
  };

  async function getJSON(url) {
    const r = await fetch(url, { headers: { Accept: "application/json" } });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || r.status);
    return j;
  }
  async function postJSON(url, body) {
    const r = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf || "" },
      body: JSON.stringify(body || {}),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || r.status);
    return j;
  }
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  const fmt = (ts) => (ts ? new Date(ts).toLocaleString() : "—");

  // The single status that best describes a page, mapped to a badge tone.
  function statusOf(p) {
    if (p.disabled) return { label: "disabled", cls: "error" };
    if (p.published_version_id) return { label: "live", cls: "live" };
    if (p.require_approval) return { label: "approval", cls: "pending" };
    return { label: "draft", cls: "draft" };
  }

  function card(p) {
    const st = statusOf(p);
    // "View live" goes through the dashboard /view broker (SSO → content-host
    // session), which works for staff on BOTH password and Elcano-only pages.
    // Linking straight to the content host would just hit the staff-only gate.
    const meta = el("div", { class: "page-card__meta" },
      el("span", {}, p.published_version_id ? `Version ${p.published_version_id}` : "No published version"),
      el("span", { class: "sep" }, "·"),
      el("span", {}, `Updated ${fmt(p.updated_at)}`)
    );
    const foot = el("div", { class: "page-card__foot" },
      el("a", { class: "btn btn-sm btn-primary", href: `/admin/${encodeURIComponent(p.slug)}` }, "Open"),
      p.published_version_id
        ? el("a", { class: "btn btn-sm", href: `/view/${encodeURIComponent(p.slug)}`, target: "_blank", rel: "noopener" }, "View live")
        : null,
      // Revise: drive the compose panel in audit mode for this page (dev only).
      boot.compose && p.published_version_id
        ? el("button", { class: "btn btn-sm", onclick: () => reviseFor(p.slug) }, "Revise")
        : null
    );
    return el("div", { class: "card page-card" },
      el("div", { class: "page-card__head" },
        el("div", { class: "spread" },
          el("span", { class: "status " + st.cls }, el("span", { class: "dot" }), st.label),
          el("code", { class: "page-card__slug mono" }, "/" + p.slug)
        ),
        el("h3", { class: "page-card__title" }, p.title || p.slug)
      ),
      meta,
      foot
    );
  }

  // ── compose panel (DEV/TEST only; shown when the server enables it) ──
  // Describe a page → POST spawns the Cutlass CLI → it deploys back via MCP →
  // we poll the job log and refresh the grid when it finishes.
  const CREATE_PH = "Describe the page — e.g. 'A KPI dashboard for Omnicom: three metric cards (spend, impressions, CTR) and a bar chart of monthly spend.'";
  const REVISE_PH = "What's wrong / what to change — e.g. 'the WoW % colors are flipped; the chart y-axis should start at 0; tighten the KPI cards and drop the appendix.'";

  function composePanel() {
    const promptTa = el("textarea", { rows: "4", required: "", placeholder: CREATE_PH });
    const slugIn = el("input", { type: "text", placeholder: "omnicom-q2", autocapitalize: "off", autocomplete: "off", spellcheck: "false" });
    const titleIn = el("input", { type: "text", placeholder: "Omnicom Q2 Dashboard" });
    const titleRow = el("label", {}, el("span", { class: "label-text" }, "Title (optional)"), titleIn);
    const status = el("span", { class: "muted compose__status" });
    const logPre = el("pre", { class: "compose__log", style: "display:none" });
    const btn = el("button", { class: "btn btn-primary", type: "submit" }, "Generate with Cutlass");
    const heading = el("h2", { style: "margin:0" }, "Compose with Cutlass");
    const tag = el("span", { class: "tag" }, "Dev");
    const intro = el("p", { class: "compose-card__intro" },
      "Describe a page and Cutlass writes themed HTML and publishes it here. Runs the local Cutlass agent; a run typically takes 30–90 seconds.");
    const promptLabel = el("span", { class: "label-text" }, "Prompt");
    let running = false;
    let mode = "create";

    function setMode(m, slug) {
      mode = m === "revise" ? "revise" : "create";
      if (mode === "revise") {
        heading.textContent = "Revise with Cutlass";
        tag.textContent = "Audit";
        intro.textContent = "Cutlass reads the current published page, audits it against your notes, and publishes an improved version. Roll back anytime from the page's admin view.";
        promptLabel.textContent = "What to change";
        promptTa.placeholder = REVISE_PH;
        if (slug) slugIn.value = slug;
        slugIn.setAttribute("readonly", "");
        titleRow.style.display = "none";
        btn.textContent = "Revise with Cutlass";
      } else {
        heading.textContent = "Compose with Cutlass";
        tag.textContent = "Dev";
        intro.textContent = "Describe a page and Cutlass writes themed HTML and publishes it here. Runs the local Cutlass agent; a run typically takes 30–90 seconds.";
        promptLabel.textContent = "Prompt";
        promptTa.placeholder = CREATE_PH;
        slugIn.removeAttribute("readonly");
        titleRow.style.display = "";
        btn.textContent = "Generate with Cutlass";
      }
    }

    async function run(ev) {
      ev.preventDefault();
      if (running) return;
      const prompt = promptTa.value.trim();
      // Normalize: lowercase, strip slashes/spaces → hyphens, collapse/trim hyphens.
      const slug = slugIn.value.trim().toLowerCase()
        .replace(/^\/+|\/+$/g, "")
        .replace(/[\s_]+/g, "-")
        .replace(/[^a-z0-9-]+/g, "")
        .replace(/-+/g, "-").replace(/^-|-$/g, "");
      slugIn.value = slug;
      if (!prompt) { status.textContent = mode === "revise" ? "describe what to change" : "enter a prompt"; return; }
      if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(slug)) { status.textContent = "slug: lowercase letters/digits with single hyphens"; return; }
      running = true; btn.disabled = true;
      logPre.style.display = "block"; logPre.textContent = "";
      status.textContent = mode === "revise" ? "auditing with Cutlass…" : "starting Cutlass…";
      try {
        const { jobId } = await postJSON("/api/v1/admin/compose", { prompt, slug, title: titleIn.value.trim(), mode });
        for (;;) {
          await sleep(1500);
          const j = await getJSON("/api/v1/admin/compose/" + jobId);
          logPre.textContent = j.log || "";
          logPre.scrollTop = logPre.scrollHeight;
          if (j.status === "running") { status.textContent = "Cutlass is working…"; continue; }
          if (j.status === "done") {
            status.textContent = `done ✓ — published /${j.slug}`;
            await sleep(700);
            load(); // re-render the grid (resets the panel to create mode)
          } else {
            status.textContent = "failed — see log below";
          }
          break;
        }
      } catch (e) {
        status.textContent = "error: " + e.message;
      } finally {
        running = false; btn.disabled = false;
      }
    }

    const panel = el("div", { class: "card compose-card" },
      el("div", { class: "spread" }, heading, tag),
      intro,
      el("form", { class: "compose", onsubmit: run },
        el("label", {}, promptLabel, promptTa),
        el("div", { class: "compose__row" },
          el("label", {}, el("span", { class: "label-text" }, "Slug"), slugIn),
          titleRow
        ),
        el("div", { class: "compose__actions" }, btn, status),
        logPre
      )
    );
    composeRef = { panel, promptTa, setMode };
    return panel;
  }

  // Drive the compose panel into audit/revise mode for an existing page.
  function reviseFor(slug) {
    if (!composeRef) return;
    composeRef.setMode("revise", slug);
    composeRef.promptTa.value = "";
    composeRef.panel.scrollIntoView({ behavior: "smooth", block: "start" });
    composeRef.promptTa.focus();
  }

  function render({ pages }) {
    app.innerHTML = "";
    const live = pages.filter((p) => p.published_version_id && !p.disabled).length;

    // ── page header ──
    app.append(el("header", { class: "page-head" },
      el("div", { class: "page-head__top" },
        el("div", {},
          el("p", { class: "overline" }, "Admin"),
          el("h1", {}, "Client Pages")
        ),
        el("div", { class: "stats" },
          el("span", { class: "stat" }, el("b", {}, String(pages.length)), el("span", {}, pages.length === 1 ? "page" : "pages")),
          el("span", { class: "stat" }, el("b", {}, String(live)), el("span", {}, "live"))
        )
      ),
      el("p", { class: "page-head__sub" },
        "Versioned, Flag-themed client pages. Open a page to review versions, preview, publish, or roll back.")
    ));

    // ── compose (dev/test) ──
    if (boot.compose) app.append(composePanel());

    // ── empty state ──
    if (!pages.length) {
      app.append(el("div", { class: "card empty" },
        el("h2", {}, "No pages yet"),
        el("p", { class: "muted" }, "Pages are created by agents (Cutlass / chat) over MCP, or via the REST API."),
        el("p", { class: "muted" }, "Once a page is deployed it will appear here.")
      ));
      return;
    }

    // ── page grid ──
    app.append(el("div", { class: "section-label" }, el("span", { class: "overline" }, "All pages")));
    app.append(el("div", { class: "page-grid" }, ...pages.map(card)));
  }

  function load() {
    getJSON("/api/v1/admin/pages").then(render, (e) => {
      app.innerHTML = "";
      app.append(el("div", { class: "card" },
        el("h2", {}, "Couldn't load pages"),
        el("p", { class: "muted" }, String(e.message))));
    });
  }
  load();
})();
