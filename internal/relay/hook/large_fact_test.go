package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The deadline-bearing native hook classifies a valid large marker (a 5 MiB intent) as Python's
// guard does. large_fact.py prepare lays the fixture out (late_verdict.py's, the intent padded,
// the store handed to Go) and answers Python's guard verdict and records; both are recorded
// (pythonFixture). The owned hook evaluates in-process, so the control socket sees no request.
func Test33LargeFactHookPython(t *testing.T) {
	home := hookHome(t, 5)
	built := binary(t) // built before the timeout starts, which is for the hook
	out, _ := pythonFixture(t, "fixture", home, func() ([]byte, error) {
		return pythonScript(t, nil, nil, "testdata/large_fact.py", "-", home, "prepare")
	}, nil)
	var python struct {
		HookOutput string                    `json:"hook_output"`
		Files      map[string]map[string]any `json:"files"`
	}
	if err := json.Unmarshal(out, &python); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	listener, err := net.Listen("unix", filepath.Join(home, "state", "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			served <- err
			return
		}
		raw, err := io.ReadAll(conn)
		if err == nil && len(raw) != 0 {
			err = fmt.Errorf("owned hook unexpectedly used RPC: %q", raw)
		}
		served <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	payload, err := os.ReadFile(filepath.Join(home, "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, built, "hook")
	env := hookEnv(home)
	command.Env = append(env, "CRW_COMPLETION_HOOK_CONFIG=")
	command.Stdin = strings.NewReader(string(payload))
	got := runOutcome(t, command)
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("the hook never reached the control socket")
	}
	if got != (outcomeBytes{0, python.HookOutput, ""}) {
		t.Fatalf("hook %+v, Python answered %q", got, python.HookOutput)
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" || rows[0]["guardState"] != "receipt_missing" || rows[0]["held"] != true {
		t.Fatal(rows)
	}
	paths, _ := filepath.Glob(filepath.Join(home, "markers", "*", "*", "hook", "s", "t", "*.json"))
	actual := map[string]map[string]any{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err = json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		delete(record, "at") // independent real clocks
		actual[filepath.Base(path)] = record
	}
	for _, record := range python.Files {
		delete(record, "at")
	}
	if !reflect.DeepEqual(actual, python.Files) {
		t.Fatalf("go %v\npython %v", actual, python.Files)
	}
}
