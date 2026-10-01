package pluginwiring

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// pointerBin is where both declared commands find the runtime: the installer's `current`
// pointer under $HOME, never XDG_DATA_HOME (docs/port/decisions.md sections 11 and 12).
const pointerBin = ".local/share/crw-runtime/current/bin"

// commandTimeout bounds every spawned shell; each one is waited on, never slept for.
const commandTimeout = 30 * time.Second

// moduleRoot is the checkout this package sits in; tests may change directory, so it is fixed once.
var moduleRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}()

// linkDir holds the names the tests give the built crw (bridgeEntry).
var linkDir string

func TestMain(m *testing.M) {
	testsupport.Main(m, func(root string) (func() error, error) {
		linkDir = filepath.Join(root, "links")
		return nil, os.Mkdir(linkDir, 0o755)
	})
}

// builtCrw is the real multi-call binary (testsupport.CRW).
func builtCrw(t *testing.T) string {
	t.Helper()
	return testsupport.CRW(t)
}

func repoRoot(*testing.T) string { return moduleRoot }

func packageRoot(t *testing.T) string {
	return filepath.Join(repoRoot(t), "plugins", "crw")
}

// stopCommandIn is the one Stop command hook a declaration file registers.
func stopCommandIn(t *testing.T, path string) (string, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	groups := document.Hooks["Stop"]
	if len(document.Hooks) != 1 || len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Type != "command" {
		t.Fatalf("expected exactly one Stop command hook, got %s", data)
	}
	return groups[0].Hooks[0].Command, groups[0].Hooks[0].Timeout
}

// declaredStopCommand is the command string the shipped Stop declaration registers.
func declaredStopCommand(t *testing.T) (string, int) {
	return stopCommandIn(t, filepath.Join(packageRoot(t), "wiring", "hooks", "stop-recording-completion.json"))
}

// declaredServerEntry is the shipped MCP declaration of the bridge.
func declaredServerEntry(t *testing.T) (command string, args []string, cwd string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Cwd     string   `json:"cwd"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	server, ok := document.Servers["codex-thread-bridge"]
	if !ok || len(document.Servers) != 1 {
		t.Fatalf("expected only codex-thread-bridge, got %s", data)
	}
	return server.Command, server.Args, server.Cwd
}

type outcome struct {
	code           int
	pid            int
	stdout, stderr string
}

// run starts argv in dir with exactly env and stdin, and waits for it to exit.
func run(t *testing.T, dir string, env []string, stdin string, argv ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("%v did not exit within %s", argv, commandTimeout)
	}
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return outcome{code, cmd.Process.Pid, stdout.String(), stderr.String()}
}

func shell(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	return sh
}

// compatibilityNames are the links the installer places beside bin/crw (decision 38).
var compatibilityNames = []string{"codex-session-relay", "codex-thread-bridge"}

// pointerTo lays out, under home, a pointer `current` -> `bin-test/` whose bin/ holds crw
// (writing it with write) and the three compatibility names as links to it, as the installer
// places them.
func pointerTo(t *testing.T, home string, write func(path string)) {
	t.Helper()
	bin := filepath.Join(home, ".local", "share", "crw-runtime", "bin-test", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin-test", filepath.Join(home, ".local", "share", "crw-runtime", "current")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(bin, "crw"))
	for _, name := range compatibilityNames {
		if err := os.Symlink("crw", filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeRuntime installs a pointer whose bin/crw is a POSIX sh script using builtins only (the
// MCP environment carries no PATH). It records its $0, its pid, its arguments and one line of
// stdin in home/witness, prints `answer` and exits `code`. Reached through a link, $0 is the
// link's path and the script is still crw's.
func fakeRuntime(t *testing.T, home, answer string, code int) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"{ printf 'argv0=%s\\n' \"$0\"; printf 'pid=%s\\n' \"$$\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; printf 'stdin=%s\\n' \"$line\"; } > \"$HOME/witness\"\n" +
		"printf '%s' '" + answer + "'\n" +
		"exit " + strconv.Itoa(code) + "\n"
	pointerTo(t, home, func(path string) {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	})
}

// realRuntime installs a pointer whose bin/crw is the built binary.
func realRuntime(t *testing.T, home string) {
	t.Helper()
	built := builtCrw(t)
	pointerTo(t, home, func(path string) {
		raw, err := os.ReadFile(built)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o755); err != nil {
			t.Fatal(err)
		}
	})
}

type witness struct {
	argv0, pid, stdin string
	args              []string
}

func readWitness(t *testing.T, home string) (witness, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "witness"))
	if errors.Is(err, os.ErrNotExist) {
		return witness{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var w witness
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "argv0":
			w.argv0 = value
		case "pid":
			w.pid = value
		case "arg":
			w.args = append(w.args, value)
		case "stdin":
			w.stdin = value
		}
	}
	return w, true
}

// homes returns an ordinary HOME and one whose path needs the quoting the declaration carries.
func homes(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	var out []string
	for _, name := range []string{"plain", "it's $HOME really"} {
		home := filepath.Join(root, name)
		if err := os.Mkdir(home, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, home)
	}
	return out
}

// hookEnv is what the host hands a plugin hook: a shell with CODEX_HOME and PLUGIN_ROOT set, and
// the store's live-state refusal, as for every process a test starts (testsupport).
func hookEnv(t *testing.T, home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"),
		"PLUGIN_ROOT=" + packageRoot(t), testsupport.RefuseLiveStateEnv + "=1"}
}

const stopPayload = `{"hook_event_name": "Stop", "session_id": "s", "turn_id": "t"}`

// Neither declared command names the version cache, which every install replaces wholesale: the
// Stop command reaches the runtime through the pointer alone, and the launcher (which lives in
// the cache) only execs it.
func TestDeclaredCommands_name_nothing_in_the_version_cache(t *testing.T) {
	command, _ := declaredStopCommand(t)
	want := `"$HOME/.local/share/crw-runtime/current/bin/crw" hook ` + Flag + `; exit 0`
	if command != want {
		t.Fatalf("declared Stop command %q, want %q", command, want)
	}
	launcher, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", launcherName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(launcher), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "#!/bin/sh" || lines[2] != `exec "$HOME/.local/share/crw-runtime/current/bin/codex-thread-bridge" `+Flag+` "$@"` {
		t.Fatalf("launcher:\n%s", launcher)
	}
}

func TestStopCommand_releases_the_turn_when_the_runtime_is_absent(t *testing.T) {
	sh := shell(t)
	command, _ := declaredStopCommand(t)
	for _, home := range homes(t) {
		got := run(t, home, hookEnv(t, home), stopPayload, sh, "-c", command)
		if got.code != 0 || got.stdout != "" {
			t.Errorf("HOME=%q: exit %d stdout %q stderr %q; want exit 0 and empty stdout", home, got.code, got.stdout, got.stderr)
		}
	}
}

// HOME unset: sh expands "$HOME" to nothing, the absolute path names no binary, `; exit 0` holds.
func TestStopCommand_releases_the_turn_when_HOME_is_unset(t *testing.T) {
	sh := shell(t)
	command, _ := declaredStopCommand(t)
	if _, err := os.Stat("/" + pointerBin + "/crw"); err == nil {
		t.Skip("this machine has a runtime at /" + pointerBin + "/crw, so an empty HOME would reach it")
	}
	dir := t.TempDir()
	got := run(t, dir, []string{"PATH=" + os.Getenv("PATH")}, stopPayload, sh, "-c", command)
	if got.code != 0 || got.stdout != "" {
		t.Fatalf("exit %d stdout %q stderr %q; want exit 0 and empty stdout", got.code, got.stdout, got.stderr)
	}
}

func TestStopCommand_runs_crw_hook_through_the_pointer_and_forwards_its_answer(t *testing.T) {
	sh := shell(t)
	command, _ := declaredStopCommand(t)
	const block = `{"decision":"block","reason":"r","continue":true}`
	for _, home := range homes(t) {
		fakeRuntime(t, home, block, 0)
		got := run(t, home, hookEnv(t, home), stopPayload+"\n", sh, "-c", command)
		w, ran := readWitness(t, home)
		if !ran {
			t.Fatalf("HOME=%q: the runtime was not started (exit %d stderr %q)", home, got.code, got.stderr)
		}
		if want := filepath.Join(home, pointerBin, "crw"); w.argv0 != want || !slices.Equal(w.args, []string{"hook", Flag}) {
			t.Errorf("HOME=%q: ran %q %q, want %q [hook %s]", home, w.argv0, w.args, want, Flag)
		}
		if w.stdin != stopPayload {
			t.Errorf("HOME=%q: the runtime read %q, want the host's payload", home, w.stdin)
		}
		if got.code != 0 || got.stdout != block || got.stderr != "" {
			t.Errorf("HOME=%q: exit %d stdout %q stderr %q", home, got.code, got.stdout, got.stderr)
		}
	}
}

// Exit 2 is the host's blocking code. Whatever the runtime exits with, the declaration exits 0.
func TestStopCommand_never_passes_the_runtime_exit_status_to_the_host(t *testing.T) {
	sh := shell(t)
	command, _ := declaredStopCommand(t)
	for _, code := range []int{1, 2, 3} {
		home := t.TempDir()
		fakeRuntime(t, home, "", code)
		got := run(t, home, hookEnv(t, home), stopPayload+"\n", sh, "-c", command)
		if _, ran := readWitness(t, home); !ran || got.code != 0 {
			t.Errorf("runtime exit %d: ran=%v, declaration exit %d; want ran and 0", code, ran, got.code)
		}
	}
}

func TestStopCommand_stays_within_the_timeout_the_host_clamps(t *testing.T) {
	if _, timeout := declaredStopCommand(t); timeout <= 0 || timeout > 10 {
		t.Fatalf("timeout %d, want 1..10", timeout)
	}
}

// The MCP server gets HOME and its working directory, nothing else (docs/plugin-packaging.md).
func bridgeEnv(home string) []string { return []string{"HOME=" + home} }

// The launcher execs the pointer's codex-thread-bridge, the installer's link to crw: the file
// that runs is crw and argv[0] ends in codex-thread-bridge, where /proc scans look for bridges.
func TestBridgeLauncher_execs_crw_as_codex_thread_bridge_through_the_pointer(t *testing.T) {
	sh := shell(t)
	command, args, cwd := declaredServerEntry(t)
	if command != "sh" || cwd != "." || !slices.Equal(args, []string{"./wiring/" + launcherName}) {
		t.Fatalf("declared command %q %q cwd %q, want sh [./wiring/%s] and .", command, args, cwd, launcherName)
	}
	for _, home := range homes(t) {
		fakeRuntime(t, home, "frame", 0)
		argv := append(append([]string{sh}, args...), "--socket", "/x y")
		got := run(t, filepath.Join(packageRoot(t), cwd), bridgeEnv(home), "hello\n", argv...)
		w, ran := readWitness(t, home)
		if !ran {
			t.Fatalf("HOME=%q: the runtime was not started (exit %d stderr %q)", home, got.code, got.stderr)
		}
		if want := filepath.Join(home, pointerBin, "codex-thread-bridge"); w.argv0 != want {
			t.Errorf("HOME=%q: argv[0] %q, want %q", home, w.argv0, want)
		}
		if want := []string{Flag, "--socket", "/x y"}; !slices.Equal(w.args, want) {
			t.Errorf("HOME=%q: args %q, want %q", home, w.args, want)
		}
		// exec, not a child: the process Codex started is the bridge, so its stdio is the MCP stream.
		if w.pid != strconv.Itoa(got.pid) {
			t.Errorf("HOME=%q: runtime pid %s, launcher pid %d; the launcher must exec it", home, w.pid, got.pid)
		}
		if w.stdin != "hello" || got.stdout != "frame" || got.code != 0 {
			t.Errorf("HOME=%q: stdin %q stdout %q exit %d", home, w.stdin, got.stdout, got.code)
		}
	}
}

// XDG_DATA_HOME is not honoured on this path (decisions.md section 11): a runtime found only
// there is not the one either command starts, even when the variable is set.
func TestDeclaredCommands_ignore_XDG_DATA_HOME(t *testing.T) {
	sh := shell(t)
	home, other := t.TempDir(), t.TempDir()
	fakeRuntime(t, home, "", 0)
	fakeRuntime(t, other, "", 0)
	xdg := "XDG_DATA_HOME=" + filepath.Join(other, ".local", "share")
	command, _ := declaredStopCommand(t)
	_, args, cwd := declaredServerEntry(t)
	for _, start := range []struct {
		name string
		run  func()
	}{
		{"crw", func() { run(t, home, append(hookEnv(t, home), xdg), stopPayload+"\n", sh, "-c", command) }},
		{"codex-thread-bridge", func() {
			run(t, filepath.Join(packageRoot(t), cwd), append(bridgeEnv(home), xdg), "\n", append([]string{sh}, args...)...)
		}},
	} {
		os.Remove(filepath.Join(home, "witness"))
		start.run()
		if w, ran := readWitness(t, home); !ran || w.argv0 != filepath.Join(home, pointerBin, start.name) {
			t.Errorf("the runtime under HOME did not run (the fake writes $HOME/witness either way): ran=%v argv0=%q", ran, w.argv0)
		}
	}
}

// Unlike the hook, a server that cannot start says so: nonzero and a reason on stderr.
func TestBridgeLauncher_fails_loudly_when_the_runtime_is_absent(t *testing.T) {
	sh := shell(t)
	_, args, cwd := declaredServerEntry(t)
	home := t.TempDir()
	got := run(t, filepath.Join(packageRoot(t), cwd), bridgeEnv(home), "", append([]string{sh}, args...)...)
	if got.code == 0 || got.stdout != "" || !strings.Contains(got.stderr, filepath.Join(home, pointerBin, "codex-thread-bridge")) {
		t.Fatalf("exit %d stdout %q stderr %q; want nonzero, no stdout, stderr naming the pointer", got.code, got.stdout, got.stderr)
	}
}

// cachedPackage places the shipped launcher in the cache layout of home/.codex, six directories
// below the Codex home, and returns the version directory the host starts it in.
func cachedPackage(t *testing.T, home string) string {
	t.Helper()
	version := filepath.Join(home, ".codex", "plugins", "cache", "crw", "crw", "0.4.0")
	launcher, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", launcherName))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(version, "wiring", launcherName), string(launcher), 0o644)
	return version
}

// The real binary behind both commands: `crw hook` with no settings releases in silence, and the
// launcher reaches the bridge, which answers --version.
func TestDeclaredCommands_reach_the_real_crw_binary(t *testing.T) {
	sh := shell(t)
	home := t.TempDir()
	realRuntime(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	command, _ := declaredStopCommand(t)
	env := append(hookEnv(t, home), "XDG_STATE_HOME="+filepath.Join(home, "state"))
	got := run(t, home, env, stopPayload, sh, "-c", command)
	if got.code != 0 || got.stdout != "" {
		t.Errorf("hook: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
	_, args, cwd := declaredServerEntry(t)
	version := cachedPackage(t, home)
	record := `{"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge", "bridgeExecutable": "` +
		filepath.Join(home, pointerBin, "codex-thread-bridge") + `", "args": []}`
	write(t, filepath.Join(home, ".codex", RecordName), record, 0o600)
	got = run(t, filepath.Join(version, cwd), bridgeEnv(home), "", append(append([]string{sh}, args...), "--version")...)
	if got.code != 0 || got.stdout != mcp.PackageVersion+"\n" {
		t.Errorf("bridge --version: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
}

// The bridge a plugin launch leaves running is the process an execve leaves: the same pid Codex
// started, argv[0] ending in codex-thread-bridge with no plugin-launch flag,
// the recorded policy in its own environment (what /proc/<pid>/environ shows an operator), and
// its executable the runtime's crw.
func TestBridgeLauncher_leaves_the_bridge_in_the_process_codex_started(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	sh := shell(t)
	home := t.TempDir()
	realRuntime(t, home)
	h := launcherHost{root: home, codexHome: filepath.Join(home, ".codex")}
	h.policy = filepath.Join(home, "execution-policy.json")
	h.digest = writePolicy(t, h.policy, "{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"high\"}}}\n")
	h.probe = filepath.Join(home, pointerBin, "codex-thread-bridge")
	socket, ledger := filepath.Join(home, "app-server.sock"), filepath.Join(home, "ledger")
	h.record(t, v2(h, map[string]any{"args": []string{"--socket", socket, "--state-dir", ledger}}))
	_, args, cwd := declaredServerEntry(t)
	version := cachedPackage(t, home)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, args...)
	cmd.Dir = filepath.Join(version, cwd)
	cmd.Env = bridgeEnv(home)
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
	defer func() { _ = cmd.Wait() }()
	defer stdin.Close()
	// An answer to initialize comes from the bridge itself, after every exec.
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":1`) {
		t.Fatalf("initialize answer %q err %v stderr %q", line, err, stderr.String())
	}
	base := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid))
	raw, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if want := []string{filepath.Join(home, pointerBin, "codex-thread-bridge"), "--socket", socket, "--state-dir", ledger}; !slices.Equal(argv, want) {
		t.Errorf("the running bridge's argv %q, want %q", argv, want)
	}
	raw, err = os.ReadFile(filepath.Join(base, "environ"))
	if err != nil {
		t.Fatal(err)
	}
	environ := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	for _, want := range []string{"HOME=" + home, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=" + h.policy, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST=" + h.digest} {
		if !slices.Contains(environ, want) {
			t.Errorf("the running bridge's environment %q lacks %q", environ, want)
		}
	}
	exe, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "crw-runtime", "bin-test", "bin", "crw"); exe != want {
		t.Errorf("the running bridge's executable %q, want %q", exe, want)
	}
}
