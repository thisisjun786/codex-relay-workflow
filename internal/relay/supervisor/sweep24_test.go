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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

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
	t.Parallel()
	f := fixture24(t)
	sweepExec(t, f, "DELETE FROM work_reports")
	if _, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at); err != nil {
		t.Fatal(err)
	}
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge','2023-11-14T22:13:20.000000+00:00')`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S2: one block stated twice (same cause, two events), staged fresh from standing.
func TestSweep24_BlockStatedTwiceStagesLatest(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	sweepExec(t, f,
		"UPDATE work_reports SET cxc_status='BLOCKED'",
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-2','rel-1',1,'def','ready_for_review','child','child','turn-2','completed','{}','2023-11-14T22:13:21.000000+00:00','2023-11-14T22:13:21.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-2',1,'rel-1',1,'def','thisisjun786/codex-relay-workflow','BLOCKED','proved','v1','the work is done','merge','2023-11-14T22:13:21.000000+00:00')`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S3: a message held superseded_by_report whose obligation is still owed, staged again.
func TestSweep24_SupersededHoldReleasedOnStage(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='superseded_by_report', next_eligible_at=123")
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S4: completion staged, supervisor handed over, message deferred_busy with a backoff: readdress.
func TestSweep24_EventReaddressFromDeferred(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET state='deferred_busy', next_eligible_at=1700000100, hold_reason='hierarchy_unresolved'")
	moveDevinSupervisor(t, f)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S5: completion staged, report resubmitted (submission 2) and supervisor moved: readdress+restate.
func TestSweep24_EventReaddressWithNewSubmission(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.staged(t)
	moveDevinSupervisor(t, f)
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:13:20.000000+00:00')`)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S6: repeated automatic ticks over an event obligation (no omissions -> F6 not involved).
func TestSweep24_StageUnsentRepeatedTicks(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
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
	checkSupervisorValues(t, got, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S7: moved hierarchy with an attempted (maybe-sent) completion: refusal text.
func TestSweep24_ReaddressRefusedAfterSend(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET state='uncertain', attempt_count=1")
	moveDevinSupervisor(t, f)
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

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
	// The checked-in snapshots predate the fence: a fixture store, stamped for the runtime under test.
	testsupport.Fence(t, root+"/state/relay.sqlite3", "go")
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
	t.Parallel()
	f, reading, project := snapshotFixture(t, "before")
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
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S9: the parent staged its reading, then the child declared in_progress; parent stages again.
func TestSweep24_DeclarationAfterParentStaged(t *testing.T) {
	t.Parallel()
	f, reading, project := snapshotFixture(t, "after")
	sweepExec(t, f, "UPDATE supervisor_messages SET reading=replace(reading,'"+"PLACEHOLDER"+"','x')")
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, []map[string]any{reading})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, append([]map[string]any{reading}, derived...), f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S10: staged parent reading, then child declared in_progress: Go attempt still sends.
func TestSweep24_DeclarationAfterStagedStillSent(t *testing.T) {
	t.Parallel()
	f, _, _ := snapshotFixture(t, "after")
	var id, frozen string
	if err := f.s.DB.QueryRow("SELECT message_id, reading FROM supervisor_messages").Scan(&id, &frozen); err != nil {
		t.Fatal(err)
	}
	var recipient string
	_ = f.s.DB.QueryRow("SELECT recipient_task_id FROM supervisor_messages").Scan(&recipient)
	f.c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return &delivery.TaskSettings{}, nil }
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
	t.Parallel()
	f, reading, project := snapshotFixture(t, "plain")
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, []map[string]any{reading}) // CLI supervisor-stage --project
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, append([]map[string]any{reading}, derived...), f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S12: store-derived omission inside its grace, no caller reading (cli supervisor-stage --project path).
func TestSweep24_ProjectCLIUsesGraceAndDeduplicatesCallerReading(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, variant string
		caller        bool
	}{{"caller reading", "plain", true}, {"inside grace", "plain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			goFixture, reading, project := snapshotFixture(t, tc.variant)
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
			binary := testsupport.CRW(t)
			goCmd := exec.Command(binary, append([]string{"relay", "--state", filepath.Join(goFixture.root, "state"), "supervisor-stage", "--project", project}, args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+goFixture.root, "XDG_STATE_HOME="+goFixture.root, "CODEX_HOME="+goFixture.root)
			goOut, goErr := goCmd.CombinedOutput()
			if goErr != nil {
				t.Fatalf("Go CLI: %v\n%s", goErr, goOut)
			}
			var goAnswer map[string]any
			if json.Unmarshal(goOut, &goAnswer) != nil {
				t.Fatalf("CLI JSON: %s", goOut)
			}
			// The CLI's staged and refused counts are the golden.
			golden.CheckJSON(t, "supervisor-stage", map[string]any{"staged": len(goAnswer["staged"].([]any)), "refused": len(goAnswer["refused"].([]any))})
		})
	}
}

func TestSweep24_StoreReadingInsideGrace(t *testing.T) {
	t.Parallel()
	f, _, project := snapshotFixture(t, "plain")
	derived, err := f.c.OmissionReadingsExcept(f.ctx, project, f.at, 300, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.StageStandingWithObservations(f.ctx, project, derived, f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S13: hierarchy_unresolved hold on a queued completion, endpoints unchanged; staged again.
func TestSweep24_UnaddressedHoldReleased(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.staged(t)
	sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved', state='withheld_pre_send', next_eligible_at=1700000500")
	answer, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

// S14: two completions on one relationship with equal first_seen_at; staging order.
func TestSweep24_EqualTimestampEvents(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	sweepExec(t, f,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-0','rel-1',1,'def','ready_for_review','child','child','turn-0','completed','{}','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`)
	a, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.c.StageUnsent(f.ctx, "PRJ-1", f.at, 300)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{a, b}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}
