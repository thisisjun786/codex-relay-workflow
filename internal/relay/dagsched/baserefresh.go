package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A base refresh (CRW-430). The parent verified and accepted a node's result at generation 1 and then could not merge it because the base moved, so the same child opened generation 2 only to merge the base
// into its branch; that pull request landed and the parent's merged mark sits on generation 2's event. The result is current (nothing it consumed changed), so dag-correct has no route for it, and a
// second ruling on an accepted result is refused; and integration, which needs the mark on the acceptance's own revision, never completed, so the node kept its slot.
//
// RecordBaseRefresh closes that gap without touching the acceptance: it records that the active acceptance ALSO stands on that later generation, after proving from git that the generation's head is the
// accepted head plus merges of the base branch (baserefresh_git.go) and nothing else. The acceptance row, its id and its digest stay as they are, so every node that consumed it stays current. What moves is
// the head integration is judged on and the generation whose merged mark counts (standOf). Nothing the caller says becomes a head: the pull request, its head and the base come from the acceptance and the
// forge, the tip of the base from the target reader, the chain from the commits themselves.

// SchemaBaseRefresh names the answer of dag-base-refresh.
const SchemaBaseRefresh = "dag-base-refresh/1"

// RefreshInput is what the parent supplies to record a base refresh. A checkout is where the commits are read (git objects are content addressed, so any clone that holds them answers alike): it is
// needed when the node lands in a forge repository and refused when the target is itself a local checkout. Resolved names the files whose hand resolution the parent accepts; the relay proves where a
// hand resolution can sit (the files git could not merge) and cannot read what was put into them, so the paths named must be exactly the ones the chain resolved by hand.
type RefreshInput struct {
	Checkout string
	Resolved []string
}

// RefreshResult is the answer of RecordBaseRefresh.
type RefreshResult struct {
	PlanID, NodeID, AcceptanceID, RefreshID, RelationshipID, EventID, RevisionHash, HeadSHA string
	Generation, Seq                                                                         int64
	BaseRepository, BaseRef, BaseTipSHA                                                     string
	Steps                                                                                   []RefreshStep
	Resolved                                                                                []string
	Replayed                                                                                bool
}

// acceptanceStand is what an acceptance currently stands on: the relationship, generation, event, revision and head integration is judged against. It is the acceptance's own unless a base refresh was recorded
// for it, and then the newest valid refresh's.
type acceptanceStand struct {
	RelationshipID string
	Generation     int64
	EventID        string
	RevisionHash   string
	Head           string
	RefreshID      string // "" when the acceptance stands on itself
}

// refreshDigest is the identity of a refresh: its refresh_id is the digest of everything it records but the time and the author, so a row written by hand under another content is not read as one.
func refreshDigest(acceptance, relationship string, generation int64, event, revision, head, baseRepository, baseRef, baseTip, proofJSON, resolvedJSON string) string {
	return "dbr-" + shaOf([]byte(dag.Canonical(map[string]any{"schema": SchemaBaseRefresh, "acceptance_id": acceptance, "relationship_id": relationship, "execution_generation": generation,
		"event_id": event, "revision_hash": revision, "head_sha": head, "base_repository": baseRepository, "base_ref": baseRef, "base_tip_sha": baseTip, "proof": proofJSON, "resolved_paths": resolvedJSON})))[:32]
}

// ownStand is what an acceptance stands on before any base refresh was recorded for it: its own relationship, generation, event, revision and head.
func ownStand(a Acceptance) acceptanceStand {
	return acceptanceStand{RelationshipID: a.RelationshipID, Generation: a.ExecutionGeneration, EventID: a.EventID, RevisionHash: a.RevisionHash, Head: a.HeadSHA}
}

// validStands are the base refreshes recorded for an acceptance that digest to their ids, newest first. The table arrived after the first zone, so a store opened read-only that predates it has none; a row
// that does not digest to its id is ignored.
func (s *Scheduler) validStands(ctx context.Context, q store.Querier, a Acceptance) ([]acceptanceStand, error) {
	present, err := tableExists(ctx, q, "dag_base_refreshes")
	if err != nil || !present {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT refresh_id, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json"+
		" FROM dag_base_refreshes WHERE acceptance_id = ? ORDER BY refresh_seq DESC", a.AcceptanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []acceptanceStand
	for rows.Next() {
		var id, rid, event, revision, head, baseRepo, baseRef, baseTip, proof, resolved string
		var generation int64
		if err := rows.Scan(&id, &rid, &generation, &event, &revision, &head, &baseRepo, &baseRef, &baseTip, &proof, &resolved); err != nil {
			return nil, err
		}
		if rid == a.RelationshipID && generation > a.ExecutionGeneration && id == refreshDigest(a.AcceptanceID, rid, generation, event, revision, head, baseRepo, baseRef, baseTip, proof, resolved) {
			out = append(out, acceptanceStand{RelationshipID: rid, Generation: generation, EventID: event, RevisionHash: revision, Head: head, RefreshID: id})
		}
	}
	return out, rows.Err()
}

// standOf reads what an acceptance stands on now: the newest valid base refresh recorded for it, else its own.
func (s *Scheduler) standOf(ctx context.Context, q store.Querier, a Acceptance) (acceptanceStand, error) {
	stands, err := s.validStands(ctx, q, a)
	if err != nil || len(stands) == 0 {
		return ownStand(a), err
	}
	return stands[0], nil
}

// stoodOn is every head the acceptance has stood on: its own and the head of each valid base refresh recorded for it, newest first. A merge turn of the node, or a check of its pull request, made for any
// of them is the node's own (the newest is what it stands on now; the others are what it stood on before a record moved it).
func (s *Scheduler) stoodOn(ctx context.Context, q store.Querier, a Acceptance) ([]string, error) {
	stands, err := s.validStands(ctx, q, a)
	if err != nil {
		return nil, err
	}
	heads := make([]string, 0, len(stands)+1)
	for _, st := range stands {
		heads = append(heads, st.Head)
	}
	return append(heads, a.HeadSHA), nil
}

// decodeRefreshProof reads back the proof and the resolved paths a record stores.
func decodeRefreshProof(proofJSON, resolvedJSON string) ([]RefreshStep, []string, error) {
	var proof struct {
		Steps []struct {
			Previous   string `json:"previous"`
			BaseParent string `json:"base_parent"`
			Head       string `json:"head"`
			Tree       string `json:"tree"`
			Resolved   []struct {
				Path string `json:"path"`
				Blob string `json:"blob"`
			} `json:"resolved"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(proofJSON), &proof); err != nil {
		return nil, nil, err
	}
	var paths []string
	if err := json.Unmarshal([]byte(resolvedJSON), &paths); err != nil {
		return nil, nil, err
	}
	steps := make([]RefreshStep, len(proof.Steps))
	for i, st := range proof.Steps {
		steps[i] = RefreshStep{Previous: st.Previous, BaseParent: st.BaseParent, Head: st.Head, Tree: st.Tree}
		for _, r := range st.Resolved {
			steps[i].Resolved = append(steps[i].Resolved, RefreshResolved{Path: r.Path, Blob: r.Blob})
		}
	}
	return steps, paths, nil
}

// refreshCheckout is where the commits are read: the target itself when it is a local checkout, else the checkout the caller names.
func refreshCheckout(target, given string) (string, error) {
	if filepath.IsAbs(target) {
		if given != "" && filepath.Clean(given) != filepath.Clean(target) {
			return "", refuse(contract.RefusalMalformedReceipt, "the node lands in the local checkout %s, which is where the commits are read: do not name another checkout", target)
		}
		return target, nil
	}
	if given == "" || !filepath.IsAbs(given) {
		return "", refuse(contract.RefusalMalformedReceipt, "the node lands in %s: name a local checkout that holds the commits of the pull request (--checkout, an absolute path), fetched beforehand", target)
	}
	return given, nil
}

// refusedProof is the refusal of a proof that did not pass: disposition_conflict with the closed code in the detail.
func refusedProof(r *refreshRefusal) error {
	return refuse(contract.RefusalDispositionConflict, "not a base refresh (%s): %s; the accepted head is not moved, and a later generation whose content is more than merges of the base goes to its child as a correction", r.Code, r.Detail)
}

// RecordBaseRefresh records that the active acceptance of a node stands on the later generation of its relationship whose head is the accepted head plus merges of the base. The node's result has to be
// current (a stale one goes through its own route, dag-correct), not landed yet, and the relationship's current generation has to be ruled verified under the plan's criteria exactly as an acceptance
// requires. The record is an append: the same chain again is a replay, and a pull request that moved on is another record with the next sequence number, proved again from the accepted head.
func (s *Scheduler) RecordBaseRefresh(ctx context.Context, plan, node, actor string, in RefreshInput) (RefreshResult, error) {
	out := RefreshResult{PlanID: plan, NodeID: node}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	if n.Kind != dag.NodeImplementation {
		return out, refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no head to refresh", node, n.Kind)
	}
	if err := lifecycleRefusal(snap, n, "recording its base refresh", true); err != nil {
		return out, err
	}
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !hasAcc || acc.HeadSHA == "" {
		return out, refuse(contract.RefusalDispositionConflict, "node %s has no accepted head: a base refresh is recorded for a result the parent accepted", node)
	}
	out.AcceptanceID = acc.AcceptanceID
	if err := s.observableRelationship(ctx, q, acc, actor); err != nil {
		return out, err
	}
	var forge string
	var number int64
	if has, err := queryOne(ctx, q, "SELECT forge_repository, pr_number FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{acc.AcceptanceID}, &forge, &number); err != nil {
		return out, err
	} else if !has {
		return out, refuse(contract.RefusalDispositionConflict, "acceptance %s has no forge identity recorded, so the pull request that carried the refresh cannot be read", acc.AcceptanceID)
	}
	if landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc); err != nil {
		return out, err
	} else if landed {
		return out, refuse(contract.RefusalDispositionConflict, "%s landed (its accepted head %s is contained in every target it lands on and the parent marked it merged): there is nothing to refresh", node, short(acc.HeadSHA))
	}
	if st, err := s.staleOf(ctx, q, plan, snap, n); err != nil {
		return out, err
	} else if st != nil {
		return out, refuse(contract.RefusalDispositionConflict, "the accepted result of %s is stale (%s): a base refresh is recorded for a result that is current; a stale one goes through its route (the stale reading names it, dag-correct)", node, st.Reason())
	}
	rel, found, err := loadRelationship(ctx, q, acc.RelationshipID)
	if err != nil {
		return out, err
	}
	if !found || rel.Superseded {
		return out, refuse(contract.RefusalRelationshipNotActive, "the relationship %s of the accepted result is not the one the node stands on any more", acc.RelationshipID)
	}
	if rel.Generation <= acc.ExecutionGeneration {
		return out, refuse(contract.RefusalDispositionConflict, "the relationship %s is still at generation %d, the one %s was accepted on: a base refresh is recorded for a later generation of the same child", rel.ID, rel.Generation, node)
	}
	// the later generation has to be one the parent ruled verified under the plan's criteria: the same chain an acceptance applies to a head, asked before any commit is read
	head, err := s.verifiedHead(ctx, q, rel, "")
	if err != nil {
		return out, err
	}
	if head.SetDigest != n.CriteriaSetDigest {
		return out, refuse(contract.RefusalCriteriaSetChanged, "generation %d of %s was ruled against criteria %s and the plan fixed %s for %s", rel.Generation, rel.ID, head.SetDigest, n.CriteriaSetDigest, node)
	}
	// what the acceptance stands on before this call proves anything: a record that lands while the proof is read makes this one a call to repeat, so a proof that finishes late never puts the acceptance back
	standBefore, err := s.standOf(ctx, q, acc)
	if err != nil {
		return out, err
	}
	if s.PRs == nil || s.Tips == nil {
		return out, errors.New("this scheduler has no pull request reader or target reader, so it cannot read the head to refresh")
	}
	pr, err := s.PRs(ctx, forge, number)
	if err != nil {
		return out, err
	}
	if pr.State != "open" && pr.State != "merged" {
		return out, refuse(contract.RefusalDispositionConflict, "pull request %s#%d is %s: a base refresh is read from a pull request that is open or merged", forge, number, pr.State)
	}
	// the reading is classified as every reader of a pull request classifies it; a merged pull request is readable by the rule in ClassifyPullRequest (forge.go)
	if err := ClassifyPullRequest(pr); err != nil {
		return out, err
	}
	if !refreshCommitPattern.MatchString(pr.HeadSHA) || pr.BaseRef == "" {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "the forge did not answer the head and the base branch of pull request %s#%d", forge, number)
	}
	if known, err := s.nodeTargets(ctx, q, snap, acc); err != nil {
		return out, err
	} else if len(known) > 0 {
		onTarget := false
		for _, t := range known {
			onTarget = onTarget || (t.Repository == acc.Repository && t.BaseRef == pr.BaseRef)
		}
		if !onTarget {
			return out, refuse(contract.RefusalDispositionConflict, "pull request %s#%d is based on %s of %s, which is not one of the targets %s lands on", forge, number, pr.BaseRef, acc.Repository, node)
		}
	}
	tip, err := s.Tips.Tip(ctx, acc.Repository, pr.BaseRef)
	if err != nil {
		return out, err
	}
	checkout, err := refreshCheckout(acc.Repository, in.Checkout)
	if err != nil {
		return out, err
	}
	g, err := openRefreshRepo(ctx, checkout)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read: %v", checkout, err)
	}
	defer g.close()
	for _, c := range []struct{ what, sha string }{{"the accepted head", acc.HeadSHA}, {"the head of the pull request", pr.HeadSHA}, {"the tip of " + pr.BaseRef, tip.SHA}} {
		if !g.hasCommit(ctx, c.sha) {
			return out, refuse(contract.RefusalMergeTargetUnreadable, "%s (%s) is not in %s: fetch it there first (git fetch) and record the refresh again", c.what, c.sha, checkout)
		}
	}
	// the report the parent ruled verified names the head it describes: a head the forge shows now that the report does not name is not what was verified (a report that names none leaves nothing to compare)
	var reported sql.NullString
	if _, err := queryOne(ctx, q, "SELECT head_sha FROM work_reports WHERE event_id = ? AND relationship_id = ? ORDER BY submission_no DESC LIMIT 1", []any{head.EventID, rel.ID}, &reported); err != nil {
		return out, err
	}
	if said := strings.ToLower(strings.TrimSpace(reported.String)); said != "" && !(len(said) >= 7 && strings.HasPrefix(pr.HeadSHA, said)) {
		return out, refuse(contract.RefusalDispositionConflict, "the report of generation %d (event %s) names the head %s and pull request %s#%d is at %s now: the head that was verified is not the head to refresh", rel.Generation, head.EventID, said, forge, number, pr.HeadSHA)
	}
	proof, refusal, err := proveBaseRefresh(ctx, g, acc.HeadSHA, pr.HeadSHA, tip.SHA)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not answer for %s: %v", checkout, err)
	}
	if refusal != nil {
		return out, refusedProof(refusal)
	}
	resolved := proof.resolvedPaths()
	named := append([]string{}, in.Resolved...)
	sort.Strings(named)
	if strings.Join(named, "\x00") != strings.Join(resolved, "\x00") {
		return out, refuse(contract.RefusalDispositionConflict, "the merges between %s and %s resolved these files by hand: [%s]; the relay proves only that the resolution is confined to files git could not merge, and cannot read what was put into them. Name exactly them (--resolved) to accept them, after reading them; you named [%s]",
			short(acc.HeadSHA), short(pr.HeadSHA), strings.Join(resolved, ", "), strings.Join(named, ", "))
	}
	steps := make([]any, len(proof.Steps))
	for i, st := range proof.Steps {
		list := make([]any, len(st.Resolved))
		for j, r := range st.Resolved {
			list[j] = map[string]any{"path": r.Path, "blob": r.Blob}
		}
		steps[i] = map[string]any{"previous": st.Previous, "base_parent": st.BaseParent, "head": st.Head, "tree": st.Tree, "resolved": list}
	}
	proofJSON := dag.Canonical(map[string]any{"schema": SchemaBaseRefresh, "accepted_head": acc.HeadSHA, "steps": steps})
	pathList := make([]any, len(resolved))
	for i, p := range resolved {
		pathList[i] = p
	}
	resolvedJSON := dag.Canonical(pathList)
	out.RelationshipID, out.Generation, out.HeadSHA = rel.ID, rel.Generation, pr.HeadSHA
	out.BaseRepository, out.BaseRef, out.BaseTipSHA, out.Steps, out.Resolved = acc.Repository, pr.BaseRef, tip.SHA, proof.Steps, resolved

	if s.testBeforeRefreshTx != nil {
		s.testBeforeRefreshTx()
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
		cn, ok := nodeOf(current, node)
		if !ok || cn.SliceDigest != n.SliceDigest || cn.CriteriaSetDigest != n.CriteriaSetDigest {
			return refuse(contract.RefusalDispositionConflict, "the plan changed while the refresh of %s was being proved", node)
		}
		if err := lifecycleRefusal(current, cn, "recording its base refresh", true); err != nil {
			return err
		}
		now, found, err := loadActiveAcceptance(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found || now.AcceptanceID != acc.AcceptanceID {
			return refuse(contract.RefusalDispositionConflict, "the accepted result of %s changed while its base refresh was being proved", node)
		}
		if err := s.observableRelationship(txCtx, tx, acc, actor); err != nil {
			return err
		}
		if standNow, err := s.standOf(txCtx, tx, now); err != nil {
			return err
		} else if standNow != standBefore {
			return refuse(contract.RefusalDispositionConflict, "the base refresh recorded for %s changed while this one was being proved: read the head again and call again", node)
		}
		if landed, _, err := s.nodeIntegrated(txCtx, tx, plan, current, now); err != nil {
			return err
		} else if landed {
			return refuse(contract.RefusalDispositionConflict, "%s landed while its base refresh was being proved: there is nothing to refresh", node)
		}
		relNow, found, err := loadRelationship(txCtx, tx, acc.RelationshipID)
		if err != nil {
			return err
		}
		if !found || relNow.Generation != rel.Generation {
			return refuse(contract.RefusalDispositionConflict, "the relationship %s moved from generation %d while the refresh of %s was being proved", acc.RelationshipID, rel.Generation, node)
		}
		headNow, err := s.verifiedHead(txCtx, tx, relNow, "")
		if err != nil {
			return err
		}
		if headNow.EventID != head.EventID || headNow.RevisionHash != head.RevisionHash || headNow.SetDigest != cn.CriteriaSetDigest {
			return refuse(contract.RefusalDispositionConflict, "the head of generation %d of %s changed while the refresh of %s was being proved", rel.Generation, rel.ID, node)
		}
		if st, err := s.staleOf(txCtx, tx, plan, current, cn); err != nil {
			return err
		} else if st != nil {
			return refuse(contract.RefusalDispositionConflict, "the accepted result of %s became stale (%s) while its base refresh was being proved", node, st.Reason())
		}
		out.EventID, out.RevisionHash = head.EventID, head.RevisionHash
		out.RefreshID = refreshDigest(acc.AcceptanceID, rel.ID, rel.Generation, head.EventID, head.RevisionHash, pr.HeadSHA, acc.Repository, pr.BaseRef, tip.SHA, proofJSON, resolvedJSON)
		var existing, baseRepo, baseRef, baseTip, storedProof, storedResolved string
		var seq int64
		if has, err := queryOne(txCtx, tx, "SELECT refresh_id, refresh_seq, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json FROM dag_base_refreshes WHERE acceptance_id = ? AND event_id = ? AND head_sha = ?",
			[]any{acc.AcceptanceID, head.EventID, pr.HeadSHA}, &existing, &seq, &baseRepo, &baseRef, &baseTip, &storedProof, &storedResolved); err != nil {
			return err
		} else if has {
			// the same generation head and pull request head recorded before: a replay, answered with what the first record holds (the tip of the base may have moved on since, and the record stays the first one)
			steps, paths, err := decodeRefreshProof(storedProof, storedResolved)
			if err != nil {
				return err
			}
			out.Replayed, out.RefreshID, out.Seq = true, existing, seq
			out.BaseRepository, out.BaseRef, out.BaseTipSHA, out.Steps, out.Resolved = baseRepo, baseRef, baseTip, steps, paths
			return nil
		}
		var last sql.NullInt64
		if err := tx.QueryRowContext(txCtx, "SELECT MAX(refresh_seq) FROM dag_base_refreshes WHERE acceptance_id = ?", acc.AcceptanceID).Scan(&last); err != nil {
			return err
		}
		out.Seq = last.Int64 + 1
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha,"+
			" proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			out.RefreshID, acc.AcceptanceID, out.Seq, rel.ID, rel.Generation, head.EventID, head.RevisionHash, pr.HeadSHA, acc.Repository, pr.BaseRef, tip.SHA, proofJSON, resolvedJSON, actor, s.ExpectedEpoch, s.now())
		return err
	})
	return out, err
}

// refreshCarries is whether the node's active acceptance stands, through a recorded base refresh, on the current generation of its relationship.
func (s *Scheduler) refreshCarries(ctx context.Context, q store.Querier, plan, node string, rel relRow) (bool, error) {
	acc, has, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil || !has {
		return false, err
	}
	stand, err := s.standOf(ctx, q, acc)
	if err != nil {
		return false, err
	}
	return stand.RefreshID != "" && stand.RelationshipID == rel.ID && stand.Generation == rel.Generation, nil
}
