// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package readiness answers /readyz: can this process serve pages right now —
// is the database reachable, and does it carry every migration this binary
// ships? (/healthz is liveness and never touches the database.)
//
// The probe keeps out of the application's pool, so exhausted application
// connections cannot queue unbounded health checks: it has its own one
// connection with one-second connect, statement and lock bounds, closed
// promptly when idle. Simultaneous callers share one in-flight probe.
package readiness

import (
	"context"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/migrate"
)

const bound = time.Second

// Checker is a readiness probe with its own single-connection pool.
type Checker struct {
	pool       *pgxpool.Pool
	poolErr    error
	migrations []string

	mu      sync.Mutex
	pending *call
}

type call struct {
	done  chan struct{}
	ready bool
}

// New builds a Checker for connString ("" ⇒ the libpq PG* variables) that
// requires every migration file in migrations (pages.Migrations()) to be
// recorded in schema_migrations. It does not connect.
func New(connString string, migrations fs.FS) (*Checker, error) {
	files, err := migrate.Files(migrations)
	if err != nil {
		return nil, err
	}
	c := &Checker{migrations: files}
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		// Readiness replies stay generic: a bad configuration is "not ready".
		c.poolErr = err
		return c, nil //nolint:nilerr // reported through Check, by design
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = bound
	cfg.HealthCheckPeriod = bound
	cfg.ConnConfig.ConnectTimeout = bound
	rp := cfg.ConnConfig.RuntimeParams
	rp["statement_timeout"] = "1000"
	rp["lock_timeout"] = "1000"
	if rp["application_name"] == "" {
		rp["application_name"] = "pages-readiness"
	}
	c.pool, c.poolErr = pgxpool.NewWithConfig(context.Background(), cfg)
	return c, nil
}

// NewFromEnv is New(db.ConnString(), migrations).
func NewFromEnv(migrations fs.FS) (*Checker, error) { return New(db.ConnString(), migrations) }

// Close releases the probe's pool.
func (c *Checker) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

func (c *Checker) probe(ctx context.Context) bool {
	if c.poolErr != nil || c.pool == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*bound)
	defer cancel()
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, bound)
	conn, err := c.pool.Acquire(acquireCtx)
	cancelAcquire()
	if err != nil {
		return false
	}
	healthy := false
	defer func() {
		if !healthy {
			// A failed probe must not leave a possibly-broken connection for
			// the next one.
			_ = conn.Conn().Close(context.Background())
		}
		conn.Release()
	}()
	q := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, bound) }
	// Parsing this zero-row read checks the current rendering tables/columns
	// and read permissions without scanning page data.
	qctx, qcancel := q()
	rows, err := conn.Query(qctx, `SELECT p.slug, p.published_version_id, v.html, t.default_mode FROM pages p
		LEFT JOIN page_versions v ON v.id=p.published_version_id LEFT JOIN themes t ON t.id=p.theme_id WHERE false`)
	if err == nil {
		rows.Close()
		err = rows.Err()
	}
	qcancel()
	if err != nil {
		return false
	}
	qctx, qcancel = q()
	defer qcancel()
	var n int
	if err := conn.QueryRow(qctx, `SELECT count(*)::int FROM schema_migrations WHERE filename = ANY($1::text[])`, c.migrations).Scan(&n); err != nil {
		return false
	}
	healthy = true
	return n == len(c.migrations)
}

// Check reports readiness. Concurrent callers share one probe.
func (c *Checker) Check(ctx context.Context) bool {
	c.mu.Lock()
	if p := c.pending; p != nil {
		c.mu.Unlock()
		select {
		case <-p.done:
			return p.ready
		case <-ctx.Done():
			return false
		}
	}
	p := &call{done: make(chan struct{})}
	c.pending = p
	c.mu.Unlock()
	// The shared probe must not die with the first caller's request.
	p.ready = c.probe(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.pending = nil
	c.mu.Unlock()
	close(p.done)
	return p.ready
}

type status struct {
	Status string `json:"status"`
}

// ServeHTTP answers {"status":"ready"} 200 or {"status":"not_ready"} 503,
// never cached.
func (c *Checker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ready := c.Check(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	if ready {
		httpx.SendJSON(w, r, http.StatusOK, status{"ready"})
		return
	}
	httpx.SendJSON(w, r, http.StatusServiceUnavailable, status{"not_ready"})
}
