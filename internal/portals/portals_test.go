// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package portals

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/versions"
)

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	e, ok := apierror.As(err)
	if !ok || e.Status != status || e.Code != code {
		t.Fatalf("want %d %s, got %v (%#v)", status, code, err, err)
	}
}

// One call per mutation. Deliberately VALID input everywhere, and a nil
// database, so a passing case can only mean the authority guard ran first —
// before validation, before scrypt, before any database work.
var mutationCalls = map[string]func(context.Context, *db.DB, versions.Actor) error{
	"create": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := Create(ctx, d, CreateInput{Slug: "nwm", Name: "Northwind Media Group", Password: "a-long-enough-portal-secret"}, a)
		return err
	},
	"rename": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := Rename(ctx, d, RenameInput{ID: 1, Name: "NWM"}, a)
		return err
	},
	"setPassword": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := SetPassword(ctx, d, SetPasswordInput{ID: 1, Password: "a-long-enough-portal-secret"}, a)
		return err
	},
	"addPage": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := AddPage(ctx, d, AddPageInput{ID: 1, Slug: "nwm-contoso-fabrikam", Label: "Contoso — Fabrikam"}, a)
		return err
	},
	"updatePage": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := UpdatePage(ctx, d, UpdatePageInput{ID: 1, Slug: "nwm-contoso-fabrikam", Label: "Contoso"}, a)
		return err
	},
	"removePage": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := RemovePage(ctx, d, RemovePageInput{ID: 1, Slug: "nwm-contoso-fabrikam"}, a)
		return err
	},
	"setHome": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := SetHome(ctx, d, SetHomeInput{ID: 1, Slug: "nwm-client-overview"}, a)
		return err
	},
	"remove": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := Remove(ctx, d, 1, a)
		return err
	},
	"revealPassword": func(ctx context.Context, d *db.DB, a versions.Actor) error {
		_, err := RevealPassword(ctx, d, 1, a)
		return err
	},
}

// The Go names of the mutations, and every other exported function, which
// must be a declared read or helper.
var (
	mutationFuncs = map[string]string{
		"Create": "create", "Rename": "rename", "SetPassword": "setPassword", "RevealPassword": "revealPassword",
		"AddPage": "addPage", "UpdatePage": "updatePage", "RemovePage": "removePage", "SetHome": "setHome", "Remove": "remove",
	}
	nonMutations = []string{
		"AssertPortalAdmin", "NormalizeID", "NormalizeSlug", "NormalizeName", "NormalizeLabel",
		"NormalizeSortOrder", "GeneratePassword", "AssertPasswordAcceptable", "List", "Get", "ListForPage", "ExtractLinkedSlugs",
	}
)

func exportedFuncs(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && ast.IsExported(fn.Name.Name) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	return out
}

// Design decision 2: an agent may never change which dashboards a partner
// credential opens. Table-driven over Mutations so the guarantee cannot rot —
// a new verb has to be added to Mutations (or the export audit fails) AND to
// the table (or the coverage check fails).
func TestEveryMutationRefusesANonHumanActorFirst(t *testing.T) {
	keys := make([]string, 0, len(mutationCalls))
	for k := range mutationCalls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	listed := slices.Clone(Mutations)
	sort.Strings(listed)
	if !slices.Equal(keys, listed) {
		t.Fatalf("every portal mutation needs a case here, and every case a mutation: %v vs %v", keys, listed)
	}
	var unclassified []string
	for _, name := range exportedFuncs(t) {
		if js, ok := mutationFuncs[name]; ok {
			if !slices.Contains(Mutations, js) {
				t.Errorf("%s is not listed in Mutations", name)
			}
			continue
		}
		if !slices.Contains(nonMutations, name) {
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		t.Fatalf("a new portals export must be classified as a mutation or a read: %v", unclassified)
	}
	if len(mutationFuncs) != len(Mutations) {
		t.Fatal("mutationFuncs and Mutations disagree")
	}

	ctx := context.Background()
	for _, actor := range []versions.Actor{{}, {ActorType: "agent", Actor: "agent"}, {ActorType: "system"}, {ActorType: "User"}} {
		for _, verb := range Mutations {
			err := mutationCalls[verb](ctx, nil, actor)
			e, ok := apierror.As(err)
			if !ok || e.Status != 403 || e.Code != "portal_admin_only" {
				t.Fatalf("%s must refuse actor %+v, got %v", verb, actor, err)
			}
		}
	}
	// The error code is distinct from workspaces' admin_only on purpose: the
	// two guards are opposites and a grep for one must never turn up the other.
	if err := AssertPortalAdmin(versions.Actor{ActorType: "user"}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, AssertPortalAdmin(versions.Actor{ActorType: "agent"}), 403, "portal_admin_only")
}

func TestNormalizers(t *testing.T) {
	for in, want := range map[string]string{"  NWM  ": "nwm", "fabrikam_ssp-2": "fabrikam_ssp-2"} {
		if got, err := NormalizeSlug(in); err != nil || got != want {
			t.Errorf("NormalizeSlug(%q) = %q, %v", in, got, err)
		}
	}
	// ONE segment: the partner URL is <content-host>/portal/<slug>.
	for _, bad := range []string{"nwm/contoso", "-nwm", "nwm-", "nwm..a", strings.Repeat("a", 65), "nwm page", "İnwm"} {
		_, err := NormalizeSlug(bad)
		wantCode(t, err, 400, "bad_portal_slug")
	}
	for _, missing := range []any{"", "   ", nil, versions.Null, 7} {
		_, err := NormalizeSlug(missing)
		wantCode(t, err, 400, "portal_slug_required")
	}

	if got, _ := NormalizeName("  Northwind   Media Group "); got != "Northwind Media Group" {
		t.Errorf("NormalizeName collapses: %q", got)
	}
	_, err := NormalizeName(strings.Repeat("x", 101))
	wantCode(t, err, 400, "portal_name_too_long")
	_, err = NormalizeName("bad\nname")
	wantCode(t, err, 400, "bad_portal_name")
	_, err = NormalizeName("  ")
	wantCode(t, err, 400, "portal_name_required")

	// A null/blank label means "fall back to pages.title", which is the only
	// reason the column is nullable.
	for _, blank := range []any{nil, versions.Null, "", "   "} {
		if l, err := NormalizeLabel(blank); l != nil || err != nil {
			t.Errorf("NormalizeLabel(%v) = %v, %v", blank, l, err)
		}
	}
	if l, _ := NormalizeLabel(" Contoso —  Fabrikam "); l == nil || *l != "Contoso — Fabrikam" {
		t.Errorf("label: %v", l)
	}
	for _, bad := range []any{strings.Repeat("x", 201), "a\u0000b", 12.0} {
		_, err := NormalizeLabel(bad)
		wantCode(t, err, 400, "bad_portal_label")
	}

	if n, err := NormalizeSortOrder(nil); n != 0 || err != nil {
		t.Error("omitted sort order is 0")
	}
	if n, err := NormalizeSortOrder("7"); n != 7 || err != nil {
		t.Error("decimal string sort order")
	}
	if n, err := NormalizeSortOrder(9999.0); n != 9999 || err != nil {
		t.Error("the ceiling itself is allowed")
	}
	for _, bad := range []any{-1.0, 1.5, 10000.0, "seven", map[string]any{}, true, "99999999999999999999"} {
		_, err := NormalizeSortOrder(bad)
		wantCode(t, err, 400, "bad_portal_sort_order")
	}

	if id, err := NormalizeID("42", ""); id != 42 || err != nil {
		t.Error("decimal id")
	}
	if id, err := NormalizeID(42.0, ""); id != 42 || err != nil {
		t.Error("number id")
	}
	if id, err := NormalizeID("9007199254740992", ""); id != 9007199254740992 || err != nil {
		t.Error("a BIGINT past 2^53 is accepted as a string")
	}
	for _, bad := range []any{0.0, -1.0, 1.5, "nope", nil, versions.Null, "9223372036854775808", 9007199254740992.0, "042"} {
		_, err := NormalizeID(bad, "")
		wantCode(t, err, 400, "bad_portal_id")
	}
	_, err = NormalizeID("x", "page_id")
	if e, _ := apierror.As(err); e == nil || e.Code != "bad_page_id" || e.Message != "page_id must be a positive database id" {
		t.Fatalf("field-specific id error: %v", err)
	}

	_, err = normalizeMemberTarget("a", "1")
	wantCode(t, err, 400, "bad_portal_member_target")
	_, err = normalizeMemberTarget(nil, "")
	wantCode(t, err, 400, "portal_member_target_required")
	if tg, err := normalizeMemberTarget(nil, "12"); err != nil || tg.pageID != 12 {
		t.Fatal(tg, err)
	}
}

var generatedRE = regexp.MustCompile(`^[a-hjkmnp-z2-9]{4}(?:-[a-hjkmnp-z2-9]{4}){3}$`)

func TestPasswords(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		g, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if !generatedRE.MatchString(g) || strings.ContainsAny(g, "ilo01") {
			t.Fatalf("generated %q", g)
		}
		if got, err := AssertPasswordAcceptable(g); err != nil || got != g {
			t.Fatalf("a generated credential clears its own floor: %v", err)
		}
		seen[g] = true
	}
	if len(seen) != 200 {
		t.Fatal("generated portal credentials do not repeat")
	}

	// Pinned by VALUE, not against MinPasswordLength: a test that derives its
	// inputs from the constant it checks stays green when the constant is
	// lowered to 1, which is precisely the regression worth catching.
	if MinPasswordLength < 16 {
		t.Fatal("a floor below 16 characters is not a floor for a credential this valuable")
	}
	floor := strings.Repeat("x", 16)
	if got, err := AssertPasswordAcceptable(floor); err != nil || got != floor {
		t.Fatal(err)
	}
	_, err := AssertPasswordAcceptable(strings.Repeat("x", 15))
	wantCode(t, err, 400, "portal_password_too_weak")
	// Never silently repaired.
	for _, bad := range []any{floor + "\n", " " + floor, floor + "\u0007", floor + "\u00a0", nil, versions.Null, 16.0} {
		_, err := AssertPasswordAcceptable(bad)
		wantCode(t, err, 400, "bad_portal_password")
	}
	_, err = AssertPasswordAcceptable(strings.Repeat("x", 513))
	wantCode(t, err, 400, "portal_password_too_long")
	// Length is JavaScript's: UTF-16 units, so 8 astral characters are 16.
	if _, err := AssertPasswordAcceptable(strings.Repeat("😀", 8)); err != nil {
		t.Fatal(err)
	}
}

// The Fabrikam drift shape: a hub page links a dashboard nobody registered as
// a member. The extractor must take both spellings a hub uses (absolute on the
// content origin, root-relative), and nothing else.
func TestExtractLinkedSlugs(t *testing.T) {
	origin := "https://content.example.test"
	html := `<!doctype html><body>
    <a href="` + origin + `/woodgrove-campaign-overview">Woodgrove</a>
    <a href="/harborsun-example">Harbor Sun</a>
    <a href='/nested/campaign_page'>nested</a>
    <a href="/woodgrove-campaign-overview?ref=hub#kpis">same page again</a>
    <a href="https://elsewhere.example.com/not-ours">external</a>
    <a href="//protocol-relative.example.com/nope">protocol-relative</a>
    <a href="mailto:west@example.com">mail</a>
    <a href="relative-sibling">bare relative is ambiguous under nested slugs</a>
    <a href="/portal/fabrikam">a portal route, not a page</a>
    <a href="/raw/some-page?t=tok">raw route</a>
    <a href="/shell-assets/portals.js">asset</a>
    <a href="/Has-Caps">not slug-shaped</a>
    <a HREF = "/trailing/">trailing slash</a>
  </body>`
	want := []string{"woodgrove-campaign-overview", "harborsun-example", "nested/campaign_page", "trailing"}
	if got := ExtractLinkedSlugs(html, origin); !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if got := ExtractLinkedSlugs("", origin); got == nil || len(got) != 0 {
		t.Fatal("empty input → empty (non-nil) list")
	}
	// The default origin is the configured content origin.
	t.Setenv("CONTENT_ORIGIN", origin)
	if got := ExtractLinkedSlugs(`<a href="`+origin+`/northwind">x</a>`, ""); !slices.Equal(got, []string{"northwind"}) {
		t.Fatal(got)
	}
	// Capped at 200 distinct candidates.
	var b strings.Builder
	for i := 0; i < 250; i++ {
		b.WriteString(`<a href="/p` + strconv.Itoa(i) + `">`)
	}
	if got := ExtractLinkedSlugs(b.String(), origin); len(got) != 200 {
		t.Fatal(len(got))
	}
}

// The browser module keeps its own copy of the sort_order ceiling, so the
// admin UI can append a member without proposing a value the write would
// reject. The only machine-checked guard on the two agreeing used to be a
// native max="9999" on an input that no longer exists; this is what is left.
func TestBrowserMaxSortOrderIsTheAPIs(t *testing.T) {
	src, err := fs.ReadFile(pages.Public(), "shell-assets/portals.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^const MAX_SORT_ORDER = ([0-9]+);`).FindSubmatch(src)
	if m == nil {
		t.Fatal("public/shell-assets/portals.js no longer declares const MAX_SORT_ORDER")
	}
	if n, _ := strconv.Atoi(string(m[1])); n != MaxSortOrder {
		t.Fatalf("browser MAX_SORT_ORDER %d != portals.MaxSortOrder %d", n, MaxSortOrder)
	}
}

func TestCollapseMatchesJSWhitespace(t *testing.T) {
	// U+0085 is not JavaScript whitespace; U+FEFF is.
	if got := collapse("\ufeffa\u0085b\u3000 c"); got != "a\u0085b c" {
		t.Fatalf("%q", got)
	}
}
