// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Partner portals screen (/admin/portals).
//
// Everything here is a human decision by construction: lib/portals.js refuses any
// actor that is not an admin, and there is no agent-facing equivalent of a single
// call this file makes. Two consequences shape the UI:
//
//   1. Adding a page with no client password of its own RECLASSIFIES it from
//      staff-only to client-readable. The API reports that; this screen says so
//      BEFORE the click and confirms it after, because the moment of adding is the
//      only moment the person deciding is present.
//   2. A portal password exists in plaintext exactly once, in the response that
//      mints it. It cannot be read back, so that one render has to be a dialog the
//      admin dismisses deliberately — not a toast that scrolls away.

// describeMember — the warnings an admin needs about one membership row, in the
// order they matter: first anything that means a partner cannot open it at all,
// then anything that means it opens but behaves less well than expected. Every
// input comes from the API, so this screen and the partner's own index can never
// disagree about what they see.
function describeMember(member) {
  const warnings = [];
  if (member.page_deleted) {
    warnings.push({ kind: "blocked", text: "Page deleted — a partner cannot open this. Remove it from the portal." });
  } else if (!member.published) {
    warnings.push({ kind: "blocked", text: "Nothing published yet — hidden from this partner until there is." });
  } else if (member.disabled) {
    warnings.push({ kind: "blocked", text: "Taken down — hidden from this partner while it is disabled." });
  }
  if (!member.page_deleted && !member.has_password) {
    warnings.push({ kind: "note", text: "No password of its own: this portal is what makes it readable." });
  }
  // Every published member shows a Page menu when opened through this portal —
  // themed via the template block or Pages' built-in control, raw via the
  // injected control (#125) — so a menu warning would only ever mislead.
  return {
    openable: !member.page_deleted && !member.disabled && !!member.published,
    warnings,
  };
}

// portalIdFromSearch — the selected portal as the URL states it. Only a bare id
// is honoured: anything else is somebody else's query string, or a hand-edited
// one, and the screen falls back rather than requesting /portals/<junk>.
function portalIdFromSearch(search) {
  const value = new URLSearchParams(String(search || "")).get("portal");
  return /^\d+$/.test(value || "") ? value : null;
}

// resolveSelection — which portal a list and a wanted id agree on. A wanted id
// that is not in the list (a retired portal still in a bookmark, a stale
// history entry) falls back to the first, because a screen showing a list with
// no detail under it reads as broken.
function resolveSelection(portals, wanted) {
  const list = portals || [];
  if (list.some((portal) => String(portal.id) === String(wanted))) return wanted;
  return list[0] ? list[0].id : null;
}

// ── membership order, without a number ──────────────────────────────────────
// The order used to be a zero-based integer an admin typed into a dialog: to
// move a dashboard up you guessed a value, and two rows could legitimately share
// 0 (the API defaults to it, and both the picker and the link audit sent it), at
// which point the list ordered itself by title and no number an admin typed
// could change that. These four helpers are the whole model behind the
// move-up/move-down controls that replaced it. They are pure so test/unit.test.js
// can cover the arithmetic under plain `node --test`, with no DOM.

// The API's own ceiling. It is lib/portals.MAX_SORT_ORDER, and the two are
// asserted equal in test/unit.test.js — the only machine-checked guard on this
// number used to be the deleted input's native max="9999", so without that
// assertion nothing would fail if they drifted apart. Appending must never
// propose a value the write would reject, nor one already taken.
const MAX_SORT_ORDER = 9999;

// sortOrderWrites — the smallest set of updates that makes the list's stored
// order match its position: one write per row not already numbered by its index.
// Renumbering rather than swapping two values is what makes a move work on a
// list that shares a sort_order across rows — a swap of 0 and 0 is a no-op, and
// that is exactly the state the old numeric field left behind.
function sortOrderWrites(list) {
  const writes = [];
  (list || []).forEach((member, index) => {
    if (Number(member.sort_order) !== index) writes.push({ page_id: member.page_id, sort_order: index });
  });
  return writes;
}

// describeMove — what an admin is TOLD after a move. Once the integer is gone
// the order is not written down anywhere on the row, so a keyboard operator has
// nothing to read that confirms the press did something: this sentence is that
// confirmation, spoken through the toast's live region. It names a neighbour
// rather than a position, because the neighbour is what the person was aiming at.
function describeMove(list, index, direction) {
  const member = (list || [])[index];
  if (!member) return "";
  const name = member.display_title;
  if (list.length < 2) return `${name} is the only page in this portal.`;
  if (index === 0) return `${name} is now first.`;
  if (index === list.length - 1) return `${name} is now last.`;
  const neighbour = list[direction === "up" ? index + 1 : index - 1].display_title;
  return direction === "up" ? `${name} moved above ${neighbour}.` : `${name} moved below ${neighbour}.`;
}

// planMove — one press, resolved against the list as it stands. Returns null for
// a press that cannot go anywhere (the first row's up, the last row's down, a
// row a concurrent change has already removed) so the caller sends nothing.
function planMove(members, pageId, direction) {
  const list = Array.isArray(members) ? members.slice() : [];
  const index = list.findIndex((member) => String(member.page_id) === String(pageId));
  const target = direction === "up" ? index - 1 : index + 1;
  if (index < 0 || target < 0 || target >= list.length) return null;
  const moved = list[index];
  list[index] = list[target];
  list[target] = moved;
  return {
    members: list,
    index: target,
    writes: sortOrderWrites(list),
    announcement: describeMove(list, target, direction),
  };
}

// memberCaption — the one sentence over the member table, which is all that
// replaced the numeric column's help text ("Lowest first. The home page
// always sorts to the top."). Its first draft read "in this order — with the home
// dashboard first" directly above a table that routinely shows the home row
// second or third, because the arrows really do move it and really do report
// success. The claim belongs to the PARTNER's view, not to this table, and it
// must not be conditioned on position: db.getPortalPages orders by home_page_id
// BEFORE sort_order, so the home row can sit anywhere here and the partner still
// opens it first. Pure so test/unit.test.js can pin the home-not-first wording.
function memberCaption({ count, openable, hasHome }) {
  const noun = count === 1 ? "page" : "pages";
  // "in this order" only earns its place when there are two rows to order, and it
  // is said about every row rather than about the visible subset — the old
  // wording attached the order to the 2 of 3 that a partner can open.
  const head = count > 1
    ? `${count} ${noun} in this order, ${openable} of them visible to this partner.`
    : `${count} ${noun}, ${openable === count ? "visible" : "not visible"} to this partner.`;
  const home = hasHome
    ? " The partner's own index always shows the home page first, wherever it sits here."
    : "";
  // The shortcut was named only in each control's `title`, which renders on hover
  // — so the operator who most needs it was the one who never saw it. Said once
  // here, in plain sight, as well as on each control via aria-keyshortcuts.
  const shortcut = count > 1 ? " Alt+↑ and Alt+↓ move a row from anywhere in it." : "";
  return `${head}${home}${shortcut}`
    + " The list is read live: changes here reach them on their next page load, with nothing redeployed.";
}

// nextSortOrder — where a newly added member lands now that nothing asks for a
// number: after everything already there. null when "after everything" is past
// the ceiling. Clamping to 9999 there — which this did — appended straight into a
// TIE with the row already holding 9999, and a shared sort_order is the whole
// defect #173 exists to remove: lib/portals.get falls through to the title and
// nothing an admin does moves either row.
function nextSortOrder(members) {
  const orders = (members || [])
    .map((member) => Number(member.sort_order))
    .filter((order) => Number.isInteger(order) && order >= 0);
  if (!orders.length) return 0;
  const next = Math.max(...orders) + 1;
  return next > MAX_SORT_ORDER ? null : next;
}

// appendPlan — the writes and the sort_order that one "add a page" needs.
// Almost always no writes at all. In the one case where the list already reaches
// the ceiling there is no free number above it, so the list is renumbered by
// position first — exactly what a move does — and the new row lands after it.
function appendPlan(members) {
  const list = members || [];
  const next = nextSortOrder(list);
  if (next != null) return { writes: [], sort_order: next };
  return { writes: sortOrderWrites(list), sort_order: Math.min(list.length, MAX_SORT_ORDER) };
}

// appendAllPlan — the writes and the sort_orders one "Add all N" needs. Calling
// appendPlan N times would hand every row the SAME number: the list it plans
// against is state.detail.members, which does not change until the screen
// reloads, so N appends all read the same maximum. A shared sort_order is the
// whole defect #173 exists to remove — lib/portals.get falls through to the
// title and nothing an admin does moves either row. Planned once, against the
// list as it stands, so the N rows land after it in the order they are listed.
function appendAllPlan(members, count) {
  const list = members || [];
  const total = Math.max(0, Math.trunc(Number(count) || 0));
  const first = nextSortOrder(list);
  if (first != null && first + total - 1 <= MAX_SORT_ORDER) {
    return { writes: [], sort_orders: Array.from({ length: total }, (_, index) => first + index) };
  }
  // No room for all of them above what is already there, so the list is
  // renumbered by position first — exactly what appendPlan does for one — and
  // they land after it. A portal long enough to reach the ceiling even then
  // ties at it, which is what the API's own maximum leaves available.
  const orders = [];
  for (let index = 0; index < total; index += 1) orders.push(Math.min(list.length + index, MAX_SORT_ORDER));
  return { writes: sortOrderWrites(list), sort_orders: orders };
}

// auditCaption — the link-audit table's ACCESSIBLE NAME, and nothing else. A
// table's caption is its name, and this table sits directly under the membership
// table with the same two column headers, so it is the only thing telling a
// screen reader which of the two it has landed in.
//
// It carried a second sentence — "Adding one is the same decision as Add a
// dashboard, one click shorter" — which was the rationale for building this
// screen at all, addressed to the next developer rather than to an admin, and a
// caption is read out on entry. Cut. What is left is rendered sr-only, the way
// the three captions in templates.js and welcome.js's are: on screen the count
// is simply the number of rows, it restated the paragraph directly above it, and
// `.operation-table caption` pads its text one step in, which gave the section a
// third left edge under a heading and a paragraph that share the first two.
function auditCaption(count) {
  const noun = count === 1 ? "page" : "pages";
  const verb = count === 1 ? "is" : "are";
  return `${count} linked ${noun} ${verb} not in this portal.`;
}

// describeAdded — what an admin is TOLD after an add made from the audit. The
// row simply vanished before: no toast at all, on the one action here that can
// change who is allowed to read a page. The reclassification is confirmed by
// NAME because "Add all 3" can reclassify some of the three and not the others,
// and the row that would have carried the news is gone by the time it arrives.
function describeAdded(results) {
  const list = (results || []).filter(Boolean);
  // Nothing landed is not "Added 0 dashboards." — it is nothing to say, which is
  // what describePartialAdd asks this for when a batch fails on its first write.
  if (!list.length) return "";
  const head = list.length === 1 ? "Page added." : `Added ${list.length} pages.`;
  const names = list
    .filter((result) => result.reclassifies_staff_only)
    .map((result) => (result.member && result.member.display_title) || "A page");
  if (!names.length) return head;
  const joined = names.length === 1
    ? names[0]
    : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
  return `${head} ${joined} ${names.length === 1 ? "is" : "are"} now readable with this portal's password.`;
}

// describePartialAdd — what an admin is told when "Add all N" fails PART WAY.
// The adds are sequential and each one is its own committed transaction, so a
// failure on page 2 leaves page 1 a real member — and the re-read that follows
// has already taken away the audit row that would otherwise have carried the
// news. Reporting only the failure therefore drops the single sentence this
// screen exists to say out loud: a staff-only dashboard just became readable by
// everyone holding the portal's password. That is the file's opening invariant
// — said before the click, CONFIRMED AFTER — and it does not stop applying
// because the batch around it stopped early.
//
// One toast rather than two: the failure and what survived it are one outcome,
// and a green "Added 1" stacked beside a red error can be read apart or dismissed
// apart.
function describePartialAdd(message, results) {
  const added = describeAdded(results);
  if (!added) return message;
  return `${message} — the rest were not added. ${added}`;
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    describeMember, portalIdFromSearch, resolveSelection,
    sortOrderWrites, describeMove, planMove, nextSortOrder, appendPlan, appendAllPlan,
    memberCaption, auditCaption, describeAdded, describePartialAdd, MAX_SORT_ORDER,
  };
}

if (typeof document !== "undefined") (function () {
  const UI = window.PagesUI;
  const { el, errorState, emptyState, toast, makeDialog, confirmDialog, setBusy, copyText, keepingFocus, pageHeader, statTile, loadFailed } = UI;
  const { field, runAction, timeWhen, icon } = UI;
  const boot = UI.bootstrap();
  const api = UI.requestScope("/api/v1/admin");
  const app = document.getElementById("app");

  const state = { portals: [], selectedId: null, detail: null };

  // Two counters, not one. detailRequest guards against a slower earlier detail
  // request landing after a newer one and rendering portal A's dashboards under
  // portal B's highlighted row; every path that fetches a detail — load(),
  // select(), popstate — takes a ticket from it, the way templates.js guards its
  // previews. listRequest is separate because load() also refreshes the LIST, and
  // a click made during a reload supersedes only the detail: sharing one counter
  // discarded the freshly fetched list too, which left a retired portal listed,
  // counted and clickable after "Portal retired."
  let listRequest = 0;
  let detailRequest = 0;

  // The selected portal is route state: a reload, a bookmark, or a link to "the
  // Fabrikam portal" has to come back to the same detail, the way ?workspace= does
  // on the index.
  function setPortalInURL(id, { replace = false } = {}) {
    const url = new URL(window.location.href);
    if (id == null) url.searchParams.delete("portal");
    else url.searchParams.set("portal", String(id));
    // Re-selecting the portal already in the URL — every mutation on this screen
    // calls select() when it is done — must not stack entries that go nowhere.
    if (url.href === window.location.href) return;
    window.history[replace ? "replaceState" : "pushState"]({}, "", url);
  }

  // "—" is this screen's fallback wording, passed per call; #151 owns unifying
  // the four wordings across the admin UI.

  function copyButton(value, label) {
    return el("button", {
      class: "btn btn-sm",
      type: "button",
      onclick: async () => {
        try {
          await copyText(value);
          toast(`${label} copied.`);
        } catch (error) {
          toast(error.message, { tone: "error" });
        }
      },
    }, `Copy ${label.toLowerCase()}`);
  }

  // formDialog — the shape every mutation dialog here shares: fields, one primary
  // action, and an inline error rather than a toast, so a rejected value stays next
  // to the field that caused it.
  //
  // The fields live in a real <form>, so Enter in any input submits. The primary
  // action stays in the dialog's action bar — outside the form, where every other
  // dialog in the admin puts it — and is tied back to the form by its `form`
  // attribute. That association is what makes Enter work at all: implicit
  // submission needs a submit button the form OWNS unless the form holds exactly
  // one field, and Edit membership and Add a dashboard hold several. It also
  // leaves one submit path, with one busy state, for both the pointer and the
  // keyboard.
  //
  // A real submitter also turns on native constraint validation, which is a
  // second gate whose message is the browser's own bubble rather than this
  // dialog's error node. That is kept on purpose: before it, a type="button"
  // click bypassed min/max entirely and `Number(order.value) || 0` posted 99999
  // to an API that accepts 0-9999. The bubble names the field and points at it,
  // which is better than an error line at the foot of the form — so the rule is
  // "the browser refuses a value the field itself declares impossible, this node
  // carries everything else", and admin-portals.spec.js pins the out-of-range
  // case so the next change to those attributes is caught.
  let formSeq = 0;
  function formDialog({ title, description, closeLabel, fields, confirmLabel, onConfirm }) {
    const modal = makeDialog({
      title,
      kicker: "Partner portals",
      description,
      // A fixed label, never derived from the title: a title carries a partner
      // name and a field name, so a derived one reads badly to a screen reader and
      // collides with the labels of the fields inside the dialog.
      closeLabel: closeLabel || "Close dialog",
      onClose() {
        modal.dialog.remove();
      },
    });
    const error = el("p", { class: "note note--warning", role: "alert", hidden: true });
    const formId = `portal-dialog-form-${++formSeq}`;
    const form = el("form", { id: formId, class: "form-stack" }, ...fields, error);
    modal.body.append(form);
    const cancel = el("button", { class: "btn", type: "button", onclick: () => modal.requestClose("cancel") }, "Cancel");
    const proceed = el("button", { class: "btn btn-primary", type: "submit", form: formId }, confirmLabel);
    // setBusy disables the button while a request is in flight, and a disabled
    // default button also blocks implicit submission — but only while the form
    // owns it, so the guard is stated here too rather than left to that.
    let submitting = false;
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (submitting) return;
      submitting = true;
      error.hidden = true;
      setBusy(proceed, true);
      try {
        await onConfirm();
        modal.close("done");
      } catch (failure) {
        // Unhidden first, then written: a live region mutated while it is out of
        // the accessibility tree announces nothing. Then scrolled to, because the
        // dialog body scrolls and the refusal lands BELOW the fold at common short
        // viewports — at 1440x600 this form is 447px in a 357px body, so pressing
        // the button looked like it did nothing at all.
        error.hidden = false;
        error.textContent = failure.message;
        error.scrollIntoView({ block: "nearest" });
      } finally {
        submitting = false;
        setBusy(proceed, false, confirmLabel);
      }
    });
    modal.actions.append(cancel, proceed);
    // The primary action is exposed because a dialog whose fields are still
    // loading has to be able to turn it off: an enabled action over a control
    // with nothing in it is a dead end.
    modal.proceed = proceed;
    modal.open();
    return modal;
  }

  // The one render of a freshly minted credential.
  function showCredential(portal, password) {
    const modal = makeDialog({
      title: `Password for ${portal.name}`,
      kicker: "Copy this now",
      description:
        "This is the only time Pages will show this password. It cannot be read back — losing it means rotating, which signs every partner out.",
      closeLabel: "Close password",
      onClose() {
        modal.dialog.remove();
      },
    });
    modal.body.append(
      el("pre", { class: "code-block", tabindex: "0" }, password),
      el("p", { class: "note" },
        "Send it separately from the link, and only to the people who should have it: one password opens every page in this portal, and a forwarded password is as good as the holder's."),
      el("p", { class: "note" }, `Portal link: ${portal.url}`)
    );
    modal.actions.append(
      copyButton(password, "Password"),
      copyButton(portal.url, "Link"),
      el("button", { class: "btn btn-primary", type: "button", onclick: () => modal.close("ack") }, "Done")
    );
    modal.open();
  }

  async function load({ keepSelection = true } = {}) {
    const listId = ++listRequest;
    const detailId = ++detailRequest;
    // Nothing is selected on the first load, so the URL is what the person asked
    // for. The whole /pages index is no longer fetched here: it exists only to
    // fill the add-dashboard picker, which now fetches it when it opens.
    const previous = keepSelection ? (state.selectedId ?? portalIdFromSearch(window.location.search)) : null;
    const list = await api("/portals");
    // Only a newer load() can invalidate this list, and a newer load() is fetching
    // one of its own. A selection made in the meantime is not a reason to keep
    // showing a portal the server no longer lists.
    if (listId !== listRequest) return;
    state.portals = list.portals || [];
    if (detailId !== detailRequest) {
      // A click during the reload owns the detail now, and its own render follows
      // when it lands. Show the refreshed list rather than sitting on the stale one,
      // and drop a detail whose portal the refreshed list no longer holds — that
      // panel's Rotate and Retire act on something the server has already retired.
      const shown = state.detail && state.detail.portal ? state.detail.portal.id : null;
      if (shown != null && !state.portals.some((portal) => String(portal.id) === String(shown))) state.detail = null;
      render();
      return;
    }
    state.selectedId = resolveSelection(state.portals, previous);
    const detail = state.selectedId != null ? await api(`/portals/${state.selectedId}`) : null;
    if (detailId !== detailRequest) return;
    state.detail = detail;
    // replace, not push: arriving at the screen is not a navigation, and a URL
    // naming a portal that no longer exists is corrected in place.
    setPortalInURL(state.selectedId, { replace: true });
    render();
  }

  // `route: false` is the popstate case — the URL already says this, so writing
  // it again would push back the entry the Back button just left.
  async function select(id, { route = true } = {}) {
    const requestId = ++detailRequest;
    // The highlight follows the latest click immediately; only the detail waits.
    state.selectedId = id;
    let detail;
    try {
      detail = await api(`/portals/${id}`);
    } catch (error) {
      // A superseded failure is not this selection's failure, so it must not
      // reach the toast the caller attached.
      if (requestId !== detailRequest) return;
      throw error;
    }
    if (requestId !== detailRequest) return;
    state.detail = detail;
    if (route) setPortalInURL(id);
    render();
  }

  // ── create ────────────────────────────────────────────────────────────────
  function newPortalDialog() {
    const name = el("input", { id: "portal-name", type: "text", maxlength: "100", autocomplete: "off" });
    const slug = el("input", { id: "portal-slug", type: "text", maxlength: "64", autocomplete: "off" });
    // Deriving the slug from the name is a convenience, not a rule: it lands in the
    // partner's bookmark permanently, so it stays editable and is never rewritten
    // once touched.
    let slugTouched = false;
    slug.addEventListener("input", () => { slugTouched = true; });
    name.addEventListener("input", () => {
      if (slugTouched) return;
      slug.value = name.value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 64);
    });
    formDialog({
      title: "New partner portal",
      closeLabel: "Close new portal",
      description: "Pages generates the password and shows it once, on the next screen.",
      fields: [
        field({ id: "portal-name", label: "Partner name", control: name, help: "What the partner sees at the top of their page." }),
        field({
          id: "portal-slug",
          label: "URL slug",
          control: slug,
          help: `Their link will be ${boot.contentOrigin || ""}/portal/<slug>. Lower case letters, digits, - and _.`,
        }),
      ],
      confirmLabel: "Create portal",
      onConfirm: async () => {
        const created = await api("/portals", { body: { name: name.value, slug: slug.value } });
        await load({ keepSelection: false });
        await select(created.portal.id);
        showCredential(created.portal, created.password);
      },
    });
  }

  // ── reordering ────────────────────────────────────────────────────────────
  // One chain for the whole screen. Two presses in quick succession used to be
  // the only way to move a dashboard two places, and computing both off the same
  // captured list would write the second one against a stale order. Queueing
  // instead of dropping means a held key still moves the row once per press, and
  // every plan is made against state.detail as it stands when its turn comes.
  let reorderChain = Promise.resolve();

  function moveButtonId(member, direction) {
    return `portal-move-${direction}-${member.page_id}`;
  }

  // The Flag sprite ships one chevron and it points down; the up control is the
  // same symbol turned about the centre of its own 24×24 viewBox. The rotation is
  // an SVG transform on the <use> rather than a CSS rule because shell.css owns
  // no rotation utility, this is geometry rather than style, and #173's scope is
  // this file. Both directions declare the viewBox so the two icons cannot
  // disagree about the coordinate system the rotation happens in.
  function chevron(direction) {
    const svg = icon("chevron-down");
    svg.setAttribute("viewBox", "0 0 24 24");
    if (direction === "up") svg.firstChild.setAttribute("transform", "rotate(180 12 12)");
    return svg;
  }

  async function runMove(portal, member, direction) {
    // The order the person is looking at, not the one captured when the row was
    // built: an earlier queued move, or any other action, may have changed it.
    if (!state.detail || String(state.detail.portal.id) !== String(portal.id)) return;
    const plan = planMove(state.detail.members, member.page_id, direction);
    if (!plan) return;
    // Where the operator actually was. Alt+↑/↓ fires from anywhere in the row, so
    // this is often Edit or Remove rather than an arrow, and the rebuild must not
    // relocate them — least of all onto the arrow with the OPPOSITE effect, where
    // the next Enter or Space would undo the move they just asked for.
    const wasFocused = document.activeElement ? document.activeElement.id : "";
    await runAction({
      // No `button`: setBusy would swap a 2rem icon control for a spinner and the
      // word "Working…", and the row it belongs to is about to be rebuilt anyway.
      success: plan.announcement,
      run: async () => {
        // Sequential on purpose: each write is its own audited transaction, and a
        // renumbering that raced with itself would interleave two orders.
        try {
          for (const write of plan.writes) {
            await api(`/portals/${portal.id}/pages/update`, {
              body: { page_id: write.page_id, sort_order: write.sort_order },
            });
          }
        } catch (error) {
          // A renumber that fails part-way leaves the stored order between the two
          // — on the very list this feature exists to repair, that can be two rows
          // back on one sort_order. Re-read regardless: a screen still showing the
          // order it HOPED for is the worse failure, because the next press plans
          // against what it can see, and the reload is what makes that recovery
          // possible at all. It is self-healing precisely because a move renumbers
          // by position instead of swapping two values, so the next successful
          // move writes every row that is out of place. The write's own error is
          // what reaches the toast; a re-read that also fails must not mask it.
          await select(portal.id).catch(() => {});
          throw error;
        }
        await select(portal.id);
      },
    });
    // render() restores focus by id, but the control was pressed and then the row
    // was replaced, so there is nothing left on the document to read it off —
    // and the button just pressed may now be the disabled end of the list. Go back
    // to where the operator actually was if it survived the rebuild, then to the
    // same direction, then to the other one, so nobody is dropped on <body> (#146)
    // and nobody who pressed Alt+↑ from Edit is silently moved onto "Move down".
    for (const id of [wasFocused, moveButtonId(member, direction), moveButtonId(member, direction === "up" ? "down" : "up")]) {
      const control = id ? document.getElementById(id) : null;
      if (control && !control.disabled) {
        control.focus();
        return;
      }
    }
  }

  function queueMove(portal, member, direction) {
    // The chain always settles clean: runAction reports its own failure, and a
    // rejected link left in place would take the next press down with it.
    reorderChain = reorderChain.then(() => runMove(portal, member, direction)).catch(() => {});
  }

  // moveControl — one end of the pair. Disabled at the end of the list it cannot
  // leave, and named per row: a table of identical "Move up" buttons tells a
  // screen reader nothing about which dashboard it would move.
  function moveControl(portal, members, member, direction) {
    const index = members.findIndex((candidate) => String(candidate.page_id) === String(member.page_id));
    const target = direction === "up" ? index - 1 : index + 1;
    const possible = index >= 0 && target >= 0 && target < members.length;
    // "Move up: Taken down", not "Move Taken down up", which doubles the word on
    // any title ending in a direction. The row is still named, because a table of
    // identical "Move up" buttons tells a screen reader nothing about which
    // dashboard it would move.
    const label = `Move ${direction}: ${member.display_title}`;
    const button = el("button", {
      id: moveButtonId(member, direction),
      class: "icon-action",
      type: "button",
      disabled: !possible,
      "aria-label": label,
      // Announced, rather than only drawn on hover the way `title` is. The
      // aria-label deliberately stays free of it so the NAME stays short.
      "aria-keyshortcuts": direction === "up" ? "Alt+ArrowUp" : "Alt+ArrowDown",
      title: possible ? `${label} (Alt+${direction === "up" ? "↑" : "↓"})` : label,
      "data-testid": moveButtonId(member, direction),
    }, chevron(direction));
    if (possible) button.addEventListener("click", () => queueMove(portal, member, direction));
    return button;
  }

  // ── membership ────────────────────────────────────────────────────────────
  // addMember — one "add a page", from the dialog or from the link audit.
  // Nothing asks for a position any more: the new row goes after everything
  // already there, and appendPlan renumbers the list first in the one case where
  // there is no free number left above it.
  async function addMember(portal, body) {
    const plan = appendPlan(state.detail ? state.detail.members : []);
    for (const write of plan.writes) {
      await api(`/portals/${portal.id}/pages/update`, {
        body: { page_id: write.page_id, sort_order: write.sort_order },
      });
    }
    return api(`/portals/${portal.id}/pages`, { body: { ...body, sort_order: plan.sort_order } });
  }

  function addPageDialog(portal) {
    const picker = el("select", { id: "portal-add-page" },
      el("option", { value: "" }, "Loading pages…"));
    // What the picker is doing is said in prose beside it, not only in the text of
    // a disabled <option>. Disabled option text composites to about 2.3:1 in the
    // light theme (3.2:1 in dark), so it is legible in one theme and nearly
    // invisible in the other — it cannot be the only carrier of a state. role
    // status also means the list ARRIVING is announced, rather than a select
    // silently gaining 38 options under a focus that never moved.
    const pickerStatus = el("p", {
      class: "field-help",
      role: "status",
      "data-testid": "portal-add-page-status",
    }, "Loading the page list…");
    // Swallowing this list's failure left the picker holding one option reading
    // "Choose a page…" with nothing to choose and no reason given, on a
    // screen whose whole job is telling an admin what a click will do. Say it,
    // next to the control it is about, and offer the retry. `cluster` makes the
    // button a flex item rather than a word after a space, so its focus ring has
    // clearance from the end of the sentence.
    const pickerFailure = el("div", {
      class: "note note--warning cluster",
      role: "alert",
      hidden: true,
      "data-testid": "portal-add-page-error",
    });
    const retry = el("button", { class: "btn btn-sm", type: "button" }, "Try again");
    retry.addEventListener("click", () => { loadCandidates(); });

    // Whether there is anything to add is a property of the list, so the action
    // that adds carries it too. An enabled "Add page" over a picker holding
    // nothing answered a click with "Choose a page to add." — an instruction
    // the operator cannot follow — and answered one over a picker still loading
    // with the same sentence, which was not even true.
    let loading = true;
    let dialog = null;
    function syncAction() {
      if (!dialog) return;
      dialog.proceed.disabled = loading || picker.disabled;
    }

    async function loadCandidates() {
      loading = true;
      pickerFailure.hidden = true;
      pickerFailure.replaceChildren();
      // Enabled while in flight: a disabled control carries its text at 2.3:1, and
      // this is the state a screen reader is least likely to be told about.
      picker.disabled = false;
      picker.setAttribute("aria-busy", "true");
      picker.replaceChildren(el("option", { value: "" }, "Loading pages…"));
      pickerStatus.textContent = "Loading the page list…";
      syncAction();
      let index;
      try {
        index = await api("/pages");
      } catch (error) {
        picker.replaceChildren(el("option", { value: "" }, "Pages unavailable"));
        picker.disabled = true;
        picker.removeAttribute("aria-busy");
        pickerStatus.textContent = "";
        // Unhidden BEFORE the text is written: a live region mutated while it is
        // out of the accessibility tree announces nothing.
        pickerFailure.hidden = false;
        pickerFailure.replaceChildren(
          el("span", {}, `Couldn't load the page list: ${error.message}`),
          retry
        );
        loading = false;
        syncAction();
        pickerFailure.scrollIntoView({ block: "nearest" });
        return;
      }
      const alreadyIn = new Set((state.detail.members || []).map((member) => String(member.page_id)));
      const all = index.pages || [];
      const candidates = all.filter((page) => !alreadyIn.has(String(page.id)));
      // Nothing to add has two causes and they are not the same sentence: every
      // dashboard is already a member, or there are no dashboards at all. Saying
      // the first on a fresh install would be a plain lie.
      const emptyLabel = all.length ? "Every page is already in this portal" : "No pages in Pages yet";
      picker.replaceChildren(
        el("option", { value: "" }, candidates.length ? "Choose a page…" : emptyLabel),
        ...candidates.map((page) =>
          el("option", { value: page.slug, dataset: { hasPassword: page.has_password ? "1" : "0" } },
            `${page.title || page.slug} — /${page.slug}${page.has_password ? "" : " · staff-only"}`)));
      picker.disabled = !candidates.length;
      picker.removeAttribute("aria-busy");
      pickerStatus.textContent = candidates.length
        ? `${candidates.length} page${candidates.length === 1 ? "" : "s"} to choose from.`
        : all.length
          ? "Every page in Pages is already in this portal — there is nothing left to add."
          : "There are no pages in Pages yet. Create one, then add it here.";
      loading = false;
      syncAction();
    }

    const label = el("input", { id: "portal-add-label", type: "text", maxlength: "200", autocomplete: "off" });

    // The reclassification warning, BEFORE the click. Whether it applies is a
    // property of the page chosen, so it appears and disappears with the choice.
    const notice = el("p", { id: "portal-reclassify-notice", class: "note note--warning", role: "status", hidden: true });
    picker.addEventListener("change", () => {
      const chosen = picker.selectedOptions[0];
      const reclassifies = Boolean(chosen && chosen.value && chosen.dataset.hasPassword === "0");
      notice.textContent = reclassifies
        ? "This page has no client password of its own, so today it is staff-only. Adding it here is what makes it readable by everyone holding this portal's password."
        : "";
      notice.hidden = !reclassifies;
    });

    dialog = formDialog({
      title: `Add a page to ${portal.name}`,
      closeLabel: "Close add page",
      description: "The partner sees it on their next page load. Nothing is redeployed.",
      fields: [
        // The picker's failure and its status line belong INSIDE its field: at a
        // form-stack gap they sat as far from the select they describe as an
        // unrelated field would.
        field({ id: "portal-add-page", label: "Page", control: [picker, pickerFailure], help: pickerStatus }),
        notice,
        field({
          id: "portal-add-label",
          label: "Label for the partner (optional)",
          control: label,
          help: "Overrides the page title in this partner's list. A page title can be changed by an agent; this cannot.",
        }),
      ],
      confirmLabel: "Add page",
      onConfirm: async () => {
        // "Add page" is disabled while the list is loading and while there is
        // nothing to pick, and a disabled default button is also what blocks
        // implicit submission — but that is one attribute away from not being
        // true, and Enter can still arrive with the placeholder selected. Both
        // refusals stay written here, in the dialog, next to the picker.
        if (loading) throw new Error("Still loading the page list.");
        if (!picker.value) throw new Error("Choose a page to add.");
        // No Order field to read: a new dashboard goes last, and the ↑/↓ controls
        // on its row are how it moves from there.
        const result = await addMember(portal, { slug: picker.value, label: label.value || null });
        await select(portal.id);
        // describeAdded, not a second hand-rolled wording: the dialog and the
        // link audit call the same addMember, so the same event must read the
        // same way whichever control was pressed, and one helper carries the
        // unit tests for both.
        toast(describeAdded([result]));
      },
    });
    syncAction();
    loadCandidates();
  }

  function editMemberDialog(portal, member) {
    const label = el("input", { id: "portal-edit-label", type: "text", maxlength: "200", value: member.label || "", autocomplete: "off" });
    formDialog({
      title: `Edit ${member.display_title}`,
      closeLabel: "Close edit membership",
      fields: [
        field({
          id: "portal-edit-label",
          label: "Label for the partner",
          control: label,
          help: `Empty falls back to the page title (${member.title || member.slug}).`,
        }),
      ],
      confirmLabel: "Save",
      onConfirm: async () => {
        await api(`/portals/${portal.id}/pages/update`, {
          // Label only. The endpoint is partial: lib/adminapi passes sortOrder
          // through only when the key is PRESENT, and lib/portals refuses the call
          // only when neither field is — which a label always satisfies. Sending
          // the row's current sort_order bought nothing and cost two things: a
          // no-op audit row with sort_order_from === sort_order_to, and a value
          // captured when the dialog opened silently rewinding any reorder made
          // while it was open.
          body: { page_id: member.page_id, label: label.value || null },
        });
        await select(portal.id);
        toast("Membership updated.");
      },
    });
  }

  function renameDialog(portal) {
    const name = el("input", { id: "portal-rename", type: "text", maxlength: "100", value: portal.name });
    formDialog({
      title: "Rename portal",
      closeLabel: "Close rename portal",
      fields: [field({ id: "portal-rename", label: "Partner name", control: name, help: "The link and the password do not change." })],
      confirmLabel: "Rename",
      onConfirm: async () => {
        await api(`/portals/${portal.id}/rename`, { body: { name: name.value } });
        await load();
        toast("Portal renamed.");
      },
    });
  }

  // ── list + detail ─────────────────────────────────────────────────────────
  function portalList() {
    if (!state.portals.length) {
      return emptyState(
        "No partner portals yet",
        "A portal gives one partner one link and one password for a set of pages. Create one, add their pages, then send them the link.",
        el("button", { class: "btn btn-primary", type: "button", onclick: newPortalDialog }, "New portal")
      );
    }
    const rows = state.portals.map((portal) => {
      const selected = String(portal.id) === String(state.selectedId);
      return el("tr", selected ? { class: "row--selected", "aria-current": "true" } : {},
        el("td", { "data-label": "Partner" },
          el("button", {
            class: "btn btn-sm",
            type: "button",
            onclick: () => select(portal.id).catch((error) => toast(error.message, { tone: "error" })),
          }, portal.name),
          el("div", { class: "table-meta" }, `/portal/${portal.slug}`)),
        el("td", { "data-label": "Pages" }, String(portal.page_count)),
        el("td", { "data-label": "Home" }, portal.home_page_slug ? `/${portal.home_page_slug}` : el("span", { class: "table-meta" }, "None")),
        el("td", { class: "table-meta", "data-label": "Updated" }, timeWhen(portal.updated_at)));
    });
    return el("div", { class: "operation-table-wrap" },
      el("table", { class: "operation-table" },
        el("caption", { class: "table-meta" }, `${state.portals.length} portal${state.portals.length === 1 ? "" : "s"}`),
        el("thead", {}, el("tr", {},
          el("th", { scope: "col" }, "Partner"),
          el("th", { scope: "col" }, "Pages"),
          el("th", { scope: "col" }, "Home"),
          el("th", { scope: "col" }, "Updated"))),
        el("tbody", {}, ...rows)));
  }

  function memberRow(portal, members, member) {
    const described = describeMember(member);
    const isHome = portal.home_page_id != null && String(portal.home_page_id) === String(member.page_id);

    // Two groups pinned to the two edges of the column, not one flat cluster.
    // .row-actions is right-aligned, and "Make home" comes and goes with the row's
    // state, so leading arrows sat at a DIFFERENT x on every row: press once and
    // the row that slides under the pointer has its arrow ~97px away, so the
    // second press in the same spot lands on empty cell space and does nothing.
    // Splitting the cluster gives every row's arrows one x and every row's Edit
    // another. (Collapsed to cards, shell.css packs both groups to the start, so
    // the three cards also wrap the same way as each other.)
    const reorder = el("div", { class: "row-actions__group" });
    const rest = el("div", { class: "row-actions__group" });
    const actions = el("div", { class: "row-actions row-actions--split" }, reorder, rest);

    // Nothing to reorder in a portal holding one dashboard: the pair would be two
    // permanently dead controls rather than the two ends of an order.
    let up = null;
    let down = null;
    if (members.length > 1) {
      up = moveControl(portal, members, member, "up");
      down = moveControl(portal, members, member, "down");
      reorder.append(up, down);
    }

    if (!isHome && described.openable) {
      // Ids on the text buttons too: they are what a keyboard operator is standing
      // on when Alt+↑/↓ rebuilds the row, and #146's keepingFocus and runMove both
      // restore focus by id.
      const home = el("button", { id: `portal-home-${member.page_id}`, class: "btn btn-sm", type: "button" }, "Make home");
      home.addEventListener("click", () => runAction({
        button: home,
        success: "Home page set.",
        run: async () => {
          await api(`/portals/${portal.id}/home`, { body: { slug: member.slug } });
          await select(portal.id);
        },
      }));
      rest.append(home);
    }
    rest.append(el("button", {
      id: `portal-edit-${member.page_id}`,
      class: "btn btn-sm",
      type: "button",
      onclick: () => editMemberDialog(portal, member),
    }, "Edit"));

    const remove = el("button", { id: `portal-remove-${member.page_id}`, class: "btn btn-sm btn-danger", type: "button" }, "Remove");
    remove.addEventListener("click", () => runAction({
      button: remove,
      confirm: {
        title: `Remove ${member.display_title}?`,
        message: "This partner stops being able to open it on their next request. The page itself, its versions and its own password are untouched.",
        confirmLabel: "Remove from portal",
        danger: true,
      },
      success: "Removed from portal.",
      run: async () => {
        await api(`/portals/${portal.id}/pages/remove`, { body: { page_id: member.page_id } });
        await select(portal.id);
      },
    }));
    rest.append(remove);

    const row = el("tr", {});
    // The shortcut each control declares with aria-keyshortcuts and the caption
    // names in plain sight. It fires from anywhere in the row, so a keyboard
    // operator who has tabbed to Edit or Remove can still reorder without hunting
    // back to the arrows — and stays on the control they were on. Alt is what
    // keeps it clear of the browser's own scrolling and of a select's arrow keys.
    row.addEventListener("keydown", (event) => {
      if (!event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
      const control = event.key === "ArrowUp" ? up : event.key === "ArrowDown" ? down : null;
      if (!control || control.disabled) return;
      event.preventDefault();
      queueMove(portal, member, control === up ? "up" : "down");
    });

    row.append(
      el("td", { "data-label": "Page" },
        el("strong", {}, member.display_title),
                // Home is a role in this portal, not a serving status; it used to borrow
        // the success pill and read as "this one is live and the others are not".
        isHome ? el("span", { class: "badge" }, "Home") : null,
        el("div", { class: "table-meta" }, `/${member.slug}`),
        ...described.warnings.map((warning) =>
          el("div", { class: warning.kind === "blocked" ? "note note--warning" : "note" }, warning.text))),
      // The zero-based integer that used to live here is gone: the position is
      // the row's position, and the controls in the action cluster are how it
      // changes. #173.
      el("td", { "data-label": "Actions" }, actions));
    return row;
  }

  function portalDetail() {
    if (!state.detail) return null;
    const portal = state.detail.portal;
    const members = state.detail.members || [];
    const openable = members.filter((member) => describeMember(member).openable).length;

    const rotate = el("button", { class: "btn btn-sm", type: "button" }, "Rotate password");
    rotate.addEventListener("click", async () => {
      // No success toast: the credential dialog IS the confirmation, and a toast
      // that scrolls away next to a password shown exactly once reads as noise.
      const { ok, result } = await runAction({
        button: rotate,
        confirm: {
          title: `Rotate the password for ${portal.name}?`,
          message: "Every partner using the current password is signed out immediately and needs the new one. Use this when a password has been shared too widely, or when someone has left.",
          confirmLabel: "Rotate password",
          danger: true,
        },
        success: null,
        run: () => api(`/portals/${portal.id}/password`, { body: {} }),
      });
      if (!ok) return;
      await select(portal.id);
      showCredential({ ...portal, ...result.portal, url: portal.url }, result.password);
    });

    const retire = el("button", { class: "btn btn-sm btn-danger", type: "button" }, "Retire portal");
    retire.addEventListener("click", () => runAction({
      button: retire,
      confirm: {
        title: `Retire ${portal.name}?`,
        message: `Their link stops working immediately and everyone using it is signed out. The ${members.length} membership${members.length === 1 ? "" : "s"} and the audit trail are kept, and no page is deleted.`,
        confirmLabel: "Retire portal",
        danger: true,
      },
      success: "Portal retired.",
      run: async () => {
        await api(`/portals/${portal.id}/delete`, { body: {} });
        await load({ keepSelection: false });
      },
    }));

    const memberTable = members.length
      ? el("div", { class: "operation-table-wrap" },
          el("table", { class: "operation-table" },
            el("caption", { class: "table-meta" },
              memberCaption({ count: members.length, openable, hasHome: portal.home_page_id != null })),
            el("thead", {}, el("tr", {},
              el("th", { scope: "col", class: "operation-table__page" }, "Page"),
              el("th", { scope: "col", class: "operation-table__member-actions" }, "Actions"))),
            el("tbody", {}, ...members.map((member) => memberRow(portal, members, member)))))
      : el("p", { class: "note" }, "No pages yet. Until one is added, this partner's link shows an empty portal.");

    return el("section", { class: "panel section-block" },
      el("div", { class: "section-heading section-heading--row" },
        el("div", { class: "section-heading" },
          el("h2", {}, portal.name),
          el("p", { class: "table-meta" }, portal.url)),
        el("div", { class: "row-actions" },
          copyButton(portal.url, "Link"),
          el("button", { class: "btn btn-sm", type: "button", onclick: () => renameDialog(portal) }, "Rename"),
          rotate,
          retire)),
      el("div", { class: "section-heading section-heading--row" },
        // tabindex="-1" so an add that empties the audit below has somewhere to
        // land focus — see focusAfterAdd.
        el("h3", { id: "portal-members-heading", tabindex: "-1" }, "Pages in this portal"),
        el("button", {
          class: "btn btn-sm btn-primary",
          type: "button",
          onclick: () => addPageDialog(portal),
        }, "Add a page")),
      memberTable,
      linkAuditSection(portal));
  }

  // ── link audit ──────────────────────────────────────────────────────────
  // Pages the home dashboard links to that are NOT members. A partner clicking
  // such a link loses the Page menu and, without a page password, hits a wall —
  // the drift that keeps recurring because hub links are agent-maintained while
  // membership is human-curated. Rendered only when there is something to act
  // on; the Add button is the same human decision as the dialog, one click
  // shorter.
  //
  // It is a TABLE, with the member table's own row anatomy — title, code slug,
  // the action at the end of the same 22rem column, a hairline between rows —
  // because it is a list of dashboards directly under a list of dashboards, and
  // the two are read together. Each item used to be an `el("div", { class:
  // "row-actions" })`: the table CELL cluster, which is justify-content:
  // flex-end, so every item floated to the far right edge of the panel with
  // nothing aligning it to the heading above it. With several missing pages that
  // reads as a layout failure rather than as a list (#155).

  // The audit's controls act as ONE set. runAction only ever knows about the
  // single button it was handed, so while "Add all N" was in flight every row's
  // own "Add to portal" stayed live — and an add is not idempotent: lib/portals
  // refuses a page that is already a member with `portal_page_exists`, which
  // aborts the rest of the sequential batch and silently skips what was left of
  // it. The reverse race is the same shape. So the section is held as a whole for
  // the length of any add and rebuilt from the server's answer afterwards
  // (#209 review).
  const auditControls = [];

  // runAction with the whole section held. The pressed button is left alone —
  // setBusy owns its disabled state AND its label, and re-enabling it here would
  // wipe the spinner.
  function runAudit(options) {
    return runAction({
      ...options,
      run: async () => {
        for (const control of auditControls) if (control !== options.button) control.disabled = true;
        try {
          return await options.run();
        } finally {
          // A success has already re-read the portal, so `auditControls` now
          // holds the freshly built (and already enabled) buttons and this is a
          // no-op. It is the FAILURE path that needs it: those buttons are still
          // in the document, and a dead row of disabled controls is worse than
          // the race it was guarding against.
          for (const control of auditControls) control.disabled = false;
        }
      },
    });
  }

  // focusAfterAdd — a successful add DELETES the control that was pressed, and
  // "Add all N" deletes the whole section around it. keepingFocus cannot rescue
  // this one: setBusy disables the button before the rebuild, so the browser has
  // already moved focus to <body> and there is no id left in the document to read
  // back. Land on the heading of whatever survived instead of at the top of the
  // page (#209 review).
  function focusAfterAdd() {
    const anchor = document.getElementById("portal-link-audit-heading")
      || document.getElementById("portal-members-heading");
    if (anchor && typeof anchor.focus === "function") anchor.focus();
  }

  // addLinked — one row's action. It used to POST straight from the handler with
  // `event.target.disabled = true` for a busy state, no confirmation of what it
  // did and no error voice but a bare toast: the row simply vanished. Through
  // runAction it gets the same spinner, the same one error voice and the same
  // told-what-happened as every other mutation on this screen — which matters
  // most here, because this is the click that can make a staff-only dashboard
  // readable by everyone holding the portal's password.
  function addLinked(portal, page) {
    const button = el("button", {
      id: `portal-link-audit-add-${page.page_id}`,
      class: "btn btn-sm",
      type: "button",
      // A column of identical "Add to portal" buttons tells a screen reader
      // nothing about which dashboard it would add. The visible word stays short.
      "aria-label": `Add to portal: ${page.title || page.slug}`,
    }, "Add to portal");
    button.addEventListener("click", () => runAudit({
      button,
      success: (result) => describeAdded([result]),
      run: async () => {
        const result = await addMember(portal, { slug: page.slug, label: null });
        await select(portal.id);
        focusAfterAdd();
        return result;
      },
    }));
    auditControls.push(button);
    return button;
  }

  // addAllLinked — the whole audit in one decision. Planned ONCE, because
  // appendPlan called N times reads the same state.detail.members every time and
  // would hand all N the same sort_order — see appendAllPlan.
  async function addAllLinked(portal, pages) {
    const plan = appendAllPlan(state.detail ? state.detail.members : [], pages.length);
    const results = [];
    try {
      // Sequential on purpose, like a renumber: each write is its own audited
      // transaction, and two adds racing would interleave two orders.
      for (const write of plan.writes) {
        await api(`/portals/${portal.id}/pages/update`, {
          body: { page_id: write.page_id, sort_order: write.sort_order },
        });
      }
      for (let index = 0; index < pages.length; index += 1) {
        results.push(await api(`/portals/${portal.id}/pages`, {
          body: { slug: pages[index].slug, label: null, sort_order: plan.sort_orders[index] },
        }));
      }
    } catch (error) {
      // Whatever landed before the failure is a real membership, and the screen
      // is still showing the list from before any of it — including audit rows
      // for pages that are now members. Re-read regardless, the way a part-way
      // renumber does, so what is on screen is what the server holds; the write's
      // own error is what reaches the toast, and a failing re-read must not mask
      // it.
      await select(portal.id).catch(() => {});
      focusAfterAdd();
      // …and the re-read has just deleted the rows that would have said what DID
      // land. Carry them out with the error so the failure toast can report both:
      // discarding them meant a reclassification that really happened was never
      // confirmed anywhere, in the one path where the row cannot confirm it
      // itself (#209 review).
      error.partialAdds = results;
      throw error;
    }
    await select(portal.id);
    focusAfterAdd();
    return results;
  }

  function linkAuditSection(portal) {
    // Rebuilt every render, so the set the lock closes over is rebuilt with it.
    auditControls.length = 0;
    const audit = state.detail.link_audit;
    if (!audit || !audit.scanned || !audit.missing.length) return null;
    const missing = audit.missing;

    // Only over more than one row: with a single row its own button already adds
    // all of them, and "Add all 1" beside it is two controls for one action.
    let addAll = null;
    if (missing.length > 1) {
      addAll = el("button", {
        id: "portal-link-audit-add-all",
        class: "btn btn-sm",
        type: "button",
        "data-testid": "portal-link-audit-add-all",
      }, `Add all ${missing.length}`);
      addAll.addEventListener("click", () => runAudit({
        button: addAll,
        // Named in the confirmation, before the click: one press adding several
        // pages at once is the one action here whose reach is not on the button.
        confirm: {
          title: `Add all ${missing.length} to ${portal.name}?`,
          message: `${missing.map((page) => page.title || page.slug).join(", ")} join this portal, after the pages already in it.`
            + " Any of them with no client password of its own becomes readable by everyone holding this portal's password.",
          confirmLabel: `Add all ${missing.length}`,
        },
        success: (results) => describeAdded(results),
        // A failure that is only half a failure says so, and names what it left
        // behind — see describePartialAdd.
        failure: (error) => describePartialAdd(error.message, error.partialAdds),
        run: () => addAllLinked(portal, missing),
      }));
      auditControls.push(addAll);
    }

    return el("div", { class: "section-block", "data-testid": "portal-link-audit" },
      el("div", { class: "section-heading" },
        // The action on the heading's own line, where "Add a page" sits over
        // the member table: the two share a right EDGE — not a left x, the
        // heading beside each is a different length — and the paragraph keeps its
        // 70ch measure underneath instead of squeezing into a flex column beside
        // a button floating at the foot of it. `.section-heading--row` wraps at
        // phone widths, where space-between drops a lone wrapped button to
        // flex-start; shell.css now gives the action side `margin-inline-start:
        // auto` so it keeps that right edge wrapped or not, and the two section
        // headers go on reading the same way at 390 (#209 review).
        el("div", { class: "section-heading section-heading--row" },
          el("h3", { id: "portal-link-audit-heading", tabindex: "-1" }, "Linked from the home page, but not members"),
          addAll),
        el("p", { class: "note" },
          "The home page links to these pages, but they are not in this portal — so a partner following the link loses the Page menu and may hit a password wall. Add the ones this partner is entitled to see."),
        // The reclassification warning belongs on the SCREEN, before any click —
        // the same invariant this file opens with — and in the SAME voice the
        // dialog says it in. Buried in the descriptive paragraph it rendered as
        // `.note`: muted, caption-sized, the least emphasised text in the panel,
        // while the dialog's identical statement is `.note--warning`. One
        // statement, one treatment.
        //
        // It is said about all of the rows at once because the audit payload
        // carries no has_password; only the add RESPONSE reports it, which is
        // exactly why the confirmation has to come afterwards as well. Making it
        // conditional means adding has_password to link_audit.missing in
        // lib/portals.js, which is a change to the admin API's response shape and
        // out of this PR's scope.
        el("p", { class: "note note--warning" },
          "Any of them with no client password of its own becomes readable by everyone holding this portal's password.")),
      el("div", { class: "operation-table-wrap" },
        el("table", { class: "operation-table" },
          // sr-only: the caption is this table's accessible name, not a second
          // paragraph of prose — see auditCaption.
          el("caption", { class: "sr-only" }, auditCaption(missing.length)),
          // The same two columns, at the same widths, as the member table above:
          // that is what makes the two lists read as one column of dashboards
          // rather than as a table and a drift of right-aligned text.
          el("thead", {}, el("tr", {},
            el("th", { scope: "col", class: "operation-table__page" }, "Page"),
            el("th", { scope: "col", class: "operation-table__member-actions" }, "Actions"))),
          el("tbody", {}, ...missing.map((page) =>
            el("tr", { "data-testid": `portal-link-audit-${page.slug}` },
              el("td", { "data-label": "Page" },
                el("strong", {}, page.title || page.slug),
                el("div", { class: "table-meta" }, `/${page.slug}`)),
              el("td", { "data-label": "Actions" },
                el("div", { class: "row-actions" }, addLinked(portal, page)))))))));
  }

  function render() {
    // Every action on this screen rebuilds the whole subtree, which dropped focus
    // to <body> — on the one screen that is only ever operated by a human.
    keepingFocus(() => renderNow(), { fallback: ['.row--selected button', "#app h1"] });
  }

  function renderNow() {
    const shared = state.portals.reduce((total, portal) => total + Number(portal.page_count || 0), 0);
    app.replaceChildren();
    app.append(
      // The <h1> and the primary action used to live INSIDE a bordered panel, so
      // this screen's title sat in a card and the other three screens' did not.
      pageHeader({
        title: "Partner portals",
        intro: "One link and one password for a set of pages.",
        actions: state.portals.length
          ? el("button", { id: "new-portal", class: "btn btn-primary", type: "button", onclick: newPortalDialog }, "New portal")
          : null,
        stats: state.portals.length
          ? [statTile(state.portals.length, "portal"), statTile(shared, "page shared", "pages shared")]
          : null,
      }),
      el("section", { class: "section-block" }, portalList())
    );
    const detail = portalDetail();
    if (detail) app.append(detail);
  }

  // Back and Forward move between portals, now that the URL says which one.
  window.addEventListener("popstate", () => {
    const asked = portalIdFromSearch(window.location.search);
    const wanted = resolveSelection(state.portals, asked);
    if (wanted == null) return;
    // A history entry can name a portal that has since been retired, or junk that
    // was hand-edited into the bar. resolveSelection falls back to the first one,
    // so the address bar has to be corrected to what is actually rendered — the
    // same correction load() makes on arrival, which `route: false` would
    // otherwise skip. replaceState, so Back stays where the person put it.
    const correct = () => {
      if (String(wanted) !== String(asked) && String(state.selectedId) === String(wanted)) {
        setPortalInURL(wanted, { replace: true });
      }
    };
    if (String(wanted) === String(state.selectedId)) {
      correct();
      return;
    }
    select(wanted, { route: false })
      .then(correct)
      .catch((error) => toast(error.message, { tone: "error" }));
  });

  load().catch((error) => {
    app.replaceChildren(loadFailed("partner portals", error, () => load()));
  });
})();
