package service

import (
	"bytes"
	"errors"

	"encoding/json"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type capture struct {
	Out, Err string
	Code     int
}

func environment(home string) []string {
	env := os.Environ()
	for key, value := range map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/xdg-state", "XDG_CONFIG_HOME": home + "/config", "XDG_DATA_HOME": home + "/data", "CODEX_HOME": home + "/codex", "CODEX_SESSION_RELAY_STATE": home + "/state", "CODEX_SESSION_RELAY_SCOPE_DIR": home + "/scopes", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY": "", "CODEX_SESSION_RELAY_MARKER_ROOT": ""} {
		env = environmentSet(env, key, value)
	}
	return env
}
func invoke(t *testing.T, home string, python bool, args ...string) capture {
	t.Helper()
	prepareParityOwnership(t, home, python, args)
	program := filepath.Join(filepath.Dir(testBinary), "codex-session-relay")
	argv := append([]string{"--state", home + "/state"}, args...)
	if python {
		program = testPython
		argv = append([]string{"--state", home + "/state"}, args...)
	}
	cmd := exec.Command(program, argv...)
	cmd.Env = environment(home)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var e *exec.ExitError
		if !errorsAs(err, &e) {
			t.Fatal(err)
		}
	}
	return capture{out.String(), stderr.String(), cmd.ProcessState.ExitCode()}
}

// emptiedStores remembers each home whose fenced store resetRuntime emptied.
var emptiedStores = map[string]bool{}

// prepareParityOwnership puts the state directory into the ownership state a host running
// the runtime under test has at this step, and nothing more:
//   - an absent store stays absent: each runtime's own absent-store initializer creates it;
//   - a store an earlier invocation left behind that the other runtime owns is that store after
//     a completed takeover to the runtime under test;
//   - the empty file resetRuntime leaves keeps the inode the worker receipt compares byte for
//     byte. Python initializes that file itself, but Go cannot tell it from a legacy unfenced
//     store, so Go's store is created in it as Go's initializer would have created it at Go's
//     first invocation: bound to that invocation's socket, or unbound, in which case Go's first
//     socketed writable open binds it (cutover.md Record, Socket binding), as the previous
//     runtime's own store was bound.
//
// The scope key a helper stamps is the one the runtime computes in the environment it runs in.
func prepareParityOwnership(t *testing.T, home string, python bool, args []string) {
	t.Helper()
	socket := ""
	for i, a := range args {
		if a == "--socket" && i+1 < len(args) {
			socket = parityCanonicalSocket(t, args[i+1])
		}
	}
	path := filepath.Join(home, "state/relay.sqlite3")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 && (python || !emptiedStores[home]) {
		return
	}
	defer inRuntimeScope(t, home)()
	if info.Size() == 0 {
		testsupport.Create(t, path, socket, "go")
		delete(emptiedStores, home)
		return
	}
	owner := "go"
	if python {
		owner = "python"
	}
	testsupport.HandOver(t, path, owner)
}

// inRuntimeScope gives this process the scope registry root environment(home) gives both
// runtimes, so a scope key computed here is the one they compute; the returned func restores it.
func inRuntimeScope(t *testing.T, home string) func() {
	t.Helper()
	old, had := os.LookupEnv(ScopeEnv)
	if err := os.Setenv(ScopeEnv, home+"/scopes"); err != nil {
		t.Fatal(err)
	}
	return func() {
		var err error
		if had {
			err = os.Setenv(ScopeEnv, old)
		} else {
			err = os.Unsetenv(ScopeEnv)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// parityCanonicalSocket is the spelling both runtimes store: absolute, symlinks resolved as far
// as the path exists.
func parityCanonicalSocket(t *testing.T, socket string) string {
	t.Helper()
	absolute, err := filepath.Abs(socket)
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(absolute); err == nil {
		return real
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return absolute
	}
	return filepath.Join(parent, filepath.Base(absolute))
}
func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

var stampPattern = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|\+00:00)`)
var volatilePattern = regexp.MustCompile(`("(?:pid|workerPid|startTicks|workerStartTicks|bootId|token|launchId|storeId|store_id|store_created_at)": )(?:(?:"(?:[^"\\]|\\.)*")|-?[0-9]+)`)

func normalize(raw string) string {
	raw = stampPattern.ReplaceAllString(raw, "TIME")
	return volatilePattern.ReplaceAllString(raw, `${1}"VOLATILE"`)
}
func compare(t *testing.T, want, got capture) {
	t.Helper()
	if want.Code != got.Code || normalize(want.Out) != normalize(got.Out) || want.Err != got.Err {
		t.Fatalf("Python (%d)\n%s\nstderr %s\nGo (%d)\n%s\nstderr %s", want.Code, want.Out, want.Err, got.Code, got.Out, got.Err)
	}
}
func resetRuntime(t *testing.T, home string) {
	t.Helper()
	// Reinitialize SQLite through the SAME inode. Device/inode values are not
	// permitted normalizers and the worker receipt must compare byte for byte.
	entries, err := os.ReadDir(filepath.Join(home, "state"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	delete(emptiedStores, home)
	if _, e := os.Stat(filepath.Join(home, "state", "takeover.json")); e == nil {
		if _, e = ownership.ReadRecord(filepath.Join(home, "state", "relay.sqlite3")); e != nil {
			t.Fatal(e)
		}
		emptiedStores[home] = true
	} else if !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	for _, entry := range entries {
		path := filepath.Join(home, "state", entry.Name())
		if entry.Name() == "relay.sqlite3" {
			err = os.Truncate(path, 0)
		} else {
			err = os.RemoveAll(path)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scopes", "xdg-state"} {
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// processRecords are the files whose python_compatibility_build names the runtime that wrote
// them (with scopes/*.json), the one value testsupport.RuntimeIdentityText neutralizes once it
// is that runtime's own.
var processRecords = map[string]bool{"daemon.json": true, "worker-policy.json": true}

// writtenBy is the runtime an invoke(..., python, ...) ran.
func writtenBy(python bool) testsupport.Runtime {
	if python {
		return testsupport.Python
	}
	return testsupport.Go
}

// files reads the state files writer, the runtime that ran against home, left behind.
func files(t *testing.T, home string, writer testsupport.Runtime) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"daemon.json", "daemon.log", "launch-policy.json", "service.json", "worker-policy.json"} {
		raw, err := os.ReadFile(filepath.Join(home, "state", name))
		if err == nil {
			text := string(raw)
			if processRecords[name] {
				text = testsupport.RuntimeIdentityText(t, writer, text)
			}
			out[name] = normalize(text)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(home, "scopes", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out["scopes/"+filepath.Base(path)] = normalize(testsupport.RuntimeIdentityText(t, writer, string(raw)))
	}
	return out
}
func Test29ConsoleParity(t *testing.T) {
	cases := [][]string{{"service", "status"}, {"service", "enable", "--actor", "tester"}, {"service", "disable"}, {"service", "stop"}, {"service", "declare", "--forget-execution-policy"}, {"service", "start"}, {"service", "restart"}, {"service", "run"}, {"daemon"}, {"--socket", "/absent", "daemon", "--deadline", "nan"}, {"--socket", "/absent", "daemon", "--deadline", "0", "--deadline-monotonic", "0"}, {"--socket", "/absent", "daemon", "--deadline-monotonic", "0"}, {"--socket", "/absent", "daemon", "--max-ticks", "0", "--allow-isolated-scope"}, {"--socket", "/absent", "daemon", "--max-ticks", "1", "--allow-isolated-scope"}}
	cases = append(cases,
		[]string{"--socket", "/absent", "daemon", "--max-ticks", "0", "--allow-isolated-scope", "--supervised-token", "test-run"},
		[]string{"--socket", "/absent", "daemon", "--max-ticks", "0", "--allow-isolated-scope", "--supervised-token", "test-run", "--supervised-lock-fd", "3", "--supervised-scope-fd", "4"},
		[]string{"--socket", "/absent", "daemon", "--max-ticks", "0"},
		[]string{"--socket", "/absent", "service", "start", "--allow-isolated-scope"},
		[]string{"--socket", "/absent", "service", "restart", "--allow-isolated-scope"},
		[]string{"--socket", "/absent", "service", "run", "--allow-isolated-scope"})
	for i, args := range cases {
		t.Run(fmt.Sprintf("%02d_%s", i, strings.Join(args, "_")), func(t *testing.T) {
			home := t.TempDir()
			want := invoke(t, home, true, args...)
			wf := files(t, home, testsupport.Python)
			resetRuntime(t, home)
			got := invoke(t, home, false, args...)
			gf := files(t, home, testsupport.Go)
			compare(t, want, got)
			wb, _ := json.Marshal(wf)
			gb, _ := json.Marshal(gf)
			if !bytes.Equal(wb, gb) {
				t.Fatalf("persisted files\nPython %s\nGo %s", wb, gb)
			}
		})
	}
}
func Test29CLIShape(t *testing.T) {
	commands := [][]string{{"daemon"}, {"service"}, {"service", "status"}, {"service", "enable"}, {"service", "disable"}, {"service", "stop"}, {"service", "declare"}, {"service", "start"}, {"service", "restart"}, {"service", "run"}}
	for _, command := range commands {
		for _, suffix := range [][]string{{"--help"}, {"--unknown"}, {"--actor"}} {
			args := append(append([]string{}, command...), suffix...)
			t.Run(strings.Join(args, "_"), func(t *testing.T) {
				home := t.TempDir()
				compare(t, invoke(t, home, true, args...), invoke(t, home, false, args...))
			})
		}
	}
}
