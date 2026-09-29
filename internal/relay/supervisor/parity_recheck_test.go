package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func recheckPython(t *testing.T, root, script string, args ...string) []byte {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), append([]string{"-c", script}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python: %v %s", err, out)
	}
	return out
}

func Test24_ReportStorageBytes(t *testing.T) {
	for _, revision := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked", true: "revision"}[revision], func(t *testing.T) {
			f := fixture24(t)
			outcome := "blocked_needs_input"
			if revision {
				outcome = "revision_request"
			}
			if _, err := f.s.DB.Exec("UPDATE events SET outcome=?; DELETE FROM work_reports", outcome); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"repository": "owner/repo", "cxc_status": "BLOCKED", "cxc_reason": "<reason> & café", "summary": "<summary> & café", "next_action": "review", "evidence": []any{map[string]any{"check": "<check> & café", "exitCode": 0, "detail": "<detail> & café"}, "second"}, "unresolved": []any{map[string]any{"id": "<id> & café", "note": "<note> & café"}}, "restore": map[string]any{"skills": []any{"development"}, "mode": "<mode> & café", "scope": "<scope> & café"}}
			if revision {
				input["review"] = map[string]any{"kind": "FAIL", "findings": []any{map[string]any{"id": "criterion", "verdict": "needs_changes", "note": "<note> & café", "anchor": "<anchor> & café"}}}
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			pyPath := filepath.Join(f.root, "python.sqlite3")
			recheckPython(t, f.root, `import sqlite3,sys
s=sqlite3.connect(sys.argv[1]); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()`, f.s.Path, pyPath)
			ownCopied(t, pyPath, "python")
			stored, err := RecordWorkReport(f.ctx, f.s, &delivery.FakeClock{T: 1700000000}, f.event, input)
			if err != nil {
				t.Fatal(err)
			}
			want := recheckPython(t, f.root, `import json,sys
from codex_session_relay import report
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
s=Store(sys.argv[1])
stored=report.record(s,FakeClock(1700000000),event_id=sys.argv[2],**json.loads(sys.argv[3]))
r=dict(s.one('SELECT evidence,unresolved,review,restore FROM work_reports WHERE event_id=?',(sys.argv[2],)))
j=s.one("SELECT detail FROM journal WHERE kind='restoration_rendered'")
r['restorationJournal']=j['detail'] if j else None
r['restoration']=stored.get('restoration')
r['readback']=json.dumps(report.read(s,sys.argv[2]),sort_keys=True)
print(json.dumps(r,sort_keys=True))
s.close()`, pyPath, f.event, string(payload))
			row, err := f.s.One(f.ctx, "SELECT evidence,unresolved,review,restore FROM work_reports WHERE event_id=?", f.event)
			if err != nil {
				t.Fatal(err)
			}
			var expected map[string]any
			if err := json.Unmarshal(want, &expected); err != nil {
				t.Fatal(err)
			}
			readback, err := ReadWorkReport(f.ctx, f.s, f.event)
			if err != nil {
				t.Fatal(err)
			}
			if got := evidence.Dumps(readback, false, true, true); got != expected["readback"] {
				t.Errorf("ReadWorkReport byte diff\nGo: %s\nPython: %s", got, expected["readback"])
			}
			if revision {
				var journal string
				if err := f.s.DB.QueryRow("SELECT detail FROM journal WHERE kind='restoration_rendered'").Scan(&journal); err != nil {
					t.Fatal(err)
				}
				if journal != expected["restorationJournal"] {
					t.Errorf("restoration_rendered.detail byte diff\nGo: %s\nPython: %s", journal, expected["restorationJournal"])
				}
				gotProjection, err := json.Marshal(stored["restoration"])
				if err != nil {
					t.Fatal(err)
				}
				wantProjection, err := json.Marshal(expected["restoration"])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotProjection, wantProjection) {
					t.Errorf("restoration projection diff\nGo: %s\nPython: %s", gotProjection, wantProjection)
				}
			}
			for _, key := range []string{"evidence", "unresolved", "review", "restore"} {
				if row.Get(key) != expected[key] {
					t.Errorf("work_reports.%s byte diff\nGo: %s\nPython: %s", key, row.Get(key), expected[key])
				}
			}
		})
	}
}

func Test24_AutoFaultJournalBytes(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	pyPath := filepath.Join(f.root, "python.sqlite3")
	recheckPython(t, f.root, `import sqlite3,sys
s=sqlite3.connect(sys.argv[1]); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()`, f.s.Path, pyPath)
	ownCopied(t, pyPath, "python")
	fault := "<host> & café"
	if err := f.c.deferAutoFault(f.ctx, id, 1700000000, errors.New(fault)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := f.s.DB.QueryRow("SELECT detail FROM journal WHERE kind='supervisor_attempt_faulted'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := recheckPython(t, f.root, `import sys
from codex_session_relay.store import Store
from codex_session_relay.clock import FakeClock
from codex_session_relay.supervisorchannel import SupervisorChannel
s=Store(sys.argv[1])
c=SupervisorChannel(s,None,None,FakeClock(1700000000))
c.defer_after_fault(sys.argv[2],1700000000.0,Exception(sys.argv[3]))
print(s.one("SELECT detail FROM journal WHERE kind='supervisor_attempt_faulted'")['detail'],end='')
s.close()`, pyPath, id, fault)
	if !bytes.Equal([]byte(got), want) {
		t.Errorf("supervisor_attempt_faulted.detail byte diff\nGo: %s\nPython: %s", got, want)
	}
}

func Test24_ObligationHTMLBuiltBinaryBytes(t *testing.T) {
	root, _ := pythonSupervisorCapture(t, "TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
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
	binary := supervisorBinary(t)
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
	restoreSnapshot(t, path, before, "python")
	want := recheckPython(t, root, `import sys
from codex_session_relay import cli,clock,supervisorchannel
clock.SystemClock.iso=lambda self: sys.argv[1]
supervisorchannel.relay_program=lambda: (sys.argv[2],)
raise SystemExit(cli.main(sys.argv[3:]))`, answer.Message.At, alias, "--state", state, "supervisor-stage", "--event", event)
	if !bytes.Equal(got, want) {
		t.Errorf("obligation/message CLI byte diff\nGo: %s\nPython: %s", got, want)
	}
}
