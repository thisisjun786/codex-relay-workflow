package dag

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Queryer is what a reader needs of the database: the store's connection, a transaction's, or the
// read-only handle that creates no sidecar.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Repo reads and writes plans in the relay store. Its writes are single store transactions
// (BEGIN IMMEDIATE), so a revision is visible whole or not at all.
type Repo struct {
	Store *store.Store
	// Now is the clock of the rows' timestamps; registry.SystemISO when nil.
	Now func() string
}

func (r *Repo) now() string {
	if r.Now != nil {
		return r.Now()
	}
	return registry.SystemISO()
}

// Result is what Put answers: the revision the request produced or, for a repeated request, the one it
// produced the first time.
type Result struct {
	PlanID           string
	ProjectKey       string
	RevisionNo       int64
	ParentRevisionNo int64
	RequestID        string
	RequestDigest    string
	StateDigest      string
	CoordinatorEpoch int64
	AuthorTaskID     string
	RecordedAt       string
	Replayed         bool
	// NodeDigests are the slice digests of the plan's nodes at RevisionNo, by node id.
	NodeDigests map[string]string
}

// isMissingZone is a read of a store that has no DAG zone (a store no write open has reached since
// the zone shipped, or one a read-only handle opened).
func isMissingZone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: dag_")
}

func scanOne(ctx context.Context, q Queryer, query string, args []any, dest ...any) (bool, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	if err := rows.Scan(dest...); err != nil {
		return false, err
	}
	return true, rows.Err()
}

func loadHeader(ctx context.Context, q Queryer, planID string) (project string, found bool, err error) {
	found, err = scanOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{planID}, &project)
	if isMissingZone(err) {
		return "", false, nil
	}
	return project, found, err
}

func headRevision(ctx context.Context, q Queryer, planID string) (int64, error) {
	var head int64
	_, err := scanOne(ctx, q, "SELECT COALESCE(MAX(revision_no), 0) FROM dag_plan_revisions WHERE plan_id = ?", []any{planID}, &head)
	return head, err
}

const revisionColumns = "plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, coordinator_epoch, author_task_id, recorded_at"

func scanEvent(rows *sql.Rows) (Event, error) {
	var ev Event
	var changes string
	if err := rows.Scan(&ev.PlanID, &ev.RevisionNo, &ev.ParentRevisionNo, &ev.RequestID, &ev.RequestDigest, &changes, &ev.StateDigest, &ev.CoordinatorEpoch, &ev.AuthorTaskID, &ev.RecordedAt); err != nil {
		return Event{}, err
	}
	decoded, err := DecodeChanges(changes)
	if err != nil {
		return Event{}, err
	}
	ev.Changes = decoded
	return ev, nil
}

func queryEvents(ctx context.Context, q Queryer, query string, args ...any) ([]Event, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func revisionByRequest(ctx context.Context, q Queryer, planID, requestID string) (Event, bool, error) {
	events, err := queryEvents(ctx, q, "SELECT "+revisionColumns+" FROM dag_plan_revisions WHERE plan_id = ? AND request_id = ?", planID, requestID)
	if isMissingZone(err) || len(events) == 0 {
		if isMissingZone(err) {
			err = nil
		}
		return Event{}, false, err
	}
	return events[0], true, err
}

func parseAuthority(text string) ([]string, error) {
	if text == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, &CorruptError{Detail: "a stored required_authority is not a JSON list of strings"}
	}
	return out, nil
}

// loadState reads every node and edge row of a plan (live and retired) as the fold left it.
func loadState(ctx context.Context, q Queryer, planID, project string, head int64) (State, error) {
	st := State{PlanID: planID, ProjectKey: project, Revision: head}
	nodes, err := q.QueryContext(ctx, "SELECT node_id, introduced_rev, COALESCE(retired_rev, 0), slice_digest, issue_key, node_kind, COALESCE(title, ''), criteria_set_digest, COALESCE(supersedes_node_id, '') FROM dag_nodes WHERE plan_id = ? ORDER BY introduced_rev, node_id", planID)
	if err != nil {
		return st, err
	}
	for nodes.Next() {
		var n NodeVersion
		if err := nodes.Scan(&n.NodeID, &n.IntroducedRev, &n.RetiredRev, &n.SliceDigest, &n.IssueKey, &n.Kind, &n.Title, &n.CriteriaSetDigest, &n.SupersedesNodeID); err != nil {
			_ = nodes.Close()
			return st, err
		}
		st.Nodes = append(st.Nodes, n)
	}
	if err := errors.Join(nodes.Err(), nodes.Close()); err != nil {
		return st, err
	}
	edges, err := q.QueryContext(ctx, "SELECT edge_id, introduced_rev, COALESCE(retired_rev, 0), from_node_id, to_node_id, kind, COALESCE(target_repository, ''), COALESCE(target_base_ref, ''), pins_code_head, COALESCE(decision_subject, ''), COALESCE(decision_digest, ''), COALESCE(required_authority, '') FROM dag_edges WHERE plan_id = ? ORDER BY introduced_rev, edge_id", planID)
	if err != nil {
		return st, err
	}
	for edges.Next() {
		var e EdgeRow
		var pins int
		var authority string
		if err := edges.Scan(&e.EdgeID, &e.IntroducedRev, &e.RetiredRev, &e.FromNodeID, &e.ToNodeID, &e.Kind, &e.TargetRepository, &e.TargetBaseRef, &pins, &e.DecisionSubject, &e.DecisionDigest, &authority); err != nil {
			_ = edges.Close()
			return st, err
		}
		e.PinsCodeHead = pins == 1
		if e.RequiredAuthority, err = parseAuthority(authority); err != nil {
			_ = edges.Close()
			return st, err
		}
		st.Edges = append(st.Edges, e)
	}
	return st, errors.Join(edges.Err(), edges.Close())
}

// Put appends a revision to a plan, in one store transaction:
//
//  1. a request id the plan already recorded returns that revision (Replayed) when the request is the
//     same one, and conflicts when it is another;
//  2. an expected parent that is not the plan's head conflicts (the writer lost a race);
//  3. the changes are folded onto the head and the plan they produce is validated;
//  4. only then is anything written: the plan's header (first revision), the revision, and the rows of the fold.
//
// A request that fails at any step leaves every table as it was.
func (r *Repo) Put(ctx context.Context, rev Revision) (Result, error) {
	rev, err := Checked(rev)
	if err != nil {
		return Result{}, err
	}
	digest := RequestDigest(rev)
	var result Result
	err = r.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if existing, found, err := revisionByRequest(ctx, conn, rev.PlanID, rev.RequestID); err != nil {
			return err
		} else if found {
			if existing.RequestDigest != digest {
				return conflict("request id %s already produced revision %d of plan %s for a different request; a request id names one request", rev.RequestID, existing.RevisionNo, rev.PlanID)
			}
			project, hasHeader, err := loadHeader(ctx, conn, rev.PlanID)
			if err != nil {
				return err
			}
			if !hasHeader {
				return &CorruptError{Detail: fmt.Sprintf("plan %s has revisions and no header", rev.PlanID)}
			}
			head, err := headRevision(ctx, conn, rev.PlanID)
			if err != nil {
				return err
			}
			// The stored result is answered only from rows that still agree with the log: the same checks a read makes.
			_, snap, err := verifiedState(ctx, conn, rev.PlanID, project, head, existing.RevisionNo)
			if err != nil {
				return err
			}
			result = resultOf(existing, project, snap, true)
			return nil
		}
		project, exists, err := loadHeader(ctx, conn, rev.PlanID)
		if err != nil {
			return err
		}
		head, err := headRevision(ctx, conn, rev.PlanID)
		if err != nil {
			return err
		}
		if exists && head == 0 {
			return &CorruptError{Detail: fmt.Sprintf("plan %s has a header and no revision", rev.PlanID)}
		}
		if !exists && head > 0 {
			return &CorruptError{Detail: fmt.Sprintf("plan %s has revisions and no header", rev.PlanID)}
		}
		if rev.ExpectedParent != head {
			return conflict("the request expects parent revision %d, but plan %s is at revision %d", rev.ExpectedParent, rev.PlanID, head)
		}
		st := State{PlanID: rev.PlanID, ProjectKey: project, Revision: head}
		if exists {
			// The next revision is built only on rows that agree with the log, so it never carries a damaged
			// plan forward under digests that look right.
			if st, _, err = verifiedState(ctx, conn, rev.PlanID, project, head, head); err != nil {
				return err
			}
		}
		next, diff, violations := Apply(st, rev)
		if len(violations) > 0 {
			return &PlanRejected{Violations: violations}
		}
		snap := next.At(next.Revision)
		now := r.now()
		if !exists {
			if _, err := conn.ExecContext(ctx, "INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?, ?, ?, ?)", rev.PlanID, rev.ProjectKey, rev.AuthorTaskID, now); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO dag_plan_revisions ("+revisionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			rev.PlanID, next.Revision, head, rev.RequestID, digest, ChangesJSON(rev.Changes), snap.StateDigest, rev.CoordinatorEpoch, rev.AuthorTaskID, now); err != nil {
			return err
		}
		if err := writeDiff(ctx, conn, rev.PlanID, next.Revision, diff); err != nil {
			return err
		}
		result = resultOf(Event{PlanID: rev.PlanID, RevisionNo: next.Revision, ParentRevisionNo: head, RequestID: rev.RequestID, RequestDigest: digest,
			CoordinatorEpoch: rev.CoordinatorEpoch, AuthorTaskID: rev.AuthorTaskID, RecordedAt: now, StateDigest: snap.StateDigest}, st.ProjectKey, snap, false)
		if !exists {
			result.ProjectKey = rev.ProjectKey
		}
		return nil
	})
	return result, err
}

func resultOf(ev Event, project string, snap Snapshot, replayed bool) Result {
	digests := map[string]string{}
	for _, n := range snap.Nodes {
		digests[n.NodeID] = n.SliceDigest
	}
	return Result{PlanID: ev.PlanID, ProjectKey: project, RevisionNo: ev.RevisionNo, ParentRevisionNo: ev.ParentRevisionNo, RequestID: ev.RequestID,
		RequestDigest: ev.RequestDigest, StateDigest: ev.StateDigest, CoordinatorEpoch: ev.CoordinatorEpoch, AuthorTaskID: ev.AuthorTaskID,
		RecordedAt: ev.RecordedAt, Replayed: replayed, NodeDigests: digests}
}

func authorityText(a []string) any {
	if len(a) == 0 {
		return nil
	}
	return canonical(authorityList(a))
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// writeDiff writes the rows of a fold: versions and edges that left are retired first, then the new
// ones are inserted (the partial unique index of live node rows is checked per statement).
func writeDiff(ctx context.Context, conn *sql.Conn, planID string, rev int64, d Diff) error {
	for _, n := range d.RetireNodes {
		res, err := conn.ExecContext(ctx, "UPDATE dag_nodes SET retired_rev = ? WHERE plan_id = ? AND node_id = ? AND introduced_rev = ? AND retired_rev IS NULL", rev, planID, n.NodeID, n.IntroducedRev)
		if err != nil {
			return err
		}
		if changed, err := res.RowsAffected(); err != nil || changed != 1 {
			return fmt.Errorf("retiring node %s (introduced at revision %d) changed %d rows (%v)", n.NodeID, n.IntroducedRev, changed, err)
		}
	}
	for _, e := range d.RetireEdges {
		res, err := conn.ExecContext(ctx, "UPDATE dag_edges SET retired_rev = ? WHERE plan_id = ? AND edge_id = ? AND retired_rev IS NULL", rev, planID, e.EdgeID)
		if err != nil {
			return err
		}
		if changed, err := res.RowsAffected(); err != nil || changed != 1 {
			return fmt.Errorf("retiring edge %s changed %d rows (%v)", e.EdgeID, changed, err)
		}
	}
	for _, n := range d.InsertNodes {
		if _, err := conn.ExecContext(ctx, "INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, title, criteria_set_digest, supersedes_node_id) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)",
			planID, n.NodeID, n.IntroducedRev, n.SliceDigest, n.IssueKey, n.Kind, nullable(n.Title), n.CriteriaSetDigest, nullable(n.SupersedesNodeID)); err != nil {
			return err
		}
	}
	for _, e := range d.InsertEdges {
		pins := 0
		if e.PinsCodeHead {
			pins = 1
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind, target_repository, target_base_ref, pins_code_head, decision_subject, decision_digest, required_authority) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			planID, e.EdgeID, e.IntroducedRev, e.FromNodeID, e.ToNodeID, e.Kind, nullable(e.TargetRepository), nullable(e.TargetBaseRef), pins, nullable(e.DecisionSubject), nullable(e.DecisionDigest), authorityText(e.RequiredAuthority)); err != nil {
			return err
		}
	}
	return nil
}

// readTx runs fn on one connection inside one deferred read transaction, so every query of fn sees the
// same committed state (a WAL snapshot) however many revisions commit meanwhile.
func (r *Repo) readTx(ctx context.Context, fn func(Queryer) error) (err error) {
	conn, err := r.Store.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() { _, e := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err = errors.Join(err, e) }()
	return fn(conn)
}

// Snapshot is the plan as of revision rev (0 = the head) and the head revision, read in one snapshot and
// verified: slice digests and the state digest are recomputed from the rows and compared with what was
// stored, so a plan that does not agree with itself is a *CorruptError and never a partial plan.
func (r *Repo) Snapshot(ctx context.Context, planID string, rev int64) (snap Snapshot, head int64, err error) {
	err = r.readTx(ctx, func(q Queryer) error {
		snap, head, err = SnapshotAt(ctx, q, planID, rev)
		return err
	})
	return snap, head, err
}

// SnapshotAt is Snapshot on any reader.
func SnapshotAt(ctx context.Context, q Queryer, planID string, rev int64) (Snapshot, int64, error) {
	project, found, err := loadHeader(ctx, q, planID)
	if err != nil {
		return Snapshot{}, 0, err
	}
	if !found {
		return Snapshot{}, 0, notFound("no plan %s is registered", planID)
	}
	head, err := headRevision(ctx, q, planID)
	if err != nil {
		return Snapshot{}, 0, err
	}
	if head == 0 {
		return Snapshot{}, 0, &CorruptError{Detail: fmt.Sprintf("plan %s has a header and no revision", planID)}
	}
	if rev == 0 {
		rev = head
	}
	if rev < 1 || rev > head {
		return Snapshot{}, head, notFound("plan %s has no revision %d; its revisions are 1 to %d", planID, rev, head)
	}
	_, snap, err := verifiedState(ctx, q, planID, project, head, rev)
	if err != nil {
		return Snapshot{}, 0, err
	}
	return snap, head, nil
}

// verifiedState reads the rows of a plan whose head is head and the plan as of revision rev, and
// checks them against the log: every node's slice digest is recomputed from the edges beside it, and
// the plan's state digest must be the one the revision recorded. Rows that disagree with the log are
// a *CorruptError and never a plan, for a read and for a write alike.
func verifiedState(ctx context.Context, q Queryer, planID, project string, head, rev int64) (State, Snapshot, error) {
	st, err := loadState(ctx, q, planID, project, head)
	if err != nil {
		return State{}, Snapshot{}, err
	}
	snap := st.At(rev)
	if err := verifySlices(snap); err != nil {
		return State{}, Snapshot{}, err
	}
	events, err := queryEvents(ctx, q, "SELECT "+revisionColumns+" FROM dag_plan_revisions WHERE plan_id = ? AND revision_no = ?", planID, rev)
	if err != nil {
		return State{}, Snapshot{}, err
	}
	if len(events) != 1 {
		return State{}, Snapshot{}, &CorruptError{Detail: fmt.Sprintf("plan %s has no revision row %d", planID, rev)}
	}
	if events[0].StateDigest != snap.StateDigest {
		return State{}, Snapshot{}, &CorruptError{Detail: fmt.Sprintf("the rows of plan %s at revision %d digest to %s, but the revision recorded %s", planID, rev, snap.StateDigest, events[0].StateDigest)}
	}
	return st, snap, nil
}

// verifySlices recomputes each node's slice digest from the edges live beside it.
func verifySlices(s Snapshot) error {
	incoming := map[string][]Edge{}
	for _, e := range s.Edges {
		incoming[e.ToNodeID] = append(incoming[e.ToNodeID], e.Edge)
	}
	for _, n := range s.Nodes {
		if got := SliceDigest(n.Node, incoming[n.NodeID]); got != n.SliceDigest {
			return &CorruptError{Detail: fmt.Sprintf("node %s of plan %s at revision %d digests to %s, but %s was stored", n.NodeID, s.PlanID, s.Revision, got, n.SliceDigest)}
		}
	}
	return nil
}

// Page is a run of the log after a cursor.
type Page struct {
	PlanID string
	After  int64
	Head   int64
	// Cursor is where the next read resumes: the last event returned, else After.
	Cursor int64
	Events []Event
}

// Events reads the log after revision cursor, at most limit events (1..MaxPage), in order.
func (r *Repo) Events(ctx context.Context, planID string, after int64, limit int) (page Page, err error) {
	err = r.readTx(ctx, func(q Queryer) error {
		page, err = EventsAfter(ctx, q, planID, after, limit)
		return err
	})
	return page, err
}

// MaxPage bounds one page of the log.
const MaxPage = 1000

// EventsAfter is Events on any reader.
func EventsAfter(ctx context.Context, q Queryer, planID string, after int64, limit int) (Page, error) {
	if _, found, err := loadHeader(ctx, q, planID); err != nil {
		return Page{}, err
	} else if !found {
		return Page{}, notFound("no plan %s is registered", planID)
	}
	head, err := headRevision(ctx, q, planID)
	if err != nil {
		return Page{}, err
	}
	if after < 0 || after > head {
		return Page{}, notFound("plan %s has no revision %d to resume after; its revisions are 1 to %d", planID, after, head)
	}
	if limit < 1 || limit > MaxPage {
		limit = MaxPage
	}
	events, err := queryEvents(ctx, q, "SELECT "+revisionColumns+" FROM dag_plan_revisions WHERE plan_id = ? AND revision_no > ? ORDER BY revision_no LIMIT ?", planID, after, limit)
	if err != nil {
		return Page{}, err
	}
	page := Page{PlanID: planID, After: after, Head: head, Cursor: after, Events: events}
	if n := len(events); n > 0 {
		page.Cursor = events[n-1].RevisionNo
	}
	return page, nil
}

// VerifyLog replays the whole log from the empty plan with every rule (history included, so an id
// that was retired is judged not to have been reused) and requires each revision's recorded state
// digest and the rows the fold left to be exactly what the replay produces.
func (r *Repo) VerifyLog(ctx context.Context, planID string) error {
	return r.readTx(ctx, func(q Queryer) error { return VerifyLogOn(ctx, q, planID) })
}

// VerifyLogOn is VerifyLog on any reader.
func VerifyLogOn(ctx context.Context, q Queryer, planID string) error {
	project, found, err := loadHeader(ctx, q, planID)
	if err != nil {
		return err
	}
	if !found {
		return notFound("no plan %s is registered", planID)
	}
	var events []Event
	for after := int64(0); ; {
		page, err := EventsAfter(ctx, q, planID, after, MaxPage)
		if err != nil {
			return err
		}
		events = append(events, page.Events...)
		if len(page.Events) == 0 || page.Cursor >= page.Head {
			break
		}
		after = page.Cursor
	}
	st := State{PlanID: planID, ProjectKey: project}
	for _, ev := range events {
		if ev.ParentRevisionNo != st.Revision || ev.RevisionNo != st.Revision+1 {
			return &CorruptError{Detail: fmt.Sprintf("revision %d of plan %s does not follow revision %d", ev.RevisionNo, planID, st.Revision)}
		}
		next, _, violations := applyChanges(st, ev.Changes, false)
		if len(violations) > 0 {
			return &CorruptError{Detail: fmt.Sprintf("revision %d of plan %s does not apply: %s", ev.RevisionNo, planID, (&PlanRejected{Violations: violations}).refusal().Detail)}
		}
		if got := next.At(next.Revision).StateDigest; got != ev.StateDigest {
			return &CorruptError{Detail: fmt.Sprintf("replaying revision %d of plan %s gives state digest %s; the revision recorded %s", ev.RevisionNo, planID, got, ev.StateDigest)}
		}
		st = next
	}
	head, err := headRevision(ctx, q, planID)
	if err != nil {
		return err
	}
	if st.Revision != head {
		return &CorruptError{Detail: fmt.Sprintf("plan %s replays to revision %d but its head is %d", planID, st.Revision, head)}
	}
	stored, err := loadState(ctx, q, planID, project, head)
	if err != nil {
		return err
	}
	if !sameRows(st, stored) {
		return &CorruptError{Detail: fmt.Sprintf("the node and edge rows of plan %s are not what replaying its log produces", planID)}
	}
	return nil
}

// sameRows compares every row of two folds, retired history included.
func sameRows(a, b State) bool {
	key := func(n NodeVersion) string { return fmt.Sprintf("%s/%d", n.NodeID, n.IntroducedRev) }
	sort.Slice(a.Nodes, func(i, j int) bool { return key(a.Nodes[i]) < key(a.Nodes[j]) })
	sort.Slice(b.Nodes, func(i, j int) bool { return key(b.Nodes[i]) < key(b.Nodes[j]) })
	sort.Slice(a.Edges, func(i, j int) bool { return a.Edges[i].EdgeID < a.Edges[j].EdgeID })
	sort.Slice(b.Edges, func(i, j int) bool { return b.Edges[i].EdgeID < b.Edges[j].EdgeID })
	if len(a.Nodes) != len(b.Nodes) || len(a.Edges) != len(b.Edges) {
		return false
	}
	for i := range a.Nodes {
		if a.Nodes[i].NodeID != b.Nodes[i].NodeID || a.Nodes[i].Node != b.Nodes[i].Node || a.Nodes[i].SliceDigest != b.Nodes[i].SliceDigest ||
			a.Nodes[i].SupersedesNodeID != b.Nodes[i].SupersedesNodeID || a.Nodes[i].IntroducedRev != b.Nodes[i].IntroducedRev || a.Nodes[i].RetiredRev != b.Nodes[i].RetiredRev {
			return false
		}
	}
	for i := range a.Edges {
		x, y := a.Edges[i], b.Edges[i]
		if canonical(edgeObject(x.Edge)) != canonical(edgeObject(y.Edge)) || x.IntroducedRev != y.IntroducedRev || x.RetiredRev != y.RetiredRev {
			return false
		}
	}
	return true
}

// Preflight judges a revision as far as it can WITHOUT writing or creating anything, before the
// command opens the store for writing: an invalid first revision, a stale or impossible parent, or
// a revision that breaks the plan rules is refused here, so a refused request leaves a state
// directory without a store as it found it. The transaction in Put repeats every check as the
// authority; this one is a refusal's early form. A failure to read the store is the host's failure
// and is returned as it is, never handed on to the writing open.
func Preflight(ctx context.Context, dbPath string, rev Revision) error {
	rev, err := Checked(rev)
	if err != nil {
		return err
	}
	if rev.ExpectedParent == 0 {
		_, _, violations := Apply(State{PlanID: rev.PlanID}, rev)
		if len(violations) > 0 {
			return &PlanRejected{Violations: violations}
		}
		return nil
	}
	if _, err := os.Lstat(dbPath); errors.Is(err, os.ErrNotExist) {
		return conflict("the request expects parent revision %d, but there is no relay store, so plan %s has no revision", rev.ExpectedParent, rev.PlanID)
	}
	ro, err := store.OpenInPlace(ctx, dbPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("checking the plan before writing: %w", err)
	}
	defer ro.Close()
	return preflightRead(ctx, ro, rev)
}

// preflightRead is Preflight's reading of an existing store. Its reads are separate queries, so another
// writer's commit can land between them; when one of them is this very request (a repeated request racing
// its own first copy), every later read sees the plan after that commit and the request looks stale or
// conflicting. So a refusal is handed on, not given, once the request is found in the log: the writing open
// answers it with the stored result, and judges it again as the authority. A request that is not in the log
// is refused as it was.
func preflightRead(ctx context.Context, q Queryer, rev Revision) error {
	err := judgeAgainstStore(ctx, q, rev)
	if err == nil {
		return nil
	}
	if _, found, lookup := revisionByRequest(ctx, q, rev.PlanID, rev.RequestID); lookup == nil && found {
		return nil
	}
	return err
}

func judgeAgainstStore(ctx context.Context, ro Queryer, rev Revision) error {
	if _, found, err := revisionByRequest(ctx, ro, rev.PlanID, rev.RequestID); err != nil {
		return err
	} else if found {
		return nil // a repeated request: the writing open returns the stored result
	}
	project, exists, err := loadHeader(ctx, ro, rev.PlanID)
	if err != nil {
		return err
	}
	head := int64(0)
	if exists {
		if head, err = headRevision(ctx, ro, rev.PlanID); err != nil {
			return err
		}
	}
	if rev.ExpectedParent != head {
		return conflict("the request expects parent revision %d, but plan %s is at revision %d", rev.ExpectedParent, rev.PlanID, head)
	}
	st, err := loadState(ctx, ro, rev.PlanID, project, head)
	if err != nil {
		return err
	}
	if _, _, violations := Apply(st, rev); len(violations) > 0 {
		return &PlanRejected{Violations: violations}
	}
	return nil
}
