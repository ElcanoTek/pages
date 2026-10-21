// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/install"
)

// DefaultDestination is where `pages backup` writes without an argument.
const DefaultDestination = "/var/backups/pages"

// requiredSecrets must be defined BY THE SERVICE ENVIRONMENT FILE: a backup
// without them cannot restore working tokens or sessions, and a value the
// calling shell happens to carry must not mask their absence.
var requiredSecrets = []string{"RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "PAGE_COOKIE_SECRET"}

// Capture is `pages backup [destination]` for an installation: it takes the
// updater's lock SHARED (a running update makes it decline; concurrent
// backups are fine), loads the service environment without evaluating it,
// and creates the backup. It returns the published directory.
func Capture(ctx context.Context, o Options, s install.Settings, destination string) (string, error) {
	o.defaults()
	if destination == "" {
		destination = DefaultDestination
	}
	if st, err := os.Stat(s.AppDir); err != nil || !st.IsDir() {
		return "", errors.New("installed application and service environment are required")
	}
	if st, err := os.Stat(s.EnvFile); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("installed application and service environment are required")
	}
	lock, err := install.Lock(s.LockFile(), false)
	if err != nil {
		return "", errors.New("a Pages update is running; retry the backup after it finishes")
	}
	defer lock.Close()
	// Snapshot the configuration once: the digest is of exactly the bytes
	// parsed, so a file edited in between is caught by Create.
	raw, err := os.ReadFile(s.EnvFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	app, err := filepath.EvalSymlinks(s.AppDir)
	if err != nil {
		return "", err
	}
	// The service environment replaces, never merges with, the caller's
	// secrets and connection settings.
	serviceEnv := map[string]string{}
	for k, v := range envMap(o.Environ) {
		if strings.HasPrefix(k, "PG") || k == "DATABASE_URL" || contains(requiredSecrets, k) {
			continue
		}
		serviceEnv[k] = v
	}
	file := envfile.Parse(string(raw))
	for _, e := range file.Entries {
		serviceEnv[e.Key] = e.Value
	}
	for _, k := range requiredSecrets {
		if file.Value(k) == "" {
			return "", fmt.Errorf("service environment must define %s before backup", k)
		}
	}
	return Create(ctx, o, CreateInput{
		Destination: destination, Application: app, EnvFile: s.EnvFile, InstallConfig: s.InstallConfig,
		AppUser: s.AppUser, EnvDigest: hex.EncodeToString(sum[:]), ServiceEnv: serviceEnv,
	})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
