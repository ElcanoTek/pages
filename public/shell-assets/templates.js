// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The template library (/admin/templates): browse the stored designs, read what
// each one requires, preview a revision, and add one by hand.
//
// Three things this screen is careful about:
//
//  * A design is UNTRUSTED HTML. Nothing here ever injects template bytes into
//    this document — previewing opens a short-TTL signed URL on the content
//    host, where the sandbox + CSP apply. Schemas and configs are built as text
//    nodes through el(), never as markup.
//  * A VERDICT BELONGS TO THE BYTES IT WAS COMPUTED FROM. The format check is a
//    separate write-nothing request, and Register stays disabled unless the
//    check on record matches the file and name currently in the form. Swapping
//    the file after a passing check must not leave Register armed.
//  * RE-RENDERING MUST NOT MOVE THE OPERATOR. The list and detail are rebuilt
//    wholesale, so focus is restored by id afterwards; the add-a-template form
//    keeps stable input nodes inside a dialog and only swaps its verdict block.

(function () {
  const UI = window.PagesUI;
  const { el, errorState, emptyState, toast, makeDialog, confirmDialog, setBusy, keepingFocus, statusChip, pageHeader, statTile, loadFailed } = UI;
  const { field, runAction, timeWhen, formatCount, slugPath, pathSegment, loadingContent } = UI;
  const boot = UI.bootstrap();
  const api = UI.requestScope("/api/v1/admin");
  const app = document.getElementById("app");

  // "Not recorded" is this screen's fallback wording, passed per call; #151 owns
  // unifying the four wordings across the admin UI.
  const shortHash = (value) => (typeof value === "string" ? value.slice(0, 12) : "—");
  const pretty = (value) => JSON.stringify(value, null, 2);
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

  const CONTRACT_VIEWS = [
    { key: "config", label: "Reference config", pick: (d) => d.reference_config },
    { key: "config_schema", label: "Config schema", pick: (d) => d.config_schema },
    { key: "data_schema", label: "Data schema", pick: (d) => d.data_schema },
  ];

  const state = {
    templates: [],
    selected: null,
    detail: null,
    contractView: "config",
    // The revision currently in the preview frame, and what it rendered with.
    preview: null,
  };

  // Guards against a slower earlier preview-token request landing after a newer
  // one and pointing the frame at the wrong revision.
  let previewRequest = 0;

  // ── rendering ───────────────────────────────────────────────────────────────

  function render() {
    // render() replaces the subtree, which drops focus to <body>. Keyboard
    // operators would lose their place on every Inspect and every Preview. This
    // screen solved it first by hand; keepingFocus is that, shared, so the other
    // three screens get it too and none of them can forget.
    keepingFocus(() => {
      // replaceChildren() stringifies whatever it is given, so a null section
      // renders the word "null". el() filters for us; this call has to do it here.
      app.replaceChildren(...[
        heading(),
        state.templates.length ? listPanel() : emptyPanel(),
        state.detail ? detailPanel() : null,
      ].filter(Boolean));
    }, { fallback: [".row--selected button", "#app h1"] });
  }

  function heading() {
    const pages = state.templates.reduce((total, t) => total + Number(t.page_count || 0), 0);
    return pageHeader({
      title: "Template library",
      intro: "Every stored design, what it needs to render, and the pages built from it.",
      // When the library is empty the state panel below carries this action, with
      // the explanation attached. One invitation is enough.
      actions: state.templates.length
        ? el("button", { id: "tpl-add", class: "btn btn-primary", type: "button", onclick: (event) => openUploadDialog(event.currentTarget) }, "Add a template")
        : null,
      stats: state.templates.length
        ? [statTile(state.templates.length, "template"), statTile(pages, "page built", "pages built")]
        : null,
    });
  }

  function emptyPanel() {
    return el("section", { class: "panel section-block" },
      emptyState(
        "No templates yet",
        "Register a design once and every page built from it carries the same layout. " +
          "Add an HTML file here, or ship it in the repo under templates/<name>/template.html and run pages template sync.",
        el("button", {
          id: "tpl-add-empty",
          class: "btn btn-primary",
          type: "button",
          onclick: (event) => openUploadDialog(event.currentTarget),
        }, "Add a template")
      ));
  }

  function listPanel() {
    const table = el("table", { class: "operation-table", id: "template-list" },
      el("caption", { class: "sr-only" }, "Templates, current revision, pages built from each, and when each was last updated"),
      el("thead", {}, el("tr", {},
        el("th", { class: "template-table__template", scope: "col" }, "Template"),
        el("th", { class: "template-table__num", scope: "col" }, "Revision"),
        el("th", { class: "template-table__num", scope: "col" }, "Pages"),
        el("th", { class: "template-table__when", scope: "col" }, "Updated"),
        el("th", { class: "template-table__actions", scope: "col" }, "Actions"))),
      el("tbody", {}, state.templates.map(listRow)));
    // The wrap already draws the border, radius and shadow; a .panel around it
    // drew a second one, so the library's list had two frames where the index's
    // has one.
    return el("section", { class: "section-block" },
      el("div", { class: "operation-table-wrap" }, table));
  }

  function listRow(template) {
    const count = Number(template.page_count || 0);
    return el("tr", { class: state.selected === template.name ? "row--selected" : null },
      el("td", { "data-label": "Template" },
        el("div", { class: "page-cell" },
          el("strong", {}, template.title || template.name),
          el("code", {}, template.name))),
      el("td", { "data-label": "Revision" }, el("span", { class: "table-meta" }, `Revision ${template.current_revision}`)),
      el("td", { "data-label": "Pages" },
        el("span", { class: "table-meta" }, count === 0 ? "None yet" : String(count))),
      el("td", { "data-label": "Updated" },
        timeWhen(template.updated_at, { class: "table-meta" })),
      el("td", { "data-label": "Actions" },
        el("div", { class: "row-actions" },
          // One action: inspecting now renders the design, so a separate row
          // Preview would open the same thing one panel further down.
          // Ghost, not primary. A filled button is the page's one loudest thing,
          // and a row's action is never that — the index settled this for its own
          // rows and the library kept a purple block per row anyway.
          el("button", {
            id: `tpl-open-${template.name}`,
            class: "btn btn-ghost btn-sm",
            type: "button",
            onclick: () => openTemplate(template.name),
          }, "Inspect"))));
  }

  function detailPanel() {
    const d = state.detail;
    // Named so the section is labelled by it, and so a test can target it: the
    // loading panel inside the preview frame has an <h2> of its own.
    // The detail leads with the human title and keeps the machine name beneath it,
    // as the page detail does. It used to lead with the slug.
    const heading2 = el("h2", { id: "tpl-detail-title", tabindex: "-1" }, d.template.title || d.template.name);
    return el("section", {
      class: "panel section-block template-detail",
      id: "detail",
      "aria-labelledby": "tpl-detail-title",
    },
      el("div", { class: "section-heading section-heading--row" },
        el("div", { class: "page-heading__copy" },
          heading2,
          el("code", {}, d.template.name),
          el("span", { class: "badge" }, `Revision ${d.revision.revision}`)),
        el("div", { class: "cluster" },
          // Retiring is here rather than on the list row: it needs the page count
          // in front of you, which is two panels down.
          el("button", {
            id: "tpl-detail-retire",
            class: "btn btn-sm btn-danger",
            type: "button",
            onclick: (event) => retireTemplate(d, event.currentTarget),
          }, "Retire"),
          el("button", {
            id: "tpl-detail-close",
            class: "btn btn-sm",
            type: "button",
            onclick: () => closeTemplate(),
          }, "Close"))),
      d.template.description ? el("p", {}, d.template.description) : null,
      el("dl", { class: "template-kv" },
        el("dt", {}, "Registered by"),
        el("dd", {}, `${d.revision.author} `, el("span", { class: "muted" }, `(${d.revision.source})`)),
        el("dt", {}, "Registered"),
        el("dd", {}, timeWhen(d.revision.created_at)),
        el("dt", {}, "Content hash"),
        el("dd", {}, el("code", {}, shortHash(d.revision.content_sha256)))),
      previewSection(d),
      d.pages.length ? pagesSection(d.pages) : null,
      contractSection(d),
      revisionsSection(d));
  }

  // The design itself, in place. A library whose whole job is deciding whether a
  // design is the right one should not make you leave to look at it — and every
  // other preview surface in this product renders inline, so this one did not
  // need to be different. Same trust split either way: a short-TTL signed URL on
  // the cookieless content host, sandboxed, in a frame that cannot reach back.
  function previewSection(d) {
    return el("div", { class: "section-block" },
      el("div", { class: "section-heading section-heading--row" },
        el("h3", {}, "The design"),
        el("div", { class: "cluster" },
          el("button", {
            id: "tpl-preview-reload",
            class: "btn btn-sm",
            type: "button",
            onclick: (event) =>
              loadPreview(d.template.name, framedRevision(d), event.currentTarget),
          }, "Reload"),
          el("button", {
            id: "tpl-preview-tab",
            class: "btn btn-sm",
            type: "button",
            onclick: (event) =>
              openPreviewTab(d.template.name, framedRevision(d), event.currentTarget),
          }, "Open full size"))),
      el("div", { class: "panel preview-workspace" },
        el("div", { class: "preview-stage" },
          el("span", {
            id: "tpl-preview-status",
            class: "preview-stage__status",
            role: "status",
            "aria-live": "polite",
          }, "Loading the design"),
          el("div", { id: "tpl-preview-state", class: "state-panel state-panel--loading", role: "status" },
            ...loadingContent("Loading the design", "Preparing a short-lived sandboxed preview…", { level: "h4" })),
          el("iframe", {
            id: "tpl-preview-frame",
            hidden: true,
            sandbox: "allow-scripts",
            referrerpolicy: "no-referrer",
            title: `Preview of ${d.template.name} revision ${d.revision.revision}`,
          }))));
  }

  function pagesSection(pages) {
    const table = el("table", { class: "operation-table" },
      el("caption", { class: "sr-only" }, "Pages built from this template, the revision each is pinned to, and whether it is serving"),
      el("thead", {}, el("tr", {},
        el("th", { scope: "col" }, "Page"),
        el("th", { class: "template-table__num", scope: "col" }, "Revision"),
        el("th", { class: "template-table__num", scope: "col" }, "Design"),
        el("th", { class: "template-table__num", scope: "col" }, "Serving"))),
      el("tbody", {}, pages.map((page) => el("tr", {},
        el("td", { "data-label": "Page" },
          el("a", { href: `/admin/${slugPath(page.slug)}` }, el("code", {}, page.slug))),
        el("td", { "data-label": "Revision" }, el("span", { class: "table-meta" }, String(page.revision))),
        el("td", { "data-label": "Design" },
          page.behind
            ? statusChip("behind")
            : statusChip("current")),
        el("td", { "data-label": "Serving" },
          page.page_is_live
            ? statusChip("live")
            : statusChip("draft", "Not live"))))));
    return el("div", { class: "section-block" },
      el("div", { class: "section-heading" }, el("h3", {}, "Pages built from this")),
      el("div", { class: "operation-table-wrap" }, table));
  }

  // One code block behind a segmented switch. Three stacked scroll regions is a
  // wall; the operator is reading one of these at a time.
  //
  // Switching views mutates these two nodes rather than calling render(). A full
  // rebuild would recreate the preview iframe below, throwing away a loaded
  // design and its signed token just because someone looked at a schema.
  function contractSection(d) {
    const block = el("pre", { class: "code-block" });
    const buttons = new Map();

    function show(key) {
      const view = CONTRACT_VIEWS.find((candidate) => candidate.key === key) || CONTRACT_VIEWS[0];
      state.contractView = view.key;
      block.replaceChildren(pretty(view.pick(d)));
      block.setAttribute("aria-label", view.label);
      for (const [candidate, button] of buttons) {
        button.setAttribute("aria-pressed", String(candidate === view.key));
      }
    }

    const segmented = el("div", { class: "segmented", role: "group", "aria-label": "Which part of the contract to show" },
      CONTRACT_VIEWS.map((view) => {
        const button = el("button", {
          id: `tpl-view-${view.key}`,
          class: "segmented__button",
          type: "button",
          onclick: () => show(view.key),
        }, view.label);
        buttons.set(view.key, button);
        return button;
      }));
    show(state.contractView);
    return el("div", { class: "section-block" },
      el("div", { class: "section-heading" },
        el("h3", {}, "Contract"),
        el("p", { class: "muted" },
          "What a page built from this design must supply. The reference config is a starting point to read — ",
          el("code", {}, "create_page_from_template"),
          " requires a complete config and never merges this in.")),
      el("div", { class: "template-contract" }, segmented, block));
  }

  function revisionsSection(d) {
    const table = el("table", { class: "operation-table" },
      el("caption", { class: "sr-only" }, "Every registered revision of this template, newest first"),
      el("thead", {}, el("tr", {},
        el("th", { class: "template-table__num", scope: "col" }, "Revision"),
        el("th", { class: "template-table__hash", scope: "col" }, "Hash"),
        el("th", { scope: "col" }, "Registered by"),
        el("th", { class: "template-table__when", scope: "col" }, "When"),
        el("th", { scope: "col" }, "Note"),
        el("th", { class: "template-table__num", scope: "col" }, "Actions"))),
      el("tbody", {}, d.revisions.map((revision) => el("tr", {},
        el("td", { "data-label": "Revision" },
          el("span", { class: "table-meta" }, String(revision.revision)),
          revision.is_current ? statusChip("current") : null),
        el("td", { "data-label": "Hash" }, el("code", {}, shortHash(revision.content_sha256))),
        el("td", { "data-label": "Registered by" }, el("span", { class: "table-meta" }, revision.author)),
        el("td", { "data-label": "When" },
          timeWhen(revision.created_at, { class: "table-meta" })),
        el("td", { "data-label": "Note" }, el("span", { class: "table-meta" }, revision.note || "—")),
        el("td", { "data-label": "Actions" },
          el("div", { class: "row-actions" },
            el("button", {
              id: `tpl-preview-revision-${revision.revision}`,
              class: "btn btn-sm",
              type: "button",
              // Several of these sit in one table, so the name has to say which.
              "aria-label": `Preview revision ${revision.revision}`,
              onclick: (event) => loadPreview(d.template.name, revision.revision, event.currentTarget),
            }, "Preview")))))));
    return el("div", { class: "section-block" },
      el("div", { class: "section-heading" }, el("h3", {}, "Revisions")),
      el("div", { class: "operation-table-wrap" }, table));
  }

  // ── actions ─────────────────────────────────────────────────────────────────

  async function load() {
    try {
      const { templates } = await api("/templates");
      state.templates = templates;
      if (state.selected && !templates.some((t) => t.name === state.selected)) {
        state.selected = null;
        state.detail = null;
      }
      render();
    } catch (error) {
      app.replaceChildren(loadFailed("the template library", error, () => load()));
    }
  }

  async function openTemplate(name) {
    try {
      state.detail = await api(`/templates/${pathSegment(name)}`);
      state.selected = name;
      state.contractView = "config";
      state.preview = null;
      render();
      const panel = document.getElementById("detail");
      if (panel) {
        panel.scrollIntoView({ behavior: reducedMotion.matches ? "auto" : "smooth", block: "start" });
        // Land the operator on the heading rather than leaving focus on a button
        // that no longer exists after the rebuild.
        const title = document.getElementById("tpl-detail-title");
        if (title) title.focus({ preventScroll: true });
      }
      // Seeing the design is the point of opening it, so it loads without being
      // asked for. Not awaited: the contract and revision tables are already
      // readable while the frame fetches its token.
      loadPreview(name, state.detail.revision.revision, null);
    } catch (error) {
      toast(`Couldn't open ${name}: ${error.message}`, { tone: "error" });
    }
  }

  function closeTemplate() {
    state.detail = null;
    state.selected = null;
    state.preview = null;
    // Any in-flight preview belongs to a frame that no longer exists.
    previewRequest += 1;
    render();
    const back = document.getElementById("tpl-add");
    if (back) back.focus();
  }

  // Retiring frees the name, which is the point: a mistyped template used to be
  // permanent. The confirmation says what actually happens, and says it
  // differently once pages exist — those keep serving but lose the design link.
  async function retireTemplate(d, trigger) {
    const built = d.pages.length;
    const approved = await confirmDialog({
      title: `Retire ${d.template.name}?`,
      kicker: "Template library",
      message: built === 0
        ? "It leaves the library and the name becomes reusable. Revisions are kept."
        : `${built} page${built === 1 ? "" : "s"} ${built === 1 ? "was" : "were"} built from this. ` +
          `${built === 1 ? "It" : "They"} will keep serving — each page carries its own copy of the design — but ` +
          `${built === 1 ? "it" : "they"} can no longer be re-rendered from this template.`,
      confirmLabel: built === 0 ? "Retire" : `Retire anyway`,
      danger: true,
      trigger,
    });
    if (!approved) return;
    // load() stays outside run() so the confirmation of a destructive action is
    // spoken the moment the DELETE lands, not a whole list round trip later.
    // keepBusy holds the button disabled across that reload, exactly as the
    // sequential version did.
    const { ok } = await runAction({
      button: trigger,
      busyLabel: "Retiring…",
      idleLabel: "Retire",
      keepBusy: true,
      success: `${d.template.name} retired. The name is free to re-register.`,
      failure: `Couldn't retire ${d.template.name}`,
      run: async () => {
        await api(`/templates/${pathSegment(d.template.name)}${built > 0 ? "?force=true" : ""}`, {
          method: "DELETE",
        });
        state.detail = null;
        state.selected = null;
        state.preview = null;
        previewRequest += 1;
      },
    });
    if (ok) await load();
  }

  async function previewToken(name, revision) {
    return api(`/templates/${pathSegment(name)}/preview-token`, {
      body: { revision: revision === null ? undefined : revision, render_mode: "themed" },
    });
  }

  // Reload and Open full size act on whatever is in the frame, which may be an
  // older revision the operator chose from the revisions table.
  function framedRevision(d) {
    return state.preview && state.preview.revision !== undefined
      ? state.preview.revision
      : d.revision.revision;
  }

  // What the preview is showing matters as much as that it renders: a template
  // ships an empty #pages-data so no page inherits its rows, so without an
  // example dataset the honest label is "empty state", not "here is the design".
  function previewLabel(result) {
    return result.has_sample_data
      ? `Revision ${result.revision}, rendered with the design's example data`
      : `Revision ${result.revision}, empty state — this design ships no example data`;
  }

  async function loadPreview(name, revision, trigger) {
    const requestId = ++previewRequest;
    const frame = document.getElementById("tpl-preview-frame");
    const panel = document.getElementById("tpl-preview-state");
    const status = document.getElementById("tpl-preview-status");
    if (!frame || !panel || !status) return;
    if (trigger) setBusy(trigger, true, "Loading…");
    frame.hidden = true;
    panel.hidden = false;
    panel.className = "state-panel state-panel--loading";
    panel.replaceChildren(...loadingContent("Loading the design", "Preparing a short-lived sandboxed preview…", { level: "h4" }));
    status.textContent = "Loading the design";
    try {
      const result = await previewToken(name, revision);
      if (requestId !== previewRequest) return;
      frame.src = result.url;
      frame.onload = () => {
        if (requestId !== previewRequest) return;
        panel.hidden = true;
        frame.hidden = false;
        status.textContent = previewLabel(result);
      };
      state.preview = result;
    } catch (error) {
      if (requestId !== previewRequest) return;
      frame.hidden = true;
      panel.hidden = false;
      // Become the error panel rather than nesting one inside the loading panel —
      // the same defect the page detail had, and for the same reason.
      panel.className = "state-panel state-panel--error";
      panel.replaceChildren(
        el("h4", { class: "state-panel__title" }, "Preview unavailable"),
        el("p", {}, `${error.message}. Select Reload to try again.`)
      );
      status.textContent = "Preview failed";
      toast(`Preview failed: ${error.message}`, { tone: "error" });
    } finally {
      if (trigger && trigger.isConnected) setBusy(trigger, false);
    }
  }

  // Full size, for reading a dense dashboard. Still the signed content-host URL;
  // noopener so it cannot reach back into this trusted document.
  async function openPreviewTab(name, revision, trigger) {
    await runAction({
      button: trigger,
      busyLabel: "Opening…",
      idleLabel: "Open full size",
      failure: "Preview failed",
      run: async () => {
        const result = await previewToken(name, revision);
        const opened = window.open(result.url, "_blank", "noopener");
        if (!opened) toast("Your browser blocked the preview window. Allow pop-ups for this site.", { tone: "error" });
      },
    });
  }

  // ── add a template ──────────────────────────────────────────────────────────

  // The form lives in a dialog with STABLE input nodes: nothing re-renders the
  // fields, so nothing can wipe what the operator typed. Only the verdict block
  // is replaced. `check` records which bytes and which name it was computed
  // from, and Register is armed only while both still match the form.
  function openUploadDialog(trigger) {
    let html = null;
    let fileName = "";
    let check = null;
    // A name derived from the filename follows the file. A name the operator
    // typed is theirs, and picking another file must not overwrite it.
    let nameIsDerived = false;

    const fileInput = el("input", { type: "file", id: "tpl-file", accept: ".html,.htm,text/html" });
    const fileHelp = el("span", { class: "field-help" }, "Nothing attached yet.");
    const nameInput = el("input", { type: "text", id: "tpl-name", placeholder: "nwm-campaign-dashboard", autocomplete: "off" });
    const titleInput = el("input", { type: "text", id: "tpl-title", placeholder: "NWM Campaign Dashboard", autocomplete: "off" });
    const descriptionInput = el("textarea", { id: "tpl-description", rows: "2", placeholder: "What this design is for, and when to reach for it." });
    const verdict = el("div", { id: "tpl-verdict", role: "status", "aria-live": "polite" });

    const modal = makeDialog({
      title: "Add a template",
      kicker: "Template library",
      description:
        "Pick an HTML file. It is checked against the template format before anything is written — four managed " +
        "blocks, both schemas self-contained JSON Schema 2020-12, and the shipped payloads validating against them.",
      closeLabel: "Close add a template",
      onClose() {
        modal.dialog.remove();
      },
    });

    const cancel = el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("cancel") }, "Cancel");
    const register = el("button", { id: "tpl-register", class: "btn btn-primary", type: "button", disabled: true }, "Register");

    let busy = false;

    function currentName() {
      return nameInput.value.trim();
    }

    // `check.result` is the server's verdict; `check.forHtml`/`check.forName` are
    // the inputs it was computed from. Keeping them apart matters: result.name is
    // the NORMALIZED name and is null when the name is rejected, so conflating
    // the two both misreports the verdict and arms Register on a bad name.
    function syncRegister() {
      const fresh = Boolean(
        check &&
        check.result.contract_ok &&
        check.result.name &&
        check.forHtml === html &&
        check.forName === currentName()
      );
      register.disabled = !fresh || busy;
      return fresh;
    }

    function setBusyState(next) {
      busy = next;
      fileInput.disabled = next;
      nameInput.disabled = next;
      cancel.disabled = next;
      syncRegister();
    }

    function showVerdict(node) {
      verdict.replaceChildren(node || "");
    }

    // .note, not .field-help: the verdict is a sibling of the field() rows, not
    // any field's help slot, and nothing ties it to an input by aria-describedby.
    function pendingVerdict(message) {
      showVerdict(el("p", { class: "note" }, message));
    }

    async function runCheck() {
      if (!html) return;
      const forHtml = html;
      const forName = currentName();
      setBusyState(true);
      pendingVerdict("Checking the format…");
      try {
        const result = await api("/templates/validate", {
          body: { html: forHtml, name: forName },
        });
        // A slower earlier request must not overwrite a newer verdict.
        if (forHtml !== html || forName !== currentName()) return;
        check = { result, forHtml, forName };
        showVerdict(verdictNode(result));
      } catch (error) {
        if (forHtml !== html || forName !== currentName()) return;
        check = null;
        showVerdict(el("p", { class: "note note--warning" }, `Check failed: ${error.message}`));
      } finally {
        setBusyState(false);
      }
    }

    fileInput.addEventListener("change", async () => {
      const chosen = fileInput.files && fileInput.files[0];
      if (!chosen) return;
      check = null;
      syncRegister();
      html = await chosen.text();
      fileName = chosen.name;
      fileHelp.replaceChildren(`Attached ${fileName} — ${formatCount(html.length)} characters.`);
      if (!currentName() || nameIsDerived) {
        nameInput.value = fileName.replace(/\.html?$/i, "").toLowerCase().replace(/[^a-z0-9_-]+/g, "-");
        nameIsDerived = true;
      }
      await runCheck();
    });

    // Changing the name invalidates the verdict immediately, and re-checks once
    // the operator stops typing (blur or Enter).
    nameInput.addEventListener("input", () => {
      nameIsDerived = false;
      if (check && check.forName !== currentName()) {
        check = null;
        syncRegister();
        pendingVerdict("Name changed — checking again when you leave the field.");
      }
    });
    nameInput.addEventListener("change", () => {
      if (html && !check) runCheck();
    });

    register.addEventListener("click", async () => {
      if (!syncRegister()) return;
      const preflightFailed = check.result.preflight && check.result.preflight.ok === false;
      if (preflightFailed) {
        const approved = await confirmDialog({
          title: "Register despite preflight errors?",
          kicker: "Template library",
          message:
            "Preflight found errors in this design. Every page built from this template inherits them.",
          confirmLabel: "Register anyway",
          danger: true,
          trigger: register,
        });
        if (!approved) return;
      }
      setBusyState(true);
      pendingVerdict("Registering…");
      try {
        const result = await api("/templates", {
          body: {
            html,
            name: currentName(),
            title: titleInput.value.trim(),
            description: descriptionInput.value.trim(),
            allow_preflight_errors: preflightFailed ? true : undefined,
          },
        });
        modal.close("registered");
        const what = result.deduped ? "was already registered as" : result.created ? "created as" : "registered as";
        toast(`${result.template.name} ${what} revision ${result.revision.revision}.`);
        await load();
        await openTemplate(result.template.name);
      } catch (error) {
        showVerdict(el("p", { class: "note note--warning" }, `Register failed: ${error.message}`));
      } finally {
        setBusyState(false);
      }
    });

    modal.body.append(el("div", { class: "form-stack" },
      field({ id: "tpl-file", label: "Template HTML", control: fileInput, help: fileHelp }),
      field({
        id: "tpl-name",
        label: "Name",
        control: nameInput,
        help: "Lowercase letters, digits, hyphen and underscore. No slashes.",
      }),
      field({ id: "tpl-title", label: "Title", control: titleInput }),
      field({ id: "tpl-description", label: "Description", control: descriptionInput }),
      verdict));
    modal.actions.append(cancel, register);
    modal.open(trigger);
    fileInput.focus();
  }

  function verdictNode(result) {
    const rows = [];
    const add = (term, ...definition) => rows.push(el("dt", {}, term), el("dd", {}, ...definition));

    add("Name", result.name
      ? el("code", {}, result.name)
      : el("strong", {}, result.name_error ? result.name_error.message : "not set"));
    add("Bytes", String(result.bytes));
    add("Template format", result.contract_ok
      ? "Valid — four managed blocks, both schemas 2020-12, payloads satisfy them."
      : el("span", {},
          el("strong", {}, "Invalid"),
          ` — ${result.contract_error.message} `,
          el("code", {}, result.contract_error.code)));

    if (result.contract_ok) {
      add("Config fields", String(Object.keys(result.reference_config).length));
      add("Data keys", (result.data_keys || []).join(", ") || "—");
      add("Ships empty", result.ships_empty === true
        ? "Yes."
        : result.ships_empty === false
          ? el("strong", {}, "No — it carries data rows, which every page built from it would inherit.")
          : "Not applicable.");
    }

    const preflight = result.preflight;
    if (preflight) {
      add("Preflight", preflight.ok
        ? "No problems."
        : el("span", {},
            el("strong", {}, `${preflight.errors.length} error${preflight.errors.length === 1 ? "" : "s"}`),
            " — inherited by every page built from this:",
            el("ul", {}, preflight.errors.slice(0, 10).map((error) =>
              el("li", {}, el("code", {}, error.code), ` ${error.message}`)))));
    }

    return el("dl", { class: "template-kv" }, rows);
  }

  // The shell already server-rendered a loading panel; leave it up until the
  // first payload lands so there is no flash of a second identical one.
  load();
})();
