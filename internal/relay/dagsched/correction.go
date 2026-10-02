package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// A correction goes back to the same child (contract 3.2): the parent's needs_changes ruling opens the next generation of the SAME relationship and the existing verdict writer sends the
// correction message in the same transaction, so a manifest cannot be put into that generation afterwards without the child never having seen it. The protocol therefore puts the manifest
// into the message the relay itself sends: PrepareCorrection builds and stores the manifest and returns an instruction line that the parent submits as the restoration block of the ruling
// (the one finding the relay renders into the correction message, delivery/criteria.go), and RecordCorrection, after the ruling, binds the generation to the manifest the restoration
// block names.

// CorrectionInstruction is the line a correction message must carry.
func CorrectionInstruction(issue string, generation int64, digest, where string) string {
	return fmt.Sprintf("Correction generation %d of %s: address the findings of the current ruling; before consuming any input, re-verify every uri, sha256 and byte count in manifest %s (%s) and answer blocked_needs_input on any mismatch.", generation, issue, digest, where)
}

// Prepared is what PrepareCorrection returns.
type Prepared struct {
	ManifestDigest, Instruction, FrozenPath string
}

// PrepareCorrection rebuilds and verifies the node's input manifest from the store as it is now (the inputs may have moved since the first release), stores it, and returns the instruction
// line the correction ruling must carry. The manifest carries the plan revision it was built at; nothing is released or started.
func (s *Scheduler) PrepareCorrection(ctx context.Context, plan, node, actor string, in ManifestInput, opts VerifyOptions) (Prepared, error) {
	var out Prepared
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !found || rel.Status != "active" || rel.ParentTaskID != actor {
		return out, refuse(contract.RefusalRelationshipNotActive, "node %s has no active relationship of task %s to correct", node, actor)
	}
	in.CreatedByTaskID, in.CreatedAt = actor, s.now()
	body, blocked, err := s.BuildManifest(ctx, q, plan, snap, n, in, opts)
	if err != nil {
		return out, err
	}
	if len(blocked) > 0 {
		return out, refusalOfFinding(blocked[0])
	}
	if findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, opts); err != nil {
		return out, err
	} else if len(findings) > 0 {
		return out, refusalOfFinding(findings[0])
	}
	digest, _ := body["manifest_digest"].(string)
	stored, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	if _, err := (&dag.Repo{Store: s.Store, Now: s.Now}).PutManifest(ctx, stored); err != nil {
		return out, err
	}
	out.ManifestDigest = digest
	out.Instruction = CorrectionInstruction(n.IssueKey, rel.Generation+1, digest, "stored in the relay as dag_input_manifests "+digest)
	return out, nil
}

// CorrectionResult is the answer of RecordCorrection.
type CorrectionResult struct {
	PlanID, NodeID, RelationshipID, ManifestDigest string
	Generation                                     int64
	Replayed, CarriedOver                          bool
}

var manifestNamePattern = regexp.MustCompile(`manifest ([0-9a-f]{64})`)

// RecordCorrection binds the generation the parent's needs_changes ruling opened to the node (dag_node_executions, kind correction). The manifest bound is DERIVED from what the child was told:
// the restoration block of the ruling on the previous generation's head, which names the manifest PrepareCorrection returned, never from the caller's omission; a supplied digest only
// cross-checks. A ruling with no such block leaves the previous generation's manifest in force (CarriedOver). The previous acceptance, if any, stays active and reads blocked:stale_head
// (the generation moved) until the parent accepts the corrected result with a supersede; invalidating what depends on it is a later issue.
func (s *Scheduler) RecordCorrection(ctx context.Context, plan, node, actor, suppliedDigest string) (CorrectionResult, error) {
	out := CorrectionResult{PlanID: plan, NodeID: node}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		snap, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		if _, ok := nodeOf(snap, node); !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		}
		rel, found, err := currentRelationshipOf(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to correct", node)
		}
		if rel.Status != "active" {
			return refuse(contract.RefusalRelationshipNotActive, "the relationship %s of %s is %s: a correction goes to a child whose relationship is active", rel.ID, node, rel.Status)
		}
		if rel.ParentTaskID != actor {
			return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s", actor, rel.ID)
		}
		out.RelationshipID, out.Generation = rel.ID, rel.Generation
		var recorded sql.NullInt64
		var latest string
		if err := tx.QueryRowContext(txCtx, "SELECT MAX(execution_generation) FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ?", plan, node, rel.ID).Scan(&recorded); err != nil {
			return err
		}
		if recorded.Valid && recorded.Int64 == rel.Generation {
			if _, err := queryOne(txCtx, tx, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, node, rel.ID, rel.Generation}, &latest); err != nil {
				return err
			}
			out.ManifestDigest, out.Replayed = latest, true
			return nil
		}
		if !recorded.Valid || recorded.Int64 != rel.Generation-1 {
			return refuse(contract.RefusalDispositionConflict, "generation %d of %s follows generation %d, and the last recorded execution of %s is %d", rel.Generation, rel.ID, rel.Generation-1, node, recorded.Int64)
		}
		var reason sql.NullString
		if _, err := queryOne(txCtx, tx, "SELECT reason FROM generations WHERE relationship_id = ? AND execution_generation = ?", []any{rel.ID, rel.Generation}, &reason); err != nil {
			return err
		}
		if reason.String != "needs_changes_revision" {
			return refuse(contract.RefusalDispositionConflict, "generation %d of %s was not opened by a needs_changes ruling (%q)", rel.Generation, rel.ID, reason.String)
		}
		// the ruling on the previous generation's head must have opened this generation
		var findings sql.NullString
		var next sql.NullInt64
		ruled, err := queryOne(txCtx, tx, "SELECT v.next_generation, c.findings FROM verdicts v JOIN events e ON e.event_id = v.event_id JOIN verdict_context c ON c.event_id = v.event_id"+
			" WHERE e.relationship_id = ? AND e.execution_generation = ? AND v.verdict = 'needs_changes' AND v.next_generation = ? ORDER BY v.decided_at DESC LIMIT 1", []any{rel.ID, rel.Generation - 1, rel.Generation}, &next, &findings)
		if err != nil {
			return err
		}
		if !ruled {
			return refuse(contract.RefusalDispositionConflict, "no needs_changes ruling on generation %d of %s opened generation %d", rel.Generation-1, rel.ID, rel.Generation)
		}
		var previous string
		if _, err := queryOne(txCtx, tx, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, node, rel.ID, rel.Generation - 1}, &previous); err != nil {
			return err
		}
		digest, err := restorationDigest(findings.String)
		if err != nil {
			return err
		}
		switch {
		case digest == "":
			digest, out.CarriedOver = previous, true
		default:
			body, stored, err := dag.ReadManifestOn(txCtx, tx, digest)
			if err != nil {
				return refuse(contract.RefusalRevisionMismatch, "the manifest %s named in the ruling does not digest to its name: %v", digest, err)
			}
			if !stored || body["node_id"] != node {
				return refuse(contract.RefusalDispositionConflict, "the manifest %s named in the ruling is not stored for node %s", digest, node)
			}
		}
		if suppliedDigest != "" && suppliedDigest != digest {
			return refuse(contract.RefusalDispositionConflict, "the digest given (%s) is not the one the child was told (%s)", suppliedDigest, digest)
		}
		out.ManifestDigest = digest
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,'correction',NULL)",
			plan, node, rel.ID, rel.Generation, digest)
		return err
	})
	return out, err
}

// restorationDigest reads the manifest digest the ruling's restoration block names: the one finding declared with restoration true, whose note holds "manifest <64 hex>". Two different digests are
// refused; none is "" (the previous manifest stays in force).
func restorationDigest(findings string) (string, error) {
	if findings == "" {
		return "", nil
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(findings), &list); err != nil {
		return "", nil
	}
	found := ""
	for _, f := range list {
		if flag, _ := f["restoration"].(bool); !flag {
			continue
		}
		note, _ := f["note"].(string)
		if m := manifestNamePattern.FindStringSubmatch(note); m != nil {
			if found != "" && found != m[1] {
				return "", refuse(contract.RefusalDispositionConflict, "the restoration block names two manifests, %s and %s", found, m[1])
			}
			found = m[1]
		}
	}
	return found, nil
}
