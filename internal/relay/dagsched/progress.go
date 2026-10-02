package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The progress of a DAG plan (docs/relay/dag-progress.md, CRW-286). It answers, from the relay store alone, how many nodes stand in which stage, how much has been accepted and
// integrated, what the denominator was at every plan revision and whether it changed, why every node that is not moving is not moving, and where its relationship, thread and pull
// request are. It is a projection over Ready: the derived state, the disposition and the closed reason of every node are the reading's, and nothing here derives a state again.
//
// Activity (a poll of a child's turn, a lifecycle observation, a turn admitted, a token count nobody limited) is never read, and no clock is, so no stage can move because time passed
// or a child was merely alive. A resource fact is a different thing and is read as dag-ready reads it: a declared limit with a usage someone recorded against it decides whether a
// node that has not started is ready or waiting on a resource.

// SchemaProgress names the document dag-progress prints.
const SchemaProgress = "dag-progress/1"

// The stages: a partition of the live nodes of a plan, so the counts of the stages add up to the denominator. Blocked is not a stage but an overlay (Progress.Blocked): a node keeps the
// stage of its derived state, an accepted node whose merge turn ended with an unknown effect is accepted and blocked.
const (
	StageWaitingPredecessor = "waiting_predecessor"
	StageWaitingDecision    = "waiting_decision"
	StageWaitingResource    = "waiting_resource"
	StageReady              = "ready"
	StageReleasing          = "releasing"
	StageCreationUnknown    = "creation_unknown"
	StageRunning            = "running"
	StageReported           = "reported"
	StageVerifying          = "verifying"
	StageCorrecting         = "correcting"
	StageAccepted           = "accepted"
	StageIntegrated         = "integrated"
	StageStale              = "stale"
	StagePaused             = "paused"
	StageCancelled          = "cancelled"
	StageClosed             = "closed"
	StageAmbiguous          = "ambiguous"
)

// ProgressStages are the stages in the order the document lists them: from waiting to finished, then the states that stand apart from that line.
var ProgressStages = []string{
	StageWaitingPredecessor, StageWaitingDecision, StageWaitingResource, StageReady, StageReleasing, StageCreationUnknown, StageRunning, StageReported, StageVerifying,
	StageCorrecting, StageAccepted, StageIntegrated, StageStale, StagePaused, StageCancelled, StageClosed, StageAmbiguous,
}

// The sources of a pull request link: the forge identity recorded with the active acceptance, or the work report of the head event (written by an earlier build; this build has no writer).
const (
	PRSourceAcceptance = "acceptance"
	PRSourceWorkReport = "work_report"
)

// InvariantError is a projection that cannot be printed honestly: a node it cannot explain or place, or revisions that do not end at the reading's denominator. It is the host's failure,
// never a partial document.
type InvariantError struct{ Detail string }

func (e *InvariantError) Error() string { return "dag progress: " + e.Detail }

func invariant(format string, args ...any) error {
	return &InvariantError{Detail: fmt.Sprintf(format, args...)}
}

// RevisionCount is the denominator at one plan revision: the number of live nodes the revision left, how many there were before it, and which nodes came, went or were re-introduced.
// Revision 0 is the empty plan, so revision 1 reads previous 0 and is a change. DenominatorChanged is true when a node entered or left the plan; a node re-introduced under the same id (its
// spec or its incoming edges changed) is Updated and does not by itself change the denominator.
type RevisionCount struct {
	Revision                                         int64
	RecordedAt, RequestID, AuthorTaskID, StateDigest string
	CoordinatorEpoch                                 int64
	Nodes, PreviousNodes, Delta                      int
	DenominatorChanged                               bool
	Added, Retired, Updated                          []string
}

// Denominator is the head revision's: the node count, the one before it, and the last revision at which the set of live nodes changed.
type Denominator struct {
	Revision, PreviousRevision int64
	Nodes, PreviousNodes       int
	Delta                      int
	Changed                    bool
	LastChangedRevision        int64
}

// StageCount is one stage: how many nodes and which.
type StageCount struct {
	Stage   string
	Nodes   int
	NodeIDs []string
}

// BlockedEntry is one node of the blocked overlay.
type BlockedEntry struct{ NodeID, Stage, Reason, Detail string }

// BlockedOverlay lists the nodes whose disposition is blocked, each with the stage it stays in. It overlaps the stages and is never added to them.
type BlockedOverlay struct {
	Nodes   int
	Entries []BlockedEntry
}

// Measure is a count stated against the denominator it is a part of. Two measures are never added, and a ratio is not computed: integrated is a subset of accepted.
type Measure struct{ Nodes, Of int }

// Cumulative keeps what has been accepted and integrated apart from the stage distribution. Accepted is the live nodes that hold an active acceptance whatever has happened to them since
// (integrated, stale, paused, blocked); Integrated is the live nodes in the integrated state.
type Cumulative struct{ Accepted, Integrated Measure }

// OutsideDenominator is what a revision took out of the plan: node ids that are no longer live, how many of them had an acceptance and how many still hold an execution slot. Their accepted work is not in
// the numerator and they are not in the denominator, and a reader can see that.
type OutsideDenominator struct {
	Nodes, WithAcceptance, HoldingSlot int
	NodeIDs                            []string
}

// RelationshipLink, ThreadLink, ExecutionLink, ManagedLink and PullRequestLink are exact identifiers read from the store. A link the store does not hold is absent (null when printed).
type RelationshipLink struct {
	ID         string
	Generation int64
	Status     string
}

type ThreadLink struct{ ChildTaskID, ParentTaskID string }

type ExecutionLink struct {
	RelationshipID string
	Generation     int64
	Kind           string
}

type ManagedLink struct{ RequestID, State, ReceiptStatus string }

type PullRequestLink struct {
	Repository string
	Number     int64
	HeadSHA    string
	URL        string
	Source     string
	EventID    string
}

// NodeLinks are the places a node's records live. Relationship is the one the node stands on now, Executions every relationship the plan ever bound to it, Managed the managed start of a
// release that has no relationship yet.
type NodeLinks struct {
	Relationship *RelationshipLink
	Thread       *ThreadLink
	Executions   []ExecutionLink
	Managed      *ManagedLink
	PullRequest  *PullRequestLink
}

// NodeFacts are what the store holds of one node beyond its reading.
type NodeFacts struct {
	Title        string
	AcceptanceID string // the active acceptance; empty when there is none
	HoldsSlot    bool
	Links        NodeLinks
}

// NodeProgress is one live node: its stage, the reading's state, disposition, reason and detail, and what it is linked to.
type NodeProgress struct {
	NodeID, IssueKey, Kind, Title             string
	Stage, State, Disposition, Reason, Detail string
	Stale                                     *Stale
	AcceptanceID                              string
	HoldsSlot                                 bool
	Links                                     NodeLinks
}

// Progress is the projection. Reading is the reading it is built on (artifact bytes are not read), kept so a consumer that needs the nodes' dispositions asks nothing again.
type Progress struct {
	Reading     Reading
	ProjectKey  string
	Revisions   []RevisionCount
	Denominator Denominator
	Stages      []StageCount
	Blocked     BlockedOverlay
	Cumulative  Cumulative
	Outside     OutsideDenominator
	Nodes       []NodeProgress
	// Digest is the sha256 of the printed document without this key: equal store state, equal digest.
	Digest string
}

// ProgressInput is everything the pure projection needs: a reading, the denominators per revision, the facts of each node and what lies outside the denominator.
type ProgressInput struct {
	Reading    Reading
	ProjectKey string
	Revisions  []RevisionCount
	Facts      map[string]NodeFacts
	Outside    OutsideDenominator
}

// storeOnlyKey marks a context whose reads must touch nothing but the store: with the mark BuildManifest does not stat an artifact and the assignment view spells recovery commands with a
// fixed program name instead of resolving the relay's executable. Only Progress sets it, so dag-ready and every other caller are unchanged.
type storeOnlyKey struct{}

func withStoreOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, storeOnlyKey{}, true)
}

func storeOnly(ctx context.Context) bool {
	marked, _ := ctx.Value(storeOnlyKey{}).(bool)
	return marked
}

// progressProgramName spells the relay program in the recovery commands the assignment view builds under the store-only mark; the reading discards them.
const progressProgramName = "codex-session-relay"

func progressProgram() []string { return []string{progressProgramName} }

// ReadProgress is Progress inside one read transaction of the store, the way Read is Ready: every query shares one connection and so sees one state. Inside a composing transaction it joins
// the caller's; a caller inside a plain transaction, or one that holds its own querier, calls Progress.
func (s *Scheduler) ReadProgress(ctx context.Context, plan string) (Progress, error) {
	var out Progress
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.Progress(txCtx, s.Store.Q(txCtx), plan)
		return err
	})
	return out, err
}

// Progress reads the plan at its head revision and projects it. It writes nothing, reads no clock, no artifact byte and no artifact's metadata, runs no process and asks no forge: every
// answer is a function of the store, so two calls over one store state return equal documents. A plan or revision the store does not hold is the refusal unregistered_scope, as for dag-ready.
func (s *Scheduler) Progress(ctx context.Context, q store.Querier, plan string) (Progress, error) {
	ctx = withStoreOnly(ctx)
	reading, err := s.Ready(ctx, q, plan, ReadyOptions{SkipArtifactBytes: true})
	if err != nil {
		return Progress{}, err
	}
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return Progress{}, err
	}
	revisions, err := revisionCounts(ctx, q, plan, snap.Revision)
	if err != nil {
		return Progress{}, err
	}
	facts, err := s.nodeFacts(ctx, q, plan, snap)
	if err != nil {
		return Progress{}, err
	}
	outside, err := s.outsideDenominator(ctx, q, plan, snap)
	if err != nil {
		return Progress{}, err
	}
	return ProjectProgress(ProgressInput{Reading: reading, ProjectKey: snap.ProjectKey, Revisions: revisions, Facts: facts, Outside: outside})
}

// stageOf is the stage of a node's reading. An owned node keeps the stage of its derived state; a node nobody owns is waiting on a predecessor (an edge not satisfied, or an input that cannot
// be used), on a decision, or on a resource (a ready node held back by capacity, edit regions, a merge window, ownership or another owner of the issue), or ready. A combination the rule does not
// know is refused, so a node can neither be dropped from the distribution nor counted twice.
func stageOf(n NodeReading) (string, error) {
	switch n.State {
	case StateWaiting, StateReady:
		switch {
		case n.Disposition == DispReady && n.State == StateReady:
			return StageReady, nil
		case n.Reason == DeferAuthorityPending || n.Reason == BlockedDecisionMismatch:
			return StageWaitingDecision, nil
		case n.Disposition == DispWait || n.Disposition == DispBlocked:
			return StageWaitingPredecessor, nil
		case n.Disposition == DispDefer || n.Disposition == DispSkip:
			return StageWaitingResource, nil
		}
	case StateReleasing, StateCreationUnknown, StateRunning, StateReported, StateVerifying, StateCorrecting, StateAccepted, StateIntegrated, StateStale,
		StatePausedNode, StateCancelled, StateClosedNode, StateAmbiguousNode:
		return n.State, nil
	}
	return "", invariant("node %s has state %q and disposition %q, which no stage covers", n.NodeID, n.State, n.Disposition)
}

// explain refuses a node the reading leaves without a closed reason: every blocked, stale, waiting, deferred, skipped or finished node carries one of the reasons of the closed set, a blocked
// node a blocked reason and a stale node a stale reason.
func explain(n NodeReading) error {
	switch n.Disposition {
	case DispReady:
		return nil
	case DispBlocked, DispStale, DispWait, DispDefer, DispSkip, DispDone:
		if !ReasonsClosed(n.Reason) {
			return invariant("node %s is %s with %q, which is not a reason of the closed set", n.NodeID, n.Disposition, n.Reason)
		}
		if n.Disposition == DispBlocked && !strings.HasPrefix(n.Reason, "blocked:") || n.Disposition == DispStale && !strings.HasPrefix(n.Reason, "stale:") {
			return invariant("node %s is %s with the reason %q of another class", n.NodeID, n.Disposition, n.Reason)
		}
		return nil
	}
	return invariant("node %s has the unknown disposition %q", n.NodeID, n.Disposition)
}

// ProjectProgress is the pure half of the projection: it places every node of the reading in one stage, checks that every node that is not moving has its reason, derives the denominators'
// head and the cumulative counts, and fixes the digest. It does no I/O, so a consumer that rebuilt a reading from a snapshot passes it through here and gets the same guards.
func ProjectProgress(in ProgressInput) (Progress, error) {
	r := in.Reading
	if len(in.Revisions) == 0 {
		return Progress{}, invariant("plan %s has no revision", r.PlanID)
	}
	head := in.Revisions[len(in.Revisions)-1]
	if head.Revision != r.PlanRevision || head.Nodes != len(r.Nodes) {
		return Progress{}, invariant("plan %s: the revisions end at revision %d with %d nodes and the reading holds %d nodes at revision %d", r.PlanID, head.Revision, head.Nodes, len(r.Nodes), r.PlanRevision)
	}
	readings := append([]NodeReading(nil), r.Nodes...)
	sort.Slice(readings, func(i, j int) bool { return readings[i].NodeID < readings[j].NodeID })
	members := map[string][]string{}
	nodes := make([]NodeProgress, 0, len(readings))
	overlay := BlockedOverlay{Entries: []BlockedEntry{}}
	accepted := 0
	for _, n := range readings {
		stage, err := stageOf(n)
		if err != nil {
			return Progress{}, err
		}
		if err := explain(n); err != nil {
			return Progress{}, err
		}
		facts := in.Facts[n.NodeID]
		node := NodeProgress{NodeID: n.NodeID, IssueKey: n.IssueKey, Kind: n.Kind, Title: facts.Title, Stage: stage, State: n.State, Disposition: n.Disposition, Reason: n.Reason, Detail: n.Detail,
			AcceptanceID: facts.AcceptanceID, HoldsSlot: facts.HoldsSlot, Links: facts.Links}
		if n.Stale != nil {
			// the digest of the manifest rebuilt now is computed from the size of a file when a receipt declared none: it is not a function of the store, so it is not carried
			stale := *n.Stale
			stale.RebuiltManifest = ""
			node.Stale = &stale
		}
		if node.Links.Executions == nil {
			node.Links.Executions = []ExecutionLink{}
		}
		if facts.AcceptanceID != "" {
			accepted++
		}
		if n.Disposition == DispBlocked {
			overlay.Entries = append(overlay.Entries, BlockedEntry{NodeID: n.NodeID, Stage: stage, Reason: n.Reason, Detail: n.Detail})
		}
		members[stage] = append(members[stage], n.NodeID)
		nodes = append(nodes, node)
	}
	overlay.Nodes = len(overlay.Entries)
	stages := make([]StageCount, len(ProgressStages))
	for i, stage := range ProgressStages {
		stages[i] = StageCount{Stage: stage, Nodes: len(members[stage]), NodeIDs: append([]string{}, members[stage]...)}
	}
	denominator := Denominator{Revision: head.Revision, PreviousRevision: head.Revision - 1, Nodes: head.Nodes, PreviousNodes: head.PreviousNodes, Delta: head.Delta, Changed: head.DenominatorChanged}
	for _, rv := range in.Revisions {
		if rv.DenominatorChanged {
			denominator.LastChangedRevision = rv.Revision
		}
	}
	outside := in.Outside
	outside.NodeIDs = append([]string{}, outside.NodeIDs...)
	p := Progress{Reading: r, ProjectKey: in.ProjectKey, Revisions: in.Revisions, Denominator: denominator, Stages: stages, Blocked: overlay, Outside: outside, Nodes: nodes,
		Cumulative: Cumulative{Accepted: Measure{Nodes: accepted, Of: head.Nodes}, Integrated: Measure{Nodes: len(members[StageIntegrated]), Of: head.Nodes}}}
	printed, err := pyjson.Encode(p.body(), pyjson.Options{})
	if err != nil {
		return Progress{}, invariant("plan %s: the document cannot be printed: %v", r.PlanID, err)
	}
	sum := sha256.Sum256(printed)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}

func listOf(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (r RevisionCount) object() contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "revision", Value: r.Revision}, {Key: "recorded_at", Value: optionalText(r.RecordedAt)}, {Key: "request_id", Value: optionalText(r.RequestID)},
		{Key: "author_task_id", Value: optionalText(r.AuthorTaskID)}, {Key: "coordinator_epoch", Value: r.CoordinatorEpoch}, {Key: "state_digest", Value: optionalText(r.StateDigest)},
		{Key: "nodes", Value: r.Nodes}, {Key: "previous_nodes", Value: r.PreviousNodes}, {Key: "delta", Value: r.Delta}, {Key: "denominator_changed", Value: r.DenominatorChanged},
		{Key: "added", Value: listOf(r.Added)}, {Key: "retired", Value: listOf(r.Retired)}, {Key: "updated", Value: listOf(r.Updated)},
	}
}

func (m Measure) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "nodes", Value: m.Nodes}, {Key: "of", Value: m.Of}}
}

func (l NodeLinks) object() contract.OrderedObject {
	var relationship, thread, managed, pr any
	if l.Relationship != nil {
		relationship = contract.OrderedObject{{Key: "id", Value: l.Relationship.ID}, {Key: "execution_generation", Value: l.Relationship.Generation}, {Key: "status", Value: l.Relationship.Status}}
	}
	if l.Thread != nil {
		thread = contract.OrderedObject{{Key: "child_task_id", Value: optionalText(l.Thread.ChildTaskID)}, {Key: "parent_task_id", Value: optionalText(l.Thread.ParentTaskID)}}
	}
	if l.Managed != nil {
		managed = contract.OrderedObject{{Key: "request_id", Value: l.Managed.RequestID}, {Key: "state", Value: l.Managed.State}, {Key: "receipt_status", Value: optionalText(l.Managed.ReceiptStatus)}}
	}
	if l.PullRequest != nil {
		pr = contract.OrderedObject{{Key: "repository", Value: optionalText(l.PullRequest.Repository)}, {Key: "number", Value: l.PullRequest.Number}, {Key: "head_sha", Value: optionalText(l.PullRequest.HeadSHA)},
			{Key: "url", Value: optionalText(l.PullRequest.URL)}, {Key: "source", Value: l.PullRequest.Source}, {Key: "event_id", Value: optionalText(l.PullRequest.EventID)}}
	}
	executions := make([]any, len(l.Executions))
	for i, e := range l.Executions {
		executions[i] = contract.OrderedObject{{Key: "relationship_id", Value: e.RelationshipID}, {Key: "execution_generation", Value: e.Generation}, {Key: "kind", Value: e.Kind}}
	}
	return contract.OrderedObject{{Key: "relationship", Value: relationship}, {Key: "thread", Value: thread}, {Key: "executions", Value: executions}, {Key: "managed_start", Value: managed}, {Key: "pull_request", Value: pr}}
}

// staleObject is the stale reading as this command prints it: the reading's own object without the rebuilt manifest digest.
func staleObject(s Stale) contract.OrderedObject {
	var out contract.OrderedObject
	for _, f := range s.object() {
		if f.Key != "rebuilt_manifest_digest" {
			out = append(out, f)
		}
	}
	return out
}

func (n NodeProgress) object() contract.OrderedObject {
	o := contract.OrderedObject{
		{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "kind", Value: n.Kind}, {Key: "title", Value: optionalText(n.Title)},
		{Key: "stage", Value: n.Stage}, {Key: "state", Value: n.State}, {Key: "disposition", Value: n.Disposition}, {Key: "reason", Value: optionalText(n.Reason)}, {Key: "detail", Value: optionalText(n.Detail)},
	}
	if n.Stale != nil {
		o = append(o, contract.Field{Key: "stale", Value: staleObject(*n.Stale)})
	}
	return append(o, contract.Field{Key: "acceptance_id", Value: optionalText(n.AcceptanceID)}, contract.Field{Key: "holds_slot", Value: n.HoldsSlot}, contract.Field{Key: "links", Value: n.Links.object()})
}

// body is the printed document without its digest.
func (p Progress) body() contract.OrderedObject {
	revisions := make([]any, len(p.Revisions))
	for i, r := range p.Revisions {
		revisions[i] = r.object()
	}
	stages := make([]any, len(p.Stages))
	for i, s := range p.Stages {
		stages[i] = contract.OrderedObject{{Key: "stage", Value: s.Stage}, {Key: "nodes", Value: s.Nodes}, {Key: "node_ids", Value: listOf(s.NodeIDs)}}
	}
	entries := make([]any, len(p.Blocked.Entries))
	for i, e := range p.Blocked.Entries {
		entries[i] = contract.OrderedObject{{Key: "node_id", Value: e.NodeID}, {Key: "stage", Value: e.Stage}, {Key: "reason", Value: e.Reason}, {Key: "detail", Value: optionalText(e.Detail)}}
	}
	nodes := make([]any, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = n.object()
	}
	d := p.Denominator
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaProgress}, {Key: "plan_id", Value: p.Reading.PlanID}, {Key: "project_key", Value: p.ProjectKey},
		{Key: "plan_revision", Value: p.Reading.PlanRevision}, {Key: "state_digest", Value: p.Reading.StateDigest},
		{Key: "denominator", Value: contract.OrderedObject{{Key: "revision", Value: d.Revision}, {Key: "nodes", Value: d.Nodes}, {Key: "previous_revision", Value: d.PreviousRevision},
			{Key: "previous_nodes", Value: d.PreviousNodes}, {Key: "delta", Value: d.Delta}, {Key: "changed", Value: d.Changed}, {Key: "last_changed_revision", Value: d.LastChangedRevision}}},
		{Key: "revisions", Value: revisions}, {Key: "stages", Value: stages},
		{Key: "blocked", Value: contract.OrderedObject{{Key: "nodes", Value: p.Blocked.Nodes}, {Key: "entries", Value: entries}}},
		{Key: "cumulative", Value: contract.OrderedObject{{Key: "accepted", Value: p.Cumulative.Accepted.object()}, {Key: "integrated", Value: p.Cumulative.Integrated.object()}}},
		{Key: "outside_denominator", Value: contract.OrderedObject{{Key: "nodes", Value: p.Outside.Nodes}, {Key: "with_acceptance", Value: p.Outside.WithAcceptance},
			{Key: "holding_slot", Value: p.Outside.HoldingSlot}, {Key: "node_ids", Value: listOf(p.Outside.NodeIDs)}}},
		{Key: "nodes", Value: nodes},
	}
}

// Object is the document as dag-progress prints it.
func (p Progress) Object() contract.OrderedObject {
	return append(p.body(), contract.Field{Key: "digest", Value: p.Digest})
}

// revisionCounts reads the denominators of a plan's revisions 1 to head from the node log: a node version is live at revision r when it was introduced at or before r and not retired by r. The
// history is the stored log (the head was verified by the snapshot the reading took); each revision's recorded state digest is carried so a consumer can verify any of them.
func revisionCounts(ctx context.Context, q store.Querier, plan string, head int64) ([]RevisionCount, error) {
	versions, err := q.QueryContext(ctx, "SELECT node_id, introduced_rev, COALESCE(retired_rev, 0) FROM dag_nodes WHERE plan_id = ? ORDER BY introduced_rev, node_id", plan)
	if err != nil {
		return nil, err
	}
	type version struct {
		id                 string
		introduced, retire int64
	}
	var rows []version
	for versions.Next() {
		var v version
		if err := versions.Scan(&v.id, &v.introduced, &v.retire); err != nil {
			_ = versions.Close()
			return nil, err
		}
		rows = append(rows, v)
	}
	if err := versions.Err(); err != nil {
		_ = versions.Close()
		return nil, err
	}
	if err := versions.Close(); err != nil {
		return nil, err
	}
	meta := map[int64]RevisionCount{}
	log, err := q.QueryContext(ctx, "SELECT revision_no, request_id, author_task_id, coordinator_epoch, recorded_at, state_digest FROM dag_plan_revisions WHERE plan_id = ? ORDER BY revision_no", plan)
	if err != nil {
		return nil, err
	}
	for log.Next() {
		var r RevisionCount
		if err := log.Scan(&r.Revision, &r.RequestID, &r.AuthorTaskID, &r.CoordinatorEpoch, &r.RecordedAt, &r.StateDigest); err != nil {
			_ = log.Close()
			return nil, err
		}
		meta[r.Revision] = r
	}
	if err := log.Err(); err != nil {
		_ = log.Close()
		return nil, err
	}
	if err := log.Close(); err != nil {
		return nil, err
	}
	liveAt := func(r int64) map[string]bool {
		live := map[string]bool{}
		for _, v := range rows {
			if v.introduced <= r && (v.retire == 0 || v.retire > r) {
				live[v.id] = true
			}
		}
		return live
	}
	out := make([]RevisionCount, 0, head)
	previous := liveAt(0)
	for r := int64(1); r <= head; r++ {
		count, ok := meta[r]
		if !ok {
			return nil, invariant("plan %s has no revision row %d", plan, r)
		}
		current := liveAt(r)
		reintroduced := map[string]bool{}
		for _, v := range rows {
			if v.introduced == r {
				reintroduced[v.id] = true
			}
		}
		count.Nodes, count.PreviousNodes = len(current), len(previous)
		count.Delta = count.Nodes - count.PreviousNodes
		count.Added, count.Retired, count.Updated = []string{}, []string{}, []string{}
		for id := range current {
			switch {
			case !previous[id]:
				count.Added = append(count.Added, id)
			case reintroduced[id]:
				count.Updated = append(count.Updated, id)
			}
		}
		for id := range previous {
			if !current[id] {
				count.Retired = append(count.Retired, id)
			}
		}
		sort.Strings(count.Added)
		sort.Strings(count.Retired)
		sort.Strings(count.Updated)
		count.DenominatorChanged = len(count.Added)+len(count.Retired) > 0
		out = append(out, count)
		previous = current
	}
	return out, nil
}

// nodeFacts reads, for every live node, its title, its active acceptance, whether it holds an execution slot, and its links.
func (s *Scheduler) nodeFacts(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot) (map[string]NodeFacts, error) {
	out := make(map[string]NodeFacts, len(snap.Nodes))
	for _, n := range snap.Nodes {
		facts := NodeFacts{Title: n.Title}
		acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		if hasAcc {
			facts.AcceptanceID = acc.AcceptanceID
		}
		slot, err := s.slotOf(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		facts.HoldsSlot = slot != ""
		if facts.Links, err = s.nodeLinks(ctx, q, plan, n, acc, hasAcc); err != nil {
			return nil, err
		}
		out[n.NodeID] = facts
	}
	return out, nil
}

// nodeLinks collects the identifiers a node is reachable by: the relationship it stands on and every execution bound to it, the thread of that relationship, the managed start of a release that has
// no relationship yet, and the pull request.
func (s *Scheduler) nodeLinks(ctx context.Context, q store.Querier, plan string, n dag.SnapNode, acc Acceptance, hasAcc bool) (NodeLinks, error) {
	links := NodeLinks{Executions: []ExecutionLink{}}
	rows, err := q.QueryContext(ctx, "SELECT relationship_id, execution_generation, kind FROM dag_node_executions WHERE plan_id = ? AND node_id = ? ORDER BY relationship_id, execution_generation", plan, n.NodeID)
	if err != nil {
		return links, err
	}
	for rows.Next() {
		var e ExecutionLink
		if err := rows.Scan(&e.RelationshipID, &e.Generation, &e.Kind); err != nil {
			_ = rows.Close()
			return links, err
		}
		links.Executions = append(links.Executions, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return links, err
	}
	if err := rows.Close(); err != nil {
		return links, err
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil {
		return links, err
	}
	if found {
		var child string
		if _, err := queryOne(ctx, q, "SELECT child_task_id FROM relationships WHERE relationship_id = ?", []any{rel.ID}, &child); err != nil {
			return links, err
		}
		links.Relationship = &RelationshipLink{ID: rel.ID, Generation: rel.Generation, Status: rel.Status}
		links.Thread = &ThreadLink{ChildTaskID: child, ParentTaskID: rel.ParentTaskID}
	} else if links.Managed, links.Thread, err = managedStartOf(ctx, q, plan, n.NodeID); err != nil {
		return links, err
	}
	if n.Kind == dag.NodeImplementation {
		if links.PullRequest, err = s.pullRequestOf(ctx, q, rel, found, acc, hasAcc); err != nil {
			return links, err
		}
	}
	return links, nil
}

// managedStartOf is the managed start of the node's open intent (the release, or the successor release after a close, whose request nobody closed), and the child it already created: a release
// has a thread before it has a relationship. A node whose intent was closed has none.
func managedStartOf(ctx context.Context, q store.Querier, plan, node string) (*ManagedLink, *ThreadLink, error) {
	open, found, err := latestRelease(ctx, q, plan, node)
	if err != nil || !found {
		return nil, nil, err
	}
	request := open.Request
	var state string
	var receipt, child sql.NullString
	found, err = queryOne(ctx, q, "SELECT state, receipt_status, child_task_id FROM managed_start_requests WHERE request_id = ?", []any{request}, &state, &receipt, &child)
	if err != nil || !found {
		return nil, nil, err
	}
	var thread *ThreadLink
	if child.Valid && child.String != "" {
		thread = &ThreadLink{ChildTaskID: child.String}
	}
	return &ManagedLink{RequestID: request, State: state, ReceiptStatus: receipt.String}, thread, nil
}

// pullRequestOf is the pull request an implementation node is about. The forge identity recorded with the active acceptance comes first (repository and number from the forge row, the head
// the parent accepted); before any acceptance the work report of the relationship's current head event does, bound to that event, generation and revision so a later push cannot inherit an
// earlier report. No head, an ambiguous head or no report is no link: the store is not guessed from.
func (s *Scheduler) pullRequestOf(ctx context.Context, q store.Querier, rel relRow, hasRelationship bool, acc Acceptance, hasAcceptance bool) (*PullRequestLink, error) {
	if hasAcceptance {
		link := &PullRequestLink{HeadSHA: acc.HeadSHA, Number: acc.PRNumber, Source: PRSourceAcceptance}
		var forge string
		var number int64
		found, err := queryOne(ctx, q, "SELECT forge_repository, pr_number FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{acc.AcceptanceID}, &forge, &number)
		if err != nil {
			return nil, err
		}
		if found {
			link.Repository, link.Number = forge, number
		}
		if link.Number == 0 {
			return nil, nil
		}
		return link, nil
	}
	if !hasRelationship {
		return nil, nil
	}
	head, err := registry.HeadRevision(ctx, s.Store, rel.ID, rel.Generation)
	if err != nil {
		return nil, err
	}
	if head.EventID == "" || head.Ambiguous() {
		return nil, nil
	}
	link := &PullRequestLink{Source: PRSourceWorkReport, EventID: head.EventID}
	var url, headSHA sql.NullString
	found, err := queryOne(ctx, q, "SELECT repository, pr_number, pr_url, head_sha FROM work_reports WHERE event_id = ? AND relationship_id = ? AND execution_generation = ? AND revision_hash = ?"+
		" AND pr_number IS NOT NULL ORDER BY submission_no DESC LIMIT 1", []any{head.EventID, rel.ID, rel.Generation, head.RevisionHash}, &link.Repository, &link.Number, &url, &headSHA)
	if err != nil || !found {
		return nil, err
	}
	link.URL, link.HeadSHA = url.String, headSHA.String
	return link, nil
}

// outsideDenominator is what the plan's revisions took out: node ids that have a version in the log and are not live at the head, how many of them have an acceptance, and how many still hold
// an execution slot.
func (s *Scheduler) outsideDenominator(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot) (OutsideDenominator, error) {
	live := map[string]bool{}
	for _, n := range snap.Nodes {
		live[n.NodeID] = true
	}
	column := func(query string, args ...any) ([]string, error) {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		return out, rows.Close()
	}
	ever, err := column("SELECT DISTINCT node_id FROM dag_nodes WHERE plan_id = ?", plan)
	if err != nil {
		return OutsideDenominator{}, err
	}
	accepted, err := column("SELECT DISTINCT node_id FROM dag_acceptances WHERE plan_id = ?", plan)
	if err != nil {
		return OutsideDenominator{}, err
	}
	held, err := column("SELECT subject_key FROM execution_slots WHERE subject_kind = ? AND state = 'held'", SlotSubjectKind)
	if err != nil {
		return OutsideDenominator{}, err
	}
	hadAcceptance, holding := map[string]bool{}, map[string]bool{}
	for _, id := range accepted {
		hadAcceptance[id] = true
	}
	for _, key := range held {
		holding[key] = true
	}
	out := OutsideDenominator{NodeIDs: []string{}}
	for _, id := range ever {
		if live[id] {
			continue
		}
		out.NodeIDs = append(out.NodeIDs, id)
		if hadAcceptance[id] {
			out.WithAcceptance++
		}
		if holding[SlotSubjectKey(plan, id)] {
			out.HoldingSlot++
		}
	}
	sort.Strings(out.NodeIDs)
	out.Nodes = len(out.NodeIDs)
	return out, nil
}
