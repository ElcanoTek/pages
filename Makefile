# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
.PHONY: all build compile test test-race test-db test-db-stop lint stack-check flag-status fmt tidy vet ci clean help

GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ElcanoTek/pages/internal/version.Version=$(VERSION)

# Where `make build` writes the binary. `pages update` builds each release
# straight into its release directory: make build BIN=<release>/pages.
BIN ?= pages

# The throwaway Postgres `make test-db` starts and the Go tests use
# (internal/testdb reads PAGES_TEST_DATABASE_URL; this is its default).
TEST_PG_DIR ?= $(CURDIR)/.devdata/testpg
TEST_PG_PORT ?= 5433

all: build

help:
	@echo "make build      compile-check ./... and emit ./pages (BIN=path to put it elsewhere)"
	@echo "make test       go test ./... (database tests skip without Postgres; see test-db)"
	@echo "make test-race  the same, with -race"
	@echo "make test-db    start a throwaway Postgres on :$(TEST_PG_PORT) for the database tests"
	@echo "make lint       golangci-lint run"
	@echo "make stack-check  the Elcano frontend standard: AGENTS.md block, flag.lock, no CDNs, npm"
	@echo "make flag-status  is the vendored Flag behind a flag checkout? (FLAG_DIR, default ../flag)"
	@echo "make ci         stack-check + vet + lint + test-race, as CI runs them"

build: compile
	go build -trimpath -ldflags '$(LDFLAGS)' -o '$(BIN)' ./cmd/pages

compile:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-db:
	@bash scripts/test-db.sh start "$(TEST_PG_DIR)" "$(TEST_PG_PORT)"

test-db-stop:
	@bash scripts/test-db.sh stop "$(TEST_PG_DIR)" "$(TEST_PG_PORT)"

lint:
	golangci-lint run ./...

# Offline: no flag checkout needed. CI runs it first.
stack-check:
	@bash scripts/stack-check.sh

flag-status:
	@bash scripts/flag-sync.sh status $(FLAG_DIR)

fmt:
	gofmt -w .

tidy:
	go mod tidy

ci: stack-check vet lint test-race

clean:
	rm -f pages
