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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The fixture is a real Python TheRoundtrip store, including the sent attempt,
// delivery token and verified readback. This guards both formerly crashing CLI
// results through the built executable, not through an in-process emitter.
// Sending/reading over a socket still depends on the todo-28 host adapter.
func Test24BuiltBinaryRoundtripStoreBytes(t *testing.T) {
	root := supervisorFixture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
	state := filepath.Join(root, "tree", "state")
	var id, event string
	testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "go")
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow("SELECT message_id, event_id FROM supervisor_messages ORDER BY rowid LIMIT 1").Scan(&id, &event); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	binary := testsupport.CRW(t)
	for _, args := range [][]string{
		{"supervisor-stage", "--event", event},
		{"supervisor-show", "--message", id},
		{"supervisor-select", "--event", event},
		{"supervisor-standing", "--project", "PRJ-1"},
		{"supervisor-stage", "--project", "PRJ-1"},
	} {
		t.Run(args[0], func(t *testing.T) {
			testsupport.HandOver(t, filepath.Join(state, "relay.sqlite3"), "go")
			goCmd := exec.Command(binary, append([]string{"relay", "--state", state}, args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"), "XDG_STATE_HOME="+filepath.Join(root, "home", "state"), "CODEX_HOME="+filepath.Join(root, "home", "codex"))
			got, goErr := goCmd.Output()
			goCode := commandExit24(t, goErr)
			golden.CheckJSON(t, "answer", map[string]any{"code": goCode, "stdout": string(got)}, treeGolden(t, root)...)
		})
	}
}

// A fresh packet is staged through both installed shapes. The alias must emit the golden's
// bytes for its program and instant. The multi-call shape must embed a command
// that a POSIX shell can execute and that reads the same event record as a direct invocation.
func Test24BuiltBinaryEmbeddedProgramParityAndExecution(t *testing.T) {
	root := supervisorFixture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
	snapshot, err := os.ReadFile(filepath.Join(root, "event.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "tree", "state")
	path := filepath.Join(state, "relay.sqlite3")
	restore := func(owner string) {
		t.Helper()
		restoreSnapshot(t, path, snapshot, owner)
	}
	restore("go")
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
	built := testsupport.CRWAt(t, filepath.Join(t.TempDir(), "crw"))
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
	// The alias stages with the program it was run as; staged_at is the instant it staged.
	golden.CheckJSON(t, "supervisor-stage", map[string]any{"code": goCode, "stdout": string(got)}, append([]golden.Option{golden.Substitute(alias, "<alias>"), golden.Substitute(staged.Message.At, "<staged_at>")}, treeGolden(t, root)...)...)

	restore("go")
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
