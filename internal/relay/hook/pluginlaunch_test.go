package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// declaredPluginHookArgs is what the shipped Stop declaration passes to the runtime after
// `crw`: the words between the quoted pointer path and `; exit 0`.
func declaredPluginHookArgs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testRoot, "plugins/crw/wiring/hooks/stop-recording-completion.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	command := document.Hooks["Stop"][0].Hooks[0].Command
	const pointer = `"$HOME/.local/share/crw-runtime/current/bin/crw" `
	words, ok := strings.CutPrefix(command, pointer)
	if !ok {
		t.Fatalf("declared command %q does not start with the pointer", command)
	}
	words, ok = strings.CutSuffix(words, "; exit 0")
	if !ok {
		t.Fatalf("declared command %q does not end with ; exit 0", command)
	}
	return strings.Fields(words)
}

// countingControl answers every guard request with a release and counts them. stop waits for
// the accept loop to end, so the count is final when it returns.
func countingControl(t *testing.T, home string) (stop func() int) {
	t.Helper()
	path := filepath.Join(home, "state", "control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
			if request, err := readFrame(conn); err == nil && get(request, "method") == "guard-evaluate" {
				mu.Lock()
				requests++
				mu.Unlock()
				_, _ = io.WriteString(conn, `{"decision":"release","state":"unmanaged","hook_output":{}}`+"\n")
			}
			conn.Close()
		}
	}()
	stopped := false
	stop = func() int {
		if !stopped {
			stopped = true
			_ = listener.Close()
			<-done
		}
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
	t.Cleanup(func() { stop() })
	return stop
}

func runHook(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	return runHookWith(t, home, `{"session_id":"s","turn_id":"t"}`, args...)
}

func runHookWith(t *testing.T, home, payload string, args ...string) (int, string, string) {
	t.Helper()
	cmd := hookCommand(t, home, payload)
	cmd.Args = append([]string{cmd.Args[0]}, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exit, ok := err.(interface{ ExitCode() int })
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func withOwner(t *testing.T, home string, owner any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	value, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	config := object(value)
	if owner != nil {
		config = set(config, "owner", owner)
	}
	if owner == "plugin" {
		config = set(config, "adapterInterpreter", filepath.Join(home, "never-run"))
		config = set(config, "adapterEntryPoint", filepath.Join(home, "never-run"))
	}
	writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(config, false, false, true)))
}

// A host carrying both registrations (the user's hook-file entry, `crw hook`, and the plugin's
// declared command) receives each Stop twice. The settings name the user as owner, so only the
// user's registration may evaluate it: the guard is asked once and one journal row is written.
//
// Both payload shapes are covered. Without an established event identity nothing arbitrates
// between the two invocations; with one (the r1 fixture's first Stop and its transcript) the
// event claim already keeps the guard to one request, and the second invocation still runs the
// adapter and writes a duplicate_invocation row.
func TestPluginLaunch_double_registration_evaluates_the_Stop_once(t *testing.T) {
	plugin := declaredPluginHookArgs(t)
	for name, payload := range map[string]func(home string) string{
		"unestablished identity": func(string) string { return `{"session_id":"s","turn_id":"t"}` },
		"established identity":   establishedStop(t),
	} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			stop := countingControl(t, home)
			for _, args := range [][]string{{"hook"}, plugin} {
				if code, stdout, stderr := runHookWith(t, home, payload(home), args...); code != 0 || stdout != "" || stderr != "" {
					t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
				}
			}
			rows := rowsAt(t, home)
			if requests := stop(); requests != 1 || len(rows) != 1 || rows[0]["configuration"] != filepath.Join(home, ConfigName) {
				t.Fatalf("plugin command %v: guard asked %d times, %d journal rows %v; want 1 and 1", plugin, requests, len(rows), outcomes(rows))
			}
		})
	}
}

// The once above holds because the settings name the user. With settings the plugin owns, the
// declared command evaluates, and so does a leftover hook-file `crw hook` entry: that path reads
// no owner, and cannot, because the legacy crw_stop_hook.py call reaches it without the flag under
// the same plugin-owned settings (crw_stop_hook.py and completion_hook.py behave the same). Only
// the installers keep this state from arising (`crw install hook --owner plugin` and
// runtime_install.py refuse the second registration), so it needs a hand edit; this pins what
// happens then: an unestablished Stop asks the guard twice, and an established one is kept to one
// request by the event claim and writes a duplicate_invocation row (docs/port/decisions.md 26).
func TestPluginLaunch_plugin_owned_settings_beside_a_hook_file_entry_evaluate_the_Stop_twice(t *testing.T) {
	plugin := declaredPluginHookArgs(t)
	for name, c := range map[string]struct {
		payload  func(home string) string
		requests int
		rows     []any
	}{
		"unestablished identity": {func(string) string { return `{"session_id":"s","turn_id":"t"}` }, 2, []any{"guard_answered", "guard_answered"}},
		"established identity":   {establishedStop(t), 1, []any{"duplicate_invocation", "guard_answered"}},
	} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			withOwner(t, home, "plugin")
			stop := countingControl(t, home)
			for _, args := range [][]string{{"hook"}, plugin} {
				if code, stdout, stderr := runHookWith(t, home, c.payload(home), args...); code != 0 || stdout != "" || stderr != "" {
					t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
				}
			}
			// Sorted: the journal's row order is not the order of the two invocations.
			seen := outcomes(rowsAt(t, home))
			slices.SortFunc(seen, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
			if requests := stop(); requests != c.requests || !reflect.DeepEqual(seen, c.rows) {
				t.Fatalf("guard asked %d times, journal rows %v; want %d and %v", requests, seen, c.requests, c.rows)
			}
		})
	}
}

func outcomes(rows []map[string]any) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, row["adapterOutcome"])
	}
	return out
}

// establishedStop writes the r1 fixture's first transcript under home and returns its payload.
func establishedStop(t *testing.T) func(home string) string {
	raw, err := os.ReadFile(filepath.Join(testRoot, "packages/codex-session-relay/tests/fixtures/stop_event_r1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		TranscriptLines []string `json:"transcriptLines"`
		Stops           []struct {
			Payload     map[string]any `json:"payload"`
			LinesAtStop int            `json:"linesAtStop"`
		} `json:"stops"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	first := fixture.Stops[0]
	return func(home string) string {
		path := filepath.Join(home, "transcript.jsonl")
		writeTest(t, path, []byte(strings.Join(fixture.TranscriptLines[:first.LinesAtStop], "\n")+"\n"))
		payload := map[string]any{}
		for k, v := range first.Payload {
			payload[k] = v
		}
		payload["transcript_path"] = path
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

// Under --plugin-launch, settings the plugin does not own are stood down from before anything
// happens: exit 0, empty stdout, no guard request, no journal row, no event claim. Settings the
// plugin owns under a configVersion other than 1, absent included, are released the same way
// (crw_stop_hook.py stood down for any version but absent or 1, and its adapter then refused an
// absent one; the Go validator accepts only 1).
func TestPluginLaunch_stands_down_unless_the_plugin_owns_the_settings(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, home string){
		"owner absent":            func(t *testing.T, home string) { withOwner(t, home, nil) },
		"owner user":              func(t *testing.T, home string) { withOwner(t, home, "user") },
		"plugin, configVersion 2": func(t *testing.T, home string) { withOwner(t, home, "plugin"); withVersion(t, home, int64(2), true) },
		"plugin, configVersion absent": func(t *testing.T, home string) {
			withOwner(t, home, "plugin")
			withVersion(t, home, nil, false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			change(t, home)
			stop := countingControl(t, home)
			code, stdout, stderr := runHook(t, home, "hook", "--plugin-launch")
			claims, _ := filepath.Glob(filepath.Join(home, "crw-completion-hook", "stop-events", "*"))
			if requests := stop(); code != 0 || stdout != "" || stderr != "" || requests != 0 || len(rowsAt(t, home)) != 0 || len(claims) != 0 {
				t.Errorf("exit %d stdout %q stderr %q requests %d rows %d claims %d", code, stdout, stderr, requests, len(rowsAt(t, home)), len(claims))
			}
		})
	}
}

// withVersion sets configVersion in the settings under home, or removes it when present is false.
func withVersion(t *testing.T, home string, version any, present bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	value, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	config := Object{}
	for _, field := range object(value) {
		if field.Key != "configVersion" {
			config = append(config, field)
		}
	}
	if present {
		config = append(Object{{Key: "configVersion", Value: version}}, config...)
	}
	writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(config, false, false, true)))
}

// Settings the plugin owns are evaluated under --plugin-launch exactly as without the flag.
func TestPluginLaunch_evaluates_settings_the_plugin_owns(t *testing.T) {
	home := hookHome(t, 5)
	withOwner(t, home, "plugin")
	stop := countingControl(t, home)
	code, stdout, stderr := runHook(t, home, "hook", "--plugin-launch")
	rows := rowsAt(t, home)
	if requests := stop(); code != 0 || stdout != "" || stderr != "" || requests != 1 || len(rows) != 1 {
		t.Fatalf("exit %d stdout %q stderr %q requests %d rows %d", code, stdout, stderr, requests, len(rows))
	}
	// The flag is not a settings path: the row names the settings under CODEX_HOME.
	if rows[0]["configuration"] != filepath.Join(home, ConfigName) {
		t.Fatalf("configuration %v", rows[0]["configuration"])
	}
}

// pythonPluginSettingsPath is crw_stop_hook.py settings_path() under env: the path the Python
// plugin launcher reads. The package shipped it until todo 43; the pre-native testdata keeps it,
// byte for byte the <CODEX_HOME>/crw-stop-hook.py copy a host may still hold.
func pythonPluginSettingsPath(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command(python(t), "-c", "import runpy,sys;sys.stdout.write(str(runpy.run_path(sys.argv[1])['settings_path']()))",
		filepath.Join(testRoot, "internal/pluginwiring/testdata/pre-native-wiring/crw_stop_hook.py"))
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A plugin declaration carries no settings argument, so the Python plugin launcher reads only
// <CODEX_HOME>/crw-completion-hook.json and never CRW_COMPLETION_HOOK_CONFIG. Under
// --plugin-launch the Go hook reads the same file whatever the variable or an argument names;
// without the flag the todo-33 precedence (argument, then the variable, then CODEX_HOME) holds.
func TestPluginLaunch_reads_only_the_codex_home_settings(t *testing.T) {
	home := hookHome(t, 5)
	withOwner(t, home, "plugin")
	elsewhere := filepath.Join(home, "elsewhere.json")
	raw, err := os.ReadFile(filepath.Join(home, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, elsewhere, raw)
	argument := filepath.Join(home, "argument.json")
	writeTest(t, argument, raw)
	env := append(hookEnv(home), "CRW_COMPLETION_HOOK_CONFIG="+elsewhere)
	pythonReads := pythonPluginSettingsPath(t, env)
	if pythonReads != filepath.Join(home, ConfigName) {
		t.Fatalf("the Python plugin launcher reads %q", pythonReads)
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"plugin launch, variable set", []string{"hook", "--plugin-launch"}, pythonReads},
		{"plugin launch, variable and argument", []string{"hook", "--plugin-launch", argument}, pythonReads},
		{"no flag, variable set", []string{"hook"}, elsewhere},
		{"no flag, argument and variable", []string{"hook", argument}, argument},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(home, "journal"))
			stop := countingControl(t, home)
			cmd := hookCommand(t, home, `{"session_id":"s","turn_id":"t"}`)
			cmd.Args = append([]string{cmd.Args[0]}, c.args...)
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("%v stdout %q stderr %q", err, stdout.String(), stderr.String())
			}
			rows := rowsAt(t, home)
			if requests := stop(); requests != 1 || len(rows) != 1 || rows[0]["configuration"] != c.want {
				t.Fatalf("requests %d rows %d configuration %v; want %s", requests, len(rows), configurations(rows), c.want)
			}
		})
	}
}

func configurations(rows []map[string]any) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, row["configuration"])
	}
	return out
}

// Hook status reads the shipped native command, flag included, as a native registration of this
// adapter that names no settings file: `--plugin-launch` is not a settings path.
func TestPluginLaunch_status_reads_the_declared_command_as_naming_no_settings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	raw, err := os.ReadFile(filepath.Join(testRoot, "plugins/crw/wiring/hooks/stop-recording-completion.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(home, "hooks.json"), raw)
	_, ours, readable := readRegistrations(filepath.Join(home, "hooks.json"), "Stop")
	if !readable || len(ours) != 1 || !ours[0].Native || ours[0].Settings != "" || ours[0].Target != filepath.Join(home, ".local/share/crw-runtime/current/bin/crw") {
		t.Fatalf("readable %v registrations %+v", readable, ours)
	}
}
