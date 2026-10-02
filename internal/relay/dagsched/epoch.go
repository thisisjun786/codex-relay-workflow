package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The coordinator epoch, restart and adoption (docs/relay/dag-scheduler.md, "The coordinator epoch" and "Restart and adoption"; contract 5 and 6).
// The fence itself is dag.CheckCoordinatorEpoch: it is the first statement of every write that decides, inside the transaction that writes.

// SchemaClaim, SchemaAdopt and SchemaRestart name the documents dag-coordinator-claim, dag-adopt and dag-restart print.
const (
	SchemaClaim   = "dag-coordinator-claim/1"
	SchemaAdopt   = "dag-adopt/1"
	SchemaRestart = "dag-restart/1"
)

// fence checks, in the caller's transaction (or, from a read, before any effect), that this scheduler's session holds the plan's coordinator epoch.
// The project is the plan header's: every fenced write is on a plan that has one.
func (s *Scheduler) fence(ctx context.Context, q store.Querier, plan, actor string) error {
	return dag.CheckCoordinatorEpoch(ctx, q, plan, "", actor, s.ExpectedEpoch)
}

// claimText is the shape of the identifiers a claim carries (the plan's own identifier shape: letters, digits and . _ : -, at most 128 characters).
var claimText = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ClaimInput is what a parent session gives to raise a plan's coordinator epoch: the project (needed only while the plan has no revision, hence no
// header), the task that is the project's parent and a nonce that names this session once and is never reused.
type ClaimInput struct{ Project, Actor, SessionNonce string }

// ClaimResult is the claim that holds the plan after ClaimEpoch, and what it replaced.
type ClaimResult struct {
	dag.Claim
	ProjectKey    string
	Replayed      bool
	PreviousEpoch int64
	PreviousTask  string
}

// ClaimEpoch raises the coordinator epoch of a plan to the next number for a new parent session (contract 6.2). The actor must hold the live parent
// binding of the plan's project. The claim of a session that repeats its call while it is still the newest is a replay (same epoch); the nonce of a
// claim that was superseded, or that another task made, is stale_coordinator_epoch, so a replaced session cannot take the plan back by repeating itself.
// Claims are serialised by the store's write lock and the (plan, epoch) key.
func (s *Scheduler) ClaimEpoch(ctx context.Context, plan string, in ClaimInput) (ClaimResult, error) {
	var out ClaimResult
	for _, field := range []struct{ name, value string }{{"plan", plan}, {"actor", in.Actor}, {"session nonce", in.SessionNonce}} {
		if !claimText.MatchString(field.value) {
			return out, refuse(contract.RefusalMalformedReceipt, "the %s is letters, digits and . _ : - (at most 128 characters), not %q", field.name, field.value)
		}
	}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		project := in.Project
		var header string
		hasHeader, err := queryOne(txCtx, tx, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &header)
		if err != nil {
			return err
		}
		switch {
		case hasHeader && project != "" && project != header:
			return refuse(contract.RefusalMalformedReceipt, "plan %s belongs to project %s, not %s", plan, header, project)
		case hasHeader:
			project = header
		}
		if !hasHeader {
			// a plan that has no revision has no project of its own, but a claim already made for it was made under a parent binding of one: that project is the plan's, and a claim for another is refused
			// (a second project cannot take over the plan before its first revision)
			if first, claimed, err := dag.LatestClaim(txCtx, tx, plan); err != nil {
				return err
			} else if claimed {
				var claimedProject string
				if _, err := queryOne(txCtx, tx, "SELECT scope_key FROM scope_bindings WHERE binding_id = ?", []any{first.BindingID}, &claimedProject); err != nil {
					return err
				}
				if project != "" && project != claimedProject {
					return refuse(contract.RefusalMalformedReceipt, "plan %s was claimed under project %s and has no revision yet: it is not claimed for %s", plan, claimedProject, project)
				}
				project = claimedProject
			}
			if project == "" {
				return refuse(contract.RefusalMalformedReceipt, "plan %s has no revision yet, so it has no project: name it with --project", plan)
			}
		}
		out.ProjectKey = project
		binding, revision, live, err := dag.LiveParentBinding(txCtx, tx, project, in.Actor)
		if err != nil {
			return err
		}
		if !live {
			return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the live parent of project %s, so it cannot claim the coordinator epoch of plan %s", in.Actor, project, plan)
		}
		latest, claimed, err := dag.LatestClaim(txCtx, tx, plan)
		if err != nil {
			return err
		}
		if prior, seen, err := dag.ClaimByNonce(txCtx, tx, plan, in.SessionNonce); err != nil {
			return err
		} else if seen {
			if claimed && prior.Epoch == latest.Epoch && prior.TaskID == in.Actor && prior.BindingID == binding {
				out.Claim, out.Replayed = prior, true
				out.PreviousEpoch = prior.Epoch - 1
				return nil
			}
			return dag.StaleEpoch("session %s already claimed epoch %d of plan %s and that claim is not the newest (epoch %d): a session that was replaced does not take the plan back; a new session claims with a new nonce", in.SessionNonce, prior.Epoch, plan, latest.Epoch)
		}
		out.PreviousEpoch, out.PreviousTask = latest.Epoch, latest.TaskID
		out.Claim = dag.Claim{PlanID: plan, Epoch: latest.Epoch + 1, BindingID: binding, BindingRevision: revision, TaskID: in.Actor, SessionNonce: in.SessionNonce, ClaimedAt: s.now()}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_coordinator_claims (plan_id, epoch, binding_id, binding_revision, task_id, session_nonce, claimed_at) VALUES (?,?,?,?,?,?,?)",
			plan, out.Epoch, binding, revision, in.Actor, in.SessionNonce, out.ClaimedAt)
		return err
	})
	return out, err
}

// Object is the claim as the relay prints it (dag-coordinator-claim).
func (r ClaimResult) Object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaClaim}, {Key: "plan_id", Value: r.PlanID}, {Key: "project_key", Value: r.ProjectKey},
		{Key: "epoch", Value: r.Epoch}, {Key: "task_id", Value: r.TaskID}, {Key: "session_nonce", Value: r.SessionNonce}, {Key: "binding_id", Value: r.BindingID},
		{Key: "binding_revision", Value: r.BindingRevision}, {Key: "claimed_at", Value: r.ClaimedAt}, {Key: "replayed", Value: r.Replayed},
		{Key: "previous_epoch", Value: r.PreviousEpoch}, {Key: "previous_task_id", Value: optionalText(r.PreviousTask)}}
}

// successor is the part of a relationship that adoption reads.
type successor struct {
	ID, Status, Issue, Parent, Child string
	Generation                       int64
	Next                             sql.NullString
}

func loadSuccessor(ctx context.Context, q store.Querier, id string) (successor, bool, error) {
	var r successor
	found, err := queryOne(ctx, q, "SELECT relationship_id, status, issue_key, parent_task_id, child_task_id, execution_generation, superseded_by FROM relationships WHERE relationship_id = ?",
		[]any{id}, &r.ID, &r.Status, &r.Issue, &r.Parent, &r.Child, &r.Generation, &r.Next)
	return r, found, err
}

// finalSuccessor follows the replacements of a relationship (relationships.superseded_by) to the one nobody replaced: found is false when the chain ends
// in a relationship the store does not hold or is longer than 16.
func finalSuccessor(ctx context.Context, q store.Querier, id string) (successor, bool, error) {
	cur, found, err := loadSuccessor(ctx, q, id)
	for hops := 0; err == nil && found && cur.Next.Valid; hops++ {
		if hops == 16 {
			return cur, false, nil
		}
		cur, found, err = loadSuccessor(ctx, q, cur.Next.String)
	}
	return cur, found, err
}

// AdoptResult is the answer of Adopt.
type AdoptResult struct {
	PlanID, NodeID, RelationshipID, FromRelationshipID, ChildTaskID string
	Generation                                                      int64
	Replayed                                                        bool
}

// relationshipWord is the relay's state word for a relationship (the view the node states read; no clock is involved).
func (s *Scheduler) relationshipWord(ctx context.Context, rid string) (string, error) {
	view := registry.NewAssignmentView(&registry.Registry{Store: s.Store})
	view.Clock = func() float64 { return 0 }
	answer, err := view.State(ctx, rid)
	if err != nil {
		return "", err
	}
	return orderedString(answer, "state"), nil
}

// liveRelationshipWord is whether a relationship's state word is one of a child that is still working on, or reporting, the node's result (the words nodestate.go reads as a running, reported, verifying, correcting or paused node).
func liveRelationshipWord(word string) bool {
	switch word {
	case registry.StateRequested, registry.StateReceived, registry.StateVerifying, registry.StateCorrected, registry.StateVerified, registry.StateRereview, registry.StateMerged,
		registry.StateNeedsChanges, registry.StatePaused:
		return true
	}
	return false
}

// liveNodeState is whether a derived node state is one of a child that is working on the node.
func liveNodeState(state string) bool {
	switch state {
	case StateRunning, StateReported, StateVerifying, StateCorrecting, StatePausedNode:
		return true
	}
	return false
}

// Adopt gives a live child to the parent that replaced the node's parent (contract 5.1 ID-5, E-28). The new parent has registered a relationship with the old one
// superseded (the relay's own re-registration and handover commands); the node's executions still stand on the old relationship, which is archived, so the new parent could not
// accept, correct or observe the node. Adopt binds the successor relationship to the node as an execution of kind parent_handover with the manifest the child was released
// under: no child is created, no generation opened, nothing reserved, and the manifest is the same. The slot stays held under the task it was reserved for; releaseSlot returns it
// as that task when the node's current parent accepts or observes the landing (accept.go).
//
// Only a node whose child is still working is adopted, and a node that has an acceptance is refused: the acceptance stands, but the merge turn and the merged mark of the previous
// parent are not handed over here (dag-restart reports such a node as needing an operator). The order of the judgement matters: the successor is resolved and validated first, on every
// call, because the node's own derived state reads the replaced relationship as closed and says nothing about the successor.
func (s *Scheduler) Adopt(ctx context.Context, plan, node, actor string) (AdoptResult, error) {
	out := AdoptResult{PlanID: plan, NodeID: node}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		snap, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		n, ok := nodeOf(snap, node)
		if !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		}
		if _, has, err := loadActiveAcceptance(txCtx, tx, plan, node); err != nil {
			return err
		} else if has {
			return refuse(contract.RefusalDispositionConflict, "node %s has an accepted result: its acceptance stands, and the merge turn and the merged mark of the previous parent are not handed over by an adoption", node)
		}
		rel, found, err := currentRelationshipOf(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to adopt", node)
		}
		out.FromRelationshipID = rel.ID
		from, _, err := loadSuccessor(txCtx, tx, rel.ID)
		if err != nil {
			return err
		}
		if !rel.Superseded {
			// nothing was replaced: the actor holds the node's relationship (adopted already, or never replaced), or it is another parent's and has to be replaced first
			if rel.ParentTaskID != actor {
				return refuse(contract.RefusalRelationshipConflict, "relationship %s of node %s is held by task %s and was not replaced: register the adopting parent's relationship with it superseded first", rel.ID, node, rel.ParentTaskID)
			}
			if rel.Status != "active" && rel.Status != "paused" {
				return refuse(contract.RefusalRelationshipNotActive, "relationship %s is %s: only a live relationship is adopted", rel.ID, rel.Status)
			}
			out.RelationshipID, out.ChildTaskID, out.Generation, out.Replayed = rel.ID, from.Child, rel.Generation, true
			return nil
		}
		to, ok, err := finalSuccessor(txCtx, tx, rel.ID)
		if err != nil {
			return err
		}
		if !ok {
			return refuse(contract.RefusalUnregisteredRelationship, "relationship %s of node %s was replaced and the replacement chain does not end in a registered relationship", rel.ID, node)
		}
		switch {
		case to.Issue != n.IssueKey || to.Child != from.Child:
			return refuse(contract.RefusalRelationshipConflict, "relationship %s is not the replacement of %s for %s with the same child (%s): another child is a replacement of the node, not an adoption", to.ID, rel.ID, n.IssueKey, from.Child)
		case (to.Status != "active" && to.Status != "paused") || to.Next.Valid:
			return refuse(contract.RefusalRelationshipNotActive, "relationship %s is %s: only a live relationship is adopted", to.ID, to.Status)
		case to.Parent != actor:
			return refuse(contract.RefusalScopeRoleMismatch, "relationship %s is held by task %s, not by %s: register it for the adopting parent first", to.ID, to.Parent, actor)
		}
		out.RelationshipID, out.ChildTaskID, out.Generation = to.ID, to.Child, to.Generation
		var bound string
		if had, err := queryOne(txCtx, tx, "SELECT plan_id || '/' || node_id FROM dag_node_executions WHERE relationship_id = ? AND execution_generation = ?", []any{to.ID, to.Generation}, &bound); err != nil {
			return err
		} else if had {
			if bound != plan+"/"+node {
				return refuse(contract.RefusalRelationshipConflict, "relationship %s generation %d is already an execution of %s", to.ID, to.Generation, bound)
			}
			out.Replayed = true
			return nil
		}
		if word, err := s.relationshipWord(txCtx, to.ID); err != nil {
			return err
		} else if !liveRelationshipWord(word) {
			return refuse(contract.RefusalDispositionConflict, "relationship %s of node %s is %s: only a child that is working on its result is adopted", to.ID, node, word)
		}
		var manifest string
		if _, err := queryOne(txCtx, tx, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? ORDER BY execution_generation DESC LIMIT 1",
			[]any{plan, node, rel.ID}, &manifest); err != nil {
			return err
		}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,'parent_handover',NULL)",
			plan, node, to.ID, to.Generation, manifest)
		return err
	})
	return out, err
}

// fenceCapBasis ties a cap basis to the coordinator epoch where it can be tied. A basis belongs to a limit and has no plan, so the fence needs the plan the recorder coordinates. For a limit of the
// project scope the plan (--plan) must be a plan of that project and runs the fence; once any claim was made under a parent binding of the project (the claim of a plan that has no revision yet
// counts, and so does a claim whose binding was replaced since), a basis without a plan, and a basis for a plan that has no claim of its own, is stale_coordinator_epoch: an unclaimed second plan is no way
// round the fence. A limit of a wider scope is declared by a supervisor, who coordinates no plan: it takes no plan and is not fenced.
func (s *Scheduler) fenceCapBasis(ctx context.Context, q store.Querier, scopeKind, scopeKey string, in CapBasis) error {
	if scopeKind != "project" {
		if in.Plan != "" {
			return refuse(contract.RefusalMalformedReceipt, "limit %s is a limit of the %s scope, declared by a supervisor who coordinates no plan: a plan is named only for a project limit", in.LimitID, scopeKind)
		}
		return nil
	}
	if in.Plan != "" {
		var project string
		if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{in.Plan}, &project); err != nil {
			return err
		} else if !has || project != scopeKey {
			return refuse(contract.RefusalMalformedReceipt, "plan %s is not a plan of project %s", in.Plan, scopeKey)
		}
	}
	var claims int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_coordinator_claims c JOIN scope_bindings b ON b.binding_id = c.binding_id WHERE b.scope_kind = 'project' AND b.scope_key = ?", []any{scopeKey}, &claims); err != nil {
		return err
	}
	if in.Plan == "" {
		if claims > 0 {
			return dag.StaleEpoch("project %s has plans under a coordinator epoch: name the plan you coordinate (--plan) and the epoch you hold (--expect-epoch)", scopeKey)
		}
		return nil
	}
	if claims > 0 {
		if _, claimed, err := dag.LatestClaim(ctx, q, in.Plan); err != nil {
			return err
		} else if !claimed {
			return dag.StaleEpoch("plan %s has no claim although project %s has plans under a coordinator epoch", in.Plan, scopeKey)
		}
	}
	return s.fence(ctx, q, in.Plan, in.DecidedBy)
}

// Object is the adoption as the relay prints it (dag-adopt).
func (r AdoptResult) Object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaAdopt}, {Key: "plan_id", Value: r.PlanID}, {Key: "node_id", Value: r.NodeID},
		{Key: "relationship_id", Value: r.RelationshipID}, {Key: "from_relationship_id", Value: optionalText(r.FromRelationshipID)}, {Key: "child_task_id", Value: r.ChildTaskID},
		{Key: "execution_generation", Value: r.Generation}, {Key: "replayed", Value: r.Replayed}}
}

// What a restarted or replacement parent does with a node it finds in the store (dag-restart).
const (
	ResumeNone          = "none"           // nothing is outstanding
	ResumeAdopt         = "adopt"          // a live child this actor holds: carry on, release nothing
	ResumeAdoptNeeded   = "adopt_needed"   // a live child whose relationship was replaced by one this actor holds: dag-adopt
	ResumeReconcile     = "reconcile"      // an effect whose outcome is not known: resolve it by replay or observation, never by doing it again
	ResumeNeedsOperator = "needs_operator" // nothing in this build moves it on
)

// RestartNode is one owned node in the rebuilt picture.
type RestartNode struct {
	NodeID, IssueKey, State, Disposition, Reason, Detail string
	Resume, ResumeDetail                                 string
	RelationshipID, ParentTaskID, ChildTaskID            string
	Generation                                           int64
}

// RestartReport is what the store says to a parent that starts again: the plan, who holds its epoch, and, for every node somebody owns, what to do with it. It is a reading: it
// writes nothing, reads no clock, and the same store state gives the same report.
type RestartReport struct {
	PlanID, ProjectKey string
	PlanRevision       int64
	Claim              dag.Claim
	Claimed, Holds     bool
	Counts             map[string]int
	Nodes              []RestartNode
}

// Restart rebuilds the picture of a plan's execution from the store alone, for the actor that starts again (a new session, or a replacement parent). Nothing is remembered between calls
// and time decides nothing: a lease or a heartbeat that ran out does not make a node ready again, an effect whose outcome is unknown stays unknown until its receipt
// is reconciled, and a node is released only by the ready reading.
func (s *Scheduler) Restart(ctx context.Context, plan, actor string) (RestartReport, error) {
	out := RestartReport{PlanID: plan, Counts: map[string]int{ResumeAdopt: 0, ResumeAdoptNeeded: 0, ResumeReconcile: 0, ResumeNeedsOperator: 0}}
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		reading, err := s.Ready(txCtx, q, plan, ReadyOptions{SkipArtifactBytes: true})
		if err != nil {
			return err
		}
		out.PlanRevision = reading.PlanRevision
		if _, err := queryOne(txCtx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &out.ProjectKey); err != nil {
			return err
		}
		if out.Claim, out.Claimed, err = dag.LatestClaim(txCtx, q, plan); err != nil {
			return err
		}
		out.Holds = out.Claimed && out.Claim.TaskID == actor && dag.CheckCoordinatorEpoch(txCtx, q, plan, "", actor, out.Claim.Epoch) == nil
		for _, n := range reading.Nodes {
			if n.State == StateWaiting || n.State == StateReady || n.State == StatePlanned {
				continue
			}
			node := RestartNode{NodeID: n.NodeID, IssueKey: n.IssueKey, State: n.State, Disposition: n.Disposition, Reason: n.Reason, Detail: n.Detail}
			if err := s.resumeOf(txCtx, q, plan, actor, n, &node); err != nil {
				return err
			}
			if _, counted := out.Counts[node.Resume]; counted {
				out.Counts[node.Resume]++
			}
			out.Nodes = append(out.Nodes, node)
		}
		return nil
	})
	return out, err
}

// frozenParent is the parent task the request frozen with a node's latest release names ("" when there is none or it cannot be read).
func frozenParent(ctx context.Context, q store.Querier, plan, node string) (string, error) {
	row, found, err := latestRelease(ctx, q, plan, node)
	if err != nil || !found {
		return "", err
	}
	var raw string
	if has, err := queryOne(ctx, q, "SELECT request_json FROM dag_release_requests WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?", []any{plan, node, row.Digest}, &raw); err != nil || !has {
		return "", err
	}
	var request struct {
		Parent struct {
			TaskID string `json:"taskId"`
		} `json:"parent"`
	}
	if json.Unmarshal([]byte(raw), &request) != nil {
		return "", nil
	}
	return request.Parent.TaskID, nil
}

func (s *Scheduler) resumeOf(ctx context.Context, q store.Querier, plan, actor string, n NodeReading, out *RestartNode) error {
	out.Resume = ResumeNone
	switch n.State {
	case StateCreationUnknown, StateReleasing:
		parent, err := frozenParent(ctx, q, plan, n.NodeID)
		if err != nil {
			return err
		}
		switch {
		case n.Reason == BlockedReleaseAbandoned:
			out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the managed start was released before it created a child: a plan revision that changes the slice, or the operator's slot-release, is the way on"
		case parent != "" && parent != actor:
			out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the request frozen with the release names "+parent+" as the parent, so it cannot continue under "+actor
		default:
			out.Resume, out.ResumeDetail = ResumeReconcile, "repeat dag-release: the same request id reads the host's record of the creation before it creates anything"
		}
		return nil
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil || !found {
		return err
	}
	out.RelationshipID, out.ParentTaskID, out.Generation = rel.ID, rel.ParentTaskID, rel.Generation
	if from, ok, err := loadSuccessor(ctx, q, rel.ID); err != nil {
		return err
	} else if ok {
		out.ChildTaskID = from.Child
	}
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return err
	}
	if rel.Superseded {
		switch {
		case hasAcc && (n.State == StateIntegrated || acc.HeadSHA == ""):
			// a landed result, or a result with no head to land: nothing of the previous parent's is outstanding
		case hasAcc:
			out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the result is accepted and not landed, and its merge turn and merged mark belong to the previous parent: nothing in this build hands them over"
		default:
			next, ok, err := finalSuccessor(ctx, q, rel.ID)
			if err != nil {
				return err
			}
			if ok && next.Parent == actor && (next.Status == "active" || next.Status == "paused") && next.Issue == n.IssueKey && next.Child == out.ChildTaskID {
				out.Resume, out.ResumeDetail = ResumeAdoptNeeded, "run dag-adopt: relationship "+next.ID+" replaced "+rel.ID+" for the same child"
			} else {
				out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the relationship was replaced and no live replacement of the same child is held by "+actor+": register it, with the old one superseded, and hand over first"
			}
		}
		return nil
	}
	switch {
	case n.Reason == BlockedEffectUnknown:
		out.Resume, out.ResumeDetail = ResumeReconcile, "a merge turn for the accepted head ended with an unknown effect: observe where the head is (dag-integration-observe) and do not request another turn"
	case liveNodeState(n.State) && rel.ParentTaskID == actor:
		out.Resume, out.ResumeDetail = ResumeAdopt, "the child is working and this actor holds its relationship: release nothing, carry on"
	case liveNodeState(n.State):
		out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the child's relationship is held by "+rel.ParentTaskID+": a replacement parent registers its own with the old one superseded first"
	case n.State == StateAmbiguousNode:
		out.Resume, out.ResumeDetail = ResumeNeedsOperator, n.Detail
	case hasAcc && n.State == StateAccepted && acc.HeadSHA != "" && rel.ParentTaskID != actor:
		out.Resume, out.ResumeDetail = ResumeNeedsOperator, "the result is accepted and not landed, and the relationship is held by "+rel.ParentTaskID
	}
	return nil
}

// Object is the report as the relay prints it (dag-restart).
func (r RestartReport) Object() contract.OrderedObject {
	nodes := make([]any, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = contract.OrderedObject{{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "state", Value: n.State}, {Key: "disposition", Value: n.Disposition},
			{Key: "reason", Value: optionalText(n.Reason)}, {Key: "resume", Value: n.Resume}, {Key: "resume_detail", Value: optionalText(n.ResumeDetail)},
			{Key: "relationship_id", Value: optionalText(n.RelationshipID)}, {Key: "parent_task_id", Value: optionalText(n.ParentTaskID)}, {Key: "child_task_id", Value: optionalText(n.ChildTaskID)},
			{Key: "execution_generation", Value: n.Generation}, {Key: "detail", Value: optionalText(n.Detail)}}
	}
	coordinator := contract.OrderedObject{{Key: "claimed", Value: r.Claimed}, {Key: "holds", Value: r.Holds}}
	if r.Claimed {
		coordinator = append(coordinator, contract.Field{Key: "epoch", Value: r.Claim.Epoch}, contract.Field{Key: "task_id", Value: r.Claim.TaskID},
			contract.Field{Key: "session_nonce", Value: r.Claim.SessionNonce}, contract.Field{Key: "claimed_at", Value: r.Claim.ClaimedAt})
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaRestart}, {Key: "plan_id", Value: r.PlanID}, {Key: "project_key", Value: r.ProjectKey},
		{Key: "plan_revision", Value: r.PlanRevision}, {Key: "coordinator", Value: coordinator},
		{Key: "counts", Value: contract.OrderedObject{{Key: ResumeAdopt, Value: r.Counts[ResumeAdopt]}, {Key: ResumeAdoptNeeded, Value: r.Counts[ResumeAdoptNeeded]},
			{Key: ResumeReconcile, Value: r.Counts[ResumeReconcile]}, {Key: ResumeNeedsOperator, Value: r.Counts[ResumeNeedsOperator]}}},
		{Key: "nodes", Value: nodes}}
}
