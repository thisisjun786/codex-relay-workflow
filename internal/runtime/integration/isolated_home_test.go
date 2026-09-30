//go:build integration

// Package integration_test installs the Go runtime and runs the plugin's declared wiring in an
// isolated home, end to end (todo 40: IS-1, IS-7, IS-8). Release archives of the real crw
// binary are built in the release layout (docs/releases.md) and installed with `crw install`
// into a temporary HOME whose Codex home is not $HOME/.codex. The working-tree plugin package is
// copied into that Codex home's plugin cache, and its declared commands run the way the host
// runs them: the Stop hook through `/bin/sh -c` with CODEX_HOME and PLUGIN_ROOT set, the MCP
// server as `sh ./wiring/crw-bridge.sh` from the installed version directory without CODEX_HOME,
// the first time with only HOME set.
//
// The Stop hook, `crw install` and every MCP start after the first run with a PATH of tripwires
// (python, python3, pip, uv) and a Codex stub, so a PATH lookup of Python or its package tools
// fails the test. That covers PATH lookups only: an absolute /usr/bin/python3 would go unseen,
// and so would anything the first MCP start runs, since with no PATH at all /bin/sh searches its
// built-in default and finds the real one.
//
// Every home and relay root the products resolve lies under the temporary root: HOME,
// CODEX_HOME, XDG_STATE_HOME and TMPDIR, and the relay roots HOME does not move (isolation).
// What they still read of this machine is its process table and the programs they classify:
// `crw install remove` reads /proc to rule out a process running out of the runtime it deletes,
// and stats the paths other processes' command lines and working directories name to tell an
// alias of that runtime; the executables PATH and the installer's settings name (sh,
// /usr/bin/env) are classified where they lie. Apart from this checkout, which the plugin package
// and the release files are copied from, and the Go toolchain relinking B (its caches, its
// telemetry, and git reading the repository to stamp the build), nothing the test starts opens a
// file under this user's home. IS-8 requires every path the remove answers name, the relay
// records the verdict read (relayRecords) among them, to lie under the root. The test cannot
// control this host's process table: when the remove of the unselected runtime refuses only for
// processes another uid runs and the test did not start (heldByHost: an Azure runner's root
// WALinuxAgent runs a relative script from a working directory no other user may read), it logs
// them, requires that nothing was removed or written and that the answer gives the removal by
// hand, and skips only the success assertions; any other refusal fails.
//
// Every Stop hook path exits 0, so no hook assertion rests on the exit status: each one finds
// the journal row the hook wrote for that session and turn, or proves that none was written.
//
// Run it with `go test -tags integration ./internal/runtime/integration/...`. CRW_TEST_BINARY
// names the binary to install (CI passes the dist leg's dist/crw_linux_amd64/crw); without it
// the test builds ./cmd/crw. The second binary is always relinked from source with its own
// version, so an update can be told apart from the install it replaces.
package integration_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// moduleRoot is the checkout under test. `go test` runs a test binary in its package's source
// directory, which holds under -trimpath too, where runtime.Caller names no file on disk.
var moduleRoot string

// toolchainEnv is this process's environment before the relay state was isolated: the Go
// toolchain that relinks the second binary keeps its own caches.
var toolchainEnv []string

func TestMain(m *testing.M) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	moduleRoot = filepath.Join(cwd, "..", "..", "..")
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		fmt.Fprintln(os.Stderr, "the module root is not three directories above the package:", err)
		os.Exit(1)
	}
	toolchainEnv = os.Environ()
	// This process's own homes and relay state roots point at a temporary tree too, so nothing
	// the test runs in-process can reach the machine's real ones.
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

const (
	// The archive versions (crw_<version>_<os>_<arch>.tar.gz). A keeps whatever version its
	// binary was built with; B is relinked with main.version set to versionB.
	versionA = "it-a"
	versionB = "it-b"
	// hookCeiling is a generous bound on one Stop hook run, never a latency claim.
	hookCeiling = 5 * time.Second
	// commandTimeout bounds every process this test starts; each one is waited on.
	commandTimeout = 60 * time.Second
	// socketLimit is the longest unix socket path the kernel takes.
	socketLimit = 107
)

// toolNames are the 12 tools the bridge's MCP contract lists (tools/list), sorted.
var toolNames = []string{
	"create_thread", "create_worktree_thread", "get_active_turn", "get_capabilities", "get_goal", "get_operation",
	"list_threads", "pause_goal", "read_thread", "send_message_to_thread", "steer_thread", "wait_thread",
}

// isolated is one temporary host: every path below lives under root, which is t.TempDir().
type isolated struct {
	root, home, codex, state, tmp, trip, tools, sentinel string
	// scopes and markers are the relay scope registry and marker root; see isolation.
	scopes, markers string
	// socket is the relay socket the installer's settings default to. No relay runs here, so
	// every Stop the hook records finds nothing listening there.
	socket string
	// dest is the installer's default destination and current its owned pointer.
	dest, current, record, relayState, ledger string
	// journal, settings and bridgeRecord are what `crw install hook` and `register-mcp` write
	// and the declared commands read, under the Codex home.
	journal, settings, bridgeRecord string
	// payload is the installed plugin version directory: PLUGIN_ROOT and the MCP server's cwd.
	payload, manifestVersion string
	downloaded, versionA     string
	envA, envB               string
	fake                     *fakehost.Server
}

func TestIsolatedHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc for the running bridge; CI runs this on linux/amd64")
	}
	h := newIsolated(t)
	archiveA, archiveB := h.release(t)
	if !t.Run("IS-1 install and wiring", func(t *testing.T) { h.installAndWire(t, archiveA) }) {
		return
	}
	if !t.Run("IS-7 cache replacement and a missing runtime", h.cacheReplacement) {
		return
	}
	t.Run("IS-8 update, refusal, rollback and remove", func(t *testing.T) { h.updateAndRollBack(t, archiveA, archiveB) })
}

func newIsolated(t *testing.T) *isolated {
	t.Helper()
	root := t.TempDir()
	h := &isolated{root: root, home: filepath.Join(root, "home"), codex: filepath.Join(root, "codex"), state: filepath.Join(root, "state"),
		tmp: filepath.Join(root, "tmp"), trip: filepath.Join(root, "trip"), tools: filepath.Join(root, "tools"),
		sentinel: filepath.Join(root, "tripwire-fired"), relayState: filepath.Join(root, "relay-state"), ledger: filepath.Join(root, "bridge-ledger"),
		scopes: filepath.Join(root, "scopes"), markers: filepath.Join(root, "markers")}
	h.socket = filepath.Join(h.state, "codex-session-relay", "default", "control.sock")
	h.dest = filepath.Join(h.home, ".local", "share", "crw-runtime")
	h.current = filepath.Join(h.dest, "current")
	h.record = filepath.Join(h.state, "codex-relay-workflow", "host-record.json")
	h.journal = filepath.Join(h.codex, "crw-completion-hook", "journal")
	h.settings = filepath.Join(h.codex, "crw-completion-hook.json")
	h.bridgeRecord = filepath.Join(h.codex, "crw-bridge-mcp.json")
	for _, path := range []string{h.home, h.codex, h.state, h.tmp, h.trip, h.tools, h.sentinel, h.relayState, h.ledger, h.dest, h.current, h.record, h.journal, h.settings, h.bridgeRecord, h.scopes, h.markers, h.socket} {
		h.mustBeInside(t, path)
	}
	// The MCP launcher is never given CODEX_HOME, so it can find this Codex home only from the
	// plugin cache it runs in; a Codex home at $HOME/.codex would also be its default.
	if h.codex == filepath.Join(h.home, ".codex") {
		t.Fatalf("CODEX_HOME %s is $HOME/.codex", h.codex)
	}
	for _, dir := range []string{h.home, h.codex, h.state, h.tmp, h.trip, h.tools, h.relayState} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(h.codex, "config.toml"), "# the isolated Codex home of the todo 40 integration test\n", 0o644)
	// The tripwires: a command that asks PATH for Python or its package tools leaves a line in
	// the sentinel and fails. The shebang names /bin/sh by absolute path.
	for _, name := range []string{"python", "python3", "pip", "uv"} {
		writeFile(t, filepath.Join(h.trip, name), "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> "+shellQuote(h.sentinel)+"\nexit 97\n", 0o755)
	}
	// The package declares its MCP server as `sh ./wiring/crw-bridge.sh`, which the host finds
	// on its PATH, and `crw install remove` resolves that command the same way before it rules
	// out that a registration runs from the runtime it deletes: a host PATH without sh leaves the
	// declaration unjudged and the remove refused. PATH holds the system sh, as a host's does.
	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatalf("this host has no /bin/sh: %v", err)
	}
	if err := os.Symlink(sh, filepath.Join(h.tools, "sh")); err != nil {
		t.Fatal(err)
	}
	// `crw install` records the Codex CLI version as one dimension of the measured point.
	writeFile(t, filepath.Join(h.tools, "codex"), "#!/bin/sh\n[ \"$1\" = --version ] || exit 1\necho 'codex-cli 0.0.0-isolated'\n", 0o755)
	// The fake App Server's socket, and every temporary file this process makes, land under root.
	t.Setenv("TMPDIR", h.tmp)
	h.fake = fakehost.Start(t)
	h.mustBeInside(t, h.fake.SocketPath)
	if len(h.fake.SocketPath) > socketLimit {
		t.Fatalf("the fake App Server socket %s is longer than a unix socket path may be", h.fake.SocketPath)
	}
	h.manifestVersion = manifestVersion(t)
	h.payload = h.replaceCache(t, h.manifestVersion, nil)
	return h
}

// mustBeInside aborts the test unless path lies under the temporary root.
func (h *isolated) mustBeInside(t *testing.T, path string) {
	t.Helper()
	if !h.inside(path) {
		t.Fatalf("%s is not under the test's temporary root %s", path, h.root)
	}
}

// inside is whether path lies under the temporary root.
func (h *isolated) inside(path string) bool {
	relative, err := filepath.Rel(h.root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, "../") && !filepath.IsAbs(relative)
}

// outside is every absolute path an answer names, at any depth, that is not under the temporary
// root.
func (h *isolated) outside(answer any) []string {
	var paths []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, value := range v {
				walk(value)
			}
		case []any:
			for _, value := range v {
				walk(value)
			}
		case string:
			if filepath.IsAbs(v) && !h.inside(v) {
				paths = append(paths, v)
			}
		}
	}
	walk(answer)
	slices.Sort(paths)
	return paths
}

// env is the isolated environment: the four homes, a PATH of tripwires and the Codex stub, and
// isolation. Nothing is inherited from this process. The store's live-state refusal, which
// isolation names, has nothing here to refuse: the relay state `crw install` exercises is named
// with --state outside $XDG_STATE_HOME/codex-session-relay, and a Stop hook whose relay is
// unreachable opens no store.
func (h *isolated) env(extra ...string) []string {
	env := append([]string{"HOME=" + h.home, "CODEX_HOME=" + h.codex, "XDG_STATE_HOME=" + h.state, "TMPDIR=" + h.tmp,
		"PATH=" + h.trip + ":" + h.tools}, h.isolation()...)
	return append(env, extra...)
}

// isolation names the relay roots HOME does not move. The scope registry is resolved from the
// passwd entry's home (service.ResolveScope, as Python's pwd.getpwuid), never from $HOME, so
// without CODEX_SESSION_RELAY_SCOPE_DIR the relay `crw install`'s swap gate asks
// (`codex-session-relay service status`) would read this machine's live registry, and so would
// `crw install remove`, which reads the daemon records of the registry the relay resolves
// (doctor.RecordedDaemons) and the state directories its claims name. The marker root follows
// $HOME already and is named here too, as testsupport.IsolateRelayState names both, and so is the
// store's live-state refusal it keeps (testsupport.RefuseLiveStateEnv), which a process started
// with nothing inherited would otherwise lack.
func (h *isolated) isolation() []string {
	return []string{"CODEX_SESSION_RELAY_SCOPE_DIR=" + h.scopes, "CODEX_SESSION_RELAY_MARKER_ROOT=" + h.markers, testsupport.RefuseLiveStateEnv + "=1"}
}

// bareBridgeEnv is the least a host can start the MCP server with: HOME and nothing else, no
// PATH either.
func (h *isolated) bareBridgeEnv() []string { return []string{"HOME=" + h.home} }

// bridgeEnv adds the tripwire PATH and isolation to HOME, and still no CODEX_HOME: a PATH lookup
// of Python from the launcher or the Go bridge fires a tripwire.
func (h *isolated) bridgeEnv() []string {
	return append([]string{"HOME=" + h.home, "PATH=" + h.trip + ":" + h.tools}, h.isolation()...)
}

// hookEnv is what the host hands the declared Stop hook of the installed package.
func (h *isolated) hookEnv() []string { return h.env("PLUGIN_ROOT=" + h.payload) }

// bin is a name under the owned pointer's bin/.
func (h *isolated) bin(name string) string { return filepath.Join(h.current, "bin", name) }

func (h *isolated) noTripwireFired(t *testing.T) {
	t.Helper()
	if raw, err := os.ReadFile(h.sentinel); err == nil {
		t.Fatalf("a command asked PATH for Python or its tools:\n%s", raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// ---- binaries, archives and the plugin payload

// release builds A and B and writes their archives with one SHA256SUMS beside them.
func (h *isolated) release(t *testing.T) (string, string) {
	t.Helper()
	build := filepath.Join(h.root, "build")
	// A is what an operator downloaded and runs once to install.
	h.downloaded = filepath.Join(h.root, "downloaded", "crw")
	if named := os.Getenv("CRW_TEST_BINARY"); named != "" {
		abs, err := filepath.Abs(named)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("CRW_TEST_BINARY: %v", err)
		}
		writeFile(t, h.downloaded, string(raw), 0o755)
	} else {
		goBuild(t, h.downloaded, versionA)
	}
	b := filepath.Join(build, "b", "crw")
	goBuild(t, b, versionB)
	h.versionA = strings.TrimSpace(h.mustRun(t, h.downloaded, "version").stdout)
	if got := strings.TrimSpace(h.mustRun(t, b, "version").stdout); got != versionB || h.versionA == versionB || h.versionA == "" {
		t.Fatalf("A reports %q and B %q; the update could not be told from the install", h.versionA, got)
	}
	dir := filepath.Join(h.root, "release")
	archiveA := writeArchive(t, dir, versionA, h.downloaded)
	archiveB := writeArchive(t, dir, versionB, b)
	var sums strings.Builder
	for _, path := range []string{archiveA, archiveB} {
		sums.WriteString(fileDigest(t, path) + "  " + filepath.Base(path) + "\n")
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), sums.String(), 0o644)
	h.envA = filepath.Join(h.dest, "bin-"+versionA+"-"+fileDigest(t, archiveA)[:12])
	h.envB = filepath.Join(h.dest, "bin-"+versionB+"-"+fileDigest(t, archiveB)[:12])
	return archiveA, archiveB
}

// goBuild links ./cmd/crw as the release does (static, trimmed) with main.version set.
func goBuild(t *testing.T, output, version string) {
	t.Helper()
	// `go test` puts its own GOROOT/bin first on the PATH it runs tests with.
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command to link crw %s with: %v", version, err)
	}
	cmd := exec.Command(goTool, "build", "-trimpath", "-ldflags=-X main.version="+version, "-o", output, "./cmd/crw")
	cmd.Dir = moduleRoot
	cmd.Env = append(append([]string{}, toolchainEnv...), "CGO_ENABLED=0", "TMPDIR="+os.Getenv("TMPDIR"))
	started := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", version, err, out)
	}
	t.Logf("built crw %s in %s", version, time.Since(started).Round(time.Millisecond))
}

// writeArchive writes crw_<version>_<os>_<arch>.tar.gz in the layout docs/releases.md states:
// the binary, its three compatibility names as links to it, the repository LICENSE and the
// bridge's MIT provenance.
func writeArchive(t *testing.T, dir, version, binary string) string {
	t.Helper()
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
	w := tar.NewWriter(compressed)
	add := func(header *tar.Header, body []byte) {
		header.ModTime = time.Unix(0, 0)
		header.Size = int64(len(body))
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "crw", Mode: 0o755, Typeflag: tar.TypeReg}, raw)
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
		add(&tar.Header{Name: name, Linkname: "crw", Mode: 0o777, Typeflag: tar.TypeSymlink}, nil)
	}
	for _, licence := range []string{"LICENSE", "packages/codex-thread-bridge/LICENSE"} {
		add(&tar.Header{Name: licence, Mode: 0o644, Typeflag: tar.TypeReg}, readFile(t, filepath.Join(moduleRoot, licence)))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "crw_"+version+"_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz")
	writeFile(t, path, buf.String(), 0o644)
	return path
}

func manifestVersion(t *testing.T) string {
	t.Helper()
	var manifest struct{ Version string }
	if err := json.Unmarshal(readFile(t, filepath.Join(moduleRoot, "plugins", "crw", ".codex-plugin", "plugin.json")), &manifest); err != nil || manifest.Version == "" {
		t.Fatalf("the plugin manifest names no version: %v", err)
	}
	return manifest.Version
}

// replaceCache replaces the Codex home's crw plugin cache wholesale, as a plugin install does,
// with the working-tree package at <cache>/crw/crw/<version>, whose files named in overrides are
// replaced by the given sources. It returns that version directory.
func (h *isolated) replaceCache(t *testing.T, version string, overrides map[string]string) string {
	t.Helper()
	cache := filepath.Join(h.codex, "plugins", "cache", "crw")
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "crw", version)
	h.mustBeInside(t, dir)
	copyTree(t, filepath.Join(moduleRoot, "plugins", "crw"), dir)
	for rel, source := range overrides {
		writeFile(t, filepath.Join(dir, rel), string(readFile(t, source)), 0o644)
	}
	return dir
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, raw, info.Mode().Perm())
		}
		return fmt.Errorf("%s is neither a file, a directory nor a link", path)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- the declared commands, as the host caches them

// stopCommand is the one Stop command a cached package declares.
func stopCommand(t *testing.T, payload string) string {
	t.Helper()
	var declaration struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
			}
		}
	}
	if err := json.Unmarshal(readFile(t, filepath.Join(payload, "wiring", "hooks", "stop-recording-completion.json")), &declaration); err != nil {
		t.Fatal(err)
	}
	stop := declaration.Hooks["Stop"]
	if len(declaration.Hooks) != 1 || len(stop) != 1 || len(stop[0].Hooks) != 1 || stop[0].Hooks[0].Type != "command" {
		t.Fatalf("expected exactly one Stop command hook in %s", payload)
	}
	return stop[0].Hooks[0].Command
}

// mcpServer is the bridge server a cached package declares.
func mcpServer(t *testing.T, payload string) (string, []string, string) {
	t.Helper()
	var declaration struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Cwd     string
		}
	}
	if err := json.Unmarshal(readFile(t, filepath.Join(payload, "wiring", "mcp.json")), &declaration); err != nil {
		t.Fatal(err)
	}
	server, ok := declaration.MCPServers["codex-thread-bridge"]
	if !ok || len(declaration.MCPServers) != 1 {
		t.Fatalf("expected only codex-thread-bridge in %s", payload)
	}
	return server.Command, server.Args, server.Cwd
}

// ---- processes

type outcome struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

// run starts argv in dir with exactly env and stdin and waits for it.
func run(t *testing.T, dir string, env []string, stdin string, argv ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	if ctx.Err() != nil {
		t.Fatalf("%q did not exit within %s", argv, commandTimeout)
	}
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%q: %v", argv, err)
	}
	return outcome{code, stdout.String(), stderr.String(), elapsed}
}

// mustRun runs a crw binary in the isolated environment and requires exit 0.
func (h *isolated) mustRun(t *testing.T, binary string, args ...string) outcome {
	t.Helper()
	got := run(t, h.root, h.env(), "", append([]string{binary}, args...)...)
	if got.code != 0 {
		t.Fatalf("%s %q: exit %d\nstdout %s\nstderr %s", binary, args, got.code, got.stdout, got.stderr)
	}
	return got
}

// crw runs `<binary> install ...` in the isolated environment and decodes its JSON report.
func (h *isolated) crw(t *testing.T, binary string, args ...string) (map[string]any, int) {
	t.Helper()
	got := run(t, h.root, h.env(), "", append([]string{binary, "install"}, args...)...)
	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("crw install %q: exit %d, no JSON report (%v)\nstdout %s\nstderr %s", args, got.code, err, got.stdout, got.stderr)
	}
	return report, got.code
}

// at is the value under a path of object keys.
func at(v any, path ...string) any {
	for _, key := range path {
		object, _ := v.(map[string]any)
		v = object[key]
	}
	return v
}

func show(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

// ---- the Stop hook and its journal

// stop runs a declared Stop command through `/bin/sh -c`, the way the host runs it, with the
// Stop payload inline, and records its wall time.
func (h *isolated) stop(t *testing.T, command string, env []string, session, turn string) outcome {
	t.Helper()
	payload := `{"hook_event_name": "Stop", "session_id": "` + session + `", "turn_id": "` + turn + `"}`
	got := run(t, h.root, env, payload, "/bin/sh", "-c", command)
	t.Logf("Stop hook %s/%s: %s, exit %d", session, turn, got.elapsed.Round(time.Millisecond), got.code)
	if got.elapsed >= hookCeiling {
		t.Errorf("Stop hook %s/%s took %s", session, turn, got.elapsed)
	}
	return got
}

// rows is every journal row under the Codex home's journal.
func (h *isolated) rows(t *testing.T) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(h.journal, "[0-9]*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for _, path := range paths {
		var row map[string]any
		if err := json.Unmarshal(readFile(t, path), &row); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, row)
	}
	return out
}

// journaled requires a released turn (exit 0, nothing on stdout) that left exactly one journal
// row for that session and turn, read from the installer's settings, whose outcome is the only
// healthy one here: no relay runs in this test, so the hook asked the guard at the default
// socket the settings name and found nothing there. Any other outcome, adapter_faulted above
// all, is a hook that failed on this install.
func (h *isolated) journaled(t *testing.T, got outcome, session, turn string) {
	t.Helper()
	if got.code != 0 || got.stdout != "" {
		t.Fatalf("Stop %s/%s: exit %d stdout %q stderr %q; want exit 0 and nothing on stdout", session, turn, got.code, got.stdout, got.stderr)
	}
	var found []map[string]any
	for _, row := range h.rows(t) {
		if row["sessionId"] == session && row["turnId"] == turn {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("Stop %s/%s: %d journal rows for it (stderr %q); the hook exits 0 on every path, so only a row shows it ran", session, turn, len(found), got.stderr)
	}
	row := found[0]
	if detail, _ := row["detail"].(string); row["adapterOutcome"] != "guard_unreachable" || row["guardInvoked"] != true ||
		!strings.Contains(detail, h.socket) || row["configuration"] != h.settings {
		t.Fatalf("Stop %s/%s: want guard_unreachable at %s with the guard invoked, read from %s; row %s", session, turn, h.socket, h.settings, show(row))
	}
}

// ---- the MCP server

type bridgeAnswer struct {
	tools  []string
	policy map[string]any
	// exe and argv0 are the running bridge's, read from /proc while it answered.
	exe, argv0 string
}

// bridge starts the declared MCP server from the installed version directory with exactly env,
// and asks it initialize, tools/list and get_capabilities over its stdio.
func (h *isolated) bridge(t *testing.T, env []string) bridgeAnswer {
	t.Helper()
	command, args, cwd := mcpServer(t, h.payload)
	if command != "sh" || cwd != "." {
		t.Fatalf("declared server %q %q cwd %q", command, args, cwd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	// The declared "sh" is named by absolute path: the environment names no PATH, or only
	// tripwires and the Codex stub.
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.Dir = filepath.Join(h.payload, cwd)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	lines := bufio.NewReader(stdout)
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := stdin.Write(append(frame, '\n')); err != nil {
			t.Fatalf("%s: %v (stderr %q)", method, err, stderr.String())
		}
		for {
			line, err := lines.ReadBytes('\n')
			if err != nil {
				t.Fatalf("%s: the server closed its output: %v (stderr %q)", method, err, stderr.String())
			}
			var message map[string]any
			if json.Unmarshal(line, &message) != nil || message["method"] != nil || message["id"] != float64(id) {
				continue
			}
			if message["error"] != nil {
				t.Fatalf("%s: %s", method, line)
			}
			result, _ := message["result"].(map[string]any)
			return result
		}
	}
	call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "isolated-home", "version": "1"}})
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var answer bridgeAnswer
	listed, _ := at(call(2, "tools/list", map[string]any{}), "tools").([]any)
	for _, tool := range listed {
		name, _ := at(tool, "name").(string)
		answer.tools = append(answer.tools, name)
	}
	capabilities := call(3, "tools/call", map[string]any{"name": "get_capabilities", "arguments": map[string]any{}})
	if capabilities["isError"] == true {
		t.Fatalf("get_capabilities: %s", show(capabilities))
	}
	answer.policy, _ = at(capabilities, "structuredContent", "executionPolicy").(map[string]any)
	proc := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid))
	if answer.exe, err = os.Readlink(filepath.Join(proc, "exe")); err != nil {
		t.Fatal(err)
	}
	cmdline := readFile(t, filepath.Join(proc, "cmdline"))
	answer.argv0, _, _ = strings.Cut(string(cmdline), "\x00")
	return answer
}

// bridgeAnswers requires the declared server, started with env, to be runtimeDir's crw running
// as the bridge (the pid the host started, exec'd through the pointer's codex-thread-bridge), to
// list exactly the 12 tools and to report the policy the bridge record names: its digest when it
// names one, presence_only when it names none. No tripwire may have fired by then.
func (h *isolated) bridgeAnswers(t *testing.T, runtimeDir string, env []string) {
	t.Helper()
	answer := h.bridge(t, env)
	h.noTripwireFired(t)
	listed := slices.Sorted(slices.Values(answer.tools))
	if !slices.Equal(listed, toolNames) {
		t.Errorf("tools/list %q, want %q", answer.tools, toolNames)
	}
	if want := filepath.Join(runtimeDir, "bin", "crw"); answer.exe != want || answer.argv0 != h.bin("codex-thread-bridge") {
		t.Errorf("the running bridge is %s started as %s, want %s started as %s", answer.exe, answer.argv0, want, h.bin("codex-thread-bridge"))
	}
	var bridgeRecord map[string]any
	if err := json.Unmarshal(readFile(t, h.bridgeRecord), &bridgeRecord); err != nil {
		t.Fatal(err)
	}
	if digest, named := at(bridgeRecord, "executionPolicy", "digest").(string); named {
		if answer.policy["mode"] != "allowlist" || answer.policy["digest"] != digest {
			t.Errorf("the record names the policy digest %s and the bridge reports %s", digest, show(answer.policy))
		}
	} else if answer.policy["mode"] != "presence_only" || answer.policy["digest"] != nil {
		t.Errorf("the record names no policy and the bridge reports %s", show(answer.policy))
	}
}

// ---- IS-1

func (h *isolated) installAndWire(t *testing.T, archiveA string) {
	report, code := h.crw(t, h.downloaded, "install", "--from", archiveA, "--socket", h.fake.SocketPath, "--state", h.relayState)
	if code != 0 || report["command"] != "install" || report["applied"] != true || report["promoted"] != true ||
		report["claimSettled"] != true || report["inService"] != true || report["environment"] != h.envA {
		t.Fatalf("install: exit %d\n%s", code, show(report))
	}
	steps, _ := report["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("install reports no steps:\n%s", show(report))
	}
	for _, step := range steps {
		if at(step, "ok") != true {
			t.Errorf("install step %s", show(step))
		}
	}
	envBin := filepath.Join(h.envA, "bin")
	if at(report, "pointer", "path") != h.current || at(report, "pointer", "target") != h.envA ||
		at(report, "selected", "codex-session-relay") != envBin || at(report, "selected", "codex-thread-bridge") != envBin {
		t.Fatalf("install report: pointer %s selected %s", show(report["pointer"]), show(report["selected"]))
	}
	// The report is read back against the link and the host record it says it wrote.
	if target := h.pointerTarget(t); target != h.envA {
		t.Fatalf("the pointer names %s, the report %s", target, h.envA)
	}
	var hostRecord map[string]any
	if err := json.Unmarshal(readFile(t, h.record), &hostRecord); err != nil {
		t.Fatal(err)
	}
	if at(hostRecord, "selected", "codex-session-relay") != envBin || at(hostRecord, "selected", "codex-thread-bridge") != envBin || at(hostRecord, "pointer", "path") != h.current {
		t.Fatalf("host record selected %s pointer %s", show(hostRecord["selected"]), show(hostRecord["pointer"]))
	}

	// The bin/ layout: the downloaded binary's bytes, and the three names beside it.
	if fileDigest(t, filepath.Join(envBin, "crw")) != fileDigest(t, h.downloaded) {
		t.Error("bin/crw is not the archived binary")
	}
	if info, err := os.Lstat(filepath.Join(envBin, "crw")); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/crw: %v %v", info, err)
	}
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
		if link, err := os.Readlink(filepath.Join(envBin, name)); err != nil || link != "crw" {
			t.Errorf("bin/%s -> %q %v, want a link to crw", name, link, err)
		}
	}
	for _, licence := range []string{"LICENSE", "packages/codex-thread-bridge/LICENSE"} {
		if !bytes.Equal(readFile(t, filepath.Join(h.envA, licence)), readFile(t, filepath.Join(moduleRoot, licence))) {
			t.Errorf("%s was not unpacked as released", licence)
		}
	}

	// The two records the declared commands read, written by the installed runtime itself.
	crw := h.bin("crw")
	if report, code := h.crw(t, crw, "hook", "--owner", "plugin"); code != 0 || at(report, "settings", "outcome") != "config_created" {
		t.Fatalf("hook: exit %d\n%s", code, show(report))
	}
	bridgeArgs := []string{"--bridge-arg=--socket", "--bridge-arg=" + h.fake.SocketPath, "--bridge-arg=--state-dir", "--bridge-arg=" + h.ledger}
	if report, code := h.crw(t, crw, append([]string{"register-mcp", "--owner", "plugin"}, bridgeArgs...)...); code != 0 || report["outcome"] != "record_created" {
		t.Fatalf("register-mcp: exit %d\n%s", code, show(report))
	}
	for _, path := range []string{h.settings, h.bridgeRecord, h.record} {
		if bytes.Contains(bytes.ToLower(readFile(t, path)), []byte("python")) {
			t.Errorf("%s names python:\n%s", path, readFile(t, path))
		}
	}
	h.settingsAccepted(t)

	command := stopCommand(t, h.payload)
	h.journaled(t, h.stop(t, command, h.hookEnv(), "is1-session", "is1-turn"), "is1-session", "is1-turn")
	// HOME alone first, as the least a host passes; every later start adds the tripwire PATH.
	h.bridgeAnswers(t, h.envA, h.bareBridgeEnv())

	// The host's execution policy: the record is moved aside, as its repair says, and written
	// again naming the policy by path and digest; the bridge then starts under that policy.
	if err := os.Rename(h.bridgeRecord, h.bridgeRecord+".before-policy"); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(h.root, "execution-policy.json")
	writeFile(t, policy, `{"allowed": [{"model": "gpt-5", "efforts": ["high"]}]}`, 0o644)
	report, code = h.crw(t, crw, append([]string{"register-mcp", "--owner", "plugin", "--execution-policy", policy}, bridgeArgs...)...)
	if code != 0 || report["outcome"] != "record_created" || at(report, "executionPolicy", "digest") != fileDigest(t, policy) {
		t.Fatalf("register-mcp --execution-policy: exit %d\n%s", code, show(report))
	}
	h.bridgeAnswers(t, h.envA, h.bridgeEnv())

	h.mustRun(t, h.bin("codex-session-relay"), "--help")
	h.relayScope(t)
	h.noTripwireFired(t)
}

// relayScope asks the selected relay what `crw install`'s swap gate asks it, `service status`
// with the gate's --socket and --state, in the isolated environment: it must read the scope
// registry under the temporary root, not the one under this user's passwd home.
func (h *isolated) relayScope(t *testing.T) {
	t.Helper()
	got := run(t, h.root, h.env("CODEX_SESSION_RELAY_STATE="+h.relayState), "", h.bin("codex-session-relay"), "--socket", h.fake.SocketPath, "--state", h.relayState, "service", "status")
	var status map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &status); err != nil {
		t.Fatalf("service status: exit %d, no JSON (%v)\nstdout %s\nstderr %s", got.code, err, got.stdout, got.stderr)
	}
	if status["scopeRoot"] != h.scopes || status["scopeAuthority"] != "isolated" {
		t.Fatalf("the relay the swap gate asks reads the scope registry %v (%v), want %s (isolated)", status["scopeRoot"], status["scopeAuthority"], h.scopes)
	}
}

// settingsAccepted reads the settings the installer wrote: the Go hook's own validator has no
// complaint, the plugin owns them, and the relay and adapter they name are files through the
// pointer. The Stops the test then sends are what show the hook answering under them.
func (h *isolated) settingsAccepted(t *testing.T) {
	t.Helper()
	document, err := hook.Decode(readFile(t, h.settings))
	if err != nil {
		t.Fatal(err)
	}
	if complaints := hook.Complaints(document); len(complaints) != 0 {
		t.Fatalf("the Go hook refuses the installer's settings: %q", complaints)
	}
	fields := map[string]any{}
	if object, ok := document.(hook.Object); ok {
		for _, field := range object {
			fields[field.Key] = field.Value
		}
	}
	if fields["owner"] != "plugin" {
		t.Errorf("the settings' owner is %v, want plugin", fields["owner"])
	}
	for _, key := range []string{"relayExecutable", "adapterEntryPoint"} {
		path, _ := fields[key].(string)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the settings' %s %q is not a file: %v", key, path, err)
		}
	}
}

func (h *isolated) pointerTarget(t *testing.T) string {
	t.Helper()
	link, err := os.Readlink(h.current)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(h.dest, link)
	}
	return filepath.Clean(link)
}

// ---- IS-7

func (h *isolated) cacheReplacement(t *testing.T) {
	// Neither native command names the version cache, which every install replaces wholesale.
	command := stopCommand(t, h.payload)
	launcher := string(readFile(t, filepath.Join(h.payload, "wiring", "crw-bridge.sh")))
	for name, text := range map[string]string{"the Stop command": command, "crw-bridge.sh": launcher} {
		if strings.Contains(text, "PLUGIN_ROOT") || strings.Contains(text, "plugins/cache") || !strings.Contains(text, "$HOME/.local/share/crw-runtime/current/bin/") {
			t.Errorf("%s does not reach the runtime through the pointer alone:\n%s", name, text)
		}
	}

	// The native command a turn fixed before the cache was replaced still journals the Stop.
	fixedEnv := h.hookEnv()
	h.payload = h.replaceCache(t, h.manifestVersion+"-replacement", nil)
	h.journaled(t, h.stop(t, command, fixedEnv, "is7-replaced", "t"), "is7-replaced", "t")
	h.bridgeAnswers(t, h.envA, h.bridgeEnv())

	// With the pointer's target gone the turn is released unrecorded, and the bridge refuses
	// loudly, naming the runtime it could not start.
	target := h.pointerTarget(t)
	if err := os.Rename(target, target+".moved-aside"); err != nil {
		t.Fatal(err)
	}
	before := len(h.rows(t))
	got := h.stop(t, command, h.hookEnv(), "is7-gone", "t")
	t.Logf("Stop hook with the runtime gone: stderr %q", got.stderr)
	if got.code != 0 || got.stdout != "" || len(h.rows(t)) != before {
		t.Errorf("runtime gone: exit %d stdout %q, %d rows from %d", got.code, got.stdout, len(h.rows(t)), before)
	}
	_, args, cwd := mcpServer(t, h.payload)
	refused := run(t, filepath.Join(h.payload, cwd), h.bridgeEnv(), "", append([]string{"/bin/sh"}, args...)...)
	t.Logf("MCP launcher with the runtime gone: exit %d stderr %q", refused.code, refused.stderr)
	if refused.code == 0 || refused.stdout != "" || !strings.Contains(refused.stderr, h.bin("codex-thread-bridge")) {
		t.Errorf("runtime gone: the MCP launcher exited %d stdout %q stderr %q; want nonzero, naming the runtime", refused.code, refused.stdout, refused.stderr)
	}
	if err := os.Rename(target+".moved-aside", target); err != nil {
		t.Fatal(err)
	}
	h.journaled(t, h.stop(t, command, h.hookEnv(), "is7-back", "t"), "is7-back", "t")
	h.noTripwireFired(t)
}

// ---- IS-8

func (h *isolated) updateAndRollBack(t *testing.T, archiveA, archiveB string) {
	crw := h.bin("crw")
	gate := []string{"--socket", h.fake.SocketPath, "--state", h.relayState}
	report, code := h.crw(t, crw, append([]string{"update", "--from", archiveB}, gate...)...)
	if code != 0 || report["promoted"] != true || report["environment"] != h.envB || at(report, "pointer", "previousTarget") != h.envA {
		t.Fatalf("update: exit %d\n%s", code, show(report))
	}
	h.selects(t, h.envB, versionB)
	h.journaled(t, h.stop(t, stopCommand(t, h.payload), h.hookEnv(), "is8-updated", "t"), "is8-updated", "t")
	h.bridgeAnswers(t, h.envB, h.bridgeEnv())

	// An archive that does not hash to what SHA256SUMS lists is refused before anything is
	// unpacked: the destination, the host record and the pointer are as they were. The
	// substitute is a whole, valid release archive (A's) under B's name, so nothing after the
	// digest check would refuse it.
	tampered := filepath.Join(h.root, "tampered", filepath.Base(archiveB))
	writeFile(t, tampered, string(readFile(t, archiveA)), 0o644)
	writeFile(t, filepath.Join(filepath.Dir(tampered), "SHA256SUMS"), string(readFile(t, filepath.Join(filepath.Dir(archiveB), "SHA256SUMS"))), 0o644)
	listing, recordBefore := h.destination(t), readFile(t, h.record)
	report, code = h.crw(t, crw, append([]string{"update", "--from", tampered}, gate...)...)
	// Refused for the digest and nothing else: the archive's own digest and the one SHA256SUMS
	// lists for B are both named.
	refusal, _ := report["refused"].(string)
	note, _ := report["note"].(string)
	if code != 1 || report["applied"] != false || !strings.Contains(refusal, " hashes to "+fileDigest(t, archiveA)) ||
		!strings.Contains(refusal, " lists "+fileDigest(t, archiveB)) || !strings.HasPrefix(note, "nothing was unpacked") {
		t.Fatalf("a tampered archive: exit %d, want a refusal naming the digest mismatch before unpacking\n%s", code, show(report))
	}
	if after := h.destination(t); !slices.Equal(after, listing) || !bytes.Equal(readFile(t, h.record), recordBefore) || h.pointerTarget(t) != h.envB {
		t.Fatalf("a refused archive changed the host: destination %q -> %q, pointer %s", listing, after, h.pointerTarget(t))
	}

	report, code = h.crw(t, crw, append([]string{"rollback"}, gate...)...)
	if code != 0 || report["applied"] != true || report["environment"] != h.envA {
		t.Fatalf("rollback: exit %d\n%s", code, show(report))
	}
	h.selects(t, h.envA, h.versionA)
	h.journaled(t, h.stop(t, stopCommand(t, h.payload), h.hookEnv(), "is8-rolled-back", "t"), "is8-rolled-back", "t")
	h.bridgeAnswers(t, h.envA, h.bridgeEnv())

	report, code = h.crw(t, crw, "remove", h.envA)
	if code != 1 || report["applied"] != false || !strings.Contains(fmt.Sprint(report["refused"]), "the host record selects this runtime") {
		t.Fatalf("removing the selected runtime: exit %d, want a refusal naming the selection\n%s", code, show(report))
	}
	if outside := h.outside(report); len(outside) != 0 {
		t.Errorf("the refused remove names paths outside the temporary root: %q\n%s", outside, show(report))
	}
	if _, err := os.Stat(filepath.Join(h.envA, "bin", "crw")); err != nil {
		t.Fatalf("a refused remove removed the selected runtime: %v", err)
	}
	listing, recordBefore = h.destination(t), readFile(t, h.record)
	report, code = h.crw(t, crw, "remove", h.envB)
	if held := heldByHost(report, code); held != nil {
		// This host's own processes, which the test neither started nor can stop (an Azure
		// runner's root WALinuxAgent runs a relative script from a working directory no other
		// user may read), keep the runtime from being ruled out of use: remove refused and
		// removed nothing, and the rest of IS-8 goes on without the removal.
		t.Logf("remove refused for this host's own processes, which the test did not start; the success assertions are skipped:\n%s", show(held))
		if after := h.destination(t); !slices.Equal(after, listing) || !bytes.Equal(readFile(t, h.record), recordBefore) {
			t.Fatalf("a refused remove changed the host: destination %q -> %q\n%s", listing, after, show(report))
		}
		if _, err := os.Stat(filepath.Join(h.envB, "bin", "crw")); err != nil {
			t.Fatalf("a refused remove removed the runtime: %v", err)
		}
		if !strings.Contains(fmt.Sprint(report["recoveryRequires"]), "delete "+h.envB) {
			t.Errorf("the refused remove gives no removal by hand\n%s", show(report))
		}
		delete(report, "unreadableProcesses") // this host's, which lie where they lie
		if outside := h.outside(report); len(outside) != 0 {
			t.Errorf("the refused remove names paths outside the temporary root: %q\n%s", outside, show(report))
		}
	} else {
		if code != 0 || report["removed"] != true {
			t.Fatalf("removing the unselected runtime: exit %d\n%s", code, show(report))
		}
		// The remove's verdict read the relay records of this environment alone: the registry
		// CODEX_SESSION_RELAY_SCOPE_DIR names, not the one under this user's passwd home, and
		// the state directories under the temporary root. Nothing the answer names lies outside
		// it.
		registries, _ := at(report, "relayRecords", "scopeRegistries").([]any)
		states, _ := at(report, "relayRecords", "stateDirectories").([]any)
		if len(registries) != 1 || registries[0] != h.scopes || len(states) == 0 {
			t.Errorf("the remove read the scope registries %v and state directories %v, want %s alone and at least the state root", registries, states, h.scopes)
		}
		if outside := h.outside(report); len(outside) != 0 {
			t.Errorf("the remove names paths outside the temporary root: %q\n%s", outside, show(report))
		}
		if _, err := os.Lstat(h.envB); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the removed runtime is still there: %v", err)
		}
	}
	h.journaled(t, h.stop(t, stopCommand(t, h.payload), h.hookEnv(), "is8-removed", "t"), "is8-removed", "t")
	h.noTripwireFired(t)
}

// unruledDetail is the refusal `crw install remove` gives when a process it could not rule out
// may run out of the runtime, and nothing else refused.
const unruledDetail = "what a live process runs could not be read, so it cannot be ruled out that it runs out of this directory"

// heldByHost is the processes a remove answer names when it refused for them alone and every one
// is this host's own: not this test process or a descendant of it, and another uid's. nil for
// any other answer, which the caller judges as it would without this host.
func heldByHost(report map[string]any, code int) []any {
	processes, _ := report["unreadableProcesses"].([]any)
	if code != 1 || report["applied"] != false || report["refused"] != unruledDetail || len(processes) == 0 {
		return nil
	}
	for key := range report {
		if !slices.Contains([]string{"command", "applied", "directory", "refused", "unreadableProcesses", "recoveryRequires", "note"}, key) {
			return nil
		}
	}
	for _, raw := range processes {
		process, _ := raw.(map[string]any)
		pid, isPid := process["pid"].(float64)
		uid, isUid := process["uid"].(float64)
		if !isPid || !isUid || int(uid) == os.Getuid() || startedHere(int(pid)) {
			return nil
		}
	}
	return processes
}

// A remove refused for this host's processes alone is told from every other refusal: another
// uid's process the test did not start is the host's; this test process, a process it started,
// one of this uid, another reason or another field is not, and neither is a remove that succeeded.
func TestHeldByHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc for the parent chain")
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	stranger := float64(os.Getuid() + 1)
	refusal := func(processes ...map[string]any) map[string]any {
		list := []any{}
		for _, p := range processes {
			list = append(list, p)
		}
		return map[string]any{"command": "remove", "applied": false, "directory": "/d", "refused": unruledDetail,
			"unreadableProcesses": list, "recoveryRequires": "remove it by hand", "note": "nothing was removed and nothing was written."}
	}
	agent := map[string]any{"pid": float64(1), "uid": stranger, "why": "its command line runs bin/WALinuxAgent.egg relative to its working directory"}
	if held := heldByHost(refusal(agent), 1); len(held) != 1 {
		t.Errorf("another uid's process the test did not start: %v", held)
	}
	withRegistration := refusal(agent)
	withRegistration["registrations"] = []any{"/d/bin/crw"}
	otherReason := refusal(agent)
	otherReason["refused"] = "live processes run out of this directory"
	for label, c := range map[string]struct {
		report map[string]any
		code   int
	}{
		"this test process":          {refusal(map[string]any{"pid": float64(os.Getpid()), "uid": stranger}), 1},
		"a process the test started": {refusal(agent, map[string]any{"pid": float64(child.Process.Pid), "uid": stranger}), 1},
		"a process of this uid":      {refusal(map[string]any{"pid": float64(1), "uid": float64(os.Getuid())}), 1},
		"no process named":           {refusal(), 1},
		"another field":              {withRegistration, 1},
		"another reason":             {otherReason, 1},
		"another exit":               {refusal(agent), 0},
		"a process without its uid":  {refusal(map[string]any{"pid": float64(1)}), 1},
	} {
		if held := heldByHost(c.report, c.code); held != nil {
			t.Errorf("%s: taken for this host's own: %v", label, held)
		}
	}
}

// startedHere is whether pid is this test process or descends from it, read up the parent chain
// in /proc. A pid whose chain cannot be read is not taken for one the test started: heldByHost
// asks this only of another uid's process, which nothing the test starts is.
func startedHere(pid int) bool {
	for steps := 0; pid > 1 && steps < 1<<12; steps++ {
		if pid == os.Getpid() {
			return true
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return false
		}
		// The command name, in parentheses, may hold spaces; the parent pid follows the state.
		fields := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
		if len(fields) < 2 {
			return false
		}
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			return false
		}
	}
	return pid == os.Getpid()
}

// selects requires the pointer and `crw install status` to agree on runtimeDir, and the runtime
// reached through the pointer to report version.
func (h *isolated) selects(t *testing.T, runtimeDir, version string) {
	t.Helper()
	link, err := os.Readlink(h.current)
	if err != nil {
		t.Fatal(err)
	}
	status, code := h.crw(t, h.bin("crw"), "status")
	if code != 0 || h.pointerTarget(t) != runtimeDir || at(status, "runtime", "target") != link ||
		at(status, "runtime", "targetResolves") != runtimeDir || at(status, "runtime", "agrees") != true || status["selected"] != "go-binary" {
		t.Fatalf("pointer %s (link %s), status: exit %d\n%s", h.pointerTarget(t), link, code, show(status["runtime"]))
	}
	if got := strings.TrimSpace(h.mustRun(t, h.bin("crw"), "version").stdout); got != version {
		t.Fatalf("the runtime through the pointer is %s, want %s", got, version)
	}
}

// destination lists the installer's destination.
func (h *isolated) destination(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.dest)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// ---- files

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
