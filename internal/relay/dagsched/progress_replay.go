package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The progress view is rebuildable (docs/relay/dag-progress.md, "Rebuilding the view", CRW-287). A reader that keeps its own copy of the view (the Linear summary outbox, a dashboard) holds a
// ProgressSnapshot, asks for the events after its cursor and folds them with ApplyProgressDelta; replaying from the beginning is the same read from the empty cursor, and what it rebuilds is
// the document dag-progress prints, byte for byte, with its digest. Nothing here writes: the readers are reads, the fold is a pure function.
//
// What an event is. The view rests on the plan log and on execution rows, and the two have different histories. The plan log is a true log: committed revisions, appended and never changed
// (dag.Event, CRW-183), whose position is a revision number. The execution rows are not: the store updates them in place (relationships.status, managed_start_requests.state,
// merge_turns.state, execution_slots.state, dag_acceptances.state, an observation's reverted_by), carries no sequence shared between tables, and journals only some of the writers, so there
// is nothing to replay and the order in which they changed is not recoverable. What can be replayed is what the view takes from them, which is the scheduler's reading of each node and the
// node's facts (the inputs of ProjectProgress, one ProgressRecord per live node) and the one aggregate of the nodes the revisions took out of the plan (ProgressOutside). Each is
// content-addressed by a digest of its printed fields, and an event is a plan revision, a record or outside aggregate whose digest differs from the reader's, or the removal of a record the
// reader holds. The consequence is stated in the page: transitions of a record between two reads coalesce (running, reported, verifying is one change); only the plan revisions are complete history.
//
// The cursor is the reader's state, not a position in a list: the plan it holds, the last revision with a hash chain over the log metadata of the revisions up to it, the digest of its outside
// aggregate and the digest of every record it holds. A read is "what differs from that", in a fixed order, cut at the limit; no page remembers anything, so a store that moves between two pages
// does not lose or repeat anything the reader holds (a record that changed after it was read is read again, as a new event) and the fold ends at the view of the page with more false.

// The kinds of ProgressEvent.
const (
	// ProgressEventPlan opens a read from a cursor that holds no plan (or another plan): the plan's identity.
	ProgressEventPlan = "plan"
	// ProgressEventRevision is a committed revision of the plan log: the event of CRW-183, folded with dag.Replay.
	ProgressEventRevision = "revision"
	// ProgressEventRemoved is a node the reader holds a record of that is no longer live.
	ProgressEventRemoved = "removed"
	// ProgressEventOutside is the aggregate of the nodes that left the plan, when it differs from the reader's.
	ProgressEventOutside = "outside"
	// ProgressEventNode is the record of a live node that differs from the reader's, or that the reader does not hold.
	ProgressEventNode = "node"
)

// ProgressRecord is what the view takes of one live node: the scheduler's reading of it (without its rank, which is not printed, and without the digest of a manifest rebuilt from the file
// system, which is not a function of the store) and its facts. Digest covers every printed field of the node except its stage, which ProjectProgress derives again.
type ProgressRecord struct {
	NodeID  string
	Reading NodeReading
	Facts   NodeFacts
	Digest  string
}

// ProgressOutside is the aggregate of the nodes the plan's revisions took out of it (the outside_denominator of the document) with its digest.
type ProgressOutside struct {
	Outside OutsideDenominator
	Digest  string
}

// ProgressEvent is one change a reader has not seen. Kind says which fields are set: the plan header (PlanID, ProjectKey), a revision (Revision), a removed node (NodeID), the outside
// aggregate (Outside), a node's record (NodeID and Record).
type ProgressEvent struct {
	Kind               string
	PlanID, ProjectKey string
	Revision           *dag.Event
	NodeID             string
	Record             *ProgressRecord
	Outside            *ProgressOutside
}

// ID names an event by what it is and what it says: two events with one ID are the same change, so a read that delivers an ID twice has delivered a change twice.
func (e ProgressEvent) ID() string {
	switch e.Kind {
	case ProgressEventPlan:
		return "plan/" + e.PlanID
	case ProgressEventRevision:
		if e.Revision == nil {
			return "revision/"
		}
		return fmt.Sprintf("revision/%d@%s", e.Revision.RevisionNo, e.Revision.StateDigest)
	case ProgressEventRemoved:
		return "node/" + e.NodeID + "@removed"
	case ProgressEventOutside:
		if e.Outside == nil {
			return "outside@"
		}
		return "outside@" + e.Outside.Digest
	case ProgressEventNode:
		if e.Record == nil {
			return "node/" + e.NodeID + "@"
		}
		return "node/" + e.NodeID + "@" + e.Record.Digest
	}
	return e.Kind
}

// ProgressCursor is the state a reader holds, as the digests of it. The zero value holds nothing: it is the beginning. RevisionDigest is the hash chain of the log metadata of the revisions 1
// to Revision (revision number, recorded_at, request id, author, epoch, state digest and operations of each, which a reader computes from its own revisions), so a log that was replaced by
// another is found by its metadata and not only by its state digest. Outside is the digest of the outside aggregate held, Records the digest of each node record held.
type ProgressCursor struct {
	PlanID         string
	Revision       int64
	RevisionDigest string
	Outside        string
	Records        map[string]string
}

type cursorWire struct {
	PlanID         string            `json:"plan_id"`
	Revision       int64             `json:"revision"`
	RevisionDigest string            `json:"revision_digest"`
	Outside        string            `json:"outside"`
	Records        map[string]string `json:"records"`
}

// String is the cursor as text a reader can keep: JSON with every key, in a fixed order, map keys sorted, so equal cursors are equal text.
func (c ProgressCursor) String() string {
	w := cursorWire(c)
	if w.Records == nil {
		w.Records = map[string]string{}
	}
	out, err := json.Marshal(w)
	if err != nil {
		return "" // a map of strings and plain fields cannot fail to marshal
	}
	return string(out)
}

// Equal is whether two cursors hold the same state (an absent map and an empty one are the same).
func (c ProgressCursor) Equal(o ProgressCursor) bool {
	if c.PlanID != o.PlanID || c.Revision != o.Revision || c.RevisionDigest != o.RevisionDigest || c.Outside != o.Outside || len(c.Records) != len(o.Records) {
		return false
	}
	for id, digest := range c.Records {
		if other, ok := o.Records[id]; !ok || other != digest {
			return false
		}
	}
	return true
}

var digestPattern = regexp.MustCompile("^[0-9a-f]{64}$")

// ParseProgressCursor reads the text String prints. The empty text is the beginning. Anything that is not a cursor is refused as a malformed document (malformed_receipt, the reason the relay
// gives a structured document that is malformed): unknown keys, trailing data, a revision without its digest, a digest that is not a sha256 in lowercase hex, state held of no plan.
func ParseProgressCursor(text string) (ProgressCursor, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ProgressCursor{}, nil
	}
	bad := func(format string, args ...any) (ProgressCursor, error) {
		return ProgressCursor{}, refuse(contract.RefusalMalformedReceipt, "progress cursor: "+format, args...)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var w cursorWire
	if err := dec.Decode(&w); err != nil {
		return bad("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad("data after the cursor")
	}
	switch {
	case w.Revision < 0:
		return bad("revision %d is negative", w.Revision)
	case w.Revision == 0 && w.RevisionDigest != "":
		return bad("a revision digest without a revision")
	case w.Revision > 0 && !digestPattern.MatchString(w.RevisionDigest):
		return bad("revision %d has no sha256 digest", w.Revision)
	case w.Outside != "" && !digestPattern.MatchString(w.Outside):
		return bad("the outside digest is not a sha256")
	case w.PlanID == "" && (w.Revision != 0 || w.Outside != "" || len(w.Records) > 0):
		return bad("state held of no plan")
	}
	for id, digest := range w.Records {
		if id == "" || !digestPattern.MatchString(digest) {
			return bad("record %q has no sha256 digest", id)
		}
	}
	c := ProgressCursor{PlanID: w.PlanID, Revision: w.Revision, RevisionDigest: w.RevisionDigest, Outside: w.Outside}
	if len(w.Records) > 0 {
		c.Records = w.Records
	}
	return c, nil
}

// ProgressSnapshot is the view's inputs as a reader holds them: the plan as of its revision (a CRW-183 snapshot), the denominators of every revision, one record per live node and the outside
// aggregate. A snapshot taken from the live view (ProgressSnapshotOf) is complete, and so is one that ApplyProgressDelta folded up to a page with more false and whose digest it checked; any
// other (the zero value, one cut off between pages, one built by hand) is not, and Project refuses it. Outside is a pointer so that an aggregate not held differs from one held and empty.
type ProgressSnapshot struct {
	Plan      dag.Snapshot
	Revisions []RevisionCount
	Records   map[string]ProgressRecord
	Outside   *ProgressOutside
	complete  bool
}

// printedDigest is the sha256 of the printed form of a value, the way the document's own digest is taken.
func printedDigest(o contract.OrderedObject) (string, error) {
	printed, err := pyjson.Encode(o, pyjson.Options{})
	if err != nil {
		return "", invariant("a progress record cannot be printed: %v", err)
	}
	sum := sha256.Sum256(printed)
	return hex.EncodeToString(sum[:]), nil
}

// recordObject is what a record's digest covers: every printed field of the node entry except its stage, which is a function of the rest.
func recordObject(r ProgressRecord) contract.OrderedObject {
	n := r.Reading
	o := contract.OrderedObject{
		{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "kind", Value: n.Kind}, {Key: "title", Value: optionalText(r.Facts.Title)},
		{Key: "state", Value: n.State}, {Key: "disposition", Value: n.Disposition}, {Key: "reason", Value: optionalText(n.Reason)}, {Key: "detail", Value: optionalText(n.Detail)},
	}
	if n.Stale != nil {
		o = append(o, contract.Field{Key: "stale", Value: staleObject(*n.Stale)})
	}
	return append(o, contract.Field{Key: "lifecycle", Value: optionalText(n.Lifecycle)}, contract.Field{Key: "acceptance_id", Value: optionalText(r.Facts.AcceptanceID)},
		contract.Field{Key: "holds_slot", Value: r.Facts.HoldsSlot}, contract.Field{Key: "links", Value: r.Facts.Links.object()})
}

func recordDigest(r ProgressRecord) (string, error) { return printedDigest(recordObject(r)) }

func outsideObject(o OutsideDenominator) contract.OrderedObject {
	return contract.OrderedObject{{Key: "nodes", Value: o.Nodes}, {Key: "with_acceptance", Value: o.WithAcceptance}, {Key: "holding_slot", Value: o.HoldingSlot}, {Key: "node_ids", Value: listOf(o.NodeIDs)}}
}

func makeOutside(o OutsideDenominator) (ProgressOutside, error) {
	o.NodeIDs = append([]string{}, o.NodeIDs...)
	digest, err := printedDigest(outsideObject(o))
	return ProgressOutside{Outside: o, Digest: digest}, err
}

// makeRecord is the record of a node entry of a projected view: its reading and its facts as ProjectProgress takes them.
func makeRecord(n NodeProgress) (ProgressRecord, error) {
	r := ProgressRecord{NodeID: n.NodeID,
		Reading: NodeReading{NodeID: n.NodeID, IssueKey: n.IssueKey, Kind: n.Kind, State: n.State, Disposition: n.Disposition, Reason: n.Reason, Detail: n.Detail, Lifecycle: n.Lifecycle},
		Facts:   NodeFacts{Title: n.Title, AcceptanceID: n.AcceptanceID, HoldsSlot: n.HoldsSlot, Links: n.Links}}
	if n.Stale != nil {
		stale := *n.Stale
		stale.RebuiltManifest = ""
		r.Reading.Stale = &stale
	}
	var err error
	r.Digest, err = recordDigest(r)
	return r, err
}

// chainLink is the next link of the hash chain over the log metadata of the revisions: the previous link and the fields of one revision, each length-prefixed so no two sequences of fields
// give one input.
func chainLink(previous string, revision int64, recordedAt, requestID, author string, epoch int64, stateDigest string, ops []string) string {
	h := sha256.New()
	field := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s|", len(s), s) }
	field(previous)
	field(fmt.Sprint(revision))
	field(recordedAt)
	field(requestID)
	field(author)
	field(fmt.Sprint(epoch))
	field(stateDigest)
	field(fmt.Sprint(len(ops)))
	for _, op := range ops {
		field(op)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func chainOf(revisions []RevisionCount) string {
	link := ""
	for _, r := range revisions {
		link = chainLink(link, r.Revision, r.RecordedAt, r.RequestID, r.AuthorTaskID, r.CoordinatorEpoch, r.StateDigest, r.Ops)
	}
	return link
}

func opsOf(ev dag.Event) []string {
	ops := make([]string, len(ev.Changes))
	for i, c := range ev.Changes {
		ops[i] = c.Op
	}
	return ops
}

// Cursor is the cursor of the state the snapshot holds: the point from which the events after it complete it. It is what a reader keeps, and what ProgressDelta continues from.
func (s ProgressSnapshot) Cursor() ProgressCursor {
	c := ProgressCursor{PlanID: s.Plan.PlanID, Revision: int64(len(s.Revisions)), RevisionDigest: chainOf(s.Revisions), Records: map[string]string{}}
	if s.Outside != nil {
		c.Outside = s.Outside.Digest
	}
	for id, r := range s.Records {
		c.Records[id] = r.Digest
	}
	return c
}

func (s ProgressSnapshot) clone() ProgressSnapshot {
	c := s
	c.Revisions = append([]RevisionCount(nil), s.Revisions...)
	c.Records = make(map[string]ProgressRecord, len(s.Records))
	for id, r := range s.Records {
		c.Records[id] = r
	}
	if s.Outside != nil {
		o := *s.Outside
		c.Outside = &o
	}
	return c
}

// project is ProjectProgress over a reading rebuilt from the snapshot: the plan's identity, head and state from the replayed plan snapshot, the records as the readings and facts of the nodes,
// the revisions and the outside aggregate as they are. The projection is ProjectProgress itself, so the stages, the guards and the digest are the ones dag-progress has.
func (s ProgressSnapshot) project() (Progress, error) {
	if s.Plan.PlanID == "" || len(s.Revisions) == 0 {
		return Progress{}, invariant("the snapshot holds no plan revision")
	}
	if int64(len(s.Revisions)) != s.Plan.Revision {
		return Progress{}, invariant("the snapshot holds %d revisions and its plan is at revision %d", len(s.Revisions), s.Plan.Revision)
	}
	if s.Outside == nil {
		return Progress{}, invariant("the snapshot holds no outside aggregate")
	}
	ids := make([]string, 0, len(s.Records))
	for id := range s.Records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	readings := make([]NodeReading, 0, len(ids))
	facts := make(map[string]NodeFacts, len(ids))
	for _, id := range ids {
		r := s.Records[id]
		if r.NodeID != id {
			return Progress{}, invariant("the snapshot holds the record of node %s under %s", r.NodeID, id)
		}
		readings = append(readings, r.Reading)
		facts[id] = r.Facts
	}
	outside := s.Outside.Outside
	outside.NodeIDs = append([]string{}, outside.NodeIDs...)
	return ProjectProgress(ProgressInput{
		Reading:    Reading{PlanID: s.Plan.PlanID, PlanRevision: s.Plan.Revision, StateDigest: s.Plan.StateDigest, PlanState: s.Plan.PlanState, Nodes: readings},
		ProjectKey: s.Plan.ProjectKey, Revisions: append([]RevisionCount(nil), s.Revisions...), Facts: facts, Outside: outside,
	})
}

// Project is the view the snapshot stands for: the document dag-progress prints, with its digest. It refuses (an InvariantError, the host's failure class) a snapshot that is not complete: a
// fold that has only some of the pages of a read would otherwise print a hybrid of two states of the store that passes every check ProjectProgress has.
func (s ProgressSnapshot) Project() (Progress, error) {
	if !s.complete {
		return Progress{}, invariant("the snapshot has not caught up: it was not taken from the live view and no read that ended (more false) was folded into it")
	}
	return s.project()
}

// ProgressDelta is one page of a read: the events a reader that holds the cursor it asked from has not seen, at most limit of them, and the cursor that holds them too. More says that the next
// page has events (the same call from Cursor). Fingerprint is the digest of the live view the page was read at; the fold of the page with More false must project exactly that digest, and
// ApplyProgressDelta checks it, so only that page's fingerprint means anything to a reader.
type ProgressDelta struct {
	PlanID, ProjectKey string
	Events             []ProgressEvent
	Cursor             ProgressCursor
	More               bool
	Fingerprint        string
}

// snapshotOfProgress decomposes a projected view, with the plan snapshot of the same store state, into the state a reader holds.
func snapshotOfProgress(p Progress, plan dag.Snapshot) (ProgressSnapshot, error) {
	if plan.PlanID != p.Reading.PlanID || plan.Revision != p.Reading.PlanRevision || int64(len(p.Revisions)) != plan.Revision || len(p.Revisions) == 0 {
		return ProgressSnapshot{}, invariant("plan %s: the view is at revision %d with %d revisions, its snapshot at revision %d", p.Reading.PlanID, p.Reading.PlanRevision, len(p.Revisions), plan.Revision)
	}
	if head := p.Revisions[len(p.Revisions)-1]; head.StateDigest != plan.StateDigest {
		return ProgressSnapshot{}, invariant("plan %s: revision %d recorded state digest %s and the snapshot has %s", plan.PlanID, head.Revision, head.StateDigest, plan.StateDigest)
	}
	s := ProgressSnapshot{Plan: plan, Revisions: append([]RevisionCount(nil), p.Revisions...), Records: make(map[string]ProgressRecord, len(p.Nodes)), complete: true}
	for _, n := range p.Nodes {
		r, err := makeRecord(n)
		if err != nil {
			return ProgressSnapshot{}, err
		}
		s.Records[n.NodeID] = r
	}
	outside, err := makeOutside(p.Outside)
	if err != nil {
		return ProgressSnapshot{}, err
	}
	s.Outside = &outside
	return s, nil
}

// ProgressSnapshotOf is the live view as the state a reader holds, read in the caller's transaction (as Progress is) together with the view it was taken from. Its cursor is where a reader that
// keeps it resumes from.
func (s *Scheduler) ProgressSnapshotOf(ctx context.Context, q store.Querier, plan string) (ProgressSnapshot, Progress, error) {
	p, err := s.Progress(ctx, q, plan)
	if err != nil {
		return ProgressSnapshot{}, Progress{}, err
	}
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return ProgressSnapshot{}, Progress{}, err
	}
	out, err := snapshotOfProgress(p, snap)
	if err != nil {
		return ProgressSnapshot{}, Progress{}, err
	}
	return out, p, nil
}

// ReadProgressSnapshot is ProgressSnapshotOf in one read transaction of the store, the way ReadProgress is Progress.
func (s *Scheduler) ReadProgressSnapshot(ctx context.Context, plan string) (ProgressSnapshot, Progress, error) {
	var (
		snap ProgressSnapshot
		view Progress
	)
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		snap, view, err = s.ProgressSnapshotOf(txCtx, s.Store.Q(txCtx), plan)
		return err
	})
	return snap, view, err
}

// advanceCursor is the cursor after a reader that held c has been given events: what applying them to a snapshot with cursor c yields as its cursor.
func advanceCursor(c ProgressCursor, events []ProgressEvent) ProgressCursor {
	next := ProgressCursor{PlanID: c.PlanID, Revision: c.Revision, RevisionDigest: c.RevisionDigest, Outside: c.Outside, Records: make(map[string]string, len(c.Records))}
	for id, digest := range c.Records {
		next.Records[id] = digest
	}
	for _, ev := range events {
		switch ev.Kind {
		case ProgressEventPlan:
			if next.PlanID != ev.PlanID {
				next = ProgressCursor{PlanID: ev.PlanID, Records: map[string]string{}}
			}
		case ProgressEventRevision:
			if ev.Revision != nil {
				next.RevisionDigest = chainLink(next.RevisionDigest, ev.Revision.RevisionNo, ev.Revision.RecordedAt, ev.Revision.RequestID, ev.Revision.AuthorTaskID,
					ev.Revision.CoordinatorEpoch, ev.Revision.StateDigest, opsOf(*ev.Revision))
				next.Revision = ev.Revision.RevisionNo
			}
		case ProgressEventRemoved:
			delete(next.Records, ev.NodeID)
		case ProgressEventOutside:
			if ev.Outside != nil {
				next.Outside = ev.Outside.Digest
			}
		case ProgressEventNode:
			if ev.Record != nil {
				next.Records[ev.NodeID] = ev.Record.Digest
			}
		}
	}
	return next
}

// ProgressDelta is the read: the events after a cursor, inside the caller's transaction (as Progress is). It reads the live view once, so the page is one state of the store, and sends, in
// this order and cut at limit (1 to dag.MaxPage, counting events of every kind): the plan header when the cursor holds no plan (a cursor of another plan holds nothing of this one), the
// revisions after the cursor, the removal of every record the cursor holds that is no longer live, the outside aggregate when it differs, the record of every live node whose digest differs, by
// node id. There is no positional marker: a page is the first limit things that differ from the cursor, so whatever changed after an earlier page is found by the next. A cursor that names a
// revision the plan does not have, or an unknown plan, is the refusal unregistered_scope (what dag-plan-log answers); a cursor whose revision chain is not the log's is revision_mismatch.
func (s *Scheduler) ProgressDelta(ctx context.Context, q store.Querier, plan string, after ProgressCursor, limit int) (ProgressDelta, error) {
	live, view, err := s.ProgressSnapshotOf(ctx, q, plan)
	if err != nil {
		return ProgressDelta{}, err
	}
	if limit < 1 || limit > dag.MaxPage {
		limit = dag.MaxPage
	}
	if after.PlanID != live.Plan.PlanID {
		after = ProgressCursor{}
	}
	head := live.Plan.Revision
	if after.Revision < 0 || after.Revision > head {
		return ProgressDelta{}, refuse(contract.RefusalUnregisteredScope, "plan %s has no revision %d to resume after; its revisions are 1 to %d", plan, after.Revision, head)
	}
	if want := chainOf(live.Revisions[:after.Revision]); after.RevisionDigest != want {
		return ProgressDelta{}, refuse(contract.RefusalRevisionMismatch, "the cursor holds revision %d of plan %s with the log digest %q and the plan's log has %q: it is not the same log", after.Revision, plan, after.RevisionDigest, want)
	}
	var events []ProgressEvent
	if after.PlanID == "" {
		events = append(events, ProgressEvent{Kind: ProgressEventPlan, PlanID: live.Plan.PlanID, ProjectKey: live.Plan.ProjectKey})
	}
	page, err := dag.EventsAfter(ctx, q, plan, after.Revision, limit)
	if err != nil {
		return ProgressDelta{}, err
	}
	more := int64(len(page.Events)) < head-after.Revision
	for i := range page.Events {
		ev := page.Events[i]
		events = append(events, ProgressEvent{Kind: ProgressEventRevision, Revision: &ev})
	}
	var removed []string
	for id := range after.Records {
		if _, ok := live.Records[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	for _, id := range removed {
		events = append(events, ProgressEvent{Kind: ProgressEventRemoved, NodeID: id})
	}
	if live.Outside != nil && after.Outside != live.Outside.Digest {
		outside := *live.Outside
		events = append(events, ProgressEvent{Kind: ProgressEventOutside, Outside: &outside})
	}
	ids := make([]string, 0, len(live.Records))
	for id := range live.Records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if record := live.Records[id]; after.Records[id] != record.Digest {
			events = append(events, ProgressEvent{Kind: ProgressEventNode, NodeID: id, Record: &record})
		}
	}
	if len(events) > limit {
		events, more = events[:limit], true
	}
	return ProgressDelta{PlanID: live.Plan.PlanID, ProjectKey: live.Plan.ProjectKey, Events: events, Cursor: advanceCursor(after, events), More: more, Fingerprint: view.Digest}, nil
}

// ReadProgressDelta is ProgressDelta in one read transaction of the store, the way ReadProgress is Progress: inside a composing transaction it joins the caller's, inside a plain one it is
// store.ErrNestedTransaction. Through a store opened read-only it works the same and writes nothing.
func (s *Scheduler) ReadProgressDelta(ctx context.Context, plan string, after ProgressCursor, limit int) (ProgressDelta, error) {
	var out ProgressDelta
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.ProgressDelta(txCtx, s.Store.Q(txCtx), plan, after, limit)
		return err
	})
	return out, err
}

// revisionCountOf is the denominator of the revision the event committed, derived from the plan before it and the plan after it: the live nodes each side holds, the nodes that entered and
// left, and the nodes live on both sides whose version was introduced by this revision (their spec or their incoming edges changed). It is the rule revisionCounts applies to the rows of the log,
// applied to the plan the fold produced, so a snapshot that folded the events holds the revisions a store that stored them holds.
func revisionCountOf(before, after dag.Snapshot, ev dag.Event) RevisionCount {
	was := map[string]bool{}
	for _, n := range before.Nodes {
		was[n.NodeID] = true
	}
	r := RevisionCount{Revision: ev.RevisionNo, RecordedAt: ev.RecordedAt, RequestID: ev.RequestID, AuthorTaskID: ev.AuthorTaskID, StateDigest: ev.StateDigest, CoordinatorEpoch: ev.CoordinatorEpoch,
		Nodes: len(after.Nodes), PreviousNodes: len(before.Nodes), Added: []string{}, Retired: []string{}, Updated: []string{}, Ops: opsOf(ev)}
	r.Delta = r.Nodes - r.PreviousNodes
	is := map[string]bool{}
	for _, n := range after.Nodes {
		is[n.NodeID] = true
		switch {
		case !was[n.NodeID]:
			r.Added = append(r.Added, n.NodeID)
		case n.IntroducedRev == ev.RevisionNo:
			r.Updated = append(r.Updated, n.NodeID)
		}
	}
	for _, n := range before.Nodes {
		if !is[n.NodeID] {
			r.Retired = append(r.Retired, n.NodeID)
		}
	}
	sort.Strings(r.Added)
	sort.Strings(r.Retired)
	sort.Strings(r.Updated)
	r.DenominatorChanged = len(r.Added)+len(r.Retired) > 0
	return r
}

// applyEvent folds one event into the snapshot (a copy the caller owns).
func (s *ProgressSnapshot) applyEvent(d ProgressDelta, ev ProgressEvent) error {
	if ev.Kind != ProgressEventPlan && s.Plan.PlanID == "" {
		return invariant("a %s event before the plan header", ev.Kind)
	}
	switch ev.Kind {
	case ProgressEventPlan:
		if ev.PlanID != d.PlanID {
			return invariant("the plan header names plan %s in a delta of plan %s", ev.PlanID, d.PlanID)
		}
		if s.Plan.PlanID == "" {
			s.Plan = dag.Snapshot{PlanID: ev.PlanID, ProjectKey: ev.ProjectKey}
		} else if s.Plan.ProjectKey != ev.ProjectKey {
			return invariant("plan %s is of project %s in the snapshot and of project %s in its header", ev.PlanID, s.Plan.ProjectKey, ev.ProjectKey)
		}
	case ProgressEventRevision:
		if ev.Revision == nil {
			return invariant("a revision event carries no revision")
		}
		if ev.Revision.PlanID != s.Plan.PlanID {
			return invariant("an event of plan %s given to the fold of plan %s", ev.Revision.PlanID, s.Plan.PlanID)
		}
		if int64(len(s.Revisions)) != s.Plan.Revision {
			return invariant("the snapshot holds %d revisions and its plan is at revision %d", len(s.Revisions), s.Plan.Revision)
		}
		replayed, err := dag.Replay(s.Plan, []dag.Event{*ev.Revision})
		if err != nil {
			return invariant("revision %d of plan %s does not follow the snapshot: %v", ev.Revision.RevisionNo, s.Plan.PlanID, err)
		}
		s.Revisions = append(s.Revisions, revisionCountOf(s.Plan, replayed, *ev.Revision))
		s.Plan = replayed
	case ProgressEventRemoved:
		if ev.NodeID == "" {
			return invariant("a removal names no node")
		}
		delete(s.Records, ev.NodeID)
	case ProgressEventOutside:
		if ev.Outside == nil {
			return invariant("an outside event carries no aggregate")
		}
		want, err := makeOutside(ev.Outside.Outside)
		if err != nil {
			return err
		}
		if want.Digest != ev.Outside.Digest {
			return invariant("the outside aggregate has digest %s and its content digests to %s", ev.Outside.Digest, want.Digest)
		}
		o := *ev.Outside
		s.Outside = &o
	case ProgressEventNode:
		if ev.Record == nil || ev.NodeID == "" || ev.Record.NodeID != ev.NodeID {
			return invariant("a node event carries no record of its node")
		}
		want, err := recordDigest(*ev.Record)
		if err != nil {
			return err
		}
		if want != ev.Record.Digest {
			return invariant("the record of node %s has digest %s and its content digests to %s", ev.NodeID, ev.Record.Digest, want)
		}
		s.Records[ev.NodeID] = *ev.Record
	default:
		return invariant("an event of the unknown kind %q", ev.Kind)
	}
	return nil
}

// ApplyProgressDelta folds a page into a snapshot and returns the result; it is pure, writes nothing and leaves the snapshot it was given as it was. It is the only way a snapshot is folded, and
// it refuses, with the zero snapshot and an InvariantError, whatever a log cannot contain: an event that does not follow the one before or a revision twice (dag.Replay, which also checks the state
// digest each revision recorded), a record or outside aggregate whose digest is not its content, an event of another plan (dag.Replay does not look at the plan), a delta of another plan than the
// snapshot holds unless it starts with the plan header (the fold is then reset and starts over: a cursor of another plan holds nothing of this one), and a fold whose cursor is not the delta's.
// While the delta says more the result is not complete. On the page with more false it projects the result and requires its digest to be the delta's fingerprint before it marks the snapshot
// complete: a fold that dropped an event, folded a page that was not read from its cursor or was read from another store cannot report that it caught up.
func ApplyProgressDelta(s ProgressSnapshot, d ProgressDelta) (ProgressSnapshot, error) {
	if d.PlanID == "" {
		return ProgressSnapshot{}, invariant("a delta names no plan")
	}
	next := s.clone()
	next.complete = false
	if next.Plan.PlanID != "" && next.Plan.PlanID != d.PlanID {
		if len(d.Events) == 0 || d.Events[0].Kind != ProgressEventPlan {
			return ProgressSnapshot{}, invariant("the delta is of plan %s and the snapshot holds plan %s: only a delta that starts with the plan header replaces it", d.PlanID, next.Plan.PlanID)
		}
		next = ProgressSnapshot{}
	}
	if next.Records == nil {
		next.Records = map[string]ProgressRecord{}
	}
	for _, ev := range d.Events {
		if err := next.applyEvent(d, ev); err != nil {
			return ProgressSnapshot{}, err
		}
	}
	if got := next.Cursor(); !got.Equal(d.Cursor) {
		return ProgressSnapshot{}, invariant("plan %s: the fold reached the cursor %s and the delta says %s", d.PlanID, got, d.Cursor)
	}
	if d.More {
		return next, nil
	}
	view, err := next.project()
	if err != nil {
		return ProgressSnapshot{}, err
	}
	if view.Digest != d.Fingerprint {
		return ProgressSnapshot{}, invariant("plan %s: the rebuilt view digests to %s and the live view the delta was read at to %s", d.PlanID, view.Digest, d.Fingerprint)
	}
	next.complete = true
	return next, nil
}
