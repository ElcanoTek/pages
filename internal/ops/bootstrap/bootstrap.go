// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package bootstrap is `pages bootstrap`, the interactive installer
// scripts/bootstrap.sh hands off to once it has built the binary:
//
//  1. installs PostgreSQL and the build tools (dnf)
//  2. creates the service account and the source checkout
//  3. installs this binary as the first release (APP_DIR → release symlink)
//  4. provisions PostgreSQL (initdb, loopback scram-sha-256, role+db pages),
//     writes the environment file (AUTH_SIGNING_PUBKEY, the two hostnames,
//     DATABASE_URL, generated secrets), migrates, and mints the initial agent
//     API token (first run only)
//  5. installs the systemd unit, the CLI link and the login banner; removes
//     Cockpit; makes SSH key-only; names the box; starts the service and
//     waits for /readyz
//  6. optionally configures Caddy + auto-TLS for BOTH hostnames
//
// Idempotent: re-runs preserve existing secrets, the database role password,
// data, and an active release (use `pages update` to change code).
//
// Scripted: PAGES_BOOTSTRAP_NON_INTERACTIVE=1 + AUTH_SIGNING_PUBKEY +
// PAGES_BOOTSTRAP_DASHBOARD_HOST + PAGES_BOOTSTRAP_CONTENT_HOST (+ the
// Caddy/Let's Encrypt answers).
package bootstrap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/migrate"
	"github.com/ElcanoTek/pages/internal/ops/doctor"
	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/hostname"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/psl"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/sshd"
	"github.com/ElcanoTek/pages/internal/ops/term"
	"github.com/ElcanoTek/pages/internal/version"
)

// Options configures a bootstrap. Zero hooks take production defaults.
type Options struct {
	Settings install.Settings
	// SrcDir is the checkout bootstrap runs from (copied to the installed
	// checkout on first install).
	SrcDir string
	Runner runner.Runner
	Log    *term.Printer
	Getenv func(string) string
	// Prompts reads answers (the TTY); Interactive says whether one exists.
	Prompts     io.Reader
	Interactive bool
	// Self is the binary installed as the release.
	Self string
	// Root prefixes the fixed system paths (/etc/..., /var/lib/pgsql) and
	// the ssh/hostname steps (tests only).
	Root string

	Probe   func(ctx context.Context, url string) bool
	Sleep   func(time.Duration)
	Now     func() time.Time
	Migrate func(ctx context.Context, url string) error
	// ActiveTokens counts unrevoked agent tokens (-1 when unknown).
	ActiveTokens func(ctx context.Context, url string) int
	// Chown gives a path to the service account (recursively for a
	// directory when recursive).
	Chown func(ctx context.Context, path string, recursive bool) error
}

func (o *Options) defaults() {
	if o.Runner == nil {
		o.Runner = runner.Default
	}
	if o.Log == nil {
		o.Log = term.New()
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Prompts == nil {
		o.Prompts = os.Stdin
	}
	if o.Probe == nil {
		o.Probe = readyProbe
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Migrate == nil {
		o.Migrate = migrateURL
	}
	if o.ActiveTokens == nil {
		o.ActiveTokens = activeTokens
	}
	if o.Chown == nil {
		o.Chown = func(ctx context.Context, path string, recursive bool) error {
			args := []string{"-h"}
			if recursive {
				args = []string{"-R"}
			}
			_, err := o.Runner.Run(ctx, runner.Cmd{Name: "chown", Args: append(args, o.Settings.AppUser+":"+o.Settings.AppUser, path)})
			return err
		}
	}
}

func (o *Options) path(p string) string { return filepath.Join(o.Root, p) }

// Error is a bootstrap stop with its operator-facing reason.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func die(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

type run struct {
	o       *Options
	log     *term.Printer
	vars    map[string]string
	reader  *bufio.Reader
	nonInt  bool
	initial string
}

// Run bootstraps the box.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	r := &run{o: &o, log: o.Log, reader: bufio.NewReader(o.Prompts), nonInt: o.Getenv("PAGES_BOOTSTRAP_NON_INTERACTIVE") == "1"}
	return r.all(ctx)
}

func (r *run) cmd(ctx context.Context, name string, args ...string) error {
	_, err := r.o.Runner.Run(ctx, runner.Cmd{Name: name, Args: args})
	return err
}

func (r *run) all(ctx context.Context) error {
	o, s, log := r.o, r.o.Settings, r.log
	if _, err := os.Stat(filepath.Join(o.SrcDir, "go.mod")); err != nil {
		return die("not an Elcano Pages checkout at %s", o.SrcDir)
	}
	log.Say(log.Bold() + "Elcano Pages — bootstrap" + log.Reset())
	log.Say(log.Dim() + "Fedora / RHEL 9+  •  systemd  •  PostgreSQL  •  one Go binary, two hostnames" + log.Reset())
	log.Say("")
	log.Say("Safe to re-run: existing env is preserved; only missing values are prompted.")

	log.Step("1/6  Installing PostgreSQL + build tools + system dependencies")
	if err := r.cmd(ctx, "dnf", "install", "-y", "git", "make", "golang", "postgresql", "postgresql-server"); err != nil {
		return die("dnf install failed — check the package repositories and network")
	}
	if err := s.Validate(); err != nil {
		log.Error(err.Error())
		return die("invalid installation settings")
	}
	pg, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "postgres", Args: []string{"--version"}})
	if pg == "" {
		pg = "postgresql installed"
	}
	log.OK(fmt.Sprintf("pages %s  •  %s", version.Version, pg))

	log.Step(fmt.Sprintf("2/6  Preparing %s + '%s' user", s.AppDir, s.AppUser))
	if r.cmd(ctx, "id", "-u", s.AppUser) != nil {
		if err := r.cmd(ctx, "useradd", "--system", "--home-dir", s.AppDir, "--shell", "/usr/sbin/nologin", s.AppUser); err != nil {
			return die("useradd %s failed", s.AppUser)
		}
	}
	if _, err := os.Stat(filepath.Join(s.SrcDir, ".git")); err != nil {
		if _, err := os.Stat(filepath.Join(o.SrcDir, ".git")); err != nil {
			return die("bootstrap must run from a git checkout (no .git at %s)", o.SrcDir)
		}
		if err := os.MkdirAll(s.SrcDir, 0o755); err != nil {
			return err
		}
		// Root-owned whoever cloned it: the updater fetches, merges and
		// builds this checkout as root.
		if err := r.cmd(ctx, "cp", "-a", "--no-preserve=ownership", o.SrcDir+"/.", s.SrcDir+"/"); err != nil {
			return die("could not copy the checkout to %s", s.SrcDir)
		}
	}

	log.Step("3/6  Installing the release")
	if err := r.release(ctx); err != nil {
		return err
	}

	log.Step("4/6  Database + instance configuration")
	if err := r.configure(ctx); err != nil {
		return err
	}

	log.Step("5/6  Installing systemd unit + CLI + starting")
	if err := r.services(ctx); err != nil {
		return err
	}

	log.Step("6/6  Reverse proxy / TLS (both hostnames)")
	if err := r.caddy(ctx); err != nil {
		return err
	}
	r.summary()
	return nil
}

// release installs this binary as the active release unless a Go release is
// already active (a re-run never changes code; `pages update` does).
func (r *run) release(ctx context.Context) error {
	o, s, log := r.o, r.o.Settings, r.log
	if target, err := filepath.EvalSymlinks(s.AppDir); err == nil && isSymlink(s.AppDir) {
		root, _ := filepath.EvalSymlinks(s.ReleaseRoot())
		if root != "" && strings.HasPrefix(target, root+"/") && exists(filepath.Join(target, install.BinaryName)) {
			log.Info("release-managed installation: preserving active release; use pages update to change code")
			if _, err := install.TrustedFile(filepath.Join(target, install.BinaryName)); err != nil {
				log.Warn("the active release can be changed by an account other than root (" + err.Error() + "); root runs it — reinstall it root-owned with pages update")
			}
			return nil
		}
	}
	if err := os.MkdirAll(s.ReleaseRoot(), 0o755); err != nil {
		return err
	}
	sha, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "git", Args: []string{"-C", s.SrcDir, "rev-parse", "--short", "HEAD"}})
	if sha == "" {
		sha = "unknown"
	}
	stamp := o.Now().UTC().Format("20060102T150405Z")
	release := filepath.Join(s.ReleaseRoot(), fmt.Sprintf("%s-%s-%d", sha, stamp, os.Getpid()))
	if err := os.Mkdir(release, 0o755); err != nil {
		return err
	}
	if err := copyExecutable(o.Self, filepath.Join(release, install.BinaryName)); err != nil {
		return die("could not install the binary into %s: %v", release, err)
	}
	// A legacy directory install (an older bootstrap's, or just the empty
	// home directory useradd made) becomes a retained predecessor, its
	// mutable paths moving to the shared locations every release links to.
	if st, err := os.Lstat(s.AppDir); err == nil && st.IsDir() {
		entries, _ := os.ReadDir(s.AppDir)
		if len(entries) == 0 {
			if err := os.Remove(s.AppDir); err != nil {
				return err
			}
		} else {
			previous := filepath.Join(s.ReleaseRoot(), fmt.Sprintf("legacy-%s-%d", stamp, os.Getpid()))
			if err := os.Rename(s.AppDir, previous); err != nil {
				return err
			}
			if exists(filepath.Join(previous, "assets")) && !exists(s.SharedAssets()) {
				if err := os.Rename(filepath.Join(previous, "assets"), s.SharedAssets()); err != nil {
					return err
				}
				if err := os.Symlink(s.SharedAssets(), filepath.Join(previous, "assets")); err != nil {
					return err
				}
			}
			if exists(filepath.Join(previous, ".env")) && !exists(s.SharedEnv()) {
				if err := os.Rename(filepath.Join(previous, ".env"), s.SharedEnv()); err != nil {
					return err
				}
				if err := os.Symlink(s.SharedEnv(), filepath.Join(previous, ".env")); err != nil {
					return err
				}
			}
			log.Info("previous installation retained at " + previous)
		}
	}
	if err := os.MkdirAll(s.SharedAssets(), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(s.SharedAssets(), filepath.Join(release, "assets")); err != nil {
		return err
	}
	if exists(s.SharedEnv()) {
		if err := os.Symlink(s.SharedEnv(), filepath.Join(release, ".env")); err != nil {
			return err
		}
	}
	// The release stays root's (0755): root runs its binary (the CLI links
	// to it, and `sudo pages …` re-executes as root), so the service account
	// may read and execute it but never replace it. What the service writes
	// lives outside releases, in the shared assets directory.
	if err := os.Chmod(release, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(release, install.BinaryName), 0o755); err != nil {
		return err
	}
	if _, err := install.TrustedFile(filepath.Join(release, install.BinaryName)); err != nil {
		return die("release %s is not root's alone: %v", release, err)
	}
	if err := o.Chown(ctx, filepath.Join(release, "assets"), false); err != nil {
		return die("could not give %s to %s", filepath.Join(release, "assets"), s.AppUser)
	}
	if err := o.Chown(ctx, s.SharedAssets(), true); err != nil {
		return die("could not give %s to %s", s.SharedAssets(), s.AppUser)
	}
	if err := install.SymlinkAtomic(release, s.AppDir); err != nil {
		return err
	}
	log.OK("release " + filepath.Base(release) + " active at " + s.AppDir)
	return nil
}

func (r *run) ask(label string) {
	fmt.Fprintf(r.log.Err, "%s?%s %s ", r.log.Cyan(), r.log.Reset(), label)
}

// prompt answers name: an existing value wins, then (non-interactive) the
// default or a stop, then the operator's answer, blank taking the default.
func (r *run) prompt(name, label, def string) (string, error) {
	if v := r.vars[name]; v != "" {
		return v, nil
	}
	if r.nonInt || !r.o.Interactive {
		if def == "" {
			return "", die("non-interactive + missing: set %s", name)
		}
		return def, nil
	}
	if def != "" {
		r.ask(label + " " + r.log.Dim() + "[" + def + "]" + r.log.Reset() + ":")
	} else {
		r.ask(label + ":")
	}
	answer, _ := r.reader.ReadString('\n')
	if answer = strings.TrimSpace(answer); answer == "" {
		answer = def
	}
	return answer, nil
}

var (
	hbaV4 = regexp.MustCompile(`(?m)^(host\s+all\s+all\s+127\.0\.0\.1/32\s+).*$`)
	hbaV6 = regexp.MustCompile(`(?m)^(host\s+all\s+all\s+::1/128\s+).*$`)
	token = regexp.MustCompile(`pgs_[A-Za-z0-9_-]{20,}`)
)

func (r *run) configure(ctx context.Context) error {
	o, s, log := r.o, r.o.Settings, r.log
	// The variables bootstrap reasons about: the caller's environment with
	// the existing service environment over it (as sourcing it did) — read
	// without evaluating it.
	r.vars = map[string]string{}
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			r.vars[k] = o.Getenv(k)
		}
	}
	for _, k := range []string{"PAGES_BOOTSTRAP_NON_INTERACTIVE", "AUTH_SIGNING_PUBKEY", "PAGES_BOOTSTRAP_DASHBOARD_HOST", "PAGES_BOOTSTRAP_CONTENT_HOST",
		"PAGES_BOOTSTRAP_SETUP_CADDY", "PAGES_BOOTSTRAP_USE_LETSENCRYPT", "PAGES_BOOTSTRAP_LE_EMAIL", "DASHBOARD_ORIGIN", "CONTENT_ORIGIN",
		"AUTH_COOKIE_NAME", "AUTH_LOGIN_URL", "ADMIN_EMAIL_DOMAIN", "PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "DATABASE_URL", "PGDATA"} {
		if v := o.Getenv(k); v != "" {
			r.vars[k] = v
		}
	}
	if f, err := envfile.Read(s.EnvFile); err == nil {
		log.Info("found existing " + s.EnvFile + " — re-using values")
		for k, v := range f.Map() {
			r.vars[k] = v
		}
	}
	prevDashboard, prevContent := r.vars["DASHBOARD_HOST"], r.vars["CONTENT_HOST"]

	pub, err := r.prompt("AUTH_SIGNING_PUBKEY", "Auth service public signing key (AUTH_SIGNING_PUBKEY from the auth host)", r.vars["AUTH_SIGNING_PUBKEY"])
	if err != nil {
		return err
	}
	switch {
	case pub == "":
		return die("AUTH_SIGNING_PUBKEY is required — copy it from the auth host. Without it Pages rejects every elcano_auth cookie and bounces all admin requests to login.")
	case !doctor.ValidSigningKey(pub):
		return die("AUTH_SIGNING_PUBKEY must be a base64-encoded 32-byte Ed25519 public key.")
	}
	// Defaults are Elcano's deployment; an operator deploying for someone
	// else answers the prompts (or sets PAGES_BOOTSTRAP_*_HOST).
	dashboard, err := r.prompt("PAGES_BOOTSTRAP_DASHBOARD_HOST", "Dashboard hostname (trusted auth zone)", or(r.vars["DASHBOARD_HOST"], "pages.elcanotek.com"))
	if err != nil {
		return err
	}
	content, err := r.prompt("PAGES_BOOTSTRAP_CONTENT_HOST", "Content hostname (SEPARATE registrable domain, cookieless)", or(r.vars["CONTENT_HOST"], "elcano-pages.com"))
	if err != nil {
		return err
	}
	if content == "" {
		return die("CONTENT_HOST is required — it MUST be a different registrable domain from the dashboard (PLAN.md §7).")
	}
	if psl.SameSite(dashboard, content) {
		log.Warn(fmt.Sprintf("CONTENT_HOST (%s) shares the registrable domain %s with DASHBOARD_HOST (%s).", content, psl.RegistrableDomain(content), dashboard))
		log.Warn("That defeats the isolation model (cookie-tossing). Use a different eTLD+1. Continuing, but fix before production.")
	}
	r.vars["DASHBOARD_HOST"], r.vars["CONTENT_HOST"], r.vars["AUTH_SIGNING_PUBKEY"] = dashboard, content, pub

	// Per-instance secrets: generated once, preserved across re-runs.
	for _, k := range []string{"PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER"} {
		if r.vars[k] == "" {
			v, err := install.RandomHex(32)
			if err != nil {
				return err
			}
			r.vars[k] = v
		}
	}

	log.Info("configuring PostgreSQL")
	pgdata := or(r.vars["PGDATA"], o.path("/var/lib/pgsql/data"))
	if b, err := os.ReadFile(filepath.Join(pgdata, "PG_VERSION")); err != nil || len(b) == 0 {
		if r.cmd(ctx, "postgresql-setup", "--initdb") != nil && r.cmd(ctx, "/usr/bin/postgresql-setup", "--initdb") != nil {
			return die("initdb failed (PGDATA=%s)", pgdata)
		}
		log.OK("initialized PostgreSQL cluster at " + pgdata)
	}
	// Loopback auth → scram-sha-256 (the app connects over 127.0.0.1 with a
	// password). Rewritten in place: keeps the file's owner and mode.
	hba := filepath.Join(pgdata, "pg_hba.conf")
	if b, err := os.ReadFile(hba); err == nil {
		next := hbaV6.ReplaceAllString(hbaV4.ReplaceAllString(string(b), "${1}scram-sha-256"), "${1}scram-sha-256")
		if next != string(b) {
			if err := os.WriteFile(hba, []byte(next), 0o600); err != nil {
				return err
			}
		}
	}
	if r.cmd(ctx, "systemctl", "enable", "--now", "postgresql") != nil {
		if err := r.cmd(ctx, "systemctl", "restart", "postgresql"); err != nil {
			return die("postgresql failed to start (%v) — check: journalctl -u postgresql", err)
		}
	}
	// The reload applies the pg_hba.conf rewrite above; without it the app's
	// password login is refused.
	if err := r.cmd(ctx, "systemctl", "reload", "postgresql"); err != nil {
		return die("systemctl reload postgresql failed (%v) — pg_hba.conf changes not applied", err)
	}
	// Reuse the existing role password (from a prior DATABASE_URL) so re-runs
	// never lock the app out; otherwise mint one.
	pass, err := install.DatabasePassword(r.vars["DATABASE_URL"])
	if err != nil {
		return die("no database password: DATABASE_URL in %s is not a pages URL with a URL-safe password (correct or remove it), or generating one failed; database role left unchanged", s.EnvFile)
	}
	psql := func(sql string) (string, error) {
		// SQL on stdin: a password in argv would be visible in ps.
		return runner.Output(ctx, o.Runner, runner.Cmd{Name: "runuser", Args: []string{"-u", "postgres", "--", "psql", "-v", "ON_ERROR_STOP=1", "-tA"}, Stdin: strings.NewReader(sql)})
	}
	if out, _ := psql("SELECT 1 FROM pg_roles WHERE rolname='pages';"); strings.Contains(out, "1") {
		if _, err := psql("ALTER ROLE pages WITH LOGIN PASSWORD '" + pass + "';"); err != nil {
			return die("ALTER ROLE pages failed")
		}
	} else if _, err := psql("CREATE ROLE pages WITH LOGIN PASSWORD '" + pass + "';"); err != nil {
		return die("CREATE ROLE pages failed")
	}
	if out, _ := psql("SELECT 1 FROM pg_database WHERE datname='pages';"); !strings.Contains(out, "1") {
		if err := r.cmd(ctx, "runuser", "-u", "postgres", "--", "createdb", "-O", "pages", "pages"); err != nil {
			return die("createdb pages failed")
		}
	}
	url := "postgres://pages:" + pass + "@127.0.0.1:5432/pages?sslmode=disable"
	log.OK("database 'pages' ready")

	// Preserve operator overrides while updating the fields bootstrap owns.
	// The complete candidate is validated before atomic replacement;
	// .previous retains the old file. No credential is passed in argv.
	if o := r.vars["DASHBOARD_ORIGIN"]; o == "" || o == "https://"+prevDashboard {
		r.vars["DASHBOARD_ORIGIN"] = "https://" + dashboard
	}
	if o := r.vars["CONTENT_ORIGIN"]; o == "" || o == "https://"+prevContent {
		r.vars["CONTENT_ORIGIN"] = "https://" + content
	}
	r.vars["AUTH_COOKIE_NAME"] = or(r.vars["AUTH_COOKIE_NAME"], "elcano_auth")
	r.vars["AUTH_LOGIN_URL"] = or(r.vars["AUTH_LOGIN_URL"], "https://auth.elcanotek.com")
	r.vars["ADMIN_EMAIL_DOMAIN"] = or(r.vars["ADMIN_EMAIL_DOMAIN"], "elcanotek.com")
	r.vars["DATABASE_URL"], r.vars["PORT"] = url, s.Port
	var updates []envfile.Update
	for _, k := range []string{"PORT", "DASHBOARD_HOST", "CONTENT_HOST", "DASHBOARD_ORIGIN", "CONTENT_ORIGIN", "AUTH_SIGNING_PUBKEY", "AUTH_COOKIE_NAME",
		"AUTH_LOGIN_URL", "ADMIN_EMAIL_DOMAIN", "PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "DATABASE_URL"} {
		updates = append(updates, envfile.Update{Key: k, Value: r.vars[k]})
	}
	if err := os.MkdirAll(filepath.Dir(s.EnvFile), 0o755); err != nil {
		return err
	}
	if err := envfile.Write(ctx, o.Runner, s.EnvFile, updates); err != nil {
		log.Error(err.Error())
		return die("environment generation failed — previous file retained")
	}
	if err := os.Chmod(s.EnvFile, 0o640); err != nil {
		return err
	}
	// The service account reads the 0640 file through its group; without it
	// pages.service cannot start.
	if err := r.cmd(ctx, "chown", "root:"+s.AppUser, s.EnvFile); err != nil {
		return die("could not give %s to group %s: %v", s.EnvFile, s.AppUser, err)
	}
	log.OK("env seeded at " + s.EnvFile)

	log.Info("running database migrations")
	if err := o.Migrate(ctx, url); err != nil {
		log.Error(err.Error())
		return die("migrations failed — check DATABASE_URL and that postgresql is running")
	}
	log.OK("schema migrated")

	// The initial agent API token, on FIRST bootstrap only (no active token).
	// Minted by the installed release as the service account with the
	// service's environment, so its hash uses the pepper the server verifies
	// with.
	switch n := o.ActiveTokens(ctx, url); {
	case n == 0:
		out, _ := r.appOutput(ctx, s.Binary(), "token", "add", "initial")
		if r.initial = token.FindString(out); r.initial != "" {
			log.OK("minted initial agent API token (label 'initial')")
		} else {
			log.Warn("could not mint an initial token — run 'pages token add <label>' after bootstrap")
		}
	case n > 0:
		log.Info(fmt.Sprintf("%d active agent token(s) already exist — mint more with 'pages token add <label>'", n))
	}
	return nil
}

// appOutput runs argv as the service account with the service environment.
func (r *run) appOutput(ctx context.Context, argv ...string) (string, error) {
	s := r.o.Settings
	script := `env_file="$1"; directory="$2"; shift 2
set -a; . "$env_file"; set +a
export PAGES_ENV=production
cd "$directory"
exec "$@"`
	return runner.Output(ctx, r.o.Runner, runner.Cmd{Name: "runuser", Args: append([]string{"-u", s.AppUser, "--", "bash", "-c", script, "_", s.EnvFile, s.AppDir}, argv...)})
}

func (r *run) services(ctx context.Context) error {
	o, s, log := r.o, r.o.Settings, r.log
	if err := install.WriteFileAtomic(s.InstallConfig, []byte(s.RenderConfig()), 0o644); err != nil {
		return err
	}
	unit, err := s.RenderService()
	if err != nil {
		return err
	}
	if err := install.WriteFileAtomic(o.path("/etc/systemd/system/"+install.Service), []byte(unit), 0o644); err != nil {
		return err
	}
	if err := install.SymlinkAtomic(s.Binary(), s.CLITarget); err != nil {
		return err
	}
	// Login banner: deploy/motd is the single source, installed
	// unconditionally so the image's own /etc/motd does not survive.
	// `pages update` keeps it in sync.
	motd, err := os.ReadFile(filepath.Join(s.SrcDir, "deploy", "motd"))
	if err != nil {
		m, merr := install.Motd()
		if merr != nil {
			return merr
		}
		motd = []byte(m)
	}
	if err := install.WriteFileAtomic(o.path("/etc/motd"), motd, 0o644); err != nil {
		return err
	}
	r.removeCockpit(ctx)
	// Key-only SSH: Vultr's cloud-init turns password login on. Never fails.
	sshd.KeyOnly{Root: o.Root, Runner: o.Runner, Log: log, Getenv: o.Getenv}.Apply(ctx)
	hostname.Setter{Root: o.Root, Runner: o.Runner, Log: log, Getenv: o.Getenv}.Set(ctx, r.vars["DASHBOARD_HOST"])
	// A failed daemon-reload leaves systemd on the previous unit: the
	// restart below would run the old ExecStart/WorkingDirectory.
	if err := r.cmd(ctx, "systemctl", "daemon-reload"); err != nil {
		return die("systemctl daemon-reload failed (%v) — %s was not loaded", err, install.Service)
	}
	if err := r.cmd(ctx, "systemctl", "enable", install.Service); err != nil {
		return die("systemctl enable %s failed (%v) — it would not start on boot", install.Service, err)
	}
	if err := r.cmd(ctx, "systemctl", "restart", install.Service); err != nil {
		return die("pages.service failed to start — check: pages logs")
	}
	for i := 0; i < 15; i++ {
		if o.Probe(ctx, "http://127.0.0.1:"+s.Port+"/readyz") {
			log.OK("/readyz → 200 (server up)")
			return nil
		}
		o.Sleep(time.Second)
	}
	return die("pages did not become ready — check: pages logs")
}

// removeCockpit uninstalls Cockpit: the Vultr Fedora image ships its
// root-capable web login on :9090, and these boxes are administered over ssh
// only. PAGES_KEEP_COCKPIT=1 keeps it; a failure only warns.
func (r *run) removeCockpit(ctx context.Context) {
	if r.o.Getenv("PAGES_KEEP_COCKPIT") == "1" {
		r.log.Info("PAGES_KEEP_COCKPIT=1: leaving Cockpit installed")
		return
	}
	if !runner.Have(r.o.Runner, "rpm") || r.cmd(ctx, "rpm", "-q", "cockpit-ws") != nil {
		return
	}
	if r.cmd(ctx, "dnf", "-y", "remove", "cockpit*") == nil {
		r.log.OK("Cockpit removed")
	} else {
		r.log.Warn("could not remove Cockpit (dnf -y remove 'cockpit*'); continuing")
	}
}

var (
	importLine = regexp.MustCompile(`(?m)^\s*import\s+conf\.d/`)
	emailLine  = regexp.MustCompile(`(?m)^\s*email\s`)
)

func yes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes":
		return true
	}
	return false
}

func (r *run) caddy(ctx context.Context) error {
	o, s, log := r.o, r.o.Settings, r.log
	dashboard, content := r.vars["DASHBOARD_HOST"], r.vars["CONTENT_HOST"]
	ans, err := r.prompt("PAGES_BOOTSTRAP_SETUP_CADDY", fmt.Sprintf("Set up Caddy + auto-TLS for %s and %s? (Y/n)", dashboard, content), "Y")
	if err != nil {
		return err
	}
	if !yes(ans) {
		log.Info("skipping Caddy — reach pages at http://127.0.0.1:" + s.Port + " (front with your own proxy)")
		return nil
	}
	le, err := r.prompt("PAGES_BOOTSTRAP_USE_LETSENCRYPT", "Use Let's Encrypt (needs public 80/443 + DNS for both names)? (Y/n)", "Y")
	if err != nil {
		return err
	}
	useLE := yes(le)
	email := ""
	if useLE {
		if email, err = r.prompt("PAGES_BOOTSTRAP_LE_EMAIL", "LE contact email (blank to skip)", r.vars["PAGES_BOOTSTRAP_LE_EMAIL"]); err != nil {
			return err
		}
	}
	if err := r.cmd(ctx, "dnf", "install", "-y", "caddy"); err != nil {
		return die("dnf install caddy failed")
	}
	caddyDir := o.path("/etc/caddy")
	if err := os.MkdirAll(filepath.Join(caddyDir, "conf.d"), 0o755); err != nil {
		return err
	}
	caddyfile := filepath.Join(caddyDir, "Caddyfile")
	current, _ := os.ReadFile(caddyfile)
	if !importLine.Match(current) {
		current = append(current, []byte("\n# Managed by Elcano service bootstraps\nimport conf.d/*.caddy\n")...)
	}
	if email != "" && !emailLine.Match(current) {
		current = append([]byte(fmt.Sprintf("{\n\temail %s\n}\n\n", email)), current...)
	}
	if err := install.WriteFileAtomic(caddyfile, current, 0o644); err != nil {
		return err
	}
	site, err := s.RenderCaddy(dashboard, content, !useLE)
	if err != nil {
		return die("%v", err)
	}
	if err := install.WriteFileAtomic(filepath.Join(caddyDir, "conf.d", "pages.caddy"), []byte(site), 0o644); err != nil {
		return err
	}
	if r.cmd(ctx, "systemctl", "is-active", "--quiet", "firewalld") == nil {
		_ = r.cmd(ctx, "firewall-cmd", "--add-service=http", "--permanent")
		_ = r.cmd(ctx, "firewall-cmd", "--add-service=https", "--permanent")
		_ = r.cmd(ctx, "firewall-cmd", "--reload")
		log.OK("firewalld: http + https opened")
	}
	_ = r.cmd(ctx, "systemctl", "enable", "caddy")
	if r.cmd(ctx, "systemctl", "is-active", "--quiet", "caddy") == nil {
		if err := r.cmd(ctx, "systemctl", "reload", "caddy"); err != nil {
			return die("caddy reload failed")
		}
	} else if err := r.cmd(ctx, "systemctl", "start", "caddy"); err != nil {
		return die("caddy failed to start")
	}
	log.OK("Caddy running for both hostnames (auto-renew, no cron needed)")
	return nil
}

func (r *run) summary() {
	log := r.log
	b, d, z, g := log.Bold(), log.Dim(), log.Reset(), log.Green()
	log.Say("")
	log.Say(g + "═══════════════════════════════════════════════" + z)
	log.Say(b + " ✓ Elcano Pages installed" + z)
	log.Say(g + "═══════════════════════════════════════════════" + z)
	log.Say("")
	log.Say("  Dashboard   " + b + "https://" + r.vars["DASHBOARD_HOST"] + z + "   " + d + "(staff: /admin/<slug>, clients: /view/<slug>)" + z)
	log.Say("  Content     " + b + "https://" + r.vars["CONTENT_HOST"] + z + "   " + d + "(cookieless render zone)" + z)
	log.Say("  CLI         " + d + "pages start|stop|restart|status|update|env|tls|token|doctor" + z)
	log.Say("  Logs        " + d + "pages logs" + z)
	if r.initial != "" {
		log.Say("")
		log.Say("  Agent token " + b + r.initial + z)
		log.Say("              " + d + "(label 'initial' — shown ONCE, not recoverable; set it as")
		log.Say("               PAGES_MCP_TOKEN / PAGES_API_TOKEN, or mint per-agent tokens")
		log.Say("               with 'pages token add <label>' and revoke this one)" + z)
	}
	log.Say("")
	log.Say("  " + d + "Login is owned by " + r.vars["AUTH_LOGIN_URL"] + "; @" + r.vars["ADMIN_EMAIL_DOMAIN"] + " auto-admins." + z)
	log.Say("  " + d + "Health: pages doctor --check   Update: PAGES_UPDATE_YES=1 pages update" + z)
	log.Say("")
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func migrateURL(ctx context.Context, url string) error {
	d, err := db.Open(url, db.BoundsFromEnv())
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = migrate.Run(ctx, d.Pool, pages.Migrations(), migrate.Options{LockTimeout: 30 * time.Second, Log: io.Discard})
	return err
}

func activeTokens(ctx context.Context, url string) int {
	d, err := db.Open(url, db.BoundsFromEnv())
	if err != nil {
		return -1
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(ctx, "SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL").Scan(&n); err != nil {
		return -1
	}
	return n
}

func readyProbe(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// ErrNotRoot is returned when bootstrap runs without root.
var ErrNotRoot = errors.New("run as root: sudo bash scripts/bootstrap.sh")
