package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

var buildDir string

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir, err = os.MkdirTemp("", "crw-install-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(buildDir)
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// binary is the crw this package's tests install: the real multi-call binary, built once.
var binary = sync.OnceValues(func() ([]byte, error) {
	if built := os.Getenv("CRW_TEST_CRW"); built != "" { // a test rerun in a child process
		return os.ReadFile(built)
	}
	path := filepath.Join(buildDir, "crw")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, "./cmd/crw")
	cmd.Dir = golden.Root()
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build: %w\n%s", err, out)
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
// digest), and a SHA256SUMS beside it.
func archive(t *testing.T, version, extra string) string {
	t.Helper()
	raw, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "crw_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
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
	for _, link := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
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
	path := filepath.Join(dir, name)
	write(t, path, buf.String())
	sum := sha256.Sum256(buf.Bytes())
	write(t, filepath.Join(dir, install.SumsName), hex.EncodeToString(sum[:])+"  "+name+"\n")
	return path
}

func archiveDigest(t *testing.T, path string) string {
	t.Helper()
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
