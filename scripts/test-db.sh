#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/test-db.sh — the throwaway PostgreSQL 16 the Go database tests use.
#
#   bash scripts/test-db.sh start DIR PORT   # initdb (once) + start; idempotent
#   bash scripts/test-db.sh stop DIR PORT
#
# `make test-db` / `make test-db-stop` call it with DIR=.devdata/testpg and
# PORT=5433, which is internal/testdb's default URL:
#   postgres://pages@127.0.0.1:5433/postgres?sslmode=disable
# Trust auth for the role `pages`, loopback TCP plus a socket in DIR, fsync
# off: test data only. As root the cluster belongs to the postgres account
# (Postgres refuses to run as root) via runuser; as a normal user it is yours.
set -euo pipefail

cmd="${1:-}" dir="${2:-}" port="${3:-5433}"
usage() { echo "usage: $0 start|stop DIR [PORT]" >&2; exit 2; }
[[ -n "$cmd" && -n "$dir" ]] || usage
[[ "$port" =~ ^[0-9]+$ ]] || usage
mkdir -p "$dir"
dir="$(cd "$dir" && pwd)"
data="$dir/data"

# Prefer a PostgreSQL 16 install; fall back to whatever pg_ctl is on PATH.
bindir=""
for candidate in /usr/lib/postgresql/16/bin /usr/pgsql-16/bin /opt/homebrew/opt/postgresql@16/bin /usr/local/opt/postgresql@16/bin; do
  if [[ -x "$candidate/pg_ctl" ]]; then bindir="$candidate"; break; fi
done
if [[ -z "$bindir" ]]; then
  command -v pg_ctl >/dev/null 2>&1 || { echo "PostgreSQL server tools not found (install postgresql16-server / postgresql-16)" >&2; exit 1; }
  bindir="$(dirname "$(command -v pg_ctl)")"
  echo "! no PostgreSQL 16 found; using $("$bindir/pg_ctl" --version)" >&2
fi

as_pg() {
  if [[ $EUID -eq 0 ]]; then runuser -u postgres -- "$@"; else "$@"; fi
}

running() { as_pg "$bindir/pg_ctl" -D "$data" status >/dev/null 2>&1; }

case "$cmd" in
  start)
    if [[ $EUID -eq 0 ]]; then
      id -u postgres >/dev/null 2>&1 || { echo "running as root needs a postgres account" >&2; exit 1; }
      chown postgres:postgres "$dir"
      runuser -u postgres -- test -x "$dir" || {
        echo "the postgres account cannot reach $dir (a parent directory is private); use e.g. TEST_PG_DIR=/var/tmp/pages-testpg" >&2
        exit 1
      }
    fi
    if running; then
      echo "test database already running on 127.0.0.1:$port ($data)"
      exit 0
    fi
    if [[ ! -s "$data/PG_VERSION" ]]; then
      as_pg "$bindir/initdb" -D "$data" -A trust -U pages --encoding=UTF8 --no-locale >/dev/null
    fi
    as_pg "$bindir/pg_ctl" -D "$data" -l "$dir/postgres.log" -w -t 60 \
      -o "-p $port -k $dir -c listen_addresses=127.0.0.1 -c max_connections=400 -c fsync=off -c synchronous_commit=off -c full_page_writes=off" \
      start >/dev/null
    echo "test database on postgres://pages@127.0.0.1:$port/postgres?sslmode=disable"
    ;;
  stop)
    if [[ -s "$data/PG_VERSION" ]] && running; then
      as_pg "$bindir/pg_ctl" -D "$data" -m fast -w stop >/dev/null
      echo "test database stopped"
    else
      echo "test database not running"
    fi
    ;;
  *) usage ;;
esac
