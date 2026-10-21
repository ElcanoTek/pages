// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package devenv is the hidden `pages dev` verb: a one-command local
// environment. It stands up a self-contained PostgreSQL cluster (trust auth,
// Unix socket only) — or, on macOS or with PAGES_DEV_DATABASE_URL, uses the
// developer's own server — migrates, ensures an agent API token exists, mints
// a throwaway SSO keypair and admin cookie, and runs `pages serve` with both
// hosts pointed at localhost and the gated dev-login route on
// (PAGES_DEV_LOGIN=1 — never in production). Re-running reuses the cluster,
// secrets and token.
//
// Secrets and the saved token live in .devdata/ (gitignored). The cluster and
// its socket live under /var/tmp/pages-dev because the postgres account
// (which owns the cluster when run as root) cannot traverse a mode-700 /root.
package devenv

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/migrate"
	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

// Options configures the dev environment.
type Options struct {
	// Root is the checkout (.devdata lives under it).
	Root   string
	Runner runner.Runner
	Log    *term.Printer
	Getenv func(string) string
	EUID   int
	// Self is the pages binary (run for `token add`).
	Self string
}

// Env is the resolved development environment.
type Env struct {
	Vars      map[string]string
	TokenFile string
	DSN       string // what the CLI itself connects with
	External  bool
}

var tokenPattern = regexp.MustCompile(`pgs_[A-Za-z0-9_-]{20,}`)

// Token prints the saved agent token (`pages dev token`).
func Token(o Options) (string, error) {
	b, err := os.ReadFile(filepath.Join(o.Root, ".devdata", "agent-token"))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("no token yet — run 'pages dev' once to create one")
	}
	return strings.TrimSpace(string(b)), nil
}

func (o *Options) get(k, def string) string {
	if v := o.Getenv(k); v != "" {
		return v
	}
	return def
}

// Secrets ensures .devdata/dev.env (stable dev secrets, generated once: the
// pepper must not change or every issued token dies) and returns it.
func Secrets(dev string) (map[string]string, error) {
	if err := os.MkdirAll(dev, 0o700); err != nil {
		return nil, err
	}
	file := filepath.Join(dev, "dev.env")
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		var b strings.Builder
		for _, k := range []string{"RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "PAGE_COOKIE_SECRET"} {
			v, err := install.RandomHex(32)
			if err != nil {
				return nil, err
			}
			b.WriteString(k + "=" + v + "\n")
		}
		if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
			return nil, err
		}
	}
	f, err := envfile.Read(file)
	if err != nil {
		return nil, err
	}
	return f.Map(), nil
}

// Auth mints the local-only SSO material: a throwaway Ed25519 keypair
// (persisted in dev/auth-dev.pem), its public half as AUTH_SIGNING_PUBKEY,
// and a 30-day admin cookie for email signed by the private half.
func Auth(dev, email string, now time.Time) (pub, cookie string, err error) {
	pemFile := filepath.Join(dev, "auth-dev.pem")
	var priv ed25519.PrivateKey
	if b, rerr := os.ReadFile(pemFile); rerr == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return "", "", errors.New(pemFile + ": not PEM")
		}
		k, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return "", "", perr
		}
		var ok bool
		if priv, ok = k.(ed25519.PrivateKey); !ok {
			return "", "", errors.New(pemFile + ": not an Ed25519 key")
		}
	} else {
		if _, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return "", "", err
		}
		der, merr := x509.MarshalPKCS8PrivateKey(priv)
		if merr != nil {
			return "", "", merr
		}
		if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return "", "", err
		}
	}
	pub = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	claims, _ := json.Marshal(struct {
		Email  string `json:"email"`
		Tenant string `json:"tenant"`
		Iat    int64  `json:"iat"`
		Exp    int64  `json:"exp"`
	}{email, "", now.Unix(), now.Unix() + 30*86400})
	body := base64.RawURLEncoding.EncodeToString(claims)
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return pub, body + "." + sig, nil
}

// Prepare provisions the database, secrets, auth and token and returns the
// server environment.
func Prepare(ctx context.Context, o Options) (*Env, error) {
	if o.Runner == nil {
		o.Runner = runner.Default
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Log == nil {
		o.Log = term.New()
	}
	dev := filepath.Join(o.Root, ".devdata")
	secrets, err := Secrets(dev)
	if err != nil {
		return nil, err
	}
	port := o.get("PORT", "3099")
	e := &Env{Vars: map[string]string{}, TokenFile: filepath.Join(dev, "agent-token")}
	for k, v := range secrets {
		e.Vars[k] = v
	}
	e.External = runtime.GOOS == "darwin" || o.Getenv("PAGES_DEV_DATABASE_URL") != ""
	if e.External {
		if err := o.external(ctx, e); err != nil {
			return nil, err
		}
	} else if err := o.cluster(ctx, e); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{
		"DASHBOARD_HOST": "localhost", "CONTENT_HOST": "content.localhost", "CONTENT_HOST_ALSO": "content.localhost",
		"CONTENT_ORIGIN": "http://content.localhost:" + port, "DASHBOARD_ORIGIN": "http://localhost:" + port, "PORT": port,
		// Keep auth-failure redirects LOCAL: never bounce to the real auth
		// service. A no-cookie /admin visit round-trips through dev-login.
		"PAGES_DEV_LOGIN": "1", "AUTH_LOGIN_URL": "http://localhost:" + port + "/__dev/login",
	} {
		e.Vars[k] = v
	}
	email := o.get("DEV_ADMIN_EMAIL", "dev@elcanotek.com")
	pub, cookie, err := Auth(dev, email, time.Now())
	if err != nil {
		return nil, err
	}
	e.Vars["AUTH_SIGNING_PUBKEY"], e.Vars["DEV_ADMIN_COOKIE"], e.Vars["DEV_ADMIN_EMAIL"] = pub, cookie, email
	// DEV-only "compose a page with Cutlass" panel, when the binary is built
	// next door. NEVER set PAGES_COMPOSE=1 in production.
	cutlassDir := o.get("CUTLASS_DIR", filepath.Join(filepath.Dir(o.Root), "cutlass"))
	cutlassBin := o.get("CUTLASS_BIN", filepath.Join(cutlassDir, "bin", "cutlass"))
	if st, err := os.Stat(cutlassBin); err == nil && st.Mode().Perm()&0o111 != 0 {
		e.Vars["CUTLASS_DIR"], e.Vars["CUTLASS_BIN"], e.Vars["PAGES_COMPOSE"] = cutlassDir, cutlassBin, "1"
	} else {
		o.Log.Info("compose panel OFF (no cutlass binary at " + cutlassBin + " — run 'make build' in cutlass to enable)")
	}

	o.Log.Info("migrate")
	d, err := db.Open(e.DSN, db.BoundsFromEnv())
	if err != nil {
		return nil, err
	}
	_, err = migrate.Run(ctx, d.Pool, pages.Migrations(), migrate.Options{LockTimeout: 30 * time.Second, Log: io.Discard})
	d.Close()
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if b, _ := os.ReadFile(e.TokenFile); strings.TrimSpace(string(b)) == "" {
		o.Log.Info("minting agent token 'dev-agent'")
		out, err := runner.Output(ctx, o.Runner, runner.Cmd{Name: o.Self, Args: []string{"token", "add", "dev-agent", "deploy"}, Env: e.Environ()})
		tok := tokenPattern.FindString(out)
		if err != nil || tok == "" {
			return nil, fmt.Errorf("could not mint the dev agent token (pages token add): %w", err)
		}
		if err := os.WriteFile(e.TokenFile, []byte(tok+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Environ is the server's environment: the caller's, minus any connection
// settings that would point elsewhere, plus the dev variables.
func (e *Env) Environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ours := e.Vars[k]; ours || (strings.HasPrefix(k, "PG") && !e.External) || (k == "DATABASE_URL" && !e.External) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range e.Vars {
		out = append(out, k+"="+v)
	}
	return out
}

func (o *Options) pg(ctx context.Context, name string, args ...string) error {
	c := runner.Cmd{Name: name, Args: args}
	if o.EUID == 0 {
		c = runner.Cmd{Name: "runuser", Args: append([]string{"-u", "postgres", "--", name}, args...)}
	}
	_, err := o.Runner.Run(ctx, c)
	return err
}

// cluster runs the private trust-auth cluster (role pages, socket only).
func (o *Options) cluster(ctx context.Context, e *Env) error {
	root := o.get("PAGES_DEV_DIR", "/var/tmp/pages-dev")
	data := filepath.Join(root, "pg")
	pgport := o.get("PGPORT", "5499")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	initdb, pgctl := pgTool("initdb"), pgTool("pg_ctl")
	if b, err := os.ReadFile(filepath.Join(data, "PG_VERSION")); err != nil || len(b) == 0 {
		o.Log.Info("initdb (trust auth, role 'pages') → " + data)
		if o.EUID == 0 {
			if _, err := o.Runner.Run(ctx, runner.Cmd{Name: "chown", Args: []string{"-R", "postgres:postgres", root}}); err != nil {
				return err
			}
		}
		if err := o.pg(ctx, initdb, "-D", data, "-A", "trust", "-U", "pages"); err != nil {
			return fmt.Errorf("initdb: %w", err)
		}
	}
	if o.pg(ctx, pgctl, "-D", data, "status") != nil {
		o.Log.Info("starting Postgres on socket " + root + ":" + pgport)
		if o.EUID == 0 {
			_, _ = o.Runner.Run(ctx, runner.Cmd{Name: "chown", Args: []string{"-R", "postgres:postgres", root}})
		}
		if err := o.pg(ctx, pgctl, "-D", data, "-o", "-p "+pgport+" -k "+root+" -c listen_addresses=''", "-l", filepath.Join(root, "pg.log"), "-w", "start"); err != nil {
			return fmt.Errorf("pg_ctl start: %w", err)
		}
	}
	admin := fmt.Sprintf("host=%s port=%s user=pages dbname=postgres sslmode=disable", root, pgport)
	if err := ensureDatabase(ctx, admin, "pages", o.Log); err != nil {
		return err
	}
	e.DSN = fmt.Sprintf("host=%s port=%s user=pages dbname=pages sslmode=disable", root, pgport)
	for k, v := range map[string]string{"PGHOST": root, "PGPORT": pgport, "PGUSER": "pages", "PGDATABASE": "pages"} {
		e.Vars[k] = v
	}
	return nil
}

// external uses the developer's own PostgreSQL (Homebrew on macOS).
func (o *Options) external(ctx context.Context, e *Env) error {
	me := "postgres"
	if u, err := user.Current(); err == nil {
		me = u.Username
	}
	host, port, pguser, name := o.get("PGHOST", "127.0.0.1"), o.get("PGPORT", "5432"), o.get("PGUSER", me), o.get("PGDB", "pages")
	url := o.get("PAGES_DEV_DATABASE_URL", o.get("DATABASE_URL", fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable", pguser, host, port, name)))
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("dev database URL: %w", err)
	}
	admin := cfg.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return fmt.Errorf("cannot reach Postgres at %s:%d as '%s' — start it (brew services start postgresql@16)", cfg.Host, cfg.Port, cfg.User)
	}
	conn.Close(ctx)
	if err := ensureDatabaseConfig(ctx, admin, cfg.Database, o.Log); err != nil {
		return err
	}
	e.DSN = url
	e.Vars["DATABASE_URL"] = url
	return nil
}

func ensureDatabase(ctx context.Context, dsn, name string, log *term.Printer) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	return ensureDatabaseConfig(ctx, cfg, name, log)
}

func ensureDatabaseConfig(ctx context.Context, cfg *pgx.ConnConfig, name string, log *term.Printer) error {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	log.Info("createdb " + name)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}

// pgTool finds a PostgreSQL program on PATH or under the versioned trees.
func pgTool(name string) string {
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), globDesc("/usr/lib/postgresql/*/bin")...) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, dir := range append(globDesc("/usr/pgsql-*/bin"), globDesc("/opt/homebrew/opt/postgresql*/bin")...) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func globDesc(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	for i, j := 0, len(m)-1; i < j; i, j = i+1, j-1 {
		m[i], m[j] = m[j], m[i]
	}
	return m
}

// Banner is what `pages dev` prints before the server starts.
func Banner(e *Env, token string) string {
	port := e.Vars["PORT"]
	db := "unix socket " + e.Vars["PGHOST"] + ":" + e.Vars["PGPORT"]
	if e.External {
		db = e.Vars["DATABASE_URL"]
	}
	return fmt.Sprintf(`
  Pages dev environment ready.
    dashboard host : http://localhost:%[1]s       (Host: localhost)
    content host   : http://content.localhost:%[1]s
    database       : %[2]s
    agent token    : %[3]s
                     (also saved in .devdata/agent-token)

  MCP endpoint (e.g. for cutlass):
    PAGES_MCP_URL=http://127.0.0.1:%[1]s/mcp
    PAGES_MCP_TOKEN=%[3]s

  Admin GUI (local dev-login, then pick any page slug):
    http://localhost:%[1]s/__dev/login?next=/admin/<slug>
    (signs you in as %[4]s via a throwaway dev keypair)

  Example — deploy a page as the agent:
    TOKEN=$(pages dev token)
    curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" \
         -H 'Content-Type: application/json' \
         -d '{"slug":"demo","title":"Demo"}' http://127.0.0.1:%[1]s/api/v1/pages

  Ctrl-C stops the server (Postgres keeps running; data persists).

`, port, db, token, e.Vars["DEV_ADMIN_EMAIL"])
}
