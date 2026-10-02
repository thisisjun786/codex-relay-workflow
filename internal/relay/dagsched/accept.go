package dagsched

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// PRRef names the pull request of an implementation node's work: the forge repository (owner/name) and the number. The head is never given: the relay reads it from the forge.
type PRRef struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
}

// VerifierRule names the rules the parent verified the result under (recorded with the acceptance, left out of its identity).
type VerifierRule struct {
	SkillsDigest string `json:"skills_digest"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
}

// AcceptInput is what a parent supplies to accept a node's result. Nothing in it names a head: for an implementation node the head is the one the relay reads from the forge, so a child's
// statement can never become the accepted head (P-AV-2).
type AcceptInput struct {
	Event       string       // the event the parent verified; the current head of the generation when empty
	PullRequest *PRRef       // required for an implementation node, forbidden for a non_pr node
	Supersedes  string       // the acceptance this one replaces, when the node already has an active acceptance of another output
	RuleVersion VerifierRule // every field required
}

// AcceptResult is the answer of Accept.
type AcceptResult struct {
	PlanID, NodeID, AcceptanceID, RelationshipID, SupersededID, HeadSHA, EvidenceDigest string
	Generation                                                                          int64
	Replayed, Revalidated, SlotReleased                                                 bool
}

// slotState is the state and tenure of the newest tenure of a node's slot ("" when it never had one). A slot returned and reserved again (a replay of a release whose slot was returned
// meanwhile) has several tenures, and a release that does not name one is refused.
func (s *Scheduler) slotState(ctx context.Context, q store.Querier, plan, node string) (string, int64, error) {
	var state string
	var tenure int64
	_, err := queryOne(ctx, q, "SELECT state, tenure FROM execution_slots WHERE subject_kind = ? AND subject_key = ? ORDER BY tenure DESC LIMIT 1", []any{SlotSubjectKind, SlotSubjectKey(plan, node)}, &state, &tenure)
	return state, tenure, err
}

// releaseSlot returns the node's slot when its newest tenure is still held. An operator's earlier slot-release with another reason would make Release record a conflict and refuse, which would
// roll the surrounding transaction back, so only a held slot is released, and the tenure read here is the one named.
func (s *Scheduler) releaseSlot(ctx context.Context, q store.Querier, plan, node, actor, reason string) (bool, error) {
	state, tenure, err := s.slotState(ctx, q, plan, node)
	if err != nil || state != "held" {
		return false, err
	}
	releasedBy, err := s.slotReturner(ctx, q, plan, node, actor, tenure)
	if err != nil {
		return false, err
	}
	_, err = (&capacity.Capacity{Store: s.Store, Now: s.now}).Release(ctx, capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey(plan, node), ReleasedBy: releasedBy, Reason: reason,
		Tenure: sql.NullInt64{Int64: tenure, Valid: true}})
	return err == nil, err
}

// slotReturner is the task a slot goes back as. It is the caller, who holds it, except after a parent was replaced and the node adopted (epoch.go, Adopt): the slot was reserved for the previous parent, and
// both callers of releaseSlot have already shown that the caller is the parent of the node's current relationship. It then goes back as the recorded holder, with no reservation, so a ceiling or a
// measurement cannot refuse it. Only when the slot is held for the plan's own project and its holder is the parent of a relationship in this node's own execution chain: any other holder
// is left to capacity's own rule and the caller is refused as before.
func (s *Scheduler) slotReturner(ctx context.Context, q store.Querier, plan, node, actor string, tenure int64) (string, error) {
	var holder, project string
	held, err := queryOne(ctx, q, "SELECT parent_task_id, project_key FROM execution_slots WHERE subject_kind = ? AND subject_key = ? AND tenure = ? AND state = 'held'", []any{SlotSubjectKind, SlotSubjectKey(plan, node), tenure}, &holder, &project)
	if err != nil || !held || holder == actor {
		return actor, err
	}
	var planProject string
	if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &planProject); err != nil || !has || planProject != project {
		return actor, err
	}
	var one int
	chain, err := queryOne(ctx, q, "SELECT 1 FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.plan_id = ? AND e.node_id = ? AND r.parent_task_id = ? LIMIT 1", []any{plan, node, holder}, &one)
	if err != nil || !chain {
		return actor, err
	}
	return holder, nil
}

// verifiedHead is the live reading of P-AV-1 (contract 2.1): the final, unsuppressed ready_for_review event that is the one head of its generation, acknowledged and verified by the host,
// ruled verified in the current criteria, in managed verification. Every link is read now; the reason a link fails is the relay's own.
type verifiedHead struct {
	EventID, RevisionHash, SetDigest, AckTier, VerdictTurn, ManifestRef string
	Generation                                                          int64
}

func (s *Scheduler) verifiedHead(ctx context.Context, q store.Querier, rel relRow, want string) (verifiedHead, error) {
	var h verifiedHead
	head, err := delivery.HeadRevisionFrom(ctx, q, rel.ID, rel.Generation)
	if err != nil {
		return h, err
	}
	event, _ := objString(head, "eventId")
	if event == "" {
		if evidenceKind, _ := objString(head, "evidence"); evidenceKind != "" && evidenceKind != delivery.NoRevision {
			return h, refuse(contract.RefusalRevisionAmbiguous, "the head of generation %d of %s is ambiguous (%s)", rel.Generation, rel.ID, evidenceKind)
		}
		return h, refuse(contract.RefusalNotAcknowledged, "generation %d of %s has no reviewable revision", rel.Generation, rel.ID)
	}
	if want != "" && want != event {
		var older int
		known, err := queryOne(ctx, q, "SELECT 1 FROM events WHERE event_id = ? AND relationship_id = ? AND execution_generation = ?", []any{want, rel.ID, rel.Generation}, &older)
		if err != nil {
			return h, err
		}
		if known {
			return h, refuse(contract.RefusalSupersededRevision, "event %s is not the head of its generation: %s is", want, event)
		}
		return h, refuse(contract.RefusalStaleGeneration, "event %s is not an event of generation %d of %s", want, rel.Generation, rel.ID)
	}
	var stage, outcome string
	var suppressed sql.NullString
	if _, err := queryOne(ctx, q, "SELECT stage, outcome, suppressed_reason FROM events WHERE event_id = ?", []any{event}, &stage, &outcome, &suppressed); err != nil {
		return h, err
	}
	if stage != "final" || outcome != "ready_for_review" || suppressed.Valid {
		return h, refuse(contract.RefusalNotAcknowledged, "event %s is not a final, unsuppressed report", event)
	}
	h.EventID, h.Generation = event, rel.Generation
	if _, err := queryOne(ctx, q, "SELECT revision_hash, COALESCE(manifest_ref, '') FROM events WHERE event_id = ?", []any{event}, &h.RevisionHash, &h.ManifestRef); err != nil {
		return h, err
	}
	var accepted int
	var verified string
	if found, err := queryOne(ctx, q, "SELECT accepted, verified FROM acks WHERE event_id = ?", []any{event}, &accepted, &verified); err != nil {
		return h, err
	} else if !found || accepted != 1 || verified != "verified" {
		return h, refuse(contract.RefusalNotAcknowledged, "event %s has no accepted, host-verified acknowledgement", event)
	}
	if found, err := queryOne(ctx, q, "SELECT tier FROM ack_evidence WHERE event_id = ? AND tier <> 'unverified'", []any{event}, &h.AckTier); err != nil {
		return h, err
	} else if !found {
		return h, refuse(contract.RefusalNotAcknowledged, "the acknowledgement of event %s rests on no verified evidence", event)
	}
	var verdict string
	if found, err := queryOne(ctx, q, "SELECT verdict, verdict_turn_id FROM verdicts WHERE event_id = ?", []any{event}, &verdict, &h.VerdictTurn); err != nil {
		return h, err
	} else if !found || verdict != "verified" {
		return h, refuse(contract.RefusalDispositionConflict, "event %s has no verified ruling (it is %q)", event, verdict)
	}
	var currency, headEvent string
	if found, err := queryOne(ctx, q, "SELECT set_digest, currency, head_event_id FROM verdict_context WHERE event_id = ?", []any{event}, &h.SetDigest, &currency, &headEvent); err != nil {
		return h, err
	} else if !found || currency != "current" || headEvent != event {
		return h, refuse(contract.RefusalSupersededRevision, "the ruling on event %s is not current", event)
	}
	var mode string
	if found, err := queryOne(ctx, q, "SELECT mode FROM verification_mode WHERE relationship_id = ?", []any{rel.ID}, &mode); err != nil {
		return h, err
	} else if !found || mode != "managed" {
		return h, refuse(contract.RefusalDispositionConflict, "relationship %s is not under managed verification", rel.ID)
	}
	var rows, distinct int
	var canonical sql.NullString
	if _, err := queryOne(ctx, q, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{rel.ID}, &rows, &distinct, &canonical); err != nil {
		return h, err
	}
	if rows == 0 || distinct != 1 || canonical.String != h.SetDigest {
		return h, refuse(contract.RefusalCriteriaSetChanged, "the criteria registered for %s are not the ones event %s was ruled against", rel.ID, event)
	}
	return h, nil
}

// Accept records that the parent accepted a node's result (contract 4.3). It is written only when P-AV-1 holds now at the current head and criteria digest, and for an implementation node
// only on the head the relay itself read from the forge. An acceptance is idempotent per output: the same output accepted again is a replay, and the same output re-ruled under re-registered
// criteria is a revalidation of the same acceptance (contract E-11), never a second acceptance, a new generation or a new child.
func (s *Scheduler) Accept(ctx context.Context, plan, node, actor string, in AcceptInput) (AcceptResult, error) {
	out := AcceptResult{PlanID: plan, NodeID: node}
	rule := in.RuleVersion
	if rule.SkillsDigest == "" || rule.Model == "" || rule.Effort == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an acceptance records the rules it was verified under: skills_digest, model and effort are required")
	}
	ruleJSON := dag.Canonical(map[string]any{"skills_digest": rule.SkillsDigest, "model": rule.Model, "effort": rule.Effort})
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	// the plan's hold is read before the forge is (a node the plan paused or ended is not accepted); the transaction below reads it again
	if err := lifecycleRefusal(snap, n, "accepting its result", false); err != nil {
		return out, err
	}
	implementation := n.Kind == dag.NodeImplementation
	var pr PullRequest
	if implementation {
		if in.PullRequest == nil || in.PullRequest.Repository == "" || in.PullRequest.Number < 1 {
			return out, refuse(contract.RefusalMalformedReceipt, "an implementation node is accepted on its pull request: name its repository (owner/name) and number")
		}
		if s.PRs == nil {
			return out, errors.New("this scheduler has no pull request reader, so it cannot read the head to accept")
		}
		if pr, err = s.PRs(ctx, in.PullRequest.Repository, in.PullRequest.Number); err != nil {
			return out, err
		}
		if err := ClassifyPullRequest(pr); err != nil {
			return out, err
		}
	} else if in.PullRequest != nil {
		return out, refuse(contract.RefusalMalformedReceipt, "node %s is a %s node: it has no pull request to name", node, n.Kind)
	}
	if s.testBeforeAcceptTx != nil {
		s.testBeforeAcceptTx()
	}
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		current, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		if cn, ok := nodeOf(current, node); !ok || cn.SliceDigest != n.SliceDigest || cn.CriteriaSetDigest != n.CriteriaSetDigest {
			return refuse(contract.RefusalDispositionConflict, "the plan changed while the result of %s was being verified", node)
		} else if err := lifecycleRefusal(current, cn, "accepting its result", false); err != nil {
			// a pause does not move the slice or criteria digests, so the plan's hold is asked again here
			return err
		}
		rel, found, err := currentRelationshipOf(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to accept", node)
		}
		if rel.Status != "active" || rel.Superseded {
			return refuse(contract.RefusalRelationshipNotActive, "the relationship %s of %s is %s: a result is accepted while its child's relationship is active", rel.ID, node, map[bool]string{true: "superseded", false: rel.Status}[rel.Superseded])
		}
		if rel.ParentTaskID != actor {
			return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s, which is held by %s", actor, rel.ID, rel.ParentTaskID)
		}
		var manifest string
		if bound, err := queryOne(txCtx, tx, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, node, rel.ID, rel.Generation}, &manifest); err != nil {
			return err
		} else if !bound {
			return refuse(contract.RefusalStaleGeneration, "generation %d of %s is not recorded as an execution of %s (a correction is recorded with dag-correct before its result is accepted)", rel.Generation, rel.ID, node)
		}
		head, err := s.verifiedHead(txCtx, tx, rel, in.Event)
		if err != nil {
			return err
		}
		if head.SetDigest != n.CriteriaSetDigest {
			return refuse(contract.RefusalCriteriaSetChanged, "the output was ruled against criteria %s and the plan fixed %s for %s", head.SetDigest, n.CriteriaSetDigest, node)
		}
		out.RelationshipID, out.Generation = rel.ID, rel.Generation

		// the same output accepted before: a replay, or the same acceptance re-ruled under re-registered criteria
		var existing, state, ownDigest string
		if exists, err := queryOne(txCtx, tx, "SELECT acceptance_id, state, criteria_set_digest FROM dag_acceptances WHERE relationship_id = ? AND execution_generation = ? AND revision_hash = ?", []any{rel.ID, rel.Generation, head.RevisionHash}, &existing, &state, &ownDigest); err != nil {
			return err
		} else if exists {
			out.AcceptanceID = existing
			if state != "active" {
				return refuse(contract.RefusalDispositionConflict, "this output was accepted as %s and that acceptance is %s; accept a new output instead", existing, state)
			}
			a, err := loadAcceptanceByID(txCtx, tx, existing)
			if err != nil {
				return err
			}
			if implementation {
				// the output is accepted already, on the pull request and at the head recorded then: a call that reads another pull request, or the same one at another head, is not a replay of it
				if err := s.sameForgeReading(txCtx, tx, a, *in.PullRequest, pr); err != nil {
					return err
				}
			}
			effective, err := effectiveCriteria(txCtx, tx, a)
			if err != nil {
				return err
			}
			out.HeadSHA = a.HeadSHA
			if effective == head.SetDigest {
				out.Replayed = true
				return nil
			}
			var last sql.NullInt64
			if err := tx.QueryRowContext(txCtx, "SELECT MAX(reval_seq) FROM dag_acceptance_revalidations WHERE acceptance_id = ?", existing).Scan(&last); err != nil {
				return err
			}
			seq := last.Int64 + 1
			if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?,?,?,?,?,?,?,?)",
				"drv-"+shaOf([]byte(existing + "|" + itoa64(seq)))[:32], existing, head.SetDigest, head.EventID, head.VerdictTurn, seq, actor, s.now()); err != nil {
				return err
			}
			out.Revalidated = true
			return nil
		}

		// a new output: it supersedes the node's active acceptance only explicitly
		if implementation && (pr.State != "open" || pr.IsDraft) {
			return refuse(contract.RefusalDispositionConflict, "pull request %s#%d is %s%s: only an open, non-draft pull request is accepted", pr.Repository, pr.Number, pr.State, map[bool]string{true: " and a draft", false: ""}[pr.IsDraft])
		}
		var activeID string
		if active, err := queryOne(txCtx, tx, "SELECT acceptance_id FROM dag_acceptances WHERE plan_id = ? AND node_id = ? AND state = 'active'", []any{plan, node}, &activeID); err != nil {
			return err
		} else if active {
			if in.Supersedes != activeID {
				return refuse(contract.RefusalDispositionConflict, "node %s already has the active acceptance %s of another output; supersede it explicitly", node, activeID)
			}
			if _, err := tx.ExecContext(txCtx, "UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", activeID); err != nil {
				return err
			}
			out.SupersededID = activeID
		} else if in.Supersedes != "" {
			return refuse(contract.RefusalDispositionConflict, "node %s has no active acceptance to supersede", node)
		}
		a := Acceptance{PlanID: plan, NodeID: node, ManifestDigest: manifest, RelationshipID: rel.ID, ExecutionGeneration: rel.Generation, EventID: head.EventID, RevisionHash: head.RevisionHash,
			CriteriaSetDigest: head.SetDigest, Verdict: "verified", OutputManifestRef: head.ManifestRef, AckTier: head.AckTier, VerdictTurnID: head.VerdictTurn, RuleVersionJSON: ruleJSON, AcceptedByTask: actor,
			CoordinatorEpoch: s.ExpectedEpoch, AcceptedAt: s.now(), SupersedesAcceptanceID: activeID, State: "active"}
		if implementation {
			repository, err := s.acceptTarget(txCtx, tx, current, node, in.PullRequest.Repository)
			if err != nil {
				return err
			}
			a.HeadSHA, a.Repository, a.PRNumber, a.EvidenceDigest = pr.HeadSHA, repository, pr.Number, EvidenceDigest(EvidenceBodyOf(pr))
		}
		a.AcceptanceID = AcceptanceDigest(a)
		var supersedes, ref, head2, repo2, evidence2, pr2 any
		for _, v := range []struct {
			dst *any
			val string
		}{{&supersedes, a.SupersedesAcceptanceID}, {&ref, a.OutputManifestRef}, {&head2, a.HeadSHA}, {&repo2, a.Repository}, {&evidence2, a.EvidenceDigest}} {
			if v.val != "" {
				*v.dst = v.val
			}
		}
		if a.PRNumber > 0 {
			pr2 = a.PRNumber
		}
		if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_acceptances ("+acceptanceColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			a.AcceptanceID, a.PlanID, a.NodeID, a.ManifestDigest, a.RelationshipID, a.ExecutionGeneration, a.EventID, a.RevisionHash, a.CriteriaSetDigest, a.Verdict, head2, repo2, pr2, ref, evidence2,
			a.AckTier, a.VerdictTurnID, a.RuleVersionJSON, a.AcceptedByTask, a.CoordinatorEpoch, a.AcceptedAt, supersedes, a.State); err != nil {
			return err
		}
		// the acceptance row first: dag_acceptance_forge references it with an immediate foreign key
		if implementation {
			if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?,?,?)", a.AcceptanceID, in.PullRequest.Repository, pr.Number); err != nil {
				return err
			}
		} else if out.SlotReleased, err = s.releaseSlot(txCtx, tx, plan, node, actor, "dag_accepted"); err != nil {
			return err
		}
		out.AcceptanceID, out.HeadSHA = a.AcceptanceID, a.HeadSHA
		out.EvidenceDigest = a.EvidenceDigest
		return nil
	})
	return out, err
}

// acceptTarget is the repository an implementation node's accepted head is judged against: the single repository its outgoing integrated and code-pinned edges name. A node with none
// (a terminal node) is judged against the forge repository, observed with an explicit target. Several repositories are not a single acceptance's.
func (s *Scheduler) acceptTarget(ctx context.Context, q store.Querier, snap dag.Snapshot, node, forge string) (string, error) {
	targets := map[string]bool{}
	for _, e := range snap.Edges {
		if e.FromNodeID == node && e.TargetRepository != "" && (e.Kind == dag.EdgeIntegrated || (e.Kind == dag.EdgeArtifactVerified && e.PinsCodeHead)) {
			targets[e.TargetRepository] = true
		}
	}
	switch len(targets) {
	case 0:
		return forge, nil
	case 1:
		for t := range targets {
			// a forge target is the pull request's own repository; a local checkout is allowed as the target of ancestry
			if _, _, err := evidence.SplitRepository(t); err == nil && t != forge {
				return "", refuse(contract.RefusalDispositionConflict, "the outgoing edges of %s land on %s and the pull request is in %s", node, t, forge)
			}
			return t, nil
		}
	}
	return "", refuse(contract.RefusalMalformedReceipt, "the outgoing edges of %s name more than one repository; one acceptance is judged against one", node)
}

// sameForgeReading is what makes a repeated acceptance of an implementation node's output a replay: the pull request named is the one recorded with the acceptance, and the head the forge
// shows now is the accepted one. A call that reads another pull request, or the same one at another head, is not a replay of that acceptance and is refused.
func (s *Scheduler) sameForgeReading(ctx context.Context, q store.Querier, a Acceptance, named PRRef, pr PullRequest) error {
	var forge string
	var number int64
	has, err := queryOne(ctx, q, "SELECT forge_repository, pr_number FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{a.AcceptanceID}, &forge, &number)
	if err != nil {
		return err
	}
	if !has {
		return refuse(contract.RefusalDispositionConflict, "acceptance %s has no forge identity recorded, so the pull request named cannot be compared with it", a.AcceptanceID)
	}
	if forge != named.Repository || number != named.Number {
		return refuse(contract.RefusalDispositionConflict, "this output was accepted on pull request %s#%d and the call names %s#%d", forge, number, named.Repository, named.Number)
	}
	if pr.HeadSHA != a.HeadSHA {
		return refuseCandidateMoved("pull request %s#%d is at %s and the accepted head of %s is %s", forge, number, pr.HeadSHA, a.NodeID, a.HeadSHA)
	}
	return nil
}
