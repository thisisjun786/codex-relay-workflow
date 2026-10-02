package dagsched

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// BaseRef is the branch a node's work starts from: the repository, the ref and the commit the ref pointed at when the manifest was built.
type BaseRef struct{ Repository, Ref, SHA string }

// Volatile is a thing outside the store a node reads that can change (a Linear document, an issue): a snapshot of it taken before dispatch, so the node reads what the
// manifest says and a later edit is a different manifest.
type Volatile struct{ Source, SnapshotURI, SHA256, CapturedAt string }

// RuleVersion names the rules a node was dispatched under (contract 4.2, 4.3): recorded with the manifest, left out of its digest, and every field required.
type RuleVersion struct{ SkillsDigest, Model, Effort, PromptTemplate, RelayBuild string }

// ManifestInput is what a manifest needs that the store does not hold. CreatedAt is the one clock read of a release and is left out of the digest.
type ManifestInput struct {
	Base            *BaseRef
	Volatile        []Volatile
	RuleVersion     RuleVersion
	CreatedByTaskID string
	CreatedAt       string
}

// VerifyOptions say how much of the world a verification reads. SkipFileBytes leaves every file unread (the store half, which is what can change under the release
// lock); ArtifactRoots are the roots a volatile snapshot has to lie under (the roots of the child's request).
type VerifyOptions struct {
	SkipFileBytes bool
	ArtifactRoots []string
}

// maxInlineManifest is the manifest size a prompt carries inline; a larger one is frozen to a file and the prompt names its path and digest.
const maxInlineManifest = 60000

func findingOfReason(reason, detail string) BlockedFinding {
	for _, p := range BlockedPaths {
		if p.Reason == reason {
			return BlockedFinding{Code: p.Code, Reason: reason, Detail: detail}
		}
	}
	return BlockedFinding{Reason: reason, Detail: detail}
}

func incomingEdges(snap dag.Snapshot, node string) []dag.SnapEdge {
	var out []dag.SnapEdge
	for _, e := range snap.Edges {
		if e.ToNodeID == node {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EdgeID < out[j].EdgeID })
	return out
}

func (r RuleVersion) object() map[string]any {
	return map[string]any{"skills_digest": r.SkillsDigest, "model": r.Model, "effort": r.Effort, "prompt_template": r.PromptTemplate, "relay_build": r.RelayBuild}
}

// shapeFindings are B-01 and B-16: the manifest reads strictly (contract 4.2), every field of the rule version is present, and a node that edits a repository has the base it
// starts from.
func shapeFindings(body map[string]any, node dag.SnapNode) []BlockedFinding {
	var out []BlockedFinding
	if violations := dag.CheckManifest(body); len(violations) > 0 {
		details := make([]string, 0, len(violations))
		for _, v := range violations {
			details = append(details, v.Path+": "+v.Detail)
		}
		out = append(out, BlockedFinding{"B-01", BlockedManifestIncomplete, strings.Join(details, "; ")})
	}
	if rv, ok := body["rule_version"].(map[string]any); ok {
		for _, key := range []string{"skills_digest", "model", "effort", "prompt_template", "relay_build"} {
			if s, _ := rv[key].(string); s == "" {
				out = append(out, BlockedFinding{"B-16", BlockedManifestIncomplete, "rule_version." + key + " is empty: a manifest records the rules it was dispatched under"})
			}
		}
	}
	if node.Kind == dag.NodeImplementation && body["base"] == nil {
		out = append(out, BlockedFinding{"B-01", BlockedManifestIncomplete, "an implementation node needs the base it starts from (repository, ref, sha)"})
	}
	return out
}

// BuildManifest builds the input manifest of a node from the store (contract 4.2): for every incoming edge the value that satisfied it, each artifact's uri, hash, size and
// scope, and the base and volatile snapshots the caller supplies. Every input is verified as it is built; the findings it returns are the contract's blocked paths and an
// empty list means the manifest is whole. It writes nothing.
func (s *Scheduler) BuildManifest(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, node dag.SnapNode, in ManifestInput, opts VerifyOptions) (map[string]any, []BlockedFinding, error) {
	var blocked []BlockedFinding
	edges := incomingEdges(snap, node.NodeID)
	statuses := map[string]EdgeStatus{}
	inputs := []any{}
	for _, e := range edges {
		st, err := s.edgeStatus(ctx, q, plan, snap, e)
		if err != nil {
			return nil, nil, err
		}
		statuses[e.EdgeID] = st
		if !st.Satisfied {
			blocked = append(blocked, findingOfReason(st.Reason, st.Detail))
			continue
		}
		input, finding, err := s.buildInput(ctx, q, e, st, opts)
		if err != nil {
			return nil, nil, err
		}
		if finding != nil {
			blocked = append(blocked, *finding)
			continue
		}
		inputs = append(inputs, input)
	}
	if len(blocked) == 0 {
		finding, err := s.closure(ctx, q, edges, statuses)
		if err != nil {
			return nil, nil, err
		}
		if finding != nil {
			blocked = append(blocked, *finding)
		}
	}
	body := map[string]any{
		"schema": dag.SchemaManifest, "node_id": node.NodeID, "issue_key": node.IssueKey, "node_slice_digest": node.SliceDigest, "criteria_set_digest": node.CriteriaSetDigest,
		"inputs": inputs, "rule_version": in.RuleVersion.object(), "plan_revision_no": snap.Revision, "coordinator_epoch": int64(0),
		"created_by_task_id": in.CreatedByTaskID, "created_at": in.CreatedAt,
	}
	if in.Base != nil {
		body["base"] = map[string]any{"repository": in.Base.Repository, "ref": in.Base.Ref, "sha": in.Base.SHA}
	}
	if len(in.Volatile) > 0 {
		list := make([]any, len(in.Volatile))
		for i, v := range in.Volatile {
			list[i] = map[string]any{"source": v.Source, "snapshot_uri": v.SnapshotURI, "sha256": v.SHA256, "captured_at": v.CapturedAt}
		}
		body["volatile"] = list
		if finding := s.verifyVolatile(ctx, in.Volatile, opts); finding != nil {
			blocked = append(blocked, *finding)
		}
	}
	blocked = append(blocked, shapeFindings(body, node)...)
	if len(blocked) == 0 {
		body["manifest_digest"] = dag.ManifestDigest(body)
	}
	return body, blocked, nil
}

// buildInput is the input one satisfied edge contributes: the acceptance it rests on and the value the node reads (a pinned head, the declared artifacts, the landed commit
// or the recorded decision).
func (s *Scheduler) buildInput(ctx context.Context, q store.Querier, e dag.SnapEdge, st EdgeStatus, opts VerifyOptions) (map[string]any, *BlockedFinding, error) {
	input := map[string]any{"edge_id": e.EdgeID, "kind": e.Kind, "from_node_id": e.FromNodeID}
	switch e.Kind {
	case dag.EdgeArtifactVerified:
		a, err := loadAcceptanceByID(ctx, q, st.AcceptanceID)
		if err != nil {
			return nil, nil, err
		}
		input["acceptance_id"], input["relationship_id"], input["execution_generation"] = a.AcceptanceID, a.RelationshipID, a.ExecutionGeneration
		input["event_id"], input["revision_hash"] = a.EventID, a.RevisionHash
		if e.PinsCodeHead {
			input["head_sha"] = a.HeadSHA
			return input, nil, nil
		}
		finding, _, err := s.checkArtifacts(ctx, q, a.AcceptanceID, opts.SkipFileBytes)
		if err != nil || finding != nil {
			return nil, finding, err
		}
		var receipt, roots string
		if _, err := queryOne(ctx, q, "SELECT e.receipt, r.artifact_roots FROM events e JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.event_id = ?", []any{a.EventID}, &receipt, &roots); err != nil {
			return nil, nil, err
		}
		entries, _ := receiptEntries(receipt)
		var artifactRoots []string
		_ = json.Unmarshal([]byte(roots), &artifactRoots)
		artifacts := make([]any, 0, len(entries))
		for _, entry := range entries {
			scope := scopeOf(entry.Path, artifactRoots)
			if scope == "" {
				return nil, &BlockedFinding{"B-05", BlockedInputOutOfScope, entry.Path + " lies under none of the predecessor's artifact roots"}, nil
			}
			size := int64(0)
			if entry.Bytes != nil {
				size = *entry.Bytes
			} else if info, err := os.Stat(entry.Path); err == nil {
				size = info.Size()
			}
			artifacts = append(artifacts, map[string]any{"uri": entry.Path, "sha256": entry.SHA256, "bytes": size, "scope": scope})
		}
		input["artifacts"] = artifacts
	case dag.EdgeIntegrated:
		a, err := loadAcceptanceByID(ctx, q, st.AcceptanceID)
		if err != nil {
			return nil, nil, err
		}
		var landed string
		if _, err := queryOne(ctx, q, "SELECT COALESCE(NULLIF(m.landed_sha, ''), o.tip_sha) FROM dag_integration_observations o LEFT JOIN merge_turns m ON m.turn_id = o.merge_turn_id"+
			" WHERE o.acceptance_id = ? AND o.repository = ? AND o.base_ref = ? AND o.is_ancestor = 1 ORDER BY o.observed_seq DESC LIMIT 1",
			[]any{a.AcceptanceID, e.TargetRepository, e.TargetBaseRef}, &landed); err != nil {
			return nil, nil, err
		}
		input["acceptance_id"], input["head_sha"], input["landed_sha"] = a.AcceptanceID, a.HeadSHA, landed
	case dag.EdgeDecision:
		id, revision, err := s.decisionOf(ctx, q, e)
		if err != nil {
			return nil, nil, err
		}
		input["decision_id"], input["decision_digest"], input["decision_revision"] = id, e.DecisionDigest, revision
	}
	return input, nil, nil
}

// decisionOf is the decision that satisfies a decision edge: the recorded decision with the highest revision (P-DEC-2) or, failing that, the supervisor directive that settled the digest (P-DEC-1).
func (s *Scheduler) decisionOf(ctx context.Context, q store.Querier, e dag.SnapEdge) (string, int64, error) {
	authority, err := json.Marshal(e.RequiredAuthority)
	if err != nil {
		return "", 0, err
	}
	var id string
	var revision int64
	found, err := queryOne(ctx, q, "SELECT x.decision_id, x.revision FROM dag_decisions x WHERE x.subject = ? AND x.digest = ? AND x.disposition = 'approved' AND x.state = 'active'"+
		" AND x.authority_kind IN (SELECT value FROM json_each(?)) ORDER BY x.revision DESC LIMIT 1", []any{e.DecisionSubject, e.DecisionDigest, string(authority)}, &id, &revision)
	if err != nil || found {
		return id, revision, err
	}
	found, err = queryOne(ctx, q, "SELECT directive_id FROM scope_directives WHERE digest = ? AND disposition = 'chosen' ORDER BY decided_at DESC LIMIT 1", []any{e.DecisionDigest}, &id)
	if err != nil {
		return "", 0, err
	}
	if !found {
		return "", 0, errors.New("the decision of edge " + e.EdgeID + " is not recorded")
	}
	return id, 0, nil
}

// scopeOf is the artifact root a path lies under: the longest one, so nested roots are told apart.
func scopeOf(path string, roots []string) string {
	best := ""
	for _, root := range roots {
		clean := strings.TrimRight(root, "/")
		if (path == clean || strings.HasPrefix(path, clean+"/")) && len(clean) > len(best) {
			best = root
		}
	}
	return best
}

// verifyVolatile is the contract's check of what is outside the store: every snapshot is a file under one of the child's artifact roots, is there, and hashes to the digest the manifest names.
func (s *Scheduler) verifyVolatile(ctx context.Context, volatile []Volatile, opts VerifyOptions) *BlockedFinding {
	for _, v := range volatile {
		if !filepath.IsAbs(v.SnapshotURI) || scopeOf(v.SnapshotURI, opts.ArtifactRoots) == "" {
			return &BlockedFinding{"B-05", BlockedInputOutOfScope, "volatile snapshot " + v.SnapshotURI + " is not a file under one of the child's artifact roots"}
		}
		if opts.SkipFileBytes {
			continue
		}
		finding, _, err := hashEntry(ctx, consumed{Path: v.SnapshotURI, SHA256: v.SHA256}, []string{scopeOf(v.SnapshotURI, opts.ArtifactRoots)})
		if err != nil {
			return &BlockedFinding{"B-03", BlockedInputMissing, v.SnapshotURI + ": " + err.Error()}
		}
		if finding != nil {
			return finding
		}
	}
	return nil
}

// VerifyManifest judges a manifest against the store and the disk as they are now (contract 4.4): it reads strictly, digests to its name, rests on the edges the node has and on
// their current acceptances, and every artifact and volatile snapshot it names is there and hashes to what it says. The findings are the violated paths; none means the manifest is
// fit to release. It writes nothing.
func (s *Scheduler) VerifyManifest(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, node dag.SnapNode, body map[string]any, opts VerifyOptions) ([]BlockedFinding, error) {
	if out := shapeFindings(body, node); len(out) > 0 {
		return out, nil
	}
	if claimed, _ := body["manifest_digest"].(string); claimed != dag.ManifestDigest(body) {
		return []BlockedFinding{{"B-02", BlockedManifestTampered, "the manifest digests to " + dag.ManifestDigest(body) + " and carries " + claimed}}, nil
	}
	edges := incomingEdges(snap, node.NodeID)
	byEdge := map[string]dag.SnapEdge{}
	for _, e := range edges {
		byEdge[e.EdgeID] = e
	}
	items, _ := body["inputs"].([]any)
	if len(items) != len(edges) {
		return []BlockedFinding{{"B-01", BlockedManifestIncomplete, fmt.Sprintf("the manifest has %d inputs and the node has %d incoming edges", len(items), len(edges))}}, nil
	}
	statuses := map[string]EdgeStatus{}
	var out []BlockedFinding
	for _, item := range items {
		in, _ := item.(map[string]any)
		id, _ := in["edge_id"].(string)
		e, ok := byEdge[id]
		if !ok || in["kind"] != e.Kind || in["from_node_id"] != e.FromNodeID {
			return []BlockedFinding{{"B-01", BlockedManifestIncomplete, "an input of the manifest is not an incoming edge of the node: " + id}}, nil
		}
		st, err := s.edgeStatus(ctx, q, plan, snap, e)
		if err != nil {
			return nil, err
		}
		statuses[id] = st
		if !st.Satisfied {
			out = append(out, findingOfReason(st.Reason, st.Detail))
			continue
		}
		if finding, err := s.verifyInput(ctx, q, e, st, in, opts); err != nil {
			return nil, err
		} else if finding != nil {
			out = append(out, *finding)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if volatile, _ := body["volatile"].([]any); len(volatile) > 0 {
		var list []Volatile
		for _, item := range volatile {
			v, _ := item.(map[string]any)
			list = append(list, Volatile{Source: textOf(v["source"]), SnapshotURI: textOf(v["snapshot_uri"]), SHA256: textOf(v["sha256"]), CapturedAt: textOf(v["captured_at"])})
		}
		if finding := s.verifyVolatile(ctx, list, opts); finding != nil {
			return []BlockedFinding{*finding}, nil
		}
	}
	finding, err := s.closure(ctx, q, edges, statuses)
	if err != nil || finding == nil {
		return nil, err
	}
	return []BlockedFinding{*finding}, nil
}

// verifyInput compares one input of a manifest with what its edge is satisfied by now.
func (s *Scheduler) verifyInput(ctx context.Context, q store.Querier, e dag.SnapEdge, st EdgeStatus, in map[string]any, opts VerifyOptions) (*BlockedFinding, error) {
	switch e.Kind {
	case dag.EdgeArtifactVerified, dag.EdgeIntegrated:
		if in["acceptance_id"] != st.AcceptanceID {
			return &BlockedFinding{"B-06", BlockedInputUnaccepted, "the manifest rests on acceptance " + textOf(in["acceptance_id"]) + " and the edge is now satisfied by " + st.AcceptanceID}, nil
		}
		a, err := loadAcceptanceByID(ctx, q, st.AcceptanceID)
		if err != nil {
			return nil, err
		}
		if (e.Kind == dag.EdgeIntegrated || e.PinsCodeHead) && in["head_sha"] != a.HeadSHA {
			return &BlockedFinding{"B-09", BlockedStaleHead, "the manifest names head " + textOf(in["head_sha"]) + " and the accepted head is " + a.HeadSHA}, nil
		}
		if e.Kind == dag.EdgeArtifactVerified && !e.PinsCodeHead {
			var roots string
			if _, err := queryOne(ctx, q, "SELECT r.artifact_roots FROM relationships r WHERE r.relationship_id = ?", []any{a.RelationshipID}, &roots); err != nil {
				return nil, err
			}
			var artifactRoots []string
			_ = json.Unmarshal([]byte(roots), &artifactRoots)
			artifacts, _ := in["artifacts"].([]any)
			if len(artifacts) == 0 {
				return &BlockedFinding{"B-17", BlockedInputMissing, "the input of edge " + e.EdgeID + " names no artifact"}, nil
			}
			for _, item := range artifacts {
				art, _ := item.(map[string]any)
				uri, scope := textOf(art["uri"]), textOf(art["scope"])
				member := false
				for _, r := range artifactRoots {
					member = member || r == scope
				}
				if !member || scopeOf(uri, []string{scope}) == "" {
					return &BlockedFinding{"B-05", BlockedInputOutOfScope, uri + " is not under a root of the predecessor (" + scope + ")"}, nil
				}
				if opts.SkipFileBytes {
					continue
				}
				size, _ := art["bytes"].(int64)
				finding, _, err := hashEntry(ctx, consumed{Path: uri, SHA256: textOf(art["sha256"]), Bytes: &size}, []string{scope})
				if err != nil || finding != nil {
					return finding, err
				}
			}
		}
	case dag.EdgeDecision:
		if in["decision_digest"] != e.DecisionDigest {
			return &BlockedFinding{"B-11", BlockedDecisionMismatch, "the manifest names decision digest " + textOf(in["decision_digest"]) + " and the plan fixed " + e.DecisionDigest}, nil
		}
	}
	return nil, nil
}

// frozenManifestPath is where a manifest too large for a prompt is kept: under the child's first artifact root, named by its digest.
func frozenManifestPath(root, digest string) string {
	return filepath.Join(root, "dag-input-manifests", digest+".json")
}

// FreezeManifest writes the canonical bytes of a manifest to path, create-exclusive and 0600, and reads them back and hashes them: a copy that is not what was written is an
// error and never a manifest. An existing file is accepted only when it holds the same bytes (two releases of one manifest write one file).
func FreezeManifest(path string, canonical []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("freeze the manifest: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		_, werr := file.Write(canonical)
		if cerr := file.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("freeze the manifest: %w", werr)
		}
	case !errors.Is(err, os.ErrExist):
		return fmt.Errorf("freeze the manifest: %w", err)
	}
	read, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("freeze the manifest: %w", err)
	}
	if !bytes.Equal(read, canonical) {
		return fmt.Errorf("freeze the manifest: %s holds other bytes than the manifest (%s, %s)", path, shaOf(read), shaOf(canonical))
	}
	return nil
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
