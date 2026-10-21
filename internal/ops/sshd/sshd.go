// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package sshd is bootstrap's key-only SSH safety net and doctor's read-only
// password-login probe.
//
// Vultr's cloud-init writes /etc/ssh/sshd_config.d/50-cloud-init.conf with
// `PasswordAuthentication yes`, so a fresh box takes root password guesses all
// day. The Vultr startup script "elcano-key-only-ssh" fixes that at first boot;
// this is the net for when nobody selected it. Same drop-in, same name.
package sshd

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

// DropIn is the drop-in's content.
const DropIn = `# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's
# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
`

// DropInPath is the drop-in's path relative to the filesystem root.
const DropInPath = "etc/ssh/sshd_config.d/01-no-password-login.conf"

var (
	acceptsPasswords = regexp.MustCompile(`(?im)^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin) yes$`)
	pubkeyOff        = regexp.MustCompile(`(?im)^pubkeyauthentication no$`)
	authKeysFile     = regexp.MustCompile(`(?im)^authorizedkeysfile (.*)$`)
	keyLine          = regexp.MustCompile(`(^|\s)(ssh-|ecdsa-|sk-)\S+\s+AAAA`)
)

// AcceptsPasswords reports whether `sshd -T` output still lets passwords in.
func AcceptsPasswords(effective string) bool { return acceptsPasswords.MatchString(effective) }

// KeyOnly applies the safety net.
type KeyOnly struct {
	// Root prefixes every path (tests only; "" in production).
	Root   string
	Runner runner.Runner
	Log    *term.Printer
	// Getenv reads PAGES_KEEP_SSH_PASSWORDS (os.Getenv when nil).
	Getenv func(string) string
}

func (k KeyOnly) effective(ctx context.Context) string {
	out, _ := runner.Output(ctx, k.Runner, runner.Cmd{Name: "sshd", Args: []string{"-T"}})
	return out
}

// CountAuthorizedKeys is how many public keys sshd would accept for root and
// /home/* users, honouring the effective AuthorizedKeysFile from `sshd -T`.
// It is the lockout guard.
func (k KeyOnly) CountAuthorizedKeys(ctx context.Context) int {
	var files []string
	if m := authKeysFile.FindStringSubmatch(k.effective(ctx)); m != nil {
		files = strings.Fields(m[1])
	}
	if len(files) == 0 {
		files = []string{".ssh/authorized_keys", ".ssh/authorized_keys2"}
	}
	homes := []string{filepath.Join(k.Root, "root")}
	if more, err := filepath.Glob(filepath.Join(k.Root, "home", "*")); err == nil {
		homes = append(homes, more...)
	}
	count := 0
	for _, home := range homes {
		if st, err := os.Stat(home); err != nil || !st.IsDir() {
			continue
		}
		rel := strings.TrimPrefix(home, k.Root)
		for _, f := range files {
			if f == "none" {
				continue
			}
			p := strings.ReplaceAll(f, "%h", rel)
			p = strings.ReplaceAll(p, "%u", filepath.Base(rel))
			if !strings.HasPrefix(p, "/") {
				p = rel + "/" + p
			}
			b, err := os.ReadFile(k.Root + p)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
					continue
				}
				// A key line may start with options (restrict, from=, command=...).
				if keyLine.MatchString(line) {
					count++
				}
			}
		}
	}
	return count
}

// Apply installs the drop-in (passwords off, root by key only), checks it
// with `sshd -t` and reloads sshd. PAGES_KEEP_SSH_PASSWORDS=1 skips it. It is
// silent without sshd or sshd_config.d, a no-op when the file already has
// this content, and skips with a warning when no authorized key is installed
// or pubkey authentication is off (either would lock everyone out). It never
// fails the caller.
func (k KeyOnly) Apply(ctx context.Context) {
	getenv := k.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("PAGES_KEEP_SSH_PASSWORDS") == "1" {
		k.Log.Info("PAGES_KEEP_SSH_PASSWORDS=1: leaving sshd's password settings alone")
		return
	}
	dir := filepath.Join(k.Root, "etc/ssh/sshd_config.d")
	conf := filepath.Join(k.Root, DropInPath)
	if !runner.Have(k.Runner, "sshd") {
		return
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return
	}
	old, oldErr := os.ReadFile(conf)
	if oldErr == nil && string(old) == DropIn {
		k.effectiveCheck(ctx)
		return
	}
	if pubkeyOff.MatchString(k.effective(ctx)) {
		k.Log.Warn("sshd has PubkeyAuthentication no: leaving password login on so nobody is locked out")
		return
	}
	if k.CountAuthorizedKeys(ctx) < 1 {
		k.Log.Warn("no SSH authorized keys found — leaving password login on so nobody is locked out; add a key, then re-run bootstrap")
		return
	}
	if err := install.WriteFileAtomic(conf, []byte(DropIn), 0o600); err != nil {
		k.Log.Warn("could not write " + conf + " — SSH still accepts passwords")
		return
	}
	if !runner.OK(ctx, k.Runner, runner.Cmd{Name: "sshd", Args: []string{"-t"}}) {
		// Put a differing hand-made copy back; otherwise remove ours.
		if oldErr == nil {
			_ = install.WriteFileAtomic(conf, old, 0o600)
		} else {
			_ = os.Remove(conf)
		}
		k.Log.Warn("sshd -t rejected the key-only drop-in — reverted " + conf + "; sshd left as it was")
		return
	}
	if runner.OK(ctx, k.Runner, runner.Cmd{Name: "systemctl", Args: []string{"reload", "sshd"}}) ||
		runner.OK(ctx, k.Runner, runner.Cmd{Name: "systemctl", Args: []string{"reload", "ssh"}}) {
		k.Log.OK("SSH is key-only: passwords off, root by key (" + conf + ")")
	} else {
		k.Log.Warn("wrote " + conf + " but could not reload sshd — run: systemctl reload sshd")
	}
	k.effectiveCheck(ctx)
}

// effectiveCheck warns when the effective config still accepts passwords
// although the drop-in is in place (an earlier directive wins).
func (k KeyOnly) effectiveCheck(ctx context.Context) {
	if AcceptsPasswords(k.effective(ctx)) {
		k.Log.Warn("sshd still accepts passwords: an earlier directive wins (e.g. 'PermitRootLogin yes' above the Include line in /etc/ssh/sshd_config) — fix it by hand")
	}
}
