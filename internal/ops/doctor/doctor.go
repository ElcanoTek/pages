// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package doctor is `pages doctor`: it walks every box-level prerequisite
// Pages depends on and reports PASS/WARN/FAIL with the remedy; run as root it
// also makes the safe repairs (environment file ownership/mode, starting a
// stopped service, restarting a stale release). --check reports without
// touching anything (the CLI runs it as the service user, not root). What it
// never does: git pull or fetch, dnf upgrade, migrations or rebuilds — doctor
// makes the box readable; `pages update` lands the code.
//
// Doctor never sources or executes a file it inspects: the environment file
// holds every credential and the repair path exists precisely for when its
// permissions have gone wrong, so values are read with the non-evaluating
// envfile parser and are never printed.
package doctor

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/psl"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/sshd"
	"github.com/ElcanoTek/pages/internal/ops/term"
	"github.com/ElcanoTek/pages/internal/version"
)

// CaddySnippet is the Pages site-block file.
const CaddySnippet = "/etc/caddy/conf.d/pages.caddy"

// Caddyfile is Caddy's main configuration.
const Caddyfile = "/etc/caddy/Caddyfile"

// Options configures a run. Zero hooks take production defaults.
type Options struct {
	Settings install.Settings
	Runner   runner.Runner
	Log      *term.Printer

	Check, NoRestart, DryRun, Strict bool

	// CaddySnippet and Caddyfile locate the proxy configuration.
	CaddySnippet, Caddyfile string
	// OSRelease is /etc/os-release (PAGES_OS_RELEASE).
	OSRelease string
	// EUID is the effective uid; Username the account name.
	EUID     int
	Username string
	// Self is the running executable.
	Self string

	// StatOwner returns "<octal mode> <user>:<group>" (stat -c '%a %U:%G').
	StatOwner func(path string) (string, error)
	// Readable reports whether path can be opened for reading.
	Readable func(path string) bool
	// RepairEnv sets path to 0640 root:<group>.
	RepairEnv func(path, group string) error
	// ProcCwd resolves a process's working directory.
	ProcCwd func(pid int) (string, error)
	// FreeKiB is the space available to unprivileged users under path.
	FreeKiB func(path string) (uint64, error)
	// Probe GETs url and reports a 2xx answer.
	Probe func(ctx context.Context, url string) bool
	// Certificate verifies https://host (chain + hostname) and returns the
	// leaf's expiry.
	Certificate func(ctx context.Context, host string) (time.Time, error)
	// FedoraLatest is the newest Fedora stable release.
	FedoraLatest func(ctx context.Context) (int, error)
	// Kernel is the running kernel release (uname -r).
	Kernel func() string
	Sleep  func(time.Duration)
	Now    func() time.Time
}

func (o *Options) defaults() {
	if o.Runner == nil {
		o.Runner = runner.Default
	}
	if o.Log == nil {
		o.Log = term.New()
	}
	if o.CaddySnippet == "" {
		o.CaddySnippet = CaddySnippet
	}
	if o.Caddyfile == "" {
		o.Caddyfile = Caddyfile
	}
	if o.OSRelease == "" {
		o.OSRelease = "/etc/os-release"
	}
	if o.Username == "" {
		if u, err := user.Current(); err == nil {
			o.Username = u.Username
		} else {
			o.Username = strconv.Itoa(o.EUID)
		}
	}
	if o.StatOwner == nil {
		o.StatOwner = statOwner
	}
	if o.Readable == nil {
		o.Readable = readable
	}
	if o.RepairEnv == nil {
		o.RepairEnv = repairEnv
	}
	if o.ProcCwd == nil {
		o.ProcCwd = func(pid int) (string, error) { return filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/cwd", pid)) }
	}
	if o.FreeKiB == nil {
		o.FreeKiB = freeKiB
	}
	if o.Probe == nil {
		o.Probe = httpProbe
	}
	if o.Certificate == nil {
		o.Certificate = certificate
	}
	if o.FedoraLatest == nil {
		o.FedoraLatest = fedoraLatest
	}
	if o.Kernel == nil {
		o.Kernel = func() string {
			b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
			return strings.TrimSpace(string(b))
		}
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

type report struct {
	log                    *term.Printer
	ok, fixed, warn, fails int
}

func (r *report) pass(s string)   { r.log.Mark("✓", term.MarkGreen, s); r.ok++ }
func (r *report) repair(s string) { r.log.Mark("↻", term.MarkCyan, s); r.fixed++ }
func (r *report) advise(s string) { r.log.Mark("!", term.MarkYellow, s); r.warn++ }
func (r *report) fail(s string)   { r.log.Mark("✗", term.MarkRed, s); r.fails++ }

// Usage is `pages doctor --help`.
const Usage = `pages doctor — diagnose this Pages box

USAGE
  sudo pages doctor               diagnose; fix what is safe (env perms, start, restart)
  pages doctor --check            diagnose only, change nothing, exit 1 on any failure
  sudo pages doctor --no-restart  fix but never restart the service
  pages doctor --dry-run          print the checklist; touch nothing (no root needed)
  pages doctor --strict           warnings also exit 1

Read-only modes run as the service user (root drops to it), not root; the
plain command needs root because repairs need it.

Checks: the pages binary (the active release's executable and the CLI link to
it), the environment file (presence, 0640 root:pages, required keys — values
are never printed), the DASHBOARD_HOST/CONTENT_HOST eTLD+1 split, PostgreSQL,
the systemd service (active, enabled, running the current release), /readyz,
Caddy + chain and hostname verified TLS for BOTH hostnames, free disk, the OS
support window, pending dnf updates, a pending reboot, Fedora vs latest
stable, and the source checkout (clean, on main, compared against origin via
ls-remote — no fetch, the checkout is never mutated).
Repairs are limited to: env file ownership/mode, starting a stopped service,
restarting a stale release. Doctor never pulls, fetches, upgrades or
rebuilds — that stays ` + "`pages update`" + `.
`

// Run diagnoses the box and returns the exit code (0 healthy or repaired,
// 1 problems remain).
func Run(ctx context.Context, o Options) int {
	o.defaults()
	s := o.Settings
	log := o.Log
	if o.DryRun {
		log.Step(fmt.Sprintf("pages doctor --dry-run (app=%s, src=%s, service=%s)", s.AppDir, s.SrcDir, install.Service))
		for _, line := range []string{
			"Runtime: " + s.Binary() + " present and executable; " + s.CLITarget + " links to it; go and make present for pages update",
			"Configuration: " + s.EnvFile + " exists, mode 0640 root:" + s.AppUser + ", with AUTH_SIGNING_PUBKEY (base64 32-byte), PAGE_COOKIE_SECRET, RAW_TOKEN_SECRET, API_TOKEN_PEPPER, DATABASE_URL, DASHBOARD_HOST and CONTENT_HOST set; content host on a DIFFERENT registrable domain (eTLD+1) than the dashboard host",
			"Database: postgresql.service active; psql probes DATABASE_URL as " + s.AppUser,
			"Service: " + install.Service + " active + enabled; the running release matches " + s.AppDir + " (stale releases restart, unless --no-restart); filesystem repairs happen before any unit start",
			"Readiness: /readyz → 200 on :" + s.Port + " (15 tries after a repair start/restart)",
			"Caddy/TLS: caddy active, configuration valid, certificates for BOTH hostnames chain + hostname verified and more than 14 days from expiry",
			"Disk: at least 1 GiB free on " + s.AppDir,
			"Host: OS support window, pending dnf updates, reboot-needed kernel check, Fedora vs latest stable; as root, sshd -T must not accept passwords",
			"Source: checkout clean, on main, compared with origin via git ls-remote (read-only — no fetch)",
		} {
			log.Info("[dry-run] " + line)
		}
		return 0
	}
	if !exists(s.EnvFile) && !exists(filepath.Join(s.SrcDir, ".git")) && !lexists(s.AppDir) {
		log.Error(fmt.Sprintf("no Pages install at %s and no checkout at %s (run scripts/bootstrap.sh first)", s.AppDir, s.SrcDir))
		return 1
	}
	r := &report{log: log}
	d := &run{o: &o, r: r}
	d.runtime()
	d.configuration()
	d.database(ctx)
	d.service(ctx)
	d.readiness(ctx)
	d.caddy(ctx)
	d.disk()
	d.host(ctx)
	d.source(ctx)

	log.Say("")
	problems := r.fails
	if o.Strict {
		problems += r.warn
	}
	switch {
	case problems > 0:
		log.Mark("✗", term.MarkRed, fmt.Sprintf("doctor: %d ok, %d fixed, %d advisories, %d PROBLEM(S)", r.ok, r.fixed, r.warn, problems))
		return 1
	case r.fixed > 0:
		log.Mark("✓", term.MarkGreen, fmt.Sprintf("doctor: %d ok, %d fixed, %d advisories — box repaired", r.ok, r.fixed, r.warn))
	default:
		log.Mark("✓", term.MarkGreen, fmt.Sprintf("doctor: %d ok, %d advisories — box healthy", r.ok, r.warn))
	}
	return 0
}

type run struct {
	o           *Options
	r           *report
	env         *envfile.File
	envReadable bool
	dashboard   string
	content     string
	restarted   bool
}

func (d *run) runtime() {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Runtime")
	r.pass(fmt.Sprintf("pages %s (%s)", version.Version, version.Revision()))
	bin := s.Binary()
	if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
		r.pass("active release binary " + bin + " present")
		if cli, err := filepath.EvalSymlinks(s.CLITarget); err == nil {
			if resolved, err := filepath.EvalSymlinks(bin); err == nil && cli == resolved {
				r.pass(s.CLITarget + " is the active release binary")
			} else {
				r.advise(s.CLITarget + " is not the active release binary — rerun scripts/bootstrap.sh or sudo pages update")
			}
		} else {
			r.advise(s.CLITarget + " missing — rerun scripts/bootstrap.sh")
		}
	} else {
		r.fail("no executable " + bin + " — the active release is not a Go release; run sudo pages update")
	}
	var missing []string
	for _, tool := range []string{"go", "make", "git"} {
		if !runner.Have(o.Runner, tool) {
			missing = append(missing, tool)
		}
	}
	if len(missing) == 0 {
		r.pass("go, make and git present (pages update builds from the checkout)")
	} else {
		r.advise(strings.Join(missing, ", ") + " missing — pages update cannot build; rerun scripts/bootstrap.sh")
	}
}

func (d *run) configuration() {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Configuration (" + s.EnvFile + ")")
	d.envReadable = true
	switch {
	case !exists(s.EnvFile):
		r.fail(s.EnvFile + " missing — run scripts/bootstrap.sh")
		return
	case !o.Readable(s.EnvFile):
		if o.EUID == 0 {
			r.fail(s.EnvFile + " unreadable even as root — inspect its ownership and parent directories")
		}
		// The read-only run (service user) cannot read a root-only file
		// (e.g. one tightened to 0600 root:root). Report the limited
		// validation instead of cascading false 'key unset' failures.
		r.advise(s.EnvFile + " is not readable as " + o.Username + " — key, hostname and database checks are limited; run sudo pages doctor for full validation")
		d.envReadable = false
		return
	}
	if lexists(s.EnvFile) && isSymlink(s.EnvFile) {
		// Privileged chown/chmod follows symlinks; refuse rather than repair
		// through one — a symlinked credential file is drift worth an operator.
		r.fail(s.EnvFile + " is a symlink — refusing to touch it; investigate and replace with a regular file")
	} else {
		perms, err := o.StatOwner(s.EnvFile)
		if err != nil {
			perms = "unknown"
		}
		want := "640 root:" + s.AppUser
		switch {
		case perms == want:
			r.pass(s.EnvFile + " is " + perms)
		case perms == "600 root:root":
			// Stricter than the shipped state: never downgrade it. CLI verbs
			// that read this file as the service user will fail, so warn.
			r.pass(s.EnvFile + " is " + perms + " (stricter than " + want + ")")
			r.advise("pages CLI commands that read " + s.EnvFile + " as " + s.AppUser + " will fail while it is root-only — set " + want + " if you use them")
		case o.Check:
			r.fail(s.EnvFile + " is " + perms + " — want " + want + " (it holds every credential)")
		case o.RepairEnv(s.EnvFile, s.AppUser) == nil:
			r.repair(s.EnvFile + " set to " + want)
		default:
			r.fail("could not set " + s.EnvFile + " to " + want + " — fix ownership/mode by hand")
		}
	}
	env, err := envfile.Read(s.EnvFile)
	if err != nil {
		env = &envfile.File{}
	}
	d.env = env
	for _, key := range []string{"AUTH_SIGNING_PUBKEY", "PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "DATABASE_URL", "DASHBOARD_HOST", "CONTENT_HOST"} {
		if env.Value(key) != "" {
			r.pass(key + " set")
		} else {
			r.fail(key + " unset in " + s.EnvFile)
		}
	}
	if pub := env.Value("AUTH_SIGNING_PUBKEY"); pub != "" {
		if ValidSigningKey(pub) {
			r.pass("AUTH_SIGNING_PUBKEY is a base64 32-byte Ed25519 key")
		} else {
			r.fail("AUTH_SIGNING_PUBKEY is not a base64 32-byte Ed25519 key — copy the auth service's key (printed by its bootstrap)")
		}
	}
	// The trust split is a security boundary, not a preference (PLAN.md §7):
	// compare eTLD+1, not first-label-stripped parents — us.example.com vs
	// eu.example.com is NOT a split even though the naive check passes.
	d.dashboard, d.content = env.Value("DASHBOARD_HOST"), env.Value("CONTENT_HOST")
	if d.dashboard != "" && d.content != "" {
		dreg, creg := psl.RegistrableDomain(d.dashboard), psl.RegistrableDomain(d.content)
		if creg == dreg {
			r.fail(fmt.Sprintf("CONTENT_HOST (%s) shares the registrable domain %s with DASHBOARD_HOST (%s) — agent HTML can toss cookies onto the trusted host (PLAN.md §7)", d.content, creg, d.dashboard))
		} else {
			r.pass(fmt.Sprintf("content host registrable domain (%s) differs from the dashboard's (%s)", creg, dreg))
		}
	}
}

// ValidSigningKey reports whether v is a base64 32-byte Ed25519 public key.
// The whole value must decode: a decoder that stops at a bad character must
// not turn "valid32bytes!" into a pass.
func ValidSigningKey(v string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
			return true
		}
	}
	return false
}

// SplitDatabaseURL separates the userinfo password from a connection URL:
// it returns the password and the URL without it. The password must never
// reach psql's argv (world-readable via ps and /proc/<pid>/cmdline); it
// travels in PGPASSWORD, readable only to the owner and root.
func SplitDatabaseURL(url string) (password, redacted string) {
	if !strings.Contains(url, "@") {
		return "", url
	}
	userinfo := url
	if i := strings.Index(userinfo, "://"); i >= 0 {
		userinfo = userinfo[i+3:]
	}
	if i := strings.Index(userinfo, "@"); i >= 0 {
		userinfo = userinfo[:i]
	}
	i := strings.Index(userinfo, ":")
	if i < 0 {
		return "", url
	}
	return userinfo[i+1:], strings.Replace(url, userinfo+"@", userinfo[:i]+"@", 1)
}

func (d *run) database(ctx context.Context) {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Database")
	if runner.Have(o.Runner, "systemctl") {
		if d.systemctlQuiet(ctx, "is-active", "postgresql.service") {
			r.pass("postgresql.service active")
		} else {
			r.fail("postgresql.service not active — pages.service Requires it: systemctl status postgresql")
		}
	}
	url := d.env.Value("DATABASE_URL")
	if url == "" {
		return
	}
	if !runner.Have(o.Runner, "psql") {
		r.advise("psql missing — skipping the direct database probe (/readyz below still covers schema)")
		return
	}
	password, arg := SplitDatabaseURL(url)
	env := append(os.Environ(), "PGPASSWORD="+password)
	c := runner.Cmd{Name: "psql", Args: []string{arg, "-tAc", "SELECT 1"}, Env: env}
	if o.EUID == 0 {
		c = runner.Cmd{Name: "runuser", Args: append([]string{"-u", s.AppUser, "--", "psql"}, c.Args...), Env: env}
	}
	out, err := runner.Output(ctx, o.Runner, c)
	switch {
	case err == nil && strings.Contains(out, "1"):
		r.pass("database accepts the '" + s.AppUser + "' role (DATABASE_URL)")
	case o.EUID == 0:
		r.fail("database probe failed — check DATABASE_URL in " + s.EnvFile + " and pg_hba.conf")
	default:
		// The read-only run probes as the service user, so a failed probe
		// means the configured URL is unusable even though the still-running
		// service may be healthy on its old environment — the next restart
		// is what breaks. That is a failure, not a rerun-with-root hint.
		r.fail("database probe failed — DATABASE_URL in " + s.EnvFile + " is not usable as " + o.Username + "; the running service may still be on its old environment, but the next restart will take Pages down")
	}
}

func (d *run) systemctlQuiet(ctx context.Context, args ...string) bool {
	args = append([]string{args[0], "--quiet"}, args[1:]...)
	return runner.OK(ctx, d.o.Runner, runner.Cmd{Name: "systemctl", Args: args})
}

func (d *run) service(ctx context.Context) {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Service")
	if !runner.Have(o.Runner, "systemctl") {
		r.advise("no systemd on this host — skipping service checks (a dev box, or an unusual supervisor)")
		return
	}
	switch {
	case d.systemctlQuiet(ctx, "is-active", install.Service):
		r.pass(install.Service + " active")
	case o.Check:
		r.fail(install.Service + " not active — inspect: pages logs")
	case runner.OK(ctx, o.Runner, runner.Cmd{Name: "systemctl", Args: []string{"start", install.Service}}) && d.systemctlQuiet(ctx, "is-active", install.Service):
		r.repair("started " + install.Service)
		// Readiness polls like after a restart: Type=simple means `start`
		// can return before the port is listening.
		d.restarted = true
	default:
		r.fail(install.Service + " failed to start — inspect: pages logs")
	}
	if d.systemctlQuiet(ctx, "is-enabled", install.Service) {
		r.pass(install.Service + " enabled")
	} else {
		r.advise(install.Service + " not enabled — it will not start on boot: systemctl enable pages")
	}
	// A release switch is only complete once the service restarts onto it;
	// a box that skipped that keeps serving the old release while every file
	// on disk claims the new one. Compare the process cwd (the release dir)
	// against the APP_DIR symlink target. Invariant: every filesystem repair
	// above runs BEFORE any unit start.
	out, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "systemctl", Args: []string{"show", "-p", "MainPID", "--value", install.Service}})
	pid, err := strconv.Atoi(out)
	if err != nil || pid <= 0 {
		return
	}
	if !isSymlink(s.AppDir) {
		r.pass(s.AppDir + " is a legacy (non-release) install — nothing to compare")
		return
	}
	running, err1 := o.ProcCwd(pid)
	current, err2 := filepath.EvalSymlinks(s.AppDir)
	if err1 != nil || err2 != nil || running == "" || current == "" {
		r.advise(fmt.Sprintf("cannot read the service's working directory as uid %d — re-run with sudo to compare the running release", o.EUID))
		return
	}
	switch {
	case running == current:
		r.pass("running release matches " + s.AppDir)
	case o.Check:
		r.fail("service still runs " + running + " while " + s.AppDir + " points at " + current + " — the last release switch never restarted: sudo systemctl restart pages.service")
	case o.NoRestart:
		r.advise("service runs the previous release — apply it: sudo pages restart")
	case runner.OK(ctx, o.Runner, runner.Cmd{Name: "systemctl", Args: []string{"restart", install.Service}}):
		r.repair("restarted " + install.Service + " onto the current release")
		d.restarted = true
	default:
		r.fail("restart failed — inspect: pages logs")
	}
}

func (d *run) readiness(ctx context.Context) {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Readiness")
	port := s.Port
	tries := 1
	if d.restarted {
		tries = 15
	}
	healthy := false
	for i := 0; i < tries; i++ {
		if o.Probe(ctx, "http://127.0.0.1:"+port+"/readyz") {
			healthy = true
			break
		}
		if i < tries-1 {
			o.Sleep(time.Second)
		}
	}
	switch {
	case healthy:
		r.pass("/readyz → 200 (database + migrations ready)")
	case !d.envReadable && !s.PortExplicit:
		r.fail("/readyz not ready on :" + port + " — inspect: pages logs (PORT is the built-in default; " + s.EnvFile + " was unreadable, so the real port is unknown — run sudo pages doctor)")
	default:
		r.fail("/readyz not ready on :" + port + " — inspect: pages logs")
	}
}

var tlsInternal = regexp.MustCompile(`(?m)^\s*tls\s+internal(\s|$)`)

func (d *run) caddy(ctx context.Context) {
	o, r := d.o, d.r
	o.Log.Step("Caddy / TLS")
	if !runner.Have(o.Runner, "caddy") {
		r.advise("caddy not installed — fronting the service is the operator's proxy's job; skipping TLS checks")
		return
	}
	if st, err := os.Stat(o.CaddySnippet); err != nil || !st.Mode().IsRegular() {
		r.advise("no " + o.CaddySnippet + " — Caddy runs without a Pages site block; rerun bootstrap to set up TLS")
		return
	}
	snippet, _ := os.ReadFile(o.CaddySnippet)
	v, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "caddy", Args: []string{"version"}})
	if v = strings.SplitN(v, "\n", 2)[0]; v == "" {
		v = "caddy present"
	}
	r.pass(v)
	if d.systemctlQuiet(ctx, "is-active", "caddy.service") {
		r.pass("caddy.service active")
	} else {
		r.fail("caddy.service inactive — inspect: journalctl -u caddy")
	}
	switch {
	case !exists(o.Caddyfile):
		r.fail(o.Caddyfile + " missing — caddy cannot serve the " + o.CaddySnippet + " site block")
	case runner.OK(ctx, o.Runner, runner.Cmd{Name: "caddy", Args: []string{"validate", "--config", o.Caddyfile}}):
		r.pass("Caddy configuration valid")
	default:
		r.fail("Caddy configuration invalid — run: caddy validate --config " + o.Caddyfile)
	}
	if d.dashboard == "" && d.content == "" {
		r.advise("no hostnames in " + d.o.Settings.EnvFile + " — skipping certificate probes")
	}
	for _, host := range []string{d.dashboard, d.content} {
		if host == "" {
			continue
		}
		// Chain and hostname verification gate the expiry read: a
		// self-signed, untrusted or wrong-host certificate never yields one.
		// The one supported deployment that is intentionally not publicly
		// trusted is a bootstrap with Let's Encrypt declined (`tls internal`):
		// that one downgrades to an advisory; every other failure is FAIL.
		end, err := o.Certificate(ctx, host)
		if err != nil {
			if tlsInternal.Match(snippet) {
				r.advise("https://" + host + " does not verify against public trust (Caddy serves 'tls internal') — expected for this self-signed install; browsers will warn")
			} else {
				r.fail("no verifiable certificate for https://" + host + " (chain or hostname verification failed) — check DNS, firewalld and: journalctl -u caddy")
			}
			continue
		}
		enddate := end.UTC().Format("Jan _2 15:04:05 2006 GMT")
		days := int(end.Sub(o.Now()).Hours() / 24)
		switch {
		case end.Before(o.Now()):
			r.fail("certificate for " + host + " expired on " + enddate)
		case days < 14:
			r.advise(fmt.Sprintf("certificate for %s expires in %d day(s) (%s)", host, days, enddate))
		default:
			r.pass(fmt.Sprintf("https://%s — certificate verified (chain + hostname), %d days left (%s)", host, days, enddate))
		}
	}
}

func (d *run) disk() {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Disk")
	if st, err := os.Stat(s.AppDir); err != nil || !st.IsDir() {
		r.fail("application directory missing: " + s.AppDir + " — restore the install or rerun scripts/bootstrap.sh")
		return
	}
	free, err := o.FreeKiB(s.AppDir)
	switch {
	case err != nil:
		r.fail("cannot read free space for " + s.AppDir + " — inspect its filesystem/mount")
	case free >= 1048576:
		r.pass("at least 1 GiB free on " + s.AppDir)
	default:
		r.fail("less than 1 GiB free on " + s.AppDir + " — reclaim space (updates keep predecessors under " + s.ReleaseRoot() + ")")
	}
}

var fedoraVersion = regexp.MustCompile(`^[0-9]+$`)

func (d *run) host(ctx context.Context) {
	o, r := d.o, d.r
	o.Log.Step("Host packages")
	osr, err := envfile.Read(o.OSRelease)
	if err != nil {
		osr = &envfile.File{}
	}
	if end := osr.Value("SUPPORT_END"); end != "" {
		if t, err := time.ParseInLocation("2006-01-02", end, time.Local); err == nil {
			days := int(t.Sub(o.Now()).Hours() / 24)
			if days < 30 {
				r.fail(fmt.Sprintf("OS support ends %s (%d days) — schedule a distro upgrade", end, days))
			} else {
				r.pass(fmt.Sprintf("OS support ends %s (%d days)", end, days))
			}
		} else {
			r.advise("OS support end '" + end + "' is not parseable — verify the vendor lifecycle")
		}
	} else {
		r.advise("OS does not publish SUPPORT_END — verify the vendor lifecycle")
	}
	if runner.Have(o.Runner, "dnf") {
		// `dnf check-update` exits 100 when updates are available; that is a
		// report, not an error. Updating stays the operator's `dnf upgrade`.
		// Under `--check`, doctor runs as the service user whose HOME is
		// /opt/pages (root-owned 0755), so dnf5 gets a writable scratch home
		// instead of failing on mkdir ~/.local/state.
		var env []string
		if scratch, err := os.MkdirTemp("", "pages-dnf-"); err == nil {
			defer os.RemoveAll(scratch)
			env = append(os.Environ(),
				"HOME="+scratch,
				"XDG_STATE_HOME="+filepath.Join(scratch, "state"),
				"XDG_CACHE_HOME="+filepath.Join(scratch, "cache"),
			)
		}
		_, err := o.Runner.Run(ctx, runner.Cmd{Name: "dnf", Args: []string{"check-update"}, Env: env})
		switch code := runner.ExitCode(err); code {
		case 0:
			r.pass("dnf packages current")
		case 100:
			r.advise("dnf updates pending — run: sudo dnf upgrade --refresh")
		default:
			r.advise(fmt.Sprintf("dnf check-update failed (rc=%d) — inspect repos and network", code))
		}
	} else {
		r.advise("no dnf on this host — keep packages current with your package manager")
	}
	if runner.Have(o.Runner, "rpm") {
		// Reboot-needed without dnf-utils: the running kernel is older than
		// the newest installed one.
		out, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "rpm", Args: []string{"-q", "kernel", "--qf", "%{VERSION}-%{RELEASE}.%{ARCH}\n"}})
		if latest := newestVersion(strings.Fields(out)); latest != "" {
			if running := o.Kernel(); latest != running {
				r.advise("reboot pending: running kernel " + running + ", installed " + latest)
			} else {
				r.pass("running kernel matches the newest installed")
			}
		}
	}
	// Read-only: bootstrap makes SSH key-only; this only reports a box where
	// passwords still work. `sshd -T` needs root to read the host keys.
	if o.EUID == 0 && runner.Have(o.Runner, "sshd") {
		out, _ := runner.Output(ctx, o.Runner, runner.Cmd{Name: "sshd", Args: []string{"-T"}})
		if sshd.AcceptsPasswords(out) {
			r.advise("sshd accepts passwords — see production-support README, Access: SSH keys")
		}
	}
	if osr.Value("ID") == "fedora" && fedoraVersion.MatchString(osr.Value("VERSION_ID")) {
		current, _ := strconv.Atoi(osr.Value("VERSION_ID"))
		latest, err := o.FedoraLatest(ctx)
		switch {
		case err != nil || latest <= 0:
			r.advise("could not read the Fedora release feed — skipping the version check")
		case latest <= current:
			r.pass(fmt.Sprintf("Fedora %d (latest stable: %d)", current, latest))
		default:
			r.advise(fmt.Sprintf("Fedora %d is behind latest stable (%d) — plan a system upgrade (two releases per hop maximum)", current, latest))
		}
	}
}

func (d *run) source(ctx context.Context) {
	o, s, r := d.o, d.o.Settings, d.r
	o.Log.Step("Source checkout")
	if !runner.Have(o.Runner, "git") {
		r.advise("git missing — cannot inspect the source checkout")
		return
	}
	if st, err := os.Stat(filepath.Join(s.SrcDir, ".git")); err != nil || !st.IsDir() {
		r.advise("no git checkout at " + s.SrcDir + " — 'pages update' will not work on this box")
		return
	}
	// safe.directory because the checkout is root-owned while the read-only
	// run is the service user; core.fsmonitor=false keeps a tampered
	// checkout's config from running a command as this user.
	git := func(ctx context.Context, args ...string) (string, error) {
		base := []string{"-c", "safe.directory=" + s.SrcDir, "-c", "core.fsmonitor=false", "-C", s.SrcDir}
		res, err := o.Runner.Run(ctx, runner.Cmd{Name: "git", Args: append(base, args...), Env: append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")})
		return res.Stdout, err
	}
	branch, err := git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)
	if err != nil || branch == "" {
		branch = "?"
	}
	if branch == "main" {
		r.pass("checkout on main")
	} else {
		r.advise("checkout is on '" + branch + "', not main — 'pages update' follows the checked-out branch")
	}
	switch dirty, err := git(ctx, "status", "--porcelain"); {
	case err != nil:
		// A failed status is not a clean checkout — update cannot use it.
		r.advise("could not read checkout status (git error) — inspect: git -C " + s.SrcDir + " status")
	case strings.TrimSpace(dirty) == "":
		r.pass("checkout clean")
	default:
		r.advise("checkout has local changes — resolve them before pages update")
	}
	// Freshness WITHOUT mutating the checkout: fetch writes objects, refs and
	// FETCH_HEAD, which even a diagnostic must not do. ls-remote reads the
	// remote tip only; a failed or blocked probe reads as unknown.
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := git(lctx, "ls-remote", "origin", "main")
	cancel()
	remote := ""
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/main" {
				remote = f[0]
				break
			}
		}
	}
	if remote == "" {
		r.advise("could not reach origin — remote freshness unknown (network, auth, or timeout)")
		return
	}
	head, _ := git(ctx, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); head != "" && head == remote {
		r.pass("checkout current with origin/main")
		return
	}
	behind, err := git(ctx, "rev-list", "--count", "HEAD.."+remote)
	if n, perr := strconv.Atoi(strings.TrimSpace(behind)); err == nil && perr == nil && n > 0 {
		r.advise(fmt.Sprintf("checkout is %d commit(s) behind origin/main — run: sudo pages update", n))
	} else {
		r.advise("checkout differs from origin/main — run: sudo pages update")
	}
}

// newestVersion is `sort -V | tail -n1`.
func newestVersion(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[i], vs[j]) })
	return vs[len(vs)-1]
}

var numRun = regexp.MustCompile(`\d+|\D+`)

func versionLess(a, b string) bool {
	pa, pb := numRun.FindAllString(a, -1), numRun.FindAllString(b, -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

func statOwner(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("no ownership information")
	}
	owner, group := strconv.Itoa(int(sys.Uid)), strconv.Itoa(int(sys.Gid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return fmt.Sprintf("%o %s:%s", st.Mode().Perm(), owner, group), nil
}

func readable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func repairEnv(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	if err := os.Lchown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o640)
}

func freeKiB(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(max(st.Bsize, 0)) / 1024, nil
}

func httpProbe(ctx context.Context, url string) bool {
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
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func certificate(ctx context.Context, host string) (time.Time, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, errors.New("no certificate")
	}
	return certs[0].NotAfter, nil
}

func fedoraLatest(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fedoraproject.org/releases.json", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("releases.json: %s", resp.Status)
	}
	return LatestFedora(resp.Body)
}

// LatestFedora is the newest numeric release in a releases.json feed.
func LatestFedora(r io.Reader) (int, error) {
	var releases []struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 8<<20)).Decode(&releases); err != nil {
		return 0, err
	}
	best := 0
	for _, rel := range releases {
		if fedoraVersion.MatchString(rel.Version) {
			if n, err := strconv.Atoi(rel.Version); err == nil && n > best {
				best = n
			}
		}
	}
	if best == 0 {
		return 0, errors.New("no numeric release in the feed")
	}
	return best, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}
