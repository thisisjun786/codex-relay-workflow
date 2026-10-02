package dagsched

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SchemaReleaseRequest names the document dag-release reads.
const SchemaReleaseRequest = "dag-release-request/1"

// SlotSubjectKind is the subject kind of the execution slot a release holds (D-15: release is coupled with slot reservation).
const SlotSubjectKind = "dag_node"

// SlotSubjectKey is the subject key of a node's slot: the plan and the node joined by a character neither id may contain (ids are letters, digits and . _ : -), so two different nodes can
// never share a slot (with a colon, plan "a:b" node "c" and plan "a" node "b:c" would, and a second release would read as the replay of the first).
func SlotSubjectKey(plan, node string) string { return plan + "/" + node }

// Criterion is one completion criterion of the child's assignment.
type Criterion struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Required bool   `json:"required"`
}

// BaseSpec is the branch an implementation node starts from; the commit is read from the target, never given.
type BaseSpec struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

// Endpoint is one side of the managed-start request: the host, the settings the task runs with, and (child) its title.
type Endpoint struct {
	HostID   string         `json:"host_id"`
	Title    string         `json:"title"`
	Settings map[string]any `json:"settings"`
}

// ReleaseRequest is the document dag-release reads (dag-release-request/1): everything a managed child needs that the plan and the store do not hold.
type ReleaseRequest struct {
	Schema            string      `json:"schema"`
	Base              *BaseSpec   `json:"base"`
	RuleVersion       RuleVersion `json:"rule_version"`
	Volatile          []Volatile  `json:"volatile"`
	Instructions      string      `json:"instructions"`
	Criteria          []Criterion `json:"criteria"`
	CriteriaSource    string      `json:"criteria_source"`
	ScopeRef          string      `json:"scope_ref"`
	ArtifactRoots     []string    `json:"artifact_roots"`
	AllowedRecipients []string    `json:"allowed_recipients"`
	Parent            Endpoint    `json:"parent"`
	Child             Endpoint    `json:"child"`
}

// ReleaseResult is what Release answers. Bound is true once the child is bound to the node; a managed start that was refused or left incomplete leaves the intent and the slot in
// place and says where it stopped.
type ReleaseResult struct {
	PlanID, NodeID, ManifestDigest, RequestID, SlotID string
	Replayed, Bound                                   bool
	State, Stage, Reason                              string
	RelationshipID, ChildTaskID                       string
	Generation                                        int64
	Managed                                           contract.OrderedObject
}

// DecodeReleaseRequest reads the request strictly: an unknown field, a missing text, a list over its bound or a trailing document is refused.
func DecodeReleaseRequest(raw []byte) (ReleaseRequest, error) {
	var req ReleaseRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return ReleaseRequest{}, refuse(contract.RefusalMalformedReceipt, "the release request is not a %s document: %v", SchemaReleaseRequest, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ReleaseRequest{}, refuse(contract.RefusalMalformedReceipt, "the release request holds more than one document")
	}
	blank := func(name, v string) error {
		if strings.TrimSpace(v) == "" {
			return refuse(contract.RefusalMalformedReceipt, "the release request has no %s", name)
		}
		return nil
	}
	if req.Schema != SchemaReleaseRequest {
		return ReleaseRequest{}, refuse(contract.RefusalMalformedReceipt, "the release request schema is %s", SchemaReleaseRequest)
	}
	for name, v := range map[string]string{"instructions": req.Instructions, "criteria_source": req.CriteriaSource, "scope_ref": req.ScopeRef, "parent.host_id": req.Parent.HostID,
		"child.host_id": req.Child.HostID, "child.title": req.Child.Title} {
		if err := blank(name, v); err != nil {
			return ReleaseRequest{}, err
		}
	}
	if len(req.Criteria) == 0 || len(req.Criteria) > 256 || len(req.ArtifactRoots) == 0 || len(req.ArtifactRoots) > 64 || len(req.AllowedRecipients) > 64 || len(req.Volatile) > 64 {
		return ReleaseRequest{}, refuse(contract.RefusalMalformedReceipt, "criteria, artifact_roots, allowed_recipients and volatile are lists of bounded size, and criteria and artifact_roots are not empty")
	}
	if req.Parent.Settings == nil || req.Child.Settings == nil {
		return ReleaseRequest{}, refuse(contract.RefusalMalformedReceipt, "parent.settings and child.settings are the settings the tasks run with")
	}
	return req, nil
}

// releaseRow is a decided release: the manifest it was decided on and the managed-start request id derived from it.
type releaseRow struct{ Digest, Request string }

func latestRelease(ctx context.Context, q store.Querier, plan, node string) (releaseRow, bool, error) {
	var r releaseRow
	found, err := queryOne(ctx, q, "SELECT manifest_digest, managed_request_id FROM dag_releases WHERE plan_id = ? AND node_id = ? ORDER BY decided_at DESC, manifest_digest DESC LIMIT 1", []any{plan, node}, &r.Digest, &r.Request)
	return r, found, err
}

// boundChild is the execution a release's request id is bound to, if any.
func boundChild(ctx context.Context, q store.Querier, plan, node, request string) (relationship string, generation int64, child string, found bool, err error) {
	found, err = queryOne(ctx, q, "SELECT e.relationship_id, e.execution_generation, r.child_task_id FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" WHERE e.plan_id = ? AND e.node_id = ? AND e.managed_request_id = ?", []any{plan, node, request}, &relationship, &generation, &child)
	return
}

func (s *Scheduler) slotOf(ctx context.Context, q store.Querier, plan, node string) (string, error) {
	var id string
	_, err := queryOne(ctx, q, "SELECT slot_id FROM execution_slots WHERE subject_kind = ? AND subject_key = ? AND state = 'held' ORDER BY tenure DESC LIMIT 1", []any{SlotSubjectKind, SlotSubjectKey(plan, node)}, &id)
	return id, err
}

// Release gives a ready node to a Codex child (contract 2.6, 3.2, 4.4, D-15). It is idempotent and never creates a second child:
//
//  1. an intent already recorded for the node is replayed first, from the exact request bytes frozen with it, and never rebuilt;
//  2. a new release is judged outside any transaction (the reading, the freshness of pinned pull requests, the criteria, the manifest built and verified, hashing included);
//  3. the managed-start request is assembled (its id derived from the node and the manifest only) and parsed;
//  4. under one transaction the same judgement is repeated on the store half, the slot is reserved and the manifest, the frozen request and the intent are written together;
//  5. the managed start runs outside any transaction, with the frozen bytes;
//  6. the child it created is bound to the node.
//
// A refused or incomplete start leaves the intent and the slot in place: the next call reaches step 5 again and the managed engine reconciles with the creation it already made.
func (s *Scheduler) Release(ctx context.Context, plan, node, actor string, req ReleaseRequest) (ReleaseResult, error) {
	out := ReleaseResult{PlanID: plan, NodeID: node}
	// 0. a session that does not hold the plan's epoch decides nothing, and is told nothing about the release (the transactions below check again, where they write)
	q := s.Store.Q(ctx)
	if err := s.fence(ctx, q, plan, actor); err != nil {
		return out, err
	}
	// 1. replay first
	if row, found, err := latestRelease(ctx, q, plan, node); err != nil {
		return out, err
	} else if found {
		return s.replay(ctx, plan, node, actor, row)
	}
	// 2. a new release, judged on plain reads
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	var base *BaseRef
	if n.Kind == dag.NodeImplementation {
		if req.Base == nil {
			return out, refuse(contract.RefusalMalformedReceipt, "an implementation node starts from a base: the request names its repository and ref")
		}
		if s.Tips == nil {
			return out, errors.New("this scheduler has no target reader, so it cannot read the tip the child starts from")
		}
		tip, err := s.Tips.Tip(ctx, req.Base.Repository, req.Base.Ref)
		if err != nil {
			return out, err
		}
		base = &BaseRef{Repository: req.Base.Repository, Ref: req.Base.Ref, SHA: tip.SHA}
	}
	reading, err := s.Ready(ctx, q, plan, ReadyOptions{})
	if err != nil {
		return out, err
	}
	if s.testAfterReading != nil {
		s.testAfterReading()
	}
	r, known := reading.find(node)
	if !known {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	if r.Disposition != DispReady && r.Reason != DeferNoCapacity {
		// a concurrent call may have decided the release since the replay-first read: then this call is its replay, not a refusal.
		if row, found, err := latestRelease(ctx, q, plan, node); err != nil {
			return out, err
		} else if found {
			return s.replay(ctx, plan, node, actor, row)
		}
		return out, refusalOfReading(r)
	}
	if err := s.checkCriteria(n, req.Criteria); err != nil {
		return out, err
	}
	preds, err := s.pinnedPredecessors(ctx, q, plan, incomingEdges(snap, node))
	if err != nil {
		return out, err
	}
	if err := s.freshness(ctx, preds); err != nil {
		return out, err
	}
	if s.testAfterFreshness != nil {
		s.testAfterFreshness()
	}
	opts := VerifyOptions{ArtifactRoots: req.ArtifactRoots}
	in := ManifestInput{Base: base, Volatile: req.Volatile, RuleVersion: req.RuleVersion, CreatedByTaskID: actor, CreatedAt: s.now()}
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
	// E-10 binds the acceptance whose pull request was read to the one the manifest consumes: a predecessor accepted again while the forge was being read is a different head that was
	// never checked.
	checked := map[string]bool{}
	for _, p := range preds {
		checked[p.Acceptance.AcceptanceID] = true
	}
	for _, e := range incomingEdges(snap, node) {
		if !e.PinsCodeHead {
			continue
		}
		if acceptance := inputAcceptance(body, e.EdgeID); !checked[acceptance] {
			return out, refuse(contract.RefusalDispositionConflict, "the manifest rests on acceptance %s for the pinned edge %s, whose pull request the relay did not read: the predecessor's acceptance changed while the release was judged; repeat it", acceptance, e.EdgeID)
		}
	}
	for _, p := range preds {
		if !restsOn(body, p.Acceptance.AcceptanceID) {
			return out, refuse(contract.RefusalDispositionConflict, "the acceptance of %s changed while its pull request %s#%d was being read; repeat the release so the current head is checked", p.Acceptance.NodeID, p.Forge, p.Number)
		}
	}
	digest, _ := body["manifest_digest"].(string)
	out.ManifestDigest, out.RequestID = digest, ReleaseRequestID(plan, node, digest)
	// 3. the request
	raw, err := s.assemble(plan, n, snap.ProjectKey, actor, req, base, body, digest)
	if err != nil {
		return out, err
	}
	if s.testBetweenReadAndIntent != nil {
		s.testBetweenReadAndIntent()
	}
	// 4. the intent
	var stash error
	var concurrent bool
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		if _, exists, err := existingRelease(txCtx, tx, plan, node, digest); err != nil {
			return err
		} else if exists {
			concurrent = true
			return nil
		}
		// a refusal is returned after the transaction (and the conflict row a refusal recorded commits with it); any other failure rolls everything back.
		if refused, err := asRefusal(s.rejudge(txCtx, tx, plan, node, body, opts, actor)); err != nil {
			return err
		} else if refused != nil {
			stash = refused
			return nil
		}
		if refused, err := asRefusal(s.reserve(txCtx, tx, snap.ProjectKey, plan, node, actor, digest)); err != nil {
			return err
		} else if refused != nil {
			stash = refused
			return nil
		}
		if s.testAfterReserve != nil {
			if err := s.testAfterReserve(); err != nil {
				return err
			}
		}
		stored, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if _, err := (&dag.Repo{Store: s.Store, Now: s.Now}).PutManifest(txCtx, stored); err != nil {
			return err
		}
		at := s.now()
		if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_release_requests (plan_id, node_id, manifest_digest, request_sha256, request_json, marker_root, socket, state_selector, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)",
			plan, node, digest, shaOf(raw), string(raw), s.Selectors.MarkerRoot, s.Selectors.Socket, s.Selectors.StateSelector, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,?,?)", plan, node, digest, out.RequestID, s.ExpectedEpoch, at)
		return err
	})
	if err != nil {
		return out, err
	}
	if stash != nil {
		return out, stash
	}
	if concurrent {
		row, found, err := latestRelease(ctx, s.Store.Q(ctx), plan, node)
		if err != nil || !found {
			return out, errors.Join(err, errors.New("a concurrent release of the node was decided and cannot be read"))
		}
		return s.replay(ctx, plan, node, actor, row)
	}
	return s.startAndBind(ctx, plan, node, actor, out, raw, false)
}

func existingRelease(ctx context.Context, q store.Querier, plan, node, digest string) (string, bool, error) {
	var request string
	found, err := queryOne(ctx, q, "SELECT managed_request_id FROM dag_releases WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?", []any{plan, node, digest}, &request)
	return request, found, err
}

// checkCriteria is B-10 at release: the criteria the child will be registered with are the plan's.
func (s *Scheduler) checkCriteria(n dag.SnapNode, criteria []Criterion) error {
	list := make([]delivery.Criterion, len(criteria))
	for i, c := range criteria {
		list[i] = delivery.Criterion{ID: strings.TrimSpace(c.ID), Title: strings.TrimSpace(c.Title), Required: c.Required}
	}
	if got := delivery.SetDigest(list); got != n.CriteriaSetDigest {
		return refuse(contract.RefusalCriteriaSetChanged, "the request's criteria digest to %s and the plan fixed %s for node %s", got, n.CriteriaSetDigest, n.NodeID)
	}
	return nil
}

// rejudge repeats, under the lock, what the store half of the judgement said before it (a plan revision, an acceptance or a registration may have moved in between): the node is
// still ready or waiting only for capacity, the manifest still matches the node's slice, criteria and incoming edges, every input still stands, and nobody owns the issue.
func (s *Scheduler) rejudge(ctx context.Context, q store.Querier, plan, node string, body map[string]any, opts VerifyOptions, actor string) error {
	reading, err := s.Ready(ctx, q, plan, ReadyOptions{SkipArtifactBytes: true})
	if err != nil {
		return err
	}
	r, known := reading.find(node)
	if !known {
		return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	if r.Disposition != DispReady && r.Reason != DeferNoCapacity {
		return refusalOfReading(r)
	}
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return err
	}
	n, _ := nodeOf(snap, node)
	edges := incomingEdges(snap, node)
	ids := make([]string, len(edges))
	for i, e := range edges {
		ids[i] = e.EdgeID
	}
	var held []string
	inputs, _ := body["inputs"].([]any)
	for _, item := range inputs {
		in, _ := item.(map[string]any)
		held = append(held, textOf(in["edge_id"]))
	}
	sort.Strings(held)
	if body["node_slice_digest"] != n.SliceDigest || body["criteria_set_digest"] != n.CriteriaSetDigest || strings.Join(held, ",") != strings.Join(ids, ",") {
		return refuse(contract.RefusalDispositionConflict, "the plan changed between the judgement and the intent: slice %v is now %s", body["node_slice_digest"], n.SliceDigest)
	}
	findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, VerifyOptions{SkipFileBytes: true, ArtifactRoots: opts.ArtifactRoots})
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return refusalOfFinding(findings[0])
	}
	var open int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM relationships WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL", []any{n.IssueKey}, &open); err != nil {
		return err
	}
	if open > 0 {
		return refuse(contract.RefusalDuplicateAssignment, "%s already has an open relationship", n.IssueKey)
	}
	var pending int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM managed_start_requests WHERE issue_key = ? AND state IN ('reserved','create_armed')", []any{n.IssueKey}, &pending); err != nil {
		return err
	}
	if pending > 0 {
		return refuse(contract.RefusalDuplicateAssignment, "%s has a managed start in flight", n.IssueKey)
	}
	return nil
}

// reserve is the capacity decision of a release. The DAG owns the standing-cap clamp, which capacity.Reserve does not know, so a reading with no free slot is recorded here as the
// contest it is and refused; otherwise Reserve is the first write, and what it refuses (the actor is not the project's registered parent, another parent holds the subject, a
// ceiling at initiative or store scope) is returned after the conflict row it recorded commits with the transaction. A non-nil result is a refusal to return, not a failure.
func (s *Scheduler) reserve(ctx context.Context, q store.Querier, project, plan, node, actor, digest string) error {
	subject := SlotSubjectKey(plan, node)
	c, err := s.capacity(ctx, q, project)
	if err != nil {
		return err
	}
	if c.Free <= 0 {
		refusal := registry.CoordinationRefusal{Reason: contract.RefusalCapacityExhausted, Domain: "execution", Subject: subject, Incumbent: project, Challenger: actor,
			Detail: fmt.Sprintf("project %s holds %d of its %d slots (%s); %s waits for one", project, c.Held, c.Ceiling, c.Source, subject)}
		if c.Unmeasured {
			refusal.Reason = contract.RefusalCapacityUnmeasured
			refusal.Detail = "an enforced ceiling on a dimension nobody measured leaves no basis to say a slot is free for " + subject
		}
		if err := (&registry.Registry{Store: s.Store}).RecordCoordinationConflict(ctx, refusal, s.now()); err != nil {
			return err
		}
		return refusal.Error()
	}
	_, err = (&capacity.Capacity{Store: s.Store, Now: s.now}).Reserve(ctx, capacity.Reservation{SubjectKind: SlotSubjectKind, SubjectKey: subject, ParentTask: actor, Project: project, ReservedBy: actor,
		Detail: sql.NullString{String: digest, Valid: true}})
	return err
}

// asRefusal separates a refusal (an answer to the caller: the relay's own reason, with whatever conflict row it recorded) from a failure of the store or of anything else, which must
// roll the transaction back.
func asRefusal(err error) (refused, failure error) {
	var r *store.RefusedError
	if errors.As(err, &r) {
		return err, nil
	}
	return nil, err
}

// inputAcceptance is the acceptance the manifest's input for an edge rests on.
func inputAcceptance(body map[string]any, edge string) string {
	inputs, _ := body["inputs"].([]any)
	for _, item := range inputs {
		if in, _ := item.(map[string]any); in["edge_id"] == edge {
			return textOf(in["acceptance_id"])
		}
	}
	return ""
}

// restsOn is whether any input of the manifest rests on the acceptance.
func restsOn(body map[string]any, acceptance string) bool {
	inputs, _ := body["inputs"].([]any)
	for _, item := range inputs {
		if in, _ := item.(map[string]any); in["acceptance_id"] == acceptance {
			return true
		}
	}
	return false
}

// assemble builds the managed-start request of a release (contract 2.6): the id is derived from the node and the manifest, and the prompt carries the manifest (inline, or the
// path and digest of its frozen copy when it is large) and the instruction to verify every input before consuming it. The bytes are what the intent freezes.
func (s *Scheduler) assemble(plan string, n dag.SnapNode, project, actor string, req ReleaseRequest, base *BaseRef, body map[string]any, digest string) ([]byte, error) {
	manifest := dag.Canonical(body)
	where := "inline below"
	text := manifest
	if len(manifest) > maxInlineManifest {
		path, err := FreezeManifest(req.ArtifactRoots[0], []byte(manifest))
		if err != nil {
			return nil, err
		}
		where, text = "stored at "+path+" (sha256 "+shaOf([]byte(manifest))+")", "(see the stored copy)"
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Assignment %s: node %s of DAG plan %s, released under input manifest %s (%s).\n\n", n.IssueKey, n.NodeID, plan, digest, where)
	prompt.WriteString(strings.TrimSpace(req.Instructions))
	prompt.WriteString("\n\nInput manifest:\n")
	prompt.WriteString(text)
	prompt.WriteString("\n\nBefore you consume any input, verify every uri, sha256 and byte count listed in the manifest against the files on disk. If any input is missing, differs from the manifest or lies outside its scope, stop and report blocked_needs_input naming it. Do not continue on a mismatch.\n")
	if prompt.Len() > 90000 {
		return nil, refuse(contract.RefusalMalformedReceipt, "the assignment prompt is %d characters; a request carries at most 90000", prompt.Len())
	}
	recipients := append([]string(nil), req.AllowedRecipients...)
	if !contains(recipients, actor) {
		recipients = append(recipients, actor)
	}
	criteria := make([]any, len(req.Criteria))
	for i, c := range req.Criteria {
		criteria[i] = map[string]any{"id": strings.TrimSpace(c.ID), "title": strings.TrimSpace(c.Title), "required": c.Required}
	}
	baseline := "dag-input-manifest:" + digest
	if base != nil {
		baseline = base.SHA
	}
	roots := make([]any, len(req.ArtifactRoots))
	for i, r := range req.ArtifactRoots {
		roots[i] = r
	}
	allowed := make([]any, len(recipients))
	for i, r := range recipients {
		allowed[i] = r
	}
	request := map[string]any{
		"schema": managed.Schema, "requestId": ReleaseRequestID(plan, n.NodeID, digest), "issueKey": n.IssueKey, "projectKey": project,
		"parent":        map[string]any{"taskId": actor, "hostId": req.Parent.HostID, "settings": req.Parent.Settings},
		"child":         map[string]any{"hostId": req.Child.HostID, "title": req.Child.Title, "settings": req.Child.Settings},
		"artifactRoots": roots, "allowedRecipients": allowed, "criteria": criteria, "criteriaSource": req.CriteriaSource,
		"baselineRevision": baseline, "scopeRef": req.ScopeRef, "prompt": prompt.String(),
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := managed.ParseRequest(raw); err != nil {
		return nil, refuse(contract.RefusalMalformedReceipt, "the managed-start request cannot be admitted: %v", err)
	}
	return raw, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// replay continues a release whose intent was recorded: bound already (nothing to do), or not yet (send the frozen request again). The request is never rebuilt: the managed engine
// fingerprints the whole request and a tip or a clock that moved would read as a second release.
func (s *Scheduler) replay(ctx context.Context, plan, node, actor string, row releaseRow) (ReleaseResult, error) {
	out := ReleaseResult{PlanID: plan, NodeID: node, ManifestDigest: row.Digest, RequestID: row.Request, Replayed: true}
	q := s.Store.Q(ctx)
	slot, err := s.slotOf(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	out.SlotID = slot
	if rid, generation, child, bound, err := boundChild(ctx, q, plan, node, row.Request); err != nil {
		return out, err
	} else if bound {
		out.Bound, out.RelationshipID, out.Generation, out.ChildTaskID, out.State = true, rid, generation, child, "admitted"
		return out, nil
	}
	// Before anything is started, one transaction decides what may continue: a managed start that was released before it created anything is a tombstone (the engine refuses its request
	// id forever, so nothing is reserved or started for it); a slot held under the subject by another parent or project is not this release's (capacity.Reserve would refuse it, and so do
	// we); and a slot that was returned is reserved again through the very function a new release uses, under the same ceilings. Reading the tombstone and reserving in separate
	// transactions would let a release commit in between and leave a slot held for a request that can never start.
	var project string
	if _, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return out, err
	}
	if s.testBeforeReplayTx != nil {
		s.testBeforeReplayTx()
	}
	var refused error
	if err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		var managedState string
		if _, err := queryOne(txCtx, tx, "SELECT state FROM managed_start_requests WHERE request_id = ?", []any{row.Request}, &managedState); err != nil {
			return err
		}
		if managedState == "released" {
			refused = refuse(contract.RefusalDispositionConflict, "%s: the managed start %s of the release of %s was released before it created a child; a plan revision that changes the slice is the way on", BlockedReleaseAbandoned, row.Request, node)
			return nil
		}
		var holder, heldProject string
		held, err := queryOne(txCtx, tx, "SELECT parent_task_id, project_key FROM execution_slots WHERE subject_kind = ? AND subject_key = ? AND state = 'held'", []any{SlotSubjectKind, SlotSubjectKey(plan, node)}, &holder, &heldProject)
		if err != nil {
			return err
		}
		switch {
		case held && (holder != actor || heldProject != project):
			refused = refuse(contract.RefusalDispositionConflict, "the slot of %s is held by %s for project %s, not by %s for project %s, so this release cannot continue under it", node, holder, heldProject, actor, project)
		case !held:
			r, err := asRefusal(s.reserve(txCtx, tx, project, plan, node, actor, row.Digest))
			if err != nil {
				return err
			}
			refused = r
		}
		return nil
	}); err != nil {
		return out, err
	}
	if refused != nil {
		return out, refused
	}
	if out.SlotID, err = s.slotOf(ctx, q, plan, node); err != nil {
		return out, err
	}
	var raw, sum, marker, socket, selector string
	found, err := queryOne(ctx, q, "SELECT request_json, request_sha256, marker_root, socket, state_selector FROM dag_release_requests WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?",
		[]any{plan, node, row.Digest}, &raw, &sum, &marker, &socket, &selector)
	if err != nil {
		return out, err
	}
	if !found || shaOf([]byte(raw)) != sum {
		return out, refuse(contract.RefusalRevisionMismatch, "the request frozen with the release of %s is missing or no longer digests to %s", node, sum)
	}
	if _, stored, err := dag.ReadManifestOn(ctx, q, row.Digest); err != nil {
		var corrupt *dag.CorruptError
		if errors.As(err, &corrupt) {
			return out, refuse(contract.RefusalRevisionMismatch, "%s", corrupt.Detail)
		}
		return out, err
	} else if !stored {
		return out, refuse(contract.RefusalRevisionMismatch, "the manifest %s of the release of %s is not stored", row.Digest, node)
	}
	if marker != s.Selectors.MarkerRoot || socket != s.Selectors.Socket || selector != s.Selectors.StateSelector {
		return out, refuse(contract.RefusalDispositionConflict, "the release of %s was frozen under marker root %q, socket %q and state %q and is replayed under other spellings", node, marker, socket, selector)
	}
	return s.startAndBind(ctx, plan, node, actor, out, []byte(raw), true)
}

// startAndBind runs the managed start with the frozen bytes and binds the child it admitted to the node.
func (s *Scheduler) startAndBind(ctx context.Context, plan, node, actor string, out ReleaseResult, raw []byte, replayed bool) (ReleaseResult, error) {
	out.Replayed = replayed
	if s.Start == nil {
		return out, errors.New("this scheduler has no managed-start engine")
	}
	answer, err := s.Start(ctx, raw)
	if errors.Is(err, managed.ErrBusy) {
		// another caller is advancing this very request (a second wake): wait for it to bind the child, and never start a second one.
		if bound, ok, werr := s.awaitBound(ctx, plan, node, out); werr != nil || ok {
			return bound, werr
		}
	}
	if err != nil {
		return out, err
	}
	out.State, out.Stage, out.Reason, out.Managed = answer.State, answer.Stage, answer.Reason, answer.Answer
	if slot, err := s.slotOf(ctx, s.Store.Q(ctx), plan, node); err == nil {
		out.SlotID = slot
	}
	if answer.State != "admitted" {
		return out, nil
	}
	if s.testAfterStart != nil {
		if err := s.testAfterStart(); err != nil {
			return out, err
		}
	}
	rid, child := orderedString(answer.Answer, "relationshipId"), orderedString(answer.Answer, "childTaskId")
	var generation int64
	for _, f := range answer.Answer {
		if f.Key == "executionGeneration" {
			generation = toInt64(f.Value)
		}
	}
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		if err := s.fence(txCtx, q, plan, actor); err != nil {
			return err
		}
		var issue, parent, kid string
		var gen int64
		found, err := queryOne(txCtx, q, "SELECT issue_key, parent_task_id, child_task_id, execution_generation FROM relationships WHERE relationship_id = ?", []any{rid}, &issue, &parent, &kid, &gen)
		if err != nil {
			return err
		}
		var nodeIssue string
		if _, err := queryOne(txCtx, q, "SELECT issue_key FROM dag_nodes WHERE plan_id = ? AND node_id = ? AND retired_rev IS NULL", []any{plan, node}, &nodeIssue); err != nil {
			return err
		}
		if !found || issue != nodeIssue || parent != actor || kid != child || gen != generation {
			return refuse(contract.RefusalRelationshipConflict, "the relationship the managed start admitted (%s) is not the one %s's release is for", rid, node)
		}
		var other string
		if had, err := queryOne(txCtx, q, "SELECT manifest_digest FROM dag_node_executions WHERE relationship_id = ? AND execution_generation = ?", []any{rid, generation}, &other); err != nil {
			return err
		} else if had && other != out.ManifestDigest {
			return refuse(contract.RefusalRelationshipConflict, "relationship %s generation %d is already bound to manifest %s", rid, generation, other)
		}
		_, err = q.ExecContext(txCtx, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,'initial',?)"+
			" ON CONFLICT (relationship_id, execution_generation) DO NOTHING", plan, node, rid, generation, out.ManifestDigest, out.RequestID)
		return err
	})
	if err != nil {
		return out, err
	}
	out.Bound, out.RelationshipID, out.ChildTaskID, out.Generation = true, rid, child, generation
	return out, nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// Object is the result as the relay prints it (dag-release).
func (r ReleaseResult) Object() contract.OrderedObject {
	managedAnswer := any(nil)
	if r.Managed != nil {
		managedAnswer = r.Managed
	}
	return contract.OrderedObject{
		{Key: "ok", Value: r.Bound}, {Key: "schema", Value: "dag-release/1"}, {Key: "plan_id", Value: r.PlanID}, {Key: "node_id", Value: r.NodeID},
		{Key: "manifest_digest", Value: optionalText(r.ManifestDigest)}, {Key: "request_id", Value: optionalText(r.RequestID)}, {Key: "slot_id", Value: optionalText(r.SlotID)},
		{Key: "replayed", Value: r.Replayed}, {Key: "bound", Value: r.Bound}, {Key: "state", Value: optionalText(r.State)}, {Key: "stage", Value: optionalText(r.Stage)},
		{Key: "reason", Value: optionalText(r.Reason)}, {Key: "relationship_id", Value: optionalText(r.RelationshipID)}, {Key: "execution_generation", Value: r.Generation},
		{Key: "child_task_id", Value: optionalText(r.ChildTaskID)}, {Key: "managed", Value: managedAnswer},
	}
}

// awaitBound waits a bounded time for the caller that holds the managed request to bind its child, then reports that child. ok is false when it did not bind in time.
func (s *Scheduler) awaitBound(ctx context.Context, plan, node string, out ReleaseResult) (ReleaseResult, bool, error) {
	for attempt := 0; attempt < 200; attempt++ {
		rid, generation, child, bound, err := boundChild(ctx, s.Store.Q(ctx), plan, node, out.RequestID)
		if err != nil {
			return out, false, err
		}
		if bound {
			out.Bound, out.RelationshipID, out.Generation, out.ChildTaskID, out.State, out.Replayed = true, rid, generation, child, "admitted", true
			return out, true, nil
		}
		select {
		case <-ctx.Done():
			return out, false, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return out, false, nil
}
