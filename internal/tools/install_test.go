package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syntheticArchive builds a release-shaped tar.gz: the executable plus the two files the real
// gitleaks archive carries, so a test proves that only the executable is installed.
func syntheticArchive(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"LICENSE", 0o644, []byte("MIT\n")},
		{"README.md", 0o644, []byte("readme\n")},
		{"gitleaks", 0o755, body},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testPin is the shipped gitleaks row with its digest replaced, so a synthetic archive can stand
// in for the release while the install path stays the production one.
func testPin(digest string) Pin {
	pin, ok := Lookup("gitleaks")
	if !ok {
		panic("the pin table has no gitleaks row")
	}
	pin.SHA256 = digest
	return pin
}

// withPin installs a pin table for one test and restores the shipped one after it.
func withPin(t *testing.T, pins ...Pin) {
	t.Helper()
	saved := Pins
	Pins = pins
	t.Cleanup(func() { Pins = saved })
}

// testTree is a temporary host: HOME and every XDG base directory point inside one temporary
// directory, so no test reads or writes the real ones.
type testTree struct {
	home      string
	toolsRoot string
	tempRoot  string
	getenv    func(string) string
}

func newTestTree(t *testing.T) testTree {
	t.Helper()
	home := t.TempDir()
	values := map[string]string{
		"HOME":            home,
		"XDG_DATA_HOME":   filepath.Join(home, "data"),
		"XDG_STATE_HOME":  filepath.Join(home, "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"TMPDIR":          filepath.Join(home, "tmp"),
	}
	return testTree{
		home:      home,
		toolsRoot: filepath.Join(home, "data", "crw", "tools"),
		tempRoot:  filepath.Join(home, "tmp", "crw"),
		getenv:    func(name string) string { return values[name] },
	}
}

// fakeRelease is the httptest server that stands in for the GitHub release download.
type fakeRelease struct {
	server *httptest.Server
	body   []byte
	status int

	mu       sync.Mutex
	requests int
	paths    []string
}

func newFakeRelease(t *testing.T, body []byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{body: body, status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		_, _ = w.Write(f.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRelease) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeRelease) requestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// runTools runs the command against a temporary host and returns its status and both streams.
func runTools(t *testing.T, ctx context.Context, tree testTree, seams *Seams, args ...string) (int, string, string) {
	t.Helper()
	if seams == nil {
		seams = &Seams{}
	}
	if seams.Getenv == nil {
		seams.Getenv = tree.getenv
	}
	var out, errOut strings.Builder
	code := Run(ctx, args, &out, &errOut, seams)
	return code, out.String(), errOut.String()
}

// decodeObject decodes one JSON object, so a test never depends on Go struct tags to read the
// product's own key names.
func decodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, raw)
	}
	return value
}

// C1: a synthetic release is fetched, its digest verified, the executable installed with a
// pin.json beside it, and a second install fetches nothing at all.
func TestInstallFetchesVerifiesAndInstallsOnce(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("#!/bin/sh\necho gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)
	fetched := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	seams := &Seams{URLBase: release.server.URL, Now: func() time.Time { return fetched }}

	code, out, errOut := runTools(t, context.Background(), tree, seams, "install", "gitleaks")
	installDir := filepath.Join(tree.toolsRoot, "gitleaks-8.30.1")
	executable := filepath.Join(installDir, "gitleaks")
	if code != 0 || errOut != "" {
		t.Fatalf("install: exit %d stderr %q", code, errOut)
	}
	if out != executable+"\n" {
		t.Fatalf("install printed %q, want the executable path %q", out, executable)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatalf("the executable is not installed: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the installed executable is not executable: %v", info.Mode())
	}
	raw, err := os.ReadFile(filepath.Join(installDir, "pin.json"))
	if err != nil {
		t.Fatalf("pin.json: %v", err)
	}
	record := decodeObject(t, string(raw))
	if record["name"] != "gitleaks" || record["version"] != "8.30.1" || record["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("pin.json = %v", record)
	}
	if record["fetched_at"] != fetched.Format(time.RFC3339) {
		t.Errorf("pin.json fetched_at = %v, want %q", record["fetched_at"], fetched.Format(time.RFC3339))
	}
	// Only the executable and its record land in the install directory: the archive's other
	// members (the licences) are not unpacked.
	entries, err := os.ReadDir(installDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "gitleaks,pin.json" {
		t.Errorf("the install directory holds %v", names)
	}
	if release.count() != 1 {
		t.Fatalf("the first install made %d requests", release.count())
	}
	if paths := release.requestedPaths(); len(paths) != 1 || paths[0] != "/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz" {
		t.Errorf("the install requested %v", paths)
	}

	// The second install answers from the installed record and fetches nothing.
	code, out, errOut = runTools(t, context.Background(), tree, seams, "install", "gitleaks")
	if code != 0 || out != executable+"\n" || errOut != "" {
		t.Fatalf("the second install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if release.count() != 1 {
		t.Errorf("the second install fetched again: %d requests", release.count())
	}
}

// An install whose record does not match the pin is not trusted: the executable is fetched again.
func TestInstallRepairsARecordThatDoesNotMatchThePin(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)
	installDir := filepath.Join(tree.toolsRoot, "gitleaks-8.30.1")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "gitleaks"), []byte("stale\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "{\"name\":\"gitleaks\",\"version\":\"8.30.1\",\"sha256\":\"" + strings.Repeat("0", 64) + "\"}"
	if err := os.WriteFile(filepath.Join(installDir, "pin.json"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 0 || errOut != "" || out != filepath.Join(installDir, "gitleaks")+"\n" {
		t.Fatalf("install over a stale record: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if release.count() != 1 {
		t.Errorf("the repair made %d requests", release.count())
	}
	body, err := os.ReadFile(filepath.Join(installDir, "gitleaks"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "gitleaks\n" {
		t.Errorf("the executable was not replaced: %q", body)
	}
}

// C2: a digest that does not match the pin exits 3 and leaves nothing under tools_root, and the
// download directory under temp_root is gone too.
func TestInstallRefusesADigestMismatchAndLeavesNothing(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	withPin(t, testPin(strings.Repeat("0", 64)))
	release := newFakeRelease(t, archive)

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 3 {
		t.Fatalf("a digest mismatch: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "digest_mismatch") {
		t.Errorf("the refusal does not name digest_mismatch: %q", errOut)
	}
	if out != "" {
		t.Errorf("a refused install printed %q", out)
	}
	if entries, err := os.ReadDir(tree.toolsRoot); err == nil && len(entries) != 0 {
		t.Errorf("tools_root holds %v after a digest mismatch", entries)
	}
	if entries, err := os.ReadDir(tree.tempRoot); err == nil && len(entries) != 0 {
		t.Errorf("temp_root holds %v after a digest mismatch", entries)
	}
	if release.count() != 1 {
		t.Errorf("the refused install made %d requests", release.count())
	}
}

// A platform the pin does not name is refused before anything is fetched.
func TestInstallRefusesAnUnpinnedPlatform(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL, GOOS: "darwin", GOARCH: "arm64"}, "install", "gitleaks")
	if code != 2 {
		t.Fatalf("an unpinned platform: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "unsupported_platform") {
		t.Errorf("the refusal does not name unsupported_platform: %q", errOut)
	}
	if release.count() != 0 {
		t.Errorf("the platform check fetched %d times", release.count())
	}
}

// A release the server refuses is a host failure, not one of the pinned identities, and it leaves
// nothing behind.
func TestInstallReportsAFailedFetch(t *testing.T) {
	tree := newTestTree(t)
	withPin(t, testPin(strings.Repeat("0", 64)))
	release := newFakeRelease(t, nil)
	release.status = http.StatusNotFound

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 1 {
		t.Fatalf("a failed fetch: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "404") {
		t.Errorf("the refusal does not carry the host's answer: %q", errOut)
	}
	if entries, err := os.ReadDir(tree.toolsRoot); err == nil && len(entries) != 0 {
		t.Errorf("tools_root holds %v after a failed fetch", entries)
	}
}

// A cancelled context stops the install before it writes anything.
func TestInstallStopsOnACancelledContext(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	withPin(t, testPin(hex.EncodeToString(sum[:])))
	release := newFakeRelease(t, archive)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _, errOut := runTools(t, ctx, tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 1 {
		t.Fatalf("a cancelled install: exit %d stderr %q", code, errOut)
	}
	if entries, err := os.ReadDir(tree.toolsRoot); err == nil && len(entries) != 0 {
		t.Errorf("tools_root holds %v after a cancelled install", entries)
	}
}
