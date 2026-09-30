package service

import (
	"bytes"
	"errors"

	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
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

// prepareParityOwnership puts the state directory into the ownership state a host running
// the runtime under test has at this step, and nothing more:
//   - an absent store stays absent: each runtime's own absent-store initializer creates it;
//   - a store an earlier invocation left behind that the other runtime owns is that store after
//     a completed takeover to the runtime under test.
//
// The scope key a helper stamps is the one the runtime computes in the environment it runs in.
func prepareParityOwnership(t *testing.T, home string, python bool, args []string) {
	t.Helper()
	path := filepath.Join(home, "state/relay.sqlite3")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		return
	}
	defer inRuntimeScope(t, home)()
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

// consoleAnswer is what one console run answered and the state files it left.
type consoleAnswer struct {
	Capture capture           `json:"capture"`
	Files   map[string]string `json:"files"`
}

// Test29ConsoleParity runs each console command over a fresh home and checks its answer and
// files against the golden, which began as the retained Python console's.
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
			got := invoke(t, home, false, args...)
			checkAnswer(t, home, "answer", consoleAnswer{normalizedCapture(got), files(t, home, testsupport.Go)})
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
				checkAnswer(t, home, "answer", normalizedCapture(invoke(t, home, false, args...)))
			})
		}
	}
}
