// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package compat decides whether an automatic update may migrate this
// database. Automatic rollback restores code, never schema, so the updater
// only applies pending migrations that migrations/compatibility.json declares
// backward-compatible (the predecessor release keeps working on the newer
// schema), and only on a database that already has the declared baseline.
package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Policy is migrations/compatibility.json.
type Policy struct {
	MinimumPreviousSchema string            `json:"minimum_previous_schema"`
	Migrations            map[string]string `json:"migrations"`
}

// LoadPolicy reads compatibility.json and the *.sql inventory from dir
// (pages.Migrations()).
func LoadPolicy(dir fs.FS) (Policy, []string, error) {
	var p Policy
	b, err := fs.ReadFile(dir, "compatibility.json")
	if err != nil {
		return p, nil, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, nil, fmt.Errorf("compatibility.json: %w", err)
	}
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return p, nil, err
	}
	var available []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			available = append(available, e.Name())
		}
	}
	sort.Strings(available)
	return p, available, nil
}

// Check refuses an automatic update when the database predates the baseline,
// carries migrations this release does not know (a downgrade), or has pending
// migrations not declared backward-compatible.
func Check(applied, available []string, p Policy) error {
	done := map[string]bool{}
	for _, a := range applied {
		done[a] = true
	}
	if !done[p.MinimumPreviousSchema] {
		return fmt.Errorf("automatic update requires schema %s; use the documented manual migration/restore procedure for older installations", p.MinimumPreviousSchema)
	}
	known := map[string]bool{}
	for _, a := range available {
		known[a] = true
	}
	var unknown []string
	for _, a := range applied {
		if !known[a] {
			unknown = append(unknown, a)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("database has migrations absent from this release: %s; downgrade requires a reviewed manual restore", strings.Join(unknown, ", "))
	}
	var unclassified []string
	for _, a := range available {
		if !done[a] && p.Migrations[a] != "backward-compatible" {
			unclassified = append(unclassified, a)
		}
	}
	if len(unclassified) > 0 {
		return fmt.Errorf("pending migrations require a reviewed manual upgrade: %s; automatic code rollback is not declared compatible", strings.Join(unclassified, ", "))
	}
	return nil
}

// Applied lists schema_migrations.
func Applied(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, "SELECT filename FROM schema_migrations ORDER BY filename")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ErrNotReady is returned by Ready when a migration is missing.
var ErrNotReady = errors.New("schema not ready")

// Ready is the bounded database readiness probe /readyz answers with: the
// core rendering tables parse and every migration this release ships is
// recorded. The updater runs it (as `pages ready-check`) for a predecessor
// that predates /readyz and only answers /healthz.
func Ready(ctx context.Context, pool *pgxpool.Pool, available []string) error {
	if _, err := pool.Exec(ctx, "SELECT p.slug, p.published_version_id, v.html, t.default_mode FROM pages p LEFT JOIN page_versions v ON v.id=p.published_version_id LEFT JOIN themes t ON t.id=p.theme_id WHERE false"); err != nil {
		return err
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE filename=ANY($1::text[])", available).Scan(&n); err != nil {
		return err
	}
	if n != len(available) {
		return ErrNotReady
	}
	return nil
}

// DefaultActiveUploadWindow is how recently an uncommitted staged upload must
// have been created or touched to count as an active write in progress.
const DefaultActiveUploadWindow = 5 * time.Minute

// ActiveWrites lists multi-request staged uploads and in-flight database write
// transactions that would be interrupted by stopping pages.service.
func ActiveWrites(ctx context.Context, pool *pgxpool.Pool, uploadWindow time.Duration) ([]string, error) {
	if uploadWindow <= 0 {
		uploadWindow = DefaultActiveUploadWindow
	}
	windowSeconds := max(int64(uploadWindow/time.Second), 1)

	var active []string
	var hasUploads bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.page_content_uploads') IS NOT NULL").Scan(&hasUploads); err != nil {
		return nil, err
	}
	if hasUploads {
		rows, err := pool.Query(ctx, `
			SELECT id::text,
			       slug,
			       bytes_received,
			       total_bytes,
			       GREATEST(0, EXTRACT(EPOCH FROM (now() - GREATEST(created_at, updated_at, COALESCE(ticket_used_at, created_at))))::bigint)
			  FROM page_content_uploads
			 WHERE committed_at IS NULL
			   AND expires_at > now()
			   AND GREATEST(created_at, updated_at, COALESCE(ticket_used_at, created_at)) > now() - ($1::bigint * interval '1 second')
			 ORDER BY GREATEST(created_at, updated_at, COALESCE(ticket_used_at, created_at)) DESC
			 LIMIT 5`, windowSeconds)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, slug string
			var received, total int
			var ageSec int64
			if err := rows.Scan(&id, &slug, &received, &total, &ageSec); err != nil {
				rows.Close()
				return nil, err
			}
			active = append(active, fmt.Sprintf("staged upload %s for %s (%d/%d bytes, touched %ds ago)", id, slug, received, total, ageSec))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	txRows, err := pool.Query(ctx, `
		SELECT pid,
		       state,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(xact_start, query_start)))::bigint)
		  FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND pid <> pg_backend_pid()
		   AND xact_start IS NOT NULL
		   AND state IN ('active', 'idle in transaction', 'fastpath function call')
		   AND (
		        backend_xid IS NOT NULL
		        OR query ~* '^\s*(INSERT|UPDATE|DELETE|COPY|LOCK)(\s|$)'
		        OR (query ~* '^\s*WITH(\s|$)' AND query ~* '(^|[^[:alnum:]_])(INSERT|UPDATE|DELETE)(\s|$)')
		   )
		 ORDER BY COALESCE(xact_start, query_start) ASC
		 LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer txRows.Close()
	for txRows.Next() {
		var pid int
		var state string
		var ageSec int64
		if err := txRows.Scan(&pid, &state, &ageSec); err != nil {
			return nil, err
		}
		active = append(active, fmt.Sprintf("database write transaction pid=%d (%s, %ds)", pid, state, ageSec))
	}
	if err := txRows.Err(); err != nil {
		return nil, err
	}
	return active, nil
}

// WaitIdle polls ActiveWrites until no staged uploads or write transactions
// remain in flight, or until maxWait elapses.
func WaitIdle(ctx context.Context, pool *pgxpool.Pool, uploadWindow, maxWait, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	deadline := time.Now().Add(max(maxWait, 0))
	for {
		active, err := ActiveWrites(ctx, pool, uploadWindow)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			return nil
		}
		if maxWait <= 0 || !time.Now().Before(deadline) {
			return fmt.Errorf("active write in progress: %s", strings.Join(active, "; "))
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
