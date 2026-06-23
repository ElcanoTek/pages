"use strict";
// admin.js — drives the admin shell. Reads/writes ONLY through the admin JSON
// API (/api/v1/admin/*), sending the CSRF token on every mutation. It never
// edits rendered DOM of a page — preview is a sandboxed iframe (invariant #2).

(function () {
  // Read bootstrap from the JSON data island (inline scripts are CSP-blocked).
  const { slug, csrf } = JSON.parse(document.getElementById("pages-bootstrap")?.textContent || "{}");
  const api = `/api/v1/admin/pages/${encodeURIComponent(slug)}`;
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

  let toastTimer;
  function toast(msg, isErr) {
    let t = document.getElementById("toast");
    if (!t) { t = el("div", { id: "toast" }); document.body.append(t); }
    t.textContent = msg;
    t.className = "show" + (isErr ? " err" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (t.className = ""), 3500);
  }

  async function getJSON(url) {
    const r = await fetch(url, { headers: { Accept: "application/json" } });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || r.status);
    return j;
  }
  async function post(path, body) {
    const r = await fetch(api + path, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body || {}),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || `${r.status}`);
    return j;
  }
  // run a mutation, then refresh; surfaces errors as a toast (incl. 409 reloads).
  function act(label, path, body) {
    return post(path, body).then(
      () => { toast(label + " ✓"); load(); },
      (e) => toast(label + " failed: " + e.message, true)
    );
  }

  const fmt = (ts) => (ts ? new Date(ts).toLocaleString() : "");
  const badge = (status, isLive) =>
    isLive ? el("span", { class: "badge live" }, "live")
           : el("span", { class: "badge " + status }, status);

  async function preview(versionId) {
    try {
      const { url } = await post(`/preview-token`, { version_id: versionId });
      let frame = document.getElementById("preview-frame");
      if (!frame) return;
      // sandboxed, no same-origin, no top-nav, no popups; referrer stripped.
      frame.setAttribute("sandbox", "allow-scripts");
      frame.setAttribute("referrerpolicy", "no-referrer");
      frame.src = url;
      document.getElementById("preview-label").textContent = `previewing version ${versionId}`;
    } catch (e) { toast("preview failed: " + e.message, true); }
  }

  // ── Phase 4: source editor ─────────────────────────────────────────────────
  // PLAN §8: edits the stored *source*, never the rendered DOM. Save deploys a
  // new draft (source:"admin") via deploy-source; the admin then clicks Preview
  // (sandboxed iframe) and Publish separately. Lossless — chart markup survives.
  let editorOpen = false;
  async function openEditor(page, published) {
    if (editorOpen) return;
    editorOpen = true;
    const card = document.getElementById("editor-card");
    card.style.display = "";
    const ta = document.getElementById("editor-src");
    const modeSel = document.getElementById("editor-mode");
    const noteIn = document.getElementById("editor-note");
    const status = document.getElementById("editor-status");
    if (!ta.value) {
      // Seed with the live HTML (or the latest version's) so the admin edits
      // from a real starting point, not a blank textarea.
      try {
        const seed = published && published.html ? published.html : "";
        ta.value = seed;
        modeSel.value = published && published.render_mode ? published.render_mode : "themed";
      } catch { /* empty page — start blank */ }
    }
    status.textContent = "";
    ta.focus();
  }
  function closeEditor() {
    editorOpen = false;
    const card = document.getElementById("editor-card");
    if (card) card.style.display = "none";
  }
  async function saveDraft(opts) {
    const ta = document.getElementById("editor-src");
    const modeSel = document.getElementById("editor-mode");
    const noteIn = document.getElementById("editor-note");
    const status = document.getElementById("editor-status");
    const html = ta.value;
    if (!html.trim()) { status.textContent = "empty — nothing to save"; return null; }
    status.textContent = "saving draft…";
    try {
      const r = await post(`/deploy-source`, {
        html,
        render_mode: modeSel.value,
        note: noteIn.value.trim() || "Inline edit",
      });
      status.textContent = r.deduped
        ? `no changes (identical to version #${r.version.id})`
        : `saved as draft #${r.version.id}`;
      noteIn.value = "";
      // Reload so the new version appears in the history + is publishable.
      load();
      return r.version.id;
    } catch (e) {
      status.textContent = "save failed: " + e.message;
      return null;
    }
  }
  async function saveAndPreview() {
    const id = await saveDraft();
    if (id != null) preview(id);
  }

  function versionRow(v, page) {
    const isLive = String(v.id) === String(page.published_version_id);
    const actions = el("div", { class: "row" });
    actions.append(el("button", { class: "btn btn-sm", onclick: () => preview(v.id) }, "Preview"));
    if (v.status === "pending") {
      actions.append(
        el("button", { class: "btn btn-sm btn-primary", onclick: () => act("Approve", `/versions/${v.id}/approve`, { expected_version: page.published_version_id }) }, "Approve"),
        el("button", { class: "btn btn-sm btn-danger", onclick: () => act("Reject", `/versions/${v.id}/reject`) }, "Reject")
      );
    }
    if (v.status === "draft") {
      actions.append(el("button", { class: "btn btn-sm btn-primary", onclick: () => act("Publish", `/publish`, { version_id: v.id, expected_version: page.published_version_id }) }, "Publish"));
    }
    if (v.status === "approved" && !isLive) {
      actions.append(el("button", { class: "btn btn-sm", onclick: () => act("Rollback", `/rollback`, { version_id: v.id, expected_version: page.published_version_id }) }, "Roll back to this"));
    }
    return el("tr", { class: isLive ? "current" : "" },
      el("td", {}, el("code", {}, "#" + v.id)),
      el("td", {}, badge(v.status, isLive)),
      el("td", {}, v.author || ""),
      el("td", {}, v.source || ""),
      el("td", {}, fmt(v.created_at)),
      el("td", {}, v.note || ""),
      el("td", {}, actions)
    );
  }

  function render(data) {
    const { page, versions, pending, themes } = data;
    app.innerHTML = "";

    // ── page controls ──
    const flags = el("div", { class: "row" },
      page.disabled ? el("span", { class: "badge error" }, "disabled") : null,
      page.require_approval ? el("span", { class: "badge pending" }, "approval required") : el("span", { class: "badge" }, "open"),
      el("span", { class: "muted" }, `live: ${page.published_version_id ? "#" + page.published_version_id : "none"}`)
    );
    const themeSel = el("select", { onchange: (e) => act("Theme set", `/theme`, { theme: e.target.value }) },
      ...themes.map((t) => el("option", { value: t.name, selected: page.theme_id == t.id || (!page.theme_id && t.name === "flag") ? "" : null }, t.name)));
    const controls = el("div", { class: "card" },
      el("div", { class: "spread" },
        el("h2", {}, page.title || page.slug),
        flags
      ),
      el("div", { class: "row", style: "margin-top:var(--space-4)" },
        el("button", { class: "btn", onclick: () => act(page.require_approval ? "Approval off" : "Approval on", `/approval`, { require_approval: !page.require_approval }) },
          page.require_approval ? "Disable approval gate" : "Require approval"),
        page.disabled
          ? el("button", { class: "btn", onclick: () => act("Enabled", `/enable`) }, "Enable page")
          : el("button", { class: "btn btn-danger", onclick: () => act("Disabled", `/disable`) }, "Take down (disable)"),
        el("button", { class: "btn", onclick: () => openEditor(page, data.published) }, "Edit source"),
        el("span", { class: "muted" }, "theme:"), themeSel
      )
    );
    app.append(controls);

    // ── source editor (Phase 4) ──────────────────────────────────────────────
    app.append(el("div", { id: "editor-card", class: "card", style: "display:none" },
      el("div", { class: "spread" },
        el("h2", {}, "Edit source"),
        el("button", { class: "btn btn-sm", onclick: closeEditor }, "Close")
      ),
      el("p", { class: "muted", style: "margin-top:0" },
        "Edits the stored HTML source (lossless). Save deploys a new draft; then Preview or Publish it."),
      el("textarea", { id: "editor-src", rows: "18", spellcheck: "false", wrap: "off", placeholder: "<!-- page HTML -->", style: "font-family:'Share Tech Mono',ui-monospace,monospace;font-size:var(--font-size-caption)" }),
      el("div", { class: "row", style: "margin-top:var(--space-3)" },
        el("label", { style: "display:flex;align-items:center;gap:var(--space-2);min-height:0" },
          el("span", { class: "muted" }, "render mode"),
          el("select", { id: "editor-mode", style: "width:auto;min-height:0" },
            el("option", { value: "themed", selected: "" }, "themed"),
            el("option", { value: "raw" }, "raw"))),
        el("label", { style: "display:flex;align-items:center;gap:var(--space-2);min-height:0;flex:1" },
          el("span", { class: "muted" }, "note"),
          el("input", { id: "editor-note", type: "text", placeholder: "what changed", style: "min-height:0" }))
      ),
      el("div", { class: "row", style: "margin-top:var(--space-3)" },
        el("button", { class: "btn btn-primary", onclick: saveDraft }, "Save draft"),
        el("button", { class: "btn", onclick: saveAndPreview }, "Save & preview"),
        el("span", { id: "editor-status", class: "muted" })
      )
    ));

    // ── pending review queue (top) ──
    if (pending.length) {
      const t = el("table", {},
        el("thead", {}, el("tr", {}, ...["", "status", "author", "source", "created", "note", ""].map((h) => el("th", {}, h)))),
        el("tbody", {}, ...pending.map((v) => versionRow(v, page)))
      );
      app.append(el("div", { class: "card" }, el("h2", {}, `Pending review (${pending.length})`), t));
    }

    // ── full history ──
    const hist = el("table", {},
      el("thead", {}, el("tr", {}, ...["", "status", "author", "source", "created", "note", ""].map((h) => el("th", {}, h)))),
      el("tbody", {}, ...versions.map((v) => versionRow(v, page)))
    );
    app.append(el("div", { class: "card" }, el("h2", {}, `Versions (${versions.length})`), hist));

    // ── preview ──
    app.append(el("div", { class: "card" },
      el("div", { class: "spread" }, el("h2", {}, "Preview"), el("span", { id: "preview-label", class: "muted" }, "click Preview on a version")),
      el("iframe", { id: "preview-frame", class: "preview", sandbox: "allow-scripts", referrerpolicy: "no-referrer", title: "version preview" })
    ));
  }

  function load() {
    getJSON(api).then(render, (e) => { app.innerHTML = ""; app.append(el("p", { class: "muted" }, "Failed to load: " + e.message)); });
  }
  load();
})();
