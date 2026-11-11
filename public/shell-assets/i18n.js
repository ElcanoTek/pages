// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
(function (root, factory) {
  const api = factory(root);
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.PagesI18n = api;
})(typeof window !== "undefined" ? window : null, function (root) {
  const STORAGE_KEY = "elcano.language";
  const COOKIE_NAME = "elcano_language";
  function normalize(value) {
    const tag = String(value || "").trim().toLowerCase().split("-")[0];
    return ["en", "es", "pt"].includes(tag) ? tag : "";
  }
  function resolve({ search = "", storage, cookie = "", languages = [] } = {}) {
    const query = normalize(new URLSearchParams(search).get("lang"));
    if (query) return query;
    let stored = "";
    try { stored = normalize(storage && storage.getItem(STORAGE_KEY)); } catch { /* URL remains usable. */ }
    if (stored) return stored;
    const preference = cookie.split(";").map((item) => item.trim()).find((item) => item.startsWith(COOKIE_NAME + "="));
    if (preference) {
      const saved = normalize(preference.slice(COOKIE_NAME.length + 1));
      if (saved) return saved;
    }
    return languages.map(normalize).find(Boolean) || "en";
  }
  function interpolate(message, values) {
    return String(message).replace(/\{(\d+)\}/g, (slot, index) => Number(index) < values.length ? String(values[Number(index)]) : slot);
  }
  function translator(language, messages = {}) {
    return (source, ...values) => interpolate(language !== "en" && Object.hasOwn(messages, source) ? messages[source] : source, values);
  }
  let language = "en";
  let messages = {};
  let serverLanguage = "en";
  let storage;
  let cookie = "";
  if (root) {
    try {
      const boot = JSON.parse(root.document.getElementById("pages-i18n").textContent);
      serverLanguage = normalize(boot.language) || "en";
      messages = boot.messages || {};
    } catch { /* English remains usable on a partial/error shell. */ }
    try { storage = root.localStorage; } catch { /* Storage can be denied on access. */ }
    try { cookie = root.document.cookie; } catch { /* Cookies may be denied independently of storage. */ }
    language = resolve({search: root.location.search, storage, cookie, languages: root.navigator.languages || [root.navigator.language]});
  }
  function withLanguage(href, selected) {
    if (!root && !selected) return href;
    const explicit = selected || normalize(new URLSearchParams(root.location.search).get("lang"));
    if (!explicit) return href;
    const base = root ? root.location.href : "https://pages.invalid/";
    const url = new URL(href, base);
    if (url.origin !== new URL(base).origin) return href;
    url.searchParams.set("lang", normalize(explicit) || "en");
    return url.pathname + url.search + url.hash;
  }
  // Only Pages-owned view/preview links use this; customer links and copied
  // client URLs remain data. The content origin is supplied by the server.
  function ownedURL(href) {
    if (!root) return href;
    const explicit = normalize(new URLSearchParams(root.location.search).get("lang"));
    if (!explicit && language === resolve({languages: root.navigator.languages || [root.navigator.language]})) return href;
    try {
      const url = new URL(href, root.location.href);
      let contentOrigin = "";
      try { contentOrigin = JSON.parse(root.document.getElementById("pages-bootstrap").textContent).contentOrigin || ""; } catch { /* Same-origin remains available. */ }
      if (url.origin !== root.location.origin && (!contentOrigin || url.origin !== new URL(contentOrigin).origin)) return href;
      url.searchParams.set("lang", language);
      return url.origin === root.location.origin ? url.pathname + url.search + url.hash : url.href;
    } catch { return href; }
  }
  function persist(next) {
    try { if (storage) storage.setItem(STORAGE_KEY, next); } catch { /* URL remains usable. */ }
    try { root.document.cookie = COOKIE_NAME + "=" + next + "; Path=/; Max-Age=31536000; SameSite=Lax" + (root.location.protocol === "https:" ? "; Secure" : ""); } catch { /* URL remains usable. */ }
  }
  function save(selected) {
    const next = normalize(selected);
    if (!next || !root) return;
    persist(next);
    root.location.assign(withLanguage(root.location.href, next));
  }
  if (root) {
    if (normalize(new URLSearchParams(root.location.search).get("lang"))) persist(language);
    // A saved browser preference can precede the server's first cookie. Reload
    // once with an explicit tag before initializing the dashboard's UI modules.
    if (language !== serverLanguage) root.location.replace(withLanguage(root.location.href, language));
    root.document.documentElement.lang = language;
    root.document.addEventListener("DOMContentLoaded", () => {
      // Flag owns theme behavior; this adapter owns Pages' localized labels.
      // Observe only the theme attribute, never customer content or arbitrary text.
      const themeLabels = () => {
        const label = root.document.documentElement.getAttribute("data-theme") === "dark" ? t("Switch to light mode") : t("Switch to dark mode");
        root.document.querySelectorAll("[data-flag-theme-toggle]").forEach((toggle) => toggle.setAttribute("aria-label", label));
      };
      themeLabels();
      new root.MutationObserver(themeLabels).observe(root.document.documentElement, {attributes: true, attributeFilter: ["data-theme"]});
      const picker = root.document.getElementById("pages-language");
      if (picker) { picker.value = language; picker.addEventListener("change", () => save(picker.value)); }
      root.document.addEventListener("click", (event) => {
        const link = event.target.closest && event.target.closest("a[href]");
        if (!link) return;
        const url = new URL(link.href, root.location.href);
        if (url.origin === root.location.origin && (url.pathname === "/admin" || url.pathname.startsWith("/admin/"))) link.href = withLanguage(link.href);
      });
    });
  }
  const t = translator(language, messages);
  return { t, language, locale: language === "pt" ? "pt-PT" : language, normalize, resolve, translator, interpolate, withLanguage, ownedURL, save };
});
