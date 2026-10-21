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
