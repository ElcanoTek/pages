// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/passwordgate"
	"github.com/ElcanoTek/pages/internal/passwordvault"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/testdb"
)

const (
	contentHost = "content.localhost"
	liveHTML    = `<!doctype html><html><head><title>Live</title></head><body><h1>Live page</h1><script>chart()</script></body></html>`
)

// faultyStore lets a test force one lookup to fail.
type faultyStore struct {
	Store
	mu           sync.Mutex
	publicPage   error
	portalPages  error
	portalsForPg error
}

func (s *faultyStore) set(f func()) { s.mu.Lock(); f(); s.mu.Unlock() }

func (s *faultyStore) GetPublicPage(ctx context.Context, slug string) (*PublicPage, error) {
	s.mu.Lock()
	err := s.publicPage
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.GetPublicPage(ctx, slug)
}

func (s *faultyStore) GetPortalPages(ctx context.Context, id int64) ([]PortalPage, error) {
	s.mu.Lock()
	err := s.portalPages
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.GetPortalPages(ctx, id)
}

func (s *faultyStore) GetPortalsForPage(ctx context.Context, id int64) ([]Portal, error) {
	s.mu.Lock()
	err := s.portalsForPg
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.GetPortalsForPage(ctx, id)
}

type fixture struct {
	t      *testing.T
	d      *db.DB
	h      *handler
	store  *faultyStore
	delays []time.Duration
	saves  []DeckSave
	saveFn func(DeckSave) (DeckSaveResult, error)
	limits map[string]int
	mu     sync.Mutex
}

type resp struct {
	status int
	header http.Header
	body   string
	csp    string
}

func (r resp) cookies() []string { return r.header.Values("Set-Cookie") }

func (r resp) cookie(re string) string {
	rx := regexp.MustCompile(re)
	for _, c := range r.cookies() {
		pair := strings.SplitN(c, ";", 2)[0]
		if rx.MatchString(pair) {
			return pair
		}
	}
	return ""
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("RAW_TOKEN_SECRET", "contentview-test-raw-secret")
	t.Setenv("PAGE_COOKIE_SECRET", "contentview-test-cookie-secret")
	t.Setenv("CONTENT_ORIGIN", "http://content.localhost")
	t.Setenv("DASHBOARD_ORIGIN", "http://localhost")
	d := testdb.New(t)
	f := &fixture{t: t, d: d, limits: map[string]int{}}
	f.store = &faultyStore{Store: NewStore(d)}
	limiter := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f.mu.Lock()
				f.limits[name]++
				f.mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}
	}
	h := ContentHandler(Deps{
		DB:            d,
		Store:         f.store,
		ContentLimit:  limiter("content"),
		PasswordLimit: limiter("password"),
		Ready:         http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ready") }),
		TemplatePreview: func(_ context.Context, id int64) (string, error) {
			if id == 77 {
				return "<html><head></head><body><p>template 77</p></body></html>", nil
			}
			return "", apierror.NotFound("template revision not found", "template_revision_not_found", nil)
		},
		SaveDeck: func(_ context.Context, in DeckSave) (DeckSaveResult, error) {
			f.mu.Lock()
			f.saves = append(f.saves, in)
			fn := f.saveFn
			f.mu.Unlock()
			if fn != nil {
				return fn(in)
			}
			return DeckSaveResult{VersionID: 4242, Status: "draft"}, nil
		},
		ClientIP: func(*http.Request) string { return "203.0.113.9" },
	}).(*handler)
	h.sleepHook = func(d time.Duration) {
		f.mu.Lock()
		f.delays = append(f.delays, d)
		f.mu.Unlock()
	}
	f.h = h
	return f
}

type reqOpt struct {
	cookie  string
	form    url.Values
	body    string
	ctype   string
	headers map[string]string
}

func (f *fixture) do(method, target string, o reqOpt) resp {
	f.t.Helper()
	var body io.Reader
	if o.form != nil {
		body = strings.NewReader(o.form.Encode())
	} else if o.body != "" {
		body = strings.NewReader(o.body)
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = contentHost
	if o.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else if o.body != "" {
		ct := o.ctype
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		req.Header.Set("Content-Type", ct)
	}
	if o.cookie != "" {
		req.Header.Set("Cookie", o.cookie)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.String(), csp: rec.Header().Get("Content-Security-Policy")}
}

func (f *fixture) get(target string, cookie ...string) resp {
	o := reqOpt{}
	if len(cookie) > 0 {
		o.cookie = strings.Join(cookie, "; ")
	}
	return f.do(http.MethodGet, target, o)
}

func (f *fixture) post(target, password string) resp {
	return f.do(http.MethodPost, target, reqOpt{form: url.Values{"password": {password}}})
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	if err := f.d.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (f *fixture) createPage(slug string) int64 {
	return f.id(`INSERT INTO pages (slug, title) VALUES ($1, $2) RETURNING id`, slug, "Title of "+slug)
}

func (f *fixture) version(pageID int64, html, mode, status string) int64 {
	sum := sha256.Sum256([]byte(html))
	return f.id(`INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author)
	              VALUES ($1, $2, $3, $4::version_status, $5::render_mode, 'qa@elcanotek.com') RETURNING id`,
		pageID, html, hex.EncodeToString(sum[:]), status, mode)
}

// live creates a page with one published themed version.
func (f *fixture) live(slug string) (pageID, versionID int64) {
	pageID = f.createPage(slug)
	versionID = f.version(pageID, liveHTML, "themed", "approved")
	f.exec(`UPDATE pages SET published_version_id = $2 WHERE id = $1`, pageID, versionID)
	return pageID, versionID
}

func (f *fixture) setPassword(slug, pw string) {
	f.t.Helper()
	if pw == "" {
		f.exec(`UPDATE pages SET password_hash = NULL WHERE slug = $1`, slug)
		return
	}
	hash, err := pagecookie.HashPassword(pw)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE pages SET password_hash = $2 WHERE slug = $1`, slug, hash)
}

func (f *fixture) portal(slug, name, pw string) int64 {
	f.t.Helper()
	hash, err := pagecookie.HashPassword(pw)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.id(`INSERT INTO page_portals (slug, name, password_hash) VALUES ($1, $2, $3) RETURNING id`, slug, name, hash)
}

func (f *fixture) portalHash(id int64) string {
	var h string
	if err := f.d.QueryRow(context.Background(), `SELECT password_hash FROM page_portals WHERE id = $1`, id).Scan(&h); err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fixture) rotatePortal(id int64, pw string) {
	hash, _ := pagecookie.HashPassword(pw)
	f.exec(`UPDATE page_portals SET password_hash = $2 WHERE id = $1`, id, hash)
}

func (f *fixture) portalCookie(id int64) string {
	return pagecookie.PortalCookieName(id) + "=" + pagecookie.MintPortalSession(id, 600, f.portalHash(id))
}

func (f *fixture) member(portalID, pageID int64, sort int) {
	f.exec(`INSERT INTO page_portal_members (portal_id, page_id, sort_order) VALUES ($1, $2, $3)`, portalID, pageID, sort)
}

func (f *fixture) unmember(portalID, pageID int64) {
	f.exec(`DELETE FROM page_portal_members WHERE portal_id = $1 AND page_id = $2`, portalID, pageID)
}

func (f *fixture) count(sql string, args ...any) int {
	var n int
	if err := f.d.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func mint(t *testing.T, p rawtoken.MintParams, ttl int) string {
	t.Helper()
	tok, err := rawtoken.Mint(p, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func q(tok string) string { return "?t=" + url.QueryEscape(tok) }

func expect(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

// ── /raw (test/integration.js) ─────────────────────────────────────────────

func TestRawRender(t *testing.T) {
	f := newFixture(t)
	pid, vid := f.live("northwind")
	chart := `<canvas id=c></canvas><script>chart()</script>`
	vChart := f.version(pid, "<html><head></head><body>"+chart+"</body></html>", "themed", "draft")
	downPage, downV := f.live("down")
	f.exec(`UPDATE pages SET disabled = true WHERE id = $1`, downPage)

	tok := mint(t, rawtoken.MintParams{VersionID: vChart, Purpose: rawtoken.View, RenderMode: "themed"}, 300)
	ok := f.get("/raw/northwind" + q(tok))
	expect(t, ok.status == 200, "valid render should be 200, got %d", ok.status)
	expect(t, strings.Contains(ok.csp, "sandbox"), "sandbox CSP header present")
	expect(t, strings.Contains(ok.body, `design-tokens.css" data-flag-injected`), "Flag tokens injected")
	expect(t, strings.Contains(ok.body, chart), "chart markup preserved verbatim")
	expect(t, strings.HasPrefix(ok.header.Get("Content-Type"), "text/html; charset=utf-8"), "html")
	expect(t, f.limits["content"] == 1, "the read limiter guards /raw")

	expect(t, f.get("/raw/northwind").status == 403, "missing token → 403")
	expect(t, strings.Contains(f.get("/raw/northwind").body, "This preview link is no longer valid"), "expired page")
	expect(t, f.get("/raw/notnorthwind"+q(tok)).status == 404, "token replayed on wrong slug → 404")
	expect(t, f.get("/raw/northwind/"+q(tok)).status == 404, "a trailing slash is not the slug")
	downTok := mint(t, rawtoken.MintParams{VersionID: downV, Purpose: rawtoken.View, RenderMode: "themed"}, 300)
	expect(t, f.get("/raw/down"+q(downTok)).status == 404, "disabled page (takedown) → 404")

	sess := mint(t, rawtoken.MintParams{PageID: pid, VersionID: vid, Purpose: rawtoken.Session, RenderMode: "themed"}, 120)
	expect(t, f.get("/raw/northwind"+q(sess)).status == 403, "/raw allow-lists view — a session token is refused")
	tmpl := mint(t, rawtoken.MintParams{VersionID: vid, Purpose: rawtoken.Template, RenderMode: "raw"}, 120)
	expect(t, f.get("/raw/northwind"+q(tmpl)).status == 403, "a template token is a different id space")
	expect(t, f.get("/raw/northwind?t=a&t=b").status == 403, "a repeated t is not a string")

	head := f.do(http.MethodHead, "/raw/northwind"+q(tok), reqOpt{})
	expect(t, head.status == 200 && head.body == "", "HEAD renders headers only")
}

func TestRawTemplate(t *testing.T) {
	f := newFixture(t)
	tok := mint(t, rawtoken.MintParams{VersionID: 77, Purpose: rawtoken.Template, RenderMode: "raw"}, 300)
	ok := f.get("/raw-template/77" + q(tok))
	expect(t, ok.status == 200 && strings.Contains(ok.body, "template 77"), "preview renders: %d", ok.status)
	expect(t, strings.Contains(ok.csp, "sandbox"), "a template is the same untrusted HTML")
	expect(t, f.get("/raw-template/78"+q(tok)).status == 403, "bound to this exact revision")
	expect(t, f.get("/raw-template/77").status == 403, "no token")
	view := mint(t, rawtoken.MintParams{VersionID: 77, Purpose: rawtoken.View, RenderMode: "raw"}, 300)
	expect(t, f.get("/raw-template/77"+q(view)).status == 403, "a view token is not a template token")
	gone := mint(t, rawtoken.MintParams{VersionID: 5, Purpose: rawtoken.Template, RenderMode: "raw"}, 300)
	expect(t, f.get("/raw-template/5"+q(gone)).status == 404, "unknown revision → 404")
}

// ── direct serve (test/view.integration.js) ────────────────────────────────

func TestDirectServeGate(t *testing.T) {
	f := newFixture(t)
	pubID, pubV := f.live("pubpage")
	f.setPassword("pubpage", "s3cret")
	staffID, staffV := f.live("staffpage")
	downID, _ := f.live("downpage")
	f.exec(`UPDATE pages SET disabled = true WHERE id = $1`, downID)
	f.createPage("nopub")

	// 1. disabled + unknown + unpublished → 404.
	for _, p := range []string{"/downpage", "/no-such-page", "/nopub", "/", "/Bad.Slug", "/pubpage/"} {
		r := f.get(p)
		expect(t, r.status == 404, "%s → 404, got %d", p, r.status)
		expect(t, strings.Contains(r.csp, "sandbox"), "content-host 404 retains raw-zone sandboxing")
	}
	unknown := f.get("/no-such-page")
	expect(t, strings.Contains(unknown.body, "elcano-mark-primary.svg"), "404 uses approved Elcano branding")
	expect(t, strings.Contains(unknown.body, "Check the link with the person who shared it"), "404 guidance")

	// 2. staff-only → 403 notice, NOT sandboxed, scriptless.
	staff := f.get("/staffpage")
	expect(t, staff.status == 403, "staff page → 403")
	expect(t, regexp.MustCompile(`hasn(&#39;|')t been shared yet`).MatchString(staff.body), "heading names what the reader can do")
	expect(t, strings.Contains(staff.body, "Ask them to share it again"), "who to ask")
	expect(t, strings.Contains(staff.body, "open it from the Pages dashboard"), "staff route kept")
	expect(t, !strings.Contains(strings.ToLower(staff.body), "<script"), "scriptless")
	expect(t, !strings.Contains(staff.csp, "sandbox"), "gate page is not sandboxed")

	// 3. password page → 401 form.
	gate := f.get("/pubpage")
	expect(t, gate.status == 401, "password page → 401")
	expect(t, strings.Contains(gate.body, `name="password"`), "form")
	expect(t, strings.Contains(gate.body, `<label for="page-password">Page password</label>`), "label")
	expect(t, !strings.Contains(gate.csp, "sandbox") && strings.Contains(gate.csp, "form-action 'self'"), "form can submit")
	expect(t, gate.header.Get("ETag") != "", "res.send ETag")

	// 3b. malformed cookies → still the gate.
	junk := f.get("/pubpage", "x=%", "pgs1=%2")
	expect(t, junk.status == 401 && strings.Contains(junk.body, `name="password"`), "malformed cookie → gate, not 500")

	// 4. wrong password → 401, announced, the page counter charged.
	wrong := f.post("/pubpage", "nope")
	expect(t, wrong.status == 401, "wrong → 401")
	expect(t, strings.Contains(wrong.body, "Incorrect") && strings.Contains(wrong.body, `role="alert"`), "announced")
	expect(t, strings.Contains(wrong.body, "Check the password and try again"), "guidance")
	expect(t, len(wrong.cookies()) == 0, "a failed attempt mints nothing")
	expect(t, len(f.delays) == 1 && f.delays[0] == passwordgate.BaseDelay, "first failure waits the base delay: %v", f.delays)
	expect(t, f.limits["password"] == 1, "the brute-force guard runs on POST")

	// 5. correct → 303 + session → sandboxed render.
	ok := f.post("/pubpage", "s3cret")
	expect(t, ok.status == 303 && ok.header.Get("Location") == "/pubpage", "303 to the clean URL: %d", ok.status)
	cookie := ok.cookie(`^pgs\d+=`)
	expect(t, cookie != "", "session cookie set")
	expect(t, f.count(`SELECT count(*) FROM page_password_failures WHERE page_id = $1`, pubID) == 0, "success resets the counter")
	livePage := f.get("/pubpage", cookie)
	expect(t, livePage.status == 200 && strings.Contains(livePage.csp, "sandbox"), "authorized render is sandboxed")
	expect(t, strings.Contains(livePage.body, `design-tokens.css" data-flag-injected`), "Flag injected")
	expect(t, strings.Contains(livePage.body, "<script>chart()</script>"), "page html served")
	expect(t, livePage.header.Get("Cache-Control") == "no-store", "no-store")

	// Secure attribute follows the scheme.
	sc := strings.Join(ok.cookies(), "\n")
	expect(t, !strings.Contains(sc, "Secure"), "plain http drops Secure: %s", sc)
	secure := f.do(http.MethodPost, "/pubpage", reqOpt{form: url.Values{"password": {"s3cret"}}, headers: map[string]string{"X-Forwarded-Proto": "https"}})
	expect(t, strings.Contains(strings.Join(secure.cookies(), ""), "Secure"), "https sets Secure")

	// 7. broker token → session cookie → render.
	brokerTok := mint(t, rawtoken.MintParams{PageID: staffID, VersionID: staffV, Purpose: rawtoken.Session, RenderMode: "themed"}, 120)
	exch := f.get("/staffpage" + q(brokerTok))
	expect(t, exch.status == 302 && exch.header.Get("Location") == "/staffpage", "token exchange → redirect: %d", exch.status)
	sc2 := exch.cookie(`^pgs\d+=`)
	expect(t, sc2 != "", "broker session cookie")
	expect(t, strings.Contains(strings.Join(exch.cookies(), ""), "Max-Age=3600"), "an hour, not thirty days")
	expect(t, f.get("/staffpage", sc2).status == 200, "staff now sees the page")

	// 7a. /admin preview tokens (draft AND published) are not page credentials.
	draft := f.version(pubID, liveHTML+"<!-- draft -->", "themed", "draft")
	for _, vid := range []int64{draft, pubV} {
		tok := mint(t, rawtoken.MintParams{PageID: pubID, VersionID: vid, Purpose: rawtoken.View, RenderMode: "themed"}, 300)
		replay := f.get("/pubpage" + q(tok))
		expect(t, replay.status == 401 && replay.cookie(`^pgs`) == "", "preview token %d must not unlock the live page", vid)
		expect(t, f.get("/raw/pubpage"+q(tok)).status == 200, "but still renders at /raw")
	}
	draftTok := mint(t, rawtoken.MintParams{PageID: pubID, VersionID: draft, Purpose: rawtoken.View, RenderMode: "themed"}, 300)
	expect(t, strings.Contains(f.get("/raw/pubpage"+q(draftTok)).body, "<!-- draft -->"), "renders the exact version")

	// 7b. session token bindings: one page, expiry, the published version.
	wrongPage := f.get("/pubpage" + q(mint(t, rawtoken.MintParams{PageID: staffID, VersionID: pubV, Purpose: rawtoken.Session, RenderMode: "themed"}, 120)))
	expect(t, wrongPage.status == 401 && wrongPage.cookie(`^pgs`) == "", "a session token naming another page")
	expired := f.get("/pubpage" + q(mint(t, rawtoken.MintParams{PageID: pubID, VersionID: pubV, Purpose: rawtoken.Session, RenderMode: "themed"}, -1)))
	expect(t, expired.status == 401, "an expired session token")
	draftSess := f.get("/pubpage" + q(mint(t, rawtoken.MintParams{PageID: pubID, VersionID: draft, Purpose: rawtoken.Session, RenderMode: "themed"}, 120)))
	expect(t, draftSess.status == 401, "a session token for a non-published version")

	// Rotation invalidates sessions.
	f.setPassword("pubpage", "n3w-secret")
	expect(t, f.get("/pubpage", cookie).status == 401, "old session cookie → gate after rotation")
	expect(t, f.post("/pubpage", "s3cret").status == 401, "old password no longer unlocks")
	fresh := f.post("/pubpage", "n3w-secret")
	expect(t, fresh.status == 303, "new password unlocks")
	expect(t, f.get("/pubpage", fresh.cookie(`^pgs\d+=`)).status == 200, "fresh session renders")

	// 8. nested slugs, case-insensitive.
	nestID, nestV := f.live("staff/nest")
	nt := mint(t, rawtoken.MintParams{PageID: nestID, VersionID: nestV, Purpose: rawtoken.Session, RenderMode: "themed"}, 120)
	nexch := f.get("/staff/nest" + q(nt))
	expect(t, nexch.status == 302 && nexch.header.Get("Location") == "/staff/nest", "nested exchange")
	expect(t, f.get("/Staff/Nest", nexch.cookie(`^pgs\d+=`)).status == 200, "nested render, slug lowercased")
	expect(t, f.get("/staff%2Fnest", nexch.cookie(`^pgs\d+=`)).status == 200, "an encoded slash decodes into the slug")
	expect(t, f.get("/bad%E0").status == 400, "a malformed escape is a 400")

	// Unlock on a page no credential opens → 404.
	expect(t, f.post("/staffpage", "x").status == 404, "no password and no portal → 404")
	expect(t, f.post("/no-such-page", "x").status == 404, "unknown")
}

func TestPasswordBackoffChargesEveryDoor(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.live("brakepage")
	f.setPassword("brakepage", "br4ke")
	for i, want := range []time.Duration{passwordgate.BaseDelay, time.Second, 2 * time.Second} {
		r := f.do(http.MethodPost, "/brakepage", reqOpt{
			form:    url.Values{"password": {"bad"}},
			headers: map[string]string{"X-Forwarded-For": "10.9." + strconv.Itoa(i) + ".1"},
		})
		expect(t, r.status == 401, "failure %d → 401", i+1)
		expect(t, f.delays[len(f.delays)-1] == want, "failure %d waits %v, got %v", i+1, want, f.delays)
	}
	n := len(f.delays)
	expect(t, f.post("/brakepage", "br4ke").status == 303, "success unlocks")
	expect(t, len(f.delays) == n, "a success is never delayed")
	f.post("/brakepage", "bad")
	expect(t, f.delays[len(f.delays)-1] == passwordgate.BaseDelay, "counter reset after success")
	_ = pid
}

// The per-page backoff used to delay only the ANSWER to a wrong guess: the
// password had already been checked, so guesses sent in parallel were all
// verified at once. An attempt now holds the page's turn from before
// verification through its failure delay, and one that cannot get the turn
// is refused with a 429 without its password being checked or charged.
func TestParallelPasswordGuessesWaitTheirTurn(t *testing.T) {
	f := newFixture(t)
	pid, _ := f.live("turnpage")
	f.setPassword("turnpage", "turn-secret")
	f.h.turnWait = 50 * time.Millisecond
	inDelay, finish := make(chan struct{}), make(chan struct{})
	f.h.sleepHook = func(time.Duration) {
		close(inDelay)
		<-finish
	}
	first := make(chan resp, 1)
	go func() { first <- f.post("/turnpage", "wrong-1") }()
	<-inDelay // the first guess failed and is serving its delay, turn held

	busy := f.post("/turnpage", "turn-secret")
	expect(t, busy.status == http.StatusTooManyRequests, "a guess during another's delay → 429, got %d", busy.status)
	expect(t, busy.header.Get("Retry-After") != "", "Retry-After")
	expect(t, len(busy.cookies()) == 0, "nothing is verified, nothing minted")
	expect(t, strings.Contains(busy.body, "being checked"), "says why")
	expect(t, f.count(`SELECT fail_count FROM page_password_failures WHERE page_id = $1`, pid) == 1, "the refused attempt is not charged")

	close(finish)
	expect(t, (<-first).status == http.StatusUnauthorized, "the first guess still gets its 401")
	f.h.sleepHook = func(time.Duration) {}
	expect(t, f.post("/turnpage", "turn-secret").status == http.StatusSeeOther, "once the turn is free the right password works")
}

// A guesser that disconnects during its failure delay ends the request (and
// its sleep) early; the turn must still be held for the whole delay, or
// disconnecting would skip the backoff.
func TestAbandonedAttemptKeepsItsDelay(t *testing.T) {
	f := newFixture(t)
	f.live("holdpage")
	f.setPassword("holdpage", "hold-secret")
	f.h.sleepHook = nil // real delays: 500ms after the first failure
	f.h.turnWait = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/holdpage", strings.NewReader(url.Values{"password": {"wrong"}}.Encode())).WithContext(ctx)
	req.Host = contentHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.h.ServeHTTP(httptest.NewRecorder(), req)
	expect(t, ctx.Err() != nil, "the request was abandoned before its delay ended")

	expect(t, f.post("/holdpage", "hold-secret").status == http.StatusTooManyRequests, "the next guess still waits out the delay")
	time.Sleep(passwordgate.BaseDelay)
	expect(t, f.post("/holdpage", "hold-secret").status == http.StatusSeeOther, "and is verified once it has passed")
}

func TestPortalIndex(t *testing.T) {
	f := newFixture(t)
	portalID := f.portal("vw-nwm", "Weather Co (view suite)", "portal-pass-1")

	expect(t, f.get("/portal/nope").status == 404, "unknown portal → 404")
	expect(t, f.get("/portal/vw-nwm/extra").status == 404, "a portal slug is one segment")
	expect(t, f.get("/portal").status == 404, "no index of portals")

	locked := f.get("/portal/vw-nwm")
	expect(t, locked.status == 401, "no session → the portal form")
	expect(t, strings.Contains(locked.body, "Weather Co (view suite)") && strings.Contains(locked.body, "Portal password"), "names the portal")
	expect(t, !strings.Contains(strings.ToLower(locked.body), "<script"), "scriptless")
	expect(t, !strings.Contains(locked.csp, "sandbox") && strings.Contains(locked.csp, "form-action 'self'"), "not sandboxed, posts back to us")
	expect(t, locked.header.Get("Cache-Control") == "no-store" && len(locked.cookies()) == 0, "hands out nothing")

	wrong := f.post("/portal/vw-nwm", "not-it")
	expect(t, wrong.status == 401 && strings.Contains(wrong.body, "Incorrect password") && len(wrong.cookies()) == 0, "wrong portal password")
	expect(t, f.count(`SELECT fail_count FROM portal_password_failures WHERE portal_id = $1`, portalID) == 1, "the portal's counter is charged once")

	opened := f.post("/portal/vw-nwm", "portal-pass-1")
	expect(t, opened.status == 303 && opened.header.Get("Location") == "/portal/vw-nwm", "303 to the index")
	portalCookie := opened.cookie(`^pgp` + strconv.FormatInt(portalID, 10) + `=`)
	expect(t, portalCookie != "", "portal session cookie")
	attrs := strings.Join(opened.cookies(), "")
	expect(t, strings.Contains(attrs, "Path=/;") && strings.Contains(attrs, "HttpOnly") && strings.Contains(attrs, "SameSite=Lax"), "cookie attrs: %s", attrs)
	expect(t, f.count(`SELECT count(*) FROM portal_password_failures WHERE portal_id = $1`, portalID) == 0, "success resets")

	overview, _ := f.live("vw-overview")
	contoso, _ := f.live("vw-contoso")
	draft := f.createPage("vw-draft")
	down, _ := f.live("vw-down")
	f.exec(`UPDATE pages SET disabled = true WHERE id = $1`, down)
	gone, _ := f.live("vw-gone")
	f.member(portalID, overview, 5)
	f.member(portalID, contoso, 1)
	for _, id := range []int64{draft, down, gone} {
		f.member(portalID, id, 5)
	}
	f.exec(`UPDATE pages SET deleted_at = now() WHERE id = $1`, gone)
	f.exec(`UPDATE page_portals SET home_page_id = $2 WHERE id = $1`, portalID, overview)

	listing := f.get("/portal/vw-nwm", portalCookie)
	expect(t, listing.status == 200, "a valid session lists the dashboards: %d", listing.status)
	expect(t, strings.Contains(listing.body, "2 dashboards available to you"), "only the openable ones")
	expect(t, strings.Contains(listing.body, `href="/vw-overview"`) && strings.Contains(listing.body, `href="/vw-contoso"`), "root-absolute hrefs")
	for _, hidden := range []string{"vw-draft", "vw-down", "vw-gone"} {
		expect(t, !strings.Contains(listing.body, `href="/`+hidden+`"`), "%s would 404, so it is not listed", hidden)
	}
	expect(t, strings.Index(listing.body, `href="/vw-overview"`) < strings.Index(listing.body, `href="/vw-contoso"`), "home first")
	expect(t, strings.Contains(listing.body, `<h2 class="portal-section">Start here</h2>`), "home set apart")
	expect(t, regexp.MustCompile(`Updated [^<]+</time>`).MatchString(listing.body), "freshness")
	expect(t, regexp.MustCompile(`<time datetime="\d{4}-\d{2}-\d{2}T`).MatchString(listing.body), "real timestamp")
	expect(t, !strings.Contains(listing.body, "Data as of"), "no envelope, no data claim")
	expect(t, len(listing.cookies()) == 0, "reading re-mints nothing")

	// Source freshness when the live version has an envelope.
	enveloped := f.id(`INSERT INTO page_versions (page_id, html, content_sha256, status, author,
	                     data_sha256, data_template_sha256, source_as_of, refreshed_at)
	                   VALUES ($1, 'x', 'x', 'approved', 'qa@elcanotek.com', repeat('a', 64), repeat('b', 64),
	                     now() - interval '3 days', now()) RETURNING id`, contoso)
	f.exec(`UPDATE pages SET published_version_id = $2 WHERE id = $1`, contoso, enveloped)
	expect(t, strings.Contains(f.get("/portal/vw-nwm", portalCookie).body, "Data as of 3 days ago"), "source_as_of wins")

	// The seizure scenario: a page at the portal's address is never served there.
	seize := f.createPage("portal/vw-nwm")
	sv := f.version(seize, liveHTML, "themed", "approved")
	f.exec(`UPDATE pages SET published_version_id = $2 WHERE id = $1`, seize, sv)
	contested := f.get("/portal/vw-nwm")
	expect(t, contested.status == 401 && strings.Contains(contested.body, "Portal password"), "the portal owns this address")

	f.unmember(portalID, contoso)
	after := f.get("/portal/vw-nwm", portalCookie)
	expect(t, !strings.Contains(after.body, `href="/vw-contoso"`) && strings.Contains(after.body, "1 dashboard available to you"), "membership is read per request")

	// Sign out.
	signedOut := f.do(http.MethodPost, "/portal/vw-nwm/lock", reqOpt{cookie: portalCookie, form: url.Values{}})
	expect(t, signedOut.status == 303 && signedOut.header.Get("Location") == "/portal/vw-nwm", "303 back to the index")
	var clearedPortal string
	clearedPage := false
	for _, c := range signedOut.cookies() {
		if strings.HasPrefix(c, "pgp"+strconv.FormatInt(portalID, 10)+"=;") {
			clearedPortal = c
		}
		if strings.HasPrefix(c, "pgs"+strconv.FormatInt(overview, 10)+"=;") {
			clearedPage = true
		}
	}
	expect(t, clearedPortal != "" && strings.Contains(clearedPortal, "Max-Age=0") && strings.Contains(clearedPortal, "Path=/;"), "portal cookie cleared: %v", signedOut.cookies())
	expect(t, strings.Contains(clearedPortal, "HttpOnly") && strings.Contains(clearedPortal, "SameSite=Lax"), "same attributes")
	expect(t, clearedPage, "member page cookies cleared too")
	expect(t, f.get("/portal/vw-nwm", portalCookie).status == 200, "sign-out clears the device, it does not revoke the token")
	expect(t, f.do(http.MethodPost, "/portal/nope/lock", reqOpt{form: url.Values{}}).status == 404, "unknown portal sign-out → 404")

	// Sign-out survives a failing membership read.
	f.store.set(func() { f.store.portalPages = errors.New("forced") })
	degraded := f.do(http.MethodPost, "/portal/vw-nwm/lock", reqOpt{form: url.Values{}})
	f.store.set(func() { f.store.portalPages = nil })
	expect(t, degraded.status == 303 && len(degraded.cookies()) == 1, "the portal cookie alone is still cleared")

	// Empty portal.
	empty := f.portal("vw-empty", "Nothing Yet", "empty-pass")
	ei := f.get("/portal/vw-empty", f.portalCookie(empty))
	expect(t, ei.status == 200 && strings.Contains(ei.body, "No dashboards are available in this portal yet"), "empty portal says so")

	// A page-session body in a portal cookie is refused.
	forged := pagecookie.PortalCookieName(portalID) + "=" + pagecookie.MintSession(portalID, 600, f.portalHash(portalID))
	expect(t, f.get("/portal/vw-nwm", forged).status == 401, "domain-separated token types")

	// Rotation revokes; retirement 404s.
	f.rotatePortal(portalID, "a-new-portal-secret-x")
	expect(t, f.get("/portal/vw-nwm", portalCookie).status == 401, "rotation revokes every live session")
	f.exec(`UPDATE page_portals SET deleted_at = now() WHERE id = $1`, portalID)
	expect(t, f.get("/portal/vw-nwm").status == 404, "a retired portal is a 404")
}

func TestPortalServePredicate(t *testing.T) {
	f := newFixture(t)
	spID := f.portal("sp-portal", "Serve Predicate Co", "sp-portal-pass")
	otherID := f.portal("sp-other", "Some Other Partner", "sp-other-pass")
	paid, _ := f.live("sp-paid")
	f.setPassword("sp-paid", "page-own-secret")
	staff, _ := f.live("sp-staff")
	f.live("sp-outside")
	f.live("staffpage")
	f.member(spID, paid, 0)
	f.member(spID, staff, 0)
	spCookie := f.portalCookie(spID)
	otherCookie := f.portalCookie(otherID)

	for _, slug := range []string{"sp-paid", "sp-staff"} {
		r := f.get("/"+slug, spCookie)
		expect(t, r.status == 200 && strings.Contains(r.csp, "sandbox"), "%s opens on a portal session: %d", slug, r.status)
		expect(t, strings.Contains(r.body, "<script>chart()</script>"), "the real page")
		expect(t, len(r.cookies()) == 0, "a portal-authorised render mints NO cookie, ever")
	}
	expect(t, f.get("/sp-outside", spCookie).status == 403, "only its own members")
	expect(t, f.get("/sp-staff", otherCookie).status == 401, "another portal's session does not open this one's pages")
	expect(t, f.get("/staffpage", spCookie).status == 403, "a page in no portal is unaffected")
	forged := pagecookie.PortalCookieName(spID) + "=" + pagecookie.MintSession(spID, 600, f.portalHash(spID))
	expect(t, f.get("/sp-staff", forged).status == 401, "a pgs body in a pgp cookie authorises nothing")

	coldPaid := f.get("/sp-paid")
	expect(t, coldPaid.status == 401 && strings.Contains(coldPaid.body, "or your portal password"), "either")
	coldStaff := f.get("/sp-staff")
	expect(t, coldStaff.status == 401 && strings.Contains(coldStaff.body, "Portal password"), "a reclassified member prompts")
	for _, b := range []string{coldPaid.body, coldStaff.body} {
		expect(t, !strings.Contains(b, "Serve Predicate Co") && !strings.Contains(b, "sp-portal"), "the gate never names the portal")
	}

	viaPortal := f.post("/sp-staff", "sp-portal-pass")
	expect(t, viaPortal.status == 303, "portal password at a member page")
	minted := viaPortal.cookie(`^pgp` + strconv.FormatInt(spID, 10) + `=`)
	expect(t, minted != "" && viaPortal.cookie(`^pgs\d+=`) == "", "mints a portal session, never a page session")
	expect(t, f.get("/sp-staff", minted).status == 200, "the minted session opens the page")
	viaPage := f.post("/sp-paid", "page-own-secret")
	expect(t, viaPage.status == 303 && viaPage.cookie(`^pgs\d+=`) != "", "a page password still mints a page session")

	f.exec(`DELETE FROM portal_password_failures`)
	f.exec(`DELETE FROM page_password_failures`)
	expect(t, f.post("/sp-paid", "wrong-everywhere").status == 401, "wrong everywhere")
	expect(t, f.count(`SELECT count(*) FROM portal_password_failures WHERE portal_id = $1`, spID) == 1, "the portal counter is charged")
	expect(t, f.count(`SELECT count(*) FROM page_password_failures WHERE page_id = $1`, paid) == 1, "and the page's own")
	wrongStaff := f.post("/sp-staff", "nope")
	expect(t, wrongStaff.status == 401 && strings.Contains(wrongStaff.body, "Portal password"), "portal-only member retry copy")

	f.exec(`UPDATE pages SET disabled = true WHERE id = $1`, paid)
	expect(t, f.get("/sp-paid", spCookie).status == 404, "disabled still 404s")
	f.exec(`UPDATE pages SET disabled = false WHERE id = $1`, paid)
	nopub := f.createPage("sp-nopub")
	f.member(spID, nopub, 0)
	expect(t, f.get("/sp-nopub", spCookie).status == 404, "unpublished still 404s")

	f.exec(`UPDATE pages SET deleted_at = now(), slug = 'sp-staff-deleted' WHERE id = $1`, staff)
	expect(t, f.get("/sp-staff", spCookie).status == 404, "a soft-deleted member 404s")
	f.live("sp-staff")
	recreated := f.get("/sp-staff", spCookie)
	expect(t, recreated.status == 403, "the recreated page is a stranger to the portal")

	expect(t, f.get("/sp-paid", spCookie).status == 200, "sanity: a member")
	f.unmember(spID, paid)
	expect(t, f.get("/sp-paid", spCookie).status == 401, "removed → closed immediately")
	f.member(spID, paid, 0)
	f.rotatePortal(spID, "rotated-portal-secret")
	expect(t, f.get("/sp-paid", spCookie).status == 401, "rotation revokes at the member page")
	rotated := f.portalCookie(spID)
	expect(t, f.get("/sp-paid", rotated).status == 200, "sanity: new credential opens it")
	f.exec(`UPDATE page_portals SET deleted_at = now() WHERE id = $1`, spID)
	expect(t, f.get("/sp-paid", rotated).status == 401, "retiring the portal takes its members out of reach")

	// A membership lookup that fails must DENY, not 500.
	f.exec(`UPDATE page_portals SET deleted_at = NULL WHERE id = $1`, spID)
	expect(t, f.get("/sp-paid", rotated).status == 200, "sanity")
	f.store.set(func() { f.store.portalsForPg = errors.New("forced failure: membership lookup") })
	degraded := f.get("/sp-paid")
	expect(t, degraded.status == 401 && !strings.Contains(degraded.body, "couldn"), "own gate, not a 500")
	expect(t, f.get("/sp-paid", rotated).status == 401, "fails CLOSED")
	f.store.set(func() { f.store.portalsForPg = nil })
}

var navRE = regexp.MustCompile(`(?s)<script type="application/json" id="pages-nav"[^>]*>(.*?)</script>`)

func navOf(t *testing.T, body string) *Nav {
	t.Helper()
	m := navRE.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var n Nav
	if err := json.Unmarshal([]byte(m[1]), &n); err != nil {
		t.Fatalf("nav json: %v", err)
	}
	return &n
}

func slugs(n *Nav) []string {
	var out []string
	for _, p := range n.Pages {
		out = append(out, p.Slug)
	}
	return out
}

func TestPageSwitcher(t *testing.T) {
	f := newFixture(t)
	one := f.portal("nav-one", "Nav Portal One", "nav-one-pass")
	two := f.portal("nav-two", "Nav Portal Two", "nav-two-pass")
	shared, _ := f.live("nav-shared")
	sibling, _ := f.live("nav-sibling")
	secret, _ := f.live("nav-secret")
	f.member(one, shared, 0)
	f.member(one, sibling, 1)
	f.member(two, shared, 0)
	f.member(two, secret, 1)
	oneCookie, twoCookie := f.portalCookie(one), f.portalCookie(two)

	viaOne := f.get("/nav-shared", oneCookie)
	n1 := navOf(t, viaOne.body)
	expect(t, viaOne.status == 200 && n1 != nil, "a portal-authorised render carries the switcher")
	expect(t, n1.Portal.Slug == "nav-one" && strings.Join(slugs(n1), ",") == "nav-shared,nav-sibling", "portal one's list: %v", slugs(n1))
	expect(t, n1.Pages[0].Current && n1.Pages[0].URL == "http://content.localhost/nav-shared", "marked, absolute")
	viaTwo := f.get("/nav-shared", twoCookie)
	n2 := navOf(t, viaTwo.body)
	expect(t, n2.Portal.Slug == "nav-two" && strings.Join(slugs(n2), ",") == "nav-shared,nav-secret", "portal two's list")
	expect(t, !strings.Contains(viaOne.body, "nav-secret") && !strings.Contains(viaTwo.body, "nav-sibling"), "neither learns of the other")
	both := f.get("/nav-shared", oneCookie, twoCookie)
	expect(t, navOf(t, both.body).Portal.Slug == "nav-one", "two cookies → lowest portal id")

	f.unmember(one, sibling)
	expect(t, strings.Join(slugs(navOf(t, f.get("/nav-shared", oneCookie).body)), ",") == "nav-shared", "live membership")

	f.live("nav-plain")
	f.setPassword("nav-plain", "nav-plain-page-pw")
	plain := f.post("/nav-plain", "nav-plain-page-pw").cookie(`^pgs\d+=`)
	r := f.get("/nav-plain", plain)
	expect(t, r.status == 200 && navOf(t, r.body) == nil, "a page-session render carries no switcher")

	f.setPassword("nav-shared", "nav-shared-page-pw")
	pageSess := f.post("/nav-shared", "nav-shared-page-pw").cookie(`^pgs\d+=`)
	withBoth := f.get("/nav-shared", pageSess, oneCookie)
	expect(t, withBoth.status == 200 && navOf(t, withBoth.body) != nil, "a page session as well must not cost the switcher")

	f.store.set(func() { f.store.portalPages = errors.New("forced failure: switcher membership") })
	deg := f.get("/nav-shared", oneCookie)
	f.store.set(func() { f.store.portalPages = nil })
	expect(t, deg.status == 200 && strings.Contains(deg.body, "<script>chart()</script>") && navOf(t, deg.body) == nil, "renders without a switcher")
}

func deckHTML(title, extra string) string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>` + title + `</title>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; connect-src 'none'">
<script id="fleet-offline-deck">(function(){try{localStorage.setItem("bento-offline","on")}catch(e){}})();</script>
<script type="application/bento+json" id="bento-doc">{"format":"bento/slides","version":1,"docId":"d-vw","title":"` + title + `","slides":[]` + extra + `}</script>
<script id="bento-rt" type="bento/deflate-b64">c8vPz1EoLSjOSczJyU9V0FIoLSktKSpJUUhJVEgQMAAA</script>
</head><body><div id="bento-splash"></div><script>(async()=>{await import(URL.createObjectURL(new Blob([""],{type:"text/javascript"})))})()</script></body></html>`
}

func TestDeckEditSessionAndSaveChannel(t *testing.T) {
	f := newFixture(t)
	origin := "http://content.localhost"
	pid := f.createPage("vw-deck")
	v1 := f.version(pid, deckHTML("Deck v1", ""), "raw", "approved")
	f.exec(`UPDATE pages SET published_version_id = $2 WHERE id = $1`, pid, v1)
	edit := mint(t, rawtoken.MintParams{PageID: pid, VersionID: v1, Purpose: rawtoken.Edit, RenderMode: "raw", Actor: "editor@elcanotek.com"}, 600)
	view := mint(t, rawtoken.MintParams{PageID: pid, VersionID: v1, Purpose: rawtoken.View, RenderMode: "raw"}, 600)

	session := f.get("/raw/vw-deck" + q(edit))
	expect(t, session.status == 200, "an edit token opens the session: %d", session.status)
	expect(t, regexp.MustCompile(`connect-src `+regexp.QuoteMeta(origin)+`(;|$)`).MatchString(session.csp), "connect-src names Pages: %s", session.csp)
	expect(t, strings.Contains(session.csp, "sandbox allow-scripts"), "sandbox untouched")
	expect(t, regexp.MustCompile(`Content-Security-Policy[^>]*connect-src `+regexp.QuoteMeta(origin)).MatchString(session.body), "the deck's own guard is widened")
	expect(t, strings.Contains(session.body, "data-pages-deck-host"), "save channel present")
	expect(t, strings.Contains(session.body, origin+"/raw/vw-deck/versions"), "save URL")

	vr := f.get("/raw/vw-deck" + q(view))
	expect(t, vr.status == 200 && strings.Contains(vr.csp, "connect-src 'none'") && !strings.Contains(vr.body, "Authorization"), "a viewer gets no channel")

	// Newest version wins in an edit session.
	f.version(pid, deckHTML("Deck v2", ""), "raw", "draft")
	expect(t, strings.Contains(f.get("/raw/vw-deck"+q(edit)).body, "Deck v2"), "reopening shows the last save")

	pre := f.do(http.MethodOptions, "/raw/vw-deck/versions", reqOpt{headers: map[string]string{"Origin": "null", "Access-Control-Request-Method": "POST"}})
	expect(t, pre.status == 204 && pre.header.Get("Access-Control-Allow-Origin") == "null", "preflight answered")
	expect(t, strings.Contains(pre.header.Get("Access-Control-Allow-Headers"), "Authorization") && pre.header.Get("Access-Control-Max-Age") == "600", "preflight headers")
	expect(t, pre.header.Get("X-Robots-Tag") != "", "zone floor on the preflight")

	saved := strings.Replace(deckHTML("Deck v3", `,"collab":{"room":"wss://sync.bento.page/d/abc","key":"k"}`), "connect-src 'none'", "connect-src "+origin, 1)
	hdr := map[string]string{"Authorization": "Bearer " + edit, "X-Pages-Base-Version": strconv.FormatInt(v1, 10), "Origin": "null"}
	post := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: hdr})
	expect(t, post.status == 201, "save → 201: %d %s", post.status, post.body)
	expect(t, post.body == `{"version_id":"4242","status":"draft","deduped":false,"collab_keys_removed":true}`, "reply: %s", post.body)
	expect(t, post.header.Get("Access-Control-Allow-Origin") == "null" && strings.HasPrefix(post.header.Get("Content-Type"), "application/json"), "json + cors")
	got := f.saves[len(f.saves)-1]
	expect(t, got.Slug == "vw-deck" && got.PageID == pid && got.Actor == "editor@elcanotek.com" && got.BaseVersion == strconv.FormatInt(v1, 10) && got.IP == "203.0.113.9", "save input: %+v", got)
	expect(t, !strings.Contains(got.HTML, `"collab"`), "collab keys stripped before the saver sees them")

	f.saveFn = func(DeckSave) (DeckSaveResult, error) {
		return DeckSaveResult{VersionID: 4242, Status: "draft", Deduped: true}, nil
	}
	dup := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: hdr})
	expect(t, dup.status == 200 && strings.Contains(dup.body, `"deduped":true`), "dedupe → 200")

	f.saveFn = func(DeckSave) (DeckSaveResult, error) {
		return DeckSaveResult{}, apierror.Conflict("a newer deck version was saved", "stale_deck_version", map[string]string{"latest_version_id": "9"})
	}
	stale := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: hdr})
	expect(t, stale.status == 409 && stale.body == `{"error":"a newer deck version was saved","code":"stale_deck_version","details":{"latest_version_id":"9"}}`, "4xx verbatim: %s", stale.body)
	f.saveFn = func(DeckSave) (DeckSaveResult, error) { return DeckSaveResult{}, errors.New("pg gone") }
	boom := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: hdr})
	expect(t, boom.status == 500 && boom.body == `{"error":"Pages could not store the version"}`, "5xx hides internals: %s", boom.body)
	f.saveFn = nil

	n := len(f.saves)
	expect(t, f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved}).status == 403, "no token")
	expect(t, f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: map[string]string{"Authorization": "Bearer " + view}}).status == 403, "a view token is not a write credential")
	notDeck := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: "<html><body><h1>not a deck</h1></body></html>", headers: map[string]string{"Authorization": "Bearer " + edit}})
	expect(t, notDeck.status == 400 && strings.Contains(notDeck.body, "only a Bento deck"), "only a deck")
	expect(t, f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, ctype: "application/json", headers: map[string]string{"Authorization": "Bearer " + edit}}).status == 400, "a non-text body is no deck")
	exp := mint(t, rawtoken.MintParams{PageID: pid, VersionID: 1, Purpose: rawtoken.Edit, RenderMode: "raw", Actor: "editor@elcanotek.com"}, -1)
	expect(t, f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: saved, headers: map[string]string{"Authorization": "Bearer " + exp}}).status == 403, "expired")
	expect(t, len(f.saves) == n, "nothing refused reached the saver")

	big := f.do(http.MethodPost, "/raw/vw-deck/versions", reqOpt{body: strings.Repeat("x", DefaultMaxRequestBytes+1), headers: hdr})
	expect(t, big.status == 413 && strings.Contains(big.body, "couldn"), "oversize → branded 413: %d", big.status)

	tmpl := mint(t, rawtoken.MintParams{PageID: pid, VersionID: 1, Purpose: rawtoken.Template, RenderMode: "raw"}, 600)
	expect(t, f.get("/raw/vw-deck"+q(tmpl)).status == 403, "a template token cannot open the editor")
	f.version(pid, "<html><body><h1>Replaced by a dashboard</h1></body></html>", "raw", "draft")
	expect(t, f.get("/raw/vw-deck"+q(edit)).status == 403, "the session is over when the page is no longer a deck")
}

func TestZoneHeaderContract(t *testing.T) {
	f := newFixture(t)
	f.live("pubpage")
	f.setPassword("pubpage", "s3cret")
	f.live("staffpage")
	want := []string{"Referrer-Policy", "X-Content-Type-Options", "X-Robots-Tag"}
	cases := []struct {
		label   string
		r       resp
		status  int
		sandbox bool
	}{
		{"terminal 404 (PUT)", f.do(http.MethodPut, "/pubpage", reqOpt{}), 404, true},
		{"terminal 404 (OPTIONS)", f.do(http.MethodOptions, "/pubpage", reqOpt{}), 404, true},
		{"routed 404", f.get("/no-such-page-here"), 404, true},
		{"/raw with no token", f.get("/raw/pubpage"), 403, true},
		{"/raw-template with no token", f.get("/raw-template/1"), 403, true},
		{"password gate", f.get("/pubpage"), 401, false},
		{"staff-only gate", f.get("/staffpage"), 403, false},
	}
	for _, c := range cases {
		expect(t, c.r.status == c.status, "%s: expected %d got %d", c.label, c.status, c.r.status)
		for _, h := range want {
			expect(t, c.r.header.Get(h) != "", "%s: missing %s", c.label, h)
		}
		expect(t, c.r.header.Get("Cache-Control") == "no-store", "%s: must not be cacheable", c.label)
		expect(t, c.r.csp != "" && strings.Contains(c.r.csp, "sandbox") == c.sandbox, "%s: sandbox expectation (%s)", c.label, c.r.csp)
		expect(t, strings.HasPrefix(c.r.header.Get("Content-Type"), "text/html"), "%s: branded HTML", c.label)
	}

	for _, p := range []string{"/healthz", "/assets/flag", "/assets"} {
		r := f.get(p)
		for _, h := range want {
			expect(t, r.header.Get(h) != "", "%s: missing %s", p, h)
		}
		expect(t, r.csp != "" && r.header.Get("X-Powered-By") == "", "%s: csp, no framework", p)
	}
	hz := f.get("/healthz")
	expect(t, hz.status == 200 && hz.body == "ok" && hz.header.Get("Content-Type") == "text/plain; charset=utf-8", "healthz")
	expect(t, f.get("/readyz").body == "ready", "readyz delegated")
	dir := f.get("/assets/flag?x=1")
	expect(t, dir.status == 301 && dir.header.Get("Location") == "/assets/flag/?x=1", "directory redirect: %d %s", dir.status, dir.header.Get("Location"))
	expect(t, strings.Contains(dir.body, "<pre>Redirecting to /assets/flag/?x=1</pre>"), "serve-static body")
	expect(t, f.get("/assets/flag/").status == 404, "a directory with a slash falls through to the 404")

	asset := f.get("/assets/flag/tokens/design-tokens.css")
	expect(t, asset.status == 200, "flag tokens serve")
	expect(t, strings.HasPrefix(asset.csp, "default-src 'none'") && !regexp.MustCompile(`script-src|unsafe-eval|\*`).MatchString(asset.csp), "/assets CSP: %s", asset.csp)
	expect(t, asset.header.Get("Cache-Control") == assetver.Revalidate, "revalidate — not no-store, not immutable")
	expect(t, asset.header.Get("Content-Type") == "text/css; charset=utf-8", "css type: %s", asset.header.Get("Content-Type"))
	etag := asset.header.Get("ETag")
	expect(t, etag != "" && asset.header.Get("Accept-Ranges") == "bytes", "etag + ranges")
	expect(t, f.do(http.MethodGet, "/assets/flag/tokens/design-tokens.css", reqOpt{headers: map[string]string{"If-None-Match": etag}}).status == 304, "revalidation → 304")
	part := f.do(http.MethodGet, "/assets/flag/tokens/design-tokens.css", reqOpt{headers: map[string]string{"Range": "bytes=0-9"}})
	expect(t, part.status == 206 && len(part.body) == 10 && strings.HasPrefix(part.header.Get("Content-Range"), "bytes 0-9/"), "a range")

	badRange := f.do(http.MethodGet, "/assets/flag/tokens/design-tokens.css", reqOpt{headers: map[string]string{"Range": "bytes=99999999-"}})
	expect(t, badRange.status == 416 && badRange.header.Get("X-Robots-Tag") != "", "416 keeps its status: %d", badRange.status)
	expect(t, f.do(http.MethodGet, "/assets/flag/tokens/design-tokens.css", reqOpt{headers: map[string]string{"If-Match": `"not-the-etag"`}}).status == 412, "412 keeps its status")
	expect(t, f.get("/assets/flag/nope.css").status == 404, "a missing asset falls to the 404")
	expect(t, f.get("/assets/flag/../flag/tokens/design-tokens.css").status == 404, "traversal falls through")
	expect(t, f.do(http.MethodHead, "/assets/flag/tokens/design-tokens.css", reqOpt{}).body == "", "HEAD")

	// The versioned form of the same file (what our chrome and the themed
	// render link) is immutable for a year; an older release's version still
	// serves the current bytes, revalidating; HTML stays uncacheable.
	cur := f.get(assetver.ContentFlag() + "/tokens/design-tokens.css")
	expect(t, cur.status == 200 && cur.body == asset.body, "versioned asset serves the same bytes: %d", cur.status)
	expect(t, cur.header.Get("Cache-Control") == "public, max-age=31536000, immutable", "current version is immutable: %s", cur.header.Get("Cache-Control"))
	expect(t, cur.csp == asset.csp && cur.header.Get("ETag") == etag, "versioned asset keeps the asset CSP and ETag")
	expect(t, f.do(http.MethodGet, assetver.ContentFlag()+"/tokens/design-tokens.css", reqOpt{headers: map[string]string{"If-None-Match": etag}}).status == 304, "versioned revalidation → 304")
	stale := f.get("/assets/_v/000000000000/flag/tokens/design-tokens.css")
	expect(t, stale.status == 200 && stale.body == asset.body && stale.header.Get("Cache-Control") == assetver.Revalidate, "a stale version revalidates: %d %s", stale.status, stale.header.Get("Cache-Control"))
	expect(t, f.get(assetver.ContentFlag()+"/nope.css").status == 404, "a missing versioned asset falls to the 404")
	expect(t, f.get(assetver.Content()+"/../flag/tokens/design-tokens.css").status == 404, "versioned traversal falls through")
	expect(t, f.get(assetver.ContentFlag()+"/motion/flag-motion.js").header.Get("Cache-Control") == "public, max-age=31536000, immutable", "the motion kit is versioned too")
	// An unsatisfiable Range on a current-version URL is an error, not an
	// asset: it must not be cached under the asset's immutable policy.
	bad := f.do(http.MethodGet, assetver.ContentFlag()+"/tokens/design-tokens.css", reqOpt{headers: map[string]string{"Range": "bytes=99999999-"}})
	expect(t, bad.status == 416 && bad.header.Get("Cache-Control") == "no-store" && bad.header.Get("ETag") != etag, "versioned 416 is no-store: %d %q", bad.status, bad.header.Get("Cache-Control"))
	okRange := f.do(http.MethodGet, assetver.ContentFlag()+"/tokens/design-tokens.css", reqOpt{headers: map[string]string{"Range": "bytes=0-9"}})
	expect(t, okRange.status == 206 && okRange.header.Get("Cache-Control") == "public, max-age=31536000, immutable", "versioned 206 stays immutable: %d %q", okRange.status, okRange.header.Get("Cache-Control"))

	fav := f.get("/favicon.ico")
	expect(t, fav.status == 301 && fav.header.Get("Location") == "/assets/flag/logos/elcano-mark-favicon.svg", "favicon redirect")

	// A failing handler → branded, hardened, non-leaking 500.
	f.store.set(func() { f.store.publicPage = errors.New("forced failure: pg connection lost") })
	b := f.get("/pubpage")
	f.store.set(func() { f.store.publicPage = nil })
	expect(t, b.status == 500 && strings.HasPrefix(b.header.Get("Content-Type"), "text/html"), "500 is branded HTML")
	expect(t, strings.Contains(b.csp, "sandbox") && b.header.Get("Cache-Control") == "no-store" && b.header.Get("X-Robots-Tag") != "", "500 hardened")
	expect(t, regexp.MustCompile(`This page couldn(&#39;|')t be loaded`).MatchString(b.body), "says whose fault it is")
	expect(t, !strings.Contains(b.body, "forced failure") && !strings.Contains(strings.ToLower(b.body), "<script"), "leaks nothing")

	// The form body is capped.
	big := f.do(http.MethodPost, "/pubpage", reqOpt{body: "password=" + strings.Repeat("a", 2000), ctype: "application/x-www-form-urlencoded"})
	expect(t, big.status == 413 && strings.Contains(big.csp, "sandbox"), "a 2 KB form is a branded 413: %d", big.status)
}

func TestSignInKeepsStaffReadableCopy(t *testing.T) {
	f := newFixture(t)
	f.live("rv-fabrikam")
	f.setPassword("rv-fabrikam", "fabrikam-client-pass")
	copyOf := func() (string, string) {
		var ct *string
		var hash string
		if err := f.d.QueryRow(context.Background(), `SELECT password_ciphertext, password_hash FROM pages WHERE slug = 'rv-fabrikam'`).Scan(&ct, &hash); err != nil {
			t.Fatal(err)
		}
		if ct == nil {
			return "", hash
		}
		return *ct, hash
	}
	expect(t, f.post("/rv-fabrikam", "not-the-password").status == 401, "wrong")
	if ct, _ := copyOf(); ct != "" {
		t.Fatal("a wrong guess never becomes the stored copy")
	}
	expect(t, f.post("/rv-fabrikam", "fabrikam-client-pass").status == 303, "the client signs in")
	ct, hash := copyOf()
	pw, ok := passwordvault.Open(ct, hash)
	expect(t, ok && pw == "fabrikam-client-pass", "the copy is kept from the sign-in that verified")

	pid := f.portal("rv-northwind", "Northwind Reveal", "northwind-portal-pass")
	expect(t, f.post("/portal/rv-northwind", "northwind-portal-pass").status == 303, "the partner signs in")
	var pct string
	if err := f.d.QueryRow(context.Background(), `SELECT password_ciphertext FROM page_portals WHERE id = $1`, pid).Scan(&pct); err != nil {
		t.Fatal(err)
	}
	ppw, ok := passwordvault.Open(pct, f.portalHash(pid))
	expect(t, ok && ppw == "northwind-portal-pass", "a portal password is kept from the partner's sign-in")
}

func TestIsContentHost(t *testing.T) {
	t.Setenv("CONTENT_HOST", "Content.Example")
	t.Setenv("CONTENT_HOST_ALSO", "content.localhost")
	for host, want := range map[string]bool{
		"content.example": true, "CONTENT.EXAMPLE:443": true, "content.localhost:3002": true,
		"pages.example": false, "": false, "content.example.evil": false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		if got := IsContentHost(r); got != want {
			t.Errorf("IsContentHost(%q) = %v", host, got)
		}
	}
}

func TestShellAssets(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
	h := ShellAssets(next)
	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}
	css := get("/shell-assets/flag/tokens/design-tokens.css")
	expect(t, css.Code == 200 && css.Header().Get("Cache-Control") == assetver.Revalidate, "the Flag tree is mounted under /shell-assets/flag")

	// Versioned URLs, as the shell links them: immutable when current.
	for _, p := range []string{assetver.ShellFlag() + "/tokens/design-tokens.css", assetver.Shell() + "/shell.css", assetver.Shell() + "/primitives.js", assetver.ShellFlag() + "/fonts/nebula-sans/NebulaSans-400.woff2"} {
		r := get(p)
		expect(t, r.Code == 200 && r.Header().Get("Cache-Control") == "public, max-age=31536000, immutable", "%s: %d %s", p, r.Code, r.Header().Get("Cache-Control"))
	}
	vcss := get(assetver.ShellFlag() + "/tokens/design-tokens.css")
	expect(t, vcss.Body.String() == css.Body.String() && vcss.Header().Get("ETag") == css.Header().Get("ETag"), "versioned is the same file")
	stale := get("/shell-assets/_v/000000000000/shell.css")
	expect(t, stale.Code == 200 && stale.Header().Get("Cache-Control") == assetver.Revalidate, "a stale version revalidates: %d %s", stale.Code, stale.Header().Get("Cache-Control"))
	rangeGet := func(p, rg string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Range", rg)
		h.ServeHTTP(rec, req)
		return rec
	}
	bad := rangeGet(assetver.Shell()+"/shell.css", "bytes=99999999-")
	expect(t, bad.Code == 416 && bad.Header().Get("Cache-Control") == "no-store" && bad.Header().Get("ETag") != get(assetver.Shell()+"/shell.css").Header().Get("ETag"), "versioned 416 is no-store: %d %q", bad.Code, bad.Header().Get("Cache-Control"))
	part := rangeGet(assetver.Shell()+"/shell.css", "bytes=0-9")
	expect(t, part.Code == 206 && part.Header().Get("Cache-Control") == "public, max-age=31536000, immutable", "versioned 206 stays immutable: %d", part.Code)
	notMod := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, assetver.Shell()+"/shell.css", nil)
	req.Header.Set("If-None-Match", get(assetver.Shell()+"/shell.css").Header().Get("ETag"))
	h.ServeHTTP(notMod, req)
	expect(t, notMod.Code == 304 && notMod.Header().Get("Cache-Control") == "public, max-age=31536000, immutable", "versioned 304 stays immutable: %d", notMod.Code)
	expect(t, get(assetver.Shell()+"/nope.js").Code == 418, "a versioned miss goes to next")
	expect(t, get(assetver.Shell()+"/../shell.css").Code == 418, "versioned traversal goes to next")
	expect(t, get("/shell-assets/nope.js").Code == 418, "a miss goes to next")
	expect(t, get("/elsewhere").Code == 418, "other paths go to next")
}
