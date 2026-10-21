// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestServiceEnvPlan(t *testing.T) {
	installed := serviceEnvInput{Settings: testSettings, Getenv: envOf(nil), EUID: 0, Me: "root", AccountExists: true,
		EnvFileExists: true, HaveSudo: "/usr/bin/sudo", Runuser: "/usr/sbin/runuser", Self: "/usr/local/bin/pages"}

	// Root on an installed box drops to the service account with runuser.
	step, argv, msg := serviceEnvPlan(installed, "token", []string{"add", "agent", "deploy"})
	joined := strings.Join(argv, " ")
	if step != becomeService || msg != "" || !strings.HasPrefix(joined, "/usr/sbin/runuser -u pages -- env ") ||
		!strings.Contains(joined, serviceEnvMarker) || !strings.Contains(joined, "PAGES_ENV_FILE=/etc/default/pages") ||
		!strings.HasSuffix(joined, "/usr/local/bin/pages token add agent deploy") {
		t.Fatalf("root: %v %q %q", step, joined, msg)
	}
	// No secret ever rides on the command line: the child reads the file.
	if strings.Contains(joined, "DATABASE_URL") || strings.Contains(joined, "PEPPER") {
		t.Fatal(joined)
	}

	// Another operator goes through sudo -u.
	op := installed
	op.EUID, op.Me = 1000, "northwind"
	if step, argv, _ := serviceEnvPlan(op, "access", []string{"list"}); step != becomeService || !strings.HasPrefix(strings.Join(argv, " "), "/usr/bin/sudo -u pages -- env ") {
		t.Fatalf("operator: %v %v", step, argv)
	}
	op.HaveSudo = ""
	if _, _, msg := serviceEnvPlan(op, "access", []string{"list"}); !strings.Contains(msg, "run it with sudo") {
		t.Fatalf("no sudo: %q", msg)
	}

	for name, in := range map[string]serviceEnvInput{
		"the service account": func() serviceEnvInput { x := installed; x.EUID, x.Me = 990, "pages"; return x }(),
		"already re-executed": func() serviceEnvInput {
			x := installed
			x.Getenv = envOf(map[string]string{"PAGES_CLI_AS_SERVICE_USER": "1"})
			return x
		}(),
		"no service account (root)": func() serviceEnvInput { x := installed; x.AccountExists = false; return x }(),
	} {
		if step, argv, msg := serviceEnvPlan(in, "template", []string{"sync"}); step != loadHere || argv != nil || msg != "" {
			t.Errorf("%s: %v %v %q", name, step, argv, msg)
		}
	}

	// Development and tests: the environment is already the caller's.
	with := func(m map[string]string) serviceEnvInput { x := installed; x.Getenv = envOf(m); return x }
	for name, tc := range map[string]struct {
		in   serviceEnvInput
		verb string
	}{
		"token: database and pepper": {with(map[string]string{"DATABASE_URL": "postgres://x", "API_TOKEN_PEPPER": "p"}), "token"},
		"token: PGHOST and pepper":   {with(map[string]string{"PGHOST": "/run/pages-dev", "API_TOKEN_PEPPER": "p"}), "token"},
		"template: database":         {with(map[string]string{"DATABASE_URL": "postgres://x"}), "template"},
		"access: PGHOST":             {with(map[string]string{"PGHOST": "/run/pages-dev"}), "access"},
		"no environment file":        {func() serviceEnvInput { x := installed; x.EnvFileExists = false; return x }(), "token"},
	} {
		if step, argv, msg := serviceEnvPlan(tc.in, tc.verb, []string{"list"}); step != runAsInvoked || argv != nil || msg != "" {
			t.Errorf("%s: %v %v %q", name, step, argv, msg)
		}
	}
}

// A partial caller environment never bypasses the installed file: token
// needs the pepper as well as the database, so a DATABASE_URL alone (or a
// pepper alone) still loads the service environment — re-executing as the
// service account from root, which then applies the file in-process.
func TestServiceEnvPlanRequiresTheCompleteEnvironment(t *testing.T) {
	installed := serviceEnvInput{Settings: testSettings, EUID: 0, Me: "root", AccountExists: true,
		EnvFileExists: true, HaveSudo: "/usr/bin/sudo", Runuser: "/usr/sbin/runuser", Self: "/usr/local/bin/pages"}
	for name, tc := range map[string]struct {
		env  map[string]string
		verb string
	}{
		"token: DATABASE_URL without pepper": {map[string]string{"DATABASE_URL": "postgres://x"}, "token"},
		"token: PGHOST without pepper":       {map[string]string{"PGHOST": "/tmp"}, "token"},
		"token: pepper without database":     {map[string]string{"API_TOKEN_PEPPER": "p"}, "token"},
		"template: pepper without database":  {map[string]string{"API_TOKEN_PEPPER": "p"}, "template"},
		"access: nothing":                    {nil, "access"},
	} {
		in := installed
		in.Getenv = envOf(tc.env)
		if step, _, msg := serviceEnvPlan(in, tc.verb, []string{"add", "agent", "deploy"}); step != becomeService || msg != "" {
			t.Errorf("%s (root): %v %q, want becomeService", name, step, msg)
		}
		in.EUID, in.Me = 990, "pages"
		if step, _, msg := serviceEnvPlan(in, tc.verb, []string{"add", "agent", "deploy"}); step != loadHere || msg != "" {
			t.Errorf("%s (service account): %v %q, want loadHere", name, step, msg)
		}
	}
}

// End to end: a caller with DATABASE_URL but no pepper gets the file's
// pepper (its own DATABASE_URL still wins); an unreadable file fails the
// verb instead of running it on the partial environment.
func TestServiceVerbPartialEnvironment(t *testing.T) {
	defer func(e func(string, []string, []string) error) { Exec = e }(Exec)
	Exec = func(string, []string, []string) error {
		t.Fatal("must not re-execute")
		return errors.New("unreachable")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "pages")
	if err := os.WriteFile(file, []byte("DATABASE_URL=\"postgres://pages@127.0.0.1:1/none\"\nAPI_TOKEN_PEPPER=\"contoso\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAGES_INSTALL_CONFIG", filepath.Join(dir, "absent-install"))
	t.Setenv("PAGES_ENV_FILE", file)
	t.Setenv("APP_USER", "pages-test-no-such-account")
	t.Setenv("DATABASE_URL", "postgres://caller")
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("PGHOST", "")
	t.Setenv("PAGES_ENV", "")

	ran := false
	run := serviceVerb("token", func([]string) int {
		ran = true
		if got := os.Getenv("API_TOKEN_PEPPER") + " " + os.Getenv("DATABASE_URL"); got != "contoso postgres://caller" {
			t.Errorf("saw %q", got)
		}
		return 7
	})
	if code := run([]string{"add", "agent", "deploy"}); code != 7 || !ran {
		t.Fatalf("code %d ran %v", code, ran)
	}

	// The file exists but cannot be read: fail, do not mint with no pepper.
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("PAGES_ENV_FILE", dir) // a directory: exists, unreadable as a file
	ran = false
	if code := run([]string{"add", "agent", "deploy"}); code == 0 || code == 7 || ran {
		t.Fatalf("unreadable file: code %d ran %v", code, ran)
	}
}

// The file is parsed, never sourced, and the caller's own values win.
func TestApplyServiceEnvParsesWithoutEvaluating(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pages")
	marker := filepath.Join(t.TempDir(), "ran")
	body := "DATABASE_URL=\"postgres://pages:s3cret@127.0.0.1:5432/pages\"\nAPI_TOKEN_PEPPER='northwind pepper'\n" +
		"PORT=3002\nCONTENT_HOST=\"$(touch " + marker + ")\"\ntouch " + marker + "\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("CONTENT_HOST", "")
	t.Setenv("PAGES_ENV", "")
	t.Setenv("PORT", "4000")
	if err := applyServiceEnv(file); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"DATABASE_URL": "postgres://pages:s3cret@127.0.0.1:5432/pages", "API_TOKEN_PEPPER": "northwind pepper",
		"PORT": "4000", "CONTENT_HOST": "$(touch " + marker + ")", "PAGES_ENV": "production",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the environment file was evaluated")
	}
}

// End to end through the verb wrapper: an installed layout whose service
// account does not exist loads the file in-process; a dev environment is
// left alone; nothing re-executes in either case.
func TestServiceVerbLoadsTheInstalledEnvironment(t *testing.T) {
	defer func(e func(string, []string, []string) error) { Exec = e }(Exec)
	Exec = func(string, []string, []string) error {
		t.Fatal("must not re-execute")
		return errors.New("unreachable")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "pages")
	if err := os.WriteFile(file, []byte("DATABASE_URL=\"postgres://pages@127.0.0.1:1/none\"\nAPI_TOKEN_PEPPER=\"contoso\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAGES_INSTALL_CONFIG", filepath.Join(dir, "absent-install"))
	t.Setenv("PAGES_ENV_FILE", file)
	t.Setenv("APP_USER", "pages-test-no-such-account")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("PAGES_ENV", "")

	var saw string
	run := serviceVerb("token", func([]string) int { saw = os.Getenv("API_TOKEN_PEPPER") + " " + os.Getenv("DATABASE_URL"); return 7 })
	if code := run([]string{"list"}); code != 7 || saw != "contoso postgres://pages@127.0.0.1:1/none" {
		t.Fatalf("installed: code %d, saw %q", code, saw)
	}

	// Already configured (pages dev, the test cluster): untouched.
	t.Setenv("API_TOKEN_PEPPER", "dev-pepper")
	t.Setenv("DATABASE_URL", "postgres://dev")
	if code := run([]string{"list"}); code != 7 || saw != "dev-pepper postgres://dev" {
		t.Fatalf("dev: code %d, saw %q", code, saw)
	}
}
