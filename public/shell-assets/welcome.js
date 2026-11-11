// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Pure workspace filtering stays CommonJS-compatible for unit coverage; the
// browser half below renders the same model as a responsive operations table.
function filterWorkspacePages(pages, view, query = "") {
  let rows = Array.isArray(pages) ? pages : [];
  if (view === "ungrouped") {
    rows = rows.filter((page) => page.workspace_id === null || page.workspace_id === undefined);
  } else if (typeof view === "string" && view.startsWith("workspace:")) {
    const id = view.slice("workspace:".length);
    rows = rows.filter((page) => String(page.workspace_id || "") === id);
  }
  const needle = String(query || "").trim().toLowerCase();
  if (!needle) return rows;
  return rows.filter((page) =>
    [page.title, page.slug, page.workspace_name].some((value) => String(value || "").toLowerCase().includes(needle))
  );
}

if (typeof module !== "undefined" && module.exports) module.exports = { filterWorkspacePages };

if (typeof document !== "undefined") (function () {
  const I18N = typeof window !== "undefined" && window.PagesI18n ? window.PagesI18n : require("./i18n.js");
  const t = I18N.t;
  const UI = window.PagesUI;
  const { el, icon, toast, makeDialog, confirmDialog, setBusy, field, runAction, formatWhen, timeWhen, slugPath, errorState, emptyState, keepingFocus, statusDot, statusChip, pageHeader, statTile, loadFailed } = UI;
  const Health = window.PagesRefreshHealth;
  const boot = UI.bootstrap();
  const api = UI.requestScope("/api/v1/admin");
  const app = document.getElementById("app");
  let currentData = { pages: [], workspaces: [] };
  let currentQuery = "";
  // How many rows the operations list is currently showing. A fleet is not 37
  // pages for long, and rendering all of them made the index 3,900px tall at
  // 1440px and 13,400px on a phone, with no way to say "just the recent ones".
  // Module scope, not local to pageIndex(), because every mutation re-renders the
  // whole screen and resetting there would throw the reader back to the top.
  const PAGE_WINDOW = 25;
  let shown = PAGE_WINDOW;

  function viewFromURL() {
    const value = new URL(window.location.href).searchParams.get("workspace");
    if (value === "ungrouped") return "ungrouped";
    return /^\d+$/.test(value || "") ? `workspace:${value}` : "all";
  }
  let currentView = viewFromURL();
  // "Which dashboards did not refresh?" used to mean opening every one of them.
  // ?attention=1 is the list of the ones that need a human, so a digest or a
  // colleague can link straight to it. ?unscheduled=1 is the gentler list beside
  // it: live pages with week-old data that nobody gave a refresh schedule. One
  // filter at a time — the two lists never share a page.
  const INDEX_FILTERS = ["attention", "unscheduled"];
  const filterFromURL = () => {
    const params = new URL(window.location.href).searchParams;
    return INDEX_FILTERS.find((name) => params.get(name) === "1") || null;
  };
  let indexFilter = filterFromURL();

  function setFilter(name) {
    indexFilter = name;
    shown = PAGE_WINDOW;
    const url = new URL(window.location.href);
    for (const other of INDEX_FILTERS) url.searchParams.delete(other);
    if (name) url.searchParams.set(name, "1");
    window.history.replaceState({}, "", url);
  }

  function setView(view, options = {}) {
    currentView = view;
    shown = PAGE_WINDOW;
    const url = new URL(window.location.href);
    if (view === "all") url.searchParams.delete("workspace");
    else if (view === "ungrouped") url.searchParams.set("workspace", "ungrouped");
    else url.searchParams.set("workspace", view.slice("workspace:".length));
    window.history[options.replace ? "replaceState" : "pushState"]({}, "", url);
  }

  function workspaceIdForView() {
    return currentView.startsWith("workspace:") ? currentView.slice("workspace:".length) : "";
  }

  const sleep = (milliseconds) => new Promise((resolve) => window.setTimeout(resolve, milliseconds));
  // "Not yet" is this screen's fallback wording and stays this screen's, passed
  // per call: #151 owns unifying the four wordings across the admin UI.

  // The same four states the detail names, in the same words, from the same map.
  function statusOf(page) {
    if (page.disabled) return "disabled";
    if (page.published_version_id) return page.has_password || page.require_approval ? "gated" : "live";
    if (page.require_approval) return "pending";
    return "draft";
  }

  function statusNode(page) {
    const kind = statusOf(page);
    return statusDot(kind, kind === "pending" ? t("Approval gated") : null);
  }

  function workspaceOptions(select, workspaces) {
    select.append(el("option", { value: "" }, t("Ungrouped")));
    for (const workspace of workspaces) {
      select.append(el("option", { value: String(workspace.id) }, workspace.name));
    }
  }

  function operationRow(page, workspaces) {
    // Reassignment is rare; a live <select> on every row is not. Thirty-seven of
    // them made the list look like a form, and a stray scroll wheel over a focused
    // one was a data change. The row now states the workspace and opens a dialog
    // to change it, so the reassignment is deliberate and the row is readable.
    const assignment = el("button", {
      id: `workspace-of-${page.id}`,
      class: "btn btn-ghost btn-sm workspace-assignment__button",
      type: "button",
      "aria-label": t("Workspace for {0}: {1}. Change", page.title || page.slug, page.workspace_name || t("Ungrouped")),
      title: t("Move to another workspace"),
      onclick: (event) => openMoveDialog(event.currentTarget, page, workspaces),
    }, page.workspace_name || t("Ungrouped"));

    const days = (n) => (n === 0 ? t("today") : n === 1 ? t("1d ago") : t("{0}d ago", n));

    // A dashboard frozen for six weeks looked exactly like one refreshed this
    // morning, in this very table: "Last update" is the page ROW's updated_at,
    // which moves when someone renames a page and does not move when a daily
    // refresh silently stops. This line is about the DATA.
    //
    // The age itself carries no verdict: it is reported for every page, and the
    // judgement lives in the refresh-health chip below it, which only a page
    // with a refresh schedule can earn (Pages knows a page's expected cadence
    // only when someone sets it). "checked" only appears when someone looked
    // MORE RECENTLY than the last write — that is the whole point of it, and
    // repeating the refresh date under a second name would just be noise.
    //
    // A page created without data has refreshed but holds no source data yet:
    // the server reports its source_as_of as null (it stores the epoch, which
    // read literally is "data 20,753 days old"), so the line says so instead.
    const dataAgeNode = (freshness) => {
      if (!freshness) return null;
      const { days_since_source: source, days_since_refresh: refresh, days_since_check: check } = freshness;
      const parts = [];
      if (source !== null) parts.push(t("Data {0}", source === 0 ? t("today") : t("{0}d old", source)));
      else if (freshness.refreshed_at) parts.push(t("No data yet"));
      if (check !== null && (refresh === null || check < refresh)) parts.push(t("checked {0}", days(check)));
      if (!parts.length) return null;
      const detail = [
        t("Source coverage: {0}", freshness.source_as_of ? formatWhen(freshness.source_as_of) : t("none yet")),
        t("Refreshed: {0}", formatWhen(freshness.refreshed_at)),
        t("Last checked: {0}", formatWhen(freshness.checked_at)),
        freshness.latest_outcome ? t("Latest outcome: {0}", window.PagesMessages.outcome(freshness.latest_outcome)) : null,
        freshness.latest_detail,
        freshness.last_check_at && freshness.last_check_at !== freshness.checked_at
          ? t("Previous check: {0} ({1})", formatWhen(freshness.last_check_at), window.PagesMessages.outcome(freshness.last_check_outcome || "unknown")) : null,
      ].filter(Boolean).join("\n");
      return el("span", { class: "table-meta", title: detail },
        ...parts.map((part) => el("span", { class: "table-meta__line" }, part)));
    };

    // Red for a run that failed, amber for one that is late, keeps finding no
    // new data, or publishes data part of which has stopped ("Partial data") —
    // and nothing at all for a page that is fine or has no schedule,
    // so the chips that do appear are the list of things to fix. The reason is
    // on the row, not only in a tooltip: it is what the reader acts on.
    //
    // The one exception is grey: a live page with week-old data and no refresh
    // schedule says "No refresh". It is a question, not a fault — the campaign
    // may simply have ended — so it takes the neutral tone, stays out of Needs
    // attention, and keeps its sentence to the hover and to assistive tech: the
    // data age is already the line above it, and on an estate where half the
    // rows carry it, a clipped sentence on each would be the noise this chip is
    // meant not to be.
    const refreshHealthNode = (row) => {
      if (Health.needsAttention(row)) {
        const { kind, label } = Health.describeHealth(row);
        const reason = window.PagesMessages.healthReason(row.refresh_health, row.freshness || {});
        return el("div", { class: "refresh-health" },
          statusChip(kind, label, { title: reason }),
          el("span", { class: "refresh-health__reason", title: reason }, reason));
      }
      const hint = Health.unscheduledStaleHint(row);
      if (!hint) return null;
      return el("div", { class: "refresh-health" },
        statusChip(Health.NO_REFRESH.kind, Health.NO_REFRESH.label, { title: hint }),
        el("span", { class: "sr-only" }, hint));
    };

    // The title is the link to the review screen, so the row does not need a button
    // to say so — and a filled primary on all 37 rows meant nothing was primary.
    // "Open as staff" named the reader, not the destination; this opens the page a
    // client sees, in a new tab, and says so.
    const actions = el("div", { class: "row-actions" },
      page.published_version_id && !page.disabled
        ? el("a", {
            class: "btn btn-ghost btn-sm",
            href: I18N.ownedURL(`/view/${slugPath(page.slug)}`),
            target: "_blank",
            rel: "noopener",
            title: t("Open the live client page for /{0} in a new tab", page.slug),
            // The sprite has no external-link glyph; the page-switcher already
            // uses a bare arrow character for the same reason.
          }, t("View live"), el("span", { "aria-hidden": "true" }, "\u2197"))
        : null,
      boot.compose && page.published_version_id
        ? el("button", { id: `revise-page-${page.id}`, class: "btn btn-ghost btn-sm", type: "button", onclick: (event) => openComposeDialog(event.currentTarget, "revise", page.slug) }, t("Revise"))
        : null);

    return el("tr", {},
      el("td", { "data-label": t("Status"), "data-column": "status" }, statusNode(page)),
      el("td", { "data-label": t("Page"), "data-column": "page" },
        el("div", { class: "page-cell" },
          el("a", { class: "page-cell__title", href: `/admin/${slugPath(page.slug)}` }, page.title || page.slug),
          el("code", {}, `/${page.slug}`))),
      el("td", { "data-label": t("Live version"), "data-column": "live" },
        page.published_version_id
          ? el("span", {
              class: "table-meta",
              title: t("Page-local version number"),
            }, Number(page.published_version_number) > 0 ? t("Version {0}", page.published_version_number) : t("Published"))
          : el("span", { class: "table-meta" }, t("Not published"))),
      // Two independent facts — when the page changed, and how old its data is —
      // were inline siblings, so they ran together as "5 minutes agoData 46d old".
      // .page-cell is the stack this table already uses for title-over-slug.
      el("td", { "data-label": t("Last update"), "data-column": "updated" },
        el("div", { class: "page-cell" },
          timeWhen(page.updated_at, { class: "table-meta" }),
          dataAgeNode(page.freshness),
          refreshHealthNode(page))),
      el("td", { "data-label": t("Workspace"), "data-column": "workspace" },
        el("div", { class: "workspace-assignment" }, assignment)),
      el("td", { "data-label": t("Actions"), "data-column": "actions" }, actions));
  }

  function operationalList(pages, workspaces) {
    // --index opts this table into the bespoke collapsed arrangement keyed to
    // these six stable column keys. Other operation-tables collapse generically.
    const table = el("table", { class: "operation-table operation-table--index" },
      el("caption", { class: "sr-only" }, t("Pages, serving status, current live version, update time, workspace, and actions")),
      el("thead", {}, el("tr", {},
        el("th", { class: "operation-table__status", scope: "col" }, t("Status")),
        el("th", { class: "operation-table__page", scope: "col" }, t("Page")),
        el("th", { class: "operation-table__live", scope: "col" }, t("Live version")),
        el("th", { class: "operation-table__updated", scope: "col" }, t("Last update")),
        el("th", { class: "operation-table__workspace", scope: "col" }, t("Workspace")),
        el("th", { class: "operation-table__actions", scope: "col" }, t("Actions")))),
      el("tbody", {}, pages.map((page) => operationRow(page, workspaces))));
    return el("div", { class: "operation-table-wrap" }, table);
  }

  function activeWorkspaceLabel(workspaces) {
    if (currentView === "ungrouped") return t("Ungrouped");
    if (currentView.startsWith("workspace:")) {
      return workspaces.find((workspace) => String(workspace.id) === workspaceIdForView())?.name || t("All pages");
    }
    return t("All pages");
  }

  function changeView(view, focusSelector) {
    if (currentView === view) return;
    setView(view);
    render(currentData);
    if (focusSelector) app.querySelector(focusSelector)?.focus();
  }

  function workspaceNavigation(pages, workspaces) {
    const nav = el("nav", { class: "workspace-nav", "aria-label": t("Page workspaces") });
    const add = (view, label, count) => {
      nav.append(el("button", {
        class: "workspace-nav__item",
        type: "button",
        "aria-pressed": currentView === view ? "true" : "false",
        onclick: () => changeView(view, '.workspace-nav__item[aria-pressed="true"]'),
      }, el("span", { class: "workspace-nav__label", title: label }, label),
         el("span", { class: "workspace-nav__count" }, String(count))));
    };
    add("all", t("All pages"), pages.length);
    add("ungrouped", t("Ungrouped"), pages.filter((page) => page.workspace_id == null).length);
    workspaces.forEach((workspace) => add(`workspace:${workspace.id}`, workspace.name, Number(workspace.page_count || 0)));
    return el("aside", { class: "workspace-sidebar" },
      el("div", { class: "workspace-sidebar__head" },
        el("h2", {}, t("Workspaces")),
        el("button", {
          id: "workspace-manager-sidebar",
          class: "icon-action",
          type: "button",
          title: t("Manage workspaces"),
          "aria-label": t("Manage workspaces"),
          onclick: (event) => openWorkspaceManager(event.currentTarget),
        }, icon("folder"))),
      nav);
  }

  function mobileWorkspacePicker(pages, workspaces) {
    const select = el("select", { id: "mobile-workspace", "aria-label": t("Current workspace") },
      el("option", { value: "all" }, t("All pages ({0})", pages.length)),
      el("option", { value: "ungrouped" }, t("Ungrouped ({0})", pages.filter((page) => page.workspace_id == null).length)));
    for (const workspace of workspaces) {
      select.append(el("option", { value: `workspace:${workspace.id}` }, `${workspace.name} (${workspace.page_count || 0})`));
    }
    select.value = currentView;
    select.addEventListener("change", () => changeView(select.value, "#mobile-workspace"));
    return el("div", { class: "workspace-mobile" },
      field({ id: "mobile-workspace", label: t("Workspace"), control: select, wrap: "label" }),
      el("button", {
        id: "workspace-manager-mobile",
        class: "btn workspace-mobile__manage",
        type: "button",
        title: t("Manage workspaces"),
        "aria-label": t("Manage workspaces"),
        onclick: (event) => openWorkspaceManager(event.currentTarget),
      }, icon("folder"), el("span", { class: "workspace-mobile__manage-label" }, t("Manage workspaces"))));
  }

  function openMoveDialog(trigger, page, workspaces) {
    const select = el("select", { id: "move-workspace" });
    workspaceOptions(select, workspaces);
    select.value = page.workspace_id == null ? "" : String(page.workspace_id);
    const status = el("p", { class: "field-error", role: "alert" });

    const modal = makeDialog({
      kicker: t("Organization"),
      title: t("Move {0}", page.title || `/${page.slug}`),
      description: t("Workspaces group pages for this index only. Moving one changes nothing a client sees."),
      closeLabel: t("Close move dialog"),
    });
    const form = el("form", { class: "form-stack" },
      field({ id: "move-workspace", label: t("Workspace"), control: select }),
      status);
    const cancel = el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("cancel") }, t("Cancel"));
    // The action row is a sibling of the body, so a type="submit" button there is
    // outside the form and submits nothing. Every dialog on this screen bridges it
    // the same way.
    const save = el("button", { class: "btn btn-primary", type: "button" }, t("Move page"));
    save.addEventListener("click", () => form.requestSubmit());
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      status.textContent = "";
      const outcome = await runAction({
        button: save,
        busyLabel: t("Moving…"),
        run: () => api(`/pages/${slugPath(page.slug)}/workspace`, {
          body: { workspace_id: select.value ? Number(select.value) : null },
        }),
        success: t("Moved /{0}", page.slug),
        failure: (error) => t("Couldn't move /{0}: {1}", page.slug, error.message),
      });
      if (!outcome.ok) {
        status.textContent = outcome.error ? outcome.error.message : t("Something went wrong.");
        return;
      }
      modal.close("moved");
      await load();
    });
    modal.body.append(form);
    modal.actions.append(cancel, save);
    modal.open(trigger);
    select.focus();
  }

  function pageIndex(pages, workspaces) {
    const label = activeWorkspaceLabel(workspaces);
    const scoped = filterWorkspacePages(pages, currentView);
    const baseCount = scoped.length;
    const attentionCount = Health.attentionRows(scoped).length;
    const unscheduledCount = Health.unscheduledStaleRows(scoped).length;
    const count = el("span", { class: "workspace-index__count", role: "status", "aria-live": "polite", "aria-atomic": "true", tabindex: "-1" });
    // Filters, so each says whether it is on (aria-pressed) and how many it
    // would show, in the scope the reader is already looking at.
    const filters = {};
    const syncFilters = () => {
      for (const [name, button] of Object.entries(filters)) button.setAttribute("aria-pressed", indexFilter === name ? "true" : "false");
    };
    const filterButton = (name, className, title, text) => {
      filters[name] = el("button", {
        id: `${name}-filter`,
        class: `btn btn-sm ${className}`,
        type: "button",
        "aria-pressed": indexFilter === name ? "true" : "false",
        title,
        onclick: () => {
          setFilter(indexFilter === name ? null : name);
          syncFilters();
          draw();
        },
      }, text);
      return filters[name];
    };
    const attention = filterButton("attention", "attention-filter",
      t("Pages whose scheduled refresh is blocked, overdue, or finding no new source data"),
      t("Needs attention ({0})", attentionCount));
    // Only offered when there is something to show (or it is already on, so it
    // can be turned off): on an estate where every live page has a schedule it
    // would be a permanent "(0)" beside the one filter that matters.
    const unscheduled = unscheduledCount || indexFilter === "unscheduled"
      ? filterButton("unscheduled", "unscheduled-filter",
          t("Live pages with no refresh schedule whose data is more than 7 days old: set a schedule, or ignore the ones whose campaign has ended"),
          `${Health.NO_REFRESH.filter} (${unscheduledCount})`)
      : null;
    const search = el("input", {
      type: "search",
      value: currentQuery,
      placeholder: t("Search title, slug, or workspace"),
      "aria-label": t("Search {0}", label),
    });
    const results = el("div", { class: "workspace-index__results" });

    function draw() {
      currentQuery = search.value;
      let visible = filterWorkspacePages(pages, currentView, currentQuery);
      // Worst first: a blocked run before a late one before a stuck source. And
      // without a schedule, the oldest data first.
      if (indexFilter === "attention") visible = Health.attentionRows(visible);
      else if (indexFilter === "unscheduled") visible = Health.unscheduledStaleRows(visible);
      const windowed = visible.slice(0, shown);
      const remaining = visible.length - windowed.length;
      // The count is the only thing that says the list is not all of it, so it
      // has to say so rather than reporting a total the table does not show.
      const total = currentQuery.trim() || indexFilter
        ? (baseCount === 1 ? t("{0} of 1 page", visible.length) : t("{0} of {1} pages", visible.length, baseCount))
        : (baseCount === 1 ? t("1 page") : t("{0} pages", baseCount));
      count.textContent = remaining > 0 ? t("Showing {0} of {1}", windowed.length, total) : total;
      if (visible.length) {
        const list = operationalList(windowed, workspaces);
        if (remaining <= 0) {
          results.replaceChildren(list);
          return;
        }
        const more = el("button", {
          id: "show-more-pages",
          class: "btn index-more__button",
          type: "button",
          onclick: () => {
            // The row that will be first of the newly revealed ones. It renders
            // exactly where this button is standing, so focusing it moves the
            // page not at all — which is the point.
            const firstNew = windowed.length;
            shown += PAGE_WINDOW;
            draw();
            // The first new row, never the button and never the count. Both of
            // those move: the count lives in the toolbar, so focusing it dragged
            // the reader the whole way back to the top of the screen, and the
            // button — when enough pages are left that it survives its own press
            // — has just been pushed a screenful DOWN by the rows inserted above
            // it, so following it threw the reader 1,856px the other way. The row
            // is the only target that renders where the reader is already looking,
            // and it is what they pressed the button to get. (Neither was an edge
            // case: the last press always removes the button, and 37 pages against
            // a 25-row window means there is only ever one press.)
            const revealed = results
              .querySelectorAll(".operation-table tbody tr")[firstNew]
              ?.querySelector(".page-cell__title");
            if (revealed) {
              revealed.focus();
              return;
            }
            // Nothing to hand focus to. The count is a live region and announces
            // the new total on its own, so it only has to hold focus off <body>
            // — without dragging the viewport along.
            count.focus({ preventScroll: true });
          },
        }, t("Show {0} more", Math.min(remaining, PAGE_WINDOW)));
        results.replaceChildren(list, el("div", { class: "index-more" },
          el("span", { class: "table-meta" }, remaining === 1 ? t("1 more page") : t("{0} more pages", remaining)),
          more));
        return;
      }
      const searching = Boolean(currentQuery.trim());
      if (indexFilter && !searching) {
        const active = filters[indexFilter];
        const [title, text] = indexFilter === "attention"
          ? [t("Nothing needs attention"), t("Every page with a refresh schedule here is on time, finding new data and showing all of it.")]
          : [t("Every live page has a refresh"), t("No live page here has gone more than 7 days without new data and without a refresh schedule.")];
        results.replaceChildren(emptyState(title, text,
          el("button", {
            class: "btn",
            type: "button",
            onclick: () => {
              setFilter(null);
              syncFilters();
              draw();
              active.focus();
            },
          }, t("Show all pages"))));
        return;
      }
      // An empty workspace used to say "Move a page here from another workspace."
      // and render no way to do it. The pages exist — they are just somewhere
      // else — so the useful action is to go and look at them.
      const action = !pages.length || searching
        ? el("button", {
            class: "btn btn-primary",
            type: "button",
            onclick: (event) => openNewPageDialog(event.currentTarget),
          }, icon("plus"), t("New page"))
        : el("button", {
            class: "btn",
            type: "button",
            onclick: () => changeView("all", '.workspace-nav__item[aria-pressed="true"]'),
          }, t("Show all {0} pages", pages.length));
      results.replaceChildren(emptyState(
        searching ? t("No matching pages") : t("Nothing in {0}", label),
        searching
          ? t("Try a title, slug, or workspace name.")
          : pages.length ? t("Move a page here from another workspace.") : t("Create an empty page, then add source from its review screen or deploy over MCP."),
        action
      ));
    }
    search.addEventListener("input", () => {
      shown = PAGE_WINDOW;
      draw();
    });
    draw();
    return el("section", { class: "workspace-index", "aria-labelledby": "page-list-title" },
      el("div", { class: "index-toolbar" },
        el("div", { class: "index-toolbar__copy" },
          // No "Current view" overline: an overline, an h2 and a caption stacked
          // in a column is a second page header, and the reader met two titles
          // for one screen. The scope and how much of it is showing are one fact.
          el("h2", { id: "page-list-title" }, label),
          count),
        el("div", { class: "index-toolbar__tools" },
          attention,
          unscheduled,
          el("label", { class: "search-field" },
            el("span", { class: "sr-only" }, t("Search {0}", label)),
            icon("search"), search))),
      results);
  }

  function openNewPageDialog(trigger) {
    const modal = makeDialog({
      title: t("New page"),
      kicker: t("Create"),
      description: t("Create an empty page shell. Add source from its review screen or deploy content over MCP."),
      onClose: () => modal.dialog.remove(),
    });
    const slug = el("input", { id: "new-page-slug", type: "text", autocapitalize: "off", autocomplete: "off", spellcheck: "false", placeholder: "client/report-q2", required: true });
    const title = el("input", { id: "new-page-title", type: "text", placeholder: t("Client Q2 report") });
    const workspace = el("select", { id: "new-page-workspace" });
    workspaceOptions(workspace, currentData.workspaces);
    workspace.value = workspaceIdForView();
    const status = el("p", { class: "form-status", role: "status", "aria-live": "polite" });
    const create = el("button", { class: "btn btn-primary", type: "submit" }, t("Create page"));
    const cancel = el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("cancel") }, t("Cancel"));
    const form = el("form", { class: "form-stack" },
      el("div", { class: "form-grid" },
        field({
          id: "new-page-slug",
          label: t("Slug"),
          control: slug,
          help: t("Lowercase path; nested slugs such as client/q2 are supported."),
          wrap: "label",
        }),
        field({ id: "new-page-title", label: t("Title (optional)"), control: title, wrap: "label" })),
      field({ id: "new-page-workspace", label: t("Workspace"), control: workspace, wrap: "label" }));
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!slug.value.trim()) { status.textContent = t("Enter a slug."); slug.focus(); return; }
      setBusy(create, true, t("Creating…"));
      status.textContent = "";
      try {
        const result = await api("/pages", {
          body: {
            slug: slug.value.trim().toLowerCase(),
            title: title.value.trim(),
            workspace_id: workspace.value ? Number(workspace.value) : null,
          },
        });
        window.location.assign(I18N.withLanguage(`/admin/${slugPath(result.page.slug)}`));
      } catch (error) {
        status.textContent = error.message;
        setBusy(create, false, t("Create page"));
      }
    });
    modal.body.append(form, status);
    modal.actions.append(cancel, create);
    create.addEventListener("click", () => form.requestSubmit());
    modal.open(trigger);
    slug.focus();
  }

  function renderWorkspaceManager(modal, workspaces) {
    const name = el("input", { id: "workspace-create-name", type: "text", maxlength: "100", placeholder: t("Client or team name"), required: true });
    const add = el("button", { class: "btn btn-primary", type: "submit" }, t("Add workspace"));
    const status = el("p", { class: "form-status", role: "status", "aria-live": "polite" });
    const createForm = el("form", { class: "workspace-create-form" },
      field({ id: "workspace-create-name", label: t("Workspace name"), control: name, wrap: "label" }),
      add,
      status);
    createForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const value = name.value.trim();
      if (!value) { status.textContent = t("Enter a workspace name."); name.focus(); return; }
      setBusy(add, true, t("Adding…"));
      try {
        const result = await api("/workspaces", { body: { name: value } });
        setView(`workspace:${result.workspace.id}`);
        await load();
        renderWorkspaceManager(modal, currentData.workspaces);
        toast(t("Created {0}", result.workspace.name));
      } catch (error) {
        status.textContent = error.message;
        setBusy(add, false, t("Add workspace"));
      }
    });

    const list = el("div", { class: "workspace-manager-list" });
    if (!workspaces.length) list.append(emptyState(t("No workspaces yet"), t("Create one to group related client pages.")));
    for (const workspace of workspaces) {
      const row = el("div", { class: "workspace-manager-row" });
      const actions = el("div", { class: "workspace-manager-row__actions" });
      const rename = el("button", { class: "btn btn-sm", type: "button" }, t("Rename"));
      const remove = el("button", { class: "btn btn-sm btn-danger", type: "button" }, t("Remove"));
      actions.append(rename, remove);
      row.append(el("div", { class: "workspace-manager-row__main" },
        el("div", { class: "workspace-manager-row__copy" },
          el("strong", {}, workspace.name),
          el("span", { class: "muted" }, `${workspace.page_count || 0} ${Number(workspace.page_count) === 1 ? "page" : "pages"}`)),
        actions));

      rename.addEventListener("click", () => {
        const input = el("input", { type: "text", maxlength: "100", value: workspace.name, "aria-label": t("New name for {0}", workspace.name) });
        const save = el("button", { class: "btn btn-sm btn-primary", type: "submit" }, t("Save"));
        const cancel = el("button", { class: "btn btn-sm", type: "button" }, t("Cancel"));
        const form = el("form", { class: "workspace-rename-form" }, input, save, cancel);
        cancel.addEventListener("click", () => renderWorkspaceManager(modal, currentData.workspaces));
        form.addEventListener("submit", async (event) => {
          event.preventDefault();
          const value = input.value.trim();
          if (!value) { input.focus(); return; }
          setBusy(save, true, t("Saving…"));
          try {
            await api(`/workspaces/${workspace.id}/rename`, { body: { name: value } });
            await load();
            renderWorkspaceManager(modal, currentData.workspaces);
            toast(t("Workspace renamed"));
          } catch (error) {
            toast(t("Rename failed: {0}", error.message), { tone: "error" });
            setBusy(save, false, t("Save"));
          }
        });
        row.replaceChildren(form);
        input.focus();
        input.select();
      });

      remove.addEventListener("click", async () => {
        const count = Number(workspace.page_count || 0);
        const approved = await confirmDialog({
          trigger: remove,
          title: t("Remove {0}?", workspace.name),
          message: count === 1 ? t("1 page will move to Ungrouped. No pages will be deleted.") : t("{0} pages will move to Ungrouped. No pages will be deleted.", count),
          confirmLabel: t("Remove workspace"),
          danger: true,
        });
        if (!approved) return;
        remove.disabled = true;
        try {
          await api(`/workspaces/${workspace.id}/delete`, { body: {} });
          if (currentView === `workspace:${workspace.id}`) setView("ungrouped", { replace: true });
          await load();
          renderWorkspaceManager(modal, currentData.workspaces);
          toast(t("Workspace removed; pages moved to Ungrouped"));
        } catch (error) {
          remove.disabled = false;
          toast(t("Remove failed: {0}", error.message), { tone: "error" });
        }
      });
      list.append(row);
    }

    modal.body.replaceChildren(createForm, list);
  }

  function openWorkspaceManager(trigger) {
    const modal = makeDialog({
      title: t("Manage workspaces"),
      kicker: t("Organization"),
      description: t("Create, rename, or remove one-level groups. Removing a workspace never deletes its pages."),
      onClose: () => modal.dialog.remove(),
    });
    modal.body.classList.add("workspace-manager-body");
    modal.actions.append(el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("done") }, t("Done")));
    renderWorkspaceManager(modal, currentData.workspaces);
    modal.open(trigger);
    // Without this the dialog opens focused on its close button, while every other
    // create dialog on this screen focuses the field you came here to fill in.
    modal.body.querySelector("input, select, textarea")?.focus();
  }

  const CREATE_PLACEHOLDER = t("Describe the page, its audience, required metrics, and desired layout.");
  const REVISE_PLACEHOLDER = t("Describe what is wrong and what Cutlass should improve.");

  function openComposeDialog(trigger, initialMode = "create", initialSlug = "") {
    const mode = initialMode === "revise" ? "revise" : "create";
    const modal = makeDialog({
      title: mode === "revise" ? t("Revise with Cutlass") : t("Compose with Cutlass"),
      kicker: mode === "revise" ? t("Existing page") : t("New page"),
      description: mode === "revise"
        ? t("Cutlass reads the published page, audits it against your notes, and publishes an improved version.")
        : t("Describe a page and Cutlass writes themed HTML and publishes it through the existing MCP workflow."),
      size: "wide",
      onClose: () => modal.dialog.remove(),
    });
    const prompt = el("textarea", { id: "compose-prompt", rows: "6", placeholder: mode === "revise" ? REVISE_PLACEHOLDER : CREATE_PLACEHOLDER, required: true });
    const slug = el("input", { id: "compose-slug", type: "text", value: initialSlug, placeholder: "client-report", autocapitalize: "off", autocomplete: "off", spellcheck: "false", required: true, readOnly: mode === "revise" });
    const title = el("input", { id: "compose-title", type: "text", placeholder: t("Client report") });
    const status = el("p", { class: "form-status", role: "status", "aria-live": "polite" });
    const log = el("pre", { class: "compose-log", hidden: true });
    const run = el("button", { class: "btn btn-primary", type: "submit" }, mode === "revise" ? t("Revise with Cutlass") : t("Generate with Cutlass"));
    const cancel = el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("cancel") }, t("Cancel"));
    const form = el("form", { class: "form-stack" },
      field({
        id: "compose-prompt",
        label: mode === "revise" ? t("What to change") : t("Page brief"),
        control: prompt,
        wrap: "label",
      }),
      el("div", { class: "form-grid" },
        field({ id: "compose-slug", label: t("Slug"), control: slug, wrap: "label" }),
        mode === "create"
          ? field({ id: "compose-title", label: t("Title (optional)"), control: title, wrap: "label" })
          : null),
      log);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const brief = prompt.value.trim();
      const pageSlug = slug.value.trim().toLowerCase();
      if (!brief) { status.textContent = mode === "revise" ? t("Describe what to change.") : t("Enter a page brief."); prompt.focus(); return; }
      if (!pageSlug) { status.textContent = t("Enter a slug."); slug.focus(); return; }
      setBusy(run, true, mode === "revise" ? t("Revising…") : t("Generating…"));
      cancel.disabled = true;
      log.hidden = false;
      log.textContent = "";
      status.textContent = t("Cutlass is starting…");
      try {
        const { jobId } = await api("/compose", { body: { prompt: brief, slug: pageSlug, title: title.value.trim(), mode } });
        for (;;) {
          await sleep(1500);
          const job = await api(`/compose/${jobId}`);
          log.textContent = job.log || "";
          log.scrollTop = log.scrollHeight;
          if (job.status === "running") { status.textContent = t("Cutlass is working…"); continue; }
          if (job.status === "done") {
            status.textContent = t("Published /{0}", job.slug);
            toast(t("Cutlass published /{0}", job.slug));
            await load();
          } else {
            status.textContent = t("Cutlass failed. Review the log for details.");
          }
          break;
        }
      } catch (error) {
        status.textContent = error.message;
      } finally {
        setBusy(run, false, mode === "revise" ? t("Revise with Cutlass") : t("Generate with Cutlass"));
        cancel.disabled = false;
      }
    });
    modal.body.append(form, status);
    modal.actions.append(cancel, run);
    run.addEventListener("click", () => form.requestSubmit());
    modal.open(trigger);
    prompt.focus();
  }

  function render(data) {
    currentData = { pages: data.pages || [], workspaces: data.workspaces || [] };
    const selectedId = workspaceIdForView();
    if (selectedId && !currentData.workspaces.some((workspace) => String(workspace.id) === selectedId)) {
      setView("all", { replace: true });
    }
    const live = currentData.pages.filter((page) => page.published_version_id && !page.disabled).length;
    const gated = currentData.pages.filter((page) => page.require_approval).length;
    const attention = Health.attentionRows(currentData.pages).length;


    // A workspace move re-renders the index, and the row control that triggered it
    // is rebuilt; without this, focus lands on <body>. The row buttons carry
    // stable ids (workspace-of-<id>, revise-page-<id>) so they can be found again.
    keepingFocus(() => app.replaceChildren(
      pageHeader({
        // No kicker: the section tab above already names this screen, and
        // repeating it over a differently-worded h1 read as two headers.
        title: t("Client pages"),
        intro: t("Review, publish, organize, and share versioned client work from one queue."),
        actions: [
          boot.compose ? el("button", { id: "compose-action", class: "btn", type: "button", onclick: (event) => openComposeDialog(event.currentTarget) }, icon("file"), t("Compose with Cutlass")) : null,
          el("button", { id: "new-page-action", class: "btn btn-primary", type: "button", onclick: (event) => openNewPageDialog(event.currentTarget) }, icon("plus"), t("New page")),
        ],
        stats: [
          statTile(currentData.pages.length, t("page"), t("pages")),
          statTile(live, t("live page"), t("live pages")),
          statTile(gated, t("approval gated"), t("approval gated")),
          statTile(attention, t("needs attention"), t("need attention")),
        ],
      }),
      mobileWorkspacePicker(currentData.pages, currentData.workspaces),
      el("div", { class: "workspace-layout" },
        workspaceNavigation(currentData.pages, currentData.workspaces),
        pageIndex(currentData.pages, currentData.workspaces))), { fallback: ['.workspace-nav__item[aria-pressed="true"]', "#app h1"] });
  }

  async function load() {
    try {
      render(await api("/pages"));
    } catch (error) {
      app.replaceChildren(loadFailed(t("client pages"), error, load));
    }
  }

  window.addEventListener("popstate", () => {
    currentView = viewFromURL();
    indexFilter = filterFromURL();
    render(currentData);
  });
  load();
})();
