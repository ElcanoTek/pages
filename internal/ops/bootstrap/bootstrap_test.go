// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

type box struct {
	dir      string
	settings install.Settings
	env      map[string]string
	fake     *runner.Fake
	out      bytes.Buffer
	roles    map[string]bool
	stdin    []string // SQL sent to psql
	chowns   []string // "<path> recursive=<bool>" per Chown
}

var pubkey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

func newBox(t *testing.T) *box {
	t.Helper()
	dir := t.TempDir()
	b := &box{dir: dir, roles: map[string]bool{}}
	src := filepath.Join(dir, "src")
	for _, p := range []string{filepath.Join(src, ".git"), filepath.Join(src, "deploy"), filepath.Join(dir, "etc/ssh"), filepath.Join(dir, "var/lib/pgsql/data")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module github.com/ElcanoTek/pages\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "deploy/motd"), []byte("Northwind banner\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "self"), []byte("pages binary"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "var/lib/pgsql/data/PG_VERSION"), []byte("16\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "var/lib/pgsql/data/pg_hba.conf"), []byte("local all all peer\nhost    all             all             127.0.0.1/32            ident\nhost    all             all             ::1/128                 ident\n"), 0o600)
	b.settings = install.Settings{
		InstallConfig: filepath.Join(dir, "etc/default/pages-install"), AppDir: filepath.Join(dir, "opt/pages"), AppUser: "contoso",
		SrcDir: src, EnvFile: filepath.Join(dir, "etc/default/pages"), CLITarget: filepath.Join(dir, "usr/local/bin/pages"), Port: "4312",
	}
	b.env = map[string]string{
		"PAGES_BOOTSTRAP_NON_INTERACTIVE": "1", "AUTH_SIGNING_PUBKEY": pubkey,
		"PAGES_BOOTSTRAP_DASHBOARD_HOST": "pages.contoso.example", "PAGES_BOOTSTRAP_CONTENT_HOST": "contoso-pages.example",
		"PAGES_BOOTSTRAP_SETUP_CADDY": "Y", "PAGES_BOOTSTRAP_USE_LETSENCRYPT": "n", "PAGES_SET_HOSTNAME": "0",
	}
	b.fake = &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		args := strings.Join(c.Args, " ")
		switch c.Name {
		case "id":
			return "", 1 // no service account yet
		case "git":
			return "abcdef0\n", 0
		case "runuser":
			if strings.Contains(args, "psql") && c.Stdin != nil {
				sql, _ := io.ReadAll(c.Stdin)
				b.stdin = append(b.stdin, string(sql))
				switch {
				case strings.Contains(string(sql), "pg_roles"):
					if b.roles["pages"] {
						return "1\n", 0
					}
					return "", 0
				case strings.Contains(string(sql), "CREATE ROLE"):
					b.roles["pages"] = true
				case strings.Contains(string(sql), "pg_database"):
					return "", 0
				}
			}
			if strings.Contains(args, "token add initial") {
				return "created token #1  label=initial\n\n  pgs_NorthwindFixtureToken0123456789\n", 0
			}
		case "systemctl":
			if strings.Contains(args, "is-active") {
				return "", 3
			}
		case "rpm":
			return "", 1 // no Cockpit
		}
		return "", 0
	}, Missing: map[string]bool{"sshd": true, "hostnamectl": true}}
	return b
}

func (b *box) run(t *testing.T) error {
	t.Helper()
	b.out.Reset()
	return Run(context.Background(), Options{
		Settings: b.settings, SrcDir: b.settings.SrcDir, Runner: b.fake, Log: term.Buffer(&b.out),
		Getenv: func(k string) string { return b.env[k] }, Self: filepath.Join(b.dir, "self"), Root: b.dir,
		Probe: func(context.Context, string) bool { return true }, Sleep: func(time.Duration) {},
		Migrate:      func(context.Context, string) error { return nil },
		ActiveTokens: func(context.Context, string) int { return map[bool]int{false: 0, true: 1}[b.roles["token"]] },
		Chown: func(_ context.Context, p string, recursive bool) error {
			b.chowns = append(b.chowns, fmt.Sprintf("%s recursive=%v", p, recursive))
			return nil
		},
	})
}

func TestBootstrapInstallsAndReruns(t *testing.T) {
	b := newBox(t)
	if err := b.run(t); err != nil {
		t.Fatalf("%v\n%s", err, b.out.String())
	}
	out := b.out.String()
	for _, want := range []string{"minted initial agent API token", "pgs_NorthwindFixtureToken0123456789", "/readyz → 200", "Caddy running for both hostnames"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	env, err := envfile.Read(b.settings.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(b.settings.EnvFile); st.Mode().Perm() != 0o640 {
		t.Fatal(st.Mode())
	}
	if env.Value("PORT") != "4312" || env.Value("DASHBOARD_ORIGIN") != "https://pages.contoso.example" || env.Value("CONTENT_HOST") != "contoso-pages.example" ||
		env.Value("AUTH_SIGNING_PUBKEY") != pubkey || len(env.Value("RAW_TOKEN_SECRET")) != 64 || env.Value("AUTH_COOKIE_NAME") != "elcano_auth" {
		t.Fatalf("%v", env.Map())
	}
	url := env.Value("DATABASE_URL")
	pass := strings.TrimSuffix(strings.TrimPrefix(url, "postgres://pages:"), "@127.0.0.1:5432/pages?sslmode=disable")
	if len(pass) != 48 {
		t.Fatal(url)
	}
	// The role password travels on psql's stdin, never in argv.
	if !strings.Contains(strings.Join(b.stdin, "\n"), "CREATE ROLE pages WITH LOGIN PASSWORD '"+pass+"'") || strings.Contains(b.fake.Log(), pass) {
		t.Fatalf("password handling:\n%s", b.fake.Log())
	}
	// The release layout, CLI link, unit, banner and proxy.
	if target, _ := os.Readlink(b.settings.CLITarget); target != filepath.Join(b.settings.AppDir, "pages") {
		t.Fatal(target)
	}
	if got, _ := os.ReadFile(filepath.Join(b.settings.AppDir, "pages")); string(got) != "pages binary" {
		t.Fatal("binary not installed as the release")
	}
	if dest, _ := os.Readlink(filepath.Join(b.settings.AppDir, "assets")); dest != b.settings.SharedAssets() {
		t.Fatal(dest)
	}
	// The release stays root's: root runs its binary through the CLI link.
	// Only the shared assets (and the release's link to them) go to the
	// service account.
	release, _ := filepath.EvalSymlinks(b.settings.AppDir)
	if _, err := install.TrustedFile(filepath.Join(release, "pages")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(release, "pages")); st.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode %v", st.Mode())
	}
	for _, c := range b.chowns {
		if strings.HasPrefix(c, release) && c != filepath.Join(release, "assets")+" recursive=false" {
			t.Fatalf("release handed to the service account: %v", b.chowns)
		}
	}
	unit, _ := os.ReadFile(filepath.Join(b.dir, "etc/systemd/system/pages.service"))
	if !strings.Contains(string(unit), "ExecStart="+b.settings.AppDir+"/pages serve") || !strings.Contains(string(unit), "User=contoso") {
		t.Fatal(string(unit))
	}
	if motd, _ := os.ReadFile(filepath.Join(b.dir, "etc/motd")); string(motd) != "Northwind banner\n" {
		t.Fatal(string(motd))
	}
	site, _ := os.ReadFile(filepath.Join(b.dir, "etc/caddy/conf.d/pages.caddy"))
	if strings.Count(string(site), "\ttls internal\n") != 2 || !strings.Contains(string(site), "pages.contoso.example {") {
		t.Fatal(string(site))
	}
	caddyfile, _ := os.ReadFile(filepath.Join(b.dir, "etc/caddy/Caddyfile"))
	if !strings.Contains(string(caddyfile), "import conf.d/*.caddy") {
		t.Fatal(string(caddyfile))
	}
	hba, _ := os.ReadFile(filepath.Join(b.dir, "var/lib/pgsql/data/pg_hba.conf"))
	if strings.Count(string(hba), "scram-sha-256") != 2 || !strings.Contains(string(hba), "local all all peer") {
		t.Fatal(string(hba))
	}
	if cfg, _ := os.ReadFile(b.settings.InstallConfig); !strings.Contains(string(cfg), `PAGES_APP_USER="${PAGES_APP_USER:-contoso}"`) {
		t.Fatal(string(cfg))
	}
	for _, want := range []string{"useradd --system --home-dir " + b.settings.AppDir, "systemctl enable --now postgresql", "systemctl restart pages.service", "dnf install -y caddy"} {
		if !strings.Contains(b.fake.Log(), want) {
			t.Fatalf("missing %q:\n%s", want, b.fake.Log())
		}
	}

	// A re-run keeps the secrets, the role password and the release, and
	// mints no second token.
	b.roles["token"] = true
	b.stdin = nil
	secret := env.Value("RAW_TOKEN_SECRET")
	if err := b.run(t); err != nil {
		t.Fatalf("%v\n%s", err, b.out.String())
	}
	again, _ := envfile.Read(b.settings.EnvFile)
	if again.Value("RAW_TOKEN_SECRET") != secret || again.Value("DATABASE_URL") != url {
		t.Fatal("re-run rotated configuration")
	}
	if !strings.Contains(strings.Join(b.stdin, "\n"), "ALTER ROLE pages WITH LOGIN PASSWORD '"+pass+"'") {
		t.Fatal(b.stdin)
	}
	if !strings.Contains(b.out.String(), "preserving active release") || strings.Contains(b.out.String(), "minted initial") {
		t.Fatal(b.out.String())
	}
	if _, err := os.Stat(b.settings.EnvFile + ".previous"); err != nil {
		t.Fatal("no recoverable previous env file")
	}
}

func TestBootstrapStopsOnBadInputs(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"missing pubkey":    {map[string]string{"AUTH_SIGNING_PUBKEY": ""}, "non-interactive + missing: set AUTH_SIGNING_PUBKEY"},
		"bad pubkey":        {map[string]string{"AUTH_SIGNING_PUBKEY": pubkey + "!"}, "must be a base64-encoded 32-byte Ed25519 public key"},
		"le email required": {map[string]string{"PAGES_BOOTSTRAP_USE_LETSENCRYPT": "Y"}, "non-interactive + missing: set PAGES_BOOTSTRAP_LE_EMAIL"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			for k, v := range tc.env {
				b.env[k] = v
			}
			err := b.run(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
		})
	}
	// Same-site hostnames are a hard stop before the environment file is
	// written or the service is started.
	b := newBox(t)
	b.env["PAGES_BOOTSTRAP_CONTENT_HOST"] = "cdn.pages.contoso.example"
	b.env["PAGES_BOOTSTRAP_SETUP_CADDY"] = "n"
	if err := b.run(t); err == nil || !strings.Contains(err.Error(), "shares the registrable domain contoso.example") {
		t.Fatalf("same-site hostnames: %v\n%s", err, b.out.String())
	}
	if _, err := os.Stat(b.settings.EnvFile); !os.IsNotExist(err) {
		t.Fatalf("environment file written for same-site hostnames: %v", err)
	}
	if strings.Contains(b.fake.Log(), "systemctl restart pages.service") || strings.Contains(b.fake.Log(), "systemctl enable") {
		t.Fatalf("service activated for same-site hostnames:\n%s", b.fake.Log())
	}
	// An existing URL whose password cannot be reused stops before the role is touched.
	b = newBox(t)
	_ = os.MkdirAll(filepath.Dir(b.settings.EnvFile), 0o755)
	_ = os.WriteFile(b.settings.EnvFile, []byte("DATABASE_URL=\"postgres://pages:p%40ss@db/pages\"\n"), 0o640)
	if err := b.run(t); err == nil || !strings.Contains(err.Error(), "database role left unchanged") {
		t.Fatal(err)
	}
	for _, sql := range b.stdin {
		if strings.Contains(sql, "ROLE pages WITH") {
			t.Fatal("role touched")
		}
	}
}

// A failing systemctl/chown on the activation path stops bootstrap before the
// service is (re)started on a half-applied configuration.
func TestBootstrapStopsOnServiceManagerFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd, want string
	}{
		"daemon-reload": {"systemctl daemon-reload", "systemctl daemon-reload failed"},
		"enable":        {"systemctl enable pages.service", "systemctl enable pages.service failed"},
		"env chown":     {"chown root:contoso", "could not give"},
		"pg reload":     {"systemctl reload postgresql", "pg_hba.conf changes not applied"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			handle := b.fake.Handle
			b.fake.Handle = func(c runner.Cmd) (string, int) {
				if strings.HasPrefix(c.String(), tc.cmd) {
					return "", 1
				}
				return handle(c)
			}
			err := b.run(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v, want %q\n%s", err, tc.want, b.out.String())
			}
			if strings.Contains(b.fake.Log(), "systemctl restart pages.service") {
				t.Fatalf("pages.service restarted after %s failed:\n%s", tc.cmd, b.fake.Log())
			}
		})
	}
}

// A rerun over an active release another account could have replaced stops:
// root runs that binary, so bootstrap neither keeps nor starts it.
func TestBootstrapRejectsUntrustedActiveRelease(t *testing.T) {
	b := newBox(t)
	if err := b.run(t); err != nil {
		t.Fatalf("%v\n%s", err, b.out.String())
	}
	bin, err := filepath.EvalSymlinks(filepath.Join(b.settings.AppDir, install.BinaryName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	before := len(b.fake.Log())
	err = b.run(t)
	if err == nil || !strings.Contains(err.Error(), "can be changed by an account other than root") || !strings.Contains(err.Error(), "scripts/bootstrap.sh") {
		t.Fatalf("untrusted active release: %v\n%s", err, b.out.String())
	}
	if rerun := b.fake.Log()[before:]; strings.Contains(rerun, "systemctl restart pages.service") {
		t.Fatalf("pages.service restarted on an untrusted release:\n%s", rerun)
	}
}
