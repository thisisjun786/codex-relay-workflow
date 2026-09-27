package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fixture is a real Python TheRoundtrip store, including the sent attempt,
// delivery token and verified readback. This guards both formerly crashing CLI
// results through the built executable, not through an in-process emitter.
// Sending/reading over a socket still depends on the todo-28 host adapter.
func Test24BuiltBinaryRoundtripStoreBytes(t *testing.T) {
	root, py := pythonSupervisorCapture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
	state := filepath.Join(root, "tree", "state")
	id := py.Tables["supervisor_messages"][0]["message_id"].(string)
	event := py.Tables["supervisor_messages"][0]["event_id"].(string)
	binary := supervisorBinary(t)
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"supervisor-stage", "--event", event},
		{"supervisor-show", "--message", id},
		{"supervisor-select", "--event", event},
		{"supervisor-standing", "--project", "PRJ-1"},
		{"supervisor-stage", "--project", "PRJ-1"},
	} {
		t.Run(args[0], func(t *testing.T) {
			goCmd := exec.Command(binary, append([]string{"relay", "--state", state}, args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"), "XDG_STATE_HOME="+filepath.Join(root, "home", "state"), "CODEX_HOME="+filepath.Join(root, "home", "codex"))
			got, goErr := goCmd.Output()
			goCode := commandExit24(t, goErr)
			pyCmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), append([]string{"-m", "codex_session_relay.cli", "--state", state}, args...)...)
			pyCmd.Env = goCmd.Env
			want, pyErr := pyCmd.Output()
			pyCode := commandExit24(t, pyErr)
			if goCode != pyCode || !bytes.Equal(got, want) {
				t.Errorf("CLI byte diff\nGo exit=%d\n%s\nPython exit=%d\n%s", goCode, got, pyCode, want)
			}
		})
	}
}

// A fresh packet is staged through both installed shapes. The alias must emit the exact bytes
// live Python emits for the same program and instant. The multi-call shape must embed a command
// that a POSIX shell can execute and that reads the same event record as a direct invocation.
func Test24BuiltBinaryEmbeddedProgramParityAndExecution(t *testing.T) {
	root, _ := pythonSupervisorCapture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
	snapshot, err := os.ReadFile(filepath.Join(root, "event.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "tree", "state")
	path := filepath.Join(state, "relay.sqlite3")
	restore := func() {
		t.Helper()
		if err := os.WriteFile(path, snapshot, 0600); err != nil {
			t.Fatal(err)
		}
	}
	restore()
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	var event string
	if err = s.DB.QueryRow("SELECT event_id FROM events LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	built := supervisorBinary(t)
	alias := filepath.Join(filepath.Dir(built), "codex-session-relay")
	if err = os.Symlink(built, alias); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+filepath.Join(root, "home"), "XDG_STATE_HOME="+filepath.Join(root, "home", "state"), "CODEX_HOME="+filepath.Join(root, "home", "codex"))
	args := []string{"supervisor-stage", "--event", event}
	cmd := exec.Command(alias, append([]string{"--state", state}, args...)...)
	cmd.Env = env
	got, goErr := cmd.Output()
	goCode := commandExit24(t, goErr)
	var staged struct {
		Message struct {
			At string `json:"staged_at"`
		} `json:"message"`
	}
	if err = json.Unmarshal(got, &staged); err != nil {
		t.Fatalf("alias stage exit=%d JSON=%s: %v", goCode, got, err)
	}

	restore()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `import datetime,sys
from codex_session_relay import cli,clock,supervisorchannel
at=sys.argv[1]
clock.SystemClock.iso=lambda self: at
clock.SystemClock.now=lambda self: datetime.datetime.fromisoformat(at).timestamp()
supervisorchannel.relay_program=lambda: (sys.argv[2],)
raise SystemExit(cli.main(sys.argv[3:]))`
	pyCmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, staged.Message.At, alias, "--state", state, "supervisor-stage", "--event", event)
	pyCmd.Env = env
	want, pyErr := pyCmd.Output()
	pyCode := commandExit24(t, pyErr)
	if goCode != pyCode || !bytes.Equal(got, want) {
		t.Fatalf("alias stage byte diff\nGo exit=%d\n%s\nPython exit=%d\n%s", goCode, got, pyCode, want)
	}

	restore()
	crwCmd := exec.Command(built, "relay", "--state", state, "supervisor-stage", "--event", event)
	crwCmd.Env = env
	crwOut, crwErr := crwCmd.Output()
	if code := commandExit24(t, crwErr); code != 0 {
		t.Fatalf("crw relay stage exit=%d: %s", code, crwOut)
	}
	var multi struct {
		Message struct {
			Packet string `json:"packet"`
		} `json:"message"`
	}
	if err = json.Unmarshal(crwOut, &multi); err != nil {
		t.Fatalf("crw relay stage JSON=%s: %v", crwOut, err)
	}
	var packet struct {
		Evidence []string `json:"evidence"`
	}
	if err = json.Unmarshal([]byte(multi.Message.Packet), &packet); err != nil || len(packet.Evidence) != 1 {
		t.Fatalf("packet evidence in %s: %v", multi.Message.Packet, err)
	}
	shell := exec.Command("sh", "-c", packet.Evidence[0])
	shell.Env = env
	embedded, shellErr := shell.Output()
	if shellErr != nil {
		t.Fatalf("embedded command %q: %v", packet.Evidence[0], shellErr)
	}
	direct := exec.Command(built, "relay", "--state", state, "show", "--event", event)
	direct.Env = env
	directOut, directErr := direct.Output()
	if directErr != nil {
		t.Fatalf("direct show: %v", directErr)
	}
	if !bytes.Equal(embedded, directOut) {
		t.Fatalf("embedded command read a different record\ncommand: %s\nembedded: %s\ndirect: %s", packet.Evidence[0], embedded, directOut)
	}
}
