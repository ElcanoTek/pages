// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package hostname names a box after its service. Vultr images boot as
// "vultr.guest", so every prompt read root@vultr. Only an unset cloud-image
// default (exactly vultr.guest, localhost or localhost.localdomain) is
// renamed; any other name is a deliberate one and is left alone with a hint.
// PAGES_SET_HOSTNAME=1 forces the rename, PAGES_SET_HOSTNAME=0 skips the
// step. The name is pinned against cloud-init and the loopback lines in
// /etc/hosts point at it. Idempotent; never fails the install.
package hostname

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

// Setter renames the box.
type Setter struct {
	// Root prefixes /etc paths (tests only).
	Root   string
	Runner runner.Runner
	Log    *term.Printer
	Getenv func(string) string
}

var (
	dottedName = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
	ipLike     = regexp.MustCompile(`^[0-9.]+$`)
)

// CloudConfig is what pins the hostname against cloud-init.
const CloudConfig = "# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.\npreserve_hostname: true\nmanage_etc_hosts: false\n"

// Set names the box fqdn.
func (s Setter) Set(ctx context.Context, fqdn string) {
	getenv := s.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	force := getenv("PAGES_SET_HOSTNAME")
	if force == "0" || !runner.Have(s.Runner, "hostnamectl") {
		return
	}
	// A dotted DNS name, not an IP address.
	if !dottedName.MatchString(fqdn) || ipLike.MatchString(fqdn) {
		return
	}
	current, _ := runner.Output(ctx, s.Runner, runner.Cmd{Name: "hostnamectl", Args: []string{"--static"}})
	if current == fqdn {
		return
	}
	if force != "1" {
		switch current {
		case "vultr.guest", "localhost", "localhost.localdomain":
		default:
			shown := current
			if shown == "" {
				shown = "unset"
			}
			s.Log.Info("hostname is " + shown + "; to name the box after the service: hostnamectl set-hostname " + fqdn)
			return
		}
	}
	if !runner.OK(ctx, s.Runner, runner.Cmd{Name: "hostnamectl", Args: []string{"set-hostname", fqdn}}) {
		s.Log.Warn("could not set the hostname to " + fqdn)
		return
	}
	was := current
	if was == "" {
		was = "unset"
	}
	s.Log.OK("hostname set to " + fqdn + " (was " + was + ")")
	if st, err := os.Stat(filepath.Join(s.Root, "etc/cloud")); err == nil && st.IsDir() {
		dir := filepath.Join(s.Root, "etc/cloud/cloud.cfg.d")
		file := filepath.Join(dir, "99-preserve-hostname.cfg")
		if err := os.MkdirAll(dir, 0o755); err != nil || os.WriteFile(file, []byte(CloudConfig), 0o644) != nil {
			s.Log.Warn("could not write /etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg")
		} else {
			_ = os.Chmod(file, 0o644)
		}
	}
	hosts := filepath.Join(s.Root, "etc/hosts")
	b, err := os.ReadFile(hosts)
	if err != nil {
		if !os.IsNotExist(err) {
			s.Log.Warn("could not update /etc/hosts")
		}
		return
	}
	next := RewriteHosts(string(b), current, fqdn)
	if next != string(b) {
		// Rewrite in place, not rename: keeps the file's owner, mode and
		// SELinux label.
		if err := os.WriteFile(hosts, []byte(next), 0o644); err != nil {
			s.Log.Warn("could not update /etc/hosts")
		}
	}
}

// RewriteHosts points the loopback lines of an /etc/hosts at fqdn: on
// 127.0.0.1/::1 lines the old hostname (and its short form) is replaced by
// "<fqdn> <short>", every other alias is kept, and a localhost name is never
// replaced. When no line then names fqdn, loopback lines for it are appended.
func RewriteHosts(text, old, fqdn string) string {
	short := strings.SplitN(fqdn, ".", 2)[0]
	oldShort := strings.SplitN(old, ".", 2)[0]
	if strings.HasPrefix(old, "localhost") {
		old = ""
	}
	if strings.HasPrefix(oldShort, "localhost") {
		oldShort = ""
	}
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for _, raw := range lines {
		if raw == "" {
			continue
		}
		line := strings.TrimSuffix(raw, "\n")
		f := strings.Fields(line)
		if old != "" && len(f) > 0 && (f[0] == "127.0.0.1" || f[0] == "::1") {
			out, hit := f[0], false
			for i := 1; i < len(f); i++ {
				if strings.HasPrefix(f[i], "#") {
					out += " " + strings.Join(f[i:], " ")
					break
				}
				if f[i] == old || f[i] == oldShort {
					if !hit {
						out += " " + fqdn + " " + short
					}
					hit = true
					continue
				}
				if f[i] == fqdn || f[i] == short {
					continue
				}
				out += " " + f[i]
			}
			if hit {
				b.WriteString(out + "\n")
				continue
			}
		}
		b.WriteString(raw)
		if !strings.HasSuffix(raw, "\n") {
			b.WriteString("\n")
		}
	}
	result := b.String()
	found := false
	for _, line := range strings.Split(result, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		for _, x := range f[1:] {
			if x == fqdn {
				found = true
			}
		}
	}
	if !found {
		result += "127.0.0.1 " + fqdn + " " + short + "\n::1 " + fqdn + " " + short + "\n"
	}
	return result
}
