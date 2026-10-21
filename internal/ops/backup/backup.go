// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package backup is `pages backup` and `pages check-integrity`: capture a
// complete recovery set (pg_dump custom-format database, the shared assets,
// the installed release, the configuration) under the updater's shared lock
// without stopping the application, then rehearse a restore into a private
// throwaway PostgreSQL cluster and probe it before publishing the backup.
// check-integrity verifies every captured byte and repeats the rehearsal.
//
// Nothing from a backup is ever executed: its files are only as trustworthy
// as its equally writable manifest, and these commands run as root. The
// captured release probes only through an installed binary that root alone
// can change and whose SHA-256 equals the captured one; otherwise this
// release serves the restored data (see Rehearse).
//
// Credentials never reach argv or output: pg_dump gets the connection as PG*
// variables, child diagnostics are redacted line by line, and errors name
// tools and exit codes, never the values a tool was given.
package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/version"
)

// excluded are top-level application entries a capture leaves out: mutable
// paths live in their own archive sections, the rest is build/test output.
var excluded = map[string]bool{".git": true, "node_modules": true, "assets": true, ".env": true, ".devdata": true, "test-results": true, "playwright-report": true}

// RuntimeSetting matches the service settings that change what a restored
// release accepts (data and HTML limits); they are carried into the probe.
var RuntimeSetting = regexp.MustCompile(`^(PAGES_DATA_[A-Z_]+|PAGES_MCP_[A-Z_]+|PAGES_SOURCE_FUTURE_TOLERANCE_MS|MAX_HTML_BYTES)$`)

// RuntimeSettings filters env to RuntimeSetting keys.
func RuntimeSettings(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if RuntimeSetting.MatchString(k) {
			out[k] = v
		}
	}
	return out
}

// FileEntry is one inventoried file.
type FileEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// Manifest is manifest.json.
type Manifest struct {
	FormatVersion         int               `json:"format_version"`
	Status                string            `json:"status"`
	StartedAt             string            `json:"started_at,omitempty"`
	CompletedAt           string            `json:"completed_at,omitempty"`
	RuntimeConfiguration  map[string]string `json:"runtime_configuration,omitempty"`
	PagesVersion          string            `json:"pages_version,omitempty"`
	GoVersion             string            `json:"go_version,omitempty"`
	PgDumpVersion         string            `json:"pg_dump_version,omitempty"`
	OptionalConfiguration *Optional         `json:"optional_configuration,omitempty"`
	ApplicationSource     string            `json:"application_source,omitempty"`
	Restored              json.RawMessage   `json:"restored,omitempty"`
	Files                 []FileEntry       `json:"files"`
}

// Optional records which optional configuration files were captured.
type Optional struct {
	InstallEnv bool `json:"install.env"`
	LocalEnv   bool `json:"local.env"`
}

// RequiredInputs are the files no recovery can do without.
var RequiredInputs = []string{"database.dump", "application/" + install.BinaryName, "config/service.env"}

// Options configures capture and rehearsal. Zero hooks take defaults.
type Options struct {
	Runner runner.Runner
	// EUID decides whether children are delegated (runuser) — root hands the
	// scratch cluster to postgres and pg_dump to the service account.
	EUID int
	// Environ is the base environment children inherit (os.Environ()).
	Environ []string
	// Tool resolves a PostgreSQL program.
	Tool func(name string) (string, error)
	// Probe runs `<binary> backup-probe <assets>` and returns its JSON.
	// binary is never a file from the backup: it is an installed binary,
	// independently trusted (install.TrustedFile), whose SHA-256 equals the
	// captured application's — so it IS the captured release.
	Probe func(ctx context.Context, binary, assets string, env []string) (json.RawMessage, error)
	// CurrentProbe runs THIS release's restore probe (`pages backup-probe`,
	// server in process) against the restored, migrated database when the
	// captured release cannot be run: a legacy Node-era backup, or a Go
	// backup whose binary matches no trusted installed release. A backup's
	// files are only as trustworthy as its equally writable manifest, and
	// check-integrity runs as root, so a captured binary is never executed.
	CurrentProbe func(ctx context.Context, assets string, env []string) (json.RawMessage, error)
	// ReleaseRoot is the installation's release directory
	// (install.Settings.ReleaseRoot); its root-owned release binaries, and
	// this executable, are the binaries a rehearsal may run.
	ReleaseRoot string
	// Scratch makes the rehearsal directory (a short /tmp path: Unix socket
	// paths are limited to ~100 bytes).
	Scratch func() (string, error)
	Now     func() time.Time
}

func (o *Options) defaults() {
	if o.Runner == nil {
		o.Runner = runner.Default
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	if o.Tool == nil {
		o.Tool = PostgresTool
	}
	if o.Probe == nil {
		o.Probe = o.execProbe
	}
	if o.CurrentProbe == nil {
		o.CurrentProbe = o.execCurrentProbe
	}
	if o.Scratch == nil {
		o.Scratch = func() (string, error) { return os.MkdirTemp("/tmp", "pgb-") }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// PostgresTool finds name on PATH, then under the versioned PostgreSQL
// install trees (newest first): Debian's /usr/lib/postgresql/N/bin, PGDG's
// /usr/pgsql-N/bin and Homebrew's kegs.
func PostgresTool(name string) (string, error) {
	var candidates []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, pattern := range []string{"/usr/lib/postgresql/*/bin", "/usr/pgsql-*/bin", "/opt/homebrew/opt/postgresql*/bin", "/usr/local/opt/postgresql*/bin"} {
		dirs, _ := filepath.Glob(pattern)
		sort.Slice(dirs, func(i, j int) bool { return numericLess(dirs[j], dirs[i]) })
		for _, d := range dirs {
			candidates = append(candidates, filepath.Join(d, name))
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s is required; install matching PostgreSQL server tools for restore verification", name)
}

var digitRun = regexp.MustCompile(`\d+`)

func numericLess(a, b string) bool {
	na, nb := digitRun.FindString(a), digitRun.FindString(b)
	ia, _ := strconv.Atoi(na)
	ib, _ := strconv.Atoi(nb)
	if ia != ib {
		return ia < ib
	}
	return a < b
}

// ToolError is a child that failed: tool, exit code and redacted stderr.
type ToolError struct {
	Tool     string
	ExitCode int
	Detail   string
}

func (e *ToolError) Error() string {
	if e.ExitCode < 0 {
		return fmt.Sprintf("%s could not start (%s)", e.Tool, e.Detail)
	}
	return fmt.Sprintf("%s failed (%d): %s", e.Tool, e.ExitCode, e.Detail)
}

// startCode reduces a start failure to an error code: the error text of a
// failed exec can quote the values it rejected, and here those include the
// connection settings, so it is never passed on.
func startCode(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, runner.ErrNotFound), errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, os.ErrPermission):
		return "EACCES"
	case errors.As(err, &errno):
		if name := errnoName(errno); name != "" {
			return name
		}
	}
	return "unknown"
}

func errnoName(e syscall.Errno) string {
	switch e {
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.E2BIG:
		return "E2BIG"
	case syscall.ENOEXEC:
		return "ENOEXEC"
	case syscall.ENOMEM:
		return "ENOMEM"
	}
	return ""
}

func envMap(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Run executes a child for capture/rehearsal: stderr is collected redacted,
// stdout is captured (or written to outputFile, created O_EXCL 0600), and
// failures are ToolErrors that never quote the environment.
func Run(ctx context.Context, r runner.Runner, name string, args []string, env []string, outputFile string) (string, error) {
	base := filepath.Base(name)
	for _, kv := range env {
		if strings.ContainsRune(kv, 0) {
			// Refused before the exec, which would quote the value.
			return "", &ToolError{Tool: base, ExitCode: -1, Detail: "EINVAL"}
		}
	}
	collector := NewStderrCollector(envMap(env), 0)
	var out bytes.Buffer
	c := runner.Cmd{Name: name, Args: args, Env: env, Stdout: &out, Stderr: collector}
	if outputFile != "" {
		f, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		defer f.Close()
		c.Stdout = f
	}
	_, err := r.Run(ctx, c)
	if err == nil {
		return strings.TrimSpace(out.String()), nil
	}
	var ee *runner.ExitError
	if errors.As(err, &ee) {
		return "", &ToolError{Tool: base, ExitCode: ee.Code, Detail: collector.Text()}
	}
	return "", &ToolError{Tool: base, ExitCode: -1, Detail: startCode(err)}
}

// Digest is a file's SHA-256, hex.
func Digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyTree copies regular files and directories; links and special files
// fail explicitly rather than producing an incomplete copy. The source can
// be writable by the service account while root copies it, so nothing is
// opened by a path that account could redirect: every entry is opened
// relative to its held parent directory without following links, the
// opened descriptor's own type decides how it is copied (a FIFO or device
// swapped in after the listing is refused, never read), and a file is read
// for exactly the length fstat reported.
func copyTree(ctx context.Context, source, target string, exclude map[string]bool) error {
	dir, err := os.OpenFile(source, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	if st, err := dir.Stat(); err != nil || !st.IsDir() {
		return fmt.Errorf("unsupported link or special file in capture: %s", source)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return copyDir(ctx, dir, target, exclude)
}

// afterListing, when set (tests only), runs between listing a source
// directory and opening its entries: the window another account races.
var afterListing func(dir string)

func copyDir(ctx context.Context, dir *os.File, target string, exclude map[string]bool) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	if afterListing != nil {
		afterListing(dir.Name())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if exclude[e.Name()] {
			continue
		}
		from, to := filepath.Join(dir.Name(), e.Name()), filepath.Join(target, e.Name())
		if !e.IsDir() && !e.Type().IsRegular() {
			return fmt.Errorf("unsupported link or special file in capture: %s", from)
		}
		in, err := openEntry(dir, e.Name(), e.IsDir())
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return fmt.Errorf("unsupported link or special file in capture: %s", from)
			}
			return err
		}
		err = copyOpened(ctx, in, e.IsDir(), from, to)
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// copyOpened copies the entry in is open on, by its own fstat type.
func copyOpened(ctx context.Context, in *os.File, wantDir bool, from, to string) error {
	st, err := in.Stat()
	if err != nil {
		return err
	}
	switch {
	case wantDir && st.IsDir():
		if err := os.Mkdir(to, 0o700); err != nil {
			return err
		}
		return copyDir(ctx, in, to, nil)
	case !wantDir && st.Mode().IsRegular():
		return copyBounded(in, st, to)
	}
	return fmt.Errorf("unsupported link or special file in capture: %s", from)
}

// copyBounded writes exactly st.Size() bytes of in to a new file to.
func copyBounded(in *os.File, st os.FileInfo, to string) error {
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, st.Size()))
	if err == nil && n != st.Size() {
		err = fmt.Errorf("%s changed during capture", in.Name())
	}
	if err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyFile copies one configuration file. Its path may be a managed symlink
// (local .env), so a final link is followed, but only a regular file is
// read (a FIFO is never waited on), for its fstat length.
func copyFile(from, to string) error {
	in, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("unsupported link or special file in capture: %s", from)
	}
	return copyBounded(in, st, to)
}

// Inventory lists every file under directory (name order, manifest.json at
// the top level excluded) with its length and SHA-256. Links and special
// files are errors.
func Inventory(directory string) ([]FileEntry, error) {
	return inventory(directory, "")
}

func inventory(directory, prefix string) ([]FileEntry, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := []FileEntry{}
	for _, e := range entries {
		if prefix == "" && e.Name() == "manifest.json" {
			continue
		}
		name, file := prefix+e.Name(), filepath.Join(directory, e.Name())
		switch {
		case e.IsDir():
			sub, err := inventory(file, name+"/")
			if err != nil {
				return nil, err
			}
			files = append(files, sub...)
		case e.Type().IsRegular():
			st, err := os.Stat(file)
			if err != nil {
				return nil, err
			}
			sum, err := Digest(file)
			if err != nil {
				return nil, err
			}
			files = append(files, FileEntry{Path: name, Bytes: st.Size(), Sha256: sum})
		default:
			return nil, fmt.Errorf("backup contains an unsupported link or special file: %s", name)
		}
	}
	return files, nil
}

// VerifyFiles checks a backup directory against its manifest: format,
// completeness, the required sections, every file's length and digest (an
// extra or missing file fails too) and the required recovery inputs.
func VerifyFiles(directory string, m *Manifest) ([]FileEntry, error) {
	if m == nil || m.FormatVersion != 1 || m.Status != "complete" || m.Files == nil {
		return nil, errors.New("unsupported or incomplete backup manifest")
	}
	for _, name := range []string{"assets", "application", "config"} {
		st, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("backup is missing required directory: %s", name)
		}
	}
	actual, err := Inventory(directory)
	if err != nil {
		return nil, err
	}
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(m.Files)
	if !bytes.Equal(a, b) {
		return nil, errors.New("backup file inventory/checksum mismatch; do not restore this artifact")
	}
	for _, req := range RequiredInputs {
		found := false
		for _, f := range actual {
			found = found || f.Path == req
		}
		if !found {
			return nil, fmt.Errorf("backup is missing required recovery input: %s", req)
		}
	}
	return actual, nil
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// IsolatedEnv is the environment of every rehearsal child. No archived
// environment is sourced: every libpq override (service files, options) is
// cleared before targeting the private cluster, credentials are synthetic,
// and sign-in is pinned to the self-contained elcano mode with no signing
// key, so the probe never needs, reads or reaches a central Auth (whose
// settings name the live host and would refuse localhost) and every admin
// request fails closed. Only the captured runtime limits carry over.
func IsolatedEnv(base []string, socket string, runtimeSettings map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range envMap(base) {
		if strings.HasPrefix(k, "PG") || k == "DATABASE_URL" || RuntimeSetting.MatchString(k) ||
			strings.HasPrefix(k, "AUTH_") || strings.HasPrefix(k, "PAGES_AUTH_") || k == "PAGES_PUBLIC_URL" {
			continue
		}
		env[k] = v
	}
	for k, v := range RuntimeSettings(runtimeSettings) {
		env[k] = v
	}
	for k, v := range map[string]string{
		"PGHOST": socket, "PGPORT": "5432", "PGUSER": scratchRole, "PGDATABASE": "pages",
		"PAGES_AUTH_MODE": "elcano", "PAGES_ENV": "production", "NODE_ENV": "production",
		"RAW_TOKEN_SECRET": randomUUID(), "API_TOKEN_PEPPER": randomUUID(), "PAGE_COOKIE_SECRET": randomUUID(),
		"RL_CONTENT_PER_MIN": "1000", "PAGES_DEV_LOGIN": "0", "AUTH_SIGNING_PUBKEY": "",
		"DASHBOARD_HOST": "localhost", "CONTENT_HOST": "content.localhost",
		"DASHBOARD_ORIGIN": "http://localhost", "CONTENT_ORIGIN": "http://content.localhost",
	} {
		env[k] = v
	}
	return env
}

// scratchRole is the rehearsal's restore and probe role (IsolatedEnv's
// PGUSER): an ordinary login role owning the scratch database, never a
// superuser. scratchSuperuser is the scratch cluster's bootstrap superuser,
// used only before the dump is touched.
const (
	scratchRole      = "pages_backup"
	scratchSuperuser = "pages_scratch_admin"
)

// UntrustedBinaryNotice is check-integrity's notice for a backup whose
// captured binary matches no trusted installed release.
const UntrustedBinaryNotice = "captured binary not executed; served with this release"

// Rehearse restores directory/database.dump into a private cluster and
// probes it. When the captured binary's SHA-256 equals a trusted installed
// binary (this executable, or a root-owned release under ReleaseRoot) that
// installed binary — the captured release itself — runs the probe, and its
// path is returned as probedWith. Otherwise nothing from the backup is
// executed: the snapshot is inventoried as restored, the scratch copy is
// migrated and served with this release (probedWith is ""). The scratch
// cluster is always stopped before its storage is removed; when the stop
// cannot be confirmed the scratch directory is retained for the operator.
func Rehearse(ctx context.Context, o Options, directory string, runtimeSettings map[string]string) (restored json.RawMessage, probedWith string, err error) {
	o.defaults()
	sum, err := Digest(filepath.Join(directory, "application", install.BinaryName))
	if err != nil {
		return nil, "", err
	}
	trusted := o.trustedBinary(sum)
	assets := filepath.Join(directory, "assets")
	restored, err = rehearse(ctx, o, directory, runtimeSettings, func(ctx context.Context, _ string, env []string) (json.RawMessage, error) {
		if trusted != "" {
			return o.Probe(ctx, trusted, assets, env)
		}
		return currentReleaseProbe(ctx, o, assets, env)
	})
	return restored, trusted, err
}

// trustedBinary is the resolved path of an installed binary whose SHA-256
// is sum and which only root can change, or "".
func (o *Options) trustedBinary(sum string) string {
	var candidates []string
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, self)
	}
	if o.ReleaseRoot != "" {
		releases, _ := filepath.Glob(filepath.Join(o.ReleaseRoot, "*", install.BinaryName))
		candidates = append(candidates, releases...)
	}
	for _, c := range candidates {
		p, err := install.TrustedFile(c)
		if err != nil {
			continue
		}
		if d, err := Digest(p); err == nil && d == sum {
			return p
		}
	}
	return ""
}

// rehearse restores directory/database.dump into a private cluster under a
// fresh scratch directory and runs probe against it with the isolated
// environment env; probe's JSON is returned compacted.
func rehearse(ctx context.Context, o Options, directory string, runtimeSettings map[string]string,
	probe func(ctx context.Context, scratch string, env []string) (json.RawMessage, error)) (restored json.RawMessage, err error) {
	initdb, err := o.Tool("initdb")
	if err != nil {
		return nil, err
	}
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	pgrestore, err := o.Tool("pg_restore")
	if err != nil {
		return nil, err
	}
	createdb, err := o.Tool("createdb")
	if err != nil {
		return nil, err
	}
	createuser, err := o.Tool("createuser")
	if err != nil {
		return nil, err
	}
	scratch, err := o.Scratch()
	if err != nil {
		return nil, err
	}
	data, socket := filepath.Join(scratch, "db"), filepath.Join(scratch, "socket")
	envm := IsolatedEnv(o.Environ, socket, runtimeSettings)
	env := envList(envm)
	asPg := func(ctx context.Context, tool string, args ...string) error {
		if o.EUID == 0 {
			_, err := Run(ctx, o.Runner, "runuser", append([]string{"-u", "postgres", "--", tool}, args...), env, "")
			var te *ToolError
			if errors.As(err, &te) && te.ExitCode >= 0 {
				te.Tool = filepath.Base(tool) // name the tool, not the delegation
			}
			return err
		}
		_, err := Run(ctx, o.Runner, tool, args, env, "")
		return err
	}
	started := false
	defer func() {
		// A failed stop deliberately retains the scratch directory; never
		// remove storage underneath a still-running PostgreSQL.
		if started {
			cctx := context.WithoutCancel(ctx)
			if serr := asPg(cctx, pgctl, "-D", data, "-w", "-t", "60", "-m", "immediate", "stop"); serr != nil {
				stopped := false
				if st := asPg(cctx, pgctl, "-D", data, "status"); st != nil {
					var te *ToolError
					stopped = errors.As(st, &te) && te.ExitCode == 3
				}
				if !stopped {
					err = fmt.Errorf("cannot confirm scratch PostgreSQL stopped; retained %s for operator cleanup", scratch)
					restored = nil
					return
				}
			}
		}
		_ = os.RemoveAll(scratch)
	}()
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, err
	}
	if o.EUID == 0 {
		if _, err := Run(ctx, o.Runner, "chown", []string{"-R", "postgres:postgres", scratch}, env, ""); err != nil {
			return nil, err
		}
	}
	// The bootstrap superuser only creates the restore role and database;
	// the dump (attacker-supplied, for check-integrity) is restored and
	// probed as scratchRole, which cannot run programs or read server files.
	if err := asPg(ctx, initdb, "-D", data, "--auth=trust", "--username="+scratchSuperuser, "--encoding=UTF8", "--no-locale"); err != nil {
		return nil, err
	}
	started = true
	if err := asPg(ctx, pgctl, "-D", data, "-l", filepath.Join(scratch, "postgres.log"), "-w", "-t", "60", "-o",
		"-p 5432 -k "+socket+" -c listen_addresses='' -c unix_socket_permissions=0700", "start"); err != nil {
		return nil, err
	}
	createEnv := map[string]string{}
	for k, v := range envm {
		createEnv[k] = v
	}
	createEnv["PGDATABASE"], createEnv["PGUSER"] = "postgres", scratchSuperuser
	// A fresh role holds none of the predefined pg_execute_server_program /
	// pg_read_server_files / pg_write_server_files roles.
	if _, err := Run(ctx, o.Runner, createuser, []string{"--login", "--no-superuser", "--no-createdb", "--no-createrole",
		"--no-replication", scratchRole}, envList(createEnv), ""); err != nil {
		return nil, err
	}
	if _, err := Run(ctx, o.Runner, createdb, []string{"--owner=" + scratchRole, "pages"}, envList(createEnv), ""); err != nil {
		return nil, err
	}
	// env connects as scratchRole (IsolatedEnv): the restore, the probe and
	// any migration of the scratch copy never hold superuser privileges.
	if _, err := Run(ctx, o.Runner, pgrestore, []string{"--exit-on-error", "--single-transaction", "--no-owner", "--no-acl", "--dbname=pages", filepath.Join(directory, "database.dump")}, env, ""); err != nil {
		return nil, err
	}
	out, err := probe(ctx, scratch, env)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out); err != nil {
		return nil, fmt.Errorf("restore probe returned invalid JSON: %w", err)
	}
	return compact.Bytes(), nil
}

// execProbe runs a trusted installed binary's probe: `<binary>
// backup-probe <assets>`. The binary is byte-identical to the captured one
// and carries its own code, so the rehearsal runs exactly the release that
// was backed up — without executing anything the backup holds.
func (o *Options) execProbe(ctx context.Context, binary, assets string, env []string) (json.RawMessage, error) {
	out, err := Run(ctx, o.Runner, binary, []string{"backup-probe", assets}, env, "")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// CreateInput names the installed state a capture reads.
type CreateInput struct {
	Destination   string
	Application   string // the installed release (APP_DIR)
	EnvFile       string
	InstallConfig string
	// AppUser runs pg_dump when the capture runs as root, keeping the
	// service account's libpq defaults and peer authentication.
	AppUser string
	// EnvDigest is the SHA-256 of EnvFile when ServiceEnv was loaded from
	// it; a mismatch means the file changed in between.
	EnvDigest string
	// ServiceEnv is the environment pg_dump starts from (the service's).
	ServiceEnv map[string]string
}

// Create captures, rehearses and publishes a backup; it returns the
// published directory. Any failure removes the incomplete capture.
func Create(ctx context.Context, o Options, in CreateInput) (published string, err error) {
	o.defaults()
	started := o.Now().UTC()
	startedAt := started.Format("2006-01-02T15:04:05.000Z")
	application, err := filepath.EvalSymlinks(in.Application)
	if err != nil {
		return "", err
	}
	assets, err := filepath.EvalSymlinks(filepath.Join(application, "assets"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(in.Destination, 0o700); err != nil {
		return "", err
	}
	// Root writes the capture here, so no other account (the service
	// account above all) may be able to replace the destination or any
	// directory above it — with a symlink to somewhere root would then write,
	// or to read the capture: it must be root's (or the caller's) alone.
	destination, err := install.TrustedDir(in.Destination)
	if err != nil {
		return "", fmt.Errorf("backup destination must be a directory only root can change: %w", err)
	}
	for _, source := range []string{application, assets} {
		if destination == source || strings.HasPrefix(destination, source+string(filepath.Separator)) {
			return "", errors.New("backup destination must be outside application and asset directories")
		}
	}
	// The partial tree is private (0700) before any byte lands in it.
	temporary, err := os.MkdirTemp(destination, ".pages-partial-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	var complete string
	defer func() {
		if err != nil {
			if complete != "" {
				_ = os.RemoveAll(complete)
			}
			_ = os.RemoveAll(temporary)
		}
	}()
	config := filepath.Join(temporary, "config")
	if err := os.Mkdir(config, 0o700); err != nil {
		return "", err
	}
	if err := copyFile(in.EnvFile, filepath.Join(config, "service.env")); err != nil {
		return "", err
	}
	if in.EnvDigest != "" {
		if d, err := Digest(filepath.Join(config, "service.env")); err != nil || d != in.EnvDigest {
			return "", errors.New("service environment changed while loading configuration; retry after configuration changes finish")
		}
	}
	optional := &Optional{}
	sources := [][2]string{{"service.env", in.EnvFile}}
	for _, opt := range []struct {
		name, source string
		flag         *bool
	}{{"install.env", in.InstallConfig, &optional.InstallEnv}, {"local.env", filepath.Join(application, ".env"), &optional.LocalEnv}} {
		if err := copyFile(opt.source, filepath.Join(config, opt.name)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			continue
		}
		*opt.flag = true
		sources = append(sources, [2]string{opt.name, opt.source})
	}
	dumpEnv, err := DumpEnvironment(in.ServiceEnv)
	if err != nil {
		return "", err
	}
	pgdump, err := o.Tool("pg_dump")
	if err != nil {
		return "", err
	}
	dumpArgs := []string{"--format=custom", "--no-owner", "--no-acl"}
	output := filepath.Join(temporary, "database.dump")
	// The database is dumped BEFORE assets are copied, so every asset the
	// dump references is already on disk when it is copied.
	if o.EUID == 0 && in.AppUser != "" {
		_, err = Run(ctx, o.Runner, "runuser", append([]string{"-u", in.AppUser, "--", pgdump}, dumpArgs...), envList(dumpEnv), output)
	} else {
		_, err = Run(ctx, o.Runner, pgdump, dumpArgs, envList(dumpEnv), output)
	}
	if err != nil {
		return "", err
	}
	if err := copyTree(ctx, assets, filepath.Join(temporary, "assets"), nil); err != nil {
		return "", err
	}
	if err := copyTree(ctx, application, filepath.Join(temporary, "application"), excluded); err != nil {
		return "", err
	}
	// Inventory comes from the restored dump, never from a later live read.
	runtimeCfg := RuntimeSettings(in.ServiceEnv)
	restored, _, err := Rehearse(ctx, o, temporary, runtimeCfg)
	if err != nil {
		return "", err
	}
	for _, src := range sources {
		a, err1 := Digest(src[1])
		b, err2 := Digest(filepath.Join(config, src[0]))
		if err1 != nil || err2 != nil || a != b {
			return "", fmt.Errorf("configuration changed during capture: %s; retry after configuration changes finish", src[0])
		}
	}
	dumpVersion, _ := Run(ctx, o.Runner, pgdump, []string{"--version"}, nil, "")
	files, err := Inventory(temporary)
	if err != nil {
		return "", err
	}
	m := Manifest{
		FormatVersion: 1, Status: "complete", StartedAt: startedAt,
		CompletedAt:          o.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		RuntimeConfiguration: runtimeCfg, PagesVersion: version.Version + " (" + version.Revision() + ")",
		GoVersion: runtime.Version(), PgDumpVersion: dumpVersion, OptionalConfiguration: optional,
		ApplicationSource: "captured installed files; source checkout HEAD is not used",
		Restored:          restored, Files: files,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temporary, "manifest.json"), append(body, '\n'), 0o600); err != nil {
		return "", err
	}
	// Flush recovery inputs and directory entries before publishing.
	for _, f := range append(files, FileEntry{Path: "manifest.json"}) {
		if err := syncPath(filepath.Join(temporary, f.Path)); err != nil {
			return "", err
		}
	}
	if err := syncDirs(temporary); err != nil {
		return "", err
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(startedAt)
	complete = filepath.Join(destination, "pages-backup-"+stamp+"-"+randomUUID())
	if err := os.Rename(temporary, complete); err != nil {
		complete = ""
		return "", err
	}
	if err := syncPath(destination); err != nil {
		return "", err
	}
	return complete, nil
}

func syncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDirs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := syncDirs(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return syncPath(dir)
}

// CheckResult is check-integrity's report. Format is set only for a legacy
// (Node-era) backup. ProbedWith is the trusted installed binary, identical
// to the captured one, that probed the restore; when none matched, Notice
// and ServedWith say the captured binary was not executed and which release
// served the restored data instead.
type CheckResult struct {
	Status     string          `json:"status"`
	BackupDir  string          `json:"backup_dir"`
	Format     string          `json:"format,omitempty"`
	Notice     string          `json:"notice,omitempty"`
	ServedWith string          `json:"served_with,omitempty"`
	ProbedWith string          `json:"probed_with,omitempty"`
	Restored   json.RawMessage `json:"restored"`
}

// Check verifies a backup's files and repeats its restore rehearsal, which
// must reproduce the capture-time result. A backup written by the Node
// release's scripts/backup.js is recognised by its manifest and checked by
// checkNodeEra instead.
func Check(ctx context.Context, o Options, directory string) (*CheckResult, error) {
	dir, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	legacy, err := IsNodeEra(b)
	if err != nil {
		return nil, err
	}
	if legacy {
		return checkNodeEra(ctx, o, dir, b)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if _, err := VerifyFiles(dir, &m); err != nil {
		return nil, err
	}
	restored, probedWith, err := Rehearse(ctx, o, dir, m.RuntimeConfiguration)
	if err != nil {
		return nil, err
	}
	var want bytes.Buffer
	if err := json.Compact(&want, m.Restored); err != nil || !bytes.Equal(want.Bytes(), restored) {
		return nil, errors.New("restored database inventory differs from the backup manifest")
	}
	res := &CheckResult{Status: "verified", BackupDir: dir, ProbedWith: probedWith, Restored: restored}
	if probedWith == "" {
		res.Notice, res.ServedWith = UntrustedBinaryNotice, version.String()
	}
	return res, nil
}
