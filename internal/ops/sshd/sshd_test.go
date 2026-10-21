// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package sshd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

type sshFixture struct {
	root, conf string
	stub       map[string]string
}

func newFixture(t *testing.T) *sshFixture {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc/ssh/sshd_config.d", "root/.ssh", "home/contoso"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &sshFixture{root: root, conf: filepath.Join(root, DropInPath), stub: map[string]string{}}
}

func (f *sshFixture) addKey(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, "etc/ssh/keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "etc/ssh/keys/contoso"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goodKeys = "# ssh-ed25519 AAAAold retired\nrestrict,from=\"10.0.0.0/8\" ssh-ed25519 AAAAC3Nza contoso@northwind\n"

// run applies the safety net with stubbed sshd/systemctl, returning the
// printed lines and the commands that ran.
func (f *sshFixture) run(t *testing.T, env map[string]string) (out, calls string) {
	t.Helper()
	get := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return f.stub[k]
	}
	fake := &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		switch c.Name {
		case "sshd":
			switch c.Args[0] {
			case "-t":
				if get("T_RC") == "1" {
					return "", 1
				}
			case "-T":
				pa, pk := get("PA"), get("PUBKEY")
				if pa == "" {
					pa = "no"
				}
				if pk == "" {
					pk = "yes"
				}
				return "authorizedkeysfile .ssh/authorized_keys /etc/ssh/keys/%u\npasswordauthentication " + pa + "\npubkeyauthentication " + pk + "\n", 0
			}
		case "systemctl":
			if get("RELOAD_RC") == "1" {
				return "", 1
			}
		}
		return "", 0
	}}
	var b bytes.Buffer
	log := term.Buffer(&b)
	KeyOnly{Root: f.root, Runner: fake, Log: log, Getenv: func(k string) string { return env[k] }}.Apply(context.Background())
	var cl []string
	for _, c := range fake.Calls() {
		if c.Name == "sshd" && c.Args[0] == "-T" {
			continue // reading the effective config is always allowed
		}
		cl = append(cl, c.String())
	}
	return b.String(), strings.Join(cl, "\n")
}

func TestNoAuthorizedKeyWarnsAndLeavesSshdAlone(t *testing.T) {
	f := newFixture(t)
	out, calls := f.run(t, nil)
	if !strings.Contains(out, "! no SSH authorized keys found") || calls != "" {
		t.Fatal(out, calls)
	}
	if _, err := os.Stat(f.conf); err == nil {
		t.Fatal("drop-in written without a key")
	}
}

func TestWithAKeyWritesValidatesReloadsThenIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	out, calls := f.run(t, nil)
	if !strings.Contains(out, "✓ SSH is key-only") || !strings.Contains(calls, "sshd -t") || !strings.Contains(calls, "systemctl reload sshd") {
		t.Fatal(out, calls)
	}
	body, _ := os.ReadFile(f.conf)
	for _, want := range []string{"PasswordAuthentication no\n", "KbdInteractiveAuthentication no\n", "PermitRootLogin prohibit-password\n"} {
		if !strings.Contains(string(body), want) {
			t.Fatal(string(body))
		}
	}
	if st, _ := os.Stat(f.conf); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	out, calls = f.run(t, nil)
	if out != "" || calls != "" {
		t.Fatalf("rerun acted: %q %q", out, calls)
	}
	// Identical file: no write or reload, but a winning earlier directive is reported.
	out, calls = f.run(t, map[string]string{"PA": "yes"})
	if !strings.Contains(out, "! sshd still accepts passwords") || calls != "" {
		t.Fatal(out, calls)
	}
}

func TestFailedValidationRestoresThePreviousFile(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	if err := os.WriteFile(f.conf, []byte("PasswordAuthentication no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, calls := f.run(t, map[string]string{"T_RC": "1"})
	if !strings.Contains(out, "! sshd -t rejected") || strings.Contains(calls, "systemctl") {
		t.Fatal(out, calls)
	}
	if b, _ := os.ReadFile(f.conf); string(b) != "PasswordAuthentication no\n" {
		t.Fatal(string(b))
	}
	// Without a previous file, ours is removed again.
	_ = os.Remove(f.conf)
	f.run(t, map[string]string{"T_RC": "1"})
	if _, err := os.Stat(f.conf); err == nil {
		t.Fatal("rejected drop-in left in place")
	}
}

func TestCommentedKeyDoesNotCount(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, "# ssh-ed25519 AAAAC3Nza contoso@northwind\n")
	if out, _ := f.run(t, nil); !strings.Contains(out, "no SSH authorized keys found") {
		t.Fatal(out)
	}
}

func TestPubkeyOffLeavesSshdAlone(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	out, calls := f.run(t, map[string]string{"PUBKEY": "no"})
	if !strings.Contains(out, "! sshd has PubkeyAuthentication no") || calls != "" {
		t.Fatal(out, calls)
	}
}

func TestEarlierDirectiveIsReported(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	if out, _ := f.run(t, map[string]string{"PA": "yes"}); !strings.Contains(out, "sshd still accepts passwords: an earlier directive wins") {
		t.Fatal(out)
	}
}

func TestReloadFailureWarns(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	if out, _ := f.run(t, map[string]string{"RELOAD_RC": "1"}); !strings.Contains(out, "could not reload sshd") {
		t.Fatal(out)
	}
}

func TestKeepPasswordsSkipsAndNoDropInDirIsSilent(t *testing.T) {
	f := newFixture(t)
	f.addKey(t, goodKeys)
	if out, _ := f.run(t, map[string]string{"PAGES_KEEP_SSH_PASSWORDS": "1"}); !strings.Contains(out, "» PAGES_KEEP_SSH_PASSWORDS=1") {
		t.Fatal(out)
	}
	if _, err := os.Stat(f.conf); err == nil {
		t.Fatal("written despite PAGES_KEEP_SSH_PASSWORDS")
	}
	_ = os.RemoveAll(filepath.Join(f.root, "etc/ssh/sshd_config.d"))
	if out, calls := f.run(t, nil); out != "" || calls != "" {
		t.Fatal(out, calls)
	}
}
