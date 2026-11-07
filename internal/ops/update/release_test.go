// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

type fakeVerifier struct {
	verified []string
	failOn   string
}

func (f *fakeVerifier) VerifyBlob(_ context.Context, path, bundle string) error {
	name := filepath.Base(path)
	f.verified = append(f.verified, name)
	raw, err := os.ReadFile(bundle)
	if err != nil {
		return err
	}
	if string(raw) != "SIG:"+name {
		return fmt.Errorf("bad bundle for %s", name)
	}
	if f.failOn == name {
		return fmt.Errorf("synthetic signature failure on %s", name)
	}
	return nil
}

// minimalELF returns a valid 64-bit little-endian ELF header for arch
// ("amd64" -> EM_X86_64=62, "arm64" -> EM_AARCH64=183) followed by payload.
func minimalELF(arch string, payload string) []byte {
	hdr := make([]byte, 64)
	copy(hdr[0:4], []byte{0x7f, 'E', 'L', 'F'})
	hdr[4] = 2                                   // ELFCLASS64
	hdr[5] = 1                                   // ELFDATA2LSB
	hdr[6] = 1                                   // EV_CURRENT
	binary.LittleEndian.PutUint16(hdr[16:18], 2) // ET_EXEC
	var machine uint16 = 62
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(hdr[18:20], machine)
	binary.LittleEndian.PutUint32(hdr[20:24], 1) // EV_CURRENT
	binary.LittleEndian.PutUint16(hdr[52:54], 64)
	return append(hdr, []byte(payload)...)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type releaseFixture struct {
	tag    Tag
	commit string
	files  map[string][]byte
}

func newReleaseFixture(t *testing.T, tagStr, commit string) *releaseFixture {
	t.Helper()
	tag, err := ParseTag(tagStr)
	if err != nil {
		t.Fatal(err)
	}
	amd64Bin := minimalELF("amd64", "pages-amd64-"+tagStr)
	arm64Bin := minimalELF("arm64", "pages-arm64-"+tagStr)
	man := Manifest{
		Schema:  1,
		Tag:     tagStr,
		Version: strings.TrimPrefix(tagStr, "v"),
		Commit:  commit,
		BuiltAt: "2026-11-07T00:00:00Z",
		Binaries: map[string]ManifestBinary{
			"linux/amd64": {Name: "pages-linux-amd64", SHA256: sha256Hex(amd64Bin)},
			"linux/arm64": {Name: "pages-linux-arm64", SHA256: sha256Hex(arm64Bin)},
		},
	}
	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sums := fmt.Sprintf("%s  pages-linux-amd64\n%s  pages-linux-arm64\n%s  release.json\n",
		sha256Hex(amd64Bin), sha256Hex(arm64Bin), sha256Hex(manBytes))
	files := map[string][]byte{
		"pages-linux-amd64":               amd64Bin,
		"pages-linux-amd64.sigstore.json": []byte("SIG:pages-linux-amd64"),
		"pages-linux-arm64":               arm64Bin,
		"pages-linux-arm64.sigstore.json": []byte("SIG:pages-linux-arm64"),
		"release.json":                    manBytes,
		"SHA256SUMS":                      []byte(sums),
		"SHA256SUMS.sigstore.json":        []byte("SIG:SHA256SUMS"),
	}
	return &releaseFixture{tag: tag, commit: commit, files: files}
}

func (rf *releaseFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name":%q}`, rf.tag.String())
			return
		}
		prefix := "/download/" + rf.tag.String() + "/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			name := strings.TrimPrefix(r.URL.Path, prefix)
			if data, ok := rf.files[name]; ok {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPinnedCosignAndEnsureCosign(t *testing.T) {
	pin := PinnedCosign()
	if pin.Version != "v3.1.4" {
		t.Fatalf("cosign version = %q, want v3.1.4", pin.Version)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if len(pin.SHA256["linux/"+arch]) != 64 {
			t.Fatalf("missing digest for linux/%s: %+v", arch, pin)
		}
	}

	fakeCosign := []byte("#!/bin/sh\nexit 0\n")
	fakeSum := sha256Hex(fakeCosign)

	// Test EnsureCosign refuses a binary whose SHA-256 does not match cosign.lock.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fakeCosign)
	}))
	defer srv.Close()
	t.Setenv("PAGES_COSIGN_URL", srv.URL)
	cacheDir := filepath.Join(t.TempDir(), "cosign")
	dl := &Downloader{Client: srv.Client()}
	if _, err := EnsureCosign(context.Background(), dl, cacheDir, "amd64"); err == nil || !strings.Contains(err.Error(), "cosign.lock pins") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}

	// Test InstallCosignFrom refuses an unpinned local binary.
	badBin := filepath.Join(t.TempDir(), "bad-cosign")
	if err := os.WriteFile(badBin, fakeCosign, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallCosignFrom(badBin, cacheDir, "amd64"); err == nil || !strings.Contains(err.Error(), fakeSum) {
		t.Fatalf("expected InstallCosignFrom mismatch error, got %v", err)
	}
}

func TestTagParsingOrderingAndVersionScript(t *testing.T) {
	t1, err := ParseTag("v2026.11.07.1")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := ParseTag("v2026.11.07.2")
	if err != nil {
		t.Fatal(err)
	}
	t3, err := ParseTag("v2026.11.08.1")
	if err != nil {
		t.Fatal(err)
	}
	t2Copy, err := ParseTag("v2026.11.07.2")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Compare(t2) >= 0 || t2.Compare(t3) >= 0 || t2.Compare(t2Copy) != 0 {
		t.Fatalf("unexpected tag comparison: %v %v %v", t1, t2, t3)
	}
	for _, bad := range []string{"2026.11.07.1", "v2026.11.07.0", "v2026.11.07.01", "v2026.11.7.1", "dev"} {
		if _, err := ParseTag(bad); err == nil {
			t.Fatalf("expected ParseTag(%q) to fail", bad)
		}
	}
	if v, ok := ParseVersion("2026.11.07.2+3.gabcdef0"); !ok || v.Compare(t2) != 0 {
		t.Fatalf("ParseVersion stamped = %v %v", v, ok)
	}
	if _, ok := ParseVersion("dev"); ok {
		t.Fatal("ParseVersion(dev) must be false")
	}

	// Verify scripts/version.sh filter agrees with ParseTag.
	cmd := exec.Command("bash", "../../../scripts/version.sh", "filter")
	cmd.Stdin = strings.NewReader("v2026.11.07.1\nv2026.11.07.0\nbogus\nv2026.11.07.12\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("version.sh filter: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "v2026.11.07.1\nv2026.11.07.12" {
		t.Fatalf("version.sh filter = %q", got)
	}
}

func TestFetchReleaseAndLoadVerified(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	rf := newReleaseFixture(t, "v2026.11.07.2", commit)
	srv := rf.server(t)
	dl := &Downloader{Client: srv.Client()}

	stage := filepath.Join(t.TempDir(), "stage")
	v := &fakeVerifier{}
	fetched, err := FetchRelease(context.Background(), dl, v, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     stage,
		BaseURL: srv.URL + "/download",
		Current: "v2026.11.07.1",
		Commit:  commit,
	})
	if err != nil {
		t.Fatalf("FetchRelease failed: %v", err)
	}
	_ = fetched.BinaryFile.Close()
	if strings.Join(v.verified, ",") != "SHA256SUMS,pages-linux-amd64" {
		t.Fatalf("verified = %v", v.verified)
	}

	// Re-verify existing directory via LoadVerified.
	v2 := &fakeVerifier{}
	loaded, err := LoadVerified(context.Background(), v2, stage, rf.tag, "amd64", commit)
	if err != nil {
		t.Fatalf("LoadVerified failed: %v", err)
	}
	_ = loaded.BinaryFile.Close()
	if strings.Join(v2.verified, ",") != "SHA256SUMS,pages-linux-amd64" {
		t.Fatalf("LoadVerified verified = %v", v2.verified)
	}

	// Downgrade refused unless AllowDowngrade is set.
	_, err = FetchRelease(context.Background(), dl, &fakeVerifier{}, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     filepath.Join(t.TempDir(), "downgrade"),
		BaseURL: srv.URL + "/download",
		Current: "v2026.11.08.1",
		Commit:  commit,
	})
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("expected ErrDowngrade, got %v", err)
	}

	// Bad SHA256SUMS signature refused.
	_, err = FetchRelease(context.Background(), dl, &fakeVerifier{failOn: "SHA256SUMS"}, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     filepath.Join(t.TempDir(), "badsig"),
		BaseURL: srv.URL + "/download",
		Commit:  commit,
	})
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("expected SHA256SUMS signature failure, got %v", err)
	}

	// Moved tag (commit mismatch) refused.
	_, err = FetchRelease(context.Background(), dl, &fakeVerifier{}, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     filepath.Join(t.TempDir(), "movedtag"),
		BaseURL: srv.URL + "/download",
		Commit:  "fedcba9876543210fedcba9876543210fedcba98",
	})
	if err == nil || !strings.Contains(err.Error(), "tag was moved") {
		t.Fatalf("expected moved-tag failure, got %v", err)
	}

	// Tampered binary bytes refused.
	rfTampered := newReleaseFixture(t, "v2026.11.07.2", commit)
	rfTampered.files["pages-linux-amd64"] = minimalELF("amd64", "tampered")
	srvTampered := rfTampered.server(t)
	_, err = FetchRelease(context.Background(), &Downloader{Client: srvTampered.Client()}, &fakeVerifier{}, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     filepath.Join(t.TempDir(), "tampered"),
		BaseURL: srvTampered.URL + "/download",
		Commit:  commit,
	})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("expected SHA-256 mismatch failure, got %v", err)
	}

	// Wrong ELF architecture refused.
	rfWrongArch := newReleaseFixture(t, "v2026.11.07.2", commit)
	armBin := rfWrongArch.files["pages-linux-arm64"]
	rfWrongArch.files["pages-linux-amd64"] = armBin
	man := Manifest{
		Schema: 1, Tag: "v2026.11.07.2", Commit: commit,
		Binaries: map[string]ManifestBinary{"linux/amd64": {Name: "pages-linux-amd64", SHA256: sha256Hex(armBin)}},
	}
	manBytes, _ := json.Marshal(man)
	rfWrongArch.files["release.json"] = manBytes
	rfWrongArch.files["SHA256SUMS"] = []byte(fmt.Sprintf("%s  pages-linux-amd64\n%s  release.json\n", sha256Hex(armBin), sha256Hex(manBytes)))
	srvWrongArch := rfWrongArch.server(t)
	_, err = FetchRelease(context.Background(), &Downloader{Client: srvWrongArch.Client()}, &fakeVerifier{}, FetchOptions{
		Tag:     rf.tag,
		Arch:    "amd64",
		Dir:     filepath.Join(t.TempDir(), "wrongarch"),
		BaseURL: srvWrongArch.URL + "/download",
		Commit:  commit,
	})
	if err == nil || !strings.Contains(err.Error(), "built for") {
		t.Fatalf("expected ELF machine mismatch failure, got %v", err)
	}

	// Group-writable staging directory refused by LoadVerified.
	badStage := filepath.Join(t.TempDir(), "badstage")
	if err := os.MkdirAll(badStage, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badStage, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerified(context.Background(), &fakeVerifier{}, badStage, rf.tag, "amd64", commit); err == nil || !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("expected private dir error, got %v", err)
	}
}

func TestRunSignedReleaseEndToEnd(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	rf := newReleaseFixture(t, "v2026.11.07.2", commit)
	srv := rf.server(t)

	dir := t.TempDir()
	app := filepath.Join(dir, "app")
	source := filepath.Join(dir, "source")
	if err := os.MkdirAll(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	motd, err := os.ReadFile("../../../deploy/motd")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, "deploy/motd"), string(motd), 0)
	write(t, filepath.Join(app, "release-marker"), "original", 0)
	write(t, filepath.Join(app, "lib/readiness.js"), "", 0)
	write(t, filepath.Join(app, "assets/chart.txt"), "Northwind asset bytes", 0)
	write(t, filepath.Join(dir, "pages.env"), "PORT=4312\n", 0)
	write(t, filepath.Join(dir, "pages.service"), "original service\n", 0)
	write(t, filepath.Join(dir, "pages-cli"), "original CLI\n", 0o755)

	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	var gitCmds []string
	running := app
	fake := &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		args := strings.Join(c.Args, " ")
		switch c.Name {
		case "make", "go":
			t.Fatalf("signed release update must not invoke %s (%s)", c.Name, args)
		case "git":
			gitCmds = append(gitCmds, args)
			switch {
			case strings.Contains(args, "refs/tags/v2026.11.07.2^{commit}"):
				return commit + "\n", 0
			case strings.Contains(args, "rev-parse --short HEAD"):
				return commit[:7] + "\n", 0
			}
			return "", 0
		case "systemctl":
			switch c.Args[0] {
			case "stop":
				running = ""
			case "start":
				running, _ = filepath.EvalSymlinks(app)
			case "show":
				return "4242\n", 0
			}
		}
		return "", 0
	}}

	settings := install.Settings{
		InstallConfig: filepath.Join(dir, "install"),
		AppDir:        app,
		AppUser:       me.Username,
		SrcDir:        source,
		EnvFile:       filepath.Join(dir, "pages.env"),
		CLITarget:     filepath.Join(dir, "pages-cli"),
		Port:          "4312",
	}
	var out bytes.Buffer
	verifier := &fakeVerifier{}
	var opts Options
	opts = Options{
		Settings:         settings,
		Runner:           fake,
		Log:              term.Buffer(&out),
		Yes:              true,
		SkipTemplateSync: true,
		ServiceFile:      filepath.Join(dir, "pages.service"),
		MotdFile:         filepath.Join(dir, "motd"),
		CurrentVersion:   "v2026.11.07.1",
		ReleaseBaseURL:   srv.URL + "/download",
		ReleaseLatestURL: srv.URL + "/latest",
		Arch:             "amd64",
		HTTPClient:       srv.Client(),
		Verifier:         verifier,
		Handoff: func(ctx context.Context, _, candidate string) error {
			return Activate(ctx, opts, candidate)
		},
		Probe:   func(_ context.Context, _ string) bool { return running != "" },
		ProcCwd: func(int) (string, error) { return running, nil },
		Sleep:   func(time.Duration) {},
		Chown:   func(_ context.Context, _ string, _ bool) error { return nil },
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run signed release failed: %v\nlog:\n%s", err, out.String())
	}
	if strings.Join(verifier.verified, ",") != "SHA256SUMS,pages-linux-amd64" {
		t.Fatalf("verified = %v", verifier.verified)
	}
	joinedGit := strings.Join(gitCmds, "\n")
	if !strings.Contains(joinedGit, "fetch --quiet --force --tags origin") {
		t.Fatalf("expected git fetch --tags, got:\n%s", joinedGit)
	}
	if !strings.Contains(joinedGit, "checkout --quiet --detach refs/tags/v2026.11.07.2") {
		t.Fatalf("expected detached checkout of release tag, got:\n%s", joinedGit)
	}
	installedBytes, err := os.ReadFile(filepath.Join(app, "pages"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installedBytes, rf.files["pages-linux-amd64"]) {
		t.Fatalf("installed binary bytes do not match verified release binary")
	}
}

func TestReleaseWorkflowAndShellContract(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	shellBytes, err := os.ReadFile("../../../scripts/lib/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	shell := string(shellBytes)
	lockBytes, err := os.ReadFile("../../../cosign.lock")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockBytes, pages.CosignLock()) {
		t.Fatal("embedded cosign.lock differs from repo root cosign.lock")
	}
	for _, needle := range []string{SignerIdentity, OIDCIssuer} {
		if !strings.Contains(workflow, needle) {
			t.Fatalf("release.yml missing %q", needle)
		}
		if !strings.Contains(shell, needle) {
			t.Fatalf("scripts/lib/release.sh missing %q", needle)
		}
	}
}
