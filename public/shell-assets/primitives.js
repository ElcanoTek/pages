// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Shared, framework-free browser primitives for every Pages admin surface.
// They centralize safe DOM construction, Flag icons, native modal dialogs,
// confirmations, live-region toasts, async button state, and state panels.
(function (root, factory) {
  const api = factory();
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (root) root.PagesUI = api;
})(typeof window !== "undefined" ? window : null, function () {
  const SPRITE = "/shell-assets/flag/icons/core-icons.svg";
  const SVG_NS = "http://www.w3.org/2000/svg";
  let nextId = 0;

  function append(parent, child) {
    if (child == null || child === false) return;
    if (Array.isArray(child)) {
      child.forEach((item) => append(parent, item));
      return;
    }
    parent.append(child.nodeType ? child : document.createTextNode(String(child)));
  }

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (value == null || value === false) continue;
      if (key === "class") node.className = value;
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key === "on") {
        for (const [eventName, listener] of Object.entries(value)) {
          node.addEventListener(eventName, listener);
        }
      } else if (key.startsWith("on") && typeof value === "function") {
        node.addEventListener(key.slice(2), value);
      } else if (["checked", "disabled", "selected", "readOnly", "required", "autofocus"].includes(key)) {
        node[key] = Boolean(value);
      } else if (key === "value") {
        node.value = value;
      } else {
        node.setAttribute(key, value === true ? "" : String(value));
      }
    }
    children.forEach((child) => append(node, child));
    return node;
  }

  function icon(name, className = "icon-inline") {
    // SVG elements created through document.createElement() live in the HTML
    // namespace. Server-rendered icons still work, but browser-created icons
    // then render as empty boxes (notably the workspace, search, and dialog
    // controls). Create both nodes in the SVG namespace so the canonical Flag
    // sprite behaves identically for static and dynamic UI.
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", className);
    svg.setAttribute("aria-hidden", "true");
    const use = document.createElementNS(SVG_NS, "use");
    use.setAttribute("href", `${SPRITE}#${name}`);
    svg.append(use);
    return svg;
  }

  // ── pure helpers ────────────────────────────────────────────────────────────
  // Every admin screen had re-implemented these with slightly different
  // behaviour. They stay pure so test/unit.test.js can cover them under plain
  // `node --test`, with no DOM and no new dependency.

  // Timestamps render in the browser's own locale, exactly as the four local
  // `fmt` helpers did. `locale`/`timeZone` are undefined in production and exist
  // only so a unit test can assert an exact string. Each call site passes the
  // fallback wording it already uses; unifying those four wordings is #151.
  // Timestamps in an operations UI answer "how stale is this?", and a reader
  // computes that from "7/22/2026, 9:25:00 AM" by doing arithmetic. So say it:
  // relative while the arithmetic is the point, absolute once it stops being —
  // past a week nobody counts days, they want the date.
  //
  // Seconds are never rendered. They were on every screen and are useful on none.
  // Absolute output is an explicit format rather than a bare toLocaleString, so an
  // EU and a US browser differ in order but never in precision or wording.
  const MINUTE = 60_000;
  const HOUR = 60 * MINUTE;
  const DAY = 24 * HOUR;
  const RELATIVE_LIMIT = 7 * DAY;
  const WHEN_FALLBACK = "Never";

  function toDate(value) {
    if (value == null || value === "") return null;
    const date = value instanceof Date ? value : new Date(value);
    return Number.isNaN(date.getTime()) ? null : date;
  }

  function absoluteWhen(date, { locale, timeZone, withTime }) {
    return date.toLocaleString(locale, {
      timeZone,
      day: "numeric",
      month: "short",
      year: "numeric",
      ...(withTime ? { hour: "2-digit", minute: "2-digit" } : {}),
    });
  }

  function relativeWhen(elapsed, locale) {
    // numeric:"auto" is what turns -1 day into "yesterday" rather than "1 day ago".
    const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
    if (elapsed < MINUTE) return "just now";
    if (elapsed < HOUR) return rtf.format(-Math.round(elapsed / MINUTE), "minute");
    if (elapsed < DAY) return rtf.format(-Math.round(elapsed / HOUR), "hour");
    return rtf.format(-Math.round(elapsed / DAY), "day");
  }

  // style: "auto" (relative while recent, then absolute) | "datetime" | "date" |
  // "time" (the clock alone).
  // `now` exists so the relative branch is testable without freezing the clock.
  function formatWhen(value, options = {}) {
    const { fallback = WHEN_FALLBACK, style = "auto", locale, timeZone, now } = options;
    const date = toDate(value);
    if (!date) return fallback;
    if (style === "date") return absoluteWhen(date, { locale, timeZone, withTime: false });
    if (style === "datetime") return absoluteWhen(date, { locale, timeZone, withTime: true });
    // The clock with no date, for a line reporting something that just happened
    // and is not re-rendered afterwards: a relative time there goes stale where it
    // stands, and the date would spend the line restating today.
    if (style === "time") return date.toLocaleString(locale, { timeZone, hour: "2-digit", minute: "2-digit" });
    const elapsed = (now == null ? Date.now() : now) - date.getTime();
    // A timestamp in the future is a clock skew, not a countdown: show the date.
    if (elapsed < 0 || elapsed >= RELATIVE_LIMIT) {
      return absoluteWhen(date, { locale, timeZone, withTime: false });
    }
    return relativeWhen(elapsed, locale);
  }

  // The rendered element, so "2 days ago" always carries the exact moment it
  // stands for and every timestamp on every screen is machine-readable.
  function timeWhen(value, options = {}) {
    const date = toDate(value);
    const text = formatWhen(value, options);
    if (!date) return el("span", { class: options.class }, text);
    return el("time", {
      class: options.class,
      datetime: date.toISOString(),
      title: absoluteWhen(date, { locale: options.locale, timeZone: options.timeZone, withTime: true }),
    }, text);
  }

  // One caller today (the attached-file help line on /admin/templates). It is
  // exported anyway so no screen has to reach for toLocaleString() itself: the
  // point of this module is that every locale-dependent format lives in one file.
  function formatCount(value, locale) {
    return Number(value).toLocaleString(locale);
  }

  // A page slug is a PATH: "client/q2 report" stays two route segments.
  function slugPath(slug) {
    return String(slug ?? "").split("/").map(encodeURIComponent).join("/");
  }

  // One path segment, where "/" IS data and must be escaped. Template names go
  // through this; slugs never do. Two names so the choice is a visible decision.
  function pathSegment(value) {
    return encodeURIComponent(String(value ?? ""));
  }

  // The single error-message policy for the whole admin UI: the server's own
  // words when it sent any, else the status. `text` is deliberately unused — a
  // non-JSON body (an nginx 502 page, say) must never reach a toast, so it is
  // kept on error.text instead. The parameter documents that decision.
  function describeError(status, body, text) {
    void text;
    const usable = (value) => (typeof value === "string" && value.trim() ? value : null);
    return (body && (usable(body.error) || usable(body.message))) || `Request failed (${status})`;
  }

  // ── the admin JSON API ──────────────────────────────────────────────────────

  // The bootstrap island every admin shell server-renders. Five copies of this
  // one line existed; more importantly, request() needs the CSRF token without
  // each screen having to wire it in. Missing island, bad JSON, or no document
  // (the Node case) all yield {} so a caller can destructure safely.
  let bootstrapCache = null;
  function bootstrap() {
    if (bootstrapCache) return bootstrapCache;
    let parsed = null;
    try {
      const island = typeof document === "undefined" ? null : document.getElementById("pages-bootstrap");
      parsed = JSON.parse((island && island.textContent) || "{}");
    } catch {
      parsed = null;
    }
    bootstrapCache = parsed && typeof parsed === "object" ? parsed : {};
    return bootstrapCache;
  }

  // The raw response text kept on a thrown error. Capped because it may be a
  // whole HTML error page from a proxy in front of the app.
  const ERROR_TEXT_CAP = 2000;

  // request — the ONE fetch wrapper. Four screens had four, disagreeing about
  // which key holds the message, whether the body is read as JSON or text,
  // whether CSRF rides along, and what an empty body resolves to.
  //
  // Contract, which no screen may extend or narrow:
  //   * method: explicit wins; otherwise a body means POST and no body means GET.
  //   * always same-origin credentials and `Accept: application/json`.
  //   * a body is a VALUE, never pre-stringified: this stringifies it and sets
  //     the content type.
  //   * X-CSRF-Token on every method but GET/HEAD, from the bootstrap island.
  //   * an empty or unparseable OK body resolves to {} — never null, so
  //     `const { pages } = await …` cannot throw.
  //   * a rejected response throws an Error carrying .message (see
  //     describeError), .status, .code, .body, .text, .method and .url. A
  //     network failure throws the same shape with status 0, code "network",
  //     and .cause set. That list is the whole seam; nothing else is promised.
  async function request(url, options = {}) {
    const { method, body, headers, csrf, signal } = options;
    const sendsBody = body !== undefined;
    const verb = method || (sendsBody ? "POST" : "GET");
    const sent = { Accept: "application/json" };
    if (sendsBody) sent["Content-Type"] = "application/json";
    if (verb !== "GET" && verb !== "HEAD") sent["X-CSRF-Token"] = String(csrf ?? bootstrap().csrf ?? "");
    Object.assign(sent, headers || {});

    let response;
    try {
      response = await fetch(url, {
        method: verb,
        credentials: "same-origin",
        headers: sent,
        body: sendsBody ? JSON.stringify(body) : undefined,
        signal,
      });
    } catch (cause) {
      const failure = new Error((cause && cause.message) || "Network request failed");
      failure.status = 0;
      failure.code = "network";
      failure.body = null;
      failure.text = "";
      failure.method = verb;
      failure.url = url;
      failure.cause = cause;
      throw failure;
    }

    const text = await response.text();
    let parsed = null;
    try {
      parsed = text ? JSON.parse(text) : null;
    } catch {
      parsed = null;
    }
    if (!response.ok) {
      const failure = new Error(describeError(response.status, parsed, text));
      failure.status = response.status;
      failure.code = (parsed && parsed.code) ?? null;
      failure.body = parsed;
      failure.text = String(text || "").slice(0, ERROR_TEXT_CAP);
      failure.method = verb;
      failure.url = url;
      throw failure;
    }
    return parsed == null ? {} : parsed;
  }

  // request bound to a path prefix — the only sanctioned per-screen short form,
  // so "/api/v1/admin" is written once per screen and no screen can grow its own
  // retry, swallow, or error-message behaviour on the way past.
  function requestScope(prefix) {
    const base = String(prefix ?? "");
    return (path, options) => request(`${base}${path ?? ""}`, options);
  }

  // The children of a loading panel, with NO attributes on any wrapper. The two
  // persistent preview panels (#preview-state, #tpl-preview-state) each sit
  // beside their own aria-live="polite" status line and are toggled and reused
  // by id, so they keep their hand-written wrapper and take only these children.
  // A whole state() wrapper would double-announce every preview and replace a
  // node other code holds by id, which is why errorState/emptyState have no
  // loading twin: an exported one had no caller on either side of this refactor.
  // level, for the same reason state() takes one: these panels are dropped inside
  // sections of different depths, and a hard-coded <h2> between two <h3>s inverts
  // the outline even while it is hidden.
  function loadingContent(title = "Loading", message, options = {}) {
    const nodes = [
      el("span", { class: "spinner spinner--panel", "aria-hidden": "true" }),
      el(options.level || "h2", { class: "state-panel__title" }, title),
    ];
    if (message) nodes.push(el("p", {}, message));
    return nodes;
  }

  // The heading level is a parameter because these panels are dropped inside
  // sections of different depths — the preview stage sits under an <h3>, so a
  // hard-coded <h2> inverted the heading order every time it was shown.
  function state(kind, title, message, action, options = {}) {
    const panel = el("div", { class: `state-panel state-panel--${kind}` });
    panel.append(el(options.level || "h2", { class: "state-panel__title" }, title));
    if (message) panel.append(el("p", {}, message));
    if (action) panel.append(action);
    return panel;
  }

  const errorState = (title, message, action, options) => state("error", title, message, action, options);
  const emptyState = (title, message, action, options) => state("empty", title, message, action, options);

  // A success toast is a receipt: it can go away on its own, because the screen
  // behind it already shows the result. An error toast is the only record that
  // something did NOT happen, and it used to be removed after the same four
  // seconds — so an operator who looked away lost the only evidence, with nothing
  // on screen changed to hint at it.
  const TOAST_SUCCESS_MS = 4000;
  const TOAST_ERROR_MS = 12000;

  function toast(message, options = {}) {
    let region = document.getElementById("pages-toast-region");
    if (!region) {
      region = el("div", {
        id: "pages-toast-region",
        class: "toast-region",
        "aria-live": "polite",
        "aria-atomic": "true",
      });
      document.body.append(region);
    }
    const tone = options.tone === "error" ? "error" : "success";
    const item = el("div", {
      class: `toast toast--${tone}`,
      role: tone === "error" ? "alert" : "status",
    }, el("span", { class: "toast__message" }, message));

    let timer = null;
    const dismiss = () => {
      if (timer) window.clearTimeout(timer);
      timer = null;
      if (item.isConnected) item.remove();
    };
    const arm = (ms) => {
      if (timer) window.clearTimeout(timer);
      timer = window.setTimeout(dismiss, ms);
    };

    if (tone === "error") {
      // Dismissible, and it survives a hover — reading a long message must not be
      // a race against its own timer.
      item.append(el("button", {
        class: "icon-action toast__close",
        type: "button",
        "aria-label": "Dismiss",
        title: "Dismiss",
        onclick: dismiss,
      }, icon("close")));
      item.addEventListener("mouseenter", () => { if (timer) window.clearTimeout(timer); timer = null; });
      item.addEventListener("mouseleave", () => arm(TOAST_ERROR_MS));
      item.addEventListener("focusin", () => { if (timer) window.clearTimeout(timer); timer = null; });
    }

    region.replaceChildren(item);
    arm(options.duration || (tone === "error" ? TOAST_ERROR_MS : TOAST_SUCCESS_MS));
    item.dismiss = dismiss;
    return item;
  }

  // Every screen's "the list would not load" panel, so there is one wording, one
  // retry, and no screen that simply reports the failure and stops. The index had
  // a Try again button; the template library and partner portals had none, and
  // portals said "Could not" where everything else says "Couldn't".
  function loadFailed(noun, error, retry) {
    // request() guarantees .message is a displayable sentence — a server's own
    // words when it sent any, "Request failed (503)" when it did not.
    const reason = (error && error.message) || "Something went wrong.";
    const guidance = error && error.status === 0
      ? "Check your connection and try again."
      : "Try again, or reload if it keeps happening.";
    return errorState(
      `Couldn't load ${noun}`,
      `${reason.replace(/\.?$/, ".")} ${guidance}`,
      typeof retry === "function"
        ? el("button", { class: "btn btn-primary", type: "button", onclick: () => retry() }, "Try again")
        : retry || null
    );
  }

  function makeDialog(options = {}) {
    const titleId = `pages-dialog-title-${++nextId}`;
    const descriptionId = `pages-dialog-description-${nextId}`;
    const dialog = el("dialog", {
      class: `ui-dialog${options.size ? ` ui-dialog--${options.size}` : ""}`,
      "aria-labelledby": titleId,
      // confirm() sends focus to Cancel, so without this a screen reader hears
      // "Reject version 6? dialog — Cancel button" and never the sentence saying
      // what rejecting does.
      "aria-describedby": options.description ? descriptionId : null,
    });
    const closeButton = el("button", {
      class: "icon-action ui-dialog__close",
      type: "button",
      "aria-label": options.closeLabel || "Close dialog",
      title: options.closeLabel || "Close dialog",
    }, icon("close"));
    const heading = el("div", { class: "ui-dialog__heading" },
      options.kicker ? el("p", { class: "overline" }, options.kicker) : null,
      el("h2", { id: titleId }, options.title || "Dialog"),
      options.description ? el("p", { id: descriptionId, class: "ui-dialog__description" }, options.description) : null);
    const body = el("div", { class: "ui-dialog__body" });
    const actions = el("div", { class: "ui-dialog__actions" });
    dialog.append(el("div", { class: "ui-dialog__surface" },
      el("header", { class: "ui-dialog__header" }, heading, closeButton),
      body,
      actions));

    let opener = null;
    let openerId = "";
    let beforeClose = options.beforeClose || null;
    let closing = false;

    async function requestClose(reason = "dismiss") {
      if (!dialog.open || closing) return false;
      if (beforeClose) {
        const allowed = await beforeClose(reason);
        if (!allowed) return false;
      }
      closing = true;
      dialog.close(reason);
      closing = false;
      return true;
    }

    closeButton.addEventListener("click", () => requestClose("close-button"));
    dialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      requestClose("escape");
    });
    if (options.closeOnBackdrop) {
      dialog.addEventListener("click", (event) => {
        if (event.target === dialog) requestClose("backdrop");
      });
    }
    dialog.addEventListener("close", () => {
      const target = opener && opener.isConnected
        ? opener
        : (openerId ? document.getElementById(openerId) : null);
      opener = null;
      openerId = "";
      if (target && target.isConnected && typeof target.focus === "function") target.focus();
      if (typeof options.onClose === "function") options.onClose(dialog.returnValue);
    });

    return {
      dialog,
      body,
      actions,
      closeButton,
      open(trigger) {
        opener = trigger || document.activeElement;
        openerId = opener && opener.id ? opener.id : "";
        if (!dialog.isConnected) document.body.append(dialog);
        if (!dialog.open) dialog.showModal();
      },
      close(value = "complete") {
        if (!dialog.open) return;
        closing = true;
        dialog.close(value);
        closing = false;
      },
      requestClose,
      setBeforeClose(handler) { beforeClose = handler; },
    };
  }

  // A value the server will never show again — a generated password, a minted
  // token. Partner portals had worked this out already ("Copy this now", the value
  // in a code block, copy buttons in the action bar); the page detail had nothing,
  // and confirmed a newly set password with a four-second toast, so if the
  // clipboard had been overwritten in between there was no record of it anywhere.
  // Shared so both screens promise the same thing in the same words.
  function credentialDialog({ title, kicker, description, value, notes, actions, closeLabel } = {}) {
    const modal = makeDialog({
      title: title || "Copy this now",
      kicker: kicker || "Copy this now",
      description: description
        || "This is the only time Pages will show this. It cannot be read back.",
      closeLabel: closeLabel || "Close",
      onClose() { modal.dialog.remove(); },
    });
    modal.body.append(
      el("pre", { class: "code-block", tabindex: "0" }, String(value)),
      ...[].concat(notes || []).filter(Boolean).map((note) =>
        (note && note.nodeType) ? note : el("p", { class: "note" }, note))
    );
    const copy = el("button", { class: "btn", type: "button" }, icon("copy"), "Copy");
    copy.addEventListener("click", async () => {
      try {
        await copyText(value);
        toast("Copied");
      } catch (error) {
        toast(error.message, { tone: "error" });
      }
    });
    modal.actions.append(
      copy,
      ...[].concat(actions || []).filter(Boolean),
      el("button", { class: "btn btn-primary", type: "button", onclick: () => modal.close("ack") }, "Done")
    );
    modal.open(document.activeElement);
    modal.body.querySelector("pre")?.focus();
    return modal;
  }

  function confirmDialog(options = {}) {
    return new Promise((resolve) => {
      let settled = false;
      const modal = makeDialog({
        title: options.title || "Confirm action",
        kicker: options.kicker || "Confirmation",
        description: options.message || "Are you sure?",
        closeLabel: "Close confirmation",
        onClose() {
          if (!settled) resolve(false);
          modal.dialog.remove();
        },
      });
      const cancel = el("button", { class: "btn", type: "button" }, options.cancelLabel || "Cancel");
      const proceed = el("button", {
        class: options.danger ? "btn btn-danger-solid" : "btn btn-primary",
        type: "button",
      }, options.confirmLabel || "Confirm");
      cancel.addEventListener("click", () => modal.requestClose("cancel"));
      proceed.addEventListener("click", () => {
        settled = true;
        resolve(true);
        modal.close("confirmed");
      });
      modal.actions.append(cancel, proceed);
      modal.open(options.trigger);
      cancel.focus();
    });
  }

  // What a button looked like before it went busy. A textContent snapshot lost
  // any icon inside the label, so "Delete page" and "Copy URL" came back as bare
  // text after a failed action. Keyed by node, so nothing is written to the DOM
  // and nothing leaks when the button is discarded by a re-render.
  const idleContent = new WeakMap();

  // Replacing a subtree drops focus to <body>, so a keyboard or screen-reader
  // operator loses their place on every action. Anything that rebuilds part of a
  // screen goes through here: remember what had focus by id, rebuild, then put
  // focus back on the same control if it still exists — or on the nearest thing
  // the caller nominates if the action removed it (approving the last pending
  // version removes the row that was focused, for instance).
  //
  // Id-based on purpose: the node itself is gone after a rebuild, so holding a
  // reference would restore focus to a detached element, which is the same as
  // losing it.
  // One status vocabulary for the whole admin. The same state used to render three
  // ways — "Live" as a dot on the index, a green lowercase "live" pill in the
  // template library, and a Title Case pill on the detail — and a version that was
  // approved but not serving fell through to the draft styling, so it looked like
  // work nobody had reviewed.
  //
  // Sentence case, always: `version.status` was printed raw next to Title Case
  // labels on the same screen. And `warning` finally uses the
  // --color-status-warning-* tokens, which the design system has always shipped
  // and this stylesheet never used.
  const STATUS_KINDS = {
    live: { tone: "live", label: "Live" },
    gated: { tone: "live", label: "Live · gated" },
    published: { tone: "live", label: "Published" },
    approved: { tone: "live", label: "Approved" },
    // "Current" is the expected state of a revision — colouring it makes every
    // healthy row shout. Only "Behind" is worth standing out.
    current: { tone: "draft", label: "Current" },
    set: { tone: "live", label: "Set" },
    pending: { tone: "pending", label: "Pending" },
    behind: { tone: "warning", label: "Behind" },
    warning: { tone: "warning", label: "Warning" },
    draft: { tone: "draft", label: "Draft" },
    neutral: { tone: "draft", label: "" },
    rejected: { tone: "error", label: "Rejected" },
    disabled: { tone: "error", label: "Disabled" },
    error: { tone: "error", label: "Error" },
  };

  // A pill. `label` overrides the vocabulary's own word where a screen has a more
  // specific one to say ("Live version 2"), but the tone always comes from `kind`.
  // One page header for every screen. The three list screens each grew their own:
  // the index a <header> with stats, the template library a <div> with the same
  // markup, and partner portals its <h1> and its primary action INSIDE a bordered
  // panel — so the product's title sat in a card on one screen out of four.
  function pageHeader({ id, title, intro, actions, stats } = {}) {
    const live = [].concat(actions || []).filter(Boolean);
    const tiles = [].concat(stats || []).filter(Boolean);
    return el("header", { class: "page-heading" },
      el("div", { class: "page-heading__row" },
        el("div", { class: "page-heading__copy" },
          el("h1", { id }, title),
          intro ? el("p", { class: "page-heading__intro" }, intro) : null),
        live.length ? el("div", { class: "page-heading__actions" }, ...live) : null),
      tiles.length ? el("div", { class: "stats", "aria-label": `${title} statistics` }, ...tiles) : null);
  }

  function statTile(value, singular, plural) {
    const count = Number(value) || 0;
    return el("div", { class: "stat" },
      el("strong", {}, String(count)),
      el("span", {}, count === 1 ? singular : (plural || `${singular}s`)));
  }

  function statusChip(kind, label, options = {}) {
    const entry = STATUS_KINDS[kind] || STATUS_KINDS.neutral;
    const classes = ["badge", `badge--${entry.tone}`];
    if (options.class) classes.push(options.class);
    return el("span", { class: classes.join(" "), title: options.title }, label || entry.label);
  }

  // The same vocabulary as a dot and a word, for dense rows where a pill per line
  // would be louder than the data.
  function statusDot(kind, label) {
    const entry = STATUS_KINDS[kind] || STATUS_KINDS.neutral;
    return el("span", { class: `status status--${entry.tone}` },
      el("span", { class: "status__dot", "aria-hidden": "true" }),
      label || entry.label);
  }

  function keepingFocus(rebuild, options = {}) {
    const active = document.activeElement;
    const hadFocus = Boolean(active) && active !== document.body;
    // A button that was disabled while its action ran has already been blurred by
    // the browser, so by the time the rebuild happens there is nothing left to
    // read off the document. `from` is how such a caller names where focus was.
    const id = hadFocus ? (active.id || null) : (options.from || null);
    const shouldRestore = hadFocus || Boolean(options.from);
    const result = rebuild();
    if (!shouldRestore) return result;
    const again = id ? document.getElementById(id) : null;
    if (again && typeof again.focus === "function") {
      again.focus();
      return result;
    }
    for (const selector of [].concat(options.fallback || [])) {
      const target = document.querySelector(selector);
      if (target && typeof target.focus === "function") {
        target.focus();
        return result;
      }
    }
    return result;
  }

  function setBusy(button, busy, label) {
    if (!button) return;
    if (busy) {
      // The guard stops a second busy call from snapshotting the spinner this
      // one just installed.
      if (!idleContent.has(button)) idleContent.set(button, Array.from(button.childNodes));
      button.disabled = true;
      button.setAttribute("aria-busy", "true");
      button.replaceChildren(
        el("span", { class: "spinner spinner--inline", "aria-hidden": "true" }),
        document.createTextNode(label || "Working…")
      );
    } else {
      button.disabled = false;
      button.removeAttribute("aria-busy");
      // An explicit restore label still wins: nine call sites pass one to change
      // what the button says after the action ("Enable page" after a disable).
      if (label) button.replaceChildren(document.createTextNode(label));
      else if (idleContent.has(button)) button.replaceChildren(...idleContent.get(button));
      else button.textContent = "Done";
      idleContent.delete(button);
    }
  }

  // field — the labelled form row. Two DOM shapes, on purpose:
  //
  //   wrap: "div"   div.field > label.field-label[for] + control + help
  //   wrap: "label" label.field[for] > span.field-label + control + help
  //
  // The label-wrapped shape folds any help text INTO the input's accessible
  // name, which is what nine inputs on /admin and the page detail read as
  // today. Collapsing the two would silently rename them, so the choice stays
  // explicit at every call site; unifying the markup belongs to its own pass.
  //
  // `control` takes a node or an array of nodes (the copy-URL row is an input
  // plus a button). `help` takes a string, or a live node appended by identity —
  // the template upload dialog mutates its help line after the file is read.
  function field(options = {}) {
    const { id, label, control, help, wrap = "div", className } = options;
    const helpNode = !help ? null : (help.nodeType ? help : el("span", { class: "field-help" }, help));
    const controls = Array.isArray(control) ? control : [control];
    const classes = `field${className ? ` ${className}` : ""}`;
    if (wrap === "label") {
      return el("label", { class: classes, for: id },
        el("span", { class: "field-label" }, label),
        ...controls,
        helpNode);
    }
    return el("div", { class: classes },
      el("label", { class: "field-label", for: id }, label),
      ...controls,
      helpNode);
  }

  // runAction — confirm, then busy, then act, then say what happened. admin.js's
  // `mutate` and portals.js's `act` were two thirds of this each, and templates
  // and welcome hand-rolled the rest, which is how one of them ended up
  // restoring a button's label to the wrong word.
  //
  // Failure is always a toast and never a throw: one error voice for the whole
  // admin UI.
  //
  // Where the reload lives decides whether the button is clickable during it:
  //
  //   run does the reload      the re-render detaches the button before the
  //                            finally, so it is never restored (portals.js)
  //   keepBusy + reload after  the button stays disabled and spinning until the
  //                            caller's own reload or navigation replaces it
  //   neither                  the button comes back the instant `run` resolves
  //
  // keepBusy exists because "toast, THEN reload" is the ordering these screens
  // shipped with, and doing the reload after runAction returns would otherwise
  // re-enable the button for the whole round trip — a second click, a second
  // POST. On failure the button is always restored, whatever keepBusy says.
  async function runAction(options = {}) {
    const { button, run, busyLabel, idleLabel, success, failure, keepBusy } = options;
    const ask = options.confirm;
    if (ask) {
      const approved = await confirmDialog({ trigger: button, ...ask });
      if (!approved) return { ok: false, cancelled: true, result: null, error: null };
    }
    setBusy(button, true, busyLabel);
    let succeeded = false;
    try {
      const result = await run();
      succeeded = true;
      if (typeof success === "function") toast(success(result));
      else if (success) toast(success);
      return { ok: true, cancelled: false, result, error: null };
    } catch (error) {
      const message = typeof failure === "function"
        ? failure(error)
        : failure ? `${failure}: ${error.message}` : error.message;
      toast(message, { tone: "error" });
      return { ok: false, cancelled: false, result: null, error };
    } finally {
      // isConnected: a button a re-render inside `run` already replaced must not
      // be resurrected. keepBusy: one the CALLER is about to replace must stay
      // busy until it does.
      if (button && button.isConnected && !(succeeded && keepBusy)) setBusy(button, false, idleLabel);
    }
  }

  // The old-school path: an off-screen textarea and execCommand. Still the only
  // thing that works when the async clipboard is refused rather than missing.
  function copyBySelection(text) {
    const input = el("textarea", { class: "clipboard-fallback", readonly: "", value: text });
    document.body.append(input);
    input.select();
    let copied = false;
    try {
      copied = document.execCommand("copy");
    } catch {
      copied = false;
    }
    input.remove();
    return copied;
  }

  // navigator.clipboard.writeText does not only go missing — it REJECTS, and often:
  // on a non-secure origin, when the tab lost focus mid-await, in Safari without a
  // fresh user gesture, under some enterprise policies. The old code only fell back
  // when the API was absent, so a rejection went straight to the caller, which
  // toasted error.message — and that message is the browser's own exception text:
  //
  //   Failed to execute 'writeText' on 'Clipboard': Write permission denied.
  //
  // Two fixes in one place. Fall back on a rejection as well as on an absence, and
  // when nothing works, fail with a sentence written for a person. Every caller
  // toasts error.message, so fixing the message here fixes all of them at once
  // without a screen having to know anything about clipboards.
  async function copyText(value) {
    const text = String(value);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(text);
        return;
      } catch {
        // Refused, not absent. Try the older path before giving up.
      }
    }
    if (copyBySelection(text)) return;
    throw new Error("Couldn't copy automatically. Select the value and copy it with your keyboard.");
  }

  return {
    el,
    icon,
    bootstrap,
    request,
    requestScope,
    describeError,
    formatWhen,
    timeWhen,
    formatCount,
    slugPath,
    pathSegment,
    field,
    runAction,
    loadingContent,
    errorState,
    emptyState,
    toast,
    loadFailed,
    makeDialog,
    confirmDialog,
    credentialDialog,
    setBusy,
    keepingFocus,
    pageHeader,
    statTile,
    statusChip,
    statusDot,
    copyText,
  };
});
