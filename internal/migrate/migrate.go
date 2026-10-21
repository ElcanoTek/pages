// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package migrate applies migrations/*.sql in filename order, once each, each
// inside its own transaction, tracked in schema_migrations. Re-running applies
// only new files. Concurrent runners serialize on a session advisory lock that
// spans the tracking-table creation, the read of applied files and every
// per-file commit.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two int32 keys name Pages' migration lock independently of schema/search_path.
const lockKey1, lockKey2 = int32(0x50414745), int32(0x4d494752)

// ErrBusy is returned when another runner held the lock for the whole wait.
var ErrBusy = errors.New("MIGRATION_BUSY")

// Options tunes a run.
type Options struct {
	// LockTimeout is how long to wait for another migrator (0 fails at once).
	LockTimeout time.Duration
	// Log receives one line per applied file and a summary (nil ⇒ discard).
	Log io.Writer
}

// Run applies every pending migration in dir to the database behind pool.
func Run(ctx context.Context, pool *pgxpool.Pool, dir fs.FS, opts Options) (applied int, err error) {
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	locked, discard := false, false
	defer func() {
		if locked {
			if _, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1, $2)", lockKey1, lockKey2); uerr != nil {
				discard = true // never return a possibly locked session to the pool
			}
		}
		if discard {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}()

	deadline := time.Now().Add(opts.LockTimeout)
	for {
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", lockKey1, lockKey2).Scan(&locked); err != nil {
			return 0, err
		}
		if locked {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, fmt.Errorf("%w: another Pages migration is running; waited %dms — retry after it finishes", ErrBusy, opts.LockTimeout.Milliseconds())
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(min(100*time.Millisecond, remaining)):
		}
	}

	if _, err := conn.Exec(ctx, `
      CREATE TABLE IF NOT EXISTS schema_migrations (
        filename   TEXT PRIMARY KEY,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
      )`); err != nil {
		return 0, err
	}
	done := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT filename FROM schema_migrations")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return 0, err
		}
		done[f] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	files, err := Files(dir)
	if err != nil {
		return 0, err
	}
	for _, file := range files {
		if done[file] {
			continue
		}
		sql, err := fs.ReadFile(dir, file)
		if err != nil {
			return applied, err
		}
		if err := applyOne(ctx, conn.Conn().PgConn(), file, string(sql)); err != nil {
			return applied, err
		}
		fmt.Fprintf(log, "migrated: %s\n", file)
		applied++
	}
	if applied > 0 {
		fmt.Fprintf(log, "applied %d migration(s)\n", applied)
	} else {
		fmt.Fprintln(log, "schema up to date")
	}
	return applied, nil
}

// applyOne runs one file and its tracking row in a single transaction. It uses
// the simple protocol so a file may hold many statements.
func applyOne(ctx context.Context, pg *pgconn.PgConn, file, sql string) error {
	quoted := strings.ReplaceAll(file, "'", "''")
	script := "BEGIN;\n" + sql + "\n;INSERT INTO schema_migrations (filename) VALUES ('" + quoted + "');\nCOMMIT;"
	if err := pg.Exec(ctx, script).Close(); err != nil {
		_ = pg.Exec(context.WithoutCancel(ctx), "ROLLBACK").Close()
		// Carry DETAIL/HINT through: a migration that asserts a precondition
		// (RAISE EXCEPTION … USING HINT = …) puts the remedy there.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			extra := strings.TrimSpace(strings.Join(nonEmpty(pgErr.Detail, pgErr.Hint), " "))
			if extra != "" {
				return fmt.Errorf("migration %s failed: %s — %s", file, pgErr.Message, extra)
			}
			return fmt.Errorf("migration %s failed: %s", file, pgErr.Message)
		}
		return fmt.Errorf("migration %s failed: %w", file, err)
	}
	return nil
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Files lists dir's *.sql files in apply order.
func Files(dir fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}
