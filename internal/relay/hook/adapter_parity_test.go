package hook

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The native hook and the Python adapter through their process entry points
// (adapter_compare.py): stdout, exit, the journal rows and the claim and outcome files agree
// scenario by scenario. Python's side (adapter_compare.py python) is recorded (pyoracle); this
// test runs the native side as the script ran it.
func Test33AdapterBinaryPython(t *testing.T) {
	tree := t.TempDir()
	raw := pyoracle.Answer(t, "python", func() ([]byte, error) {
		python := t.TempDir()
		return pythonScript(t, nil, nil, "testdata/adapter_compare.py", "-", python, testRoot, "python")
	})
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	recorded, ok := evidence.List(decoded)
	if !ok || len(recorded) != 8 {
		t.Fatalf("recorded %d QA cases: %s", len(recorded), raw)
	}
	release := Object{{Key: "decision", Value: "release"}, {Key: "state", Value: "unmanaged"}, {Key: "hook_output", Value: Object{}}}
	hold := Object{{Key: "decision", Value: "block"}, {Key: "state", Value: "receipt_missing"}, {Key: "observation", Value: "receipt_missing"},
		{Key: "assignmentId", Value: strings.Repeat("a", 64)}, {Key: "counters", Value: Object{{Key: "holdsThisTurn", Value: int64(0)}}},
		{Key: "recordedAs", Value: "hook/s/t/0"}, {Key: "hook_output", Value: Object{{Key: "decision", Value: "block"}, {Key: "reason", Value: "verify the child"}, {Key: "continue", Value: true}}}}
	notUTF8 := filepath.Join(tree, "not-utf8-\xff")
	for i, c := range []struct {
		name     string
		response Object
		base     string
	}{{"hold", hold, tree}, {"release", release, tree}, {"duplicate", release, tree}, {"malformed", release, tree},
		{"missing", release, tree}, {"unreadable", release, tree}, {"timeout", release, tree}, {"hold", hold, notUTF8}} {
		scenario := c.name
		if c.base != tree {
			scenario += " under a directory that is not UTF-8"
		}
		want := object(recorded[i])
		if get(want, "scenario") != scenario {
			t.Fatalf("recorded case %d is %v, not %s", i, get(want, "scenario"), scenario)
		}
		got := nativeAdapterRun(t, c.name, c.response, c.base)
		if c.name == "timeout" {
			// Decision 24: native cancellation has no process group. Diagnostic prose is
			// intentionally different; all machine-consumed outcome fields agree.
			rows, _ := evidence.List(get(got, "rows"))
			rows[0] = withoutKeys(object(rows[0]), "detail")
		}
		if g, w := evidence.Dumps(got, false, true, true), evidence.Dumps(get(want, "python"), false, true, true); g != w {
			t.Fatalf("%s:\n go     %s\n python %s", scenario, g, w)
		}
		t.Logf("%s: stdout, exit, journal and claim/outcome files equal", scenario)
	}
}

var attemptRowName = regexp.MustCompile(`^[0-9]{8}/[0-9a-f]{32}\.json$`)

// nativeAdapterRun is adapter_compare.py's run() for the native hook: the settings, transcript
// and payload it writes under base/name/go, a control.sock owner answering response, and what
// the hook invocations answered and left.
func nativeAdapterRun(t *testing.T, name string, response Object, base string) Object {
	t.Helper()
	home := filepath.Join(base, name, "go")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	spelled := store.FSDecode(home) // the str Python holds for home
	within := func(parts ...string) string { return spelled + "/" + strings.Join(parts, "/") }
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"HOME", "CODEX_HOME", "XDG_STATE_HOME", "CODEX_SESSION_RELAY_STATE"}, key)
	})
	env = append(env, "HOME="+home, "CODEX_HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg"), "CODEX_SESSION_RELAY_STATE="+filepath.Join(home, "state"))
	writeTest(t, filepath.Join(home, "relay"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(filepath.Join(home, "relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	var budget any = int64(5)
	if name == "timeout" {
		budget = 0.4
	}
	settings := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: within("relay")}, {Key: "markerRoot", Value: within("marker")},
		{Key: "dbPath", Value: within("state/relay.sqlite3")}, {Key: "mode", Value: "observe"}, {Key: "timeoutSeconds", Value: budget},
		{Key: "journalRoot", Value: within("journal")}, {Key: "journalPolicy", Value: "every_invocation"}}
	switch name {
	case "missing":
	case "unreadable":
		writeTest(t, filepath.Join(home, ConfigName), []byte("{"))
	default:
		writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(settings, false, false, true)))
	}
	fixtureRaw, err := os.ReadFile("testdata/stop_event_r1.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := decodeObject(fixtureRaw)
	if err != nil {
		t.Fatal(err)
	}
	stops, _ := evidence.List(get(fixture, "stops"))
	stop := object(stops[0])
	lines, _ := evidence.List(get(fixture, "transcriptLines"))
	var transcript strings.Builder
	for _, line := range lines[:int(get(stop, "linesAtStop").(int64))] {
		transcript.WriteString(strings.ReplaceAll(text(line), "<CODEX_HOME>", spelled) + "\n")
	}
	writeTest(t, filepath.Join(home, "transcript.jsonl"), []byte(transcript.String()))
	payload := append(Object{}, object(get(stop, "payload"))...)
	payload = set(payload, "transcript_path", within("transcript.jsonl"))
	payload = set(payload, "cwd", spelled)
	input := evidence.Dumps(payload, false, false, true)
	if name == "malformed" {
		input = "not json"
	}
	invocations := 1
	if name == "duplicate" {
		invocations = 2
	}
	if err := os.Mkdir(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "state", "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	if !slices.Contains([]string{"missing", "unreadable", "malformed"}, name) {
		go func() { served <- serveAdapterOwner(listener, invocations, name == "timeout", payload, response) }()
	} else {
		served <- nil
	}
	outputs := []any{}
	for range invocations {
		command := exec.Command(binary(t), "hook")
		command.Env = env
		command.Stdin = strings.NewReader(input)
		o := runOutcome(t, command)
		outputs = append(outputs, Object{{Key: "exit", Value: int64(o.Code)}, {Key: "stdout", Value: o.Stdout}, {Key: "stderr", Value: o.Stderr}})
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("control peer did not finish")
	}
	var normalize func(value any, path string) any
	normalize = func(value any, path string) any {
		switch v := value.(type) {
		case string:
			switch path {
			case "/configuration", "/eventIdentity/transcriptPath", "/claimedBy/journalRoot", "/claimedBy/hostLedger":
				return strings.ReplaceAll(v, spelled, "<HOME>")
			case "/attemptRow", "/claimedBy/attemptRow":
				if !attemptRowName.MatchString(v) {
					t.Fatalf("%s %q", path, v)
				}
				return "<ROW>"
			}
			return v
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				out[i] = normalize(item, fmt.Sprintf("%s/%d", path, i))
			}
			return out
		case Object:
			out := Object{}
			for _, field := range v {
				if !slices.Contains([]string{"/claimedBy/pid", "/at", "/claimedAt", "/elapsedMs", "/guardElapsedMs", "/identityScanMs"}, path+"/"+field.Key) {
					out = append(out, Field{Key: field.Key, Value: normalize(field.Value, path+"/"+field.Key)})
				}
			}
			return out
		}
		return value
	}
	read := func(path string) Object {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value, err := decodeObject(raw)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return normalize(value, "").(Object)
	}
	paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	rows := []any{}
	for _, path := range paths {
		rows = append(rows, read(path))
	}
	slices.SortStableFunc(rows, func(a, b any) int {
		return strings.Compare(pythonStr(get(a.(Object), "acceptance")), pythonStr(get(b.(Object), "acceptance")))
	})
	files := Object{}
	for _, root := range []string{filepath.Join(home, "journal", "accepted"), filepath.Join(home, "crw-completion-hook", "stop-events")} {
		found, err := filepath.Glob(filepath.Join(root, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(found)
		for _, path := range found {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s: %v %v", path, info.Mode(), err)
			}
			rel, _ := filepath.Rel(home, path)
			files = append(files, Field{Key: rel, Value: read(path)})
		}
	}
	return Object{{Key: "outputs", Value: outputs}, {Key: "rows", Value: rows}, {Key: "files", Value: files}}
}

// pythonStr is str() of a JSON value's string or None.
func pythonStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "None"
}

// serveAdapterOwner is adapter_compare.py's control peer: for each invocation it accepts one
// connection and, when a guard request arrives, checks it carries the payload and answers
// response, or, for the timeout case, reads until the hook disconnects.
func serveAdapterOwner(listener net.Listener, invocations int, timeout bool, payload, response Object) error {
	for range invocations {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		err = func() error {
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return err
			}
			reader := bufio.NewReader(conn)
			line, err := reader.ReadBytes('\n')
			if len(line) == 0 && (err == nil || errors.Is(err, io.EOF)) {
				return nil // duplicate exits after the dial, before a guard request
			}
			if err != nil {
				return err
			}
			request, err := decodeObject(line)
			if err != nil {
				return err
			}
			if get(request, "method") != "guard-evaluate" {
				return fmt.Errorf("method %v", get(request, "method"))
			}
			stop := get(object(get(request, "params")), "stopInput")
			if evidence.Dumps(stop, false, true, true) != evidence.Dumps(payload, false, true, true) {
				return fmt.Errorf("stopInput %s", evidence.Dumps(stop, false, true, true))
			}
			if timeout {
				rest, err := io.ReadAll(reader)
				if err != nil || len(rest) != 0 {
					return fmt.Errorf("after the request: %q %v", rest, err)
				}
				return nil
			}
			_, err = io.WriteString(conn, evidence.Dumps(response, false, false, true)+"\n")
			return err
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
