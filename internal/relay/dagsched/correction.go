package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A correction goes back to the same child (contract 3.2): the parent's needs_changes ruling opens the next generation of the SAME relationship and the existing verdict writer sends the
// correction message in the same transaction, so a manifest cannot be put into that generation afterwards without the child never having seen it. The protocol therefore puts the manifest
// into the message the relay itself sends: PrepareCorrection builds and stores the manifest and returns an instruction line that the parent submits as the restoration block of the ruling
// (the one finding the relay renders into the correction message, delivery/criteria.go), and RecordCorrection, after the ruling, binds the generation to the manifest the restoration
// block names.

// CorrectionInstruction is the line a correction message must carry. The child cannot read the relay's store, so the manifest travels as a file under its own artifact root: the line names
// the path, the manifest digest (the one name of the manifest in the line, which RecordCorrection reads back) and the sha256 of the file's bytes.
func CorrectionInstruction(issue string, generation int64, digest, path, fileSHA string) string {
	return fmt.Sprintf("Correction generation %d of %s: address the findings of the current ruling. Before consuming any input, read the input manifest at %s (manifest %s, file sha256 %s), re-verify every uri, sha256 and byte count in it against the files on disk, and answer blocked_needs_input on any mismatch.", generation, issue, path, digest, fileSHA)
}

// Prepared is what PrepareCorrection returns.
type Prepared struct {
	ManifestDigest, Instruction, FrozenPath string
}

// PrepareCorrection rebuilds and verifies the node's input manifest from the store as it is now (the inputs may have moved since the first release), stores it, keeps a copy of its
// canonical bytes under the child's first artifact root (the file the child reads), and returns the instruction line the correction ruling must carry. The manifest carries the plan
// revision it was built at; nothing is released or started.
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
	if err := lifecycleRefusal(snap, n, "correcting it", false); err != nil {
		return out, err
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !found || rel.Status != "active" || rel.Superseded || rel.ParentTaskID != actor {
		return out, refuse(contract.RefusalRelationshipNotActive, "node %s has no active relationship of task %s to correct", node, actor)
	}
	roots, err := relationshipRoots(ctx, q, rel.ID)
	if err != nil {
		return out, err
	}
	if len(roots) == 0 {
		return out, refuse(contract.RefusalMalformedReceipt, "relationship %s has no artifact root to keep the manifest in", rel.ID)
	}
	// the line travels in a finding that the relay's renderer joins onto one line, so a path (or an issue key) with a character it splits lines on would reach the child as another text
	if !lineSafe(roots[0]) || !lineSafe(n.IssueKey) {
		return out, refuse(contract.RefusalMalformedReceipt, "the artifact root %q or the issue key %q has a line-breaking character, so the path of the manifest copy cannot be handed to the child in one line", roots[0], n.IssueKey)
	}
	if len(opts.ArtifactRoots) == 0 {
		opts.ArtifactRoots = roots
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
	// the manifest of a digest is the body the store holds: the first one stored (a body built later differs in what the digest leaves out, such as the time of the build)
	keptBody, found, err := dag.ReadManifestOn(ctx, q, digest)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("manifest %s was stored and cannot be read back", digest)
	}
	canonical := []byte(dag.Canonical(keptBody))
	path, err := FreezeManifest(roots[0], canonical)
	if err != nil {
		return out, err
	}
	out.ManifestDigest = digest
	out.FrozenPath = path
	out.Instruction = CorrectionInstruction(n.IssueKey, rel.Generation+1, digest, path, shaOf(canonical))
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
		n, ok := nodeOf(snap, node)
		if !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		}
		if err := lifecycleRefusal(snap, n, "correcting it", false); err != nil {
			return err
		}
		rel, found, err := currentRelationshipOf(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to correct", node)
		}
		if rel.Status != "active" || rel.Superseded {
			return refuse(contract.RefusalRelationshipNotActive, "the relationship %s of %s is %s: a correction goes to a child whose relationship is active", rel.ID, node, map[bool]string{true: "superseded", false: rel.Status}[rel.Superseded])
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
			if suppliedDigest != "" && suppliedDigest != latest {
				return refuse(contract.RefusalDispositionConflict, "generation %d of %s is bound to manifest %s and the digest given is %s", rel.Generation, rel.ID, latest, suppliedDigest)
			}
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
		digest, note, err := restorationDigest(findings.String)
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
			if !stored || body["node_id"] != node || body["issue_key"] != n.IssueKey {
				return refuse(contract.RefusalDispositionConflict, "the manifest %s named in the ruling is not stored for node %s of %s", digest, node, n.IssueKey)
			}
			// a manifest is one version of one node: the same node id in another plan, or this node before a plan revision changed it, is not what the child should now consume
			if body["node_slice_digest"] != n.SliceDigest || body["criteria_set_digest"] != n.CriteriaSetDigest {
				return refuse(contract.RefusalDispositionConflict, "the manifest %s was prepared for another version of node %s than the plan holds now: prepare it again and rule again", digest, node)
			}
			// the child reads a file named in the ruling's note: the note has to carry exactly the line the manifest was prepared with (this generation, the path of the copy and its hash), so a
			// manifest the child was told about under another path or another hash is not the one bound. No file is read here: the child verifies the copy against the hash in the line.
			roots, err := relationshipRoots(txCtx, tx, rel.ID)
			if err != nil {
				return err
			}
			if len(roots) == 0 {
				return refuse(contract.RefusalManifestUnverified, "relationship %s has no artifact root, so the child cannot have been given the manifest %s", rel.ID, digest)
			}
			canonical := []byte(dag.Canonical(body))
			want := CorrectionInstruction(n.IssueKey, rel.Generation, digest, frozenManifestPath(roots[0], canonical), shaOf(canonical))
			if !lineSafe(want) {
				return refuse(contract.RefusalMalformedReceipt, "the artifact root of %s has a control character: the relay's message would carry another path than the one the manifest copy has", rel.ID)
			}
			if !strings.Contains(note, want) {
				return refuse(contract.RefusalDispositionConflict, "the restoration note does not carry the instruction manifest %s was prepared with (generation %d, its path and its file hash): the child was not told this manifest as prepared", digest, rel.Generation)
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

// lineSafe is whether a text keeps its form when the relay's message joins the lines of a finding: it holds no character that Python's str.splitlines, which the renderer uses, splits on
// (the ASCII control characters including \n \r \v \f and \x1c to \x1e, U+0085, U+2028 and U+2029) and no DEL.
func lineSafe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// relationshipRoots are the artifact roots of a relationship, the places its child reads and writes.
func relationshipRoots(ctx context.Context, q store.Querier, rid string) ([]string, error) {
	var raw string
	if found, err := queryOne(ctx, q, "SELECT artifact_roots FROM relationships WHERE relationship_id = ?", []any{rid}, &raw); err != nil || !found {
		return nil, err
	}
	var roots []string
	if json.Unmarshal([]byte(raw), &roots) != nil {
		return nil, nil
	}
	return roots, nil
}

// restorationDigest reads the manifest digest the ruling's restoration block names: every "manifest <64 hex>" in the note of the one finding declared with restoration true. Two different
// digests anywhere in the block are refused, because the child would be told two manifests; none is "" (the previous manifest stays in force).
func restorationDigest(findings string) (string, string, error) {
	if findings == "" {
		return "", "", nil
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(findings), &list); err != nil {
		return "", "", nil
	}
	found := map[string]bool{}
	var carried string
	for _, f := range list {
		if flag, _ := f["restoration"].(bool); !flag {
			continue
		}
		note, _ := f["note"].(string)
		carried = note
		for _, m := range manifestNamePattern.FindAllStringSubmatch(note, -1) {
			found[m[1]] = true
		}
	}
	switch len(found) {
	case 0:
		return "", "", nil
	case 1:
		for digest := range found {
			return digest, carried, nil
		}
	}
	names := make([]string, 0, len(found))
	for digest := range found {
		names = append(names, digest)
	}
	sort.Strings(names)
	return "", "", refuse(contract.RefusalDispositionConflict, "the restoration block names %d manifests (%s): the child would be told more than one", len(names), strings.Join(names, ", "))
}
