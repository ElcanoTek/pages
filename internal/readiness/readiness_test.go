// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package readiness

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/testdb"
)

func serve(t *testing.T, c *Checker) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("no-store")
	}
	return rec.Code, rec.Body.String()
}

func TestReadiness(t *testing.T) {
	d, url := testdb.NewWithURL(t)
	ctx := context.Background()
	c, err := New(url, pages.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, body := serve(t, c); code != 200 || body != `{"status":"ready"}` {
		t.Fatal(code, body)
	}
	var filename string
	var applied time.Time
	if err := d.QueryRow(ctx, `DELETE FROM schema_migrations WHERE filename=(SELECT filename FROM schema_migrations ORDER BY filename DESC LIMIT 1) RETURNING filename, applied_at`).Scan(&filename, &applied); err != nil {
		t.Fatal(err)
	}
	if code, _ := serve(t, c); code != 503 {
		t.Fatal("a pending migration prevents readiness")
	}
	if _, err := d.Exec(ctx, `INSERT INTO schema_migrations(filename, applied_at) VALUES ($1, $2)`, filename, applied); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `ALTER TABLE schema_migrations RENAME TO readiness_migrations_backup`); err != nil {
		t.Fatal(err)
	}
	code, body := serve(t, c)
	if code != 503 || body != `{"status":"not_ready"}` {
		t.Fatal("absent schema prevents readiness", code, body)
	}
	if _, err := d.Exec(ctx, `ALTER TABLE readiness_migrations_backup RENAME TO schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if code, _ := serve(t, c); code != 200 {
		t.Fatal("readiness recovers after schema repair")
	}

	// Simultaneous callers share one probe and all get the answer.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !c.Check(ctx) {
				t.Error("concurrent check")
			}
		}()
	}
	wg.Wait()
}

func TestUnavailableDatabaseFailsPromptly(t *testing.T) {
	c, err := New("postgres://pages@127.0.0.1:1/pages?sslmode=disable", pages.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if code, _ := serve(t, c); code != 503 {
		t.Fatal(code)
	}
	if time.Since(start) > 3500*time.Millisecond {
		t.Fatal("unavailable database fails promptly")
	}
	bad, _ := New("::not a url::", pages.Migrations())
	if bad.Check(context.Background()) {
		t.Fatal("bad configuration is not ready")
	}
}
