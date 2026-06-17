"use strict";
// welcome.js — drives the admin landing page. Read-only: fetches the page index
// from GET /api/v1/admin/pages and renders a Flag-themed grid of clickable cards
// (one per page), each linking to its per-slug admin shell (/admin/<slug>). It
// never mutates anything, so it carries no CSRF token.

(function () {
  // Read bootstrap from the JSON data island (inline scripts are CSP-blocked).
  const boot = JSON.parse(document.getElementById("pages-bootstrap")?.textContent || "{}");
  const { contentOrigin } = boot;
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
