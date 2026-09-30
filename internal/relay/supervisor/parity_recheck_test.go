package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// recheckPython runs a Python script with root as its HOME and returns what it printed.
func recheckPython(root, script string, args ...string) ([]byte, error) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), append([]string{"-c", script}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Python: %v %s", err, out)
	}
	return out, nil
}

// recheckCopy is a copy Python owns of the fixture's store as it stands, taken by Python's
// sqlite3 backup.
func recheckCopy(t *testing.T, f *stageFixture) (string, error) {
	pyPath := filepath.Join(f.root, "python.sqlite3")
	if _, err := recheckPython(f.root, `import sqlite3,sys
s=sqlite3.connect(sys.argv[1]); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()`, f.s.Path, pyPath); err != nil {
		return "", err
	}
	ownCopied(t, pyPath, "python")
	return pyPath, nil
}

func Test24_AutoFaultJournalBytes(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	// Python defers the same attempt on a copy of the store as it stands before Go does.
	pyPath := ""
	if pyoracle.Live() {
		var err error
		if pyPath, err = recheckCopy(t, f); err != nil {
			t.Fatal(err)
		}
	}
	fault := "<host> & café"
	if err := f.c.deferAutoFault(f.ctx, id, 1700000000, errors.New(fault)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := f.s.DB.QueryRow("SELECT detail FROM journal WHERE kind='supervisor_attempt_faulted'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := pythonOutput(t, "defer_after_fault", func() ([]byte, error) {
		return recheckPython(f.root, `import sys
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
from codex_session_relay.supervisorchannel import SupervisorChannel
s=Store(sys.argv[1])
c=SupervisorChannel(s,None,None,FakeClock(1700000000))
c.defer_after_fault(sys.argv[2],1700000000.0,Exception(sys.argv[3]))
print(s.one("SELECT detail FROM journal WHERE kind='supervisor_attempt_faulted'")['detail'],end='')
s.close()`, pyPath, id, fault)
	}, pyoracle.Substitute(f.root, "<fixture>"))
	if !bytes.Equal([]byte(got), want) {
		t.Errorf("supervisor_attempt_faulted.detail byte diff\nGo: %s\nPython: %s", got, want)
	}
}

func Test24_ObligationHTMLBuiltBinaryBytes(t *testing.T) {
	root, _ := pythonSupervisorCapture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer", "event")
	state := filepath.Join(root, "tree", "state")
	path := filepath.Join(state, "relay.sqlite3")
	snapshot, err := os.ReadFile(filepath.Join(root, "event.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	restoreSnapshot(t, path, snapshot, "go")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	var event string
	if err := s.DB.QueryRow("SELECT event_id FROM events LIMIT 1").Scan(&event); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE events SET outcome='blocked_needs_input'; UPDATE work_reports SET cxc_status='BLOCKED',cxc_reason='<reason> & café',summary='<summary> & café'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary := testsupport.CRW(t)
	alias := filepath.Join(t.TempDir(), "codex-session-relay")
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(alias, "--state", state, "supervisor-stage", "--event", event)
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go: %v %s", err, got)
	}
	var answer struct {
		Message struct {
			At string `json:"staged_at"`
		} `json:"message"`
	}
	if err := json.Unmarshal(got, &answer); err != nil {
		t.Fatal(err)
	}
	want := pythonOutput(t, "supervisor-stage", func() ([]byte, error) {
		restoreSnapshot(t, path, before, "python")
		return recheckPython(root, `import sys
from codex_session_relay import cli,clock,supervisorchannel
clock.SystemClock.iso=lambda self: sys.argv[1]
supervisorchannel.relay_program=lambda: (sys.argv[2],)
raise SystemExit(cli.main(sys.argv[3:]))`, answer.Message.At, alias, "--state", state, "supervisor-stage", "--event", event)
	}, pyoracle.Substitute(alias, "<alias>"), pyoracle.Substitute(root, "<root>"), pyoracle.Substitute(answer.Message.At, "<staged_at>"))
	if !bytes.Equal(got, want) {
		t.Errorf("obligation/message CLI byte diff\nGo: %s\nPython: %s", got, want)
	}
}
