// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package dataexport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
)

// Port of test/data-export.integration.js. The JS drove get_page_data through
// /mcp to mint; the MCP tool layer is not ported yet, so these tests mint
// directly (the bytes and checks behind the URL are the same) and serve the
// handler mounted the way server.js mounts it. The content-host refusal is the
// server's host split and is tested there.

const slug = "northwind-export"

func setSecret(t *testing.T) {
	t.Helper()
	t.Setenv("RAW_TOKEN_SECRET", "test-secret-do-not-use-in-prod")
	t.Setenv("DASHBOARD_ORIGIN", "http://localhost")
}

func block(id, typ string, value any) string {
	return `<script id="` + id + `" type="` + typ + `">` + pagedata.EscapedJSON(value) + `</script>`
}

var schema = jsonv.ObjectOf(
	"$schema", "https://json-schema.org/draft/2020-12/schema",
	"type", "object", "additionalProperties", false, "required", []any{"dataThrough", "rows"},
	"properties", jsonv.ObjectOf(
		"dataThrough", jsonv.ObjectOf("type", "string"),
		"rows", jsonv.ObjectOf("type", "array", "items", jsonv.ObjectOf(
			"type", "object", "required", []any{"date", "clicks"},
			"properties", jsonv.ObjectOf("date", jsonv.ObjectOf("type", "string"), "clicks", jsonv.ObjectOf("type", "integer")),
		)),
	),
)

// dataFor's rows carry markup on purpose: an export is JSON served as an
// attachment, never HTML.
func dataFor(days int) *jsonv.Object {
	rows := make([]any, days)
	for i := range rows {
		rows[i] = jsonv.ObjectOf("date", "2026-07-0"+string(rune('1'+i)), "clicks", float64(10*(i+1)), "note", "Contoso </script><b>")
	}
	return jsonv.ObjectOf("dataThrough", "2026-07-0"+string(rune('0'+days)), "rows", rows)
}

func pageHTML(days int, asOf string) string {
	return "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
		block("pages-data-schema", "application/schema+json", schema) +
		block("pages-data", "application/json", jsonv.ObjectOf("contract_version", 1.0, "refreshed_at", asOf, "source_as_of", asOf, "data", dataFor(days))) +
		"</body></html>"
}

type fixture struct {
	t      *testing.T
	d      *db.DB
	srv    *httptest.Server
	pageID jsx.ID
}

func newFixture(t *testing.T) *fixture {
	setSecret(t)
	d := testdb.New(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "fell through", http.StatusTeapot) })
	mux := http.NewServeMux()
	mux.Handle("/export/", http.StripPrefix("/export", &Handler{DB: d, Next: next}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := &fixture{t: t, d: d, srv: srv}
	f.pageID = jsx.ID(f.id(`INSERT INTO pages (slug, title) VALUES ($1, 'Northwind') RETURNING id`, slug))
	return f
}

func (f *fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	if err := f.d.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

// publish stores html as the page's new live version.
func (f *fixture) publish(html string) jsx.ID {
	sum := sha256.Sum256([]byte(html))
	v := f.id(`INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author)
	           VALUES ($1, $2, $3, 'approved', 'raw', 'qa@example.com') RETURNING id`, int64(f.pageID), html, hex.EncodeToString(sum[:]))
	f.exec(`UPDATE pages SET published_version_id = $2 WHERE id = $1`, int64(f.pageID), v)
	return jsx.ID(v)
}

func (f *fixture) token(label, scope string, slugs ...string) *tokens.Minted {
	f.t.Helper()
	m, err := tokens.Mint(context.Background(), f.d, tokens.MintParams{Label: label, Scope: scope, AllowedSlugs: slugs})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) mint(versionID, tokenID jsx.ID, ttl int) URLs {
	f.t.Helper()
	m, err := Mint(MintParams{PageID: f.pageID, VersionID: versionID, TokenID: tokenID}, ttl)
	if err != nil {
		f.t.Fatal(err)
	}
	return ExportURLs(m.Token)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (f *fixture) do(method, url string, body io.Reader) response {
	f.t.Helper()
	path := strings.TrimPrefix(url, "http://localhost")
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{res.StatusCode, res.Header, b}
}

func (f *fixture) refused(url, why string) {
	f.t.Helper()
	res := f.do("GET", url, nil)
	if res.status != 401 {
		f.t.Fatalf("%s: HTTP %d %s", why, res.status, res.body)
	}
	if got := string(res.body); got != `{"error":"unknown or expired data export URL","code":"data_export_invalid"}` {
		f.t.Fatalf("%s: %s", why, got)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestExportServesExactHashedBytes(t *testing.T) {
	f := newFixture(t)
	html := pageHTML(3, "2026-08-01T00:00:00.000Z")
	version := f.publish(html)
	refresher := f.token("export-refresher", tokens.ScopeDataUpdate, slug)
	urls := f.mint(version, refresher.ID, ExportTTLSeconds)
	if !strings.HasPrefix(urls.DataURL, "http://localhost/export/") || !strings.HasSuffix(urls.DataURL, "/data.json") {
		t.Fatalf("data_url %s", urls.DataURL)
	}
	if b, _ := json.Marshal(urls); !regexp.MustCompile(`^\{"schema_url":"[^"]+/schema\.json","data_url":"[^"]+/data\.json","envelope_url":"[^"]+/envelope\.json"\}$`).Match(b) {
		t.Fatalf("wire shape %s", b)
	}
	managed, err := pagedata.ParseManagedHTML(html)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for part, url := range map[string]string{"schema": urls.SchemaURL, "data": urls.DataURL, "envelope": urls.EnvelopeURL} {
		res := f.do("GET", url, nil)
		if res.status != 200 {
			t.Fatalf("%s: HTTP %d %s", part, res.status, res.body)
		}
		h := res.header
		if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s headers %v", part, h)
		}
		if want := `attachment; filename="` + slug + ".v" + version.String() + "." + part + `.json"`; h.Get("Content-Disposition") != want {
			t.Errorf("%s disposition %q, want %q", part, h.Get("Content-Disposition"), want)
		}
		if h.Get("Set-Cookie") != "" {
			t.Errorf("%s: an export sets no cookie", part)
		}
		files[part] = res.body
	}
	if sha(files["schema"]) != managed.SchemaSHA256 {
		t.Error("schema file does not hash to schema_sha256")
	}
	if sha(files["data"]) != managed.DataSHA256 {
		t.Error("data file does not hash to data_sha256")
	}
	env, err := jsonv.Parse(files["envelope"])
	if err != nil || !jsonv.Equal(env, managed.Envelope) {
		t.Errorf("envelope %s", files["envelope"])
	}
	data, err := jsonv.Parse(files["data"])
	if err != nil || !jsonv.Equal(data, managed.Envelope.Value("data")) {
		t.Errorf("data %s", files["data"])
	}
	if head := f.do("HEAD", urls.DataURL, nil); head.status != 200 || len(head.body) != 0 {
		t.Errorf("HEAD: %d %q", head.status, head.body)
	}
}

func TestExportURLIsNot(t *testing.T) {
	f := newFixture(t)
	version := f.publish(pageHTML(3, "2026-08-01T00:00:00.000Z"))
	refresher := f.token("export-refresher", tokens.ScopeDataUpdate, slug)
	urls := f.mint(version, refresher.ID, ExportTTLSeconds)

	posted := f.do("POST", urls.DataURL, strings.NewReader("{}"))
	if posted.status != 405 || posted.header.Get("Allow") != "GET, HEAD" ||
		string(posted.body) != `{"error":"export URLs are read-only","code":"method_not_allowed"}` {
		t.Errorf("POST: %d %v %s", posted.status, posted.header, posted.body)
	}
	token := strings.Split(strings.TrimPrefix(urls.DataURL, "http://localhost"), "/")[2]
	if res := f.do("GET", "/export/"+token+"/html.json", nil); res.status != 404 || string(res.body) != `{"error":"unknown export file","code":"not_found"}` {
		t.Errorf("unknown file: %d %s", res.status, res.body)
	}
	if res := f.do("GET", "/export/"+token, nil); res.status != http.StatusTeapot {
		t.Errorf("another path shape falls through: %d", res.status)
	}
	if res := f.do("GET", "/export/"+token+"/data.json/", nil); res.status != 200 {
		t.Errorf("trailing slash: %d", res.status)
	}

	body, sig, _ := strings.Cut(token, ".")
	swap := "AA"
	if strings.HasSuffix(sig, "AA") {
		swap = "BB"
	}
	f.refused("/export/"+body+"."+sig[:len(sig)-2]+swap+"/data.json", "a forged signature")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	claims, _ := jsonv.Parse(raw)
	claims.(*jsonv.Object).Set("exp", claims.(*jsonv.Object).Value("exp").(float64)+86400)
	moved := base64.RawURLEncoding.EncodeToString([]byte(jsonv.Stringify(claims)))
	f.refused("/export/"+moved+"."+sig+"/data.json", "an extended expiry")
	f.refused(f.mint(version, refresher.ID, -1).DataURL, "an expired URL")
	view, err := rawtoken.Mint(rawtoken.MintParams{PageID: int64(f.pageID), VersionID: int64(version), Purpose: rawtoken.View, RenderMode: "raw"}, rawtoken.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	f.refused("/export/"+view+"/data.json", "a /raw view token")
	f.refused("/export/not-a-token/data.json", "garbage")
	if Verify(view) != nil {
		t.Error("a /raw token verifies as an export token")
	}
	m, _ := Mint(MintParams{PageID: f.pageID, VersionID: version, TokenID: refresher.ID}, 60)
	if rawtoken.Verify(m.Token) != nil {
		t.Error("an export token verifies at /raw")
	}
}

func TestMintingTokenIsRecheckedOnEveryFetch(t *testing.T) {
	f := newFixture(t)
	version := f.publish(pageHTML(3, "2026-08-01T00:00:00.000Z"))
	doomed := f.token("export-doomed", tokens.ScopeDeploy)
	doomedURL := f.mint(version, doomed.ID, ExportTTLSeconds).DataURL
	if res := f.do("GET", doomedURL, nil); res.status != 200 {
		t.Fatalf("deploy token: %d %s", res.status, res.body)
	}
	if ok, err := tokens.Revoke(context.Background(), f.d, int64(doomed.ID)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	f.refused(doomedURL, "a URL minted by a since-revoked token")

	refresher := f.token("export-refresher", tokens.ScopeDataUpdate, slug)
	refresherURL := f.mint(version, refresher.ID, ExportTTLSeconds).DataURL
	if res := f.do("GET", refresherURL, nil); res.status != 200 {
		t.Fatalf("data_update token: %d %s", res.status, res.body)
	}
	f.exec(`DELETE FROM api_token_page_grants WHERE token_id = $1`, int64(refresher.ID))
	f.refused(refresherURL, "a URL whose token no longer holds the page grant")

	other := f.token("export-other", tokens.ScopeDataUpdate, "fabrikam-other")
	f.refused(f.mint(version, other.ID, ExportTTLSeconds).DataURL, "a token never granted this page")
}

func TestExportURLIsPinnedToOneVersion(t *testing.T) {
	f := newFixture(t)
	beforeHTML := pageHTML(3, "2026-08-01T00:00:00.000Z")
	before := f.publish(beforeHTML)
	deployer := f.token("export-deployer", tokens.ScopeDeploy)
	pinned := f.mint(before, deployer.ID, ExportTTLSeconds)
	afterHTML := pageHTML(4, "2026-08-02T00:00:00.000Z")
	after := f.publish(afterHTML)

	beforeManaged, _ := pagedata.ParseManagedHTML(beforeHTML)
	afterManaged, _ := pagedata.ParseManagedHTML(afterHTML)
	if res := f.do("GET", pinned.DataURL, nil); sha(res.body) != beforeManaged.DataSHA256 {
		t.Error("an old URL keeps serving the version it named")
	}
	fresh := f.mint(after, deployer.ID, ExportTTLSeconds)
	if res := f.do("GET", fresh.DataURL, nil); sha(res.body) != afterManaged.DataSHA256 {
		t.Error("a fresh URL serves the new version")
	}
	// A version id must belong to the page id it is minted with.
	other := jsx.ID(f.id(`INSERT INTO pages (slug, title) VALUES ('fabrikam-other', 'Fabrikam') RETURNING id`))
	m, _ := Mint(MintParams{PageID: other, VersionID: after, TokenID: deployer.ID}, ExportTTLSeconds)
	if res := f.do("GET", ExportURLs(m.Token).DataURL, nil); res.status != 404 {
		t.Errorf("version of another page: %d", res.status)
	}

	f.exec(`UPDATE pages SET deleted_at = now() WHERE id = $1`, int64(f.pageID))
	gone := f.do("GET", fresh.DataURL, nil)
	if gone.status != 404 || string(gone.body) != `{"error":"page data not found","code":"page_not_found"}` {
		t.Errorf("a deleted page exports nothing: %d %s", gone.status, gone.body)
	}
}

func TestMintRefusals(t *testing.T) {
	t.Setenv("RAW_TOKEN_SECRET", "")
	_, err := Mint(MintParams{PageID: 1, VersionID: 2, TokenID: 3}, ExportTTLSeconds)
	if e, ok := apierror.As(err); !ok || e.Status != 409 || e.Code != "data_export_unavailable" {
		t.Fatalf("no secret: %v", err)
	}
	if Verify("a.b") != nil {
		t.Error("verifies without a secret")
	}
	setSecret(t)
	if _, err := Mint(MintParams{PageID: 1, TokenID: 3}, ExportTTLSeconds); err == nil || err.Error() != "data export versionId is missing" {
		t.Fatalf("missing version: %v", err)
	}
	m, err := Mint(MintParams{PageID: 12, VersionID: 34, TokenID: 5}, ExportTTLSeconds)
	if err != nil {
		t.Fatal(err)
	}
	c := Verify(m.Token)
	if c == nil || c.PageID != "12" || c.VersionID != "34" || c.TokenID != "5" {
		t.Fatalf("claims %+v", c)
	}
	body, _, _ := strings.Cut(m.Token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	if !regexp.MustCompile(`^\{"aud":"pages-data-export","pid":"12","vid":"34","tid":"5","exp":\d+\}$`).Match(raw) {
		t.Errorf("claims body %s", raw)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.000Z$`).MatchString(m.ExpiresAt) {
		t.Errorf("expiresAt %s", m.ExpiresAt)
	}
	for _, bad := range []string{"", ".", "a.", ".a", "a.b.c", m.Token + "x", strings.Replace(m.Token, ".", "..", 1)} {
		if Verify(bad) != nil {
			t.Errorf("Verify(%q) passed", bad)
		}
	}
}
