// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package backupprobe is `pages backup-probe`, the restore check a backup
// rehearsal runs with the CAPTURED release against its private restored
// cluster. It never loads captured credentials, migrates, publishes or
// executes page JS. It checks every immutable source hash, the referenced
// asset bytes and ownership pointers, then serves the restored data and
// requests a bounded, active-first sample of pages, versions, template
// previews and portals with synthetic credentials.
package backupprobe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages/internal/db"
)

// Hooks connect the probe to the server and credential code. Each nil hook
// has a default: Serve runs `<this binary> serve` on a loopback port, and
// the minters produce the wire formats lib/rawtoken.js and lib/pagecookie.js
// define (those formats are a compatibility contract: tokens and cookies
// issued before an upgrade must keep verifying). The server packages may set
// these to their own implementations.
var Hooks struct {
	// Serve serves the application on ln until ctx ends.
	Serve func(ctx context.Context, ln net.Listener) error
	// RawToken mints a /raw access token (purpose view or template).
	RawToken func(pageID, versionID any, purpose, renderMode string) (string, error)
	// PageSession mints a content-host page session cookie value.
	PageSession func(pageID int64, ttlSeconds int64, passwordHash *string) string
	// PortalSession mints a portal session cookie value.
	PortalSession func(portalID int64, ttlSeconds int64, passwordHash *string) string
}

// Counts keeps the table order of the query (JSON object, name order).
type Counts []struct{ Table, Count string }

// MarshalJSON renders Counts as an object in table order.
func (c Counts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.Table)
		v, _ := json.Marshal(kv.Count)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Result is the probe's JSON report.
type Result struct {
	Counts     Counts   `json:"counts"`
	Migrations []string `json:"migrations"`
	Hashes     struct {
		Versions         int `json:"versions"`
		TemplateVersions int `json:"template_versions"`
	} `json:"hashes"`
	Assets int `json:"assets"`
	Probes struct {
		Pages       int `json:"pages"`
		Versions    int `json:"versions"`
		Templates   int `json:"templates"`
		Portals     int `json:"portals"`
		PortalPages int `json:"portal_pages"`
	} `json:"probes"`
}

func ensure(ok bool, format string, args ...any) error {
	if ok {
		return nil
	}
	return fmt.Errorf(format, args...)
}

var hexName = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Run probes the database behind the PG* environment, with assets the
// restored assets directory.
func Run(ctx context.Context, assets string) (*Result, error) {
	d, err := db.Open("", db.BoundsFromEnv())
	if err != nil {
		return nil, err
	}
	defer d.Close()
	res, err := Inspect(ctx, d.Pool, assets)
	if err != nil {
		return nil, err
	}
	srv, err := startServer(ctx)
	if err != nil {
		return nil, err
	}
	defer srv.stop()
	if err := httpProbes(ctx, d.Pool, srv, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Inspect is the database half of the probe: ownership pointers, every
// immutable source hash, referenced asset bytes, table counts and the
// migration inventory — all read from the restored snapshot.
func Inspect(ctx context.Context, pool *pgxpool.Pool, assets string) (*Result, error) {
	var err error
	res := &Result{Migrations: []string{}, Counts: Counts{}}
	var invalid int64
	if err := pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM pages p JOIN page_versions v ON v.id=p.published_version_id WHERE v.page_id<>p.id) +
    (SELECT count(*) FROM page_templates t JOIN page_template_versions v ON v.id=t.current_version_id WHERE v.template_id<>t.id) +
    (SELECT count(*) FROM preview_links l JOIN page_versions v ON v.id=l.version_id WHERE v.page_id<>l.page_id)`).Scan(&invalid); err != nil {
		return nil, err
	}
	if err := ensure(invalid == 0, "restored pointers cross page or template ownership"); err != nil {
		return nil, err
	}
	if res.Hashes.Versions, err = checkHashes(ctx, pool, "page_versions"); err != nil {
		return nil, err
	}
	if res.Hashes.TemplateVersions, err = checkHashes(ctx, pool, "page_template_versions"); err != nil {
		return nil, err
	}
	if res.Assets, err = checkAssets(ctx, pool, assets); err != nil {
		return nil, err
	}
	var orphan bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM themes t LEFT JOIN assets a ON a.sha256=t.logo_sha256 WHERE t.logo_sha256 IS NOT NULL AND a.sha256 IS NULL)").Scan(&orphan); err != nil {
		return nil, err
	}
	if err := ensure(!orphan, "theme references a missing asset"); err != nil {
		return nil, err
	}
	tables, err := strings1(ctx, pool, "SELECT tablename::text FROM pg_tables WHERE schemaname='public' ORDER BY tablename")
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		quoted := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		var n string
		if err := pool.QueryRow(ctx, "SELECT count(*)::text FROM public."+quoted).Scan(&n); err != nil {
			return nil, err
		}
		res.Counts = append(res.Counts, struct{ Table, Count string }{t, n})
	}
	if res.Migrations, err = strings1(ctx, pool, "SELECT filename FROM schema_migrations ORDER BY filename"); err != nil {
		return nil, err
	}
	return res, nil
}

func strings1(ctx context.Context, pool *pgxpool.Pool, sql string) ([]string, error) {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func checkHashes(ctx context.Context, pool *pgxpool.Pool, table string) (int, error) {
	var last int64
	checked := 0
	for {
		rows, err := pool.Query(ctx, "SELECT id, html, content_sha256 FROM "+table+" WHERE id>$1 ORDER BY id LIMIT 20", last)
		if err != nil {
			return 0, err
		}
		n := 0
		for rows.Next() {
			var id int64
			var html, sum string
			if err := rows.Scan(&id, &html, &sum); err != nil {
				rows.Close()
				return 0, err
			}
			h := sha256.Sum256([]byte(html))
			if hex.EncodeToString(h[:]) != sum {
				rows.Close()
				return 0, fmt.Errorf("%s content hash mismatch at id %d", table, id)
			}
			checked++
			n++
			last = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		if n == 0 {
			return checked, nil
		}
	}
}

func checkAssets(ctx context.Context, pool *pgxpool.Pool, assets string) (int, error) {
	rows, err := pool.Query(ctx, "SELECT sha256, bytes FROM assets ORDER BY sha256")
	if err != nil {
		return 0, err
	}
	type asset struct {
		sum   string
		bytes int64
	}
	var list []asset
	for rows.Next() {
		var a asset
		if err := rows.Scan(&a.sum, &a.bytes); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, a := range list {
		if !hexName.MatchString(a.sum) {
			return 0, errors.New("asset has an invalid content-addressed name")
		}
		file := filepath.Join(assets, a.sum)
		st, err := os.Lstat(file)
		if err != nil || !st.Mode().IsRegular() || st.Size() != a.bytes {
			return 0, fmt.Errorf("asset length mismatch: %s", a.sum)
		}
		f, err := os.Open(file)
		if err != nil {
			return 0, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil || hex.EncodeToString(h.Sum(nil)) != a.sum {
			return 0, fmt.Errorf("asset checksum mismatch: %s", a.sum)
		}
	}
	return len(list), nil
}

type server struct {
	base   string
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *server) stop() {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
}

// startServer serves the restored data on a loopback port: in process when
// Hooks.Serve is set, else as `<this binary> serve` with PORT set.
func startServer(ctx context.Context) (*server, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	sctx, cancel := context.WithCancel(ctx)
	s := &server{base: "http://127.0.0.1:" + strconv.Itoa(port), cancel: cancel, done: make(chan struct{})}
	if Hooks.Serve != nil {
		go func() {
			defer close(s.done)
			_ = Hooks.Serve(sctx, ln)
		}()
	} else {
		ln.Close()
		self, err := os.Executable()
		if err != nil {
			cancel()
			return nil, err
		}
		cmd := exec.CommandContext(sctx, self, "serve")
		cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port))
		cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		if err := cmd.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("captured release could not start its server: %w", err)
		}
		go func() {
			defer close(s.done)
			_ = cmd.Wait()
		}()
	}
	// Wait for the listener (liveness, not readiness: readiness is probed).
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, _, err := s.get(ctx, "/healthz", "localhost", ""); err == nil && st > 0 {
			return s, nil
		}
		select {
		case <-s.done:
			cancel()
			return nil, errors.New("captured release server exited before listening")
		default:
		}
		if time.Now().After(deadline) {
			s.stop()
			return nil, errors.New("captured release server did not start listening")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *server) get(ctx context.Context, path, host, cookie string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return 0, "", errors.New("restored application request timed out")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), nil
}

const content = "content.localhost"

func httpProbes(ctx context.Context, pool *pgxpool.Pool, s *server, res *Result) error {
	if st, _, _ := s.get(ctx, "/readyz", "localhost", ""); st != 200 {
		return errors.New("captured release is not ready against the restored schema; no migrations were applied")
	}
	if st, _, _ := s.get(ctx, "/assets/flag/tokens/design-tokens.css", content, ""); st != 200 {
		return errors.New("captured public assets cannot be served")
	}
	rawToken, pageSession, portalSession := minters()

	rows, err := pool.Query(ctx, `SELECT p.id, p.slug, p.disabled, p.deleted_at IS NOT NULL, p.password_hash, p.published_version_id IS NOT NULL FROM pages p
    WHERE p.deleted_at IS NULL OR NOT EXISTS (SELECT 1 FROM pages live WHERE live.slug=p.slug AND live.deleted_at IS NULL)
    ORDER BY (p.deleted_at IS NULL AND NOT p.disabled AND p.published_version_id IS NOT NULL) DESC, p.id LIMIT 5`)
	if err != nil {
		return err
	}
	type page struct {
		id                         int64
		slug                       string
		disabled, deleted, publish bool
		hash                       *string
	}
	var pages []page
	for rows.Next() {
		var p page
		if err := rows.Scan(&p.id, &p.slug, &p.disabled, &p.deleted, &p.hash, &p.publish); err != nil {
			rows.Close()
			return err
		}
		pages = append(pages, p)
	}
	rows.Close()
	for _, p := range pages {
		expected := 403
		switch {
		case p.disabled || p.deleted || !p.publish:
			expected = 404
		case p.hash != nil:
			expected = 200
		}
		cookie := ""
		if p.hash != nil {
			cookie = fmt.Sprintf("pgs%d=%s", p.id, pageSession(p.id, 60, p.hash))
		}
		st, _, _ := s.get(ctx, "/"+p.slug, content, cookie)
		if st != expected {
			return fmt.Errorf("restored page %d returned %d, expected %d", p.id, st, expected)
		}
		res.Probes.Pages++
	}

	vrows, err := pool.Query(ctx, `SELECT v.id, v.page_id, v.render_mode, p.slug, p.disabled OR p.deleted_at IS NOT NULL
    FROM page_versions v JOIN pages p ON p.id=v.page_id
    ORDER BY (p.deleted_at IS NULL AND NOT p.disabled) DESC, (v.id=p.published_version_id) DESC NULLS LAST, v.id LIMIT 5`)
	if err != nil {
		return err
	}
	type version struct {
		id, pageID int64
		mode, slug string
		gone       bool
	}
	var versions []version
	for vrows.Next() {
		var v version
		if err := vrows.Scan(&v.id, &v.pageID, &v.mode, &v.slug, &v.gone); err != nil {
			vrows.Close()
			return err
		}
		versions = append(versions, v)
	}
	vrows.Close()
	for _, v := range versions {
		// node-postgres returned BIGINT as strings; the probe minted them
		// that way, so it still does.
		token, err := rawToken(strconv.FormatInt(v.pageID, 10), strconv.FormatInt(v.id, 10), "view", v.mode)
		if err != nil {
			return err
		}
		expected := 200
		if v.gone {
			expected = 404
		}
		st, _, _ := s.get(ctx, "/raw/"+v.slug+"?t="+url.QueryEscape(token), content, "")
		if st != expected {
			return fmt.Errorf("restored version %d cannot be served (%d)", v.id, st)
		}
		res.Probes.Versions++
	}

	trows, err := pool.Query(ctx, `SELECT v.id, t.deleted_at IS NOT NULL FROM page_template_versions v
    JOIN page_templates t ON t.id=v.template_id
    ORDER BY (t.deleted_at IS NULL) DESC, (v.id=t.current_version_id) DESC NULLS LAST, v.id LIMIT 5`)
	if err != nil {
		return err
	}
	type tmpl struct {
		id      int64
		deleted bool
	}
	var templates []tmpl
	for trows.Next() {
		var t tmpl
		if err := trows.Scan(&t.id, &t.deleted); err != nil {
			trows.Close()
			return err
		}
		templates = append(templates, t)
	}
	trows.Close()
	for _, t := range templates {
		token, err := rawToken(0, strconv.FormatInt(t.id, 10), "template", "themed")
		if err != nil {
			return err
		}
		expected := 200
		if t.deleted {
			expected = 404
		}
		st, _, _ := s.get(ctx, fmt.Sprintf("/raw-template/%d?t=%s", t.id, url.QueryEscape(token)), content, "")
		if st != expected {
			return fmt.Errorf("restored template revision %d cannot be previewed (%d)", t.id, st)
		}
		res.Probes.Templates++
	}

	prows, err := pool.Query(ctx, `SELECT p.id, p.slug, p.password_hash, p.deleted_at IS NOT NULL FROM page_portals p
    WHERE p.deleted_at IS NULL OR NOT EXISTS (SELECT 1 FROM page_portals live WHERE live.slug=p.slug AND live.deleted_at IS NULL)
    ORDER BY (p.deleted_at IS NULL) DESC, p.id LIMIT 5`)
	if err != nil {
		return err
	}
	type portal struct {
		id      int64
		slug    string
		hash    *string
		deleted bool
	}
	var portals []portal
	for prows.Next() {
		var p portal
		if err := prows.Scan(&p.id, &p.slug, &p.hash, &p.deleted); err != nil {
			prows.Close()
			return err
		}
		portals = append(portals, p)
	}
	prows.Close()
	for _, p := range portals {
		cookie := fmt.Sprintf("pgp%d=%s", p.id, portalSession(p.id, 60, p.hash))
		expected := 200
		if p.deleted {
			expected = 404
		}
		st, _, _ := s.get(ctx, "/portal/"+p.slug, content, cookie)
		if st != expected {
			return fmt.Errorf("restored portal %d cannot be served (%d)", p.id, st)
		}
		res.Probes.Portals++
		if p.deleted {
			continue
		}
		members, err := strings1Args(ctx, pool, `SELECT p.id::text || ' ' || p.slug
       FROM page_portal_members m
       JOIN page_portals pp ON pp.id = m.portal_id
       JOIN pages p ON p.id = m.page_id
       LEFT JOIN page_versions v ON v.id = p.published_version_id
      WHERE m.portal_id = $1 AND p.deleted_at IS NULL AND NOT p.disabled AND p.published_version_id IS NOT NULL
      ORDER BY (pp.home_page_id = p.id) DESC, m.sort_order, lower(COALESCE(m.label, NULLIF(p.title, ''), p.slug)), p.slug`, p.id)
		if err != nil {
			return err
		}
		if len(members) > 5 {
			members = members[:5]
		}
		for _, member := range members {
			id, slug, _ := strings.Cut(member, " ")
			if st, _, _ := s.get(ctx, "/"+slug, content, cookie); st != 200 {
				return fmt.Errorf("restored portal %d cannot serve member %s", p.id, id)
			}
			res.Probes.PortalPages++
		}
	}
	return nil
}

func strings1Args(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]string, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func minters() (func(pageID, versionID any, purpose, mode string) (string, error), func(int64, int64, *string) string, func(int64, int64, *string) string) {
	raw, page, portal := Hooks.RawToken, Hooks.PageSession, Hooks.PortalSession
	if raw == nil {
		raw = MintRawToken
	}
	if page == nil {
		page = MintPageSession
	}
	if portal == nil {
		portal = MintPortalSession
	}
	return raw, page, portal
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func digest16(secret, text string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(text))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

func marshal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// MintRawToken is lib/rawtoken.js mint(): base64url(JSON{pid, vid, purpose,
// mode, sid, exp}) + "." + base64url(HMAC-SHA256(RAW_TOKEN_SECRET, body)),
// valid for 300 seconds.
func MintRawToken(pageID, versionID any, purpose, mode string) (string, error) {
	secret := os.Getenv("RAW_TOKEN_SECRET")
	if secret == "" {
		return "", errors.New("RAW_TOKEN_SECRET is not set")
	}
	var sid [8]byte
	_, _ = rand.Read(sid[:])
	body := base64.RawURLEncoding.EncodeToString([]byte(marshal(struct {
		Pid     any    `json:"pid"`
		Vid     any    `json:"vid"`
		Purpose string `json:"purpose"`
		Mode    string `json:"mode"`
		Sid     string `json:"sid"`
		Exp     int64  `json:"exp"`
	}{pageID, versionID, purpose, mode, hex.EncodeToString(sid[:]), time.Now().Unix() + 300})))
	return body + "." + sign(secret, body), nil
}

func hashText(h *string) string {
	if h == nil {
		return ""
	}
	return *h
}

// MintPageSession is lib/pagecookie.js mintSession().
func MintPageSession(pageID, ttl int64, passwordHash *string) string {
	secret := os.Getenv("PAGE_COOKIE_SECRET")
	body := base64.RawURLEncoding.EncodeToString([]byte(marshal(struct {
		Pid int64  `json:"pid"`
		Exp int64  `json:"exp"`
		Cd  string `json:"cd"`
	}{pageID, time.Now().Unix() + ttl, digest16(secret, "cred:"+hashText(passwordHash))})))
	return body + "." + sign(secret, body)
}

// MintPortalSession is lib/pagecookie.js mintPortalSession().
func MintPortalSession(portalID, ttl int64, passwordHash *string) string {
	secret := os.Getenv("PAGE_COOKIE_SECRET")
	body := base64.RawURLEncoding.EncodeToString([]byte(marshal(struct {
		Poid int64  `json:"poid"`
		Exp  int64  `json:"exp"`
		Cd   string `json:"cd"`
	}{portalID, time.Now().Unix() + ttl, digest16(secret, "cred:portal:"+hashText(passwordHash))})))
	return body + "." + sign(secret, "portal."+body)
}
