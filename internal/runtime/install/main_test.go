package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	testsupport.Main(m, setUpArchiveRoot)
}

// binary is the crw this package's tests install: the real multi-call binary
// (testsupport.CRW), read once.
var binary = sync.OnceValues(func() ([]byte, error) {
	path, err := testsupport.CRWPath()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// host is a temporary HOME with a Codex home, a state home, a relay state directory and a
// fake App Server, laid out as the relay host lays them out.
type host struct {
	home, dest, codex, state, record, relayState string
	fake                                         *fakehost.Server
	env                                          scope.Env
	// proc is an empty process table, so what remove and the reclaim read of the processes does
	// not depend on what else runs on the host; a test that starts a process uses realProcesses.
	proc string
}

func newHost(t *testing.T) *host {
	t.Helper()
	home := t.TempDir()
	h := &host{home: home, dest: filepath.Join(home, ".local", "share", "crw-runtime"), codex: filepath.Join(home, ".codex"),
		state: filepath.Join(home, ".local", "state"), relayState: filepath.Join(home, "relay-state")}
	h.record = filepath.Join(h.state, "codex-relay-workflow", record.Name)
	for _, d := range []string{h.codex, h.relayState} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.fake = fakehost.Start(t)
	h.proc = filepath.Join(t.TempDir(), "proc")
	if err := os.MkdirAll(filepath.Join(h.proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := scope.Env{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_STATE_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CRW_COMPLETION_HOOK_CONFIG",
			"CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST":
			continue
		}
		env = append(env, entry)
	}
	h.env = append(env, "HOME="+home, "XDG_STATE_HOME="+h.state, "CODEX_HOME="+h.codex)
	return h
}

func codexCli(context.Context) *string {
	v := "codex-cli 0.154.0"
	return &v
}

// realProcesses is options reading this host's own process table, for a test that starts a
// process and asks what is found of it.
func (h *host) realProcesses() install.Options {
	o := h.options()
	o.Proc = "/proc"
	return o
}

func (h *host) options() install.Options {
	return install.Options{Env: h.env, Dest: h.dest, CodexHome: h.codex, RecordPath: h.record, Socket: h.fake.SocketPath, State: h.relayState, ScopeRegistry: filepath.Join(h.home, "scopes"), Proc: h.proc,
		Issue: "CRW-158", CodexVersion: codexCli, Now: func() time.Time { return time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) }}
}

// archive writes crw_<version>_<os>_<arch>.tar.gz carrying the real binary, its three
// compatibility links and a licence (extra varies the bytes, so each archive has its own
// digest), and a SHA256SUMS beside it. The archive is a link to the one copy of its bytes
// this process wrote (sharedArchive), so a test that installs an archive no longer writes the
// whole binary again; the copy is read-only, and no test writes into an archive it was given.
func archive(t *testing.T, version, extra string) string {
	t.Helper()
	dir := t.TempDir()
	name := "crw_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	shared := sharedArchive(t, version, extra, name)
	path := filepath.Join(dir, name)
	if err := os.Link(shared.path, path); err != nil {
		// a different file system than the shared copy: a copy is the same archive
		if err := copyFile(shared.path, path); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, install.SumsName), shared.digest+"  "+name+"\n")
	return path
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// archiveRoot is a directory inside the isolation root that testsupport.Main removes after the
// tests; the shared archives are written under it.
var archiveRoot string

func setUpArchiveRoot(root string) (func() error, error) {
	archiveRoot = filepath.Join(root, "archives")
	return nil, nil
}

// sharedArchives holds each archive by version and extra: where its bytes are written once for
// the whole run, and their digest. The tests install a few archives over and over; compressing
// (and, later, writing and hashing) the whole binary again for each call was most of this
// package's time, and the bytes, so every digest, are the same either way.
var sharedArchives = struct {
	sync.Mutex
	made    map[[2]string]sharedFile
	digests map[fileIdentity]string
}{made: map[[2]string]sharedFile{}, digests: map[fileIdentity]string{}}

type sharedFile struct{ path, digest string }

// fileIdentity is a file as the file system names it: the device, inode, size and modification
// time. A path whose identity is a shared archive's holds that archive's bytes, so its digest is
// known without reading it again.
type fileIdentity struct {
	dev, ino uint64
	size     int64
	mtime    int64
}

func identityOf(info os.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{uint64(stat.Dev), uint64(stat.Ino), info.Size(), info.ModTime().UnixNano()}, true
}

func sharedArchive(t *testing.T, version, extra, name string) sharedFile {
	t.Helper()
	sharedArchives.Lock()
	defer sharedArchives.Unlock()
	key := [2]string{version, extra}
	if made, found := sharedArchives.made[key]; found {
		return made
	}
	compressed := archiveBytes(t, version, extra)
	dir := filepath.Join(archiveRoot, fmt.Sprintf("%d", len(sharedArchives.made)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, compressed, 0o444); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(compressed)
	made := sharedFile{path: path, digest: hex.EncodeToString(sum[:])}
	if info, err := os.Stat(path); err == nil {
		if id, ok := identityOf(info); ok {
			sharedArchives.digests[id] = made.digest
		}
	}
	sharedArchives.made[key] = made
	return made
}

// archiveBytes is an archive's bytes, built once per run and freed after they are written.
func archiveBytes(t *testing.T, version, extra string) []byte {
	t.Helper()
	raw, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) + 1<<16)
	compressed, err := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(compressed)
	add := func(header *tar.Header, body []byte) {
		header.ModTime = time.Unix(0, 0)
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "crw", Mode: 0o755, Size: int64(len(raw)), Typeflag: tar.TypeReg}, raw)
	for _, link := range []string{"codex-session-relay", "codex-thread-bridge"} {
		add(&tar.Header{Name: link, Linkname: "crw", Mode: 0o777, Typeflag: tar.TypeSymlink}, nil)
	}
	licence := []byte("MIT " + version + extra + "\n")
	add(&tar.Header{Name: "LICENSE", Mode: 0o644, Size: int64(len(licence)), Typeflag: tar.TypeReg}, licence)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// archiveDigest is the SHA-256 of the file at path: read from the file system, except for a
// shared archive (or a link to one), whose digest was taken when it was written.
func archiveDigest(t *testing.T, path string) string {
	t.Helper()
	if info, err := os.Stat(path); err == nil {
		if id, ok := identityOf(info); ok {
			sharedArchives.Lock()
			digest, known := sharedArchives.digests[id]
			sharedArchives.Unlock()
			if known {
				return digest
			}
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func runtimeDir(h *host, version, archivePath string, t *testing.T) string {
	return filepath.Join(h.dest, "bin-"+version+"-"+archiveDigest(t, archivePath)[:12])
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeExecutable writes a file this test process then runs, through the same helper the package
// uses for its own: a fork that landed inside the write would inherit the descriptor and leave
// the path unexecutable (ETXTBSY, golang/go#22315).
func writeExecutable(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := install.WriteExecutable(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func at(o record.Object, path ...string) any {
	var v any = o
	for _, key := range path {
		v = record.Get(golden.Obj(v), key)
	}
	return v
}

// mustInstall installs an archive and fails the test unless it was promoted and settled.
func (h *host) mustInstall(t *testing.T, command, archivePath string) record.Object {
	t.Helper()
	result, code := install.Install(context.Background(), h.options(), command, install.Source{From: archivePath})
	if code != install.OK || at(result, "promoted") != true || at(result, "claimSettled") != true {
		t.Fatalf("%s: exit %d\n%s", command, code, golden.Canon(result))
	}
	return result
}

func (h *host) hostRecord(t *testing.T) record.Object {
	t.Helper()
	read := record.Load(h.record, 1)
	if !read.OK() {
		t.Fatalf("host record: %+v", read)
	}
	return read.Value.(record.Object)
}

func (h *host) pointerTarget(t *testing.T) string {
	t.Helper()
	read := pointer.Read(pointer.Path(h.dest))
	if read.State != pointer.Link {
		t.Fatalf("pointer: %+v", read)
	}
	return read.Target
}

func claimState(t *testing.T, dir string) any {
	t.Helper()
	read := reading.ReadJSON(filepath.Join(dir, ".crw-staging-claim.json"), "claim", nil, nil)
	if !read.OK() {
		return read.State
	}
	return record.Get(read.Value.(record.Object), "state")
}
