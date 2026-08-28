// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// page-switcher.js — pure ordering/navigation helpers shared by the browser UI
// and unit tests. The admin shell uses a native <select>, so long page lists get
// scrolling, type-ahead, and keyboard semantics from the browser without a
// bespoke combobox implementation.

(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.PagesPageSwitcher = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  function compareText(a, b) {
    const al = String(a || "").toLowerCase();
    const bl = String(b || "").toLowerCase();
    if (al < bl) return -1;
    if (al > bl) return 1;
    const as = String(a || "");
    const bs = String(b || "");
    return as < bs ? -1 : as > bs ? 1 : 0;
  }

  function statusOf(page) {
    if (page.disabled) return "disabled";
    if (page.is_live || page.published_version_id) return "live";
    if (page.require_approval) return "approval";
    return "draft";
  }

  function encodeSlugPath(slug) {
    return String(slug || "").split("/").map(encodeURIComponent).join("/");
  }

  function adminPath(slug) {
    return "/admin/" + encodeSlugPath(slug);
  }

  // Alphabetical title + slug ordering is deliberately independent of API/DB
  // insertion order, and the unique slug is the final deterministic tie-break.
  function model(pages, currentSlug) {
    const items = (Array.isArray(pages) ? pages : [])
      .filter((page) => page && typeof page.slug === "string" && page.slug)
      .slice()
      .sort((a, b) => {
        const byTitle = compareText(a.title || a.slug, b.title || b.slug);
        return byTitle || compareText(a.slug, b.slug);
      })
      .map((page, index) => {
        const title = String(page.title || page.slug);
        const status = statusOf(page);
        return {
          slug: page.slug,
          title,
          status,
          // Preserve the admin index's organization metadata so the switcher
          // can filter/group by the same workspace without another API call.
          workspaceId: page.workspace_id == null ? null : page.workspace_id,
          workspaceName: page.workspace_name || null,
          position: index + 1,
          href: adminPath(page.slug),
          optionLabel: `${index + 1}. ${title} — /${page.slug} · ${status}`,
        };
      });
    const currentIndex = items.findIndex((item) => item.slug === currentSlug);
    return {
      items,
      total: items.length,
      currentIndex,
      current: currentIndex >= 0 ? items[currentIndex] : null,
      previous: currentIndex > 0 ? items[currentIndex - 1] : null,
      next: currentIndex >= 0 && currentIndex < items.length - 1 ? items[currentIndex + 1] : null,
    };
  }

  return { adminPath, encodeSlugPath, model, statusOf };
});
