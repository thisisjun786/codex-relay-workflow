package service

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	for key, value := range map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/xdg-state", "XDG_CONFIG_HOME": home + "/config", "XDG_DATA_HOME": home + "/data", "CODEX_HOME": home + "/codex", "CODEX_SESSION_RELAY_STATE": home + "/state", "CODEX_SESSION_RELAY_SCOPE_DIR": home + "/scopes", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY": ""} {
		env = environmentSet(env, key, value)
	}
	return env
}
func invoke(t *testing.T, home string, python bool, args ...string) capture {
	t.Helper()
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
func files(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"daemon.json", "daemon.log", "launch-policy.json", "service.json", "worker-policy.json"} {
		raw, err := os.ReadFile(filepath.Join(home, "state", name))
		if err == nil {
			out[name] = normalize(string(raw))
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
		out["scopes/"+filepath.Base(path)] = normalize(string(raw))
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
			wf := files(t, home)
			resetRuntime(t, home)
			got := invoke(t, home, false, args...)
			gf := files(t, home)
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
