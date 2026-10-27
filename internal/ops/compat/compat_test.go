// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compat

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/testdb"
)

func TestAutomaticUpdatesRequireBaselineAndReviewedMigrations(t *testing.T) {
	p := Policy{MinimumPreviousSchema: "001_base.sql", Migrations: map[string]string{"002_added.sql": "backward-compatible", "003_renamed.sql": "manual"}}
	ok := func(applied, available []string) {
		t.Helper()
		if err := Check(applied, available, p); err != nil {
			t.Fatal(err)
		}
	}
	bad := func(applied, available []string, pattern string) {
		t.Helper()
		if err := Check(applied, available, p); err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
			t.Fatalf("%v, want /%s/", err, pattern)
		}
	}
	ok([]string{"001_base.sql"}, []string{"001_base.sql", "002_added.sql"})
	bad(nil, []string{"001_base.sql"}, `older installations`)
	bad([]string{"001_base.sql"}, []string{"001_base.sql", "002_added.sql", "003_renamed.sql"}, `manual upgrade.*003_renamed`)
	bad([]string{"001_base.sql"}, []string{"001_base.sql", "004_unknown.sql"}, `manual upgrade.*004_unknown`)
	bad([]string{"001_base.sql", "005_future.sql"}, []string{"001_base.sql"}, `downgrade requires a reviewed manual restore`)
	ok([]string{"001_base.sql", "003_renamed.sql"}, []string{"001_base.sql", "003_renamed.sql"})
}

func TestShippedPolicyLoadsAndAMigratedDatabaseIsCompatibleAndReady(t *testing.T) {
	p, available, err := LoadPolicy(pages.Migrations())
	if err != nil || p.MinimumPreviousSchema == "" || len(available) == 0 {
		t.Fatal(p, available, err)
	}
	for name, class := range p.Migrations {
		if class != "backward-compatible" && class != "manual" {
			t.Fatalf("%s: unknown classification %q", name, class)
		}
	}
	d := testdb.New(t)
	ctx := context.Background()
	applied, err := Applied(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(applied, available, p); err != nil {
		t.Fatal(err)
	}
	if err := Ready(ctx, d.Pool, available); err != nil {
		t.Fatal(err)
	}
	if err := Ready(ctx, d.Pool, append(available, "999_future.sql")); err == nil {
		t.Fatal("a missing migration must read as not ready")
	}
}

func TestActiveWritesAndWaitIdle(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	// Clean database: no active writes.
	active, err := ActiveWrites(ctx, d.Pool, 5*time.Minute)
	if err != nil || len(active) != 0 {
		t.Fatalf("active = %v, err = %v", active, err)
	}
	if err := WaitIdle(ctx, d.Pool, 5*time.Minute, 0, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle on idle db: %v", err)
	}

	// Insert a token row so page_content_uploads FK is satisfied.
	var tokenID int64
	if err := d.QueryRow(ctx, `INSERT INTO api_tokens (label, token_hash, prefix) VALUES ('northwind-agent', repeat('0', 64), 'pgs_0000') RETURNING id`).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}

	// Insert a stale uncommitted upload (>10m old) and an already-committed upload: neither blocks.
	_, err = d.Exec(ctx, `
		INSERT INTO page_content_uploads (
			id, token_id, slug, total_bytes, content_sha256,
			created_at, updated_at, expires_at
		) VALUES
		(
			'00000000-0000-0000-0000-000000000001', $1, 'stale-northwind', 1024,
			repeat('a', 64),
			now() - interval '10 minutes', now() - interval '10 minutes', now() + interval '20 minutes'
		),
		(
			'00000000-0000-0000-0000-000000000002', $1, 'committed-northwind', 1024,
			repeat('b', 64),
			now() - interval '10 seconds', now() - interval '5 seconds', now() + interval '20 minutes'
		)`, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `UPDATE page_content_uploads SET commit_key = repeat('d', 64), commit_result = '{"ok":true}'::jsonb, committed_at = now() WHERE id = '00000000-0000-0000-0000-000000000002'`); err != nil {
		t.Fatal(err)
	}
	active, err = ActiveWrites(ctx, d.Pool, 5*time.Minute)
	if err != nil || len(active) != 0 {
		t.Fatalf("stale/committed uploads must not block: active=%v err=%v", active, err)
	}

	// Insert an active uncommitted upload touched 2 seconds ago: must block.
	_, err = d.Exec(ctx, `
		INSERT INTO page_content_uploads (
			id, token_id, slug, bytes_received, total_bytes, content_sha256,
			created_at, updated_at, expires_at
		) VALUES (
			'00000000-0000-0000-0000-000000000003', $1, 'live-northwind', 4096, 8192,
			repeat('c', 64),
			now() - interval '5 seconds', now() - interval '2 seconds', now() + interval '25 minutes'
		)`, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	active, err = ActiveWrites(ctx, d.Pool, 5*time.Minute)
	if err != nil || len(active) != 1 || !strings.Contains(active[0], "live-northwind") {
		t.Fatalf("expected live-northwind active upload, got active=%v err=%v", active, err)
	}
	if err := WaitIdle(ctx, d.Pool, 5*time.Minute, 0, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "live-northwind") {
		t.Fatalf("expected WaitIdle with 0 timeout to block on live-northwind, got %v", err)
	}

	// Commit the upload while WaitIdle is polling: WaitIdle must succeed.
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = d.Exec(context.Background(), `UPDATE page_content_uploads SET commit_key = repeat('e', 64), commit_result = '{"ok":true}'::jsonb, committed_at = now() WHERE id = '00000000-0000-0000-0000-000000000003'`)
	}()
	if err := WaitIdle(ctx, d.Pool, 5*time.Minute, 2*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle should succeed once upload commits: %v", err)
	}

	// Open an explicit write transaction on a separate connection: ActiveWrites must detect it.
	tx, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO pages (slug, title) VALUES ('tx-northwind', 'Northwind')`); err != nil {
		t.Fatal(err)
	}
	active, err = ActiveWrites(ctx, d.Pool, 5*time.Minute)
	if err != nil || len(active) == 0 || !strings.Contains(active[0], "database write transaction") {
		t.Fatalf("expected active write transaction to be reported, got active=%v err=%v", active, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	active, err = ActiveWrites(ctx, d.Pool, 5*time.Minute)
	if err != nil || len(active) != 0 {
		t.Fatalf("after rollback, expected no active writes, got active=%v err=%v", active, err)
	}
}
