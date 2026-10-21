// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package devenv

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

func TestSecretsAreStable(t *testing.T) {
	dev := filepath.Join(t.TempDir(), ".devdata")
	a, err := Secrets(dev)
	if err != nil || len(a["API_TOKEN_PEPPER"]) != 64 || len(a) != 3 {
		t.Fatal(a, err)
	}
	b, _ := Secrets(dev)
	if a["API_TOKEN_PEPPER"] != b["API_TOKEN_PEPPER"] {
		t.Fatal("the pepper must never change between runs")
	}
	if st, _ := os.Stat(filepath.Join(dev, "dev.env")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
}

func TestAuthCookieVerifiesWithThePublishedKey(t *testing.T) {
	dev := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	pub, cookie, err := Auth(dev, "dev@elcanotek.com", now)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(pub)
	body, sig, _ := strings.Cut(cookie, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(sig)
	if len(key) != 32 || !ed25519.Verify(key, []byte(body), raw) {
		t.Fatal("cookie does not verify against AUTH_SIGNING_PUBKEY")
	}
	claims, _ := base64.RawURLEncoding.DecodeString(body)
	if string(claims) != fmt.Sprintf(`{"email":"dev@elcanotek.com","tenant":"","iat":%d,"exp":%d}`, now.Unix(), now.Unix()+30*86400) {
		t.Fatal(string(claims))
	}
	// The keypair persists: a second run publishes the same key.
	again, _, _ := Auth(dev, "dev@elcanotek.com", now)
	if again != pub {
		t.Fatal("keypair regenerated")
	}
	var m map[string]any
	if err := json.Unmarshal(claims, &m); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRunsAThrowawayCluster(t *testing.T) {
	initdb := pgTool("initdb")
	if _, err := exec.LookPath(initdb); err != nil {
		t.Skip("no initdb")
	}
	euid := os.Geteuid()
	if euid == 0 {
		if _, err := user.Lookup("postgres"); err != nil {
			t.Skip("root needs a postgres account")
		}
	}
	// Under /var/tmp: the postgres account must traverse it.
	root, err := os.MkdirTemp("/var/tmp", "pages-dev-test-")
	if err != nil {
		t.Skip(err)
	}
	_ = os.Chmod(root, 0o755)
	checkout := t.TempDir()
	self := filepath.Join(checkout, "fake-pages")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho 'created token #1'\necho '  pgs_NorthwindDevToken0123456789abc'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(56000 + os.Getpid()%1000)
	env := map[string]string{"PAGES_DEV_DIR": root, "PGPORT": port, "PORT": "3999", "CUTLASS_DIR": filepath.Join(checkout, "none")}
	var out bytes.Buffer
	o := Options{Root: checkout, Runner: runner.Exec{}, Log: term.Buffer(&out), Getenv: func(k string) string { return env[k] }, EUID: euid, Self: self}
	t.Cleanup(func() {
		stop := exec.Command(pgTool("pg_ctl"), "-D", filepath.Join(root, "pg"), "-m", "immediate", "stop")
		if euid == 0 {
			stop = exec.Command("runuser", "-u", "postgres", "--", pgTool("pg_ctl"), "-D", filepath.Join(root, "pg"), "-m", "immediate", "stop")
		}
		_ = stop.Run()
		_ = os.RemoveAll(root)
	})
	e, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if e.Vars["PAGES_DEV_LOGIN"] != "1" || e.Vars["PGHOST"] != root || e.Vars["DASHBOARD_ORIGIN"] != "http://localhost:3999" || e.Vars["AUTH_SIGNING_PUBKEY"] == "" {
		t.Fatal(e.Vars)
	}
	if tok, err := Token(o); err != nil || tok != "pgs_NorthwindDevToken0123456789abc" {
		t.Fatal(tok, err)
	}
	// Idempotent: the second run reuses cluster, database and token.
	if _, err := Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Banner(e, "pgs_x"), "http://localhost:3999/__dev/login") {
		t.Fatal(Banner(e, "pgs_x"))
	}
}
