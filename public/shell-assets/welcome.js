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
    const meta = el("div", { class: "page-card__meta muted" },
      el("span", {}, p.published_version_id ? `live · #${p.published_version_id}` : "not published"),
      el("span", {}, "·"),
      el("span", {}, `updated ${fmt(p.updated_at)}`)
    );
    const foot = el("div", { class: "page-card__foot row" },
      el("a", { class: "btn btn-sm btn-primary", href: `/admin/${encodeURIComponent(p.slug)}` }, "Open admin"),
      p.published_version_id
        ? el("a", { class: "btn btn-sm", href: `/view/${encodeURIComponent(p.slug)}`, target: "_blank", rel: "noopener" }, "View live ↗")
        : null
    );
    return el("div", { class: "card page-card" },
      el("div", { class: "page-card__head" },
        el("div", { class: "spread" },
          el("span", { class: "page-card__kicker muted" }, "page"),
          el("span", { class: "badge " + st.cls }, st.label)
        ),
        el("h3", { class: "page-card__title" }, p.title || p.slug),
        el("code", { class: "page-card__slug" }, "/" + p.slug)
      ),
      meta,
      foot
    );
  }

  // ── compose panel (DEV/TEST only; shown when the server enables it) ──
  // Describe a page → POST spawns the Cutlass CLI → it deploys back via MCP →
  // we poll the job log and refresh the grid when it finishes.
  function composePanel() {
    const promptTa = el("textarea", { rows: "4", required: "",
      placeholder: "Describe the page — e.g. 'A KPI dashboard for Omnicom: three metric cards (spend, impressions, CTR) and a bar chart of monthly spend.'" });
    const slugIn = el("input", { type: "text", placeholder: "omnicom-q2", autocapitalize: "off", autocomplete: "off", spellcheck: "false" });
    const titleIn = el("input", { type: "text", placeholder: "Omnicom Q2 Dashboard" });
    const status = el("span", { class: "muted compose__status" });
    const logPre = el("pre", { class: "compose__log", style: "display:none" });
    const btn = el("button", { class: "btn btn-primary", type: "submit" }, "Generate with Cutlass");
    let running = false;

    async function run(ev) {
      ev.preventDefault();
      if (running) return;
      const prompt = promptTa.value.trim();
      // Normalize: lowercase, strip slashes/spaces → hyphens, collapse/trim hyphens.
      const slug = slugIn.value.trim().toLowerCase()
        .replace(/^\/+|\/+$/g, "")        // drop leading/trailing slashes (e.g. "/test")
        .replace(/[\s_]+/g, "-")           // spaces/underscores → hyphen
        .replace(/[^a-z0-9-]+/g, "")       // drop anything else
        .replace(/-+/g, "-").replace(/^-|-$/g, "");
      slugIn.value = slug; // reflect the cleaned value back to the user
      if (!prompt) { status.textContent = "enter a prompt"; return; }
      if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(slug)) { status.textContent = "slug: lowercase letters/digits with single hyphens"; return; }
      running = true; btn.disabled = true;
      logPre.style.display = "block"; logPre.textContent = "";
      status.textContent = "starting Cutlass…";
      try {
        const { jobId } = await postJSON("/api/v1/admin/compose", { prompt, slug, title: titleIn.value.trim() });
        for (;;) {
          await sleep(1500);
          const j = await getJSON("/api/v1/admin/compose/" + jobId);
          logPre.textContent = j.log || "";
          logPre.scrollTop = logPre.scrollHeight;
          if (j.status === "running") { status.textContent = "Cutlass is working…"; continue; }
          if (j.status === "done") {
            status.textContent = `done ✓ — published /${j.slug}`;
            await sleep(700);
            load(); // re-render the grid; the new page card appears
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

    return el("div", { class: "card compose-card" },
      el("div", { class: "spread" },
        el("h2", {}, "Compose a page with Cutlass"),
        el("span", { class: "badge pending" }, "dev · testing")
      ),
      el("p", { class: "muted" }, "Describe a dashboard; Cutlass writes themed HTML and publishes it here. Runs the local Cutlass CLI (OpenRouter) and can take ~30–90s."),
      el("form", { class: "compose", onsubmit: run },
        el("label", {}, "Prompt", promptTa),
        el("div", { class: "compose__row" },
          el("label", {}, "Slug", slugIn),
          el("label", {}, "Title (optional)", titleIn)
        ),
        el("div", { class: "row" }, btn, status),
        logPre
      )
    );
  }

  function render({ pages }) {
    app.innerHTML = "";

    // ── hero ──
    const live = pages.filter((p) => p.published_version_id && !p.disabled).length;
    app.append(el("section", { class: "hero" },
      el("h1", { class: "hero__title" }, "Welcome to Pages admin"),
      el("p", { class: "hero__sub muted" },
        "Versioned, Flag-themed client pages. Open a page to review versions, preview, publish, or roll back."),
      el("div", { class: "row hero__stats" },
        el("span", { class: "badge" }, `${pages.length} page${pages.length === 1 ? "" : "s"}`),
        el("span", { class: "badge live" }, `${live} live`))
    ));

    // ── compose (dev/test) ──
    if (boot.compose) app.append(composePanel());

    // ── empty state ──
    if (!pages.length) {
      app.append(el("div", { class: "card empty" },
        el("h2", {}, "No pages yet"),
        el("p", { class: "muted" },
          "Pages are created by agents (cutlass / chat) over MCP, or via the REST API. "),
        el("p", { class: "muted" }, "Once a page is deployed it'll show up here.")
      ));
      return;
    }

    // ── page grid ──
    const grid = el("div", { class: "page-grid" }, ...pages.map(card));
    app.append(grid);
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
