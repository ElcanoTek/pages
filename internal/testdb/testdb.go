// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package testdb gives a test its own freshly migrated Postgres database.
//
// It connects to PAGES_TEST_DATABASE_URL (default
// postgres://pages@127.0.0.1:5433/postgres?sslmode=disable — the cluster
// `make test-db` starts), creates a uniquely named database, migrates it and
// drops it when the test ends. When no server answers, the test is skipped —
// unless PAGES_TEST_REQUIRE_DB=1 (CI sets it), when it fails instead, so a
// green CI run always means the database suite ran.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/migrate"
)

// DefaultURL is the admin connection used when PAGES_TEST_DATABASE_URL is unset.
const DefaultURL = "postgres://pages@127.0.0.1:5433/postgres?sslmode=disable"

// AdminURL is the maintenance connection string tests create databases through.
func AdminURL() string {
	if v := os.Getenv("PAGES_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return DefaultURL
}

// New creates, migrates and returns a private database, and sets DATABASE_URL
// to it for the rest of the test (so code reading the environment agrees).
func New(t testing.TB) *db.DB {
	t.Helper()
	d, _ := NewWithURL(t)
	return d
}

// NewWithURL is New, also returning the database's connection string.
func NewWithURL(t testing.TB) (*db.DB, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		if os.Getenv("PAGES_TEST_REQUIRE_DB") == "1" {
			t.Fatalf("testdb: no Postgres at PAGES_TEST_DATABASE_URL: %v", err)
		}
		t.Skipf("testdb: no Postgres available (%v); set PAGES_TEST_DATABASE_URL or run `make test-db`", err)
	}
	defer admin.Close(context.Background())

	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "pages_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	u, err := url.Parse(AdminURL())
	if err != nil {
		t.Fatalf("testdb: parse url: %v", err)
	}
	u.Path = "/" + name
	connString := u.String()

	bounds := db.BoundsFromEnv()
	d, err := db.Open(connString, bounds)
	if err != nil {
		t.Fatalf("testdb: open: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		c, err := pgx.Connect(context.Background(), AdminURL())
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	if _, err := migrate.Run(ctx, d.Pool, pages.Migrations(), migrate.Options{LockTimeout: 30 * time.Second}); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}
	t.Setenv("DATABASE_URL", connString)
	return d, connString
}
