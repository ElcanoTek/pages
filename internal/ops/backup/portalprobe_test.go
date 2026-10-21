// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/server"
	"github.com/ElcanoTek/pages/internal/testdb"
)

// A page with no password of its own that sits in a live portal answers with
// the portal's password gate (401), not "not shared yet" (403). The probe
// used to expect 403 for every passwordless page, so a perfectly sound backup
// of a box with partner portals failed its own integrity check.
func TestProbeAcceptsPasswordlessPortalMember(t *testing.T) {
	d, connString := testdb.NewWithURL(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		"RAW_TOKEN_SECRET": "probe-raw", "PAGE_COOKIE_SECRET": "probe-cookie", "API_TOKEN_PEPPER": "probe-pepper",
		"DASHBOARD_HOST": "localhost", "CONTENT_HOST": "content.localhost", "PAGES_AUTH_MODE": "elcano",
	} {
		t.Setenv(k, v)
	}
	u, err := url.Parse(connString)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	t.Setenv("PGHOST", host)
	t.Setenv("PGPORT", port)
	t.Setenv("PGUSER", u.User.Username())
	if pw, ok := u.User.Password(); ok {
		t.Setenv("PGPASSWORD", pw)
	}
	t.Setenv("PGDATABASE", strings.TrimPrefix(u.Path, "/"))
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("DATABASE_URL", "")

	hash, err := pagecookie.HashPassword("contoso-partner-password")
	if err != nil {
		t.Fatal(err)
	}
	html := "<!doctype html><html><body><h1>Contoso</h1></body></html>"
	var pageID, versionID int64
	if err := d.QueryRow(ctx, `INSERT INTO pages (slug, title) VALUES ('contoso/q3', 'Contoso Q3') RETURNING id`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(ctx, `INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author)
		VALUES ($1, $2::text, encode(sha256(convert_to($2::text, 'UTF8')), 'hex'), 'approved', 'raw', 'probe@example.com') RETURNING id`,
		pageID, html).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE pages SET published_version_id = $1 WHERE id = $2`, []any{versionID, pageID}},
		{`INSERT INTO page_portals (slug, name, password_hash) VALUES ('contoso', 'Contoso', $1)`, []any{hash}},
		{`INSERT INTO page_portal_members (portal_id, page_id) SELECT id, $1 FROM page_portals WHERE slug = 'contoso'`, []any{pageID}},
	} {
		if _, err := d.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	srv, err := server.New(server.Config{DB: d})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	req := httptest.NewRequest("GET", "/contoso/q3", nil)
	req.Host = "content.localhost"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("content host answered %d for a passwordless portal member, want 401: %q", rec.Code, rec.Header())
	}

	saved := backupprobe.Hooks.Serve
	backupprobe.Hooks.Serve = func(ctx context.Context, ln net.Listener) error {
		return server.Run(ctx, ln, server.RunOptions{})
	}
	t.Cleanup(func() { backupprobe.Hooks.Serve = saved })

	res, err := backupprobe.Run(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("probe refused a sound backup with a passwordless portal member: %v", err)
	}
	if res.Probes.Pages != 1 {
		t.Fatalf("probed %d pages, want 1", res.Probes.Pages)
	}
}
