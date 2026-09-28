package pluginwiring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

func repoRoot(*testing.T) string { return moduleRoot }

func packageRoot(t *testing.T) string {
	return filepath.Join(repoRoot(t), "plugins", "crw")
}

// declaredStopCommand is the command string the shipped Stop declaration registers.
func declaredStopCommand(t *testing.T) (string, int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", "hooks", "stop-recording-completion.json"))
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

// fakeRuntime installs, under home, a pointer `current` -> `bin-test/` whose bin/crw is a POSIX
// sh script using builtins only (the MCP environment carries no PATH). It records its $0, its
// pid, its arguments and one line of stdin in home/witness, prints `answer` and exits `code`.
func fakeRuntime(t *testing.T, home, answer string, code int) {
	t.Helper()
	bin := filepath.Join(home, ".local", "share", "crw-runtime", "bin-test", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin-test", filepath.Join(home, ".local", "share", "crw-runtime", "current")); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"{ printf 'argv0=%s\\n' \"$0\"; printf 'pid=%s\\n' \"$$\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; printf 'stdin=%s\\n' \"$line\"; } > \"$HOME/witness\"\n" +
		"printf '%s' '" + answer + "'\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "crw"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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

// hookEnv is what the host hands a plugin hook: a shell with CODEX_HOME and PLUGIN_ROOT set.
func hookEnv(t *testing.T, home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"),
		"PLUGIN_ROOT=" + packageRoot(t)}
}

const stopPayload = `{"hook_event_name": "Stop", "session_id": "s", "turn_id": "t"}`

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
		if want := filepath.Join(home, pointerBin, "crw"); w.argv0 != want || !slices.Equal(w.args, []string{"hook", "--plugin-launch"}) {
			t.Errorf("HOME=%q: ran %q %q, want %q [hook --plugin-launch]", home, w.argv0, w.args, want)
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

func TestBridgeLauncher_execs_crw_bridge_through_the_pointer(t *testing.T) {
	sh := shell(t)
	command, args, cwd := declaredServerEntry(t)
	if command != "sh" || cwd != "." {
		t.Fatalf("declared command %q cwd %q, want sh and .", command, cwd)
	}
	for _, home := range homes(t) {
		fakeRuntime(t, home, "frame", 0)
		argv := append(append([]string{sh}, args...), "--socket", "/x y")
		got := run(t, filepath.Join(packageRoot(t), cwd), bridgeEnv(home), "hello\n", argv...)
		w, ran := readWitness(t, home)
		if !ran {
			t.Fatalf("HOME=%q: the runtime was not started (exit %d stderr %q)", home, got.code, got.stderr)
		}
		if want := filepath.Join(home, pointerBin, "crw"); w.argv0 != want {
			t.Errorf("HOME=%q: argv[0] %q, want %q", home, w.argv0, want)
		}
		if want := []string{"bridge", "--plugin-launch", "--socket", "/x y"}; !slices.Equal(w.args, want) {
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
	for _, start := range []func(){
		func() { run(t, home, append(hookEnv(t, home), xdg), stopPayload+"\n", sh, "-c", command) },
		func() {
			run(t, filepath.Join(packageRoot(t), cwd), append(bridgeEnv(home), xdg), "\n", append([]string{sh}, args...)...)
		},
	} {
		os.Remove(filepath.Join(home, "witness"))
		start()
		if w, ran := readWitness(t, home); !ran || w.argv0 != filepath.Join(home, pointerBin, "crw") {
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
	if got.code == 0 || got.stdout != "" || !strings.Contains(got.stderr, filepath.Join(home, pointerBin, "crw")) {
		t.Fatalf("exit %d stdout %q stderr %q; want nonzero, no stdout, stderr naming the pointer", got.code, got.stdout, got.stderr)
	}
}

// The real binary behind both commands: `crw hook` with no settings releases in silence, and the
// launcher reaches `crw bridge`, which answers --version.
func TestDeclaredCommands_reach_the_real_crw_binary(t *testing.T) {
	sh := shell(t)
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH to build crw")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "share", "crw-runtime", "bin-test", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin-test", filepath.Join(home, ".local", "share", "crw-runtime", "current")); err != nil {
		t.Fatal(err)
	}
	build := run(t, repoRoot(t), os.Environ(), "", gobin, "build", "-o", filepath.Join(bin, "crw"), "./cmd/crw")
	if build.code != 0 {
		t.Fatalf("go build: %s", build.stderr)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	command, _ := declaredStopCommand(t)
	env := append(hookEnv(t, home), "XDG_STATE_HOME="+filepath.Join(home, "state"))
	got := run(t, home, env, stopPayload, sh, "-c", command)
	if got.code != 0 || got.stdout != "" {
		t.Errorf("hook: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
	// The bridge finds its record six directories above the launcher, so the package is placed
	// in the cache layout of $HOME/.codex, with a record naming the pointer as the executable.
	_, args, cwd := declaredServerEntry(t)
	version := filepath.Join(home, ".codex", "plugins", "cache", "crw", "crw", "0.4.0")
	launcher, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", "crw-bridge.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(version, "wiring"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "wiring", "crw-bridge.sh"), launcher, 0o644); err != nil {
		t.Fatal(err)
	}
	record := `{"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge", "bridgeExecutable": "` +
		filepath.Join(home, pointerBin, "crw") + `", "args": []}`
	if err := os.WriteFile(filepath.Join(home, ".codex", "crw-bridge-mcp.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	got = run(t, filepath.Join(version, cwd), bridgeEnv(home), "", append(append([]string{sh}, args...), "--version")...)
	if got.code != 0 || got.stdout != "0.1.0\n" {
		t.Errorf("bridge --version: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
}
