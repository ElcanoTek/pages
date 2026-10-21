// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compat

import (
	"context"
	"regexp"
	"testing"

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
