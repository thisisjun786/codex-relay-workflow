package capacity

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fixture of test_edit_regions.py's EditRegionTestCase: PRJ-A/B/Z bound to alpha, beta and
// zeta, peer links A-B (pair) and A-Z (other), FakeClock(1_700_000_000). The peer link rows are
// the ones linkage.register_peer writes (todo 26 ports the method).
const (
	repo          = "owner/repo"
	rev           = "rev-1"
	zeta          = "task-zeta"
	betaCondition = "beta publishes parse() and restates this before renaming it"
	constraintAt  = "keep parse() at src/a.py lines 12-13 at rev-1"
	liveQuery     = "SELECT agreement_id FROM edit_agreements  WHERE state IN ('proposed','agreed','reopened') AND superseded_by IS NULL"
)

type regionEnv struct {
	*env
	regions     *EditRegions
	pair, other string
}

func newRegionEnv(t *testing.T) *regionEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	r := &registry.Registry{Store: s, Now: c.iso}
	for _, b := range [][4]string{{"PRJ-A", alpha, "host-a", "/alpha"}, {"PRJ-B", beta, "host-b", "/beta"}, {"PRJ-Z", zeta, "host-z", "/zeta"}} {
		if _, err := r.BindScope(ctx(), "parent", b[0], registry.Endpoint{TaskID: b[1], HostID: b[2], Cwd: ns(b[3])}); err != nil {
			t.Fatal(err)
		}
	}
	e := &regionEnv{env: &env{t: t, store: s, clock: c}, regions: &EditRegions{Store: s, Now: c.iso}}
	e.pair = e.peer("PRJ-A", alpha, "PRJ-B", beta)
	e.other = e.peer("PRJ-A", alpha, "PRJ-Z", zeta)
	return e
}

// peer writes register_peer's link row and journal entry.
func (e *regionEnv) peer(left, leftTask, right, rightTask string) string {
	id := "lnk-" + sha256Hex("peer|project|" + left + "|project|" + right)[:32]
	if err := e.store.InsertScopeLink(ctx(), store.ScopeLinksRow{LinkID: id, LinkKind: "peer", UpperKind: "project", UpperKey: left,
		UpperTaskID: leftTask, LowerKind: "project", LowerKey: right, LowerTaskID: rightTask, Status: "active", Revision: 1,
		CreatedAt: e.clock.iso(), UpdatedAt: e.clock.iso()}); err != nil {
		e.t.Fatal(err)
	}
	if err := journal(ctx(), e.store, "scope_linked", id, contract.OrderedObject{{Key: "kind", Value: "peer"},
		{Key: "upper", Value: left}, {Key: "lower", Value: right}, {Key: "revision", Value: 1}}, e.clock.iso()); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// handover applies linkage.handover's row writes for a parent scope (todo 26 ports the method).
func (e *regionEnv) handover(project, from, to, host string) {
	e.t.Helper()
	now := e.clock.iso()
	oldID := "bnd-" + sha256Hex("parent|project|" + project + "|" + from)[:32]
	newID := "bnd-" + sha256Hex("parent|project|" + project + "|" + to)[:32]
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE scope_bindings SET status = 'archived', superseded_by = ?, updated_at = ? WHERE binding_id = ?", []any{newID, now, oldID}},
		{"INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session, status, revision," +
			" supersedes, superseded_by, handover_note, created_at, updated_at) VALUES (?,'parent','project',?,?,?,NULL,NULL,'active',2,?,NULL,'changed hands',?,?)",
			[]any{newID, project, to, host, oldID, now, now}},
		{"UPDATE scope_links SET lower_task_id = ?, revision = revision + 1, updated_at = ? WHERE lower_kind = 'project' AND lower_key = ?" +
			"   AND lower_task_id = ? AND status IN ('active','paused')", []any{to, now, project, from}},
		{"UPDATE scope_links SET upper_task_id = ?, revision = revision + 1, updated_at = ? WHERE upper_kind = 'project' AND upper_key = ?" +
			"   AND upper_task_id = ? AND status IN ('active','paused')", []any{to, now, project, from}},
	} {
		if _, err := e.store.DB.ExecContext(ctx(), statement.query, statement.args...); err != nil {
			e.t.Fatal(err)
		}
	}
}

type proposeOpt struct {
	kind, key, class, regen, link, right, revision, task string
}

func (e *regionEnv) propose(p string, o proposeOpt) any {
	if o.kind == "" {
		o.kind = "file"
	}
	if o.class == "" {
		o.class = "source"
	}
	if o.link == "" {
		o.link = e.pair
	}
	if o.right == "" {
		o.right = "PRJ-B"
	}
	if o.revision == "" {
		o.revision = rev
	}
	if o.task == "" {
		o.task = alpha
	}
	regen := sql.NullString{String: o.regen, Valid: o.regen != ""}
	return e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: o.revision, Path: p, RegionKind: o.kind,
		RegionKey: o.key, RegionClass: o.class, RegenerateFrom: regen, LeftProject: "PRJ-A", RightProject: o.right,
		PeerLinkID: o.link, ProposerTaskID: o.task, ConstraintText: "keep the public signature", IssueKey: ns("CRW-1"),
		NextOwner: ns("task-beta")}))
}

func (e *regionEnv) byBeta() any {
	return e.step(e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/a.py", RegionKind: "file",
		LeftProject: "PRJ-B", RightProject: "PRJ-A", PeerLinkID: e.pair, ProposerTaskID: beta, ConstraintText: constraintAt,
		Condition: ns(betaCondition), IssueKey: ns("CRW-1"), NextOwner: ns(alpha)}))
}

func null() sql.NullString { return sql.NullString{} }

func (e *regionEnv) settle(agreement any, actor, disposition string, condition, reason sql.NullString) any {
	return e.step(e.regions.Settle(ctx(), id(agreement), actor, disposition, condition, reason))
}

func (e *regionEnv) agreed(p string) any {
	record := e.propose(p, proposeOpt{})
	e.settle(record, beta, "accepted", null(), null())
	return record
}

func (e *regionEnv) restate(from, to, actor string) any {
	return e.step(e.regions.RestateRevision(ctx(), repo, from, to, actor))
}

func (e *regionEnv) moved(revisions ...string) {
	previous := rev
	for _, r := range revisions {
		e.restate(previous, r, beta)
		previous = r
	}
}

func (e *regionEnv) reaffirm(agreement any, actor, revision string, condition sql.NullString) any {
	return e.step(e.regions.Reaffirm(ctx(), id(agreement), actor, revision, condition))
}

func (e *regionEnv) agreement(agreement any) any {
	value, err := e.regions.Agreement(ctx(), id(agreement))
	if value == nil && err == nil {
		return e.step(nil, nil)
	}
	return e.step(value, err)
}

func (e *regionEnv) show(f ShowFilter) any { return e.step(e.regions.Show(ctx(), repo, f)) }

func (e *regionEnv) current(revision string) any {
	return e.step(e.regions.CurrentRevision(ctx(), repo, revision))
}

func (e *regionEnv) followup(in Followup) any {
	if in.Trigger == "" {
		in.Trigger = "t"
	}
	if in.Acceptance == "" {
		in.Acceptance = "a"
	}
	if in.RecordedBy == "" {
		in.RecordedBy = alpha
	}
	return e.step(e.regions.Followup(ctx(), in))
}

func (e *regionEnv) accept(item any, actor, project string) any {
	return e.step(e.regions.AcceptFollowup(ctx(), id(item), actor, project))
}

func (e *regionEnv) settleFollowup(item any, actor, disposition string) any {
	return e.step(e.regions.SettleFollowup(ctx(), id(item), actor, disposition, null()))
}

// racing is test_edit_regions' racing(): concurrent runs once, between reaffirm's validation
// transaction and the carry's write.
func (e *regionEnv) racing(concurrent func()) {
	e.regions.beforeCarry = func() {
		e.regions.beforeCarry = nil
		concurrent()
	}
}

// id reads the agreementId or followupId of a recorded answer, or passes a literal id through.
func id(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case contract.OrderedObject:
		for _, key := range []string{"agreementId", "followupId"} {
			if s, ok := get(x, key).(string); ok && x[0].Key == key {
				return s
			}
		}
	}
	return ""
}

var pythonRegions = sync.OnceValues(func() (map[string][]map[string]any, error) {
	raw, err := os.ReadFile("testdata/python_editregion.json")
	if err != nil {
		return nil, err
	}
	var out map[string][]map[string]any
	return out, json.Unmarshal(raw, &out)
})

// sameAsPython compares every step with the Python scenario of the same name, whole.
func (e *regionEnv) sameAsPython(name string) {
	e.t.Helper()
	all, err := pythonRegions()
	if err != nil {
		e.t.Fatal(err)
	}
	want, ok := all[name]
	if !ok {
		e.t.Fatalf("no python scenario %q", name)
	}
	if len(e.steps) != len(want) {
		for i, s := range e.steps {
			e.t.Logf("go step %d: %.300v", i, s)
		}
		e.t.Fatalf("%s: %d steps, python has %d", name, len(e.steps), len(want))
	}
	for i := range want {
		g, _ := json.Marshal(e.steps[i])
		w, _ := json.Marshal(want[i])
		if !bytes.Equal(g, w) {
			e.t.Errorf("%s step %d differs from Python\n go: %v\n py: %v", name, i, e.steps[i], want[i])
		}
	}
}

// ok is the answer the step at index i recorded, decoded (the tests' own assertions read it).
func (e *regionEnv) ok(i int) map[string]any {
	e.t.Helper()
	text, isOK := e.steps[i]["ok"].(string)
	if !isOK {
		e.t.Fatalf("step %d was refused: %v", i, e.steps[i])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		e.t.Fatalf("step %d: %v", i, err)
	}
	return out
}

func (e *regionEnv) detail(i int) string {
	e.t.Helper()
	refused, isRefused := e.steps[i]["refused"].(map[string]any)
	if !isRefused {
		e.t.Fatalf("step %d was not refused: %v", i, e.steps[i])
	}
	return refused["detail"].(string)
}

func readSource(name string) (string, error) {
	raw, err := os.ReadFile(name)
	return string(raw), err
}
