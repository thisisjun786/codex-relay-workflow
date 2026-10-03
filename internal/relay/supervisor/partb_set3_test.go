package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// checkStanding compares the public project answer on the fixture's store as it stands, as JSON
// decodes it, with the golden. Observations are input, not a normalization of the result.
func checkStanding(t *testing.T, f *stageFixture, project string, readings []any) {
	t.Helper()
	got, err := f.c.Standing(context.Background(), project, readings)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, goldenKey(t, "standing"), asJSON(t, got), fixtureGolden(t, f.root)...)
}

// checkStatus is checkStanding for the project status answer.
func checkStatus(t *testing.T, f *stageFixture, project string, readings []any) {
	t.Helper()
	got, err := f.c.StatusAnswer(context.Background(), project, readings)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, goldenKey(t, "status"), asJSON(t, got), fixtureGolden(t, f.root)...)
}

func Test24_SR_16_EnvelopeCallerWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	for _, tc := range []struct {
		name     string
		decision bool
		missing  bool
	}{{"completion", false, false}, {"decision", true, false}, {"decision-missing", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			o := f.obligation(t)
			decision := ""
			if tc.decision {
				o.Kind = "decision_request"
				o.ID = hash32(o.Kind + "|" + o.RelationID + "|" + o.Subject)
				if !tc.missing {
					decision = "which of the two readings of the criterion is the agreed one"
					o.Detail = decision
				} else {
					o.Detail = ""
				}
			}
			f.c.Program = "codex-session-relay"
			r := Resolution{"01parent-task", "01supervisor-task", "PRJ-1", "INI-1", "linkage"}
			packet, composeErr := f.c.Compose(context.Background(), o, r, f.at)
			got := map[string]any{"value": nil, "error": nil}
			if composeErr != nil {
				got["error"] = "malformed_receipt: " + composeErr.Error()
			} else {
				region := packet["envelope"].(map[string]any)
				region["evidence"] = []string{"codex-session-relay show --event " + o.Subject}
				got["value"] = region
			}
			golden.CheckJSON(t, goldenKey(t, "envelope"), asJSON(t, got), fixtureGolden(t, f.root)...)
		})
	}
}

func Test24_SR_15_LiveProjectStatusWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec(`INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES ('rel-1',1,'dispatch-1','bound','turn-1','initial_assignment',?,?)`, f.at, f.at); err != nil {
		t.Fatal(err)
	}
	o := f.obligation(t)
	if _, err := f.c.RecordReport(context.Background(), o, f.at, nil, nil); err != nil {
		t.Fatal(err)
	}
	decision, err := f.c.Selection(context.Background(), o, "", nil)
	if err != nil || decision["report"] != false {
		t.Fatalf("suppressed selection: %v %v", decision, err)
	}
	checkStatus(t, f, "PRJ-1", nil)
}

// checkEvent compares the obligation Channel.FromEvent reads from event, as JSON decodes it,
// with the golden.
func checkEvent(t *testing.T, f *stageFixture, event string) {
	t.Helper()
	got, err := f.c.FromEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, goldenKey(t, "event"), asJSON(t, got), fixtureGolden(t, f.root)...)
}

// checkSelectEvent compares Channel.SelectEvent's answer, as JSON decodes it, with the golden.
func checkSelectEvent(t *testing.T, f *stageFixture, event, recipient string, now float64) {
	t.Helper()
	got, err := f.c.SelectEvent(context.Background(), event, recipient, now)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, goldenKey(t, "select_event"), asJSON(t, got), fixtureGolden(t, f.root)...)
}

func Test24_RCL_2_InvalidIdentityWholeOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, flag, value string }{{"assignment", "--assignment", "../outside"}, {"session", "--session", "a/b"}} {
		t.Run(tc.name, func(t *testing.T) {
			code, payload, _, created := reportingCLIParity(t, func(home string) []string {
				args := []string{"--state", "$STATE", "reporting-show", "--marker-root", home + "/markers", "--workspace", home + "/workspace", "--assignment", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--session", "session-1", "--turn", "turn-1"}
				for i := range args {
					if args[i] == tc.flag {
						args[i+1] = tc.value
					}
				}
				return args
			})
			if code != 4 || created || payload["error"] != "usage" {
				t.Fatalf("code=%d payload=%s created=%v", code, jsonText(payload), created)
			}
		})
	}
}

func Test24_RCL_2_MissingFlagExitTwo(t *testing.T) {
	t.Parallel()
	code, _, stderr, created := reportingCLIParity(t, func(home string) []string {
		return []string{"--state", "$STATE", "reporting-show", "--marker-root", home, "--workspace", home, "--assignment", strings.Repeat("a", 64), "--session", "session-1"}
	})
	if code != 2 || created || !strings.Contains(stderr, "required: --turn") {
		t.Fatalf("exit=%d stderr=%q state created=%t", code, stderr, created)
	}
}
func Test24_SR_1_EventWholeOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, status, outcome, producer string }{
		{"done", "DONE", "ready_for_review", "child"}, {"blocked", "BLOCKED", "blocked_needs_input", "child"},
		{"decision", "NEEDS_HUMAN", "blocked_needs_input", "child"}, {"failed", "", "failed", "child"},
		{"downward", "NEEDS_HUMAN", "revision_request", "relay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture24(t)
			if _, err := f.s.DB.Exec("UPDATE events SET outcome=?,producer=? WHERE event_id='event-1'", tc.outcome, tc.producer); err != nil {
				t.Fatal(err)
			}
			if tc.status == "" {
				if _, err := f.s.DB.Exec("DELETE FROM work_reports WHERE event_id='event-1'"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.s.DB.Exec("UPDATE work_reports SET cxc_status=? WHERE event_id='event-1'", tc.status); err != nil {
				t.Fatal(err)
			}
			checkEvent(t, f, "event-1")
		})
	}
}

func Test24_SR_1_OrdinaryEventSelectionWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("UPDATE events SET outcome='interrupted' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("DELETE FROM work_reports WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	checkSelectEvent(t, f, "event-1", "", 1700000000)
}
func Test24_SR_7_DeliveryClockWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("INSERT INTO recipient_lifecycle (task_id,deliverable,observed_at) VALUES (?,?,?)", "01supervisor-task", "yes", f.at); err != nil {
		t.Fatal(err)
	}
	checkSelectEvent(t, f, "event-1", "01supervisor-task", 1700000000)
	checkSelectEvent(t, f, "event-1", "01supervisor-task", 1700000961)
}

func Test24_SR_2_StableIdentityWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	checkEvent(t, f, "event-1")
	checkEvent(t, f, "event-1")
	if _, err := f.s.DB.Exec("UPDATE relationships SET parent_task_id='successor' WHERE relationship_id='rel-1'"); err != nil {
		t.Fatal(err)
	}
	checkEvent(t, f, "event-1")
}

func Test24_SR_3_SelectionAfterProducedReportWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	checkSelection(t, f, o, "", nil)
	first, err := f.c.RecordReport(context.Background(), o, f.at, nil, nil)
	if err != nil || first["recorded"] != true {
		t.Fatalf("record: %v %v", first, err)
	}
	checkSelection(t, f, o, "", nil)
	second, err := f.c.RecordReport(context.Background(), o, f.at, nil, nil)
	if err != nil || second["recorded"] != false {
		t.Fatalf("repeat: %v %v", second, err)
	}
	checkSelection(t, f, o, "", nil)
}
func Test24_SR_2_BlockCauseWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("UPDATE events SET outcome='blocked_needs_input' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE work_reports SET cxc_status='BLOCKED',cxc_reason='waiting on API',summary='schema update' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	checkEvent(t, f, "event-1")
	if _, err := f.s.DB.Exec("INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) SELECT 'event-2',relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,'turn-2',turn_status,receipt,first_seen_at,last_seen_at FROM events WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) SELECT 'event-2',submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at FROM work_reports WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	checkEvent(t, f, "event-2")
	if _, err := f.s.DB.Exec("UPDATE work_reports SET cxc_reason='waiting on',summary='API schema update' WHERE event_id='event-2'"); err != nil {
		t.Fatal(err)
	}
	checkEvent(t, f, "event-2")
	checkStanding(t, f, "PRJ-1", nil)
}

func Test24_SR_4_ArchivedStandingWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("UPDATE relationships SET status='archived',superseded_by='rel-next' WHERE relationship_id='rel-1'"); err != nil {
		t.Fatal(err)
	}
	checkStanding(t, f, "PRJ-1", nil)
}
func Test24_SR_9_StandingGridWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	scopes := []struct {
		name    string
		value   any
		present bool
	}{
		{"absent", nil, false}, {"null", nil, true}, {"ours", "rel-1", true},
		{"foreign", "rel-elsewhere", true}, {"list", []any{"rel-1"}, true}, {"blank", "   ", true},
	}
	for _, scope := range scopes {
		for _, state := range []string{"reported", "in_progress", "unmanaged", "unmeasured", "unreported", "foreign_schema", "something"} {
			t.Run(scope.name+"/"+state, func(t *testing.T) {
				reading := map[string]any{"schema": "reporting-observation/1", "reportingState": state, "reason": "terminal_without_report", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
				if scope.present {
					reading["relationshipId"] = scope.value
				}
				if state == "foreign_schema" {
					reading["schema"] = "something-else/1"
					reading["reportingState"] = "reported"
				}
				checkStanding(t, f, "PRJ-1", []any{reading})
			})
		}
	}
}
func Test24_SR_12_StandingDedupWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	good := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": "rel-1", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	bad := map[string]any{"schema": "reporting-observation/1", "reportingState": "something", "relationshipId": "rel-1", "selectors": map[string]any{"turn": "turn-7"}}
	foreign := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-elsewhere", "selectors": map[string]any{"turn": "turn-7"}}
	for _, tc := range []struct {
		name     string
		readings []any
	}{
		{"duplicate omission", []any{good, good}}, {"duplicate gap", []any{bad, bad}},
		{"foreign", []any{foreign, foreign}}, {"coexisting", []any{good, bad, bad}},
		{"non-object", []any{nil}}, {"malformed selectors", []any{map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-1", "selectors": []any{"turn-7"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) { checkStanding(t, f, "PRJ-1", tc.readings) })
	}
}
func Test24_SR_12_ContrastingReadingsWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	rid := "rel-1"
	base := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": rid, "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	for _, readings := range [][]any{
		{map[string]any{"schema": "reporting-observation/1", "reportingState": "unmeasured", "reason": "marker_unreadable", "relationshipId": rid}},
		{[]any{rid}, map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": []any{rid}, "selectors": map[string]any{"turn": "turn-7"}}},
		{base, map[string]any{"schema": "reporting-observation/1", "reportingState": "reported", "relationshipId": rid}},
		{base, map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": rid, "selectors": map[string]any{"turn": "turn-8"}}},
	} {
		checkStanding(t, f, "PRJ-1", readings)
	}
}
func Test24_SR_12_TwoEventsOneBlockWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("UPDATE events SET outcome='blocked_needs_input' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("UPDATE work_reports SET cxc_status='BLOCKED',cxc_reason='upstream has not landed' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) SELECT 'event-2',relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,'turn-2',turn_status,receipt,first_seen_at,last_seen_at FROM events WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) SELECT 'event-2',submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at FROM work_reports WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	checkStanding(t, f, "PRJ-1", nil)
}
func Test24_SR_13_ConfirmedOmissionWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": "rel-1", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	for _, query := range []string{
		"INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')",
		"INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES ('confirmed','rel-1','REL-1','coordination_document','doc-1','verdict','turn-7','digest','summary','confirmed','t','t')",
	} {
		if _, err := f.s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	o := ObservationObligation(reading)
	checkSelection(t, f, *o, "", nil)
	checkStanding(t, f, "PRJ-1", []any{reading})
}
func Test24_SR_5_AdditionalRulingWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ id, kind, state, ref string }{
		{"progress", "progress", "confirmed", "doc-1"}, {"failed", "verdict", "failed", "doc-1"},
		{"confirmed-elsewhere", "verdict", "confirmed", "doc-2"},
	}
	for _, r := range rows {
		if _, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", r.id, "rel-1", "REL-1", "coordination_document", r.ref, r.kind, "event-1", "digest", "summary", r.state, "t", "t"); err != nil {
			t.Fatal(err)
		}
		checkSelection(t, f, o, "", nil)
	}
}

// checkSupervisorBinary runs the built binary on the fixture's state and compares its exit code
// and stdout with the golden, the wall-clock times it wrote as placeholders. It returns the
// decoded answer.
func checkSupervisorBinary(t *testing.T, f *stageFixture, binary string, args ...string) map[string]any {
	t.Helper()
	state := filepath.Join(f.root, "state")
	goCmd := exec.Command(binary, append([]string{"relay", "--state", state}, args...)...)
	goCmd.Env = append(os.Environ(), "HOME="+f.root, "XDG_STATE_HOME="+f.root, "CODEX_HOME="+f.root)
	actual, goErr := goCmd.Output()
	goCode := commandExit24(t, goErr)
	golden.CheckJSON(t, goldenKey(t, args[0]), map[string]any{"code": goCode, "stdout": string(actual)}, append(goldenWallTimes(actual), fixtureGolden(t, f.root)...)...)
	var got map[string]any
	if err := json.Unmarshal(actual, &got); err != nil {
		t.Fatalf("Go JSON: %v %s", err, actual)
	}
	return got
}

func commandExit24(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func Test24_SupervisorEmptyStringTruthinessMatchesTheGolden(t *testing.T) {
	t.Parallel()
	binary := testsupport.CRW(t)
	observation := filepath.Join(t.TempDir(), "empty-observation.json")
	if err := os.WriteFile(observation, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"empty-project", []string{"supervisor-stage", "--project="}},
		{"empty-recipient", []string{"supervisor-stage", "--project", "p", "--recipient", ""}},
		{"empty-event-with-observation", []string{"supervisor-stage", "--event", "", "--observation", observation}},
		{"empty-event", []string{"supervisor-stage", "--event="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			goState := filepath.Join(home, "go-state")
			run := func(command *exec.Cmd) (int, []byte, []byte) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				command.Stdout, command.Stderr = &stdout, &stderr
				code := commandExit24(t, command.Run())
				return code, stdout.Bytes(), stderr.Bytes()
			}
			goCmd := exec.Command(binary, append([]string{"relay", "--state", goState}, tc.args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg-state"), "XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(home, "xdg-data"), "XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"), "CODEX_HOME="+filepath.Join(home, "codex"), "COLUMNS=80", "TMPDIR="+os.TempDir())
			goCode, goOut, goErr := run(goCmd)
			golden.CheckJSON(t, "answer", map[string]any{"code": goCode, "stdout": string(goOut), "stderr": string(goErr)}, golden.Substitute(observation, "<observation>"), golden.Substitute(home, "<home>"), golden.Substitute(repoRoot(t), "<repo>"))
		})
	}
}

func Test24_SR_18_RealBinarySelectWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	checkSupervisorBinary(t, f, testsupport.CRW(t), "supervisor-select", "--event", "event-1")
}
func Test24_SR_18_RealBinaryRecordWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	binary := testsupport.CRW(t)
	checkSupervisorBinary(t, f, binary, "supervisor-report-recorded", "--event", "event-1", "--message", "m-1")
	checkSupervisorBinary(t, f, binary, "supervisor-report-recorded", "--event", "event-1", "--message", "m-1")
	checkSupervisorBinary(t, f, binary, "supervisor-select", "--event", "event-1")
}
func Test24_SR_19_RealBinaryOmissionWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	binary := testsupport.CRW(t)
	reading := filepath.Join(f.root, "omission.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-1","executionGeneration":1,"selectors":{"turn":"turn-7"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	checkSupervisorBinary(t, f, binary, "supervisor-report-recorded", "--observation", reading, "--message", "m-1")
	checkSupervisorBinary(t, f, binary, "supervisor-report-recorded", "--observation", reading, "--message", "m-1")
}
func Test24_SR_19_StandingLiveBinaryWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec(`INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES ('rel-1',1,'dispatch-1','bound','turn-1','initial_assignment',?,?)`, f.at, f.at); err != nil {
		t.Fatal(err)
	}
	reading := filepath.Join(f.root, "observation.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-1","executionGeneration":1,"selectors":{"turn":"turn-7"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	checkSupervisorBinary(t, f, testsupport.CRW(t), "supervisor-standing", "--project", "PRJ-1", "--observation", reading)
}
func Test24_SR_19_StandingUnregisteredBinaryWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	if _, err := f.s.DB.Exec("UPDATE relationships SET status='archived' WHERE relationship_id='rel-1'"); err != nil {
		t.Fatal(err)
	}
	reading := filepath.Join(f.root, "observation.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-1","executionGeneration":1,"selectors":{"turn":"turn-7"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	checkSupervisorBinary(t, f, testsupport.CRW(t), "supervisor-standing", "--project", "PRJ-1", "--observation", reading)
}
func Test24_SR_20_UsageWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	binary := testsupport.CRW(t)
	if _, err := f.s.DB.Exec("UPDATE events SET outcome='failed' WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec("DELETE FROM work_reports WHERE event_id='event-1'"); err != nil {
		t.Fatal(err)
	}
	reading := filepath.Join(f.root, "reported.json")
	if err := os.WriteFile(reading, []byte(`{"schema":"reporting-observation/1","reportingState":"reported","relationshipId":"rel-1","selectors":{"turn":"turn-7"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(f.root, "broken.json")
	if err := os.WriteFile(broken, []byte{0xff, 0xfe}, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"failed-event", []string{"supervisor-report-recorded", "--event", "event-1"}},
		{"reported-observation", []string{"supervisor-report-recorded", "--observation", reading}},
		{"unreadable-observation", []string{"supervisor-standing", "--project", "PRJ-1", "--observation", broken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(f.root, "state")
			goCmd := exec.Command(binary, append([]string{"relay", "--state", state}, tc.args...)...)
			goCmd.Env = append(os.Environ(), "HOME="+f.root, "XDG_STATE_HOME="+f.root, "CODEX_HOME="+f.root)
			actual, goErr := goCmd.Output()
			goCode := commandExit24(t, goErr)
			golden.CheckJSON(t, "answer", map[string]any{"code": goCode, "stdout": string(actual)}, fixtureGolden(t, f.root)...)
		})
	}
}
func Test24_SR_6_UnreadableObservationWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	if _, err := f.s.DB.Exec("INSERT INTO recipient_lifecycle (task_id,deliverable,observed_at) VALUES ('01supervisor-task','yes','not-a-date')"); err != nil {
		t.Fatal(err)
	}
	now := float64(1700000000)
	checkSelection(t, f, o, "01supervisor-task", &now)
}
func Test24_SR_23_RepointedTargetWholeOutput(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, ref string }{{"old", "doc-1"}, {"new", "doc-2"}} {
		if _, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,'confirmed','t','t')", r.id, "rel-1", "REL-1", "coordination_document", r.ref, "event-1", "digest", "summary"); err != nil {
			t.Fatal(err)
		}
	}
	checkSelection(t, f, o, "", nil)
	if _, err := f.s.DB.Exec("UPDATE sync_targets SET target_ref='doc-2' WHERE relationship_id='rel-1'"); err != nil {
		t.Fatal(err)
	}
	checkSelection(t, f, o, "", nil)
	if _, err := f.s.DB.Exec("UPDATE sync_targets SET target_ref='doc-1' WHERE relationship_id='rel-1'"); err != nil {
		t.Fatal(err)
	}
	checkSelection(t, f, o, "", nil)
}
func Test24_SR_23_CurrentRulingWholeOutput(t *testing.T) {
	t.Parallel()
	for _, newState := range []string{"failed", "confirmed"} {
		t.Run(newState, func(t *testing.T) {
			f := fixture24(t)
			o := f.obligation(t)
			if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct{ id, state, ref string }{{"old", "confirmed", "doc-1"}, {"new", newState, "doc-1"}} {
				if _, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,?,?,?)", r.id, "rel-1", "REL-1", "coordination_document", r.ref, "event-1", "digest", "summary", r.state, "t", "t"); err != nil {
					t.Fatal(err)
				}
			}
			checkSelection(t, f, o, "", nil)
		})
	}
}
