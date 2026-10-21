// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
)

func TestBuildNavBoundedAndScoped(t *testing.T) {
	rows := make([]PortalPage, 60)
	for i := range rows {
		rows[i] = PortalPage{Slug: fmt.Sprintf("dash-%d", i), Title: fmt.Sprintf("Dashboard %d", i)}
	}
	nav := BuildNav(Portal{Slug: "nwm", Name: "Northwind Media Group"}, rows, "dash-3")
	if nav.Portal.Slug != "nwm" {
		t.Fatalf("the payload names the portal that authorised the request")
	}
	if len(nav.Pages) != NavMaxEntries || !nav.Truncated {
		t.Fatalf("entry count is capped and says so: %d %v", len(nav.Pages), nav.Truncated)
	}
	current := 0
	for _, p := range nav.Pages {
		if p.Current {
			current++
			if p.Slug != "dash-3" {
				t.Fatalf("wrong current page %q", p.Slug)
			}
		}
	}
	if current != 1 {
		t.Fatalf("exactly one entry is the page being viewed, got %d", current)
	}
	if !regexp.MustCompile(`^https?://[^/]+/dash-0$`).MatchString(nav.Pages[0].URL) {
		t.Fatalf("a ready-made absolute url: %q", nav.Pages[0].URL)
	}

	long := BuildNav(Portal{Slug: "s", Name: "n"}, []PortalPage{{Slug: "x", Title: strings.Repeat("t", 5000)}}, "x")
	if len(long.Pages[0].Title) != 200 {
		t.Fatalf("titles are clipped: %d", len(long.Pages[0].Title))
	}

	fat := make([]PortalPage, 50)
	for i := range fat {
		fat[i] = PortalPage{Slug: strings.Repeat("s", 198) + fmt.Sprint(i), Title: strings.Repeat("t", 200)}
	}
	fn := BuildNav(Portal{Slug: "s", Name: "n"}, fat, "s0")
	if l := jsonLen(fn); l > NavMaxBytes {
		t.Fatalf("payload must fit %d bytes, got %d", NavMaxBytes, l)
	}
	if !fn.Truncated || len(fn.Pages) < 1 {
		t.Fatalf("dropping entries to fit is reported, but never down to nothing")
	}

	empty := BuildNav(Portal{Slug: "s", Name: "n"}, nil, "x")
	if b := mustJSON(t, empty); b != `{"portal":{"slug":"s","name":"n","url":"https://elcano-pages.com/portal/s"},"pages":[],"truncated":false}` {
		t.Fatalf("wire shape: %s", b)
	}
}

func TestBuildNavWireShape(t *testing.T) {
	t.Setenv("CONTENT_ORIGIN", "https://content.example")
	nav := BuildNav(Portal{Slug: "nwm", Name: "Northwind"}, []PortalPage{
		{Slug: "a", Title: "", IsHome: true}, {Slug: "b", Title: "<B>"},
	}, "b")
	want := `{"portal":{"slug":"nwm","name":"Northwind","url":"https://content.example/portal/nwm"},` +
		`"pages":[{"slug":"a","title":"a","url":"https://content.example/a","current":false,"home":true},` +
		`{"slug":"b","title":"<B>","url":"https://content.example/b","current":true,"home":false}],"truncated":false}`
	if got := mustJSON(t, nav); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := httpx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuthorizingPortalLowestIDWins(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "unit-test-secret")
	hashA := "scrypt$16384$8$1$YQ==$YQ=="
	hashB := "scrypt$16384$8$1$Yg==$Yg=="
	portals := []Portal{{ID: 4, Slug: "fabrikam", PasswordHash: hashA}, {ID: 9, Slug: "nwm", PasswordHash: hashB}}
	cookieFor := func(id int64, hash string) map[string]string {
		return map[string]string{pagecookie.PortalCookieName(id): pagecookie.MintPortalSession(id, 600, hash)}
	}
	id := func(p *Portal) int64 {
		if p == nil {
			return 0
		}
		return p.ID
	}
	if AuthorizingPortal(map[string]string{}, portals) != nil {
		t.Fatal("no cookie authorises nothing")
	}
	if id(AuthorizingPortal(cookieFor(9, hashB), portals)) != 9 || id(AuthorizingPortal(cookieFor(4, hashA), portals)) != 4 {
		t.Fatal("the one held cookie authorises")
	}
	both := cookieFor(4, hashA)
	for k, v := range cookieFor(9, hashB) {
		both[k] = v
	}
	if id(AuthorizingPortal(both, portals)) != 4 {
		t.Fatal("two valid cookies → the lowest portal id, always")
	}
	reversed := []Portal{portals[1], portals[0]}
	if id(AuthorizingPortal(both, reversed)) != 9 {
		t.Fatal("the helper trusts its input order — which is why GetPortalsForPage orders by id")
	}
	if AuthorizingPortal(cookieFor(4, hashB), portals) != nil {
		t.Fatal("wrong credential → no authorisation")
	}
	if AuthorizingPortal(map[string]string{"pgp4": "garbage"}, portals) != nil {
		t.Fatal("garbage cookie")
	}
	if AuthorizingPortal(cookieFor(4, hashA), nil) != nil {
		t.Fatal("a page in no portal cannot be portal-authorised")
	}
}

func partnerFacing() [][2]string {
	return [][2]string{
		{"gate", GatePage(GatePageOptions{Slug: "acme/q2", ShowForm: true})},
		{"gate error", GatePage(GatePageOptions{Slug: "acme/q2", ShowForm: true, Message: "Incorrect password."})},
		{"staff notice", GatePage(GatePageOptions{Slug: "acme/q2"})},
		{"not found", NotFoundPage()},
		{"rate limit", RateLimitPage("acme/q2", 0)},
		{"rate limit bare", RateLimitPage("", 0)},
		{"busy", BusyPage("acme/q2")},
		{"busy bare", BusyPage("")},
		{"server error", ServerErrorPage()},
		{"expired link", ExpiredLinkPage()},
		{"portal index", PortalIndexPage(PortalIndexOptions{
			Portal: Portal{Slug: "nwm", Name: "Northwind Media Group"},
			Pages:  []PortalPage{{Slug: "a", Title: "Overview", IsHome: true}, {Slug: "b", Title: "Campaign"}},
		})},
		{"portal gate", PortalIndexPage(PortalIndexOptions{Portal: Portal{Slug: "nwm", Name: "Northwind Media Group"}, ShowForm: true})},
	}
}

func TestNothingClientFacingCallsThisInternal(t *testing.T) {
	for _, c := range partnerFacing() {
		if c[1] == "" {
			t.Fatalf("%s rendered nothing", c[0])
		}
		if strings.Contains(c[1], "Elcano Internal") {
			t.Fatalf("%s still calls Pages an internal tool", c[0])
		}
		if !strings.Contains(c[1], "Elcano") {
			t.Fatalf("%s lost its branding altogether", c[0])
		}
	}
}

func TestOneChromeOneFavicon(t *testing.T) {
	gate := GatePage(GatePageOptions{Slug: "acme/q2", ShowForm: true})
	base := "/assets/flag"
	for _, want := range []string{
		`<link rel="icon" href="` + base + `/logos/elcano-mark-favicon.svg"`,
		base + "/tokens/design-tokens.css",
		"min-height:100dvh",
	} {
		if !strings.Contains(gate, want) {
			t.Fatalf("gate lacks %q", want)
		}
	}
	if strings.Contains(gate, "min-height:100vh") {
		t.Fatal("gate still sizes on 100vh")
	}
	if !regexp.MustCompile(`h1\{[^}]*overflow-wrap:anywhere`).MatchString(gate) {
		t.Fatal("gate lets a long heading widen the card")
	}
	if strings.Contains(gate, `href="/admin"`) {
		t.Fatal("a CTA to /admin means nothing on the content host")
	}
}

func TestWhenPhrase(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return WhenPhrase(now.Add(-d), now) }
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{5 * time.Minute, "5 minutes ago"},
		{6 * time.Hour, "6 hours ago"},
		{26 * time.Hour, "yesterday"},
		{3 * 24 * time.Hour, "3 days ago"},
		{9 * 24 * time.Hour, "18 Aug 2026"},
	}
	for _, c := range cases {
		if got := ago(c.d); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if strings.Contains(ago(9*24*time.Hour), ":") {
		t.Fatal("seconds are never shown")
	}
	if got := WhenPhrase(now.Add(24*time.Hour), now); got != "28 Aug 2026" {
		t.Fatalf("a future stamp is clock skew, not a countdown: %q", got)
	}
	if WhenPhrase(time.Time{}, now) != "" {
		t.Fatal("no time, no phrase")
	}
	if got := WhenPhrase(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), now.AddDate(0, 1, 0)); got != "1 Sept 2026" {
		t.Fatalf("en-GB short month: %q", got)
	}
}

func TestPortalIndexFreshnessClaims(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	page := func(e PortalPage) string {
		e.Slug, e.Title = "d", "Dash"
		return PortalIndexPage(PortalIndexOptions{Portal: Portal{Slug: "nwm", Name: "Northwind Media Group"}, Now: now, Pages: []PortalPage{e}})
	}
	enveloped := page(PortalPage{SourceAsOf: now.Add(-48 * time.Hour), PublishedAt: now.Add(-time.Minute)})
	if !strings.Contains(enveloped, "Data as of 2 days ago") || strings.Contains(enveloped, "just now") {
		t.Fatal("a data envelope says when the DATA is from, never the publish time")
	}
	plain := page(PortalPage{PublishedAt: now.Add(-72 * time.Hour)})
	if !strings.Contains(plain, "Updated 3 days ago") || strings.Contains(plain, "Data as of") {
		t.Fatal("no envelope: the weaker claim, in its own words")
	}
	if !strings.Contains(plain, `<time datetime="2026-08-24T12:00:00.000Z" title="2026-08-24T12:00:00.000Z">`) {
		t.Fatalf("machine instant beside the phrase: %s", plain)
	}
	silent := page(PortalPage{})
	if strings.Contains(silent, "<time") || strings.Contains(silent, "Updated") || strings.Contains(silent, "Data as of") {
		t.Fatal("nothing known: say nothing")
	}
	if !strings.Contains(silent, "Dash") {
		t.Fatal("…and the dashboard is still listed")
	}
}

func TestPortalIndexSessionFooter(t *testing.T) {
	html := PortalIndexPage(PortalIndexOptions{Portal: Portal{Slug: "nwm", Name: "Northwind Media Group"}, Pages: []PortalPage{{Slug: "d", Title: "Dash"}}})
	if !strings.Contains(html, fmt.Sprintf("for %d days on this device", pagecookie.DefaultTTLDays)) {
		t.Fatal("the footer reads the cookie's real lifetime")
	}
	if strings.Contains(html, "as long as this portal session lasts") {
		t.Fatal("old copy")
	}
	if !strings.Contains(html, `action="/portal/nwm/lock"`) {
		t.Fatal("there is a way to end it")
	}
}

func TestPortalIndexSections(t *testing.T) {
	portal := Portal{Slug: "nwm", Name: "Northwind Media Group"}
	index := func(p ...PortalPage) string { return PortalIndexPage(PortalIndexOptions{Portal: portal, Pages: p}) }
	both := index(PortalPage{Slug: "a", Title: "Overview", IsHome: true}, PortalPage{Slug: "b", Title: "Campaign"})
	if !strings.Contains(both, "Start here") || !strings.Contains(both, "All dashboards") ||
		strings.Index(both, "Start here") > strings.Index(both, "All dashboards") {
		t.Fatal("two groups, home first")
	}
	for _, only := range []string{
		index(PortalPage{Slug: "a", Title: "Overview", IsHome: true}),
		index(PortalPage{Slug: "b", Title: "Campaign"}),
		index(PortalPage{Slug: "b", Title: "One"}, PortalPage{Slug: "c", Title: "Two"}),
	} {
		if strings.Contains(only, "Start here") || strings.Contains(only, "All dashboards") || strings.Contains(only, "<h2") {
			t.Fatal("headings over a list of one are hierarchy for its own sake")
		}
	}
	if !strings.Contains(index(PortalPage{Slug: "b", Title: "One"}), "1 dashboard available to you.") {
		t.Fatal("singular count")
	}
	empty := index()
	if !strings.Contains(empty, "No dashboards are available in this portal yet") || !strings.Contains(empty, "Nothing is wrong with your password") {
		t.Fatal("an empty portal is not a credential failure")
	}
}

var styleRE = regexp.MustCompile(`(?s)<style>.*?</style>`)

func TestPortalIndexEscapesEverything(t *testing.T) {
	html := PortalIndexPage(PortalIndexOptions{
		Portal: Portal{Slug: `x"><script>a()</script>`, Name: `<img src=x onerror=a()>`},
		Pages:  []PortalPage{{Slug: `y"><b>`, Title: `<script>b()</script>`, PublishedAt: time.Now()}},
	})
	stripped := styleRE.ReplaceAllString(html, "")
	if regexp.MustCompile(`<script>|<img src=x|<b>`).MatchString(stripped) {
		t.Fatal("a partner-supplied string became markup")
	}
	if !strings.Contains(html, "&lt;script&gt;b()&lt;/script&gt;") {
		t.Fatal("the title is shown, escaped, not dropped")
	}
	gate := GatePage(GatePageOptions{Slug: `a"b`, ShowForm: true, Message: "<x>"})
	if strings.Contains(gate, `a"b`) || strings.Contains(gate, "<x>") {
		t.Fatal("gate escapes slug and message")
	}
}

func TestNoWordOnlyWeWouldUse(t *testing.T) {
	ourWords := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bunpublished\b`),
		regexp.MustCompile(`(?i)\bthis host\b`),
		regexp.MustCompile(`(?i)\bshort-lived\b`),
		regexp.MustCompile(`(?i)\brendered\b`),
		regexp.MustCompile(`(?i)\bElcano-only\b`),
		regexp.MustCompile(`(?i)\bsandbox`),
		regexp.MustCompile(`(?i)\bpayload\b`),
		regexp.MustCompile(`(?i)\blifecycle\b`),
		regexp.MustCompile(`(?i)\bversion \d`),
		regexp.MustCompile(`(?i)\bslug\b`),
		regexp.MustCompile(`(?i)\btemplate revision\b`),
		regexp.MustCompile(`\b404\b`),
	}
	tags := regexp.MustCompile(`<[^>]+>`)
	space := regexp.MustCompile(`\s+`)
	for _, c := range partnerFacing() {
		prose := space.ReplaceAllString(tags.ReplaceAllString(styleRE.ReplaceAllString(c[1], ""), " "), " ")
		for _, re := range ourWords {
			if re.MatchString(prose) {
				t.Errorf("%s says %q — that is our word, not theirs", c[0], re)
			}
		}
	}
	if !strings.Contains(GatePage(GatePageOptions{Slug: "acme/q2"}), "Elcano staff") {
		t.Fatal("the staff line addresses Elcano staff by name")
	}
}

func TestGateCopyPerCredential(t *testing.T) {
	t.Setenv("DASHBOARD_ORIGIN", "https://dash.example")
	cases := map[string][2]string{
		"":       {"Enter the client password to continue to this page.", "Page password"},
		"page":   {"Enter the client password to continue to this page.", "Page password"},
		"portal": {"Enter your portal password to continue to this dashboard.", "Portal password"},
		"either": {"Enter the password for this page, or your portal password, to continue.", ">Password<"},
	}
	for cred, want := range cases {
		html := GatePage(GatePageOptions{Slug: "acme/q2", ShowForm: true, Credential: cred})
		if !strings.Contains(html, want[0]) || !strings.Contains(html, want[1]) {
			t.Errorf("credential %q: wrong copy", cred)
		}
		if !strings.Contains(html, `action="/acme/q2"`) || !strings.Contains(html, `aria-describedby="password-help"`) {
			t.Errorf("credential %q: form wiring", cred)
		}
	}
	withMsg := GatePage(GatePageOptions{Slug: "s", ShowForm: true, Message: "Incorrect password."})
	if !strings.Contains(withMsg, `aria-describedby="password-help password-error"`) || !strings.Contains(withMsg, `role="alert"`) {
		t.Fatal("the error is announced and described")
	}
	staff := GatePage(GatePageOptions{Slug: "acme/q2"})
	if !strings.Contains(staff, `href="https://dash.example/view/acme/q2"`) || strings.Contains(staff, "<form") {
		t.Fatal("the staff notice links the dashboard broker and has no form")
	}
}

func TestRateLimitPageWait(t *testing.T) {
	if !strings.Contains(RateLimitPage("", 0), "Wait about 15 minutes") {
		t.Fatal("default window")
	}
	t.Setenv("RL_PASSWORD_WINDOW_MIN", "7")
	if !strings.Contains(RateLimitPage("", 0), "Wait about 7 minutes") {
		t.Fatal("reads the limiter's knob")
	}
	if p := RateLimitPage("acme/q2", 3); !strings.Contains(p, "Wait about 3 minutes") || !strings.Contains(p, `<a href="/acme/q2">Return to the page</a>`) {
		t.Fatal("explicit minutes and a way back")
	}
	if strings.Contains(RateLimitPage("", 3), "Return to the page") {
		t.Fatal("no slug, no link")
	}
}
