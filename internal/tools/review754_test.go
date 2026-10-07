package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// C4: a body that is not the pinned archive is refused as a digest mismatch even when it is also
// malformed. The digest covers the whole download before the extraction error is reported, so an
// invalid response keeps the pinned refusal and its exit status instead of becoming a host failure.
func TestToolsReviewMalformedArchiveIsADigestMismatch(t *testing.T) {
	tree := newTestTree(t)
	withPin(t, testPin(strings.Repeat("0", 64)))
	release := newFakeRelease(t, []byte("not gzip data"))

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 3 || out != "" || !strings.Contains(errOut, "digest_mismatch") {
		t.Fatalf("a malformed download: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if entries, err := os.ReadDir(tree.toolsRoot); err == nil && len(entries) != 0 {
		t.Errorf("tools_root holds %v after a malformed download", entries)
	}
	if entries, err := os.ReadDir(tree.tempRoot); err == nil && len(entries) != 0 {
		t.Errorf("temp_root holds %v after a malformed download", entries)
	}
}

// review754ConfigTree is a temporary host whose configuration names the given roots with the
// exact spelling given, so a test proves the command keeps that spelling rather than cleaning it.
func review754ConfigTree(t *testing.T, paths map[string]string) testTree {
	t.Helper()
	tree := newTestTree(t)
	config := filepath.Join(tree.home, "config", "crw", "config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	pairs := make([]string, 0, len(paths))
	for _, name := range []string{"tools_root", "temp_root"} {
		if path, ok := paths[name]; ok {
			pairs = append(pairs, fmt.Sprintf("%q:%q", name, path))
		}
	}
	document := `{"schema":"crw-config/1","paths":{` + strings.Join(pairs, ",") + `}}`
	if err := os.WriteFile(config, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, ok := paths["tools_root"]; ok {
		tree.toolsRoot = path
	}
	if path, ok := paths["temp_root"]; ok {
		tree.tempRoot = path
	}
	return tree
}

// review754PlaceInstall writes a complete install of pin into dir, the way a host that already
// ran the install leaves it.
func review754PlaceInstall(t *testing.T, pin Pin, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The placed bytes are an install record the tests read, never a program they run, so this
	// write needs no syscall.ForkLock (CRW-929's sweep covers writers whose file is executed).
	if err := os.WriteFile(filepath.Join(dir, pin.Executable), []byte("gitleaks\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"name":"` + pin.Name + `","version":"` + pin.Version + `","sha256":"` + pin.SHA256 + `"}`
	if err := os.WriteFile(filepath.Join(dir, recordFile), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
}

// C1: a tools_root whose configured spelling mixes a symbolic link and ".." names the install
// the filesystem resolves it to, not the directory filepath.Clean would name. path and list
// both find it.
func TestToolsReviewPathKeepsTheConfiguredSpelling(t *testing.T) {
	tree := newTestTree(t)
	real := filepath.Join(tree.home, "real")
	if err := os.MkdirAll(filepath.Join(real, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "inner"), filepath.Join(tree.home, "link")); err != nil {
		t.Fatal(err)
	}
	spelling := filepath.Join(tree.home, "link") + "/../tools"
	tree = review754ConfigTree(t, map[string]string{"tools_root": spelling})
	pin := testPin(strings.Repeat("0", 64))
	withPin(t, pin)
	review754PlaceInstall(t, pin, filepath.Join(real, "tools", pin.DirName()))
	executable := spelling + "/" + pin.DirName() + "/" + pin.Executable

	code, out, errOut := runTools(t, context.Background(), tree, nil, "path", "gitleaks")
	if code != 0 || errOut != "" || out != executable+"\n" {
		t.Fatalf("path under the configured spelling: exit %d stdout %q stderr %q", code, out, errOut)
	}
	code, out, errOut = runTools(t, context.Background(), tree, nil, "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list under the configured spelling: exit %d stderr %q", code, errOut)
	}
	row := pinsOf(t, out)[0]
	if row["installed"] != true || row["path"] != executable {
		t.Fatalf("list did not find the install under the configured spelling: %v", row)
	}
}

// review754UnsupportedHost is a temporary host holding a linux install of the pin, so a command
// run against a darwin/arm64 seam sees files this host cannot run.
func review754UnsupportedHost(t *testing.T) (testTree, Pin) {
	t.Helper()
	tree := newTestTree(t)
	pin := testPin(strings.Repeat("0", 64))
	withPin(t, pin)
	review754PlaceInstall(t, pin, pin.InstallDir(tree.toolsRoot))
	return tree, pin
}

// C2: path refuses a platform the pin has no archive for, before it looks at the install, with
// the same refusal install gives.
func TestToolsReviewPathRefusesAnUnsupportedPlatform(t *testing.T) {
	tree, _ := review754UnsupportedHost(t)
	code, out, errOut := runTools(t, context.Background(), tree, &Seams{GOOS: "darwin", GOARCH: "arm64"}, "path", "gitleaks")
	if code != 2 || out != "" || !strings.Contains(errOut, "unsupported_platform") {
		t.Fatalf("path on an unsupported platform: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// C2: list carries supported per row and reports a pin this host cannot run as unsupported and
// not installed even though its files are present, and the command still exits 0.
func TestToolsReviewListMarksAnUnsupportedPin(t *testing.T) {
	tree, pin := review754UnsupportedHost(t)
	code, out, errOut := runTools(t, context.Background(), tree, &Seams{GOOS: "darwin", GOARCH: "arm64"}, "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list on an unsupported platform: exit %d stderr %q", code, errOut)
	}
	row := pinsOf(t, out)[0]
	if row["name"] != pin.Name {
		t.Fatalf("list reported %v", row)
	}
	if row["supported"] != false || row["installed"] != false || row["path"] != "" {
		t.Fatalf("list did not mark the unsupported pin: %v", row)
	}
}

// C3: a digest mismatch leaves no directory this call created under temp_root's chain, while a
// directory that was already there is not this call's to remove.
func TestToolsReviewDigestMismatchLeavesNoCreatedParents(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	withPin(t, testPin(strings.Repeat("0", 64)))
	release := newFakeRelease(t, archive)
	absent := filepath.Join(tree.home, "absent")
	toolsRoot := filepath.Join(absent, "tools")
	tree = review754ConfigTree(t, map[string]string{"tools_root": toolsRoot, "temp_root": toolsRoot + "/downloads"})

	code, _, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 3 || !strings.Contains(errOut, "digest_mismatch") {
		t.Fatalf("a digest mismatch: exit %d stderr %q", code, errOut)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("a refused install left %s behind: %v", absent, err)
	}

	if err := os.MkdirAll(toolsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 3 || !strings.Contains(errOut, "digest_mismatch") {
		t.Fatalf("a digest mismatch beside a pre-existing root: exit %d stderr %q", code, errOut)
	}
	if info, err := os.Stat(toolsRoot); err != nil || !info.IsDir() {
		t.Fatalf("the pre-existing tools root is gone: %v", err)
	}
	if _, err := os.Stat(toolsRoot + "/downloads"); !os.IsNotExist(err) {
		t.Fatalf("a refused install left the download directory behind: %v", err)
	}
}

// C3, success side: when temp_root sits under a tools_root that did not exist yet, a successful
// install still removes the download tree it created and leaves a complete install behind. The
// download root is a directory this call made, so it is not left for the next run to trip over.
func TestToolsReviewSuccessfulInstallLeavesNoCreatedParents(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)
	absent := filepath.Join(tree.home, "absent")
	toolsRoot := filepath.Join(absent, "tools")
	tree = review754ConfigTree(t, map[string]string{"tools_root": toolsRoot, "temp_root": toolsRoot + "/downloads"})

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 0 || errOut != "" || out != pin.ExecutablePath(toolsRoot)+"\n" {
		t.Fatalf("install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(toolsRoot + "/downloads"); !os.IsNotExist(err) {
		t.Fatalf("the download root this call created was left behind: %v", err)
	}
	// The tools root holds only the install directory and the pin's lock file: no staging copy and
	// no download tree survived the successful install.
	entries, err := os.ReadDir(toolsRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "."+pin.DirName()+".lock,"+pin.DirName() {
		t.Fatalf("the tools root holds %v after a successful install", names)
	}
	if _, err := installedPath(pin, toolsRoot); err != nil {
		t.Fatalf("the successful install is not readable as an install: %v", err)
	}
}

// C3: a concurrent install that made the shared download root first can remove it again as it
// finishes, right between this call creating the root and this call's own directory. The install
// re-creates what is missing and retries, so the overlap does not turn a valid install into a host
// failure. Only the components this call itself made are recorded, so the other install's root is
// never this call's to remove.
func TestToolsReviewInstallRecoversWhenTheSharedRootIsRemoved(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)
	absent := filepath.Join(tree.home, "absent")
	toolsRoot := filepath.Join(absent, "tools")
	tree = review754ConfigTree(t, map[string]string{"tools_root": toolsRoot, "temp_root": toolsRoot + "/downloads"})

	// The first attempt finds the root gone, the way it is when the concurrent creator finishes
	// and cleans up in the same instant; every later attempt behaves normally.
	calls := 0
	seams := &Seams{URLBase: release.server.URL}
	seams.MkdirTemp = func(dir, pattern string) (string, error) {
		calls++
		if calls == 1 {
			_ = os.Remove(dir)
		}
		return os.MkdirTemp(dir, pattern)
	}
	code, out, errOut := runTools(t, context.Background(), tree, seams, "install", "gitleaks")
	if code != 0 || errOut != "" || out != pin.ExecutablePath(toolsRoot)+"\n" {
		t.Fatalf("install under a removed shared root: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(toolsRoot + "/downloads"); !os.IsNotExist(err) {
		t.Fatalf("the download root was left behind: %v", err)
	}
	if _, err := installedPath(pin, toolsRoot); err != nil {
		t.Fatalf("the install is not readable as an install: %v", err)
	}
}

// createRoot records only the components this call itself made: a directory that already exists is
// never answered as this call's, so the caller cannot remove a directory it did not create.
func TestToolsReviewCreateRootRecordsOnlyItsOwnDirectories(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existing, "a", "b")
	created, err := createRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(existing, "a"), filepath.Join(existing, "a", "b")}
	if strings.Join(created, ",") != strings.Join(want, ",") {
		t.Fatalf("createRoot recorded %v, want only the components it made %v", created, want)
	}
	// A directory that already exists is not recorded, even when the deeper component is missing.
	again, err := createRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("createRoot recorded %v for a root that already exists", again)
	}
}

// C4: the archive is written to a file under temp_root while the download runs, and that file is
// gone once the install has finished.
func TestToolsReviewArchiveIsWrittenUnderTempRoot(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	body := &review754ObservingBody{data: archive, tempRoot: tree.tempRoot, want: pin.Archive}
	client := &http.Client{Transport: &review754ObservingTransport{body: body}}

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{Client: client}, "install", "gitleaks")
	if code != 0 || errOut != "" || out != pin.ExecutablePath(tree.toolsRoot)+"\n" {
		t.Fatalf("install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if body.err != nil {
		t.Fatalf("while the download ran: %v", body.err)
	}
	if body.name != pin.Archive || body.size != int64(len(archive)) {
		t.Fatalf("the download file was %q at %d bytes, want %q at %d bytes", body.name, body.size, pin.Archive, len(archive))
	}
	if entries, err := os.ReadDir(tree.tempRoot); err == nil && len(entries) != 0 {
		t.Errorf("temp_root holds %v after the install", entries)
	}
}

// review754ObservingTransport serves one archive body, so a test watches the download rather
// than only the finished install.
type review754ObservingTransport struct{ body *review754ObservingBody }

func (tr *review754ObservingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: tr.body, Header: http.Header{}}, nil
}

// review754ObservingBody hands the archive back and, on the read that follows its last byte,
// records what the download has left under temp_root: by then the copy has written every byte it
// was handed, so the reading is of the running download and not of a finished one.
type review754ObservingBody struct {
	data     []byte
	tempRoot string
	want     string
	pos      int
	done     bool
	name     string
	size     int64
	err      error
}

func (b *review754ObservingBody) Read(p []byte) (int, error) {
	if b.pos < len(b.data) {
		n := copy(p, b.data[b.pos:])
		b.pos += n
		return n, nil
	}
	if !b.done {
		b.done = true
		b.observe()
	}
	return 0, io.EOF
}

func (b *review754ObservingBody) Close() error { return nil }

func (b *review754ObservingBody) observe() {
	entries, err := os.ReadDir(b.tempRoot)
	if err != nil {
		b.err = fmt.Errorf("the download root could not be read: %v", err)
		return
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		b.err = fmt.Errorf("the download root holds %v", entries)
		return
	}
	files, err := os.ReadDir(filepath.Join(b.tempRoot, entries[0].Name()))
	if err != nil {
		b.err = err
		return
	}
	if len(files) != 1 || files[0].Name() != b.want {
		b.err = fmt.Errorf("the download directory holds %v, want one %q", files, b.want)
		return
	}
	info, err := files[0].Info()
	if err != nil {
		b.err = err
		return
	}
	b.name, b.size = files[0].Name(), info.Size()
}

// C5: two installs that repair the same pin's stale directory at once both succeed and leave one
// complete install. The seam releases both installs from the same point after each has prepared its
// copy and before either takes the pin's lock, so the overlap is deterministic rather than a matter
// of timing, and the seam never waits on work that is itself blocked on the lock.
func TestToolsReviewConcurrentRepairBothSucceed(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	// A stale install under the final name: a record that does not name the pin, so neither
	// install may answer from it and both must repair the directory.
	review754PlaceInstall(t, testPin(strings.Repeat("0", 64)), pin.InstallDir(tree.toolsRoot))

	// Every install holds here once it has prepared its copy, so they all reach the repair step
	// together: the ordering is decided by the seam, not by how the scheduler happens to run them.
	const installs = 8
	var reached sync.WaitGroup
	reached.Add(installs)
	seam := func() {
		reached.Done()
		reached.Wait()
	}

	codes := make([]int, installs)
	outputs := make([]string, installs)
	errs := make([]string, installs)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i], errs[i] = runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL, BeforeRepair: seam}, "install", "gitleaks")
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 || errs[i] != "" {
			t.Fatalf("overlapping install %d: exit %d stdout %q stderr %q", i, codes[i], outputs[i], errs[i])
		}
	}
	// The install that lost the race still answers with the executable's path, and the directory
	// it left is a complete install of the pin.
	executable := pin.ExecutablePath(tree.toolsRoot)
	raw, err := os.ReadFile(pin.RecordPath(tree.toolsRoot))
	if err != nil {
		t.Fatalf("the final install has no record: %v", err)
	}
	found := decodeObject(t, string(raw))
	if found["name"] != pin.Name || found["version"] != pin.Version || found["sha256"] != pin.SHA256 {
		t.Fatalf("the final record does not name the pin: %v", found)
	}
	info, err := os.Stat(executable)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the final executable is not installed: %v", err)
	}
	// Only the install directory and the lock file remain: no staging directory leaked, and the
	// losing install removed its own copy.
	entries, err := os.ReadDir(tree.toolsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != pin.DirName() && name != "."+pin.DirName()+".lock" {
			t.Errorf("the tools root holds %q, want only the install directory and its lock file", name)
		}
	}
}

// A context cancelled while an install waits for the pin's lock leaves the install directory as it
// was: the interrupt is honoured after the lock is taken, before any durable effect.
func TestToolsReviewCancelledInstallDoesNotRepair(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	// A stale install the repair would replace, so the test can tell a refused write from a no-op.
	installDir := pin.InstallDir(tree.toolsRoot)
	review754PlaceInstall(t, testPin(strings.Repeat("0", 64)), installDir)

	ctx, cancel := context.WithCancel(context.Background())
	seam := func() { cancel() }
	code, out, errOut := runTools(t, ctx, tree, &Seams{URLBase: release.server.URL, BeforeRepair: seam}, "install", "gitleaks")
	if code != 1 || out != "" {
		t.Fatalf("a cancelled install: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "cancelled") {
		t.Errorf("the refusal does not name the cancellation: %q", errOut)
	}
	raw, err := os.ReadFile(pin.RecordPath(tree.toolsRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), strings.Repeat("0", 64)) {
		t.Errorf("the cancelled install changed the install directory: %s", raw)
	}
	entries, err := os.ReadDir(tree.toolsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		// The pin's lock file may exist: it is the one file an install leaves, and it carries no
		// claim about the install state.
		if name := entry.Name(); name != pin.DirName() && name != "."+pin.DirName()+".lock" {
			t.Errorf("the tools root holds %q after a cancelled install", name)
		}
	}
}
