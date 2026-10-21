# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
.PHONY: all build compile test test-race test-db test-db-stop lint fmt tidy vet ci clean help

GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ElcanoTek/pages/internal/version.Version=$(VERSION)

# The throwaway Postgres `make test-db` starts and the Go tests use
# (internal/testdb reads PAGES_TEST_DATABASE_URL; this is its default).
TEST_PG_DIR ?= $(CURDIR)/.devdata/testpg
TEST_PG_PORT ?= 5433

all: build

help:
	@echo "make build      compile-check ./... and emit ./pages"
	@echo "make test       go test ./... (database tests skip without Postgres; see test-db)"
	@echo "make test-race  the same, with -race"
	@echo "make test-db    start a throwaway Postgres on :$(TEST_PG_PORT) for the database tests"
	@echo "make lint       golangci-lint run"
	@echo "make ci         vet + lint + test-race, as CI runs them"

build: compile
	go build -trimpath -ldflags '$(LDFLAGS)' -o pages ./cmd/pages

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

fmt:
	gofmt -w .

tidy:
	go mod tidy

ci: vet lint test-race

clean:
	rm -f pages
