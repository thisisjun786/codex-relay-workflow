package dagsched

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The activity surface of a DAG plan (docs/relay/dag-progress.md "Activity", CRW-186). It is a standalone, read-only document beside dag-progress: dag-progress/1 and every digest built on it
// (the summary outbox's) are untouched, and nothing reads this document back. For each live node it states two things that are never one, and a stagnation reading:
//
//   - evidence: the last durable verification change recorded for the node (a ruling, a verified head, an acceptance, a re-validation, a merge-lane check, a commit-path verification, a pre-merge
//     record), with the relationship, generation, event identity, recorded time and source it was read from;
//   - activity: the last recorded child activity of any of the node's executions (a final child report, a turn admitted to a generation, a poll of a turn, an observed turn end, the host's
//     lifecycle observation of the child's thread), in the same terms.
//
// Activity is display only. It is never read by the stage, and the stage of a node is the progress projection's, taken in the same read transaction; so a child that only sends heartbeats moves no
// stage, no count and no evidence. The document reads the store alone and no clock: the times it prints are values the store recorded, and the "last" of a set is decided by comparing those
// recorded values with one another, never with the present.

// SchemaActivity names the document dag-activity prints.
const SchemaActivity = "dag-activity/1"

// The labels of the two entries a node carries.
const (
	LabelEvidence = "evidence"
	LabelActivity = "activity"
)

// The sources of an evidence entry, as the entry names them.
const (
	EvidenceVerdict              = "verdict"
	EvidenceVerifiedHead         = "verified_head"
	EvidenceAcceptance           = "acceptance"
	EvidenceBaseRefresh          = "base_refresh"
	EvidenceRevalidation         = "revalidation"
	EvidenceMergeCheck           = "merge_check"
	EvidenceCommitVerification   = "commit_verification"
	EvidencePremerge             = "premerge_record"
	EvidenceRevalidationPremerge = "revalidation_premerge"
)

// The sources of an activity entry.
const (
	ActivityChildEvent      = "child_event"
	ActivityTurnAdmitted    = "turn_admitted"
	ActivityPoll            = "poll"
	ActivityTurnObservation = "turn_observation"
	ActivityLifecycle       = "child_lifecycle"
)

// ActivityEntry is one recorded row, named by what it is: the relationship and generation it belongs to (the generation is absent for a row the store keys by turn alone), the event it is about (absent
// for a row that is not about one), Ref (the stored identity of the row itself: a turn, a check, an acceptance, a record digest or the child's thread), Change (what the row says: an outcome, a
// verdict, a status), the time the store recorded it and the source it was read from.
type ActivityEntry struct {
	Label          string
	RelationshipID string
	Generation     int64
	HasGeneration  bool
	EventID        string
	Ref            string
	Change         string
	RecordedAt     string
	Source         string
}

// ArtifactLink is the artifact a non_pr node's active acceptance stands on, by the identities the acceptance stored.
type ArtifactLink struct {
	AcceptanceID, RelationshipID, EventID, RevisionHash, OutputManifestRef, EvidenceDigest string
	Generation                                                                             int64
}

// CommitLink is the commit path of an acceptance made without a forge (a commit-path acceptance): the commits and tree its verification record names and the integration refs its batches moved.
type CommitLink struct {
	AcceptanceID, HeadCommit, TreeSHA, BaseCommit, RecordDigest, RecordedAt string
	IntegrationRefs                                                         []string
}

// NodeActivity is one live node of the document.
type NodeActivity struct {
	NodeID, IssueKey, Kind, Title             string
	Stage, State, Disposition, Reason, Detail string
	Evidence, Activity                        *ActivityEntry
	Stagnation                                *Stagnation
	Relationship                              *RelationshipLink
	Thread                                    *ThreadLink
	PullRequest                               *PullRequestLink
	Artifact                                  *ArtifactLink
	Commit                                    *CommitLink
}

// Activity is the document. Progress is the dag-progress view the stages were taken from, in the same read transaction, so the document and that view are one state of the store.
type Activity struct {
	Progress Progress
	// ProgressCursor is the cursor of the progress view in its snapshot form: a reader that rebuilds the view by replay ends at a snapshot whose cursor is this and whose projection has ProgressDigest.
	ProgressCursor string
	Stagnation     StagnationCounts
	Nodes          []NodeActivity
	// Digest is the sha256 of the printed document without this key: equal store state, equal digest.
	Digest string
}

// ReadActivity is Activity inside one read transaction of the store, the way ReadProgress is Progress.
func (s *Scheduler) ReadActivity(ctx context.Context, plan string) (Activity, error) {
	var out Activity
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.Activity(txCtx, s.Store.Q(txCtx), plan)
		return err
	})
	return out, err
}

// Activity reads the plan's progress and, beside each node's stage, the last evidence and the last activity recorded for it, its stagnation reading and the exact links of its artifact or commit
// path. It writes nothing, reads no clock, no artifact byte and no forge, so two calls over one store state return equal documents.
func (s *Scheduler) Activity(ctx context.Context, q store.Querier, plan string) (Activity, error) {
	snapshot, progress, err := s.ProgressSnapshotOf(ctx, q, plan)
	if err != nil {
		return Activity{}, err
	}
	ctx = withStoreOnly(ctx)
	tables := &tableSet{q: q, known: map[string]bool{}}
	readings := map[string]NodeReading{}
	for _, n := range progress.Reading.Nodes {
		readings[n.NodeID] = n
	}
	out := Activity{Progress: progress, ProgressCursor: snapshot.Cursor().String(), Stagnation: progress.Reading.Pass.Stagnation, Nodes: make([]NodeActivity, 0, len(progress.Nodes))}
	for _, n := range progress.Nodes {
		node := NodeActivity{NodeID: n.NodeID, IssueKey: n.IssueKey, Kind: n.Kind, Title: n.Title, Stage: n.Stage, State: n.State, Disposition: n.Disposition, Reason: n.Reason, Detail: n.Detail,
			Stagnation: readings[n.NodeID].Stagnation, Relationship: n.Links.Relationship, Thread: n.Links.Thread, PullRequest: n.Links.PullRequest}
		relationships := relationshipsOf(n.Links)
		if node.Evidence, err = lastEntry(ctx, tables, plan, n.NodeID, relationships, LabelEvidence, evidenceSources); err != nil {
			return Activity{}, err
		}
		if node.Activity, err = lastEntry(ctx, tables, plan, n.NodeID, relationships, LabelActivity, activitySources); err != nil {
			return Activity{}, err
		}
		if n.AcceptanceID != "" {
			acc, has, err := tolerantAcceptanceOf(ctx, q, plan, n.NodeID)
			if err != nil {
				return Activity{}, err
			}
			if has {
				if node.Artifact = artifactOf(n.Kind, acc); node.Artifact != nil {
					node.Artifact.Generation = acc.ExecutionGeneration
				}
				if node.Commit, err = commitLinkOf(ctx, q, tables, acc); err != nil {
					return Activity{}, err
				}
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	printed, err := pyjson.Encode(out.body(), pyjson.Options{})
	if err != nil {
		return Activity{}, invariant("plan %s: the activity document cannot be printed: %v", plan, err)
	}
	out.Digest = shaOf(printed)
	return out, nil
}

func relationshipsOf(l NodeLinks) []string {
	seen := map[string]bool{}
	var ids []string
	for _, e := range l.Executions {
		if e.ExecutionID != "" {
			continue // an execution of an agent has no relationship: its evidence is read by node
		}
		if !seen[e.RelationshipID] {
			seen[e.RelationshipID] = true
			ids = append(ids, e.RelationshipID)
		}
	}
	sort.Strings(ids)
	return ids
}

// artifactOf is the artifact link of a non_pr node's acceptance: nothing for any other kind of node.
func artifactOf(kind string, a Acceptance) *ArtifactLink {
	if kind != dag.NodeNonPR || a.agent != nil { // an agent acceptance names no output manifest
		return nil
	}
	return &ArtifactLink{AcceptanceID: a.AcceptanceID, RelationshipID: a.RelationshipID, EventID: a.EventID, RevisionHash: a.RevisionHash, OutputManifestRef: a.OutputManifestRef, EvidenceDigest: a.EvidenceDigest}
}

// commitLinkOf is the commit-path link of an acceptance: present only for an acceptance made by the commit path (a verification row and no forge row).
func commitLinkOf(ctx context.Context, q store.Querier, tables *tableSet, a Acceptance) (*CommitLink, error) {
	if a.agent != nil {
		// the commit proof of an agent acceptance is in its row: the verification record, and the integration refs its batches moved (integrationRefsOf reads the agent stage rows too)
		if a.agent.Head == "" {
			return nil, nil
		}
		link := &CommitLink{AcceptanceID: a.AcceptanceID, HeadCommit: a.agent.Head, TreeSHA: a.agent.TreeSHA, BaseCommit: a.agent.Base, RecordDigest: a.agent.VerificationDigest, RecordedAt: a.agent.AcceptedAt}
		refs, err := integrationRefsOf(ctx, q, a.AcceptanceID)
		if err != nil {
			return nil, err
		}
		if refs == nil {
			refs = []string{}
		}
		link.IntegrationRefs = refs
		return link, nil
	}
	if ok, err := tables.has(ctx, "dag_acceptance_verifications"); err != nil || !ok {
		return nil, err
	}
	committed, err := commitAccepted(ctx, q, a.AcceptanceID)
	if err != nil || !committed {
		return nil, err
	}
	link := &CommitLink{AcceptanceID: a.AcceptanceID}
	found, err := queryOne(ctx, q, "SELECT head_commit, tree_sha, base_commit, record_digest, recorded_at FROM dag_acceptance_verifications WHERE acceptance_id = ?", []any{a.AcceptanceID},
		&link.HeadCommit, &link.TreeSHA, &link.BaseCommit, &link.RecordDigest, &link.RecordedAt)
	if err != nil || !found {
		return nil, err
	}
	if ok, err := tables.has(ctx, "dag_integration_stages"); err != nil {
		return nil, err
	} else if ok {
		if link.IntegrationRefs, err = integrationRefsOf(ctx, q, a.AcceptanceID); err != nil {
			return nil, err
		}
	}
	if link.IntegrationRefs == nil {
		link.IntegrationRefs = []string{}
	}
	return link, nil
}

// tableSet answers whether a store holds a table: a store that predates a table has no rows of it, and is answered without a query that would fail on it.
type tableSet struct {
	q     store.Querier
	known map[string]bool
}

func (t *tableSet) has(ctx context.Context, name string) (bool, error) {
	if v, ok := t.known[name]; ok {
		return v, nil
	}
	var found string
	ok, err := queryOne(ctx, t.q, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", []any{name}, &found)
	if err != nil {
		return false, err
	}
	t.known[name] = ok
	return ok, nil
}

// An entrySource is one table a label reads: the table it needs, the source it names, and the query. The query takes the plan and node (planNode) or the node's relationships (in), and returns the
// columns relationship_id, execution_generation, event_id, ref, change, recorded_at, in that order.
type entrySource struct {
	table, source, query string
	by                   scope
	// read replaces the query for a source whose rows need more than SQL can say (a digest to check, an identity to resolve): it returns the node's rows of this source.
	read func(ctx context.Context, tables *tableSet, plan, node string) ([]ActivityEntry, error)
}

type scope int

const (
	byNode scope = iota
	byRelationships
)

var evidenceSources = []entrySource{
	{"verdicts", EvidenceVerdict, "SELECT e.relationship_id, e.execution_generation, v.event_id, v.verdict_turn_id, v.verdict, v.decided_at FROM verdicts v JOIN events e ON e.event_id = v.event_id WHERE e.relationship_id IN (%s)", byRelationships, nil},
	{"dag_verified_heads", EvidenceVerifiedHead, "SELECT relationship_id, execution_generation, event_id, verdict_turn_id, head_sha, recorded_at FROM dag_verified_heads WHERE relationship_id IN (%s)", byRelationships, nil},
	{table: "dag_base_refreshes", source: EvidenceBaseRefresh, by: byNode, read: baseRefreshEntries},
	// the acceptance of an agent node (R06): no relationship, generation or event; the state is the row's place in the node's acceptance chain
	{"dag_agent_acceptances", EvidenceAcceptance, "SELECT '', NULL, '', a.acceptance_id, CASE WHEN EXISTS (SELECT 1 FROM dag_agent_acceptances n WHERE n.supersedes_acceptance_id = a.acceptance_id) THEN 'superseded' ELSE 'active' END, a.accepted_at FROM dag_agent_acceptances a WHERE a.plan_id = ? AND a.node_id = ?", byNode, nil},
	{"dag_acceptances", EvidenceAcceptance, "SELECT relationship_id, execution_generation, event_id, acceptance_id, state, accepted_at FROM dag_acceptances WHERE plan_id = ? AND node_id = ?", byNode, nil},
	// A revalidation names the event it was ruled on, which after a base refresh stands on a later generation than the acceptance: the entry takes that event's relationship and generation, and no
	// generation when the store does not hold the event.
	{"dag_acceptance_revalidations", EvidenceRevalidation, "SELECT COALESCE(e.relationship_id, a.relationship_id), e.execution_generation, r.event_id, r.revalidation_id, r.criteria_set_digest, r.revalidated_at FROM dag_acceptance_revalidations r JOIN dag_acceptances a ON a.acceptance_id = r.acceptance_id LEFT JOIN events e ON e.event_id = r.event_id WHERE a.plan_id = ? AND a.node_id = ?", byNode, nil},
	{table: "dag_merge_checks", source: EvidenceMergeCheck, by: byNode, read: mergeCheckEntries},
	{"dag_acceptance_verifications", EvidenceCommitVerification, "SELECT a.relationship_id, a.execution_generation, a.event_id, v.record_digest, v.head_commit, v.recorded_at FROM dag_acceptance_verifications v JOIN dag_acceptances a ON a.acceptance_id = v.acceptance_id WHERE a.plan_id = ? AND a.node_id = ?", byNode, nil},
	// A pre-merge record is stored for the head the stand was on (accepted_head): the entry takes the identity of that head, as a merge check does. The re-validation's record is the one integration
	// reads once the criteria were re-registered, and is stored after the re-validation row itself.
	{"dag_agent_acceptances", EvidenceCommitVerification, "SELECT '', NULL, '', a.verification_digest, a.head, a.accepted_at FROM dag_agent_acceptances a WHERE a.plan_id = ? AND a.node_id = ? AND a.verification_digest <> ''", byNode, nil},
	{"dag_agent_acceptances", EvidencePremerge, "SELECT '', NULL, '', a.premerge_digest, a.premerge_evaluated_head, a.accepted_at FROM dag_agent_acceptances a WHERE a.plan_id = ? AND a.node_id = ? AND a.premerge_digest <> ''", byNode, nil},
	{table: "dag_acceptance_premerge", source: EvidencePremerge, by: byNode, read: premergeEntriesOf("dag_acceptance_premerge")},
	{table: "dag_revalidation_premerge", source: EvidenceRevalidationPremerge, by: byNode, read: premergeEntriesOf("dag_revalidation_premerge")},
}

var activitySources = []entrySource{
	{"events", ActivityChildEvent, "SELECT relationship_id, execution_generation, event_id, turn_id, outcome, last_seen_at FROM events WHERE producer = 'child' AND relationship_id IN (%s)", byRelationships, nil},
	{"generation_turns", ActivityTurnAdmitted, "SELECT relationship_id, execution_generation, NULL, turn_id, evidence, admitted_at FROM generation_turns WHERE relationship_id IN (%s)", byRelationships, nil},
	// A poll row keeps the status of the newest attempt and the time of the newest successful one: when that attempt failed (the turn absent, a transport error) the status is not the one observed at
	// the printed time, so the entry keeps the time (the child answered then) and states no status.
	{"poll_observations", ActivityPoll, "SELECT relationship_id, execution_generation, NULL, turn_id, CASE WHEN last_error IS NULL THEN last_status END, last_polled_at FROM poll_observations WHERE last_polled_at IS NOT NULL AND relationship_id IN (%s)", byRelationships, nil},
	// An observation is keyed by the turn, and one turn may be admitted under several generations: it names the generation of its stored event, else the one generation that admitted the turn, else none.
	{"observations", ActivityTurnObservation, "SELECT o.relationship_id, COALESCE((SELECT e.execution_generation FROM events e WHERE e.event_id = o.event_id AND e.relationship_id = o.relationship_id), (SELECT CASE WHEN COUNT(DISTINCT g.execution_generation) = 1 THEN MIN(g.execution_generation) END FROM generation_turns g WHERE g.relationship_id = o.relationship_id AND g.turn_id = o.turn_id)), o.event_id, o.turn_id, o.terminal_status, o.observed_at FROM observations o WHERE o.relationship_id IN (%s)", byRelationships, nil},
	{"recipient_lifecycle", ActivityLifecycle, "SELECT r.relationship_id, NULL, NULL, l.task_id, l.runtime_status, l.observed_at FROM recipient_lifecycle l JOIN relationships r ON r.child_task_id = l.task_id WHERE r.relationship_id IN (%s)", byRelationships, nil},
}

// lastEntry is the newest row among the label's sources for the node, or nil when the store holds none. "Newest" compares the recorded times with one another; a tie is broken by the source, the
// relationship, the generation and the stored identities, so two readings of one store state pick the same row.
func lastEntry(ctx context.Context, tables *tableSet, plan, node string, relationships []string, label string, sources []entrySource) (*ActivityEntry, error) {
	var best *ActivityEntry
	for _, src := range sources {
		if src.by == byRelationships && len(relationships) == 0 {
			continue
		}
		if ok, err := tables.has(ctx, src.table); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		if src.read != nil {
			entries, err := src.read(ctx, tables, plan, node)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				e.Label, e.Source = label, src.source
				if best == nil || newerEntry(e, *best) {
					kept := e
					best = &kept
				}
			}
			continue
		}
		query, args := src.query, []any{}
		if src.by == byNode {
			args = append(args, plan, node)
		} else {
			query = strings.Replace(query, "%s", strings.TrimSuffix(strings.Repeat("?,", len(relationships)), ","), 1)
			for _, id := range relationships {
				args = append(args, id)
			}
		}
		rows, err := tables.q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			e := ActivityEntry{Label: label, Source: src.source}
			var generation sql.NullInt64
			var event, ref, change, at sql.NullString
			if err := rows.Scan(&e.RelationshipID, &generation, &event, &ref, &change, &at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !at.Valid || at.String == "" {
				continue
			}
			e.Generation, e.HasGeneration = generation.Int64, generation.Valid
			e.EventID, e.Ref, e.Change, e.RecordedAt = event.String, ref.String, change.String, at.String
			if best == nil || newerEntry(e, *best) {
				kept := e
				best = &kept
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return best, nil
}

// refreshRow is a base refresh recorded for an acceptance that digests to its id, with the acceptance it belongs to.
type refreshRow struct {
	acceptanceID, refreshID, relationshipID, eventID, head, recordedAt string
	generation                                                         int64
}

// validRefreshes are the node's base refreshes that count: the rows of acceptance.Stands (the refresh names the acceptance's relationship, stands on its generation or a later one, and its id is the
// digest of what it records), with the time the store recorded each. A store that predates the table, or the acceptances, has none.
func validRefreshes(ctx context.Context, tables *tableSet, plan, node string) ([]refreshRow, error) {
	for _, name := range []string{"dag_base_refreshes", "dag_acceptances"} {
		if ok, err := tables.has(ctx, name); err != nil || !ok {
			return nil, err
		}
	}
	rows, err := tables.q.QueryContext(ctx, "SELECT r.acceptance_id, a.relationship_id, a.execution_generation, r.refresh_id, r.relationship_id, r.execution_generation, r.event_id, r.revision_hash, r.head_sha,"+
		" r.base_repository, r.base_ref, r.base_tip_sha, r.proof_json, r.resolved_paths_json, r.recorded_at FROM dag_base_refreshes r JOIN dag_acceptances a ON a.acceptance_id = r.acceptance_id"+
		" WHERE a.plan_id = ? AND a.node_id = ? ORDER BY r.acceptance_id, r.refresh_seq", plan, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []refreshRow
	for rows.Next() {
		var accRelationship, revision, baseRepo, baseRef, baseTip, proof, resolved string
		var accGeneration int64
		var at sql.NullString
		r := refreshRow{}
		if err := rows.Scan(&r.acceptanceID, &accRelationship, &accGeneration, &r.refreshID, &r.relationshipID, &r.generation, &r.eventID, &revision, &r.head, &baseRepo, &baseRef, &baseTip, &proof, &resolved, &at); err != nil {
			return nil, err
		}
		if r.relationshipID != accRelationship || r.generation < accGeneration ||
			r.refreshID != acceptance.RefreshDigest(r.acceptanceID, r.relationshipID, r.generation, r.eventID, revision, r.head, baseRepo, baseRef, baseTip, proof, resolved) {
			continue
		}
		r.recordedAt = at.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// baseRefreshEntries are the node's valid base refreshes as evidence: after one, the acceptance stands on the refreshed head, and that is the latest durable change to what the node was verified at.
func baseRefreshEntries(ctx context.Context, tables *tableSet, plan, node string) ([]ActivityEntry, error) {
	refreshes, err := validRefreshes(ctx, tables, plan, node)
	if err != nil {
		return nil, err
	}
	var out []ActivityEntry
	for _, r := range refreshes {
		if r.recordedAt == "" {
			continue
		}
		out = append(out, ActivityEntry{RelationshipID: r.relationshipID, Generation: r.generation, HasGeneration: true, EventID: r.eventID, Ref: r.refreshID, Change: r.head, RecordedAt: r.recordedAt})
	}
	return out, nil
}

// mergeCheckEntries are the node's merge checks as evidence. A check judges the head its acceptance stood on when it was made (head_sha), so the entry takes the generation and event of that head: the
// acceptance's own, or the valid base refresh whose head it is. A check of a head neither names has no generation and no event: the store cannot say which execution it judged.
func mergeCheckEntries(ctx context.Context, tables *tableSet, plan, node string) ([]ActivityEntry, error) {
	if ok, err := tables.has(ctx, "dag_acceptances"); err != nil || !ok {
		return nil, err
	}
	refreshes, err := validRefreshes(ctx, tables, plan, node)
	if err != nil {
		return nil, err
	}
	rows, err := tables.q.QueryContext(ctx, "SELECT a.acceptance_id, a.relationship_id, a.execution_generation, a.event_id, a.head_sha, c.head_sha, c.check_id, c.outcome, c.recorded_at FROM dag_merge_checks c"+
		" JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id WHERE a.plan_id = ? AND a.node_id = ?", plan, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityEntry
	for rows.Next() {
		var acceptanceID, relationship, event, checkID, outcome string
		var generation int64
		var ownHead, judged, at sql.NullString
		if err := rows.Scan(&acceptanceID, &relationship, &generation, &event, &ownHead, &judged, &checkID, &outcome, &at); err != nil {
			return nil, err
		}
		if at.String == "" {
			continue
		}
		e := ActivityEntry{RelationshipID: relationship, Ref: checkID, Change: outcome, RecordedAt: at.String}
		switch {
		case judged.String == ownHead.String:
			e.Generation, e.HasGeneration, e.EventID = generation, true, event
		default:
			// the newest valid refresh recorded for this head, as the stand reads them (newest first)
			for i := len(refreshes) - 1; i >= 0; i-- {
				if r := refreshes[i]; r.acceptanceID == acceptanceID && r.head == judged.String {
					e.RelationshipID, e.Generation, e.HasGeneration, e.EventID = r.relationshipID, r.generation, true, r.eventID
					break
				}
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// headIdentity is the generation and event a head is tied to for an acceptance: the acceptance's own when the head is the one it was accepted at, the refresh's when it is the head of a valid base
// refresh of the acceptance. A head that neither names, or that more than one execution names, is tied to none: the store cannot say which execution it belongs to.
func headIdentity(head, ownHead string, own refreshRow, refreshes []refreshRow) (generation int64, event string, ok bool) {
	type identity struct {
		generation int64
		event      string
	}
	var found []identity
	add := func(i identity) {
		for _, f := range found {
			if f == i {
				return
			}
		}
		found = append(found, i)
	}
	if head == ownHead {
		add(identity{own.generation, own.eventID})
	}
	for _, r := range refreshes {
		if r.acceptanceID == own.acceptanceID && r.head == head {
			add(identity{r.generation, r.eventID})
		}
	}
	if len(found) != 1 {
		return 0, "", false
	}
	return found[0].generation, found[0].event, true
}

// premergeEntriesOf are the node's pre-merge records of one table as evidence. A record names the identity of the head it was stored for (accepted_head), resolved against the acceptance and its
// valid base refreshes; a record of a head they do not name uniquely has no generation and no event.
func premergeEntriesOf(table string) func(ctx context.Context, tables *tableSet, plan, node string) ([]ActivityEntry, error) {
	return func(ctx context.Context, tables *tableSet, plan, node string) ([]ActivityEntry, error) {
		if ok, err := tables.has(ctx, "dag_acceptances"); err != nil || !ok {
			return nil, err
		}
		refreshes, err := validRefreshes(ctx, tables, plan, node)
		if err != nil {
			return nil, err
		}
		rows, err := tables.q.QueryContext(ctx, "SELECT a.acceptance_id, a.relationship_id, a.execution_generation, a.event_id, a.head_sha, p.accepted_head, p.record_digest, p.evaluated_head, p.recorded_at FROM "+table+" p"+
			" JOIN dag_acceptances a ON a.acceptance_id = p.acceptance_id WHERE a.plan_id = ? AND a.node_id = ?", plan, node)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []ActivityEntry
		for rows.Next() {
			var acceptanceID, relationship, event, ownHead, acceptedHead, digest, evaluated string
			var generation int64
			var at sql.NullString
			if err := rows.Scan(&acceptanceID, &relationship, &generation, &event, &ownHead, &acceptedHead, &digest, &evaluated, &at); err != nil {
				return nil, err
			}
			if at.String == "" {
				continue
			}
			e := ActivityEntry{RelationshipID: relationship, Ref: digest, Change: evaluated, RecordedAt: at.String}
			if g, ev, ok := headIdentity(acceptedHead, ownHead, refreshRow{acceptanceID: acceptanceID, generation: generation, eventID: event}, refreshes); ok {
				e.Generation, e.HasGeneration, e.EventID = g, true, ev
			}
			out = append(out, e)
		}
		return out, rows.Err()
	}
}

// recordedTime reads a recorded time; ok is false for text that is not a timestamp (such text still orders, by its bytes, below every timestamp).
func recordedTime(text string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, text)
	return t, err == nil
}

// newerEntry is whether a was recorded after b. It compares what the store recorded and never the present.
func newerEntry(a, b ActivityEntry) bool {
	ta, oka := recordedTime(a.RecordedAt)
	tb, okb := recordedTime(b.RecordedAt)
	switch {
	case oka && okb:
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
	case oka != okb:
		return oka
	case a.RecordedAt != b.RecordedAt:
		return a.RecordedAt > b.RecordedAt
	}
	if a.Source != b.Source {
		return a.Source > b.Source
	}
	if a.RelationshipID != b.RelationshipID {
		return a.RelationshipID > b.RelationshipID
	}
	if a.Generation != b.Generation {
		return a.Generation > b.Generation
	}
	if a.Ref != b.Ref {
		return a.Ref > b.Ref
	}
	return a.EventID > b.EventID
}

func (e ActivityEntry) object() contract.OrderedObject {
	var generation any
	if e.HasGeneration {
		generation = e.Generation
	}
	return contract.OrderedObject{
		{Key: "label", Value: e.Label}, {Key: "relationship_id", Value: e.RelationshipID}, {Key: "execution_generation", Value: generation}, {Key: "event_id", Value: optionalText(e.EventID)},
		{Key: "ref", Value: optionalText(e.Ref)}, {Key: "change", Value: optionalText(e.Change)}, {Key: "recorded_at", Value: e.RecordedAt}, {Key: "source", Value: e.Source},
	}
}

func (a *ArtifactLink) object() any {
	if a == nil {
		return nil
	}
	return contract.OrderedObject{
		{Key: "acceptance_id", Value: a.AcceptanceID}, {Key: "relationship_id", Value: a.RelationshipID}, {Key: "execution_generation", Value: a.Generation}, {Key: "event_id", Value: a.EventID},
		{Key: "revision_hash", Value: a.RevisionHash}, {Key: "output_manifest_ref", Value: optionalText(a.OutputManifestRef)}, {Key: "evidence_digest", Value: optionalText(a.EvidenceDigest)},
	}
}

func (c *CommitLink) object() any {
	if c == nil {
		return nil
	}
	return contract.OrderedObject{
		{Key: "acceptance_id", Value: c.AcceptanceID}, {Key: "head_commit", Value: c.HeadCommit}, {Key: "tree_sha", Value: c.TreeSHA}, {Key: "base_commit", Value: c.BaseCommit},
		{Key: "record_digest", Value: c.RecordDigest}, {Key: "recorded_at", Value: c.RecordedAt}, {Key: "integration_refs", Value: listOf(c.IntegrationRefs)},
	}
}

func entryValue(e *ActivityEntry) any {
	if e == nil {
		return nil
	}
	return e.object()
}

func (n NodeActivity) object() contract.OrderedObject {
	var stagnation any
	if n.Stagnation != nil {
		stagnation = n.Stagnation.object()
	}
	links := NodeLinks{Relationship: n.Relationship, Thread: n.Thread, PullRequest: n.PullRequest}.object()
	return contract.OrderedObject{
		{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "kind", Value: n.Kind}, {Key: "title", Value: optionalText(n.Title)},
		{Key: "stage", Value: n.Stage}, {Key: "state", Value: n.State}, {Key: "disposition", Value: n.Disposition}, {Key: "reason", Value: optionalText(n.Reason)}, {Key: "detail", Value: optionalText(n.Detail)},
		{Key: "evidence", Value: entryValue(n.Evidence)}, {Key: "activity", Value: entryValue(n.Activity)}, {Key: "stagnation", Value: stagnation},
		{Key: "links", Value: contract.OrderedObject{{Key: "relationship", Value: linkValue(links, "relationship")}, {Key: "thread", Value: linkValue(links, "thread")},
			{Key: "pull_request", Value: linkValue(links, "pull_request")}, {Key: "artifact", Value: n.Artifact.object()}, {Key: "commit", Value: n.Commit.object()}}},
	}
}

// linkValue picks one member of a node's links as dag-progress prints it, so the two documents spell a link the same way.
func linkValue(links contract.OrderedObject, key string) any {
	for _, f := range links {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// body is the printed document without its digest.
func (a Activity) body() contract.OrderedObject {
	nodes := make([]any, len(a.Nodes))
	for i, n := range a.Nodes {
		nodes[i] = n.object()
	}
	r := a.Progress.Reading
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaActivity}, {Key: "plan_id", Value: r.PlanID}, {Key: "project_key", Value: a.Progress.ProjectKey},
		{Key: "plan_revision", Value: r.PlanRevision}, {Key: "state_digest", Value: r.StateDigest}, {Key: "plan_state", Value: optionalText(a.Progress.PlanState)},
		{Key: "progress", Value: contract.OrderedObject{{Key: "schema", Value: SchemaProgress}, {Key: "digest", Value: a.Progress.Digest}, {Key: "cursor", Value: a.ProgressCursor}}},
		{Key: "stagnation", Value: a.Stagnation.object()},
		{Key: "nodes", Value: nodes},
	}
}

// Object is the document as dag-activity prints it.
func (a Activity) Object() contract.OrderedObject {
	return append(a.body(), contract.Field{Key: "digest", Value: a.Digest})
}
