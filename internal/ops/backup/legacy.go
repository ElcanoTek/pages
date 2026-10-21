// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/migrate"
	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
	"github.com/ElcanoTek/pages/internal/version"
)

// Node-era backups are the ones the Node release's scripts/backup.js wrote
// before the Go rewrite: the same format_version 1 directory layout, but the
// manifest records node_version (no pages_version/go_version) and the
// captured application is the Node tree (server.js, package-lock.json,
// no binary). Operators keep those for weeks after upgrading, so
// check-integrity verifies everything that does not need the captured Node
// runtime and serves the restored data with THIS release instead.

// NodeEraFormat is CheckResult.Format for a Node-era backup.
const NodeEraFormat = "node"

// NodeEraNotice is reported (CheckResult.Notice, and by the CLI on stderr)
// whenever a Node-era backup is verified.
const NodeEraNotice = "legacy Node-era backup: application verified by manifest only; served with this release"

// NodeEraRequiredInputs are the recovery inputs scripts/backup.js required.
var NodeEraRequiredInputs = []string{"database.dump", "application/server.js", "application/package-lock.json", "config/service.env"}

// NodeEraManifest is manifest.json as scripts/backup.js wrote it.
type NodeEraManifest struct {
	FormatVersion         int               `json:"format_version"`
	Status                string            `json:"status"`
	StartedAt             string            `json:"started_at"`
	CompletedAt           string            `json:"completed_at"`
	RuntimeConfiguration  map[string]string `json:"runtime_configuration"`
	NodeVersion           string            `json:"node_version"`
	PgDumpVersion         string            `json:"pg_dump_version"`
	OptionalConfiguration map[string]bool   `json:"optional_configuration"`
	ApplicationSource     string            `json:"application_source"`
	Restored              json.RawMessage   `json:"restored"`
	Files                 []FileEntry       `json:"files"`
}

// IsNodeEra reports whether manifest (manifest.json's bytes) was written by
// the Node release: it names node_version and neither Go field. A manifest
// claiming both is refused rather than guessed at.
func IsNodeEra(manifest []byte) (bool, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &keys); err != nil {
		return false, fmt.Errorf("manifest.json: %w", err)
	}
	_, node := keys["node_version"]
	_, goVersion := keys["go_version"]
	_, pagesVersion := keys["pages_version"]
	if node && (goVersion || pagesVersion) {
		return false, errors.New("unsupported or incomplete backup manifest")
	}
	return node, nil
}

// VerifyNodeEraFiles is scripts/backup.js verifyFiles(): format and
// completeness, the required sections, every file's length and digest (an
// extra, missing or repeated file fails too), the Node release's required
// recovery inputs, and the optional configuration it recorded. The JS
// listed files in localeCompare order and Go lists them in byte order, so
// the inventories are compared as sets keyed by path.
func VerifyNodeEraFiles(directory string, m *NodeEraManifest) ([]FileEntry, error) {
	if m == nil || m.FormatVersion != 1 || m.Status != "complete" || m.Files == nil || !strings.HasPrefix(m.NodeVersion, "v") {
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
	byPath := func(files []FileEntry) []FileEntry {
		out := append([]FileEntry(nil), files...)
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return out
	}
	recorded, found := byPath(m.Files), byPath(actual)
	mismatch := errors.New("backup file inventory/checksum mismatch; do not restore this artifact")
	if len(recorded) != len(found) {
		return nil, mismatch
	}
	for i := range recorded {
		if recorded[i] != found[i] {
			return nil, mismatch
		}
	}
	present := map[string]bool{}
	for _, f := range actual {
		present[f.Path] = true
	}
	for _, req := range NodeEraRequiredInputs {
		if !present[req] {
			return nil, fmt.Errorf("backup is missing required recovery input: %s", req)
		}
	}
	for _, name := range []string{"install.env", "local.env"} {
		if flag, ok := m.OptionalConfiguration[name]; ok && flag != present["config/"+name] {
			return nil, fmt.Errorf("backup optional configuration disagrees with its files: config/%s", name)
		}
	}
	return actual, nil
}

// checkNodeEra is Check for a Node-era backup. The captured Node application
// is verified byte for byte by the manifest but cannot be run by this
// release, so the rehearsal restores the dump, runs the probe's database
// half on the snapshot exactly as captured, brings the scratch copy up to
// this release's schema (what restoring onto this release does) and serves
// it with this release for the HTTP half. Together they must reproduce the
// result the Node rehearsal recorded.
func checkNodeEra(ctx context.Context, o Options, dir string, raw []byte) (*CheckResult, error) {
	o.defaults()
	var m NodeEraManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if _, err := VerifyNodeEraFiles(dir, &m); err != nil {
		return nil, err
	}
	assets := filepath.Join(dir, "assets")
	restored, err := rehearse(ctx, o, dir, m.RuntimeConfiguration, func(ctx context.Context, _ string, env []string) (json.RawMessage, error) {
		return currentReleaseProbe(ctx, o, assets, env)
	})
	if err != nil {
		return nil, err
	}
	var want bytes.Buffer
	if err := json.Compact(&want, m.Restored); err != nil || !bytes.Equal(want.Bytes(), restored) {
		return nil, errors.New("restored database inventory differs from the backup manifest")
	}
	return &CheckResult{Status: "verified", BackupDir: dir, Format: NodeEraFormat, Notice: NodeEraNotice,
		ServedWith: version.String(), Restored: restored}, nil
}

// currentReleaseProbe produces the probe report for the restored cluster env
// targets without running any captured code: counts, migrations, hashes and
// assets from the snapshot as restored (before any migration), probes from
// THIS release's server over a migrated scratch copy. It serves Node-era
// backups and Go backups whose binary matches no trusted installed release.
func currentReleaseProbe(ctx context.Context, o Options, assets string, env []string) (json.RawMessage, error) {
	e := envMap(env)
	pool, err := pgxpool.New(ctx, fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable",
		dsnQuote(e["PGHOST"]), dsnQuote(e["PGPORT"]), dsnQuote(e["PGUSER"]), dsnQuote(e["PGDATABASE"])))
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	res, err := backupprobe.Inspect(ctx, pool, assets)
	if err != nil {
		return nil, err
	}
	known, err := migrate.Files(pages.Migrations())
	if err != nil {
		return nil, err
	}
	release := map[string]bool{}
	for _, f := range known {
		release[f] = true
	}
	for _, f := range res.Migrations {
		if !release[f] {
			return nil, fmt.Errorf("backup schema includes migration %s, which this release does not have; check it with a release that does", f)
		}
	}
	if _, err := migrate.Run(ctx, pool, pages.Migrations(), migrate.Options{LockTimeout: 30 * time.Second, Log: io.Discard}); err != nil {
		return nil, fmt.Errorf("restored database cannot be migrated to this release: %w", err)
	}
	pool.Close()
	out, err := o.CurrentProbe(ctx, assets, env)
	if err != nil {
		return nil, err
	}
	var served struct {
		Probes json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(out, &served); err != nil || served.Probes == nil {
		return nil, errors.New("restore probe returned invalid JSON")
	}
	if err := json.Unmarshal(served.Probes, &res.Probes); err != nil {
		return nil, fmt.Errorf("restore probe returned invalid JSON: %w", err)
	}
	// JSON.stringify never escapes <, > or &; nor does this report.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

func dsnQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// execCurrentProbe runs this executable's `backup-probe`, which serves the
// restored data in process with this release's server.
func (o *Options) execCurrentProbe(ctx context.Context, assets string, env []string) (json.RawMessage, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	out, err := Run(ctx, o.Runner, self, []string{"backup-probe", assets}, env, "")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}
