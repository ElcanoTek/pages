// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/install"
)

var testSettings = install.Settings{InstallConfig: "/etc/default/pages-install", AppDir: "/opt/pages", AppUser: "pages",
	SrcDir: "/opt/pages-src", EnvFile: "/etc/default/pages", CLITarget: "/usr/local/bin/pages", Port: "3002"}

// Read-only doctor runs as the SERVICE account: root drops to it with
// runuser, another operator goes through sudo -u; repairs need root.
func TestDoctorPlanDropsToTheServiceAccount(t *testing.T) {
	argv, msg := doctorPlan([]string{"--check"}, 0, "root", testSettings, "/opt/pages/pages", false)
	joined := strings.Join(argv, " ")
	if msg != "" || !strings.Contains(joined, "runuser -u pages -- env ") || !strings.HasSuffix(joined, "/opt/pages/pages doctor --check") ||
		!strings.Contains(joined, "GIT_OPTIONAL_LOCKS=0") || !strings.Contains(joined, droppedMarker) || !strings.Contains(joined, "PAGES_ENV_FILE=/etc/default/pages") {
		t.Fatal(joined)
	}
	if argv, _ := doctorPlan([]string{"--dry-run"}, 1000, "northwind", testSettings, "/opt/pages/pages", false); argv != nil && !strings.Contains(strings.Join(argv, " "), "-u pages --") {
		t.Fatal(argv)
	}
	if argv, msg := doctorPlan([]string{"--check"}, 0, "root", testSettings, "/x", true); argv != nil || msg != "" {
		t.Fatal("an already-dropped doctor must run in place")
	}
	if argv, msg := doctorPlan([]string{"--check"}, 990, "pages", testSettings, "/x", false); argv != nil || msg != "" {
		t.Fatal("the service account runs in place")
	}
	if _, msg := doctorPlan(nil, 1000, "northwind", testSettings, "/x", false); msg != "sudo" {
		t.Fatal("repairs need root")
	}
	if argv, msg := doctorPlan([]string{"--no-restart"}, 0, "root", testSettings, "/x", false); argv != nil || msg != "" {
		t.Fatal("root repairs in place")
	}
}

func TestReexecKeepsTheInstallationThroughSudo(t *testing.T) {
	t.Setenv("PAGES_UPDATE_YES", "1")
	s := testSettings
	s.Port, s.PortExplicit = "4312", true
	argv := strings.Join(reexecArgv([]string{"/usr/bin/sudo", "--"}, s, "/opt/pages/pages", "update", []string{"--yes"}), " ")
	for _, want := range []string{"/usr/bin/sudo -- env ", "PAGES_INSTALL_CONFIG=/etc/default/pages-install", "APP_USER=pages", "PAGES_PORT=4312", "PAGES_UPDATE_YES=1", "/opt/pages/pages update --yes"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("missing %q in %s", want, argv)
		}
	}
}

func TestElevateIsANoOpForRootAndSudoOtherwise(t *testing.T) {
	defer func(g func() int, e func(string, []string, []string) error) { Geteuid, Exec = g, e }(Geteuid, Exec)
	Geteuid = func() int { return 0 }
	if elevate("env", nil) {
		t.Fatal("root must not re-execute")
	}
	var got []string
	Geteuid = func() int { return 1000 }
	Exec = func(_ string, argv []string, _ []string) error { got = argv; return errors.New("stopped by test") }
	if !elevate("backup", []string{"/var/backups/pages"}) {
		t.Fatal("non-root must re-execute")
	}
	if len(got) < 3 || !strings.HasSuffix(got[0], "sudo") || got[len(got)-2] != "backup" {
		t.Fatal(got)
	}
}

func TestEnvRedactsAndReportsAMissingFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pages")
	if code := printEnv(&bytes.Buffer{}, file); code != 1 {
		t.Fatal(code)
	}
	if err := os.WriteFile(file, []byte("# comment\nRAW_TOKEN_SECRET=\"abc\"\nDATABASE_URL=\"postgres://pages:hunter2@127.0.0.1:5432/pages?sslmode=disable\"\nPORT=\"3002\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := printEnv(&out, file); code != 0 {
		t.Fatal(code)
	}
	if out.String() != "RAW_TOKEN_SECRET=[REDACTED]\nDATABASE_URL=\"postgres://pages:[REDACTED]@127.0.0.1:5432/pages?sslmode=disable\"\nPORT=\"3002\"\n" {
		t.Fatal(out.String())
	}
}

func TestPreflightRefusesMissingProductionSecrets(t *testing.T) {
	t.Setenv("PAGES_ENV", "production")
	t.Setenv("RAW_TOKEN_SECRET", "northwind")
	t.Setenv("PAGE_COOKIE_SECRET", "northwind")
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("DATABASE_URL", "postgres://pages@127.0.0.1:5432/pages?sslmode=disable")
	if err := Preflight(); err == nil || !strings.Contains(err.Error(), "API_TOKEN_PEPPER is required in production") {
		t.Fatal(err)
	}
	t.Setenv("API_TOKEN_PEPPER", "northwind")
	if err := Preflight(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://[bad")
	if err := Preflight(); err == nil {
		t.Fatal("an unparseable DATABASE_URL must fail preflight")
	}
	defer func() { PreflightChecks = nil }()
	t.Setenv("DATABASE_URL", "")
	PreflightChecks = append(PreflightChecks, func() error { return errors.New("server says no") })
	if err := Preflight(); err == nil || err.Error() != "server says no" {
		t.Fatal(err)
	}
}

func TestCheckoutRoot(t *testing.T) {
	wd, _ := os.Getwd()
	root := checkoutRoot(wd)
	if root == "" || !strings.HasSuffix(filepath.Join(root, "internal", "cli"), filepath.Join("internal", "cli")) {
		t.Fatal(root)
	}
	if _, err := os.Stat(filepath.Join(root, "deploy", "pages.service")); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRejectsUnknownArguments(t *testing.T) {
	if code := runUpdate("update", []string{"--bogus"}, false); code != 1 {
		t.Fatal(code)
	}
	if code := runUpdate("update", []string{"--activate"}, false); code != 1 {
		t.Fatal(code)
	}
}
