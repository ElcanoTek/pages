// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/migrate"
	"github.com/ElcanoTek/pages/internal/testdb"
)

func TestRerunIsANoOp(t *testing.T) {
	d := testdb.New(t)
	n, err := migrate.Run(context.Background(), d.Pool, pages.Migrations(), migrate.Options{LockTimeout: time.Second})
	if err != nil || n != 0 {
		t.Fatalf("second run applied %d, err %v", n, err)
	}
	files, _ := migrate.Files(pages.Migrations())
	var count int
	if err := d.QueryRow(context.Background(), "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != len(files) {
		t.Fatalf("schema_migrations has %d rows, want %d (%v)", count, len(files), err)
	}
}
