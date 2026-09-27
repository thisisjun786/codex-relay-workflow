package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// sweepPython runs body against a VACUUM copy of f's store. body sees store, clock,
// channel (state_directory = f.root/state, relay_program pinned) and appends to `out`.
func sweepPython(t *testing.T, f *stageFixture, body string, args ...string) supervisorCapture {
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), "relay.sqlite3")
	if _, err := f.s.DB.ExecContext(f.ctx, "VACUUM INTO ?", copyPath); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
import json,sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.linkage import Linkage
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay import supervision,supervisorchannel
from codex_session_relay.delivery import DeliveryRefused
store=Store(sys.argv[1]); clock=FakeClock()
supervisorchannel.relay_program=lambda: ('/usr/bin/codex-session-relay',)
channel=supervisorchannel.SupervisorChannel(store,Registry(store,clock),Linkage(store,clock),clock,state_directory=sys.argv[2])
args=sys.argv[3:]
out=[]
def refusal(fn):
    try:
        return {'ok':fn()}
    except DeliveryRefused as error:
        return {'error':'DeliveryRefused','reason':getattr(getattr(error,'reason',None),'value',None),'detail':error.detail}
` + body + `
tables={}
for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
    name=row['name']; values=[dict(one) for one in store.all('SELECT * FROM '+name+' ORDER BY rowid')]
    if values: tables[name]=values
print(json.dumps({'captures':out,'problems':[],'tables':tables},sort_keys=True,default=str))
store.close()
`
	cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), append([]string{"-c", script, copyPath, filepath.Join(f.root, "state")}, args...)...)
	home := t.TempDir()
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live Python: %v\n%s", err, out)
	}
	var capture supervisorCapture
	if err := json.Unmarshal(out, &capture); err != nil {
		t.Fatalf("live Python JSON: %v\n%s", err, out)
	}
	return capture
}

func sweepExec(t *testing.T, f *stageFixture, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := f.s.DB.ExecContext(f.ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// S1: a completion staged before any work report, report recorded after, staged again.
func TestSweep24_CompletionReportRecordedAfterStaging(t *testing.T) {
	f := fixture24(t)
	sweepExec(t, f, "DELETE FROM work_reports")
	if _, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at); err != nil {
		t.Fatal(err)
	}
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge','2023-11-14T22:13:20.000000+00:00')`)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S2: one block stated twice (same cause, two events), staged fresh from standing.
func TestSweep24_BlockStatedTwiceStagesLatest(t *testing.T) {
	f := fixture24(t)
	sweepExec(t, f,
		"UPDATE work_reports SET cxc_status='BLOCKED'",
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-2','rel-1',1,'def','ready_for_review','child','child','turn-2','completed','{}','2023-11-14T22:13:21.000000+00:00','2023-11-14T22:13:21.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-2',1,'rel-1',1,'def','thisisjun786/codex-relay-workflow','BLOCKED','proved','v1','the work is done','merge','2023-11-14T22:13:21.000000+00:00')`)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S3: a message held superseded_by_report whose obligation is still owed, staged again.
func TestSweep24_SupersededHoldReleasedOnStage(t *testing.T) {
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='superseded_by_report', next_eligible_at=123")
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S4: completion staged, supervisor handed over, message deferred_busy with a backoff: readdress.
func TestSweep24_EventReaddressFromDeferred(t *testing.T) {
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET state='deferred_busy', next_eligible_at=1700000100, hold_reason='hierarchy_unresolved'")
	moveDevinSupervisor(t, f)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S5: completion staged, report resubmitted (submission 2) and supervisor moved: readdress+restate.
func TestSweep24_EventReaddressWithNewSubmission(t *testing.T) {
	f := fixture24(t)
	f.staged(t)
	moveDevinSupervisor(t, f)
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:13:20.000000+00:00')`)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S6: repeated automatic ticks over an event obligation (no omissions -> F6 not involved).
func TestSweep24_StageUnsentRepeatedTicks(t *testing.T) {
	f := fixture24(t)
	python := sweepPython(t, f, `
for i in range(3):
    a=channel.stage_unsent('PRJ-1'); out.append(a)
store.db.execute("UPDATE supervisor_messages SET state='sending'")
out.append(channel.stage_unsent('PRJ-1'))
`)
	var got []any
	for i := 0; i < 3; i++ {
		a, err := f.c.StageUnsent(f.ctx, "PRJ-1", f.at, 300)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	sweepExec(t, f, "UPDATE supervisor_messages SET state='sending'")
	a, err := f.c.StageUnsent(f.ctx, "PRJ-1", f.at, 300)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, a)
	sweepDump(t, f, got, python)
	compareSupervisorValues(t, got, python)
	compareSupervisorTables(t, f.s, python)
}

// S7: moved hierarchy with an attempted (maybe-sent) completion: refusal text.
func TestSweep24_ReaddressRefusedAfterSend(t *testing.T) {
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET state='uncertain', attempt_count=1")
	moveDevinSupervisor(t, f)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

func sweepDump(t *testing.T, f *stageFixture, got []any, python supervisorCapture) {}

// snapshotFixture opens a copy of a Python-built store under a fresh state directory.
func snapshotFixture(t *testing.T, variant string) (*stageFixture, map[string]any, string) {
	t.Helper()
	repo, _ := filepath.Abs("../../..")
	src := filepath.Join(repo, "internal/relay/supervisor/testdata/sweep24", variant)
	var meta map[string]any
	raw, err := os.ReadFile(src + "/meta.json")
	if err != nil {
		t.Skip("snapshot missing: run snap_withdrawn.py")
	}
	_ = json.Unmarshal(raw, &meta)
	root := t.TempDir()
	_ = os.MkdirAll(root+"/state", 0o755)
	data, _ := os.ReadFile(src + "/relay.sqlite3")
	if err := os.WriteFile(root+"/state/relay.sqlite3", data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), root+"/state/relay.sqlite3", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	reading := meta["reading"].(map[string]any)
	reading["selectors"].(map[string]any)["state"] = root + "/state"
	f := &stageFixture{c: &Channel{Store: s, Linkage: StoreLinkage{s}, Program: "/usr/bin/codex-session-relay"}, s: s, ctx: context.Background(), root: root, at: meta["now"].(string)}
	return f, reading, meta["project"].(string)
}

// S8: the child declared the turn in_progress before the parent stages its own reading.
func TestSweep24_DeclarationBeforeParentStages(t *testing.T) {
	f, reading, project := snapshotFixture(t, "before")
	raw, _ := json.Marshal(reading)
	python := sweepPython(t, f, `out.append(channel.stage_standing(args[0], observations=[json.loads(args[1])]))`, project, string(raw))
	readings := []map[string]any{reading}
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, readings) // CLI supervisor-stage --project
	if err != nil {
		t.Fatal(err)
	}
	readings = append(readings, derived...)
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, readings, f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S9: the parent staged its reading, then the child declared in_progress; parent stages again.
func TestSweep24_DeclarationAfterParentStaged(t *testing.T) {
	f, reading, project := snapshotFixture(t, "after")
	sweepExec(t, f, "UPDATE supervisor_messages SET reading=replace(reading,'"+"PLACEHOLDER"+"','x')")
	raw, _ := json.Marshal(reading)
	python := sweepPython(t, f, `out.append(channel.stage_standing(args[0], observations=[json.loads(args[1])]))`, project, string(raw))
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, []map[string]any{reading})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, append([]map[string]any{reading}, derived...), f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S10: staged parent reading, then child declared in_progress: Go attempt still sends.
func TestSweep24_DeclarationAfterStagedStillSent(t *testing.T) {
	f, _, _ := snapshotFixture(t, "after")
	var id, frozen string
	if err := f.s.DB.QueryRow("SELECT message_id, reading FROM supervisor_messages").Scan(&id, &frozen); err != nil {
		t.Fatal(err)
	}
	var recipient string
	_ = f.s.DB.QueryRow("SELECT recipient_task_id FROM supervisor_messages").Scan(&recipient)
	f.c.settingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return &delivery.TaskSettings{}, nil }
	h := &sendHost{status: "idle"}
	record, err := f.c.Attempt(f.ctx, id, h, 1_700_010_000)
	row, _ := f.s.SupervisorMessage(f.ctx, id)
	t.Logf("record=%v err=%v sends=%d state=%s hold=%v", record, err, len(h.sends), row.State, row.HoldReason)
	if len(h.sends) != 0 || row.HoldReason.String != "superseded_by_report" {
		t.Errorf("Python holds this message superseded_by_report and sends nothing; Go sends=%d hold=%v state=%s", len(h.sends), row.HoldReason, row.State)
	}
}

// S11: parent passes its own reading of an omission the store can also derive (cli supervisor-stage --project path).
func TestSweep24_CallerReadingPlusStoreReading(t *testing.T) {
	f, reading, project := snapshotFixture(t, "plain")
	raw, _ := json.Marshal(reading)
	python := sweepPython(t, f, `
clock.advance(float(args[2]))
out.append(channel.stage_standing(args[0], observations=[json.loads(args[1])]))`, project, string(raw), "0")
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, []map[string]any{reading}) // CLI supervisor-stage --project
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, append([]map[string]any{reading}, derived...), f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S12: store-derived omission inside its grace, no caller reading (cli supervisor-stage --project path).
func TestSweep24_ProjectCLIUsesGraceAndDeduplicatesCallerReading(t *testing.T) {
	for _, tc := range []struct {
		name, variant string
		caller        bool
	}{{"caller reading", "plain", true}, {"inside grace", "plain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			goFixture, reading, project := snapshotFixture(t, tc.variant)
			pythonRoot := t.TempDir()
			pythonState := filepath.Join(pythonRoot, "state")
			if err := os.MkdirAll(pythonState, 0700); err != nil {
				t.Fatal(err)
			}
			rawDB, _ := os.ReadFile(goFixture.s.Path)
			pythonDB := filepath.Join(pythonState, "relay.sqlite3")
			if err := os.WriteFile(pythonDB, rawDB, 0600); err != nil {
				t.Fatal(err)
			}
			var args []string
			if tc.caller {
				path := filepath.Join(t.TempDir(), "reading.json")
				goReading := copyReading(t, reading)
				goReading["selectors"].(map[string]any)["state"] = filepath.Join(goFixture.root, "state")
				raw, _ := json.Marshal(goReading)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				args = []string{"--observation", path}
			}
			binary := supervisorBinary(t)
			goCmd := exec.Command(binary, append([]string{"relay", "--state", filepath.Join(goFixture.root, "state"), "supervisor-stage", "--project", project}, args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+goFixture.root, "XDG_STATE_HOME="+goFixture.root, "CODEX_HOME="+goFixture.root)
			goOut, goErr := goCmd.CombinedOutput()
			if goErr != nil {
				t.Fatalf("Go CLI: %v\n%s", goErr, goOut)
			}
			repo, _ := filepath.Abs("../../..")
			pyArgs := []string{"--state", filepath.Join(pythonRoot, "state"), "supervisor-stage", "--project", project}
			if tc.caller {
				path := filepath.Join(t.TempDir(), "python-reading.json")
				pythonReading := copyReading(t, reading)
				pythonReading["selectors"].(map[string]any)["state"] = pythonState
				raw, _ := json.Marshal(pythonReading)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				pyArgs = append(pyArgs, "--observation", path)
			}
			pyCmd := exec.Command(filepath.Join(repo, ".venv/bin/codex-session-relay"), pyArgs...)
			pyCmd.Env = append(os.Environ(), "HOME="+pythonRoot, "XDG_STATE_HOME="+pythonRoot, "CODEX_HOME="+pythonRoot, "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src"))
			pyOut, pyErr := pyCmd.CombinedOutput()
			if pyErr != nil {
				t.Fatalf("Python CLI: %v\n%s", pyErr, pyOut)
			}
			var goAnswer, pyAnswer map[string]any
			if json.Unmarshal(goOut, &goAnswer) != nil || json.Unmarshal(pyOut, &pyAnswer) != nil {
				t.Fatalf("CLI JSON\nGo: %s\nPython: %s", goOut, pyOut)
			}
			goStaged := goAnswer["staged"].([]any)
			pyStaged := pyAnswer["staged"].([]any)
			if len(goStaged) != len(pyStaged) || len(goAnswer["refused"].([]any)) != len(pyAnswer["refused"].([]any)) {
				t.Fatalf("CLI behavior differs\nGo: %s\nPython: %s", goOut, pyOut)
			}
		})
	}
}

func TestSweep24_StoreReadingInsideGrace(t *testing.T) {
	f, _, project := snapshotFixture(t, "plain")
	python := sweepPython(t, f, `out.append(channel.stage_standing(args[0]))`, project)
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, derived, f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S13: hierarchy_unresolved hold on a queued completion, endpoints unchanged; staged again.
func TestSweep24_UnaddressedHoldReleased(t *testing.T) {
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved', state='withheld_pre_send', next_eligible_at=1700000500")
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1'))`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{answer}, python)
	compareSupervisorValues(t, []any{answer}, python)
	compareSupervisorTables(t, f.s, python)
}

// S14: two completions on one relationship with equal first_seen_at; staging order.
func TestSweep24_EqualTimestampEvents(t *testing.T) {
	f := fixture24(t)
	sweepExec(t, f,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-0','rel-1',1,'def','ready_for_review','child','child','turn-0','completed','{}','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`)
	python := sweepPython(t, f, `out.append(channel.stage_standing('PRJ-1')); out.append(channel.stage_unsent('PRJ-1'))`)
	a, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.c.StageUnsent(f.ctx, "PRJ-1", f.at, 300)
	if err != nil {
		t.Fatal(err)
	}
	sweepDump(t, f, []any{a, b}, python)
	compareSupervisorValues(t, []any{a, b}, python)
	compareSupervisorTables(t, f.s, python)
}
