// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pages

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// flagMotionDir is the one first-party tree inside public/assets/flag: the
// Flag Motion kit (docs/MOTION.md) is Pages code, not a Flag file, and lives
// there only because page authors load it from /assets/flag/motion/.
const flagMotionDir = "assets/flag/motion/"

// TestEmbeddedFlagMatchesLock checks the binary's copy of the vendored Flag
// tree against flag.lock: every locked file is embedded with its locked hash,
// and nothing else sits under public/assets/flag except the Flag Motion kit.
// scripts/flag-sync.sh check covers the working tree; this covers what ships,
// including public/assets/flag/logos, which the lock lists file by file (only
// the two marks Pages uses) rather than as a whole directory.
func TestEmbeddedFlagMatchesLock(t *testing.T) {
	f, err := os.Open("flag.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	locked := map[string]string{} // path under public/ -> sha256
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 || fields[0] != "file" {
			continue
		}
		rel, ok := strings.CutPrefix(fields[3], "public/")
		if !ok {
			t.Fatalf("flag.lock vendors %s outside public/, which embed.go does not ship", fields[3])
		}
		locked[rel] = fields[1]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(locked) == 0 {
		t.Fatal("flag.lock locks no files")
	}

	pub := Public()
	for rel, want := range locked {
		b, err := fs.ReadFile(pub, rel)
		if err != nil {
			t.Errorf("locked Flag file %s is not embedded: %v", rel, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("embedded %s has sha256 %s, flag.lock says %s; run scripts/flag-sync.sh sync, never hand-edit", rel, got, want)
		}
	}

	err = fs.WalkDir(pub, "assets/flag", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := locked[p]; !ok && !strings.HasPrefix(p, flagMotionDir) {
			t.Errorf("public/%s is in the vendored Flag tree but not in flag.lock: add it from flag with scripts/flag-sync.sh add, or move product files out", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The exemption above must not hide the kit's absence: page authors load it.
	for _, p := range []string{flagMotionDir + "flag-motion.css", flagMotionDir + "flag-motion.js"} {
		if _, err := fs.Stat(pub, p); err != nil {
			t.Errorf("Flag Motion kit file %s is missing: %v", p, err)
		}
	}
}
