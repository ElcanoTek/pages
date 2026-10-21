// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package install

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestServiceProxyAndRerunShareCustomSettings(t *testing.T) {
	dir := t.TempDir()
	s := Settings{AppDir: dir + "/app", AppUser: "northwind", SrcDir: dir + "/source", EnvFile: dir + "/pages.env",
		CLITarget: dir + "/bin/pages", Port: "4312", InstallConfig: dir + "/install"}
	if err := os.WriteFile(s.InstallConfig, []byte(s.RenderConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.EnvFile, []byte("PORT=\"4517\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	unit, err := s.RenderService()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"User=northwind\nGroup=northwind", "WorkingDirectory=" + s.AppDir, "EnvironmentFile=" + s.EnvFile,
		"ReadWritePaths=" + s.AppDir + "/assets", "ExecStart=" + s.AppDir + "/pages serve", "NoNewPrivileges=true", "ProtectSystem=strict"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	caddy, err := s.RenderCaddy("dashboard.test", "content.test", false)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(caddy, "reverse_proxy 127.0.0.1:4312"); n != 2 {
		t.Fatalf("%d proxies:\n%s", n, caddy)
	}
	if strings.Contains(caddy, "\ttls internal") || strings.Contains(caddy, "{{") {
		t.Fatal(caddy)
	}
	internal, _ := s.RenderCaddy("dashboard.test", "content.test", true)
	if n := strings.Count(internal, "\ttls internal\n"); n != 2 {
		t.Fatalf("%d tls internal lines:\n%s", n, internal)
	}
	if got := SiteBlockLines(internal); len(got) != 2 || got[0] != "dashboard.test {" || got[1] != "content.test {" {
		t.Fatal(got)
	}
	if _, err := s.RenderCaddy("bad host", "content.test", false); err == nil {
		t.Fatal("bad hostname accepted")
	}

	read := Load(env(map[string]string{"PAGES_INSTALL_CONFIG": s.InstallConfig}))
	if read.AppDir != s.AppDir || read.AppUser != "northwind" || read.SrcDir != s.SrcDir || read.EnvFile != s.EnvFile ||
		read.CLITarget != s.CLITarget || read.Port != "4517" || read.PortExplicit {
		t.Fatalf("%+v", read)
	}
	if p := Load(env(map[string]string{"PAGES_INSTALL_CONFIG": s.InstallConfig, "PAGES_PORT": "4618"})); p.Port != "4618" || !p.PortExplicit {
		t.Fatalf("%+v", p)
	}
	if o := Load(env(map[string]string{"PAGES_INSTALL_CONFIG": s.InstallConfig, "APP_DIR": dir + "/override"})); o.AppDir != dir+"/override" {
		t.Fatal(o.AppDir)
	}
	if o := Load(env(map[string]string{"PAGES_INSTALL_CONFIG": s.InstallConfig, "PAGES_APP_USER": "contoso"})); o.AppUser != "contoso" {
		t.Fatal("an explicit PAGES_* override must beat the persisted default")
	}
	if d := Load(env(map[string]string{"PAGES_INSTALL_CONFIG": "/nonexistent"})); d.AppDir != DefaultAppDir || d.Port != DefaultPort || d.EnvFile != DefaultEnvFile {
		t.Fatalf("%+v", d)
	}
	// The re-executed CLI carries the same installation.
	if e := strings.Join(read.Env(), "\n"); !strings.Contains(e, "PAGES_INSTALL_CONFIG="+s.InstallConfig) || !strings.Contains(e, "APP_USER=northwind") {
		t.Fatal(e)
	}
}

func TestUnsupportedPathsAndPortsFailBeforeInstalling(t *testing.T) {
	base := Settings{AppDir: "/opt/pages", AppUser: "pages", SrcDir: "/opt/pages-src", EnvFile: "/etc/default/pages",
		CLITarget: "/usr/local/bin/pages", Port: "3002", InstallConfig: "/etc/default/pages-install"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Settings){
		func(s *Settings) { s.Port = "70000" }, func(s *Settings) { s.Port = "0" }, func(s *Settings) { s.AppDir = "/tmp/my app" },
		func(s *Settings) { s.AppDir = "/opt/../etc" }, func(s *Settings) { s.AppUser = "bad user" }, func(s *Settings) { s.EnvFile = "relative" },
	} {
		s := base
		bad(&s)
		if s.Validate() == nil {
			t.Fatalf("accepted %+v", s)
		}
		if _, err := s.RenderService(); err == nil {
			t.Fatal("rendered an invalid installation")
		}
	}
	unit, _ := base.RenderService()
	if !regexp.MustCompile(`WorkingDirectory=/opt/pages\n`).MatchString(unit) {
		t.Fatal(unit)
	}
}

func TestDatabasePasswordReusesThePriorURL(t *testing.T) {
	minted, err := DatabasePassword("")
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(minted) {
		t.Fatal(minted, err)
	}
	if again, _ := DatabasePassword(""); again == minted {
		t.Fatal("not random")
	}
	if got, _ := DatabasePassword("postgres://pages:" + minted + "@127.0.0.1:5432/pages?sslmode=disable"); got != minted {
		t.Fatal(got)
	}
	if got, _ := DatabasePassword("postgresql://pages:a.b_c~d-e@db.test/pages"); got != "a.b_c~d-e" {
		t.Fatal(got)
	}
	// An existing URL that cannot be reused verbatim fails, so bootstrap
	// stops before changing the role.
	for _, url := range []string{"mysql://pages:secret@host/pages", "postgres://other:secret@host/pages", "postgres://pages@host/pages",
		"postgres://pages:@host/pages", "postgres://pages:p%40ss@host/pages", "postgres://pages:it's@host/pages"} {
		if got, err := DatabasePassword(url); err == nil || got != "" {
			t.Fatalf("%s reused as %q", url, got)
		}
	}
}

func TestAtomicFilesAndLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "file")
	if err := WriteFileAtomic(target, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(link, []byte("old CLI"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SymlinkAtomic(target, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Fatal(got)
	}
	cp := filepath.Join(dir, "copy")
	if err := CopyPreserving(link, cp); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(cp); got != target {
		t.Fatal("CopyPreserving must keep a link a link")
	}
}
