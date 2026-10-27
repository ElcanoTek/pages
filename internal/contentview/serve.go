// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/passwordgate"
	"github.com/ElcanoTek/pages/internal/passwordvault"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/render"
)

// url-safe segments only (mirrors versions.normalizeSlug); anything else → no page.
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$`)

var portalCookieRE = regexp.MustCompile(`^pgp[0-9]+$`)

// normalizeSlug is slugFromReq: the decoded wildcard, lowercased, or "" when
// it is not a well-formed slug.
func normalizeSlug(raw string) string {
	s := strings.ToLower(raw)
	if !slugRE.MatchString(s) {
		return ""
	}
	return s
}

func isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.Split(httpx.Header(r, "X-Forwarded-Proto"), ",")[0] == "https"
}

func sendWith(w http.ResponseWriter, r *http.Request, h csp.Headers, status int, html string) {
	h.Apply(w.Header())
	httpx.SendHTML(w, r, status, html)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	sendWith(w, r, csp.RawHeaders(), http.StatusNotFound, NotFoundPage())
}

func gate(w http.ResponseWriter, r *http.Request, status int, html string) {
	sendWith(w, r, csp.GateHeaders(csp.GateOptions{}), status, html)
}

// attemptBusy answers a password attempt that did not get its credential's
// turn (passwordgate.Turns) in time: 429 with the gate headers, nothing
// verified, nothing charged. Like the dashboard's password limiter page it
// carries no ETag: a 429 is never fresh, and an answer on the password path
// must not put a SHA-1 of its body in a header (httpx.SendNoValidator).
func attemptBusy(w http.ResponseWriter, r *http.Request, slug string) {
	csp.GateHeaders(csp.GateOptions{}).Apply(w.Header())
	w.Header().Set("Retry-After", strconv.Itoa(int(passwordgate.TurnWait/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	httpx.SendNoValidator(w, r, http.StatusTooManyRequests, "text/html; charset=utf-8", []byte(AttemptBusyPage(slug)))
}

// defaultRecordTimeout bounds the attempt's database writes (recording a
// failure, clearing the counter after a success). The write is
// detached from the request (a client that disconnects must not make a guess
// free) but never unbounded: a stalled database must not keep the attempt,
// and so its credential's turn, held forever.
const defaultRecordTimeout = 5 * time.Second

func (h *handler) recordCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := h.recordTimeout
	if d <= 0 {
		d = defaultRecordTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// attemptTurn is a password attempt's hold on its credentials' turns.
type attemptTurn struct {
	release func()
	hold    time.Duration
	since   time.Time
	now     bool // release as soon as the attempt returns (tests)
}

func (h *handler) takeTurn(ctx context.Context, keys ...string) (*attemptTurn, bool) {
	release, ok := h.turns.Acquire(ctx, h.turnWait, keys...)
	if !ok {
		return nil, false
	}
	return &attemptTurn{release: release, now: h.sleepHook != nil}, true
}

// holdFor keeps the turn for d from now, the failure delay: the next attempt
// on these credentials is verified only after it, even if this request ends
// first (a client that disconnects cancels the request's sleep, and must not
// thereby skip the delay).
func (t *attemptTurn) holdFor(d time.Duration) { t.hold, t.since = d, time.Now() }

// releaseTurns releases turns at once: a verified attempt has no delay to
// serve, and its best-effort writes must not hold the credential.
func releaseTurns(turns []*attemptTurn) {
	for _, t := range turns {
		t.done()
	}
}

// done releases the turn (after any hold); calling it again is harmless.
func (t *attemptTurn) done() {
	if t.now || t.hold <= 0 {
		t.release()
		return
	}
	if left := t.hold - time.Since(t.since); left > 0 {
		time.AfterFunc(left, t.release)
		return
	}
	t.release()
}

func (h *handler) sleep(ctx context.Context, d time.Duration) {
	if h.sleepHook != nil {
		h.sleepHook(d)
		return
	}
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// navPayload — the one query on the RENDER path, and the reason a failure is
// swallowed: a membership lookup that times out must not turn a live client
// dashboard into a 500. The page renders identically without a switcher, which
// is the difference between a degraded feature and an outage.
func (h *handler) navPayload(ctx context.Context, portal *Portal, slug string) any {
	if portal == nil {
		return nil
	}
	rows, err := h.store.GetPortalPages(ctx, portal.ID)
	if err != nil {
		slog.Error("contentview: page switcher payload unavailable", "err", err)
		return nil
	}
	nav := BuildNav(*portal, rows, slug)
	return &nav
}

func (h *handler) renderLive(w http.ResponseWriter, r *http.Request, page *PublicPage, portal *Portal) {
	nav := h.navPayload(r.Context(), portal, page.Slug)
	html := render.RenderVersion(render.Renderable{
		HTML: page.HTML, RenderMode: page.RenderMode, OverrideCSS: page.OverrideCSS, Nav: nav,
	})
	sendWith(w, r, csp.RawHeaders(), http.StatusOK, html)
}

// portalsForPage — membership as the gate sees it, read live on every request.
//
// Deliberately swallows a failure. This lookup can only ever grant access, so
// an empty result is incapable of opening something it should not: the
// request falls through to exactly the gate it would have got before portals
// existed. A failure, by contrast, would take the gate out for every page on
// the host. Fail-safe here means fail-closed by construction.
func (h *handler) portalsForPage(ctx context.Context, pageID int64) []Portal {
	portals, err := h.store.GetPortalsForPage(ctx, pageID)
	if err != nil {
		slog.Error("contentview: portal membership lookup failed", "err", err)
		return nil
	}
	return portals
}

// portalSlugOf — is this request for the partner entry point rather than a
// page? Exactly two segments, `portal/<portal-slug>`; a portal slug is one
// segment by construction, so anything deeper falls through to the page lookup.
func portalSlugOf(slug string) (string, bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] != "portal" {
		return "", false
	}
	return parts[1], true
}

// portalLockOf — POST /portal/<slug>/lock, the scriptless sign-out. `portal`
// is a reserved slug segment, so no page can live at this address.
func portalLockOf(slug string) (string, bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 3 || parts[0] != "portal" || parts[2] != "lock" {
		return "", false
	}
	return parts[1], true
}

// serve is GET /<slug> — the direct-serve entry point.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, rawSlug string) error {
	ctx := r.Context()
	slug := normalizeSlug(rawSlug)
	if slug == "" {
		notFound(w, r)
		return nil
	}
	// The portal entry point is claimed BEFORE any page lookup. That ordering
	// is what makes a page at this address unable to seize a partner's
	// bookmarked URL, and `portal` is a reserved slug segment so one cannot be
	// created; together neither defence has to be perfect on its own. Handled
	// inside the wildcard so it inherits the read limiter on GET and the
	// password limiter on POST.
	if ps, ok := portalSlugOf(slug); ok {
		return h.servePortalIndex(w, r, ps)
	}

	page, err := h.store.GetPublicPage(ctx, slug)
	if err != nil {
		return err
	}
	if page == nil || page.Disabled || page.PublishedVersionID == 0 {
		notFound(w, r)
		return nil
	}

	// Broker token: exchange for a short session cookie, then the clean URL.
	//
	// Only purpose "session" — minted solely by the dashboard's staff-gated
	// /view broker — buys a page session. A "view" token is a RENDER
	// credential: /admin mints one for any version to preview it, including
	// the PUBLISHED one, so simply opening /admin/<slug> mints one. Redeemed
	// here, that read-only preview URL would become an hour-long session for
	// the whole live page, bypassing the client password. So the audiences are
	// disjoint by purpose, and the version equality stays as defence in depth.
	if t, ok := httpx.QueryParam(r, "t").String(); ok {
		c := rawtoken.Verify(t)
		// 0 is a live sentinel (template tokens carry pid 0), so require a
		// real positive version id rather than trusting coercion.
		if c != nil && c.Purpose == rawtoken.Session && c.PageID == page.ID &&
			c.VersionID > 0 && c.VersionID == page.PublishedVersionID {
			// The floor already set Cache-Control: no-store, which matters
			// most on exactly this response: it carries a page credential.
			w.Header().Set("Set-Cookie", pagecookie.SessionCookieHeader(page.ID, pagecookie.CookieOptions{
				TTLSeconds: 3600, Insecure: !isSecure(r), PasswordHash: page.PasswordHash,
			}))
			httpx.Redirect(w, r, http.StatusFound, "/"+slug)
			return nil
		}
	}

	cookies := httpx.RequestCookies(r)
	// Sessions are bound to the credential state that minted them: a password
	// change (or set/clear) invalidates every prior cookie for this page.
	tok, has := cookies[pagecookie.CookieName(page.ID)]
	pageSession := has && pagecookie.VerifySession(tok, page.ID, page.PasswordHash)
	// The membership query is skipped when it cannot change the answer: a
	// valid page session and no pgp cookie in the jar means there is no portal
	// to authorise and no switcher to populate. A partner holding BOTH still
	// gets their switcher, which is why the page session alone is not enough.
	holdsPortalCookie := false
	for name := range cookies {
		if portalCookieRE.MatchString(name) {
			holdsPortalCookie = true
			break
		}
	}

	// Portal authorisation. Disabled, unpublished and unknown still 404 first,
	// so no portal credential can reach a page those rules already closed.
	//
	// A portal-authorised render sets NO cookie, ever. Minting a pgs<page_id>
	// here would bind to the PAGE's own password, so removing the page from
	// the portal — or rotating the portal password — would revoke nothing for
	// thirty days. Membership is read live instead, on every request, and that
	// is the whole mechanism by which access is taken away.
	var portals []Portal
	if !pageSession || holdsPortalCookie {
		portals = h.portalsForPage(ctx, page.ID)
	}
	portal := AuthorizingPortal(cookies, portals)
	if pageSession || portal != nil {
		h.renderLive(w, r, page, portal)
		return nil
	}

	if page.PasswordHash != "" {
		cred := "page"
		if len(portals) > 0 {
			cred = "either"
		}
		gate(w, r, http.StatusUnauthorized, GatePage(GatePageOptions{Slug: slug, ShowForm: true, Credential: cred}))
		return nil
	}
	if len(portals) > 0 {
		// A member with no password of its own — the staff-only page a human
		// deliberately reclassified by adding it to a portal. Its portal
		// credential opens it, so it gets a prompt rather than the staff notice.
		gate(w, r, http.StatusUnauthorized, GatePage(GatePageOptions{Slug: slug, ShowForm: true, Credential: "portal"}))
		return nil
	}
	// Staff-only (no password): no SSO on this origin → route via the dashboard.
	gate(w, r, http.StatusForbidden, GatePage(GatePageOptions{Slug: slug}))
	return nil
}

// servePortalIndex is GET /portal/<slug>. An unknown or retired portal is a
// 404 like any other unknown address: which portals exist is not public.
func (h *handler) servePortalIndex(w http.ResponseWriter, r *http.Request, portalSlug string) error {
	ctx := r.Context()
	portal, err := h.store.GetPublicPortal(ctx, portalSlug)
	if err != nil {
		return err
	}
	if portal == nil {
		notFound(w, r)
		return nil
	}
	cookies := httpx.RequestCookies(r)
	if tok, ok := cookies[pagecookie.PortalCookieName(portal.ID)]; ok &&
		pagecookie.VerifyPortalSession(tok, portal.ID, portal.PasswordHash) {
		// Membership is read live, per request — never cached into the
		// session — so adding or removing a dashboard takes effect on the
		// partner's next load.
		pages, err := h.store.GetPortalPages(ctx, portal.ID)
		if err != nil {
			return err
		}
		gate(w, r, http.StatusOK, PortalIndexPage(PortalIndexOptions{Portal: *portal, Pages: pages}))
		return nil
	}
	gate(w, r, http.StatusUnauthorized, PortalIndexPage(PortalIndexOptions{Portal: *portal, ShowForm: true}))
	return nil
}

// unlockPortal is POST /portal/<slug> — the portal password submission.
func (h *handler) unlockPortal(w http.ResponseWriter, r *http.Request, portalSlug, password string) error {
	ctx := r.Context()
	portal, err := h.store.GetPublicPortal(ctx, portalSlug)
	if err != nil {
		return err
	}
	if portal == nil {
		notFound(w, r)
		return nil
	}
	turn, ok := h.takeTurn(ctx, passwordgate.PortalKey(portal.ID))
	if !ok {
		attemptBusy(w, r, "portal/"+portal.Slug)
		return nil
	}
	defer turn.done()
	if !pagecookie.VerifyPassword(password, portal.PasswordHash) {
		// Progressive backoff on the PORTAL's shared counter, which every door
		// testing this password charges — see migrations/019. Recorded even
		// if the client has gone: disconnecting must not make a guess free.
		rctx, cancel := h.recordCtx(ctx)
		delay, err := passwordgate.RecordPortalFailure(rctx, h.db, portal.ID)
		cancel()
		if err != nil {
			return err
		}
		turn.holdFor(delay)
		h.sleep(ctx, delay)
		gate(w, r, http.StatusUnauthorized, PortalIndexPage(PortalIndexOptions{
			Portal: *portal, ShowForm: true, Message: "Incorrect password.",
		}))
		return nil
	}
	// Verified: the turn is not needed for the best-effort writes below, and
	// they get a bounded context, so a stalled database cannot keep the
	// credential's next attempts waiting.
	turn.done()
	wctx, cancel := h.recordCtx(ctx)
	defer cancel()
	passwordgate.ClearPortalFailures(wctx, h.db, portal.ID)
	// A portal password set before staff-readable copies existed gets one now,
	// from the submission that just verified. Best effort.
	passwordvault.RememberOnSignIn(wctx, h.db, passwordvault.Portal, portal.ID, portal.PasswordHash, password)
	w.Header().Set("Set-Cookie", pagecookie.PortalSessionCookieHeader(portal.ID, pagecookie.CookieOptions{
		Insecure: !isSecure(r), PasswordHash: portal.PasswordHash,
	}))
	httpx.Redirect(w, r, http.StatusSeeOther, "/portal/"+portal.Slug) // 303 → browser GETs the index
	return nil
}

// lockPortal is POST /portal/<slug>/lock — end the session on this device.
//
// It clears the portal cookie AND the page cookie of every dashboard currently
// in the portal: a partner who typed a page's own password once holds that
// page's cookie too, and a "Sign out" that leaves a dashboard open on a shared
// laptop is worse than no button. What it cannot reach is a page since removed
// from the portal — a bounded residue, and why the copy says "this device". It
// clears this BROWSER's copy rather than revoking the token; rotating the
// password is what revokes.
//
// No CSRF token, like the unlock form beside it: both cookies are
// SameSite=Lax, so a cross-site POST arrives without them, and the worst a
// forged one achieves is signing the reader out.
func (h *handler) lockPortal(w http.ResponseWriter, r *http.Request, portalSlug string) error {
	ctx := r.Context()
	portal, err := h.store.GetPublicPortal(ctx, portalSlug)
	if err != nil {
		return err
	}
	if portal == nil {
		notFound(w, r)
		return nil
	}
	opts := pagecookie.CookieOptions{Insecure: !isSecure(r)}
	w.Header().Add("Set-Cookie", pagecookie.ClearedPortalSessionCookieHeader(portal.ID, opts))
	// Best effort: a membership read that fails must not turn a sign-out into
	// a 500, because the portal cookie above is the one that matters.
	pages, err := h.store.GetPortalPages(ctx, portal.ID)
	if err != nil {
		slog.Error("contentview: portal sign-out could not enumerate pages", "err", err)
	}
	for _, p := range pages {
		w.Header().Add("Set-Cookie", pagecookie.ClearedSessionCookieHeader(p.ID, opts))
	}
	// 303 back to the index, which now renders the password form — the reader
	// sees the sign-out took effect rather than being told it did.
	httpx.Redirect(w, r, http.StatusSeeOther, "/portal/"+portal.Slug)
	return nil
}

// unlock is POST /<slug> — the password form submission.
func (h *handler) unlock(w http.ResponseWriter, r *http.Request, rawSlug, password string) error {
	ctx := r.Context()
	slug := normalizeSlug(rawSlug)
	if slug == "" {
		notFound(w, r)
		return nil
	}
	if ls, ok := portalLockOf(slug); ok {
		return h.lockPortal(w, r, ls)
	}
	if ps, ok := portalSlugOf(slug); ok {
		return h.unlockPortal(w, r, ps, password)
	}

	page, err := h.store.GetPublicPage(ctx, slug)
	if err != nil {
		return err
	}
	if page == nil || page.Disabled || page.PublishedVersionID == 0 {
		notFound(w, r)
		return nil
	}
	portals := h.portalsForPage(ctx, page.ID)
	// No credential of any kind opens this page here: no password of its own,
	// and in no portal. Same 404 as before — the form that would post here is
	// not shown.
	if page.PasswordHash == "" && len(portals) == 0 {
		notFound(w, r)
		return nil
	}

	// Each credential is verified only under its own turn (passwordgate.Turns),
	// taken BEFORE it is checked and kept through the failure delay below, so
	// guesses sent in parallel are verified one backoff step apart. The page's
	// own password is checked under the page's turn alone: someone holding a
	// portal's turn must not be able to keep a member page's direct-password
	// viewers out. Portal turns are taken only when portal passwords are
	// actually about to be tested.
	var turns []*attemptTurn
	defer func() {
		for _, t := range turns {
			t.done()
		}
	}()
	secure := isSecure(r)
	if page.PasswordHash != "" {
		turn, ok := h.takeTurn(ctx, passwordgate.PageKey(page.ID))
		if !ok {
			attemptBusy(w, r, slug)
			return nil
		}
		turns = append(turns, turn)
		if pagecookie.VerifyPassword(password, page.PasswordHash) {
			releaseTurns(turns)
			wctx, cancel := h.recordCtx(ctx)
			defer cancel()
			passwordgate.ClearFailures(wctx, h.db, page.ID)
			passwordvault.RememberOnSignIn(wctx, h.db, passwordvault.Page, page.ID, page.PasswordHash, password)
			// Carries a 30-day page credential; see the note on the broker exchange.
			w.Header().Set("Set-Cookie", pagecookie.SessionCookieHeader(page.ID, pagecookie.CookieOptions{
				Insecure: !secure, PasswordHash: page.PasswordHash,
			}))
			httpx.Redirect(w, r, http.StatusSeeOther, "/"+slug)
			return nil
		}
	}
	portalsTested := false
	if len(portals) > 0 {
		keys := make([]string, 0, len(portals))
		for _, portal := range portals {
			keys = append(keys, passwordgate.PortalKey(portal.ID))
		}
		if turn, ok := h.takeTurn(ctx, keys...); ok {
			turns = append(turns, turn)
			portalsTested = true
		}
	}
	if !portalsTested && len(portals) > 0 && page.PasswordHash == "" {
		// Nothing was verified.
		attemptBusy(w, r, slug)
		return nil
	}
	// A partner who bookmarked one dashboard holds their PORTAL password, not
	// this page's, so the same submission is tried against each portal
	// containing it (which is what the portals-per-page cap is for: each is a
	// scrypt call). Success mints a PORTAL session, never a page session: the
	// credential proven is the portal's, and a pgs<id> cookie would outlive the
	// membership that justified it.
	for _, portal := range portals {
		if !portalsTested {
			break
		}
		if pagecookie.VerifyPassword(password, portal.PasswordHash) {
			releaseTurns(turns)
			wctx, cancel := h.recordCtx(ctx)
			defer cancel()
			passwordgate.ClearPortalFailures(wctx, h.db, portal.ID)
			passwordvault.RememberOnSignIn(wctx, h.db, passwordvault.Portal, portal.ID, portal.PasswordHash, password)
			w.Header().Set("Set-Cookie", pagecookie.PortalSessionCookieHeader(portal.ID, pagecookie.CookieOptions{
				Insecure: !secure, PasswordHash: portal.PasswordHash,
			}))
			httpx.Redirect(w, r, http.StatusSeeOther, "/"+slug)
			return nil
		}
	}

	// Failed. Charge EVERY counter this attempt tested — the page's and each
	// portal's that was tried — because the alternative gives an attacker one budget per door
	// against a secret worth the same at all of them. The applied delay is the
	// largest, so adding a door can never make guessing cheaper. The backoff is
	// shared across all source IPs and is deliberately a delay rather than a
	// lockout an attacker could weaponise against the page's real viewers.
	// The counters are charged even if the client has already disconnected,
	// so abandoning a request cannot make a guess free.
	var delay time.Duration
	rctx, cancel := h.recordCtx(ctx)
	defer cancel()
	if page.PasswordHash != "" {
		d, err := passwordgate.RecordFailure(rctx, h.db, page.ID)
		if err != nil {
			return err
		}
		delay = max(delay, d)
	}
	for _, portal := range portals {
		if !portalsTested {
			break
		}
		d, err := passwordgate.RecordPortalFailure(rctx, h.db, portal.ID)
		if err != nil {
			return err
		}
		delay = max(delay, d)
	}
	for _, t := range turns {
		t.holdFor(delay)
	}
	if !portalsTested && len(portals) > 0 {
		// The page's password was checked and charged; the portal passwords
		// could not be tested now. Say so rather than "incorrect".
		h.sleep(ctx, delay)
		attemptBusy(w, r, slug)
		return nil
	}
	h.sleep(ctx, delay)
	cred := "portal"
	if page.PasswordHash != "" {
		cred = "page"
		if len(portals) > 0 {
			cred = "either"
		}
	}
	gate(w, r, http.StatusUnauthorized, GatePage(GatePageOptions{
		Slug: slug, Message: "Incorrect password.", ShowForm: true, Credential: cred,
	}))
	return nil
}
