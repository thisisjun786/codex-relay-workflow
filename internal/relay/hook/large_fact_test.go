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
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The deadline-bearing native hook classifies a valid large marker (a 5 MiB intent) as Python's
// guard did: the fixture is large_fact.py prepare's (late_verdict.py's, the intent padded, the
// store handed to Go), and the hook's answer and the guard's records are the goldens, which began
// as Python's guard verdict and records. The owned hook evaluates in-process, so the control
// socket sees no request.
func Test33LargeFactHookPython(t *testing.T) {
	home := hookHome(t, 5)
	built := binary(t) // built before the timeout starts, which is for the hook
	layFixture(t, "large-fact", home)
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
	goldenOutcome(t, "hook", got, golden.Substitute(home, "<HOME>"))
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
	golden.CheckJSON(t, "files", actual, golden.Substitute(home, "<HOME>"))
}
