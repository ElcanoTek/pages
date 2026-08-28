// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/contentview.js — serve the live client page DIRECTLY on the content host
// (PLAN §6b, direct-serve decision). elcano-pages.com/<slug> is the client URL.
//
// Gate (the content host owns it; SSO cookie never reaches here):
//   • disabled / unknown / nothing-published  → 404
//   • ?t=<broker token> (minted by the dashboard /view after SSO, for Elcano-only
//     pages) → set a short page-session cookie, redirect to the clean URL
//   • valid page-session cookie → render the published version (sandbox CSP)
//   • password page, no session → show the password form (trusted, NOT sandboxed)
//   • Elcano-only page (no password), no session → 403 "open it from the dashboard"
//
// Two response flavors: the rendered agent page gets the `sandbox` CSP
// (rawHeaders), the gate/form gets the trusted gateHeaders (so it can submit).

const db = require("./db");
const standaloneChrome = require("./standalone-chrome");
const render = require("./render");
const rawtoken = require("./rawtoken");
const pagecookie = require("./pagecookie");
const passwordgate = require("./passwordgate");
const auth = require("./auth");
const { rawHeaders, gateHeaders, DASHBOARD_ORIGIN, CONTENT_ORIGIN } = require("./csp");
const { setTimeout: sleep } = require("node:timers/promises");

const FLAG_BASE = "/assets/flag"; // same-origin on the content host
// The same knob lib/ratelimit.js reads, so the page cannot promise a wait the
// limiter does not enforce. It said "a few minutes" for a fifteen-minute window.
const PASSWORD_WINDOW_MINUTES = Number(process.env.RL_PASSWORD_WINDOW_MIN) > 0
  ? Number(process.env.RL_PASSWORD_WINDOW_MIN)
  : 15;
const MARK = `${FLAG_BASE}/logos/elcano-mark-primary.svg`;

function slugFromReq(req) {
  const raw = req.params.slug;
  const s = (Array.isArray(raw) ? raw.join("/") : raw || "").toLowerCase();
  // url-safe segments only (mirrors versions.normalizeSlug); anything else → no page
  return /^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:\/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$/.test(s) ? s : null;
}
function isSecure(req) {
  return req.secure || (req.headers["x-forwarded-proto"] || "").split(",")[0] === "https";
}
function notFound(res) {
  return res.status(404).set(rawHeaders()).type("html").send(notFoundPage());
}

// The switcher payload ships on every render of every member page, so it is
// bounded on all three axes an unbounded one would grow along: entries, text
// length, and total bytes. `truncated` is in the payload rather than silent, so a
// template can say "and N more" instead of quietly showing a partial set.
const NAV_MAX_ENTRIES = 50;
const NAV_MAX_TEXT = 200;
const NAV_MAX_BYTES = 16 * 1024;

// buildNav — pure. The list is a function of the AUTHORISING portal, never of
// "every portal containing this page": a page can sit in two partners' portals,
// and showing the union would hand each of them the other's dashboard titles.
// Holding a valid cookie for that portal is what proves entitlement to its list.
function buildNav(portal, rows, currentSlug) {
  const clip = (value) => String(value == null ? "" : value).slice(0, NAV_MAX_TEXT);
  const nav = {
    // The portal index is the one surface that always works for a partner — it is
    // where their link goes and it never 404s on them. The menu could not reach it
    // because the URL was never in the payload, only the slug, and a template must
    // never build an href (see below).
    portal: {
      slug: clip(portal.slug),
      name: clip(portal.name),
      url: `${CONTENT_ORIGIN}/portal/${portal.slug}`,
    },
    // A ready-made absolute URL, because a template must never build the href:
    // slugs nest, so on /a/b/c a relative "d" resolves to /a/b/d, and
    // `base-uri 'none'` means a <base> tag cannot correct it.
    pages: rows.slice(0, NAV_MAX_ENTRIES).map((row) => ({
      slug: clip(row.slug),
      title: clip(row.title || row.slug),
      url: `${CONTENT_ORIGIN}/${row.slug}`,
      current: row.slug === currentSlug,
      // The partner's index puts the home page first whatever the sort order, so
      // the menu has to be able to say which one it is.
      home: Boolean(row.is_home),
    })),
    truncated: rows.length > NAV_MAX_ENTRIES,
  };
  // Byte ceiling last, since a title is bounded but a hundred of them are not.
  while (nav.pages.length > 1 && JSON.stringify(nav).length > NAV_MAX_BYTES) {
    nav.pages.pop();
    nav.truncated = true;
  }
  return nav;
}

// navPayload — the one query on the RENDER path, and the reason renderLive
// swallows its failure. A membership lookup that times out must not turn a live
// client dashboard into a 500: the page renders identically without a switcher,
// which is the difference between a degraded feature and an outage.
async function navPayload(portal, page) {
  if (!portal) return null;
  try {
    return buildNav(portal, await db.getPortalPages(portal.id), page.slug);
  } catch (err) {
    console.error("contentview: page switcher payload unavailable:", err.message);
    return null;
  }
}

async function renderLive(res, page, portal = null) {
  const nav = await navPayload(portal, page);
  res.set(rawHeaders());
  res.type("html").send(
    render.renderVersion({
      html: page.html,
      render_mode: page.render_mode,
      override_css: page.override_css || "",
      nav,
    })
  );
}

// chrome — the trusted, Flag-themed shell for every content-host gate page
// (password prompt, staff-only notice, 404). Scriptless by design: it runs under
// gateHeaders()/rawHeaders() CSP (default-src 'none', NO script-src), so all
// styling is an inline <style> (style-src 'unsafe-inline' is allowed) and the
// only assets are same-origin Flag tokens/fonts. The agent's page content is
// NEVER rendered here — this is our chrome only (invariant #2).
// The chrome is shared with the dashboard host now — see lib/standalone-chrome.js
// for why it stays an inline sheet and why it is dark only. This wrapper exists so
// every caller here keeps passing exactly what it passed before, and so the
// content host's own asset mount is named in one place.
function chrome(options) {
  return standaloneChrome.render({ ...options, assetsBase: FLAG_BASE });
}

// gatePage — password prompt or staff-only notice, wrapped in the shared chrome.
//
// `credential` says which secret opens this page, which changes only the wording:
//   "page"   — its own client password (the original behaviour)
//   "portal" — it has no password of its own and is in a portal, so the portal
//              credential is the only one that opens it
//   "either" — it has both, and a partner arriving from a bookmark should not be
//              told their portal password is the wrong thing to type
// The copy never names a portal. That a page belongs to *some* portal is already
// implied by offering the prompt at all; which one is not.
function gatePage({ slug, message, showForm, credential = "page" }) {
  const msg = message
    ? `<div class="alert" id="password-error" role="alert"><strong>${escapeHtml(message)}</strong><span>Check the password and try again. Each wrong attempt adds a short delay before the next one.</span></div>`
    : "";
  const prompt = {
    page: "Enter the client password to continue to this page.",
    portal: "Enter your portal password to continue to this dashboard.",
    either: "Enter the password for this page, or your portal password, to continue.",
  }[credential];
  const help = {
    page: "Use the password supplied with this page link.",
    portal: "Use the password from your portal link — the same one opens every dashboard in your portal.",
    either: "Use the password supplied with this page link, or the one from your portal link.",
  }[credential];
  const body = showForm
    ? `<p class="sub">${escapeHtml(prompt)}</p>${msg}
       <form method="post" action="/${escapeAttr(slug)}">
         <input type="text" name="account" value="${escapeAttr(slug)}" autocomplete="username" readonly tabindex="-1" aria-hidden="true" class="credential-account">
         <div class="field">
           <label for="page-password">${{ portal: "Portal password", page: "Page password", either: "Password" }[credential]}</label>
           <input id="page-password" type="password" name="password" autofocus autocomplete="current-password" required aria-describedby="password-help${message ? " password-error" : ""}">
           <span class="help" id="password-help">${escapeHtml(help)}</span>
         </div>
         <button type="submit">View page</button>
       </form>`
    : `<p class="sub">The person who sent you this link needs to turn on access before it will open. Ask them to share it again.</p>${msg}<p class="guidance">Elcano staff: <a href="${escapeAttr(`${DASHBOARD_ORIGIN}/view/${slug}`)}">open it from the Pages dashboard</a>.</p>`;
  return chrome({
    title: "Protected page · Elcano Pages",
    kicker: showForm ? "Protected" : "Restricted",
    // The heading names what the READER can do about it. "Staff-only page" is
    // true and useless to the partner who followed a link that was shared before
    // the page was passworded — which is who actually lands here.
    heading: showForm ? "This page is protected" : "This page hasn't been shared yet",
    bodyHtml: body,
  });
}

// ── partner-facing timestamps ───────────────────────────────────────────────
// These pages carry no script, so the formatting happens here. The convention is
// the admin's formatWhen (public/shell-assets/primitives.js): relative while a
// person would still count it in days, absolute once they would not, and seconds
// never. Both surfaces describing the same instant differently is how a partner
// and the operator they phone end up disagreeing about what "yesterday" meant.
//
// Relative is computed from ELAPSED time, not calendar days, which is what makes
// it safe to render server-side for a reader whose timezone we do not know: "3
// days ago" is true in every timezone, "today" would not be.
const MINUTE_MS = 60000, HOUR_MS = 3600000, DAY_MS = 86400000;
const RELATIVE_LIMIT_MS = 7 * DAY_MS;

function whenPhrase(value, now = Date.now()) {
  const t = Date.parse(value instanceof Date ? value.toISOString() : String(value || ""));
  if (!Number.isFinite(t)) return null;
  const elapsed = now - t;
  // A future timestamp is clock skew, not a countdown — show the date and let it
  // look odd rather than promise something.
  if (elapsed < 0 || elapsed >= RELATIVE_LIMIT_MS) {
    return new Date(t).toLocaleString("en-GB", { day: "numeric", month: "short", year: "numeric" });
  }
  const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
  if (elapsed < MINUTE_MS) return "just now";
  if (elapsed < HOUR_MS) return rtf.format(-Math.round(elapsed / MINUTE_MS), "minute");
  if (elapsed < DAY_MS) return rtf.format(-Math.round(elapsed / HOUR_MS), "hour");
  return rtf.format(-Math.round(elapsed / DAY_MS), "day");
}

// freshnessHtml — the one line under a dashboard's title, and the reason #176
// exists: on a set of daily-refreshed dashboards, "which of these changed this
// morning" is the question the index was not answering at all.
//
// The two timestamps are NOT interchangeable and are not coalesced into one
// phrase. source_as_of is when the DATA is from, which is what a partner is
// actually asking; the live version's created_at is only when we last published
// something. Saying "Updated today" about a page republished today from a
// three-week-old extract is precisely the false reassurance this is meant to
// remove, so each gets its own verb and a page with no data envelope simply
// says less.
function freshnessHtml(entry, now = Date.now()) {
  const asOf = entry.source_as_of, published = entry.published_at;
  const stamp = asOf || published;
  if (!stamp) return "";
  const phrase = whenPhrase(stamp, now);
  if (!phrase) return "";
  const iso = new Date(Date.parse(stamp instanceof Date ? stamp.toISOString() : String(stamp))).toISOString();
  const label = asOf ? `Data as of ${phrase}` : `Updated ${phrase}`;
  return `<span class="portal-item__when"><time datetime="${escapeAttr(iso)}" title="${escapeAttr(iso)}">${escapeHtml(label)}</time></span>`;
}

// portalIndexPage — the partner's landing page: one credential, then the set of
// dashboards that credential opens (design decision 4). Deliberately OUR page and
// not agent HTML, so it is served under gateHeaders(): scriptless, Flag-themed,
// and NOT sandboxed — which means its links are ordinary links and the sandbox
// navigation question never arises here at all. It is also the surface that always
// works: the in-page switcher depends on a template rendering a block, this does
// not.
function portalIndexPage({ portal, pages = [], showForm = false, message = "", now = Date.now() }) {
  const action = `/portal/${escapeAttr(portal.slug)}`;
  const msg = message
    ? `<div class="alert" id="portal-error" role="alert"><strong>${escapeHtml(message)}</strong><span>Check the password and try again. Each wrong attempt adds a short delay before the next one.</span></div>`
    : "";
  let body;
  if (showForm) {
    body = `<p class="sub">Enter your portal password to open your dashboards.</p>${msg}
       <form method="post" action="${action}">
         <input class="credential-account" type="text" name="account" value="${escapeAttr(portal.slug)}" tabindex="-1" aria-hidden="true" autocomplete="username" readonly>
         <div class="field">
           <label for="portal-password">Portal password</label>
           <input id="portal-password" type="password" name="password" autofocus autocomplete="current-password" required aria-describedby="portal-help${message ? " portal-error" : ""}">
           <span class="help" id="portal-help">Use the password supplied with this portal link. One password opens every dashboard listed here.</span>
         </div>
         <button type="submit">Open portal</button>
       </form>`;
  } else if (pages.length) {
    // Root-absolute hrefs, never relative: slugs nest, so on /a/b/c a relative
    // href resolves against /a/b/ — and `base-uri 'none'` means a <base> tag
    // cannot rescue it.
    //
    // The freshness line sits INSIDE the anchor, unlike the "Overview" tag it
    // replaces. That tag made the link's accessible name "Portfolio overview
    // Overview" — a word repeated for no gain. "Portfolio overview, data as of
    // 3 days ago" is the same shape and is the reason someone picks one link
    // over another, so it earns its place in the name.
    const item = (entry) =>
      `<li class="portal-item"><a href="/${escapeAttr(entry.slug)}">` +
      `<span class="portal-item__title">${escapeHtml(entry.title)}</span>` +
      `${freshnessHtml(entry, now)}</a></li>`;
    const home = pages.filter((entry) => entry.is_home);
    const rest = pages.filter((entry) => !entry.is_home);
    // Two sections only when there is something to separate. A partner with one
    // dashboard, or with no home page set, gets a plain list — headings over a
    // list of one are hierarchy for its own sake.
    const list =
      home.length && rest.length
        ? `<h2 class="portal-section">Start here</h2>` +
          `<ul class="portal-list portal-list--lead">${home.map(item).join("")}</ul>` +
          `<h2 class="portal-section">All dashboards</h2>` +
          `<ul class="portal-list">${rest.map(item).join("")}</ul>`
        : `<ul class="portal-list">${pages.map(item).join("")}</ul>`;
    body =
      `<p class="sub">${pages.length} dashboard${pages.length === 1 ? "" : "s"} available to you.</p>` +
      list +
      sessionFooter(portal);
  } else {
    // A real state, not an error: a portal can exist before anything is added to
    // it, and a member whose page is taken down or unpublished is filtered out
    // rather than listed as a dead link.
    body =
      `<p class="sub">No dashboards are available in this portal yet.</p>` +
      `<p class="guidance">Nothing is wrong with your password — ask your Elcano contact to add your dashboards to this portal.</p>` +
      sessionFooter(portal);
  }
  return chrome({
    title: `${portal.name} · Elcano Pages`,
    kicker: "Partner portal",
    heading: portal.name,
    bodyHtml: body,
  });
}

// sessionFooter — how long this browser stays open, and how to close it.
//
// The old copy said "for as long as this portal session lasts", which is true of
// every session ever and told the reader nothing; the cookie is thirty days
// (lib/pagecookie DEFAULT_TTL_DAYS, read rather than restated). Thirty days on a
// laptop a partner shares with a colleague is a real exposure, and until now
// there was no way to end it short of clearing cookies. The button is a plain
// form because this host runs no script.
function sessionFooter(portal) {
  const days = pagecookie.DEFAULT_TTL_DAYS;
  return (
    `<div class="portal-footer">` +
    `<p class="guidance">Each dashboard opens without another password for ${days} days on this device.</p>` +
    `<form class="portal-signout" method="post" action="/portal/${escapeAttr(portal.slug)}/lock">` +
    `<button type="submit">Sign out</button>` +
    `</form>` +
    `</div>`
  );
}

// notFoundPage — a styled 404 in the same chrome (was a bare one-liner).
function notFoundPage() {
  return chrome({
    title: "Not found · Elcano Pages",
    kicker: "Not found",
    heading: "Page not found",
    bodyHtml: `<p class="sub">This address may be wrong, or the page may have been taken down. Check the link with the person who shared it.</p>`,
  });
}

// expiredLinkPage — a template preview link is signed and short-lived, so this is
// the state a reviewer reaches most often: open the preview tomorrow, get a bare
// "<p>Invalid or expired link.</p>". Everything else on this host has a branded
// interstitial that says what to do next; a preview had the one page that looked
// like a server fault.
function expiredLinkPage() {
  return chrome({
    title: "Link expired · Elcano Pages",
    kicker: "Link expired",
    heading: "This preview link is no longer valid",
    bodyHtml:
      `<p class="sub">Preview links expire after a short time, so they cannot be shared or bookmarked.</p>` +
      `<p class="guidance">Ask for a fresh link \u2014 generating one takes a second and the design has not changed.</p>`,
  });
}

// `slug` and `minutes` are optional so the exported renderer stays usable without a
// request, but the live handler passes both: the wait was "a few minutes" when the
// window is fifteen, and without a link the reader is stranded on a POST target
// whose reload asks the browser to resubmit the form.
function rateLimitPage({ slug, minutes = PASSWORD_WINDOW_MINUTES } = {}) {
  const back = slug
    ? `<p class="guidance"><a href="/${escapeAttr(slug)}">Return to the page</a> when the wait is over.</p>`
    : "";
  return chrome({
    title: "Too many attempts · Elcano Pages",
    kicker: "Access paused",
    heading: "Too many password attempts",
    bodyHtml:
      `<p class="sub">Too many passwords have been tried for this page. Wait about ${minutes} minutes, then try again.</p>` +
      `<p class="guidance">If access is urgent, ask the person who shared this page to confirm the password.</p>` +
      back,
  });
}

// busyPage — the READ limiter (lib/ratelimit.js `content`), which is a different
// event from the password guard above: nothing is wrong with the viewer's
// credentials, the host is just shedding load. This used to answer a client
// navigation with a raw JSON body, so someone reloading their own dashboard was
// shown `{"error":"rate limit exceeded — slow down"}`.
function busyPage({ slug } = {}) {
  return chrome({
    title: "Too many requests · Elcano Pages",
    kicker: "Slow down",
    heading: "This page is being loaded too often",
    bodyHtml:
      `<p class="sub">The page is fine — requests from your network are arriving faster than our servers can answer them. Wait a moment and reload.</p>` +
      `<p class="guidance">If this keeps happening, tell the person who shared the page; it usually means something is reloading it automatically.</p>` +
      (slug ? `<p class="guidance"><a href="/${escapeAttr(slug)}">Reload the page</a></p>` : ""),
  });
}

// serverErrorPage — a fault on our side. Says so plainly rather than implying the
// viewer's link is wrong (which is what a 404 would imply) and rather than
// leaking Express's default error document, which is what this host emitted
// before an error handler existed.
function serverErrorPage() {
  return chrome({
    title: "Something went wrong · Elcano Pages",
    kicker: "Error",
    heading: "This page couldn't be loaded",
    bodyHtml:
      `<p class="sub">Something went wrong on our side, not with your link. Reloading in a moment usually works.</p>` +
      `<p class="guidance">If it keeps failing, tell the person who shared this page so an Elcano admin can look.</p>`,
  });
}

// portalsForPage — membership as the gate sees it, read live on every request.
//
// Deliberately swallows a failure. This lookup can only ever grant access, so an
// empty result is incapable of opening something it should not: the request falls
// through to exactly the gate it would have got before portals existed. A throw,
// by contrast, would take the gate out for every page on the host — member or not
// — and the content host's error handler would answer a client's dashboard with a
// 500. Fail-safe here means fail-closed by construction, which is why catching is
// the conservative choice rather than the lax one.
async function portalsForPage(pageId) {
  try {
    return await db.getPortalsForPage(pageId);
  } catch (err) {
    console.error("contentview: portal membership lookup failed:", err.message);
    return [];
  }
}

// authorizingPortal — which portal, if any, this request has already unlocked.
// The rows arrive ordered by portal id and the FIRST match wins, so a viewer
// holding cookies for two portals that both contain this page gets the same
// answer on every request. Holding a valid pgp<N> proves knowledge of portal N's
// password, so serving N's view to that holder is never a leak — but it must not
// flip between two of them from one load to the next, because the sibling list a
// page shows is scoped to whichever portal authorised it.
function authorizingPortal(cookies, portals) {
  for (const portal of portals) {
    if (pagecookie.verifyPortalSession(cookies[pagecookie.portalCookieName(portal.id)], portal.id, portal.password_hash)) {
      return portal;
    }
  }
  return null;
}

// portalSlugOf — is this request for the partner entry point rather than a page?
// Exactly two segments, `portal/<portal-slug>`; a portal slug is one segment by
// construction (lib/portals.normalizeSlug), so anything deeper is not a portal
// URL and falls through to the ordinary page lookup.
function portalSlugOf(slug) {
  const parts = slug.split("/");
  if (parts.length !== 2 || parts[0] !== "portal") return null;
  return parts[1];
}

// portalLockOf — POST /portal/<slug>/lock, the scriptless sign-out. Three
// segments, and `portal` is a reserved slug segment (versions.RESERVED_SLUG_SEGMENTS),
// so no page can ever live at this address for this to shadow.
function portalLockOf(slug) {
  const parts = slug.split("/");
  if (parts.length !== 3 || parts[0] !== "portal" || parts[2] !== "lock") return null;
  return parts[1];
}

// GET /<slug> — the direct-serve entry point.
async function serve(req, res) {
  const slug = slugFromReq(req);
  if (!slug) return notFound(res);
  // The portal entry point is claimed BEFORE any page lookup. That ordering is
  // what makes a page at this address unable to seize a partner's bookmarked URL,
  // and `portal` is a reserved slug segment so one cannot be created; the two
  // together mean neither defence has to be perfect on its own. Handled here,
  // inside the wildcard, so it inherits limits.content on GET and the strict
  // limits.password brute-force guard on POST — a route registered ahead of the
  // wildcard would silently have neither.
  const portalSlug = portalSlugOf(slug);
  if (portalSlug !== null) return servePortalIndex(req, res, portalSlug);

  const page = await db.getPublicPage(slug);
  if (!page || page.disabled || !page.published_version_id) return notFound(res);

  // Elcano-broker token: exchange for a short session cookie, then clean URL.
  //
  // Only purpose "session" — minted solely by the dashboard's staff-gated /view
  // broker — buys a page session. A "view" token is a RENDER credential: /admin
  // mints one for any version so the shell can preview it in a sandboxed iframe
  // (lib/adminapi.js preview-token), and when nothing is pending the shell
  // previews the PUBLISHED version, so simply opening /admin/<slug> mints one.
  // Redeemed here, that 300-second read-only preview URL would become a one-HOUR
  // session cookie for the whole live page — bypassing the client password — and
  // a preview URL is exactly the kind of link an admin pastes into a chat.
  // Binding by version alone would not separate these: the common preview IS of
  // the published version. So the audiences are disjoint by purpose, and the
  // version equality below stays as defense in depth.
  if (req.query && typeof req.query.t === "string") {
    const claims = rawtoken.verify(req.query.t);
    if (
      claims &&
      claims.purpose === "session" &&
      Number(claims.pid) === Number(page.id) &&
      // `0` is a live sentinel in this codebase (template tokens carry pid 0),
      // so require a real positive id rather than trusting coercion.
      Number.isInteger(Number(claims.vid)) &&
      Number(claims.vid) > 0 &&
      Number(claims.vid) === Number(page.published_version_id)
    ) {
      // The floor headers already set Cache-Control: no-store, which matters
      // most on exactly this response: it carries a page credential. A 302/303
      // is not heuristically cacheable, so this is belt-and-braces — but a
      // Set-Cookie response is the last place to rely on a default.
      res.setHeader(
        "Set-Cookie",
        pagecookie.sessionCookieHeader(page.id, { ttlSeconds: 3600, secure: isSecure(req), passwordHash: page.password_hash })
      );
      return res.redirect(302, "/" + slug);
    }
  }

  const cookies = auth.parseCookies(req.headers.cookie);
  // Sessions are bound to the credential state that minted them: a password
  // change (or set/clear) invalidates every prior cookie for this page.
  const pageSession = pagecookie.verifySession(cookies[pagecookie.cookieName(page.id)], page.id, page.password_hash);
  // The membership query is skipped entirely when it cannot change the answer: a
  // valid page session and no pgp cookie in the jar means there is no portal to
  // authorise and no switcher to populate, so the ordinary client viewing an
  // ordinary passworded page pays nothing for this feature. A partner holding
  // BOTH credentials still gets their switcher, which is why the page session
  // alone is not enough to skip it.
  const holdsPortalCookie = Object.keys(cookies).some((name) => /^pgp[0-9]+$/.test(name));

  // Portal authorisation. Everything above is unchanged, so `disabled`,
  // unpublished and unknown still 404 first — the takedown switch and the
  // delete→recreate slug-reuse guard keep working exactly as they did, and no
  // portal credential can reach a page those rules already closed.
  //
  // A portal-authorised render sets NO cookie, ever. Minting a pgs<page_id> here
  // would be the obvious optimisation and it would be a hole: that cookie binds
  // to the PAGE's own password, so removing the page from the portal — or
  // rotating the portal password — would revoke nothing for thirty days.
  // Membership is read live instead, on every request, and that is the whole
  // mechanism by which access is taken away.
  const portals = pageSession && !holdsPortalCookie ? [] : await portalsForPage(page.id);
  const portal = authorizingPortal(cookies, portals);
  if (pageSession || portal) {
    return renderLive(res, page, portal);
  }

  if (page.password_hash) {
    return res
      .status(401)
      .set(gateHeaders())
      .type("html")
      .send(gatePage({ slug, showForm: true, credential: portals.length ? "either" : "page" }));
  }
  if (portals.length) {
    // A member with no password of its own — the staff-only page a human
    // deliberately reclassified by adding it to a portal. Its portal credential
    // is the one that opens it, so it gets a prompt rather than the staff notice,
    // which would tell a partner they are not entitled to a page they are.
    return res.status(401).set(gateHeaders()).type("html").send(gatePage({ slug, showForm: true, credential: "portal" }));
  }
  // Elcano-only (no password): no SSO on this origin → route via the dashboard.
  return res.status(403).set(gateHeaders()).type("html").send(gatePage({ slug, showForm: false }));
}

// GET /portal/<slug> — the partner landing page. An unknown or retired portal is
// a 404 like any other unknown address: which portals exist is not public.
async function servePortalIndex(req, res, portalSlug) {
  const portal = await db.getPublicPortal(portalSlug);
  if (!portal) return notFound(res);
  const cookies = auth.parseCookies(req.headers.cookie);
  if (pagecookie.verifyPortalSession(cookies[pagecookie.portalCookieName(portal.id)], portal.id, portal.password_hash)) {
    // Membership is read live, per request — never cached into the session — so
    // adding or removing a dashboard takes effect on the partner's next load.
    const pages = await db.getPortalPages(portal.id);
    return res.status(200).set(gateHeaders()).type("html").send(portalIndexPage({ portal, pages }));
  }
  return res.status(401).set(gateHeaders()).type("html").send(portalIndexPage({ portal, showForm: true }));
}

// POST /portal/<slug> — portal password submission.
async function unlockPortal(req, res, portalSlug) {
  const portal = await db.getPublicPortal(portalSlug);
  if (!portal) return notFound(res);
  const password = (req.body && req.body.password) || "";
  if (!(await pagecookie.verifyPassword(password, portal.password_hash))) {
    // Progressive backoff on the PORTAL's shared counter (lib/passwordgate.js),
    // which every door testing this password charges — see migrations/019.
    const delayMs = await passwordgate.recordPortalFailure(portal.id);
    if (delayMs > 0) await sleep(delayMs);
    return res
      .status(401)
      .set(gateHeaders())
      .type("html")
      .send(portalIndexPage({ portal, showForm: true, message: "Incorrect password." }));
  }
  await passwordgate.clearPortalFailures(portal.id);
  res.setHeader(
    "Set-Cookie",
    pagecookie.portalSessionCookieHeader(portal.id, { secure: isSecure(req), passwordHash: portal.password_hash })
  );
  return res.redirect(303, `/portal/${portal.slug}`); // 303 → browser GETs the index
}

// POST /portal/<slug>/lock — end the session on this device.
//
// It clears the portal cookie AND the page cookie of every dashboard currently
// in the portal. Only the first is strictly this portal's, but a partner who
// typed a page's own password once holds that page's cookie too, and a "Sign
// out" that leaves a dashboard open on a shared laptop is worse than no button:
// the reader believes something that is not true.
//
// What it cannot reach: a page since removed from the portal, whose cookie this
// request has no way to name. That is a bounded residue — the page is no longer
// listed and the cookie expires with the rest — and is the reason the copy says
// "this device" rather than "everywhere". It also clears this BROWSER's copy
// rather than revoking the token; rotating the password is what revokes.
//
// No CSRF token, like the unlock form it sits beside. Both cookies are
// SameSite=Lax, so a cross-site POST arrives without them; the worst a forged
// one achieves is signing the reader out, which is the button's own effect.
async function lockPortal(req, res, portalSlug) {
  const portal = await db.getPublicPortal(portalSlug);
  if (!portal) return notFound(res);
  const secure = isSecure(req);
  const cookies = [pagecookie.clearedPortalSessionCookieHeader(portal.id, { secure })];
  // Best effort: a membership read that fails must not turn a sign-out into a
  // 500, because the portal cookie above is the one that actually matters.
  try {
    for (const page of await db.getPortalPages(portal.id)) {
      cookies.push(pagecookie.clearedSessionCookieHeader(page.id, { secure }));
    }
  } catch (err) {
    console.error("contentview: portal sign-out could not enumerate pages:", err.message);
  }
  res.setHeader("Set-Cookie", cookies);
  // 303 back to the index, which now renders the password form — the reader sees
  // the sign-out took effect rather than being told it did.
  return res.redirect(303, `/portal/${portal.slug}`);
}

// POST /<slug> — password form submission.
async function unlock(req, res) {
  const slug = slugFromReq(req);
  if (!slug) return notFound(res);
  const lockSlug = portalLockOf(slug);
  if (lockSlug !== null) return lockPortal(req, res, lockSlug);
  const portalSlug = portalSlugOf(slug);
  if (portalSlug !== null) return unlockPortal(req, res, portalSlug);

  const page = await db.getPublicPage(slug);
  if (!page || page.disabled || !page.published_version_id) return notFound(res);
  const portals = await portalsForPage(page.id);
  // No credential of any kind opens this page here: no password of its own, and
  // in no portal. Same 404 as before — the form that would post here is not shown.
  if (!page.password_hash && portals.length === 0) return notFound(res);

  const password = (req.body && req.body.password) || "";
  if (page.password_hash && (await pagecookie.verifyPassword(password, page.password_hash))) {
    await passwordgate.clearFailures(page.id);
    // Carries a 30-day page credential; see the note on the broker exchange above.
    res.setHeader("Set-Cookie", pagecookie.sessionCookieHeader(page.id, { secure: isSecure(req), passwordHash: page.password_hash }));
    return res.redirect(303, "/" + slug); // 303 → browser GETs the page
  }
  // A partner who bookmarked one dashboard holds their PORTAL password, not this
  // page's, so the same submission is tried against each portal containing it.
  // This is what the portals-per-page cap is for: every candidate is a 30-80ms
  // scrypt call and libuv's threadpool is four threads.
  //
  // Success mints a PORTAL session, never a page session. The credential proven
  // is the portal's, and a pgs<id> cookie would outlive the membership that
  // justified it — it binds to the page's own password, which this viewer never
  // demonstrated.
  for (const portal of portals) {
    if (await pagecookie.verifyPassword(password, portal.password_hash)) {
      await passwordgate.clearPortalFailures(portal.id);
      res.setHeader(
        "Set-Cookie",
        pagecookie.portalSessionCookieHeader(portal.id, { secure: isSecure(req), passwordHash: portal.password_hash })
      );
      return res.redirect(303, "/" + slug);
    }
  }

  // Failed. Charge EVERY counter this attempt tested — the page's and each
  // portal's — because the alternative gives an attacker one budget per door
  // against a secret worth the same at all of them. The applied delay is the
  // largest, so adding a door can never make guessing cheaper. Progressive
  // per-counter backoff (lib/passwordgate.js) is shared across all source IPs, so
  // a distributed run cannot buy a fresh budget per address, and it is
  // deliberately a delay rather than a lockout an attacker could weaponize
  // against the page's real viewers.
  const delays = [];
  if (page.password_hash) delays.push(await passwordgate.recordFailure(page.id));
  for (const portal of portals) delays.push(await passwordgate.recordPortalFailure(portal.id));
  const delayMs = Math.max(0, ...delays);
  if (delayMs > 0) await sleep(delayMs);
  return res
    .status(401)
    .set(gateHeaders())
    .type("html")
    .send(
      gatePage({
        slug,
        message: "Incorrect password.",
        showForm: true,
        credential: page.password_hash ? (portals.length ? "either" : "page") : "portal",
      })
    );
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function escapeAttr(s) {
  return escapeHtml(s);
}

// The pure page renderers are exported for deterministic browser fixtures. Live
// requests still enter only through serve/unlock and keep their existing CSP.
module.exports = {
  serve,
  unlock,
  gatePage,
  portalIndexPage,
  notFoundPage,
  rateLimitPage,
  busyPage,
  serverErrorPage,
  expiredLinkPage,
  // Pure, and exported so the tie-break between two held portal cookies can be
  // pinned without a database: it decides which sibling list a page shows.
  authorizingPortal,
  // Likewise pure: the switcher payload's bounds and its scoping to one portal
  // are the two properties worth pinning without a render.
  buildNav,
  // Pinned directly: the partner's freshness line is the one claim on this
  // surface that can be wrong without looking wrong.
  whenPhrase,
  NAV_MAX_ENTRIES,
  NAV_MAX_BYTES,
};
