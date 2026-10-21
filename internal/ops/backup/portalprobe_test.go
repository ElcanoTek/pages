// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"context"
	"net"
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
	if _, err := d.Exec(ctx, `
WITH p AS (INSERT INTO pages (slug, title) VALUES ('contoso/q3', 'Contoso Q3') RETURNING id),
v AS (INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author)
      SELECT id, $1::text, encode(sha256(convert_to($1::text, 'UTF8')), 'hex'), 'approved', 'raw', 'probe@example.com' FROM p RETURNING id, page_id),
u AS (UPDATE pages SET published_version_id = v.id FROM v WHERE pages.id = v.page_id RETURNING pages.id),
pp AS (INSERT INTO page_portals (slug, name, password_hash) VALUES ('contoso', 'Contoso', $2) RETURNING id)
INSERT INTO page_portal_members (portal_id, page_id) SELECT pp.id, p.id FROM pp, p`, html, hash); err != nil {
		t.Fatal(err)
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
