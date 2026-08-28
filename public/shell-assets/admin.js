// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Page detail/review UI. It keeps the existing admin API and sandboxed preview
// pipeline intact while making the selected version the single workflow focus.
(function () {
  const UI = window.PagesUI;
  const { el, icon, toast, makeDialog, confirmDialog, setBusy, keepingFocus, statusChip, copyText, errorState, emptyState, loadFailed, credentialDialog } = UI;
  const { field, runAction, formatWhen, timeWhen, slugPath, loadingContent } = UI;
  const boot = UI.bootstrap();
  const { slug, contentOrigin } = boot;
  // page-switcher.js is still its own module (it loads first and must not depend
  // on window.PagesUI), so this keeps using it for model()/adminPath().
  const switcher = window.PagesPageSwitcher;
  const adminApi = UI.requestScope("/api/v1/admin");
  const pageApi = UI.requestScope(`/api/v1/admin/pages/${slugPath(slug)}`);
  // The `|| {}` is load-bearing. /enable and /disable take no body and are
  // POST-only routes below the CSRF middleware, and request() reads "no body" as
  // "this is a read" — without it, the availability switch's commitSetting call
  // would send those two POSTs as GETs.
  const post = (path, body) => pageApi(path, { body: body || {} });
  const app = document.getElementById("app");
  const switcherRoot = document.getElementById("page-switcher");
  const switcherSelect = document.getElementById("page-switcher-select");
  const switcherCount = document.getElementById("page-switcher-count");
  const switcherPrev = document.getElementById("page-switcher-prev");
  const switcherNext = document.getElementById("page-switcher-next");

  let pageData = null;
  let selectedVersionId = null;
  let reviewFilter = null;
  let previewViewport = "desktop";
  let previewRequest = 0;
  let switcherReady = false;

  // "Not recorded" is this screen's fallback wording, passed per call; #151 owns
  // unifying the four wordings across the admin UI.

  function goToPage(targetSlug) {
    if (targetSlug && targetSlug !== slug) window.location.assign(switcher.adminPath(targetSlug));
  }

  // Navigating on `change` alone made the switcher unusable from the keyboard: on a
  // focused, closed <select> an arrow key moves the selection AND fires change, so
  // the first press left the page — you could never reach the fourth entry, let
  // alone the fortieth. The native select is here precisely for its keyboard
  // semantics (type-ahead, arrows, Home/End), so browsing has to be free and only
  // committing may navigate.
  //
  // A pointer pick is already a commit, so an unprefixed `change` still navigates.
  // A change that follows a key press is a browse: hold it, and let Enter commit.
  let browsing = false;
  const COMMIT_KEYS = new Set(["Enter", "NumpadEnter"]);

  // The settled label, remembered so abandoning a browse restores it verbatim
  // instead of recomputing the navigation model.
  let switcherCountLabel = switcherCount.textContent;

  function stopBrowsing() {
    if (!browsing) return;
    browsing = false;
    switcherSelect.value = slug;
    switcherCount.textContent = switcherCountLabel;
  }

  switcherSelect.addEventListener("keydown", (event) => {
    if (COMMIT_KEYS.has(event.key)) {
      // Read the value after the browser has applied its own commit, so this works
      // whether the dropdown was open (Enter picks the highlighted option) or
      // closed (Enter confirms what the arrows already selected).
      const commit = () => { browsing = false; goToPage(switcherSelect.value); };
      window.setTimeout(commit, 0);
      return;
    }
    if (event.key === "Escape" || event.key === "Tab") return; // the browser reverts / we blur
    browsing = true;
  });
  switcherSelect.addEventListener("change", () => {
    if (!browsing) {
      goToPage(switcherSelect.value);
      return;
    }
    // Say what the pending selection needs, in the live region that is already
    // describing this control.
    switcherCount.textContent = "Press Enter to open";
  });
  switcherSelect.addEventListener("blur", stopBrowsing);
  switcherPrev.addEventListener("click", () => goToPage(switcherPrev.dataset.slug));
  switcherNext.addEventListener("click", () => goToPage(switcherNext.dataset.slug));

  function setStep(button, item, direction) {
    button.disabled = !item;
    if (!item) {
      delete button.dataset.slug;
      button.title = `No ${direction} page`;
      button.setAttribute("aria-label", button.title);
      return;
    }
    button.dataset.slug = item.slug;
    button.title = `${direction[0].toUpperCase() + direction.slice(1)} page: ${item.title} (/${item.slug})`;
    button.setAttribute("aria-label", button.title);
  }

  function renderPageSwitcher(pages) {
    const navigation = switcher.model(pages, slug);
    switcherSelect.replaceChildren();
    if (!navigation.total) {
      switcherSelect.append(el("option", {}, "No pages available"));
      switcherSelect.disabled = true;
      switcherCount.textContent = switcherCountLabel = "0 pages";
    } else {
      if (!navigation.current) switcherSelect.append(el("option", { value: "", selected: true, disabled: true }, "Select a page"));
      navigation.items.forEach((item) => switcherSelect.append(el("option", {
        value: item.slug,
        selected: item.slug === slug,
        title: item.optionLabel,
      }, item.optionLabel)));
      switcherSelect.disabled = false;
      if (navigation.current) {
        switcherSelect.value = navigation.current.slug;
        switcherSelect.title = navigation.current.optionLabel;
        switcherCount.textContent = switcherCountLabel = `${navigation.current.position} of ${navigation.total}`;
        switcherSelect.setAttribute("aria-label", `Switch admin page; current page ${navigation.current.position} of ${navigation.total}`);
      } else {
        switcherCount.textContent = switcherCountLabel = `${navigation.total} ${navigation.total === 1 ? "page" : "pages"}`;
      }
    }
    setStep(switcherPrev, navigation.previous, "previous");
    setStep(switcherNext, navigation.next, "next");
    switcherRoot.setAttribute("aria-busy", "false");
    switcherRoot.removeAttribute("data-error");
    switcherCount.removeAttribute("title");
    browsing = false;
    switcherReady = true;
  }

  function pageSwitcherError(error) {
    switcherRoot.setAttribute("aria-busy", "false");
    switcherRoot.setAttribute("data-error", "");
    if (!switcherReady) {
      switcherSelect.replaceChildren(el("option", {}, "Page list unavailable"));
      switcherSelect.disabled = true;
      setStep(switcherPrev, null, "previous");
      setStep(switcherNext, null, "next");
    }
    switcherCount.textContent = switcherCountLabel = "Unavailable";
    switcherCount.title = error.message;
  }

  function loadPageSwitcher() {
    switcherRoot.setAttribute("aria-busy", "true");
    return adminApi("/pages").then(({ pages }) => renderPageSwitcher(pages), pageSwitcherError);
  }

  function sameId(left, right) {
    return left != null && right != null && String(left) === String(right);
  }

  // Database version ids are global concurrency tokens, not meaningful
  // customer-facing version numbers. Admin detail receives the complete
  // newest-first history, so derive a stable page-local ordinal while keeping
  // the real id for every API path and payload.
  function versionNumber(versionOrId, data = pageData) {
    const id = versionOrId && typeof versionOrId === "object" ? versionOrId.id : versionOrId;
    const versions = data?.versions || [];
    const index = versions.findIndex((version) => sameId(version.id, id));
    return index < 0 ? null : versions.length - index;
  }

  function versionLabel(versionOrId, data = pageData) {
    const number = versionNumber(versionOrId, data);
    return number == null ? "Version" : `Version ${number}`;
  }

  function inlineVersionLabel(versionOrId, data = pageData) {
    const label = versionLabel(versionOrId, data);
    return label[0].toLowerCase() + label.slice(1);
  }

  // A version's own status, in the shared vocabulary. `approved` used to fall
  // through to the draft styling, so a reviewed version that simply was not the
  // live one looked exactly like work nobody had looked at.
  const VERSION_KIND = { pending: "pending", approved: "approved", rejected: "rejected", draft: "draft" };

  function statusBadge(version, page) {
    if (sameId(version.id, page.published_version_id)) return statusChip("live");
    return statusChip(VERSION_KIND[version.status] || "draft");
  }

  function normalizeSelection(data, preferredVersionId) {
    const versions = data.versions || [];
    const pending = versions.filter((version) => version.status === "pending");
    if (!reviewFilter) reviewFilter = pending.length ? "pending" : "all";
    if (reviewFilter === "pending" && !pending.length) reviewFilter = "all";

    const candidate = preferredVersionId != null ? preferredVersionId : selectedVersionId;
    let selected = versions.find((version) => sameId(version.id, candidate));
    if (reviewFilter === "pending" && (!selected || selected.status !== "pending")) selected = pending[0] || null;
    if (!selected) {
      selected = pending[0]
        || versions.find((version) => sameId(version.id, data.page.published_version_id))
        || versions[0]
        || null;
    }
    selectedVersionId = selected ? selected.id : null;
    return selected;
  }

  // The reload stays OUTSIDE run() so the toast lands before it, as it always has.
  // keepBusy is what makes that safe: without it runAction's finally would
  // re-enable this button for the whole reload round trip, and a second click
  // would send a second POST. On success it stays disabled until the reload
  // replaces it; on failure it is restored, still on screen.
  //
  // `parts` names the sections this change can affect. It is what stops a title
  // save from rebuilding the review workspace.
  async function mutate(button, successMessage, path, body, preferredVersionId, parts) {
    const { ok } = await runAction({
      button,
      busyLabel: "Working…",
      keepBusy: true,
      success: successMessage,
      failure: (error) => `${successMessage} failed: ${error.message}`,
      run: async () => { await post(path, body); },
    });
    // The button is disabled for the whole action (keepBusy), which blurs it, so
    // the id has to be captured here rather than read off the document later.
    if (ok) await reload({ parts, preferredVersionId, focusFrom: button && button.id ? button.id : null });
    return ok;
  }

  // Refetch, then re-render only the sections this change can affect. Saving a
  // title has no bearing on the review workspace, and rebuilding it there cost a
  // fresh signed preview token and a 40rem iframe reload every time.
  async function reload({ parts, preferredVersionId, focusFrom } = {}) {
    const data = await pageApi("");
    pageData = data;
    updateLocation(data.page);
    const selected = normalizeSelection(data, preferredVersionId);
    const list = parts && parts.length ? parts : ORDER;
    refresh(list, focusFrom);
    if (list.includes("review") && selected) {
      previewedVersionId = null;
      syncPreview(selected.id);
    }
  }

  function pageOverview(page, pendingCount) {
    return el("header", { class: "panel page-overview" },
      el("div", { class: "page-overview__top" },
        el("div", { class: "page-overview__identity" },
          el("p", { class: "overline" }, "Page review"),
          el("h1", {}, page.title || page.slug),
          el("code", { class: "page-overview__slug" }, `/${page.slug}`)),
        el("div", { class: "page-overview__statuses", role: "group", "aria-label": "Page state" },
          // "Enabled" was every page's default state and badging it was noise, and
          // "Approval required" is a setting — it lives in Settings. What is left
          // is the state a client sees, and the work waiting on a human.
          page.disabled
            ? statusChip("disabled")
            : statusChip(page.has_password || page.require_approval ? "gated" : "live",
                page.published_version_id
                  ? (page.has_password ? "Live · gated" : "Live")
                  : "Nothing live"),
          page.published_version_id
            ? el("span", { class: "table-meta" }, `Live ${inlineVersionLabel(page.published_version_id)}`)
            : null,
          pendingCount
            ? el("a", { class: "page-overview__todo", href: "#review-title" },
                `${pendingCount} ${pendingCount === 1 ? "version" : "versions"} waiting for review`)
            : null)));
  }

  function versionOption(version, page) {
    const button = el("button", {
      id: `version-option-${version.id}`,
      class: "version-option",
      type: "button",
      "aria-current": sameId(version.id, selectedVersionId) ? "true" : "false",
      onclick: () => selectVersion(version.id),
    },
    el("span", { class: "version-option__top" },
      el("strong", {}, versionLabel(version)),
      statusBadge(version, page)),
    el("span", { class: "version-option__meta" },
      el("span", { title: version.author || "Unknown author" }, version.author || "Unknown author"),
      timeWhen(version.created_at)));
    return el("li", {}, button);
  }

  // Confirmation policy, in one place because it was inverted:
  //
  //   Confirm anything a CLIENT will see change, or that cannot be undone from
  //   this screen. Do not confirm anything reversible that only staff see.
  //
  // Approve on an approval-gated page IS publish — it moves the live pointer — and
  // it shipped as one unguarded click, while Reject, which leaves the version in
  // history and changes nothing a client sees, asked "are you sure?". Same for
  // Publish on a draft, and for Disable, which the state machine itself calls "an
  // admin takedown kill switch".
  function confirmClientVisible({ trigger, title, message, confirmLabel, danger }) {
    return confirmDialog({ trigger, title, message, confirmLabel, danger: Boolean(danger) });
  }

  // What a reader needs to know before publishing: which version replaces which,
  // and that the one being replaced is still there afterwards.
  function pointerMoveMessage(page, version) {
    const from = page.published_version_id
      ? `replaces live ${inlineVersionLabel(page.published_version_id)}`
      : "is the first version this page serves";
    const keep = page.published_version_id
      ? ` ${inlineVersionLabel(page.published_version_id)} stays available to roll back to.`
      : "";
    return `${inlineVersionLabel(version)} ${from} for everyone with the client link.${keep}`;
  }

  async function approveVersion(button, version, page) {
    const ok = await confirmClientVisible({
      trigger: button,
      title: `Approve and publish ${inlineVersionLabel(version)}?`,
      message: pointerMoveMessage(page, version),
      confirmLabel: "Approve & publish",
    });
    if (ok) {
      await mutate(button, `Published ${inlineVersionLabel(version)}`, `/versions/${version.id}/approve`, {
        expected_version: page.published_version_id,
      }, undefined, ["overview", "review"]);
    }
  }

  async function publishVersion(button, version, page) {
    const ok = await confirmClientVisible({
      trigger: button,
      title: `Publish ${inlineVersionLabel(version)}?`,
      message: pointerMoveMessage(page, version),
      confirmLabel: "Publish",
    });
    if (ok) {
      await mutate(button, `Published ${inlineVersionLabel(version)}`, "/publish", {
        version_id: version.id,
        expected_version: page.published_version_id,
      }, version.id, ["overview", "review"]);
    }
  }

  // reject is the same server transition for a draft and for a pending version
  // (lib/versions.js: "mark a draft/pending version rejected"), but the two are
  // different acts to a reader: one turns down work submitted for review, the
  // other throws away a draft nobody asked about. Only the words differ.
  async function rejectVersion(button, version, page) {
    const draft = version.status === "draft";
    const approved = await confirmDialog({
      trigger: button,
      title: draft
        ? `Discard ${inlineVersionLabel(version)}?`
        : `Reject ${inlineVersionLabel(version)}?`,
      message: draft
        ? "The draft stays in history and can still be previewed, but it can no longer be published. Nothing a client sees changes."
        : "Rejected versions remain in history but cannot be published.",
      confirmLabel: draft ? "Discard draft" : "Reject version",
      danger: true,
    });
    if (approved) {
      await mutate(
        button,
        draft ? `Discarded ${inlineVersionLabel(version)}` : `Rejected ${inlineVersionLabel(version)}`,
        `/versions/${version.id}/reject`,
        {},
        null
      );
    }
  }

  async function rollbackVersion(button, version, page) {
    const approved = await confirmDialog({
      trigger: button,
      title: `Roll back to ${inlineVersionLabel(version)}?`,
      message: `Clients will see ${inlineVersionLabel(version)} instead of ${page.published_version_id ? inlineVersionLabel(page.published_version_id) : "nothing"}. No version is deleted, so this can be undone.`,
      confirmLabel: "Roll back",
    });
    if (approved) {
      await mutate(button, `Rolled back to ${inlineVersionLabel(version)}`, "/rollback", {
        version_id: version.id,
        expected_version: page.published_version_id,
      }, version.id, ["overview", "review"]);
    }
  }

  function versionActions(version, page, data) {
    if (!version) return el("div", { class: "version-actions" });
    const actions = el("div", { class: "version-actions", role: "group", "aria-label": `Actions for ${inlineVersionLabel(version)}` });
    actions.append(
      // The stage is already showing this version, so the button reloads it. The
      // template library calls the same action "Reload"; this said "Preview", and
      // its own busy-restore said "Reload" — the two disagreed.
      el("button", {
        id: `preview-version-${version.id}`,
        class: "btn btn-sm",
        type: "button",
        onclick: (event) => loadPreview(version.id, event.currentTarget),
      }, "Reload preview"),
      // "Fix a small thing in the version I am reviewing" is the flow this
      // screen exists for, so the editor opens from here, on the selected
      // version — not only from Settings, two sections down, on whatever is
      // live. Offered for every status: a rejected or superseded version is a
      // perfectly good starting point for the next one.
      el("button", {
        class: "btn btn-sm",
        type: "button",
        onclick: (event) => editVersionSource(event.currentTarget, data, version),
      }, "Edit source")
    );
    if (version.status === "pending") {
      actions.append(
        el("button", {
          id: `approve-version-${version.id}`,
          class: "btn btn-sm btn-primary",
          type: "button",
          onclick: (event) => approveVersion(event.currentTarget, version, page),
        }, page.require_approval ? "Approve & publish" : "Approve"),
        el("button", { id: `reject-version-${version.id}`, class: "btn btn-sm btn-danger", type: "button", onclick: (event) => rejectVersion(event.currentTarget, version, page) }, "Reject")
      );
    } else if (version.status === "draft") {
      actions.append(
        el("button", {
          id: `publish-version-${version.id}`,
          class: "btn btn-sm btn-primary",
          type: "button",
          onclick: (event) => publishVersion(event.currentTarget, version, page),
        }, "Publish"),
        // Without this the only affordance on an unwanted draft was the button
        // that makes it live, so bad drafts accumulated in "All versions" forever.
        el("button", {
          id: `discard-version-${version.id}`,
          class: "btn btn-sm btn-danger",
          type: "button",
          onclick: (event) => rejectVersion(event.currentTarget, version, page),
        }, "Discard draft")
      );
    } else if (version.status === "approved" && !sameId(version.id, page.published_version_id)) {
      actions.append(el("button", { id: `rollback-version-${version.id}`, class: "btn btn-sm", type: "button", onclick: (event) => rollbackVersion(event.currentTarget, version, page) }, "Roll back"));
    }
    return actions;
  }

  function versionMetadata(version, page) {
    if (!version) return el("div", { class: "version-detail" });
    return el("div", { class: "version-detail" },
      el("dl", {},
        el("div", {}, el("dt", {}, "Status"), el("dd", {}, sameId(version.id, page.published_version_id) ? "Live / approved" : version.status)),
        el("div", {}, el("dt", {}, "Author"), el("dd", {}, version.author || "Unknown")),
        el("div", {}, el("dt", {}, "Source"), el("dd", {}, version.source || "Unknown")),
        el("div", {}, el("dt", {}, "Created"), el("dd", {}, timeWhen(version.created_at))),
        el("div", { class: "version-detail__note" }, el("dt", {}, "Note"), el("dd", {}, version.note || "No note provided.")),
        version.reviewed_by
          ? el("div", { class: "version-detail__note" }, el("dt", {}, "Reviewed"), el("dd", {}, `${version.reviewed_by} · `, timeWhen(version.reviewed_at)))
          : null));
  }

  async function loadPreview(versionId, trigger) {
    if (versionId == null) return;
    const requestId = ++previewRequest;
    const state = document.getElementById("preview-state");
    const frame = document.getElementById("preview-frame");
    const status = document.getElementById("preview-status");
    if (!frame || !state || !status) return;
    if (trigger) setBusy(trigger, true, "Loading…");
    frame.hidden = true;
    state.hidden = false;
    state.className = "state-panel state-panel--loading";
    state.replaceChildren(...loadingContent(`Loading ${inlineVersionLabel(versionId)}`, "Preparing a short-lived sandboxed preview…", { level: "h4" }));
    status.textContent = `Loading ${inlineVersionLabel(versionId)}`;
    try {
      const result = await post("/preview-token", { version_id: versionId });
      if (requestId !== previewRequest || !sameId(versionId, selectedVersionId)) return;
      frame.setAttribute("sandbox", "allow-scripts");
      frame.setAttribute("referrerpolicy", "no-referrer");
      frame.src = result.url;
      const full = document.getElementById("preview-open-full");
      if (full) {
        full.href = result.url;
        full.hidden = false;
      }
      frame.onload = () => {
        if (requestId !== previewRequest) return;
        state.hidden = true;
        frame.hidden = false;
        status.textContent = `Previewing ${inlineVersionLabel(versionId)}`;
      };
    } catch (error) {
      if (requestId !== previewRequest) return;
      // Nothing is showing, so the next request for this version must not be
      // skipped as "already previewed".
      previewedVersionId = null;
      frame.hidden = true;
      state.hidden = false;
      // Become the error panel rather than nesting one inside the loading panel.
      // The nested version inherited the container's neutral surface and doubled
      // its min-height, so a failure was painted like a slow success and was
      // taller than the thing it replaced — the layout jumped on error.
      state.className = "state-panel state-panel--error";
      state.replaceChildren(
        el("h4", { class: "state-panel__title" }, "Preview unavailable"),
        el("p", {}, `${error.message}. Select Reload preview to try again.`)
      );
      status.textContent = `Preview failed for ${inlineVersionLabel(versionId)}`;
      toast(`Preview failed: ${error.message}`, { tone: "error" });
    } finally {
      if (trigger && trigger.isConnected) setBusy(trigger, false, "Reload preview");
    }
  }

  function reviewSection(data, selected) {
    const { page, versions } = data;
    const pending = versions.filter((version) => version.status === "pending");
    const visible = reviewFilter === "pending" ? pending : versions;
    const needsReview = el("button", {
      class: "segmented__button",
      type: "button",
      "aria-pressed": reviewFilter === "pending" ? "true" : "false",
      onclick: () => {
        if (reviewFilter === "pending") return;
        reviewFilter = "pending";
        selectedVersionId = pending[0]?.id || null;
        refresh(["review"]);
        syncPreview(selectedVersionId);
      },
    }, `Needs review (${pending.length})`);
    const allVersions = el("button", {
      class: "segmented__button",
      type: "button",
      "aria-pressed": reviewFilter === "all" ? "true" : "false",
      onclick: () => {
        if (reviewFilter === "all") return;
        reviewFilter = "all";
        if (!versions.some((version) => sameId(version.id, selectedVersionId))) selectedVersionId = versions[0]?.id || null;
        refresh(["review"]);
        syncPreview(selectedVersionId);
      },
    }, `All versions (${versions.length})`);
    const list = el("ul", { class: "version-list", "aria-label": reviewFilter === "pending" ? "Versions needing review" : "All versions" });
    if (visible.length) list.append(...visible.map((version) => versionOption(version, page)));
    else list.append(el("li", { class: "version-list__empty" }, el("p", {}, reviewFilter === "pending" ? "No versions need review." : "No versions have been created.")));

    const browser = el("div", { class: "panel version-browser" },
      el("div", { class: "version-browser__header" },
        el("h3", {}, "Version queue"),
        el("div", { class: "segmented", role: "group", "aria-label": "Filter versions" }, needsReview, allVersions)),
      list);

    // "How will a client see this on a phone?" is the most common review question
    // and the stage could not answer it. Widths are set on the frame, not the
    // viewport, so the surrounding admin stays put.
    const VIEWPORTS = [
      { key: "desktop", label: "Desktop", width: "100%" },
      { key: "tablet", label: "Tablet", width: "768px" },
      { key: "mobile", label: "Mobile", width: "390px" },
    ];
    const stage = selected
      ? el("div", { class: "preview-stage", "data-viewport": previewViewport },
          // Not a live region: #preview-status beside it already announces this, and
            // two of them read the same sentence twice on every version change.
            el("div", { id: "preview-state", class: "state-panel state-panel--loading" },
            ...loadingContent(`Loading ${inlineVersionLabel(selected)}`, "Preparing a short-lived sandboxed preview…", { level: "h4" })),
          el("iframe", { id: "preview-frame", hidden: true, sandbox: "allow-scripts", referrerpolicy: "no-referrer", title: `Preview of ${inlineVersionLabel(selected)}` }))
      : emptyState("Nothing to preview", "Create or deploy a version to begin review.", null, { level: "h4" });

    const viewportControl = el("div", { class: "segmented preview-viewports", role: "group", "aria-label": "Preview width" });
    for (const option of VIEWPORTS) {
      const button = el("button", {
        id: `preview-viewport-${option.key}`,
        class: "segmented__button",
        type: "button",
        "aria-pressed": previewViewport === option.key ? "true" : "false",
        onclick: () => {
          previewViewport = option.key;
          stage.setAttribute("data-viewport", option.key);
          for (const other of VIEWPORTS) {
            const node = document.getElementById(`preview-viewport-${other.key}`);
            if (node) node.setAttribute("aria-pressed", other.key === option.key ? "true" : "false");
          }
        },
      }, option.label);
      viewportControl.append(button);
    }

    const preview = el("div", { class: "panel preview-workspace" },
      el("div", { class: "preview-toolbar" },
        el("div", { class: "preview-toolbar__title" },
          el("p", { class: "overline" }, "Selected version"),
          el("h3", {}, selected ? versionLabel(selected) : "No version selected")),
        versionActions(selected, page, data)),
      selected
        ? el("div", { class: "preview-frame-bar" },
            viewportControl,
            // The chip used to float over the client's own content in the top-right
            // corner of the render. It belongs to the admin, so it sits in the admin.
            el("span", { id: "preview-status", class: "preview-frame-bar__status", role: "status", "aria-live": "polite" },
              `Loading ${inlineVersionLabel(selected)}`),
            el("a", {
              id: "preview-open-full",
              class: "btn btn-ghost btn-sm",
              href: "#",
              target: "_blank",
              rel: "noopener",
              hidden: true,
            }, "Open full size", el("span", { "aria-hidden": "true" }, "\u2197")))
        : null,
      stage,
      versionMetadata(selected, page));

    return el("section", { class: "section-block", "aria-labelledby": "review-title" },
      el("div", { class: "section-heading" },
        el("p", { class: "overline" }, "Workflow"),
        el("h2", { id: "review-title", tabindex: "-1" }, "Review"),
        el("p", {}, "Pick a version to preview. Only the actions that version can take are shown.")),
      el("div", { class: "review-layout" }, browser, preview));
  }

  function generatePassword(length = 20) {
    const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789";
    const buffer = new Uint32Array(length);
    crypto.getRandomValues(buffer);
    return Array.from(buffer, (value) => alphabet[value % alphabet.length]).join("");
  }

  function clientAccessSection(page) {
    const liveUrl = contentOrigin ? `${contentOrigin.replace(/\/+$/, "")}/${slugPath(page.slug)}` : "";
    const urlInput = el("input", { id: "live-url", type: "url", value: liveUrl, readonly: true });
    const copyUrl = el("button", { class: "btn", type: "button", disabled: !liveUrl }, icon("copy"), "Copy URL");
    copyUrl.addEventListener("click", async () => {
      try { await copyText(liveUrl); toast("Live URL copied"); }
      catch (error) { toast(error.message, { tone: "error" }); }
    });

    const password = el("input", {
      id: "client-password",
      type: "password",
      autocomplete: "new-password",
      placeholder: page.has_password ? "Enter a replacement password" : "Choose a client password",
    });
    const accessStatus = el("p", { class: "form-status", role: "status", "aria-live": "polite" });
    const generate = el("button", { id: "generate-password", class: "btn", type: "button" }, "Generate");
    // Generate used to flip the input to type="text" and never flip it back, so a
    // password field silently stayed readable for the rest of the session. Showing
    // the value is its own decision now, and it is reversible.
    const reveal = el("button", {
      id: "reveal-password",
      class: "btn",
      type: "button",
      "aria-pressed": "false",
      "aria-controls": "client-password",
    }, "Show");
    const copyPassword = el("button", { class: "btn", type: "button" }, icon("copy"), "Copy");
    const save = el("button", { id: "save-password", class: "btn btn-primary", type: "submit" }, page.has_password ? "Update password" : "Set password");
    const clear = page.has_password ? el("button", { id: "clear-password", class: "btn btn-danger btn-sm", type: "button" }, "Clear password") : null;

    function setRevealed(on) {
      password.type = on ? "text" : "password";
      reveal.setAttribute("aria-pressed", on ? "true" : "false");
      reveal.textContent = on ? "Hide" : "Show";
    }
    reveal.addEventListener("click", () => setRevealed(password.type === "password"));

    generate.addEventListener("click", async () => {
      password.value = generatePassword();
      setRevealed(true);
      password.focus();
      password.select();
      try { await copyText(password.value); toast("Generated password copied; set it when ready"); }
      catch { toast("Password generated; copy it and set it when ready"); }
    });
    copyPassword.addEventListener("click", async () => {
      if (!password.value) { accessStatus.textContent = "Generate or enter a password first."; password.focus(); return; }
      try { await copyText(password.value); toast("Password copied"); }
      catch (error) { toast(error.message, { tone: "error" }); }
    });

    const form = el("form", { class: "form-stack" },
      field({
        id: "client-password",
        label: page.has_password ? "New client password" : "Client password",
        control: password,
        help: "Passwords are submitted once and never shown again.",
        wrap: "label",
      }),
      // Clear password leaves the row of things that set one: it is the opposite
      // action, and standing beside the primary it read as a fourth way to save.
      el("div", { class: "password-actions" }, generate, reveal, copyPassword, save),
      accessStatus,
      clear ? el("div", { class: "cluster" }, clear) : null);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!password.value.trim()) { accessStatus.textContent = "Enter a password first."; password.focus(); return; }
      setBusy(save, true, "Saving…");
      const submitted = password.value;
      try {
        await post("/password", { password: password.value });
        password.value = "";
        setRevealed(false);
        // The help text promises this is never shown again, and it is not — which
        // is exactly why it has to be shown ONCE, here, rather than confirmed by a
        // four-second toast and a clipboard write that may have been overwritten.
        credentialDialog({
          title: page.has_password ? "New client password" : "Client password",
          description: "This is the only time Pages will show it. Send it to the client separately from the link.",
          value: submitted,
          notes: [
            `Client link: ${liveUrl}`,
            "Anyone with the link and this password can open the page.",
          ],
        });
        await reload({ parts: ["overview", "access"] });
      } catch (error) {
        accessStatus.textContent = error.message;
        setBusy(save, false);
      }
    });
    clear?.addEventListener("click", async () => {
      const approved = await confirmDialog({
        trigger: clear,
        title: "Clear client access?",
        message: "The page becomes staff-only. Anyone a client has open stops working immediately.",
        confirmLabel: "Clear password",
        danger: true,
      });
      if (!approved) return;
      await mutate(clear, "Password cleared; page is now staff-only", "/password", { password: "" }, undefined, ["overview", "access"]);
    });

    return el("section", { class: "section-block", "aria-labelledby": "access-title" },
      el("div", { class: "section-heading" },
        el("p", { class: "overline" }, "Sharing"),
        el("h2", { id: "access-title", tabindex: "-1" }, "Client access"),
        el("p", {}, page.has_password
          ? "Clients open this address with the password. Staff can always open it with View live."
          : "This page is staff-only. Set a password before sending the address to a client.")),
      el("div", { class: "access-grid" },
        el("div", { class: "subpanel" },
          el("div", { class: "subpanel__head" }, el("h3", {}, "Live URL"), page.published_version_id ? statusChip("published") : statusChip("draft", "Not published")),
          field({
            id: "live-url",
            label: "Client-facing address",
            control: el("div", { class: "copy-field" }, urlInput, copyUrl),
          }),
          page.published_version_id ? el("a", { class: "btn", href: `/view/${slugPath(page.slug)}`, target: "_blank", rel: "noopener" }, "View live", el("span", { "aria-hidden": "true" }, "\u2197")) : null),
        el("div", { class: "subpanel" },
          el("div", { class: "subpanel__head" }, el("h3", {}, "Password"), page.has_password ? statusChip("set") : statusChip("draft", "Staff-only")),
          form)));
  }

  // Where the editor's starting HTML comes from. The detail payload strips
  // `html` from the versions list, so anything but the live version costs one
  // extra read (GET /versions/:id) — which is why the fetch happens on the
  // trigger, before the dialog opens, instead of leaving an empty textarea on
  // screen while it lands.
  async function seedFor(data, version) {
    const published = data.published || null;
    if (!version) return null;
    if (published && sameId(published.id, version.id)) return published;
    const { version: full } = await pageApi(`/versions/${version.id}`);
    return full;
  }

  async function editVersionSource(trigger, data, version) {
    setBusy(trigger, true, "Opening…");
    let seed = null;
    let failure = null;
    try {
      seed = await seedFor(data, version);
    } catch (error) {
      failure = error;
    }
    if (trigger && trigger.isConnected) setBusy(trigger, false);
    if (failure) {
      // setBusy disabled the trigger for the length of the read, which drops
      // focus to <body>, and no dialog opened to take it. Put it back so a
      // keyboard or screen-reader user is not silently moved to the top of the
      // document by a failure.
      if (trigger && trigger.isConnected) trigger.focus();
      toast(`Couldn't open ${inlineVersionLabel(version)}: ${failure.message}`, { tone: "error" });
      return;
    }
    openSourceEditor(trigger, { seed, from: version, published: data.published || null, versions: data.versions || [] });
  }

  // The dialog's own words, derived from what it is starting from. Four cases,
  // because they are four different acts: write the page's very first source,
  // write a fresh one past the versions already in review, amend what is live,
  // or amend some other version in the history.
  function editorCopy(options) {
    const { seed, from, published, versions } = options;
    if (!seed) {
      return {
        title: "Write source",
        description: (versions || []).length
          // Versions exist, they are just none of them live. Saying "first
          // version" here would be false, and would invite someone to retype
          // source that is already sitting in the review queue.
          ? "Nothing is published yet, so this starts from an empty file. "
            + "Saving adds one more version; the versions already in review are left untouched."
          : "This page has no source yet, so this starts from an empty file. "
            + "Saving creates this page's first version from exactly the HTML you write here.",
      };
    }
    // Being "the live version" is a property of the seed, not of which button
    // opened the dialog: amending the selected version when the selected version
    // IS the live one is the same act as amending it from Settings, so it gets
    // the same name either way.
    const live = Boolean(published) && sameId(published.id, seed.id);
    const name = live ? `the live ${inlineVersionLabel(seed.id)}` : inlineVersionLabel(from);
    return {
      title: live ? "Edit live source" : "Edit source",
      description: `Starting from ${name}. Saving creates a new version from this exact HTML — nothing already in history changes.`,
    };
  }

  function openSourceEditor(trigger, options = {}) {
    const seed = options.seed || null;
    const copy = editorCopy(options);
    let clean = true;
    const modal = makeDialog({
      title: copy.title,
      kicker: "New version",
      description: copy.description,
      size: "wide",
      onClose: () => {
        window.removeEventListener("beforeunload", beforeUnload);
        modal.dialog.remove();
      },
    });
    const source = el("textarea", { id: "source-html", class: "source-editor", spellcheck: "false", wrap: "off", placeholder: "<!doctype html>\n<html>…</html>" });
    source.value = seed?.html || "";
    const mode = el("select", { id: "source-mode" },
      el("option", { value: "themed" }, "Themed"),
      el("option", { value: "raw" }, "Raw"));
    mode.value = seed?.render_mode || "themed";
    const note = el("input", { id: "source-note", type: "text", maxlength: "500", placeholder: "What changed?" });
    const status = el("p", { class: "form-status", role: "status", "aria-live": "polite" });
    // One save. "Save draft" and "Save & preview" called the same function with
    // no mode argument, and render() previews the selected version either way,
    // so the pair only ever asked the reader to choose between two identical
    // outcomes.
    const save = el("button", { class: "btn btn-primary", type: "button" }, "Save as new version");
    const cancel = el("button", { class: "btn btn-ghost", type: "button", onclick: () => modal.requestClose("cancel") }, "Cancel");

    const snapshot = () => JSON.stringify([source.value, mode.value, note.value]);
    let baseline = snapshot();
    const isDirty = () => snapshot() !== baseline;
    const markDirty = () => { clean = !isDirty(); };
    [source, mode, note].forEach((control) => control.addEventListener("input", markDirty));
    function beforeUnload(event) {
      if (!clean && isDirty()) { event.preventDefault(); event.returnValue = ""; }
    }
    window.addEventListener("beforeunload", beforeUnload);
    modal.setBeforeClose(async () => {
      if (clean || !isDirty()) return true;
      return confirmDialog({
        trigger: modal.closeButton,
        title: "Discard unsaved source changes?",
        message: "Your HTML, render mode, and note changes have not been saved.",
        confirmLabel: "Discard changes",
        danger: true,
      });
    });

    async function saveSource(button) {
      if (!source.value.trim()) { status.textContent = "HTML source cannot be empty."; source.focus(); return; }
      setBusy(button, true, "Saving…");
      status.textContent = "Saving a new version…";
      try {
        const result = await post("/deploy-source", {
          html: source.value,
          render_mode: mode.value,
          note: note.value.trim() || "Inline edit",
        });
        baseline = snapshot();
        clean = true;
        modal.setBeforeClose(null);
        modal.close("saved");
        selectedVersionId = result.version.id;
        reviewFilter = result.version.status === "pending" ? "pending" : "all";
        const savedNumber = result.deduped
          ? versionNumber(result.version)
          : (pageData?.versions?.length || 0) + 1;
        const savedLabel = savedNumber ? `version ${savedNumber}` : "version";
        toast(result.deduped ? `No source changes; selected ${savedLabel}` : `Saved ${savedLabel}`);
        await load(result.version.id);
      } catch (error) {
        status.textContent = error.message;
        setBusy(button, false);
      }
    }
    save.addEventListener("click", () => saveSource(save));

    modal.body.append(el("form", { class: "form-stack" },
      field({ id: "source-html", label: "HTML source", control: source, wrap: "label" }),
      // The note is the audit trail — it is what a reviewer reads in the version
      // queue months later — so it leads the meta row and takes its wide column,
      // where main gave that column to a two-option select. It does NOT get a row
      // of its own: that pushed Render mode past the dialog's scroll fold at
      // 1440x900 (and left only its clipped label at 820x1000), with no scrollbar
      // to hint that anything was down there.
      el("div", { class: "editor-meta" },
        field({
          id: "source-note",
          label: "Version note",
          control: note,
          help: "Kept in this page's history. Left blank, it is recorded as “Inline edit”.",
          wrap: "label",
        }),
        field({
          id: "source-mode",
          label: "Render mode",
          control: mode,
          help: "Themed adds the Elcano look; Raw serves your HTML unchanged.",
          wrap: "label",
        })),
      status));
    modal.actions.append(cancel, save);
    modal.open(trigger);
    source.focus();
  }

  // ── Settings: one save model for the whole panel ─────────────────────────────
  //
  // #156: five rows used to commit four different ways — a "Save title" button, an
  // "Apply" button, a checkbox that saved silently on change, a "Disable page"
  // button that acted at once, and an "Edit source" button that saved nothing at
  // all — every one of them dressed as the same kind of row. A reader could not
  // tell which of their changes were already saved.
  //
  // Now every editable row commits itself as soon as its own control is done
  // changing — on change for a switch or a select, on Enter or on leaving the
  // field for the text input — and says so in that row: "Saving…", then "Saved"
  // with the clock time it landed, in a polite live region. A failure replaces
  // that with "Not saved" and the server's own words in a role="alert" beside the
  // control — an error is an alert, not a line of muted help text, and it says it
  // in words because the two report lines share one slot at one type size.
  // Availability is a switch with exactly that treatment (its takedown keeps the
  // #148 confirmation), and Source, which commits nothing, is rendered as the
  // navigation it is instead of a peer of the save buttons that no longer exist.
  //
  // Three rules keep the reporting honest:
  //
  //   * A settings commit does not re-render the Settings section. Replacing a live
  //     region with a fresh copy is not an announcement, so "Saved" would be
  //     silent to a screen reader. Each control already displays the value it just
  //     sent, so only the sections showing something else — the header state, the
  //     review workspace — are refreshed. The single exception is a commit that
  //     something else rebuilt this panel underneath: commitSetting rebuilds it
  //     once more at the end, because the control on the screen is then showing a
  //     value the server read before the commit landed.
  //   * The row that is MOUNTED reports the outcome. Unrelated actions DO rebuild
  //     this section (publishing a version does, setting a password does), so the
  //     outcome is kept in settingSaves and every paint goes through settingRows,
  //     the registry of the nodes currently on the screen. A commit that resolves
  //     after such a rebuild used to write "Saved" into the detached nodes it
  //     started with and leave the new row saying "Saving…" for good.
  //   * A report never resizes the panel for a one-line message: both slots stay
  //     in the DOM, collapse when empty, and share one reserved line. A commit
  //     fired by BLUR paints while the click that caused the blur is still in
  //     flight, and a slot that grew or shrank there would move the row under the
  //     pointer and swallow that click.
  const settingSaves = new Map();
  // The commits in flight, by row, kept apart from settingSaves because a control
  // stays shut past the POST that it sent — through the refetch after it (#146) —
  // so "busy" outlives "saving".
  const settingBusy = new Set();
  // The mounted row per setting: whatever settingsSection built last.
  const settingRows = new Map();

  function paintSetting(key) {
    const row = settingRows.get(key);
    if (row) row.paint();
  }

  // One row's report, and the record of that row in settingRows. The two slots are
  // separate nodes on purpose: a failure has to interrupt and progress must not,
  // and swapping role on a single node does not reliably re-announce. Both are in
  // the DOM for the life of the row — `hidden` would keep the alert out of the
  // accessibility tree until the moment it matters, and inserting a live region
  // that already carries its text is not an announcement either — so a state is
  // reported by writing into a node that is already there, and their shared slot
  // reserves its line whether or not anything is in it.
  function settingFeedback(key) {
    const live = el("span", { id: `setting-${key}-status`, class: "form-status", role: "status", "aria-live": "polite" });
    const alert = el("p", { id: `setting-${key}-error`, class: "field-error", role: "alert" });
    const slot = el("div", { class: "setting-row__feedback" }, alert, live);
    // controls and node are filled in by the caller and by settingRow: paint()
    // reads them so the busy state lands on the row that is mounted, whichever
    // build of the panel that is.
    const row = { key, slot, describedBy: alert.id, controls: [], node: null, paint };
    function paint() {
      const saved = settingSaves.get(key) || null;
      const failed = Boolean(saved && saved.kind === "error");
      if (failed) {
        live.replaceChildren();
        // "Not saved" in words. In the dark theme the alert and the progress line
        // are two pale tints of one lightness in the same slot at the same size:
        // colour cannot be the only thing that says which of the two you got.
        alert.replaceChildren(`Not saved — ${saved.message}`);
      } else {
        alert.replaceChildren();
        if (!saved) live.replaceChildren();
        else if (saved.kind === "saving") live.replaceChildren("Saving…");
        // The clock, not "just now": nothing re-renders this line, so a relative
        // time would still claim the save had just happened an hour later. The
        // <time> it renders carries the whole moment for anything reading markup.
        else live.replaceChildren("Saved · ", timeWhen(saved.at, { style: "time" }));
      }
      const busy = settingBusy.has(key);
      row.controls.forEach((control) => {
        control.disabled = busy;
        if (failed) control.setAttribute("aria-invalid", "true");
        else control.removeAttribute("aria-invalid");
      });
      if (!row.node) return;
      if (busy) row.node.setAttribute("aria-busy", "true");
      else row.node.removeAttribute("aria-busy");
    }
    settingRows.set(key, row);
    return row;
  }

  // commitSetting — the one commit path. Confirm if a client will see the change,
  // report progress, POST, then record either the moment it saved or the server's
  // own words. The control stays disabled across the refetch that follows for the
  // same reason mutate() keeps its button busy (#146): re-enabling it the instant
  // the POST resolves is a second POST waiting to be sent.
  async function commitSetting(options) {
    const { key, path, body, parts, confirm, revert } = options;
    if (confirm) {
      const approved = await confirmClientVisible(confirm);
      if (!approved) {
        if (revert) revert();
        return false;
      }
    }
    // Where focus is NOW, because painting the busy state below disables the
    // control and blurs it. Only a control the operator is actually standing on is
    // restored: a commit triggered by blur must never drag focus back out of
    // wherever they moved to.
    const startedIn = settingRows.get(key) || null;
    const refocus = (startedIn && startedIn.controls.find((node) => node === document.activeElement)) || null;
    settingSaves.set(key, { kind: "saving" });
    settingBusy.add(key);
    // Every paint goes through the registry rather than through the nodes this
    // call started with: by the time the POST resolves, the panel may have been
    // rebuilt for an unrelated reason and the reader is looking at new nodes.
    paintSetting(key);
    let ok = false;
    try {
      await post(path, body);
      ok = true;
      settingSaves.set(key, { kind: "saved", at: Date.now() });
    } catch (error) {
      settingSaves.set(key, { kind: "error", message: error.message });
      if (revert) revert();
    }
    paintSetting(key);
    try {
      // No focusFrom: the control is disabled for the length of the refresh, so
      // nominating it would only send keepingFocus to a fallback somewhere else on
      // the screen. This function puts focus back itself, below.
      if (ok) await reload({ parts });
    } catch (error) {
      // The save landed and only the refetch after it did not. Say that, rather
      // than letting the row claim the change failed.
      toast(`Saved, but this screen could not refresh: ${error.message}`, { tone: "error" });
    } finally {
      settingBusy.delete(key);
      // If this section was rebuilt while the POST was open, the control on the
      // screen was rendered from a read the server answered BEFORE the commit
      // landed: it is showing the old value under this row's "Saved". This is the
      // one case where a settings commit does re-render Settings, and here it is
      // safe — the gesture is long over, so there is no pending click to swallow,
      // and the outcome was already announced in the nodes that were mounted when
      // it arrived. paintSetting then reports it again in the new ones.
      if (ok && startedIn && settingRows.get(key) !== startedIn) refresh(["settings"]);
      paintSetting(key);
      if (refocus && refocus.isConnected) refocus.focus();
    }
    return ok;
  }

  function settingCopy(name, description, forId) {
    return el("div", { class: "setting-row__copy" },
      forId ? el("label", { for: forId }, el("strong", {}, name)) : el("strong", {}, name),
      el("span", { class: "muted" }, description));
  }

  // One row shape for the whole panel: what the setting is, then the control with
  // the state of its last commit stacked underneath it, in one control column that
  // every row shares — a control's own box ends where the control does, so nothing
  // outside it is part of its hit area. A row with no feedback (Source) is a row
  // that saves nothing.
  function settingRow(key, copy, control, feedback) {
    const node = el("div", { class: "setting-row", dataset: { setting: key } },
      copy,
      el("div", { class: "setting-row__control" }, control, ...(feedback ? [feedback.slot] : [])));
    if (feedback) {
      feedback.node = node;
      // The first paint of a freshly built row: it may be reporting a commit that
      // is still in flight, or one whose outcome the reader has not dealt with.
      feedback.paint();
    }
    return node;
  }

  function settingsSection(data) {
    const { page, themes } = data;
    // A client-side refusal belongs to the value that was typed, not to the row.
    // This section is being rebuilt from what the server holds, so an "Enter a
    // title." left standing over the server's own perfectly good title would
    // accuse a field that has nothing wrong with it — and it is wired to that
    // field through aria-describedby, so a screen reader would read the accusation
    // out on every visit. A server refusal is not dropped: nothing has dealt with
    // that yet, which is the whole reason these outcomes outlive a rebuild.
    settingSaves.forEach((state, key) => { if (state && state.client) settingSaves.delete(key); });

    const titleFeedback = settingFeedback("title");
    const title = el("input", {
      id: "page-title",
      type: "text",
      value: page.title || page.slug,
      required: true,
      "aria-describedby": titleFeedback.describedBy,
    });
    titleFeedback.controls = [title];
    // What the server holds, so leaving the field untouched — or tabbing straight
    // through it — sends nothing at all.
    let titleCommitted = page.title || page.slug;
    // And what it was last asked to hold, which is the value it refused when this
    // row is sitting on an error. A refused value differs from the committed one by
    // definition, so without this every blur looks like an unsaved change.
    let titleAttempted = null;
    // `retry` is the explicit gesture: Enter. A refused row may be sent again with
    // the value the server already rejected, but only when the operator asks for
    // it — retrying on every blur turned moving focus through an errored field
    // into a POST per pass, at a mutating endpoint, with nothing typed.
    async function commitTitle({ retry = false } = {}) {
      // The commit in flight is the one that counts. Disabling the field blurs it,
      // so a single Enter fires both gestures; without this the row would send the
      // same title twice.
      if (settingBusy.has("title")) return;
      const state = settingSaves.get("title") || null;
      const value = title.value.trim();
      if (!value) {
        // Not a toast and not muted help: the row itself says what is wrong, and
        // the typed value stays put so it can be corrected. One short line, so
        // saying it cannot reflow the panel out from under a pending click.
        settingSaves.set("title", { kind: "error", client: true, message: "Enter a title." });
        paintSetting("title");
        return;
      }
      title.value = value;
      const refused = Boolean(state && state.kind === "error");
      // Already sent: the server either holds this value or has just refused it.
      // Re-sending is then a retry, and only the explicit gesture retries — on blur
      // it turned moving focus through a refused field into one POST per pass, at a
      // mutating endpoint, with nothing typed. An edited value is not a retry and
      // commits on blur as it always did.
      const alreadySent = value === titleCommitted || (refused && value === titleAttempted);
      if (alreadySent && !(retry && refused)) return;
      titleAttempted = value;
      const ok = await commitSetting({
        key: "title",
        path: "/title",
        body: { title: value },
        // The header, the tab, and the breadcrumb are the only other places the
        // title shows; the review workspace does not care.
        parts: ["overview"],
      });
      if (ok) titleCommitted = value;
    }
    // Leaving the field commits it, and so does Enter — including Enter on a value
    // the server just refused, which fires no `change` event and would otherwise
    // leave the reader retyping their own text to get a retry.
    title.addEventListener("blur", () => commitTitle());
    title.addEventListener("keydown", (event) => { if (event.key === "Enter") commitTitle({ retry: true }); });

    const themeFeedback = settingFeedback("theme");
    // No aria-label: settingCopy renders a real <label for="page-theme">Theme</label>
    // now, and an aria-label would win the accessible name and leave the control
    // called something its visible label does not say.
    const theme = el("select", { id: "page-theme", "aria-describedby": themeFeedback.describedBy });
    themes.forEach((item) => theme.append(el("option", {
      value: item.name,
      selected: sameId(page.theme_id, item.id) || (!page.theme_id && item.name === "flag"),
    }, item.name)));
    themeFeedback.controls = [theme];
    // What the server last accepted, so a failed change reverts to the theme this
    // page is actually rendering with rather than to whatever it started as.
    let themeCommitted = theme.value;
    theme.addEventListener("change", async () => {
      const chosen = theme.value;
      const ok = await commitSetting({
        key: "theme",
        path: "/theme",
        body: { theme: chosen },
        // A theme is applied at render time, so the sandboxed preview beside it is
        // now showing the old one.
        parts: ["review"],
        revert: () => { theme.value = themeCommitted; },
      });
      if (ok) themeCommitted = chosen;
    });

    const approvalFeedback = settingFeedback("approval");
    const approval = el("input", {
      id: "approval-gate",
      type: "checkbox",
      checked: page.require_approval,
      "aria-describedby": approvalFeedback.describedBy,
    });
    approvalFeedback.controls = [approval];
    approval.addEventListener("change", () => {
      const next = approval.checked;
      commitSetting({
        key: "approval",
        path: "/approval",
        body: { require_approval: next },
        // The gate decides whether approving a version also publishes it, which is
        // what the review workspace's own buttons say out loud.
        parts: ["overview", "review"],
        // #170 left this checkbox as the only record of the gate, so it must never
        // sit showing a state the server rejected.
        revert: () => { approval.checked = !next; },
      });
    });

    // #156 asked for Availability either next to the Danger zone or as a switch
    // with the approval gate's treatment. A switch is the one that keeps the
    // promise of this panel — one commit model, one status line per row — and it
    // makes the current state readable at a glance instead of inferable from a
    // button that names the opposite of what is true.
    const availabilityFeedback = settingFeedback("availability");
    const availability = el("input", {
      id: "page-availability",
      type: "checkbox",
      checked: !page.disabled,
      "aria-describedby": availabilityFeedback.describedBy,
    });
    availabilityFeedback.controls = [availability];
    availability.addEventListener("change", () => {
      const enabling = availability.checked;
      commitSetting({
        key: "availability",
        path: enabling ? "/enable" : "/disable",
        parts: ["overview"],
        // #148 unchanged: a takedown is the most client-visible act on this
        // screen, so it is confirmed. Putting the page back is not.
        confirm: enabling ? null : {
          trigger: availability,
          title: `Take /${page.slug} down?`,
          message: "Anyone with the client link gets an error page until it is enabled again. Nothing is deleted, and the live version is unchanged.",
          confirmLabel: "Disable page",
          danger: true,
        },
        revert: () => { availability.checked = !enabling; },
      });
    });

    // The Review workspace owns "edit the version I am looking at". This entry
    // stays for the two things it is the only route to: amending what is live
    // regardless of what is selected, and writing the first source on a page that
    // has never published anything. It saves nothing by itself, so it is dressed
    // as the navigation it is — quiet, with the arrow that says "this opens
    // something" — and not as a peer of a commit control.
    const published = data.published || null;
    const versions = data.versions || [];
    const edit = el("button", {
      id: "edit-source",
      class: "btn btn-ghost",
      type: "button",
      onclick: (event) => openSourceEditor(event.currentTarget, { seed: published, from: published, published, versions }),
    }, icon("file"), published ? "Edit live source" : "Write source", el("span", { "aria-hidden": "true" }, "→"));

    const configuration = el("div", { class: "panel setting-list" },
      settingRow("title",
        settingCopy("Page title", "Shown in Pages admin; the public slug does not change.", "page-title"),
        title, titleFeedback),
      settingRow("theme",
        settingCopy("Theme", "Applied at render time without changing stored source.", "page-theme"),
        theme, themeFeedback),
      settingRow("approval",
        settingCopy("Approval gate", "New agent versions wait for human approval when enabled."),
        el("label", { class: "toggle-label", for: "approval-gate" }, approval, el("span", {}, "Require approval")),
        approvalFeedback),
      settingRow("availability",
        // The switch carries the state, so this line does not have to — and must
        // not: main's description named it ("This page is currently taken down.")
        // and so had to be re-rendered to stay honest, which is exactly what a
        // commit here must not do. Wording that holds in both states is what lets
        // a takedown skip rebuilding this section, and lets its own "Saved" survive
        // long enough to be announced.
        settingCopy("Availability", "On serves the published version; off returns an error page to anyone with the client link."),
        el("label", { class: "toggle-label", for: "page-availability" }, availability, el("span", {}, "Page enabled")),
        availabilityFeedback),
      settingRow("source",
        settingCopy("Source", published
          ? `Edit the HTML behind the live ${inlineVersionLabel(published.id)} and save it as a new version.`
          // A page can have versions and still have nothing live — the normal
          // state of an approval-gated page whose first versions are all still
          // pending. Calling that "no source" would hide them.
          : versions.length
            ? "Nothing is published yet. Write a new version by hand, or edit one from the review queue above."
            : "This page has no source yet. Write its first HTML by hand."),
        edit));

    const remove = el("button", { class: "btn btn-danger", type: "button" }, icon("trash"), "Delete page");
    remove.addEventListener("click", async () => {
      // This button carries an icon, so it is the one that proved setBusy's
      // textContent restore lossy: a failed delete used to leave "Delete page"
      // as bare text with no trash glyph.
      const { ok } = await runAction({
        button: remove,
        confirm: {
          title: `Delete /${page.slug}?`,
          message: "The page will stop serving and leave the index. Its history remains recoverable by an administrator, and the slug becomes available for reuse.",
          confirmLabel: "Delete page",
          danger: true,
        },
        busyLabel: "Deleting…",
        // Navigation is not instant, and the page is already gone: the button
        // must not offer a second delete while the browser is leaving.
        keepBusy: true,
        failure: "Delete failed",
        run: () => post("/delete", {}),
      });
      if (ok) window.location.assign("/admin");
    });
    const danger = el("section", { class: "danger-zone", "aria-labelledby": "danger-zone-title" },
      el("div", { class: "danger-zone__copy" },
        el("p", { class: "overline" }, "Destructive action"),
        el("h3", { id: "danger-zone-title" }, "Danger zone"),
        el("p", {}, "Deleting stops this page from serving and removes it from the active index. Its version history remains recoverable.")),
      el("div", { class: "danger-zone__action" }, remove));

    return el("section", { class: "section-block", "aria-labelledby": "settings-title" },
      el("div", { class: "section-heading" },
        el("p", { class: "overline" }, "Configuration"),
        el("h2", { id: "settings-title", tabindex: "-1" }, "Settings"),
        // Precise about the two rows that do not save on change: the title waits
        // for Enter or for you to leave it, and Source is a way into the editor.
        el("p", {}, "Each setting saves itself and reports the result beside its own control: the title when you press Enter or leave the field, the rest the moment you change them. Taking the page down asks first; Source opens the editor.")),
      configuration,
      danger);
  }

  // The server renders the trail from the slug alone, because that is all it has
  // at shell time. Once the payload lands we know the human title and the
  // workspace, so the tab and the last crumb say what a person would call this
  // page instead of repeating the URL.
  function updateLocation(page) {
    const label = page.title || `/${page.slug}`;
    document.title = `${label} · Pages`;
    const current = document.getElementById("breadcrumb-current");
    if (current) {
      current.textContent = label;
      current.title = `/${page.slug}`;
    }
    const workspace = document.getElementById("breadcrumb-workspace");
    if (workspace) {
      workspace.replaceChildren(page.workspace_name || "Ungrouped");
      workspace.hidden = false;
    }
  }

  // The screen is four independent sections and it used to rebuild all four for
  // anything at all — including merely clicking a version in the queue. That threw
  // away every unsaved value on the screen (type a replacement password, glance at
  // the queue, click a version: the password is gone) and re-minted a signed
  // preview token for a version that had not changed, flashing the 40rem stage and
  // shifting everything below it.
  //
  // So: keep a handle on each rendered section and replace only the ones whose
  // input actually changed. The builders stay pure — this is only about which of
  // them run.
  const SECTIONS = {
    overview: (data) => pageOverview(data.page, data.versions.filter((v) => v.status === "pending").length),
    review: (data) => reviewSection(data, currentSelection(data)),
    access: (data) => clientAccessSection(data.page),
    settings: (data) => settingsSection(data),
  };
  const ORDER = ["overview", "review", "access", "settings"];
  const mounted = {};

  // The version the preview iframe is actually showing, so re-rendering for an
  // unrelated reason costs no token and no reload.
  let previewedVersionId = null;

  function currentSelection(data) {
    return data.versions.find((version) => sameId(version.id, selectedVersionId)) || null;
  }

  // -- in-page section navigation (#172) --------------------------------------
  // The detail is one long scroll - ~4,250px at 390px, ~2,600px at 1440px - and
  // the only chrome that survived it was the app header, which carries no in-page
  // navigation. Reaching "Clear password" meant dragging a 40rem preview stage
  // past, every time. This is a row of anchor links that pins under the header
  // for the whole scroll, marks the section you are actually in, and lands focus
  // on the heading so a keyboard reader arrives where the link promised.
  //
  // It lives OUTSIDE the four sections on purpose: refresh() replaces whole
  // sections in place, so a bar rendered inside one would be destroyed by an
  // unrelated password save - and it would move, which is the one thing a fixed
  // point of reference may not do.
  const NAV_SECTIONS = [
    { part: "review", target: "review-title", label: "Review" },
    { part: "access", target: "access-title", label: "Client access" },
    { part: "settings", target: "settings-title", label: "Settings" },
  ];
  let sectionNav = null;
  const navLinks = new Map();
  // The section the reader last asked for by name. Only consulted where scroll
  // position cannot answer - see activeSectionPart.
  let requestedPart = null;

  function focusSection(target) {
    const heading = document.getElementById(target);
    if (!heading) return;
    // The browser has already done the scrolling (scroll-margin-block-start in
    // shell.css clears both sticky bars); this only moves the reading position,
    // so preventScroll stops it being done twice.
    heading.focus({ preventScroll: true });
  }

  function buildSectionNav() {
    const list = el("ul", { class: "detail-nav__list" });
    for (const entry of NAV_SECTIONS) {
      const link = el("a", {
        class: "detail-nav__link",
        href: `#${entry.target}`,
        "aria-current": "false",
        // The default jump is left intact - it updates the URL, works with Back,
        // and honours scroll-margin. All this adds is the focus move, after the
        // jump has happened.
        onclick: () => {
          requestedPart = entry.part;
          window.requestAnimationFrame(() => { focusSection(entry.target); updateSectionNav(); });
        },
      }, entry.label);
      navLinks.set(entry.part, link);
      list.append(el("li", {}, link));
    }
    // "On this page", not "Page sections": the product-level nav in
    // lib/shell.js is already labelled "Pages sections", and two navigation
    // landmarks a letter apart are indistinguishable when spoken.
    return el("nav", { class: "detail-nav", id: "detail-nav", "aria-label": "On this page" }, list);
  }

  // Which section the reader is in: the last one they have scrolled to or past.
  // "Past" is measured against the position the link itself would scroll to -
  // the heading's own scroll-margin - read back off the element rather than
  // re-derived here, so the bar and the stylesheet cannot disagree about where a
  // section starts at any breakpoint.
  //
  // null is a real answer. Above the first heading the reader is in the overview
  // panel, which has no link here; marking Review then would have aria-current
  // announce a section they have not reached and cannot get back to from the bar.
  function activeSectionPart() {
    const room = document.documentElement.scrollHeight - window.innerHeight;
    // Stranded: a section whose anchor line is further down than the document can
    // ever scroll. The last one usually is, being shorter than the viewport; on a
    // short page or in a tall window several are. Scrolling can never mark these.
    let active = null;
    const stranded = [];
    for (const entry of NAV_SECTIONS) {
      const heading = document.getElementById(entry.target);
      if (!heading) continue;
      const top = heading.getBoundingClientRect().top;
      const anchored = parseFloat(window.getComputedStyle(heading).scrollMarginBlockStart) || 0;
      if (top - anchored <= 1) active = entry.part;
      else if (window.scrollY + top - anchored > room) stranded.push(entry.part);
    }
    // Only at the end of a document that actually scrolls. Without the second
    // half of that, a window taller than the whole detail - a portrait monitor, a
    // zoomed-out tab - is permanently "at the bottom", and a reader who had not
    // scrolled at all was told they were in Settings.
    const atEnd = room > 2 && window.scrollY >= room - 2;
    // A click is an answer even when the section is not stranded. On a short
    // enough page, scrolling to Client access also puts Settings past its own
    // anchor line, so the plain "last one scrolled past" rule overrules the link
    // the reader just pressed. Honour the request while its heading is still on
    // screen; a scroll that takes it away clears it, which is the reader saying
    // something else.
    if (requestedPart) {
      const wanted = NAV_SECTIONS.find((entry) => entry.part === requestedPart);
      const heading = wanted && document.getElementById(wanted.target);
      if (heading) {
        const box = heading.getBoundingClientRect();
        if (box.bottom > 0 && box.top < window.innerHeight) return requestedPart;
      }
    }
    if (!atEnd) requestedPart = null;
    else if (stranded.length) {
      // Here the scroll position is the same for every stranded section, so it
      // cannot say which one the reader means. If they clicked one of them, that
      // is the answer; if they scrolled down here themselves, it is the deepest.
      active = stranded.includes(requestedPart) ? requestedPart : stranded[stranded.length - 1];
    }
    return active;
  }

  function updateSectionNav() {
    if (!sectionNav || sectionNav.hidden) return;
    const active = activeSectionPart();
    for (const [part, link] of navLinks) link.setAttribute("aria-current", part === active ? "true" : "false");
  }

  let navFrame = 0;
  function scheduleSectionNav() {
    if (navFrame) return;
    navFrame = window.requestAnimationFrame(() => { navFrame = 0; updateSectionNav(); });
  }

  function mountSectionNav() {
    if (sectionNav) { sectionNav.hidden = false; return; }
    sectionNav = buildSectionNav();
    // position: sticky resolves against the nearest block container, and a grid
    // item's containing block is its own grid area - both <main> and #app are
    // grids, so a bar parented to either would have no room to travel. One block
    // wrapper around the bar and the sections gives it the whole scroll.
    const body = el("div", { class: "detail-body" });
    app.before(body);
    body.append(sectionNav, app);
    window.addEventListener("scroll", scheduleSectionNav, { passive: true });
    window.addEventListener("resize", scheduleSectionNav);
  }

  function render(data, preferredVersionId) {
    pageData = data;
    updateLocation(data.page);
    const selected = normalizeSelection(data, preferredVersionId);
    keepingFocus(() => {
      for (const part of ORDER) mounted[part] = SECTIONS[part](data);
      app.replaceChildren(...ORDER.map((part) => mounted[part]));
    }, { fallback: ['.version-option[aria-current="true"]', "#app h1"] });
    mountSectionNav();
    updateSectionNav();
    previewedVersionId = null;
    if (selected) syncPreview(selected.id);
  }

  // Re-render the named sections in place. Anything not named keeps its DOM, and
  // therefore keeps whatever the reader has typed into it.
  function refresh(parts, focusFrom) {
    if (!pageData) return;
    // If the control that was focused is gone — approving the last pending version
    // removes the row it was on — land on the newly selected version, then on the
    // queue itself, rather than on <body>.
    keepingFocus(() => {
      for (const part of parts) {
        const next = SECTIONS[part](pageData);
        if (mounted[part] && mounted[part].isConnected) mounted[part].replaceWith(next);
        mounted[part] = next;
      }
    }, { from: focusFrom, fallback: ['.version-option[aria-current="true"]', ".version-list", "#app h1"] });
    // The replaced sections are new nodes and may be a different height (a
    // cleared password drops a button); re-read which one the reader is in.
    updateSectionNav();
  }

  function syncPreview(versionId) {
    if (versionId == null) return;
    // The stage was rebuilt with the section, so a cached id no longer matches
    // what is on screen; loadPreview is the only thing that can put it back.
    if (sameId(versionId, previewedVersionId) && document.getElementById("preview-frame")?.src) return;
    previewedVersionId = versionId;
    loadPreview(versionId);
  }

  // Choosing a version changes the review section and nothing else.
  function selectVersion(versionId) {
    if (sameId(versionId, selectedVersionId)) return;
    selectedVersionId = versionId;
    refresh(["review"]);
    syncPreview(selectedVersionId);
  }

  async function load(preferredVersionId) {
    try {
      const data = await pageApi("");
      render(data, preferredVersionId);
    } catch (error) {
      // Nothing the bar points at is on screen any more, and a link that scrolls
      // nowhere is worse than no link.
      if (sectionNav) sectionNav.hidden = true;
      app.replaceChildren(loadFailed("this page", error, () => load()));
    }
  }

  loadPageSwitcher();
  load();
})();
