// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package install holds the installation layout every operator verb shares —
// where the application, checkout, environment file and CLI live, which
// account runs the service, which port it listens on — and renders the
// control files (installation defaults, systemd unit, Caddy site blocks) from
// the templates embedded in this binary.
//
// The persisted /etc/default/pages-install contains only non-secret defaults,
// written as PAGES_X="${PAGES_X:-value}", so explicit caller overrides keep
// winning on a rerun; legacy APP_DIR/APP_USER override their PAGES_*
// equivalents. The file is parsed, never sourced.
package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/ops/envfile"
)

// Defaults of a standard installation.
const (
	DefaultInstallConfig = "/etc/default/pages-install"
	DefaultAppDir        = "/opt/pages"
	DefaultAppUser       = "pages"
	DefaultSrcDir        = "/opt/pages-src"
	DefaultEnvFile       = "/etc/default/pages"
	DefaultCLITarget     = "/usr/local/bin/pages"
	DefaultPort          = "3002"
	// Service is the systemd unit name.
	Service = "pages.service"
	// BinaryName is the executable inside a release directory.
	BinaryName = "pages"
)

// Settings is the resolved installation layout.
type Settings struct {
	InstallConfig string // PAGES_INSTALL_CONFIG
	AppDir        string // APP_DIR / PAGES_APP_DIR
	AppUser       string // APP_USER / PAGES_APP_USER
	SrcDir        string // PAGES_SRC_DIR
	EnvFile       string // PAGES_ENV_FILE
	CLITarget     string // PAGES_CLI_TARGET
	Port          string // PAGES_PORT, else PORT in the env file, else 3002
	// PortExplicit is true when PAGES_PORT set the port.
	PortExplicit bool
}

// ReleaseRoot is where retained releases live (APP_DIR.releases).
func (s Settings) ReleaseRoot() string { return s.AppDir + ".releases" }

// SharedAssets is the writable assets directory every release links to.
func (s Settings) SharedAssets() string { return s.AppDir + ".assets" }

// SharedEnv is the optional local .env every release links to.
func (s Settings) SharedEnv() string { return s.AppDir + ".env" }

// LockFile serializes updates (exclusive) against backups (shared).
func (s Settings) LockFile() string { return s.AppDir + ".update.lock" }

// Binary is the active release's executable.
func (s Settings) Binary() string { return filepath.Join(s.AppDir, BinaryName) }

// Getenv is the environment lookup Load uses (os.Getenv in production).
type Getenv func(string) string

var configRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*):-(.*)\}$`)

// Load resolves Settings from the process environment and the persisted
// installation file, exactly as scripts/install-config.sh did.
func Load(getenv Getenv) Settings {
	if getenv == nil {
		getenv = os.Getenv
	}
	s := Settings{InstallConfig: or(getenv("PAGES_INSTALL_CONFIG"), DefaultInstallConfig)}
	// The persisted defaults: PAGES_X="${PAGES_X:-value}" keeps an explicit
	// override; a literal value (hand-edited file) wins, as sourcing would.
	vals := map[string]string{}
	for _, k := range []string{"PAGES_APP_DIR", "PAGES_APP_USER", "PAGES_SRC_DIR", "PAGES_ENV_FILE", "PAGES_CLI_TARGET", "PAGES_PORT"} {
		vals[k] = getenv(k)
	}
	if f, err := envfile.Read(s.InstallConfig); err == nil {
		for _, e := range f.Entries {
			v := e.Value
			if m := configRef.FindStringSubmatch(v); m != nil {
				v = or(lookup(vals, getenv, m[1]), m[2])
			}
			vals[e.Key] = v
		}
	}
	s.AppDir = or(getenv("APP_DIR"), vals["PAGES_APP_DIR"], DefaultAppDir)
	s.AppUser = or(getenv("APP_USER"), vals["PAGES_APP_USER"], DefaultAppUser)
	s.SrcDir = or(vals["PAGES_SRC_DIR"], DefaultSrcDir)
	s.EnvFile = or(vals["PAGES_ENV_FILE"], DefaultEnvFile)
	s.CLITarget = or(vals["PAGES_CLI_TARGET"], DefaultCLITarget)
	if p := vals["PAGES_PORT"]; p != "" {
		s.Port, s.PortExplicit = p, true
	} else if f, err := envfile.Read(s.EnvFile); err == nil {
		s.Port = f.Value("PORT")
	}
	s.Port = or(s.Port, DefaultPort)
	return s
}

func lookup(vals map[string]string, getenv Getenv, k string) string {
	if v, ok := vals[k]; ok {
		return v
	}
	return getenv(k)
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	safePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	account  = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)
	digits   = regexp.MustCompile(`^\d+$`)
	hostname = regexp.MustCompile(`(?i)^[a-z0-9.-]+$`)
)

// Validate refuses settings that cannot be installed safely: every path is
// absolute and made of letters, digits, '/', '_', '.' and '-' (no '..'), the
// account is a system account name and the port is 1..65535.
func (s Settings) Validate() error {
	for _, p := range []struct{ key, v string }{
		{"APP_DIR", s.AppDir}, {"INSTALL_SRC_DIR", s.SrcDir}, {"ENV_FILE", s.EnvFile},
		{"CLI_TARGET", s.CLITarget}, {"PAGES_INSTALL_CONFIG", s.InstallConfig},
	} {
		if !safePath.MatchString(p.v) || containsDotDot(p.v) {
			return fmt.Errorf("%s must be an absolute path containing only letters, digits, /, _, . and -", p.key)
		}
	}
	if !account.MatchString(s.AppUser) {
		return errors.New("APP_USER must be a system account name")
	}
	n, err := strconv.Atoi(s.Port)
	if !digits.MatchString(s.Port) || err != nil || n < 1 || n > 65535 {
		return errors.New("PORT must be between 1 and 65535")
	}
	return nil
}

func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Env renders the settings as the environment a re-executed pages (under
// sudo or runuser) needs to resolve the same installation.
func (s Settings) Env() []string {
	env := []string{
		"PAGES_INSTALL_CONFIG=" + s.InstallConfig, "APP_DIR=" + s.AppDir, "APP_USER=" + s.AppUser,
		"PAGES_SRC_DIR=" + s.SrcDir, "PAGES_ENV_FILE=" + s.EnvFile, "PAGES_CLI_TARGET=" + s.CLITarget,
	}
	if s.PortExplicit {
		env = append(env, "PAGES_PORT="+s.Port)
	}
	return env
}

// RenderConfig is /etc/default/pages-install: installation defaults, no
// credentials, each overridable by the caller's environment.
func (s Settings) RenderConfig() string {
	var b strings.Builder
	b.WriteString("# Pages installation defaults; generated by bootstrap. Contains no credentials.\n")
	for _, kv := range [][2]string{
		{"PAGES_APP_DIR", s.AppDir}, {"PAGES_APP_USER", s.AppUser}, {"PAGES_SRC_DIR", s.SrcDir},
		{"PAGES_ENV_FILE", s.EnvFile}, {"PAGES_CLI_TARGET", s.CLITarget},
	} {
		fmt.Fprintf(&b, "%s=\"${%s:-%s}\"\n", kv[0], kv[0], kv[1])
	}
	return b.String()
}

func deployFile(name string) (string, error) {
	b, err := fs.ReadFile(pages.Deploy(), name)
	return string(b), err
}

var (
	unitUser    = regexp.MustCompile(`(?m)^User=.*$`)
	unitGroup   = regexp.MustCompile(`(?m)^Group=.*$`)
	unitWorkDir = regexp.MustCompile(`(?m)^WorkingDirectory=.*$`)
	unitEnvFile = regexp.MustCompile(`(?m)^EnvironmentFile=.*$`)
	unitRWPaths = regexp.MustCompile(`(?m)^ReadWritePaths=.*$`)
	unitExec    = regexp.MustCompile(`(?m)^ExecStart=.*$`)
)

// RenderService is the systemd unit for these settings.
func (s Settings) RenderService() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	t, err := deployFile("pages.service")
	if err != nil {
		return "", err
	}
	t = unitUser.ReplaceAllLiteralString(t, "User="+s.AppUser)
	t = unitGroup.ReplaceAllLiteralString(t, "Group="+s.AppUser)
	t = unitWorkDir.ReplaceAllLiteralString(t, "WorkingDirectory="+s.AppDir)
	t = unitEnvFile.ReplaceAllLiteralString(t, "EnvironmentFile="+s.EnvFile)
	t = unitRWPaths.ReplaceAllLiteralString(t, "ReadWritePaths="+s.AppDir+"/assets")
	t = unitExec.ReplaceAllLiteralString(t, "ExecStart="+s.Binary()+" serve")
	return t, nil
}

// RenderCaddy is the Caddy site-block file for the two hostnames, proxied to
// the configured port. With tlsInternal every site block gets `tls internal`
// (the self-signed install, Let's Encrypt declined).
func (s Settings) RenderCaddy(dashboardHost, contentHost string, tlsInternal bool) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	for _, h := range []struct{ key, v string }{{"DASHBOARD_HOST", dashboardHost}, {"CONTENT_HOST", contentHost}} {
		if !hostname.MatchString(h.v) {
			return "", fmt.Errorf("%s must be a hostname", h.key)
		}
	}
	t, err := deployFile("pages.caddy")
	if err != nil {
		return "", err
	}
	t = strings.ReplaceAll(t, "{{DASHBOARD_HOST}}", dashboardHost)
	t = strings.ReplaceAll(t, "{{CONTENT_HOST}}", contentHost)
	t = strings.ReplaceAll(t, "127.0.0.1:3002", "127.0.0.1:"+s.Port)
	if tlsInternal {
		t = InjectTLSInternal(t)
	}
	return t, nil
}

var siteBlock = regexp.MustCompile(`^[^\s{}#].*\{$`)

// SiteBlockLines returns the site-address lines of a Caddy file (what
// `pages tls status` lists).
func SiteBlockLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if siteBlock.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// InjectTLSInternal adds `tls internal` as the first directive of every site
// block.
func InjectTLSInternal(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		if siteBlock.MatchString(strings.TrimSuffix(l, "\n")) {
			b.WriteString("\ttls internal\n")
		}
	}
	return b.String()
}

// Motd is the login banner shipped with this binary (deploy/motd).
func Motd() (string, error) { return deployFile("motd") }

// WriteFileAtomic replaces target with data at mode via a temporary file in
// the same directory and a rename, creating the directory when needed.
func WriteFileAtomic(target string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	ok = true
	return nil
}

// SymlinkAtomic points link at target, replacing whatever link is there in
// one rename (the `ln -s … && mv -Tf` idiom).
func SymlinkAtomic(target, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.next.%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CopyPreserving copies src to dst keeping a symlink a symlink and a regular
// file's mode (the `cp -a` the updater used for its recovery snapshot).
func CopyPreserving(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(t, dst)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return WriteFileAtomic(dst, b, st.Mode().Perm())
}

var databaseURLPassword = regexp.MustCompile(`^postgres(ql)?://pages:([A-Za-z0-9._~-]+)@`)

// DatabasePassword is the pages role password for bootstrap: the one in an
// existing DATABASE_URL, so a re-run keeps it, or a freshly minted one on
// first install (48 hex characters, so no URL encoding). The same text goes
// into both the URL and ALTER ROLE, so only URL-safe passwords are reused;
// any other existing URL is an error, raised before the role is touched,
// rather than rotating the live password.
func DatabasePassword(existingURL string) (string, error) {
	if existingURL == "" {
		return RandomHex(24)
	}
	m := databaseURLPassword.FindStringSubmatch(existingURL)
	if m == nil {
		return "", errors.New("DATABASE_URL is not a pages URL with a URL-safe password")
	}
	return m[2], nil
}

// RandomHex returns n random bytes, hex-encoded (openssl rand -hex n).
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Lock takes the installation's update lock: exclusive for an update,
// shared for a backup, never waiting.
func Lock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
