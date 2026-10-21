// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package doctor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

// Static-looking fixture secrets. The leakage test asserts none of these
// values appears anywhere in doctor's output (key NAMES are fine).
var secrets = map[string]string{
	"pubkey":     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{97}, 32)),
	"pageCookie": strings.Repeat("0123456789abcdef", 4),
	"rawToken":   strings.Repeat("abcdef0123456789", 4),
	"pepper":     strings.Repeat("aaaaaaaabbbbbbbb", 4),
	"dbPassword": "db-northwind-9f2c",
}

const redactedDBURL = "postgres://pages@127.0.0.1:5432/pages?sslmode=disable"

type opts struct {
	args                 []string
	envFile              string
	statPerms            string
	runningPrevious      bool
	inactive             bool
	gitBehind, gitRemote string
	lsRemoteFail         bool
	readyzFails          int
	dbPassword           string
	psqlFail             bool
	pagesPort            string
	euid                 int
	caddySnippet         string
	certErr              error
}

type result struct {
	code    int
	output  string
	actions []string
	readyz  int
	dir     string
}

// dq escapes a value for a double-quoted env-file string, backslashes first.
func dq(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`)
}

func doctor(t *testing.T, o opts) result {
	t.Helper()
	dir := t.TempDir()
	release := filepath.Join(dir, "release-current")
	previous := filepath.Join(dir, "release-previous")
	source := filepath.Join(dir, "source")
	app := filepath.Join(dir, "app")
	envPath := filepath.Join(dir, "pages.env")
	osRelease := filepath.Join(dir, "os-release")
	for _, d := range []string{release, previous, filepath.Join(source, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(release, "pages"), []byte("contoso fixture release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, app); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "cli")
	if err := os.Symlink(filepath.Join(app, "pages"), cli); err != nil {
		t.Fatal(err)
	}
	hosts := map[string][2]string{
		"shared-domain": {"pages.contoso.example", "cdn.pages.contoso.example"},
		// Different subdomains under the SAME co.uk domain: the naive
		// first-label comparison misses this pair, the PSL-aware one must not.
		"etld-same": {"pages.contoso.co.uk", "cdn.contoso.co.uk"},
		// com.ng is a two-level public suffix: DIFFERENT registrable domains.
		"ng-distinct": {"pages.contoso.com.ng", "pages.northwind.com.ng"},
		"ng-same":     {"pages.contoso.com.ng", "cdn.contoso.com.ng"},
		// Both under the !www.ck exception: they share www.ck and must fail.
		"ck-exception": {"foo.www.ck", "bar.www.ck"},
	}[o.envFile]
	if hosts[0] == "" {
		hosts = [2]string{"pages.contoso.example", "pages.northwind.example"}
	}
	pubkey := secrets["pubkey"]
	if o.envFile == "bad-pubkey" {
		pubkey += "!"
	}
	if o.dbPassword == "" {
		o.dbPassword = secrets["dbPassword"]
	}
	dbURL := fmt.Sprintf("postgres://pages:%s@127.0.0.1:5432/pages?sslmode=disable", o.dbPassword)
	pwned := filepath.Join(dir, "pwned")
	if o.envFile != "missing" {
		lines := []string{
			`PORT="4312"`,
			`DASHBOARD_HOST="` + dq(hosts[0]) + `"`,
			`CONTENT_HOST="` + dq(hosts[1]) + `"`,
			`AUTH_SIGNING_PUBKEY="` + dq(pubkey) + `"`,
			`PAGE_COOKIE_SECRET="` + dq(secrets["pageCookie"]) + `"`,
			`RAW_TOKEN_SECRET="` + dq(secrets["rawToken"]) + `"`,
			`API_TOKEN_PEPPER="` + dq(secrets["pepper"]) + `"`,
			`DATABASE_URL="` + dq(dbURL) + `"`,
			// Sourcing canary: if anything ever evaluates this file as shell
			// code, this line runs.
			"touch '" + pwned + "'",
			"",
		}
		if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(osRelease, []byte("ID=fedora\nVERSION_ID=\"42\"\nSUPPORT_END=\"2030-12-31\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var actions []string
	act := func(s string) { mu.Lock(); actions = append(actions, s); mu.Unlock() }
	active := !o.inactive
	readyz := 0
	if o.gitRemote == "" {
		o.gitRemote = "abc1230"
	}
	fake := &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		args := strings.Join(c.Args, " ")
		switch c.Name {
		case "systemctl":
			switch c.Args[0] {
			case "is-active":
				last := c.Args[len(c.Args)-1]
				if last == "postgresql.service" || last == "caddy.service" || active {
					return "", 0
				}
				return "", 3
			case "is-enabled":
				return "", 0
			case "start", "restart":
				act(c.Args[0] + " " + c.Args[len(c.Args)-1])
				active = true
				return "", 0
			case "show":
				return "4242\n", 0
			}
		case "runuser", "psql":
			if o.psqlFail {
				return "", 1
			}
			pw := ""
			for _, e := range c.Env {
				if strings.HasPrefix(e, "PGPASSWORD=") {
					pw = strings.TrimPrefix(e, "PGPASSWORD=")
				}
			}
			act(fmt.Sprintf("%s pgpassword=%s argv=%s", c.Name, pw, args))
			return "1\n", 0
		case "git":
			switch {
			case strings.Contains(args, "fetch"):
				act("git-fetch")
			case strings.Contains(args, "ls-remote"):
				act("git-ls-remote")
				if o.lsRemoteFail {
					return "", 1
				}
				return o.gitRemote + "\trefs/heads/main\n", 0
			case strings.Contains(args, "--abbrev-ref"):
				return "main\n", 0
			case strings.Contains(args, "rev-list --count"):
				return o.gitBehind + "\n", 0
			case strings.Contains(args, "rev-parse"):
				return "abc1230\n", 0
			}
			return "", 0
		case "rpm":
			return "6.9.0-100.fc40.x86_64\n", 0
		case "caddy":
			if c.Args[0] == "version" {
				return "v2.8.4\n", 0
			}
		}
		return "", 0
	}}
	settings := install.Settings{
		InstallConfig: filepath.Join(dir, "install"), AppDir: app, AppUser: "contoso", SrcDir: source,
		EnvFile: envPath, CLITarget: cli, Port: "4312",
	}
	if o.pagesPort != "" {
		settings.Port, settings.PortExplicit = o.pagesPort, true
	}
	statPerms := o.statPerms
	if statPerms == "" {
		statPerms = "640 root:contoso"
	}
	snippet := filepath.Join(dir, "pages.caddy")
	if o.caddySnippet != "" {
		_ = os.WriteFile(snippet, []byte(o.caddySnippet), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "Caddyfile"), []byte("import conf.d/*.caddy\n"), 0o644)
	}
	now := time.Now()
	var out bytes.Buffer
	options := Options{
		Settings: settings, Runner: fake, Log: term.Buffer(&out), EUID: o.euid, Username: "contoso",
		CaddySnippet: snippet, Caddyfile: filepath.Join(dir, "Caddyfile"), OSRelease: osRelease,
		StatOwner: func(string) (string, error) { return statPerms, nil },
		Readable:  func(p string) bool { return o.envFile != "unreadable" && readable(p) },
		RepairEnv: func(p, g string) error { act("chown root:" + g + " " + p); act("chmod 0640 " + p); return nil },
		ProcCwd: func(int) (string, error) {
			if o.runningPrevious {
				return filepath.EvalSymlinks(previous)
			}
			return filepath.EvalSymlinks(release)
		},
		FreeKiB: func(string) (uint64, error) { return 1 << 30, nil },
		Probe: func(_ context.Context, url string) bool {
			if !strings.Contains(url, "127.0.0.1:4312/readyz") {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			readyz++
			return readyz > o.readyzFails
		},
		Certificate: func(context.Context, string) (time.Time, error) {
			return now.Add(45 * 24 * time.Hour), o.certErr
		},
		FedoraLatest: func(context.Context) (int, error) { return 0, errors.New("feed denied") },
		Kernel:       func() string { return "6.9.0-100.fc40.x86_64" },
		Sleep:        func(time.Duration) {},
		Now:          func() time.Time { return now },
	}
	for _, a := range o.args {
		switch a {
		case "--check":
			options.Check = true
		case "--strict":
			options.Strict = true
		case "--dry-run":
			options.DryRun = true
		case "--no-restart":
			options.NoRestart = true
		}
	}
	code := Run(context.Background(), options)
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("doctor executed content of the env file")
	}
	return result{code: code, output: out.String(), actions: actions, readyz: readyz, dir: dir}
}

func (r result) log() string { return strings.Join(r.actions, "\n") + "\n" }

func must(t *testing.T, r result, code int, patterns ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d:\n%s", r.code, code, r.output)
	}
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(r.output) {
			t.Fatalf("output lacks /%s/:\n%s", p, r.output)
		}
	}
}

func TestCheckOnHealthyBoxChangesNothing(t *testing.T) {
	r := doctor(t, opts{args: []string{"--check"}, euid: 1000})
	must(t, r, 0, `box healthy`, `/readyz`, `current with origin/main`,
		`registrable domain \(northwind\.example\) differs from the dashboard's \(contoso\.example\)`)
	if !strings.Contains(r.log(), "git-ls-remote\n") || strings.Contains(r.log(), "git-fetch") {
		t.Fatalf("freshness must come from ls-remote, never fetch:\n%s", r.log())
	}
	if regexp.MustCompile(`(start|restart|chown|chmod) `).MatchString(r.log()) {
		t.Fatalf("read-only run repaired something:\n%s", r.log())
	}
}

func TestRepairsPermsStoppedServiceAndStaleRelease(t *testing.T) {
	r := doctor(t, opts{statPerms: "600 contoso:contoso", runningPrevious: true, inactive: true, euid: 0})
	must(t, r, 0, `box repaired`)
	for _, want := range []string{"start pages.service", "restart pages.service", "chown ", "chmod "} {
		if !strings.Contains(r.log(), want) {
			t.Fatalf("missing %q in:\n%s", want, r.log())
		}
	}
	// Root probes the database AS the service account.
	if !strings.Contains(r.log(), "runuser pgpassword="+secrets["dbPassword"]+" argv=-u contoso -- psql "+redactedDBURL) {
		t.Fatalf("root probe not delegated:\n%s", r.log())
	}
}

func TestCheckFlagsMissingEnvAndSharedDomains(t *testing.T) {
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "missing", euid: 1000}), 1, `missing — run scripts/bootstrap\.sh`)
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "shared-domain", euid: 1000}), 1, `shares the registrable domain contoso\.example`)
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "etld-same", euid: 1000}), 1, `shares the registrable domain contoso\.co\.uk`)
}

func TestFullPublicSuffixList(t *testing.T) {
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "ng-distinct", euid: 1000}), 0,
		`registrable domain \(northwind\.com\.ng\) differs from the dashboard's \(contoso\.com\.ng\)`)
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "ng-same", euid: 1000}), 1, `shares the registrable domain contoso\.com\.ng`)
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "ck-exception", euid: 1000}), 1, `shares the registrable domain www\.ck`)
}

func TestUnreadableEnvLimitsValidation(t *testing.T) {
	r := doctor(t, opts{args: []string{"--check"}, envFile: "unreadable", pagesPort: "4312", euid: 1000})
	must(t, r, 0, `not readable as`, `run sudo pages doctor for full validation`)
	if strings.Contains(r.output, "unset in") {
		t.Fatalf("unreadable env produced false 'key unset' failures:\n%s", r.output)
	}
}

func TestFailedDatabaseProbeFails(t *testing.T) {
	must(t, doctor(t, opts{args: []string{"--check"}, psqlFail: true, euid: 1000}), 1,
		`database probe failed — DATABASE_URL in .* is not usable`)
}

func TestPartiallyDecodablePubkeyIsRejected(t *testing.T) {
	must(t, doctor(t, opts{args: []string{"--check"}, envFile: "bad-pubkey", euid: 1000}), 1, `not a base64 32-byte Ed25519 key`)
}

func TestDatabasePasswordTravelsInPGPASSWORD(t *testing.T) {
	for _, pw := range []string{secrets["dbPassword"], `p"'\w0rd`} {
		r := doctor(t, opts{args: []string{"--check"}, dbPassword: pw, euid: 1000})
		must(t, r, 0)
		if !strings.Contains(r.log(), "psql pgpassword="+pw+" argv="+redactedDBURL+" -tAc SELECT 1") {
			t.Fatalf("probe for %q:\n%s", pw, r.log())
		}
		if strings.Contains(r.log(), "argv=postgres://pages:") {
			t.Fatal("full URL (with password) reached psql's argv")
		}
	}
}

func TestWaitsForReadinessAfterRepairStart(t *testing.T) {
	r := doctor(t, opts{inactive: true, readyzFails: 3, euid: 0})
	must(t, r, 0, `box repaired`, `/readyz`)
	if r.readyz <= 1 {
		t.Fatalf("expected several readiness polls, got %d", r.readyz)
	}
}

func TestLsRemoteFreshness(t *testing.T) {
	behind := doctor(t, opts{args: []string{"--check"}, gitRemote: "def4567", gitBehind: "3", euid: 1000})
	must(t, behind, 0, `3 commit\(s\) behind origin/main — run: sudo pages update`)
	if strings.Contains(behind.output, "current with origin/main") || strings.Contains(behind.log(), "git-fetch") {
		t.Fatal(behind.output)
	}
	unreachable := doctor(t, opts{args: []string{"--check"}, gitRemote: "def4567", lsRemoteFail: true, euid: 1000})
	must(t, unreachable, 0, `could not reach origin`)
	if strings.Contains(unreachable.output, "behind origin/main") || strings.Contains(unreachable.output, "current with origin/main") {
		t.Fatal(unreachable.output)
	}
}

func TestSecretsNeverPrinted(t *testing.T) {
	r := doctor(t, opts{args: []string{"--check"}, euid: 1000})
	must(t, r, 0, `could not read the Fedora release feed`, `PAGE_COOKIE_SECRET set`, `API_TOKEN_PEPPER set`)
	for name, v := range secrets {
		if strings.Contains(r.output, v) {
			t.Fatalf("doctor output leaked %s", name)
		}
	}
}

func TestStrictAndDryRun(t *testing.T) {
	must(t, doctor(t, opts{args: []string{"--check", "--strict"}, euid: 1000}), 1, `PROBLEM\(S\)`)
	dry := doctor(t, opts{args: []string{"--dry-run"}, euid: 1000})
	must(t, dry, 0, `\[dry-run\]`)
	if len(dry.actions) != 0 {
		t.Fatalf("dry run acted: %v", dry.actions)
	}
}

func TestCertificates(t *testing.T) {
	block := "pages.contoso.example {\n\treverse_proxy 127.0.0.1:4312\n}\n"
	must(t, doctor(t, opts{args: []string{"--check"}, caddySnippet: block, euid: 1000}), 0,
		`https://pages\.contoso\.example — certificate verified \(chain \+ hostname\), 4[45] days left`, `Caddy configuration valid`)
	must(t, doctor(t, opts{args: []string{"--check"}, caddySnippet: block, certErr: errors.New("x509: unknown authority"), euid: 1000}), 1,
		`no verifiable certificate for https://pages\.contoso\.example`)
	must(t, doctor(t, opts{args: []string{"--check"}, caddySnippet: "pages.contoso.example {\n\ttls internal\n}\n", certErr: errors.New("x509"), euid: 1000}), 0,
		`does not verify against public trust \(Caddy serves 'tls internal'\)`)
}

func TestStaleReleaseUnderCheckFails(t *testing.T) {
	r := doctor(t, opts{args: []string{"--check"}, runningPrevious: true, euid: 1000})
	must(t, r, 1, `the last release switch never restarted`)
	noRestart := doctor(t, opts{args: []string{"--no-restart"}, runningPrevious: true, euid: 0})
	must(t, noRestart, 0, `service runs the previous release — apply it: sudo pages restart`)
	if strings.Contains(noRestart.log(), "restart pages.service") {
		t.Fatal("--no-restart restarted")
	}
}

func TestHelpers(t *testing.T) {
	if got := newestVersion([]string{"6.9.0-100.fc40.x86_64", "6.10.2-200.fc40.x86_64", "6.9.10-1.fc40.x86_64"}); got != "6.10.2-200.fc40.x86_64" {
		t.Fatal(got)
	}
	n, err := LatestFedora(strings.NewReader(`[{"version":"41"},{"version":"43"},{"version":"44 Beta"},{"version":"42"}]`))
	if err != nil || n != 43 {
		t.Fatal(n, err)
	}
	pw, red := SplitDatabaseURL("postgres://pages:p:w@x@127.0.0.1/pages")
	if pw != "p:w" || red != "postgres://pages@x@127.0.0.1/pages" {
		t.Fatal(pw, red) // the shell split at the FIRST '@', as libpq-unaware text
	}
}
