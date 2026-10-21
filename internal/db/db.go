// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package db is Pages' Postgres access: one pgx pool with bounded waits, a
// transaction helper, and the handful of read queries the content host needs
// on every request.
//
// The pool connects lazily (no connection until the first query), so the
// dashboard host boots before database configuration exists. DATABASE_URL is
// used when set, else the standard libpq PG* variables.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages/internal/env"
)

// Querier is what a query needs: satisfied by *DB, *pgxpool.Pool and pgx.Tx,
// so a helper can run either standalone or inside a caller's transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB is the process's connection pool.
type DB struct {
	*pgxpool.Pool
}

// Bounds are the server-side per-session limits every pooled connection
// carries (issue #10). Every mutation starts with SELECT … FOR UPDATE —
// deliberate per-page writer serialization — so without bounds one wedged
// transaction pins a connection per waiter and a single page can exhaust the
// pool and hang BOTH hosts. 0 disables a bound, matching Postgres.
type Bounds struct {
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	IdleInTxnTimeout time.Duration
	MaxConns         int32
	ApplicationName  string
}

// BoundsFromEnv reads PG_CONNECT_TIMEOUT_MS, PG_STATEMENT_TIMEOUT_MS,
// PG_LOCK_TIMEOUT_MS and PG_IDLE_TXN_TIMEOUT_MS (milliseconds).
func BoundsFromEnv() Bounds {
	ms := func(name string, def int) time.Duration {
		return time.Duration(env.NonNegativeInt(name, def)) * time.Millisecond
	}
	return Bounds{
		ConnectTimeout:   ms("PG_CONNECT_TIMEOUT_MS", 5000),
		StatementTimeout: ms("PG_STATEMENT_TIMEOUT_MS", 15000),
		LockTimeout:      ms("PG_LOCK_TIMEOUT_MS", 10000),
		IdleInTxnTimeout: ms("PG_IDLE_TXN_TIMEOUT_MS", 30000),
		MaxConns:         int32(min(max(env.Int("PG_POOL_MAX", 10), 1), 1000)), // #nosec G115 -- clamped to [1, 1000]
		ApplicationName:  "pages",
	}
}

// ConnString is DATABASE_URL, or "" to let pgx read the libpq PG* variables.
func ConnString() string { return os.Getenv("DATABASE_URL") }

// Open builds the pool for connString ("" ⇒ PG* variables) with bounds b. It
// does not connect.
func Open(connString string, b Bounds) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}
	if b.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = b.ConnectTimeout
	}
	if b.MaxConns > 0 {
		cfg.MaxConns = b.MaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["statement_timeout"] = fmt.Sprint(b.StatementTimeout.Milliseconds())
	rp["lock_timeout"] = fmt.Sprint(b.LockTimeout.Milliseconds())
	rp["idle_in_transaction_session_timeout"] = fmt.Sprint(b.IdleInTxnTimeout.Milliseconds())
	if b.ApplicationName != "" && rp["application_name"] == "" {
		rp["application_name"] = b.ApplicationName
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// OpenFromEnv is Open(ConnString(), BoundsFromEnv()).
func OpenFromEnv() (*DB, error) { return Open(ConnString(), BoundsFromEnv()) }

// WithTx runs fn inside BEGIN/COMMIT, rolling back on any error or panic. The
// version state machine runs every mutation and its audit_log row through
// here, so they commit or roll back together.
func (d *DB) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithTxResult is WithTx for a function that produces a value.
func WithTxResult[T any](ctx context.Context, d *DB, fn func(tx pgx.Tx) (T, error)) (T, error) {
	var out T
	err := d.WithTx(ctx, func(tx pgx.Tx) error {
		v, err := fn(tx)
		out = v
		return err
	})
	return out, err
}

// IsNoRows reports whether err is pgx.ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
