package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
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
// happens: exit 0, empty stdout, no guard request, no journal row, no event claim.
func TestPluginLaunch_stands_down_unless_the_plugin_owns_the_settings(t *testing.T) {
	for _, owner := range []any{nil, "user"} {
		home := hookHome(t, 5)
		withOwner(t, home, owner)
		stop := countingControl(t, home)
		code, stdout, stderr := runHook(t, home, "hook", "--plugin-launch")
		claims, _ := filepath.Glob(filepath.Join(home, "crw-completion-hook", "stop-events", "*"))
		if requests := stop(); code != 0 || stdout != "" || stderr != "" || requests != 0 || len(rowsAt(t, home)) != 0 || len(claims) != 0 {
			t.Errorf("owner %v: exit %d stdout %q stderr %q requests %d rows %d claims %d", owner, code, stdout, stderr, requests, len(rowsAt(t, home)), len(claims))
		}
	}
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
