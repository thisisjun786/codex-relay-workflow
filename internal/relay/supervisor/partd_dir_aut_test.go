package supervisor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func partDPython(t *testing.T, module, id string) (string, supervisorCapture) {
	t.Helper()
	root, err := os.MkdirTemp("", "crw-partd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// capture.json and the snapshots are recorded (pythonTree); Python's final store and its
	// final.sqlite3 copy are not, as no Go test reads them.
	pythonTree(t, module+" "+id, root, func() ([]byte, error) {
		repo := repoRoot(t)
		script, _ := filepath.Abs("testdata/partd_dir_aut_capture.py")
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(home, 0700); err != nil {
			return nil, err
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, root, module, id)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("python %s: %v\n%s", id, err, out)
		}
		for _, gone := range []string{home, filepath.Join(root, "final.sqlite3")} {
			if err := os.RemoveAll(gone); err != nil {
				return nil, err
			}
		}
		return nil, removeStoreFiles(filepath.Join(root, "tree", "state", "relay.sqlite3"))
	})
	return root, readSupervisorCapture(t, root)
}

func partDOpen(t *testing.T, root, snapshot string) (*store.Store, *Channel, *registry.Registry) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, snapshot+".sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tree", "state", "relay.sqlite3")
	restoreSnapshot(t, path, data, "go")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := func() string { return captureTime }
	r := &registry.Registry{Store: s, Now: now, Policy: registry.ResolveRolePolicy(map[string]string{})}
	repo, _ := filepath.Abs("../../..")
	c := &Channel{Store: s, Linkage: StoreLinkage{s}, Program: filepath.Join(repo, ".venv/bin/codex-session-relay"), clockISO: func() string { return captureTime }, Settings: &delivery.TaskSettings{}}
	return s, c, r
}
func partDCompare(t *testing.T, s *store.Store, got []any, py supervisorCapture) {
	t.Helper()
	compareSupervisorValues(t, got, py)
	compareSupervisorTables(t, s, py)
}
func partDLink(t *testing.T, s *store.Store) string {
	t.Helper()
	var x string
	if err := s.DB.QueryRow("SELECT link_id FROM scope_links WHERE lower_kind='project'").Scan(&x); err != nil {
		t.Fatal(err)
	}
	return x
}
func partDIDs(t *testing.T, r *registry.Registry) []string {
	t.Helper()
	rows, err := r.Directives(context.Background(), "project", "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, x := range rows {
		for _, f := range x {
			if f.Key == "disposition" && f.Value != nil {
				goto next
			}
		}
		out = append(out, did(x))
	next:
	}
	return out
}
func partDContest(t *testing.T, r *registry.Registry) []string {
	t.Helper()
	rows, err := r.ContestedDirectives(context.Background(), "project", "PRJ-1")
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, x := range rows {
		out = append(out, did(x))
	}
	return out
}
func partDRefusal(err error) map[string]any {
	var x *store.RefusedError
	if errors.As(err, &x) {
		return map[string]any{"reason": x.Reason, "detail": x.Detail}
	}
	if err == nil {
		return nil
	}
	return map[string]any{"error": err.Error()}
}
func pdRecord(t *testing.T, r *registry.Registry, link, digest, purpose, corr, task string) (contract.OrderedObject, error) {
	t.Helper()
	ref := sql.NullString{}
	if purpose != "" {
		ref = dref(t, purpose, link, digest, corr)
	}
	return r.RecordDirective(context.Background(), "project", "PRJ-1", task, "INI-1", link, digest, ref)
}

func Test24_DIR_1_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "TheG1OrderReportsUpward.test_a_scope_correction_beside_both_does_not_need_the_others_settled")
	s, c, r := partDOpen(t, root, "event")
	captureTokens21(t)
	h := &partDHost{sendHost{status: "idle"}}
	got, err := c.AutoSend(context.Background(), h, 1700000000, 4, 2, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	partDCompare(t, s, []any{0, 0, 0, len(partDIDs(t, r)), []any{got.SupervisorStaged, got.SupervisorSent}, partDContest(t, r)}, py)
}
func Test24_DIR_2_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_a_second_assignment_is_refused_and_names_the_one_in_force")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	a, e := pdRecord(t, r, l, "d-one", "project_assignment", "", "01supervisor-task")
	if e != nil {
		t.Fatal(e)
	}
	_, e = pdRecord(t, r, l, "d-two", "project_assignment", "", "01supervisor-task")
	x := partDRefusal(e)
	var retained []map[string]any
	rows, _ := s.All(context.Background(), "SELECT reason,incumbent,challenger,detail FROM linkage_conflicts WHERE scope_key=? ORDER BY id", "PRJ-1")
	for _, z := range rows {
		retained = append(retained, map[string]any{"reason": z.Get("reason"), "incumbent": z.Get("incumbent"), "challenger": z.Get("challenger"), "detail": z.Get("detail")})
	}
	_, _ = pdRecord(t, r, l, "d-two", "project_assignment", "", "01supervisor-task")
	var n int
	_ = s.DB.QueryRow("SELECT count(*) FROM linkage_conflicts").Scan(&n)
	partDCompare(t, s, []any{0, 2, x["reason"], contains(x["detail"], did(a)), contains(x["detail"], "linkage-settle"), partDIDs(t, r), len(retained), retained[0]["incumbent"], 2, n}, py)
}
func contains(v any, s string) bool {
	x, _ := v.(string)
	return len(s) == 0 || reflect.ValueOf(x).String() != "" && stringContains(x, s)
}
func stringContains(x, s string) bool {
	for i := 0; i+len(s) <= len(x); i++ {
		if x[i:i+len(s)] == s {
			return true
		}
	}
	return false
}

func Test24_DIR_3_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_two_different_answers_to_one_message_are_refused")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	a, _ := pdRecord(t, r, l, "d-yes", "relayed_decision", "msg-1", "01supervisor-task")
	_, e := pdRecord(t, r, l, "d-no", "relayed_decision", "msg-1", "01supervisor-task")
	x := partDRefusal(e)
	partDCompare(t, s, []any{0, 2, contains(x["detail"], did(a)), partDIDs(t, r)}, py)
}
func Test24_DIR_4_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_a_refusal_names_the_message_the_live_correction_answers")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	a, _ := pdRecord(t, r, l, "d-narrow", "scope_correction", "msg-blocked-7", "01supervisor-task")
	_, e := pdRecord(t, r, l, "d-widen", "scope_correction", "", "01supervisor-task")
	x := partDRefusal(e)
	partDCompare(t, s, []any{0, 2, contains(x["detail"], did(a)), contains(x["detail"], "msg-blocked-7")}, py)
}
func Test24_DIR_5_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_two_instructions_of_unknown_purpose_are_still_recorded_and_contested")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	_, _ = pdRecord(t, r, l, "d-one", "", "", "01supervisor-task")
	_, _ = pdRecord(t, r, l, "d-two", "", "", "01supervisor-task")
	partDCompare(t, s, []any{0, 0, len(partDIDs(t, r)), len(partDContest(t, r))}, py)
}
func Test24_DIR_6_WholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_a_place_keeps_one_live_row_across_a_handover")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	a, _ := pdRecord(t, r, l, "d-assignment", "project_assignment", "", "01supervisor-task")
	_, e := r.Handover(context.Background(), "supervisor", "INI-1", "01supervisor-task", registry.Endpoint{TaskID: "01supervisor-successor", HostID: "host-a", Cwd: sql.NullString{String: "/successor", Valid: true}, CXCSession: sql.NullString{String: "cxc-successor", Valid: true}}, nil, "the supervisor was replaced", "test")
	if e != nil {
		t.Fatal(e)
	}
	_, e1 := pdRecord(t, r, l, "d-assignment", "project_assignment", "", "01supervisor-successor")
	_, e2 := pdRecord(t, r, l, "d-other", "project_assignment", "", "01supervisor-successor")
	x1, x2 := partDRefusal(e1), partDRefusal(e2)
	vals := []any{0, 2, 2}
	for _, x := range []map[string]any{x1, x2} {
		vals = append(vals, x["reason"], contains(x["detail"], did(a)), contains(x["detail"], "link revision 1"))
	}
	vals = append(vals, contains(x1["detail"], "already has this same project_assignment"), partDIDs(t, r))
	_, e = r.SettleDirective(context.Background(), did(a), "superseded", "01supervisor-task", sql.NullString{String: "replaced by a later instruction", Valid: true})
	if e != nil {
		t.Fatal(e)
	}
	mine, _ := pdRecord(t, r, l, "d-assignment", "project_assignment", "", "01supervisor-successor")
	vals = append(vals, 0, 0, partDIDs(t, r), did(mine))
	partDCompare(t, s, vals, py)
}

func Test24_DIR_7_WholeLivePython(t *testing.T) {
	partDReplayReadScenario(t, "ARealConflictIsRefusedWhereItIsRecorded.test_a_pair_of_one_digest_an_older_writer_left_holds_no_report", 7)
}
func Test24_DIR_8_WholeLivePython(t *testing.T) {
	partDReplayReadScenario(t, "AHeldReportIsNamedWhereTheOperatorLooks.test_a_held_report_is_a_named_gap_in_supervisor_standing", 8)
}
func Test24_DIR_9_WholeLivePython(t *testing.T) {
	partDReplayReadScenario(t, "AHeldReportIsNamedWhereTheOperatorLooks.test_a_project_nobody_supervises_is_a_named_hold_of_its_own", 9)
}
func partDReplayReadScenario(t *testing.T, id string, kind int) {
	root, py := partDPython(t, "test_directive_places", id)
	snap := "event"
	s, c, r := partDOpen(t, root, snap)
	var got []any
	switch kind {
	case 7:
		standing, _ := c.Standing(context.Background(), "PRJ-1", nil)
		holds, _ := c.ReportHolds(context.Background(), standing)
		captureTokens21(t)
		tick, _ := c.AutoSend(context.Background(), &partDMissingHost{}, 1700000000, 4, 2, "", "", "")
		got = []any{0, len(partDIDs(t, r)), partDContest(t, r), 0, holds, tick.SupervisorStaged}
		partDExtra(t, root, "holds", []any{holds})
	case 8:
		l := partDLink(t, s)
		_, _ = pdRecord(t, r, l, "d-first", "", "", "01supervisor-task")
		_, _ = pdRecord(t, r, l, "d-second", "", "", "01supervisor-task")
		standing, _ := c.Standing(context.Background(), "PRJ-1", nil)
		holds, err := c.ReportHolds(context.Background(), standing)
		if err != nil {
			t.Fatal(err)
		}
		partDExtra(t, root, "holds", []any{holds})
		var reason, relation any
		var namesObligation, namesConflict bool
		if len(holds) > 0 {
			reason, relation = holds[0]["reason"], holds[0]["relationId"]
			namesConflict = contains(holds[0]["detail"], "instruction_conflict")
			for _, id := range holds[0]["obligationIds"].([]string) {
				namesObligation = namesObligation || id == captureObligation4(t, c, s).ID
			}
		}
		tick, err := c.AutoSend(context.Background(), &partDHost{sendHost{status: "idle"}}, 1700000000, 4, 2, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		got = []any{0, len(holds), reason, relation, namesObligation, namesConflict, []any{tick.SupervisorStaged, tick.SupervisorSent}, []any{}}
	case 9:
		standing, _ := c.Standing(context.Background(), "PRJ-1", nil)
		c.Linkage = staticLinkage{map[string]any{
			"state": "resolved", "readable": true, "contention": []any{},
			"gaps":   []any{map[string]any{"gap": "no_supervisor", "scopeKind": "project", "scopeKey": "PRJ-1"}},
			"levels": []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent-task"}, "depth": 1.0}},
		}}
		holds, _ := c.ReportHolds(context.Background(), standing)
		got = []any{[]any{[]any{holds[0]["gap"], holds[0]["reason"]}}, holds[0]["obligationIds"]}
	}
	partDCompare(t, s, got, py)
}

func Test24_AUT_1_WholeLivePython(t *testing.T) {
	partDAutoBasic(t, "AParentThatNeverReports.test_a_completion_goes_up_once_with_nobody_asking", 1)
}
func Test24_AUT_3_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "OneLogicalIdAcrossRestartsAndRedelivery.test_a_restarted_daemon_converges_on_the_message_already_sent", 3, 4, 2)
}
func Test24_AUT_4_WholeLivePython(t *testing.T) {
	partDAutoBasic(t, "ASupervisorWhoCannotBeWoken.test_an_archived_supervisor_is_not_woken_and_the_report_waits_for_it", 4)
}
func Test24_AUT_5_WholeLivePython(t *testing.T) {
	partDAutoBasic(t, "AParentWhoAlsoSendsByHand.test_a_parent_who_sent_first_leaves_the_daemon_nothing_to_send", 5)
}
func Test24_AUT_7_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "AReportWithNoAddresseeDoesNotHoldTheQueue.test_the_hold_is_released_when_the_hierarchy_names_the_message_again", 7, 4, 2)
}
func Test24_AUT_8_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "TwoSupervisorsOneStuck.test_a_head_that_is_never_sendable_does_not_starve_another", 8, 4, 1)
}
func partDAutoBasic(t *testing.T, id string, kind int) {
	root, py := partDPython(t, "test_supervisor_autosend", id)
	s, c, _ := partDOpen(t, root, "event")
	captureTokens21(t)
	var got []any
	switch kind {
	case 1:
		h := &partDHost{sendHost{status: "idle"}}
		a, _ := c.AutoSend(context.Background(), h, 1700000000, 4, 2, "", "", "")
		var n int
		_ = s.DB.QueryRow("SELECT count(*) FROM supervisor_messages").Scan(&n)
		var k, state string
		_ = s.DB.QueryRow("SELECT obligation_kind,state FROM supervisor_messages").Scan(&k, &state)
		b, _ := c.AutoSend(context.Background(), h, 1700003600, 4, 2, a.AfterProject, a.AfterStagedAt, a.AfterMessageID)
		got = []any{[]any{a.SupervisorStaged, a.SupervisorSent}, len(h.sends), n, []any{k, state}, []any{b.SupervisorStaged, b.SupervisorSent}, len(h.sends)}
		partDExtra(t, root, "ticks", []any{partDTickValue(1700000000, a), partDTickValue(1700003600, b)})
	case 4:
		h := &partDHost{sendHost{status: "idle", archived: true}}
		a, _ := c.AutoSend(context.Background(), h, 1700000000, 4, 2, "", "", "")
		var state string
		var hold sql.NullString
		_ = s.DB.QueryRow("SELECT state,hold_reason FROM supervisor_messages").Scan(&state, &hold)
		c.clockISO = func() string { return delivery.ISOOf(1700000005) }
		b, _ := c.AutoSend(context.Background(), h, 1700000005, 4, 2, a.AfterProject, a.AfterStagedAt, a.AfterMessageID)
		h.archived = false
		c.clockISO = func() string { return delivery.ISOOf(1700000066) }
		d, _ := c.AutoSend(context.Background(), h, 1700000066, 4, 2, b.AfterProject, b.AfterStagedAt, b.AfterMessageID)
		got = []any{[]any{}, 1 + a.SupervisorSent + b.SupervisorSent, []any{state, nil}, true, []any{}, d.SupervisorSent, stateOf(t, s, d.SupervisorSent)}
		partDExtra(t, root, "ticks", []any{partDTickValue(1700000000, a), partDTickValue(1700000005, b), partDTickValue(1700000066, d)})
	case 5:
		o := captureObligation4(t, c, s)
		st, _ := c.Stage(context.Background(), o, "", captureTime)
		h := &partDHost{sendHost{status: "idle"}}
		rec, _ := c.Attempt(context.Background(), st["messageId"].(string), h, 1700000000)
		a, _ := c.AutoSend(context.Background(), h, 1700003600, 4, 2, "", "", "")
		got = []any{rec["deliveryState"], []any{a.SupervisorStaged, a.SupervisorSent}, len(h.sends)}
		partDExtra(t, root, "ticks", []any{partDTickValue(1700003600, a)})
	}

	partDCompare(t, s, got, py)
}

type partDMissingHost struct{ partDHost }

func (*partDMissingHost) ReadThread(task string) (delivery.ThreadFacts, error) {
	return delivery.ThreadFacts{}, &delivery.HostError{Kind: "KeyError", Message: "'" + task + "'"}
}
func (*partDMissingHost) IsArchived(task string, _ any) (*bool, error) {
	return nil, &delivery.HostError{Kind: "KeyError", Message: "'" + task + "'"}
}
func (*partDMissingHost) ReadGoalStatus(task string) (any, error) {
	return nil, &delivery.HostError{Kind: "KeyError", Message: "'" + task + "'"}
}

type partDHost struct{ sendHost }

func (h *partDHost) ReadGoalStatus(string) (any, error) { return nil, nil }
func (h *partDHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	h.settings = settings
	if h.items == nil {
		h.items = map[string]string{}
	}
	turn := "turn-" + thread + "-1"
	h.items[turn] = message
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: turn}}, nil
}
func (h *partDHost) ReadTurn(thread, id string) (*delivery.TurnInfo, error) {
	at := float64(1700000001)
	if id == "turn-"+thread+"-1" {
		return &delivery.TurnInfo{TurnID: id, StartedAt: &at}, nil
	}
	return nil, nil
}

type partDRecipientHost struct {
	partDHost
	archivedTask  string
	archivedTasks map[string]bool
	recipients    []string
}

func (h *partDRecipientHost) IsArchived(task string, _ any) (*bool, error) {
	v := task == h.archivedTask || h.archivedTasks[task]
	return &v, nil
}
func (h *partDRecipientHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h.recipients = append(h.recipients, thread)
	return h.partDHost.SendMessage(id, thread, message, settings)
}
func (h *partDRecipientHost) count(task string) int {
	n := 0
	for _, x := range h.recipients {
		if x == task {
			n++
		}
	}
	return n
}

func stateOf(t *testing.T, s *store.Store, _ int) string {
	t.Helper()
	var x string
	_ = s.DB.QueryRow("SELECT state FROM supervisor_messages").Scan(&x)
	return x
}

// Each replay starts before the first tick, never from Python's final store.
// The oracle contains complete per-tick results as well as every persisted row.
func partDExtra(t *testing.T, root, key string, got any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var extra map[string]any
	if err = json.Unmarshal(raw, &extra); err != nil {
		t.Fatal(err)
	}
	compareSupervisorValues(t, []any{got}, supervisorCapture{Captures: []any{extra[key]}})
}

func partDTick(t *testing.T, c *Channel, h SendAdapter, now float64, projects, sends int, previous AutoSendResult) AutoSendResult {
	t.Helper()
	c.clockISO = func() string { return delivery.ISOOf(now) }
	result, err := c.AutoSend(context.Background(), h, now, projects, sends, previous.AfterProject, previous.AfterStagedAt, previous.AfterMessageID)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func partDTickValue(now float64, result AutoSendResult) map[string]any {
	return map[string]any{"now": now, "supervisorStaged": result.SupervisorStaged, "supervisorSent": result.SupervisorSent, "deferred": result.Deferred, "skipped": result.Skipped, "notes": result.Notes, "afterProject": result.AfterProject, "afterSend": []string{result.AfterStagedAt, result.AfterMessageID}}
}
func partDAutoReplay(t *testing.T, id string, kind, projects, sends int) {
	t.Helper()
	root, py := partDPython(t, "test_supervisor_autosend", id)
	s, c, _ := partDOpen(t, root, "pretick")
	captureTokens21(t)
	if id == "parity_expired_lease" {
		TokenSource = &captureTokenReader21{next: 1}
	}
	h := &partDRecipientHost{partDHost: partDHost{sendHost{status: "idle"}}}
	if kind == 8 || kind == 10 || kind == 12 {
		h.archivedTask = "01supervisor-task"
	}
	if kind == 13 {
		h.archivedTask = "01second-supervisor"
	}
	if kind == 11 {
		h.archivedTasks = map[string]bool{"01sup-0": true, "01sup-1": true, "01sup-2": true, "01sup-3": true}
	}
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var extra struct{ Ticks []struct{ Now float64 } }
	if err = json.Unmarshal(raw, &extra); err != nil {
		t.Fatal(err)
	}
	var ticks []any
	var result AutoSendResult
	if kind == 13 {
		if err = s.DB.QueryRow("SELECT staged_at,message_id FROM supervisor_messages ORDER BY staged_at,message_id LIMIT 1").Scan(&result.AfterStagedAt, &result.AfterMessageID); err != nil {
			t.Fatal(err)
		}
	}
	if kind == 14 {
		result.AfterProject = "PRJ-1"
	}
	served := []string{}
	if kind == 13 || kind == 14 {
		partDInterleaveStore(t, s, kind, result.AfterMessageID, &served)
	}
	live := c.Linkage
	var firstHold string
	for i, tick := range extra.Ticks {
		if kind == 3 && i > 0 {
			c = &Channel{Store: s, Linkage: live, Program: c.Program, Settings: &delivery.TaskSettings{}}
			result = AutoSendResult{}
		}
		if kind == 7 && i == 0 {
			reading, err := live.Up(context.Background(), captureRelationID(t, c))
			if err != nil {
				t.Fatal(err)
			}
			reading["state"], reading["levels"] = "unresolved", []any{}
			c.Linkage = staticLinkage{reading}
		} else {
			c.Linkage = live
		}
		result = partDTick(t, c, h, tick.Now, projects, sends, result)
		ticks = append(ticks, partDTickValue(tick.Now, result))
		if kind == 7 && i == 0 {
			if err = s.DB.QueryRow("SELECT hold_reason FROM supervisor_messages LIMIT 1").Scan(&firstHold); err != nil {
				t.Fatal(err)
			}
		}
	}
	partDExtra(t, root, "ticks", ticks)
	if kind == 14 {
		// StageUnsent reads this scope once for omissions and once for events.
		stagedProjects := []string{}
		for i := 0; i < len(served); i += 2 {
			stagedProjects = append(stagedProjects, served[i])
		}
		partDExtra(t, root, "projects", stagedProjects)
	}
	got := []any{}
	switch kind {
	case 3:
		var n int
		if err = s.DB.QueryRow("SELECT count(*) FROM supervisor_messages").Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = []any{[]any{result.SupervisorStaged, result.SupervisorSent}, n, h.count("01supervisor-task")}
	case 7:
		got = []any{firstHold, []any{}, stateOf(t, s, 0), h.count("01supervisor-task")}
	case 8:
		got = []any{h.count("01second-supervisor"), []any{}}
	case 2:
		var journal int
		if err = s.DB.QueryRow("SELECT count(*) FROM journal WHERE kind LIKE 'supervisor%'").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		got = []any{[]any{result.SupervisorStaged, result.SupervisorSent}, journal}
	case 9:
		served := []string{}
		for _, value := range ticks {
			served = append(served, value.(map[string]any)["afterProject"].(string))
		}
		got = []any{served, true}
	case 11:
		got = []any{h.count("01sup-4")}
	}
	partDCompare(t, s, got, py)
}

func Test24_AUT_8_StrugglingWrapWholeLivePython(t *testing.T) {
	partDAutoReplay(t, "parity_struggling_wrap", 13, 0, 2)
}
func Test24_AUT_9_ProjectWrapWholeLivePython(t *testing.T) {
	partDAutoReplay(t, "parity_project_wrap", 14, 4, 2)
}
func Test24_AUT_3_ExpiredLeaseWholeLivePython(t *testing.T) {
	partDAutoReplay(t, "parity_expired_lease", 0, 4, 2)
}
func Test24_AUT_8_HeadWindowWholeLivePython(t *testing.T) {
	partDAutoReplay(t, "parity_head_window", 12, 4, 1)
}
func Test24_AUT_10_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "parity_deferred", 10, 4, 2)
}
func Test24_AUT_2_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "AParentThatNeverReports.test_a_quiet_store_ticks_without_touching_the_channel", 2, 4, 2)
}
func Test24_AUT_9_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "TheSupervisorPassReadsAndReportsWhatItDid.test_projects_are_read_a_page_at_a_time_and_rotate", 9, 1, 2)
}
func Test24_AUT_11_WholeLivePython(t *testing.T) {
	partDAutoReplay(t, "AWindowOfUnsendableHeadsDoesNotHideTheRest.test_a_later_supervisor_is_reached_behind_a_window_of_unsendable_ones", 11, 4, 1)
}

// Keep real SQL and real Attempt; interleave at the exact page boundary rather
// than relying on timing. A concurrent readdress can change which recipient a
// wrapped head belongs to after the forward page has been materialized.
var partDDriverID atomic.Int64

type partDDriver struct {
	sqlite.Driver
	kind    int
	message string
	served  *[]string
}
type partDConn struct {
	driver.Conn
	kind    int
	message string
	changed bool
	served  *[]string
}
type partDRows struct {
	driver.Rows
	close  func() error
	prefix []driver.Value
}

func (d *partDDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &partDConn{Conn: conn, kind: d.kind, message: d.message, served: d.served}, nil
}
func (c *partDConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *partDConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.kind == 14 && strings.Contains(query, "SELECT r.relationship_id FROM relationships r JOIN relationship_scope s") {
		*c.served = append(*c.served, args[0].Value.(string))
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if c.kind == 13 && strings.Contains(query, "SELECT m.message_id") && !c.changed {
		c.changed = true
		return &partDRows{Rows: rows, close: func() error {
			_, err := c.ExecContext(ctx, "UPDATE supervisor_messages SET recipient_task_id=?,state='sending',lease_until=0 WHERE message_id=?", []driver.NamedValue{{Ordinal: 1, Value: "01second-supervisor"}, {Ordinal: 2, Value: c.message}})
			return err
		}}, nil
	}
	if c.kind == 14 && strings.Contains(query, "DISTINCT project_key") && strings.Contains(query, " <= ") {
		return &partDRows{Rows: rows, prefix: []driver.Value{"PRJ-2"}}, nil
	}
	return rows, nil
}
func (r *partDRows) Next(dest []driver.Value) error {
	if r.prefix != nil {
		copy(dest, r.prefix)
		r.prefix = nil
		return nil
	}
	return r.Rows.Next(dest)
}
func (r *partDRows) Close() error {
	err := r.Rows.Close()
	if r.close != nil {
		fn := r.close
		r.close = nil
		err = errors.Join(err, fn())
	}
	return err
}
func partDInterleaveStore(t *testing.T, s *store.Store, kind int, message string, served *[]string) {
	t.Helper()
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("partd-interleave-%d", partDDriverID.Add(1))
	sql.Register(name, &partDDriver{kind: kind, message: message, served: served})
	db, err := sql.Open(name, s.Path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s.DB = db
}

func Test24_DIR_3_DifferentCorrelationsWholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_answers_to_two_messages_stand_together")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	_, first := pdRecord(t, r, l, "d-first", "relayed_decision", "msg-1", "01supervisor-task")
	_, second := pdRecord(t, r, l, "d-second", "relayed_decision", "msg-2", "01supervisor-task")
	partDCompare(t, s, []any{partDCode(first), partDCode(second), len(partDIDs(t, r)), partDContest(t, r)}, py)
}
func partDCode(err error) int {
	if err != nil {
		return 2
	}
	return 0
}
func Test24_DIR_5_PurposedBesideUnknownWholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "ARealConflictIsRefusedWhereItIsRecorded.test_a_purposed_instruction_beside_one_of_unknown_purpose_is_refused")
	s, _, r := partDOpen(t, root, "setup")
	l := partDLink(t, s)
	older, err := pdRecord(t, r, l, "d-legacy", "", "", "01supervisor-task")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pdRecord(t, r, l, "d-new", "relayed_decision", "msg-1", "01supervisor-task")
	refusal := partDRefusal(err)
	partDCompare(t, s, []any{partDCode(err), contains(refusal["detail"], did(older)), contains(refusal["detail"], "purpose"), partDIDs(t, r)}, py)
}
func Test24_DIR_9_CappedWholeLivePython(t *testing.T) {
	root, py := partDPython(t, "test_directive_places", "AHeldReportIsNamedWhereTheOperatorLooks.test_a_capped_report_is_still_called_held")
	s, c, r := partDOpen(t, root, "event")
	o := captureObligation4(t, c, s)
	staged, err := c.Stage(context.Background(), o, "", captureTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE supervisor_messages SET state='withheld_pre_send',hold_reason='attempt_cap' WHERE message_id=?", staged["messageId"]); err != nil {
		t.Fatal(err)
	}
	l := partDLink(t, s)
	for _, digest := range []string{"d-first", "d-second"} {
		if _, err = pdRecord(t, r, l, digest, "", "", "01supervisor-task"); err != nil {
			t.Fatal(err)
		}
	}
	standing, err := c.Standing(context.Background(), "PRJ-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	holds, err := c.ReportHolds(context.Background(), standing)
	if err != nil {
		t.Fatal(err)
	}
	partDExtra(t, root, "holds", []any{holds})
	ids := []any{}
	for _, hold := range holds {
		ids = append(ids, hold["obligationIds"])
	}
	partDCompare(t, s, []any{0, ids}, py)
}
