package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Fixture identities match Python's tests/support.py.
const (
	parent       = "01parent-task"
	child        = "01child-task"
	issue        = "REL-1"
	host         = "host-a"
	dispatchTurn = "turn-dispatch-1"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// pyRun is one Python scenario over tree: its out block, every non-empty table, and its sends.
type pyRun struct {
	Out    map[string]any              `json:"out"`
	Tables map[string][]map[string]any `json:"tables"`
	Sends  [][]any                     `json:"sends"`
}

// runPython drives testdata/scenarios/<name>.py through the real Python package over tree.
func runPython(t *testing.T, tree, name string, args ...string) pyRun {
	t.Helper()
	root := repoRoot(t)
	script, _ := filepath.Abs("testdata/pyscenario.py")
	cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", script, tree, name}, args...)...)
	cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "PYTHONPATH="+filepath.Join(root, "packages", "codex-session-relay", "src")+":"+filepath.Join(root, "packages", "codex-session-relay"))
	output, err := cmd.Output()
	if err != nil {
		stderr := ""
		if e, ok := err.(*exec.ExitError); ok {
			stderr = string(e.Stderr)
		}
		t.Fatalf("python scenario %s: %v\n%s", name, err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var run pyRun
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &run); err != nil {
		t.Fatalf("python scenario %s output: %v\n%s", name, err, output)
	}
	return run
}

// fixture is DeliveryTestCase: a registered relationship, a final event, a fake host.
type fixture struct {
	t        *testing.T
	ctx      context.Context
	tree     string
	root     string
	clock    *FakeClock
	store    *store.Store
	intake   store.ReceiptIntake
	delivery *Service
	host     *fakeHost
	rid      string
	// skipTables are left out of a table comparison (tableFilter).
	skipTables []string
}

// newFixture shares tree with a Python run when tree is non-empty; the Go store lives beside it.
func newFixture(t *testing.T, tree string) *fixture {
	t.Helper()
	if tree == "" {
		tree = t.TempDir()
	}
	root := filepath.Join(tree, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(tree, "xdg-state"))
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(tree, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clock := NewFakeClock()
	f := &fixture{t: t, ctx: ctx, tree: tree, root: root, clock: clock, store: s, host: newFakeHost(clock)}
	f.intake = store.ReceiptIntake{Store: s, Now: clock.ISO, Minimum: store.BestEffortDetection}
	f.delivery = NewService(s, clock)
	f.host.addThread(parent)
	f.host.addThread(child)
	return f
}

func (f *fixture) artifact(name, text string) string {
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func taskSettings(cwd string) string { return taskSettingsWith(cwd, "never") }

func taskSettingsWith(cwd, approval string) string {
	return dumpsSorted(Obj{
		{Key: "sandbox", Value: Obj{{Key: "type", Value: "workspaceWrite"}, {Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false}, {Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}}},
		{Key: "approvalPolicy", Value: approval}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}},
		{Key: "model", Value: "anthropic/claude-opus-5"}, {Key: "reasoningEffort", Value: "xhigh"},
		{Key: "environments", Value: []any{Obj{{Key: "environmentId", Value: "local"}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}}}}},
	})
}

type regOpts struct {
	issue, dispatchRequest string
	// other registers support's other_assignment endpoints (CrossAssignmentDelivery).
	parent, parentCwd, parentSession, child, childRoot, childSession, turn string
	recipients                                                             []string
	noSettings                                                             bool
	parentSettings                                                         string
	scopeRef                                                               any
	// parentOnlySettings records the parent's settings only (TwoParents.assignment).
	parentOnlySettings bool
}

// register writes what Registry.register and record_settings write for the fixture (the
// registration port is todo 25; these are the same rows, compared with Python's below).
func (f *fixture) register(o regOpts) string {
	if o.issue == "" {
		o.issue = issue
	}
	if o.dispatchRequest == "" {
		o.dispatchRequest = "dispatch-1"
	}
	if o.parent == "" {
		o.parent, o.parentCwd, o.parentSession, o.child, o.childRoot, o.childSession, o.turn = parent, "/parent", "cxc-parent", child, f.root, "cxc-child", dispatchTurn
	}
	if o.recipients == nil {
		o.recipients = []string{o.parent}
	}
	rid, err := store.RelationshipID(o.parent, o.child, o.issue)
	mustDo(f.t, err)
	now := f.clock.ISO()
	recipients := make([]any, len(o.recipients))
	for i, r := range o.recipients {
		recipients[i] = r
	}
	mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := execSQL(ctx, f.store, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, parent_cwd, parent_cxc_session, child_task_id, child_host_id, child_cwd, child_cxc_session, execution_generation, artifact_roots, allowed_recipients, scope_ref, supersedes, superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
			rid, o.issue, "active", o.parent, host, o.parentCwd, nullable(o.parentSession), o.child, host, o.childRoot, nullable(o.childSession), 1, dumps([]any{o.childRoot}), dumps(recipients), o.scopeRef, nil, now, now); err != nil {
			return err
		}
		if _, err := execSQL(ctx, f.store, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?,?,?,?,?,?,?,?)", rid, 1, o.dispatchRequest, "bound", o.turn, "initial_assignment", now, now); err != nil {
			return err
		}
		return journal(ctx, f.store, "relationship_registered", rid, Obj{{Key: "issueKey", Value: o.issue}}, now)
	}))
	switch {
	case o.parentSettings != "":
		_, err := execSQL(f.ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", parent, o.parentSettings, "test-raw", now)
		mustDo(f.t, err)
	case o.parentOnlySettings:
		mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
			if _, err := execSQL(ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", o.parent, taskSettings(o.parentCwd), "creation_result", now); err != nil {
				return err
			}
			return journal(ctx, f.store, "settings_recorded", o.parent, Obj{{Key: "source", Value: "creation_result"}}, now)
		}))
	case !o.noSettings && o.parent == parent:
		for _, task := range []struct{ id, cwd string }{{parent, "/parent"}, {child, f.root}} {
			mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
				if _, err := execSQL(ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET settings = excluded.settings, source = excluded.source, recorded_at = excluded.recorded_at", task.id, taskSettings(task.cwd), "creation_result", now); err != nil {
					return err
				}
				return journal(ctx, f.store, "settings_recorded", task.id, Obj{{Key: "source", Value: "creation_result"}}, now)
			}))
		}
	}
	if o.parent == parent {
		f.rid = rid
	}
	return rid
}

// otherAssignment is CrossAssignmentDelivery.other_assignment.
func (f *fixture) otherAssignment() string {
	root := filepath.Join(f.tree, "other-project")
	mustDo(f.t, os.MkdirAll(root, 0o755))
	return f.register(regOpts{issue: "REL-2", dispatchRequest: "dispatch-2", parent: "01other-parent", parentCwd: "/other", parentSession: "cxc-other", child: "01other-child", childRoot: root, childSession: "cxc-other-c", turn: "turn-dispatch-2", noSettings: true})
}

type turnRef struct{ thread, turn, status string }

func assigned(status string) turnRef { return turnRef{child, dispatchTurn, status} }

// readyPayload is support.ready_payload, in Python's key order.
func (f *fixture) readyPayload(rid string, generation int64, paths []string, attempt int, turn turnRef) Obj {
	entries, err := store.BuildManifest(paths, []string{f.root})
	mustDo(f.t, err)
	revision, err := store.ManifestRevision(entries)
	mustDo(f.t, err)
	event, err := store.EventID(rid, int(generation), revision, "ready_for_review", turn.turn, &attempt)
	mustDo(f.t, err)
	manifest := make([]any, len(entries))
	for i, e := range entries {
		manifest[i] = Obj{{Key: "path", Value: e.Path}, {Key: "sha256", Value: e.SHA256}, {Key: "bytes", Value: *e.Bytes}}
	}
	return Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: generation}, {Key: "attempt", Value: int64(attempt)}, {Key: "revisionHash", Value: revision}, {Key: "outcome", Value: "ready_for_review"}, {Key: "producer", Value: "child"},
		{Key: "turnRef", Value: Obj{{Key: "threadId", Value: turn.thread}, {Key: "turnId", Value: turn.turn}, {Key: "turnStatus", Value: turn.status}}}, {Key: "manifest", Value: manifest}, {Key: "emittedAt", Value: f.clock.ISO()}}
}

// executionPayload is support.execution_payload.
func (f *fixture) executionPayload(rid string, generation int64, outcome string, attempt int, turn turnRef) Obj {
	event, err := store.EventID(rid, int(generation), store.NoDeliverable, outcome, turn.turn, &attempt)
	mustDo(f.t, err)
	return Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: generation}, {Key: "attempt", Value: int64(attempt)}, {Key: "revisionHash", Value: store.NoDeliverable}, {Key: "outcome", Value: outcome}, {Key: "producer", Value: "child"},
		{Key: "turnRef", Value: Obj{{Key: "threadId", Value: turn.thread}, {Key: "turnId", Value: turn.turn}, {Key: "turnStatus", Value: turn.status}}}, {Key: "manifest", Value: nil}, {Key: "emittedAt", Value: f.clock.ISO()}}
}

func (f *fixture) accept(payload Obj, options store.AcceptOptions) (store.StoredReceipt, error) {
	turn, _ := get(payload, "turnRef")
	t := turn.(Obj)
	return f.intake.AcceptChildReceiptWith(f.ctx, []byte(dumps(payload)), store.TurnReference{ThreadID: str(t, "threadId"), TurnID: str(t, "turnId"), Status: str(t, "turnStatus")}, options)
}

// readyEvent is DeliveryTestCase.ready_event.
func (f *fixture) readyEvent(o regOpts) string {
	rid := f.register(o)
	payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	_, err := f.accept(payload, store.AcceptOptions{})
	mustDo(f.t, err)
	return str(payload, "eventId")
}

// queuedEvent is DeliveryTestCase.queued_event.
func (f *fixture) queuedEvent(o regOpts) string {
	event := f.readyEvent(o)
	_, err := f.delivery.Enqueue(f.ctx, event, "", "")
	mustDo(f.t, err)
	return event
}

func (f *fixture) attempt(event string, now *float64) (Obj, error) {
	return f.delivery.Attempt(f.ctx, event, f.host, now, "")
}

func (f *fixture) mustAttempt(event string, now *float64) Obj {
	f.t.Helper()
	record, err := f.attempt(event, now)
	mustDo(f.t, err)
	return record
}

func (f *fixture) row(event string) Row {
	f.t.Helper()
	row, err := f.delivery.Get(f.ctx, event)
	mustDo(f.t, err)
	return row
}

func (f *fixture) one(query string, args ...any) Row {
	f.t.Helper()
	row, err := one(f.ctx, f.store, query, args...)
	mustDo(f.t, err)
	return row
}

func (f *fixture) count(query string, args ...any) int64 {
	return f.one(query, args...).I("c")
}

func (f *fixture) eligible() []string {
	rows, err := f.delivery.Eligible(f.ctx, f.clock.Now(), 10, 0, 0, nil)
	mustDo(f.t, err)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.S("event_id"))
	}
	return ids
}

func at(v float64) *float64 { return &v }

// tables dumps every non-empty table of the Go store, as the Python harness dumps its own.
func (f *fixture) tables() map[string][]map[string]any {
	names, err := all(f.ctx, f.store, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name")
	mustDo(f.t, err)
	out := map[string][]map[string]any{}
	for _, n := range names {
		rows, err := all(f.ctx, f.store, "SELECT * FROM "+n.S("name")+" ORDER BY rowid")
		mustDo(f.t, err)
		if len(rows) == 0 || slices.Contains(f.skipTables, n.S("name")) {
			continue
		}
		for _, r := range rows {
			out[n.S("name")] = append(out[n.S("name")], map[string]any(r))
		}
	}
	return out
}

func normalizeJSON(t *testing.T, v any) any {
	raw, err := json.Marshal(v)
	mustDo(t, err)
	var out any
	mustDo(t, json.Unmarshal(raw, &out))
	return out
}

// requireSameTables compares the Go store with the Python one, every row of every table.
func requireSameTables(t *testing.T, f *fixture, python pyRun) {
	t.Helper()
	got := normalizeJSON(t, f.tables()).(map[string]any)
	want := normalizeJSON(t, python.Tables).(map[string]any)
	var names []string
	for n := range want {
		names = append(names, n)
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if !reflect.DeepEqual(got[n], want[n]) {
			g, _ := json.MarshalIndent(got[n], "", " ")
			w, _ := json.MarshalIndent(want[n], "", " ")
			t.Errorf("table %s differs from Python\ngo:     %s\npython: %s", n, g, w)
		}
	}
}

// requireSameJSON compares one Go value with the Python value of the same name.
func requireSameJSON(t *testing.T, what string, got any, want any) {
	t.Helper()
	g, w := normalizeJSON(t, jsonable(got)), normalizeJSON(t, want)
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		t.Errorf("%s differs from Python\ngo:     %s\npython: %s", what, gb, wb)
	}
}

// jsonable turns an Obj tree into maps for comparison.
func jsonable(v any) any {
	switch t := v.(type) {
	case Obj:
		if t == nil {
			return nil
		}
		m := map[string]any{}
		for _, f := range t {
			m[f.Key] = jsonable(f.Value)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonable(x)
		}
		return out
	case Row:
		return map[string]any(t)
	case map[string]any:
		m := map[string]any{}
		for k, x := range t {
			m[k] = jsonable(x)
		}
		return m
	case error:
		return map[string]any{"reason": Reason(t), "detail": Detail(t)}
	}
	return v
}

func refusalOf(err error) map[string]any {
	return map[string]any{"reason": Reason(err), "detail": Detail(err)}
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	if Reason(err) != reason {
		t.Fatalf("want refusal %s, got %v", reason, err)
	}
}

func sendsJSON(h *fakeHost) []any {
	out := []any{}
	for _, s := range h.sends {
		out = append(out, []any{s.requestID, s.thread, s.message, s.outcome})
	}
	return out
}

// setStatus is registry.set_status for an unscoped relationship: the same rows Python writes.
func (f *fixture) setStatus(status string) { f.setStatusBy(status, "user") }

func (f *fixture) setStatusBy(status, actor string) {
	now := f.clock.ISO()
	mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := execSQL(ctx, f.store, "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?", status, now, f.rid); err != nil {
			return err
		}
		return journal(ctx, f.store, "status_changed", f.rid, Obj{{Key: "status", Value: status}, {Key: "actor", Value: actor}}, now)
	}))
}

// resume is registry.resume restating the current scope.
func (f *fixture) resume() {
	f.setStatus("active")
}

// supersede is registry.supersede.
func (f *fixture) supersede(successor string) {
	now := f.clock.ISO()
	mustDo(f.t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := execSQL(ctx, f.store, "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ? WHERE relationship_id = ?", successor, now, f.rid); err != nil {
			return err
		}
		return journal(ctx, f.store, "superseded", f.rid, Obj{{Key: "by", Value: successor}}, now)
	}))
}

func boolp(b bool) *bool { return &b }

// counted wraps the fake host and counts every pre-claim read (CountedHostReads).
type counted struct {
	*fakeHost
	calls []string
}

func (c *counted) ReadThread(t string) (ThreadFacts, error) {
	c.calls = append(c.calls, "read_thread")
	return c.fakeHost.ReadThread(t)
}
func (c *counted) IsArchived(t string, cwd any) (*bool, error) {
	c.calls = append(c.calls, "is_archived")
	return c.fakeHost.IsArchived(t, cwd)
}
func (c *counted) ReadGoalStatus(t string) (any, error) {
	c.calls = append(c.calls, "read_goal_status")
	return c.fakeHost.ReadGoalStatus(t)
}
func (c *counted) ListTurnIDs(t string, limit int) ([]string, error) {
	c.calls = append(c.calls, "list_turn_ids")
	return c.fakeHost.ListTurnIDs(t, limit)
}

// correctionAfterNeedsChanges is support.correction_after_needs_changes.
func (f *fixture) correctionAfterNeedsChanges() (string, string) {
	event := f.queuedEvent(regOpts{recipients: []string{parent, child}})
	f.mustAttempt(event, nil)
	f.clock.Advance(5)
	turn := f.host.startTurn(parent, "ack-turn", "inProgress", "")
	ack := NewAck(f.delivery)
	_, err := ack.Acknowledge(f.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, f.host)
	mustDo(f.t, err)
	_, err = ack.RecordVerdict(f.ctx, event, "needs_changes", "v1", nil, []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: "fix the shape"}}}, nil, nil)
	mustDo(f.t, err)
	correction := f.one("SELECT event_id FROM deliveries WHERE relationship_id = ? AND kind = ?", f.rid, Revision).S("event_id")
	f.clock.Advance(1)
	return event, correction
}

// requireSameTablesExcept compares every table but the named ones.
func requireSameTablesExcept(t *testing.T, f *fixture, python pyRun, skip ...string) {
	t.Helper()
	for _, name := range skip {
		delete(python.Tables, name)
	}
	g := &tableFilter{f, skip}
	requireSameTables(t, g.fixture(), python)
}

type tableFilter struct {
	f    *fixture
	skip []string
}

func (tf *tableFilter) fixture() *fixture {
	c := *tf.f
	c.skipTables = tf.skip
	return &c
}

// pythonValue runs one line of Python against the package and returns its stdout, trimmed.
func pythonValue(t *testing.T, script string) string {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", script)
	cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home)
	out, err := cmd.Output()
	mustDo(t, err)
	return strings.TrimSpace(string(out))
}

// rawSettings is json.dumps(task_settings(cwd, approvalPolicy=...)) in its insertion order, as
// support.register writes a raw settings row.
func rawSettings(cwd, approval string) string {
	return dumps(Obj{
		{Key: "sandbox", Value: Obj{{Key: "type", Value: "workspaceWrite"}, {Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false}, {Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}}},
		{Key: "approvalPolicy", Value: approval}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}},
		{Key: "model", Value: "anthropic/claude-opus-5"}, {Key: "reasoningEffort", Value: "xhigh"},
		{Key: "environments", Value: []any{Obj{{Key: "environmentId", Value: "local"}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}}}}},
	})
}
