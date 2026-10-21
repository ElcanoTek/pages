// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package envfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/runner"
)

// shellDecode evaluates text with bash and returns the environment it sets,
// the reference the parser and the quoting must agree with.
func shellDecode(t *testing.T, text string, keys ...string) map[string]string {
	t.Helper()
	script := `set -a; eval "$1"; set +a; for k in "${@:2}"; do printf '%s\0' "${!k-}"; done`
	out, err := exec.Command("bash", append([]string{"-c", script, "_", text}, keys...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	vals := strings.Split(string(out), "\x00")
	m := map[string]string{}
	for i, k := range keys {
		m[k] = vals[i]
	}
	return m
}

func TestMergePreservesOperatorValuesAndQuotesLosslessly(t *testing.T) {
	original := "# operator settings\nRL_API_PER_MIN=\"357\"\nPG_LOCK_TIMEOUT_MS=\"1234\"\nPAGES_DATA_MAX_BYTES=\"524288\"\nOPERATOR_NOTE=\"first\nPORT=inside-note\nlast\"\nPORT=\"3002\"\n"
	database := "postgres://pages:synthetic-$`\\\"'@localhost/pages"
	updates := []Update{{"PORT", "4312"}, {"DATABASE_URL", database}}
	ctx := context.Background()
	updated, err := Merge(ctx, runner.Exec{}, original, updates)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(updated, original[:strings.LastIndex(original, `PORT="3002"`)]) {
		t.Fatalf("unmanaged records changed:\n%s", updated)
	}
	env := shellDecode(t, updated, "RL_API_PER_MIN", "PG_LOCK_TIMEOUT_MS", "PAGES_DATA_MAX_BYTES", "OPERATOR_NOTE", "PORT", "DATABASE_URL")
	want := map[string]string{"RL_API_PER_MIN": "357", "PG_LOCK_TIMEOUT_MS": "1234", "PAGES_DATA_MAX_BYTES": "524288",
		"OPERATOR_NOTE": "first\nPORT=inside-note\nlast", "PORT": "4312", "DATABASE_URL": database}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Fatalf("bash decodes %v, want %v", env, want)
	}
	// The non-evaluating parser reads what bash reads.
	parsed := Parse(updated).Map()
	for k, v := range want {
		if parsed[k] != v {
			t.Fatalf("Parse %s = %q, want %q", k, parsed[k], v)
		}
	}
	again, err := Merge(ctx, runner.Exec{}, updated, updates)
	if err != nil || again != updated {
		t.Fatalf("reruns accumulate entries:\n%s", again)
	}
}

func TestWriteKeepsOriginalOnFailureAndARecoverableCopy(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pages")
	original := "PORT=\"3002\"\nRL_CONTENT_PER_MIN=\"391\"\n"
	if err := os.WriteFile(file, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Write(ctx, runner.Exec{}, file, []Update{{"PORT", "bad\nvalue"}}); err == nil || !strings.Contains(err.Error(), "single-line") {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != original {
		t.Fatal("original changed")
	}
	// A staging write failure occurs before rename and must not truncate.
	staging := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(ctx, runner.Exec{}, file, []Update{{"PORT", "4312"}}); err == nil {
		t.Fatal("staging collision must fail")
	}
	if b, _ := os.ReadFile(file); string(b) != original {
		t.Fatal("original truncated")
	}
	_ = os.Remove(staging)
	if err := Write(ctx, runner.Exec{}, file, []Update{{"PORT", "4312"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file + ".previous"); string(b) != original {
		t.Fatal("no recoverable copy")
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "PORT=\"4312\"\nRL_CONTENT_PER_MIN=\"391\"") {
		t.Fatal(string(b))
	}
	for _, f := range []string{file, file + ".previous"} {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o640 {
			t.Fatalf("%s mode %v", f, st.Mode())
		}
	}
	// A brand-new file is created 0640.
	fresh := filepath.Join(dir, "fresh")
	if err := Write(ctx, runner.Exec{}, fresh, []Update{{"PORT", "1"}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(fresh); st.Mode().Perm() != 0o640 {
		t.Fatal(st.Mode())
	}
}

func TestMergeRejectsInvalidShell(t *testing.T) {
	fake := &runner.Fake{Handle: func(runner.Cmd) (string, int) { return "", 2 }}
	if _, err := Merge(context.Background(), fake, "", []Update{{"PORT", "1"}}); err == nil || !strings.Contains(err.Error(), "not valid shell syntax") {
		t.Fatal(err)
	}
	if _, err := Merge(context.Background(), nil, "", []Update{{"BAD-KEY", "1"}}); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestParseNeverEvaluates(t *testing.T) {
	f := Parse("A=\"x\"\ntouch /tmp/pwned\nexport B='y z'\nC=plain # comment\nD=a\\ b\"c\"'d'\n# E=no\nA=\"last\"\n")
	got, _ := json.Marshal(f.Map())
	if string(got) != `{"A":"last","B":"y z","C":"plain","D":"a bcd"}` {
		t.Fatal(string(got))
	}
}

func TestRedact(t *testing.T) {
	in := strings.Join([]string{
		"# a comment with SECRET=x",
		"PORT=\"3002\"",
		"API_TOKEN_PEPPER=\"abc\"",
		"auth_signing_pubkey=\"k\"",
		"DEV_ADMIN_COOKIE=c",
		"AUTH_CLIENT_SECRET=\"s\"",
		"DATABASE_URL=\"postgres://pages:p@ss:w@rd@127.0.0.1:5432/pages?sslmode=disable\"",
		"REDIS_URL=redis://:onlypass@cache:6379/0",
		"NOEQUALS line",
		"DASHBOARD_HOST=pages.contoso.example",
		"",
	}, "\n")
	want := strings.Join([]string{
		"PORT=\"3002\"",
		"API_TOKEN_PEPPER=[REDACTED]",
		"auth_signing_pubkey=[REDACTED]",
		"DEV_ADMIN_COOKIE=[REDACTED]",
		"AUTH_CLIENT_SECRET=[REDACTED]",
		"DATABASE_URL=\"postgres://pages:[REDACTED]@127.0.0.1:5432/pages?sslmode=disable\"",
		"REDIS_URL=redis://:[REDACTED]@cache:6379/0",
		"DASHBOARD_HOST=pages.contoso.example",
		"",
	}, "\n")
	if got := Redact(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
