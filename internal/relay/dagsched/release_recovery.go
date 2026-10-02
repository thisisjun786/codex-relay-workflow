package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The recovery of a release whose managed start was released before it created a child (CRW-282), and the cleanup of the frozen manifest copies a release leaves behind.
//
// A managed start that was released is a tombstone: the engine refuses its request id forever, the id is derived from (plan, node, manifest digest) only, and dag_releases is unique on that triple.
// So a node whose start was abandoned cannot be released again under the same id. dag-release-close ends the abandoned intent explicitly (one closed row of dag_release_recoveries, the slot
// returned in the same transaction); the ordinary release then judges the node again from scratch. A manifest that changed is an ordinary new release; an unchanged one is recorded as a
// rereleased row that carries its own successor request id and frozen request bytes, because the shipped release rows cannot take a second request for one digest.

// Journal kinds of the frozen-copy cleanup: what was removed and what was kept, each with the reason.
const (
	JournalCopyRemoved = "dag_manifest_copy_removed"
	JournalCopyKept    = "dag_manifest_copy_kept"
)

// SlotReasonReleaseClosed is the reason a closed intent's slot is returned with.
const SlotReasonReleaseClosed = "dag_release_closed"

// maxCopyBytes bounds what the cleanup reads of a file it is about to remove.
const maxCopyBytes = 16 << 20

// Test seams of the cleanup (nil in production): between the verification of a copy and its removal (an error stands for a removal that fails), and after the removal before the transaction commits.
var testBeforeUnlink, testAfterUnlink func() error

// RecoveryRequestID is the managed request id of the release that follows a closed intent of the same manifest: a pure function of the closed request, so a replay is the same request (no
// attempt counter). It has the shape of ReleaseRequestID and stays within the engine's 128 characters.
func RecoveryRequestID(planID, nodeID, manifestDigest, closedRequestID string) string {
	h := sha256.Sum256([]byte(dag.Canonical([]any{"recover", planID, nodeID, manifestDigest, closedRequestID})))
	return "dag-" + hex.EncodeToString(h[:])[:40]
}

// latestRelease is the node's open intent: the release (a dag_releases row, or a rereleased row of dag_release_recoveries) whose managed request has no closed row. There is at most one: a release
// is refused while another is open (the replay-first read and the check inside the intent transaction). None means no release owns the node. A store the recovery table has not reached yet (a read-only
// open installs nothing, and the zone arrives with the first write open) has no recoveries: the open intent is then the newest dag_releases row, as it was.
func latestRelease(ctx context.Context, q store.Querier, plan, node string) (releaseRow, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT digest, request, recovered FROM ("+
		"SELECT d.manifest_digest AS digest, d.managed_request_id AS request, 0 AS recovered, d.decided_at AS at FROM dag_releases d WHERE d.plan_id = ? AND d.node_id = ?"+
		" UNION ALL "+
		"SELECT x.manifest_digest, x.successor_request_id, 1, x.recorded_at FROM dag_release_recoveries x WHERE x.plan_id = ? AND x.node_id = ? AND x.action = 'rereleased'"+
		") i WHERE NOT EXISTS (SELECT 1 FROM dag_release_recoveries c WHERE c.plan_id = ? AND c.node_id = ? AND c.manifest_digest = i.digest AND c.abandoned_request_id = i.request AND c.action = 'closed')"+
		" ORDER BY at DESC, request DESC LIMIT 1", plan, node, plan, node, plan, node)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: dag_release_recoveries") {
			var r releaseRow
			found, err := queryOne(ctx, q, "SELECT manifest_digest, managed_request_id FROM dag_releases WHERE plan_id = ? AND node_id = ? ORDER BY decided_at DESC, manifest_digest DESC LIMIT 1", []any{plan, node}, &r.Digest, &r.Request)
			return r, found, err
		}
		return releaseRow{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return releaseRow{}, false, rows.Err()
	}
	var r releaseRow
	var recovered int
	if err := rows.Scan(&r.Digest, &r.Request, &recovered); err != nil {
		return releaseRow{}, false, err
	}
	r.Recovered = recovered == 1
	return r, true, rows.Err()
}

// closedChainEnd is the closed request of a manifest digest of the node that no later release followed: the base of the successor id the next release of that digest takes.
func closedChainEnd(ctx context.Context, q store.Querier, plan, node, digest string) (string, bool, error) {
	var request string
	found, err := queryOne(ctx, q, "SELECT c.abandoned_request_id FROM dag_release_recoveries c WHERE c.plan_id = ? AND c.node_id = ? AND c.manifest_digest = ? AND c.action = 'closed'"+
		" AND NOT EXISTS (SELECT 1 FROM dag_release_recoveries r WHERE r.plan_id = c.plan_id AND r.node_id = c.node_id AND r.manifest_digest = c.manifest_digest AND r.abandoned_request_id = c.abandoned_request_id AND r.action = 'rereleased')"+
		" ORDER BY c.recorded_at DESC, c.abandoned_request_id DESC LIMIT 1", []any{plan, node, digest}, &request)
	return request, found, err
}

// frozenRequest is the request bytes an intent was frozen with and the selectors they were fingerprinted under.
type frozenRequest struct{ Raw, SHA, Marker, Socket, Selector string }

// frozenRequestOf reads the frozen request of an open intent from where it was recorded: dag_release_requests for a release, the rereleased row for a successor.
func frozenRequestOf(ctx context.Context, q store.Querier, plan, node string, row releaseRow) (frozenRequest, bool, error) {
	var f frozenRequest
	var found bool
	var err error
	if row.Recovered {
		found, err = queryOne(ctx, q, "SELECT request_json, request_sha256, marker_root, socket, state_selector FROM dag_release_recoveries WHERE plan_id = ? AND node_id = ? AND manifest_digest = ? AND successor_request_id = ? AND action = 'rereleased'",
			[]any{plan, node, row.Digest, row.Request}, &f.Raw, &f.SHA, &f.Marker, &f.Socket, &f.Selector)
	} else {
		found, err = queryOne(ctx, q, "SELECT request_json, request_sha256, marker_root, socket, state_selector FROM dag_release_requests WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?",
			[]any{plan, node, row.Digest}, &f.Raw, &f.SHA, &f.Marker, &f.Socket, &f.Selector)
	}
	return f, found, err
}

// frozenCopy is the manifest copy one Release call froze: where it is, what it holds and whether this call created the file (a file that already existed was reused, and is not the call's to remove).
type frozenCopy struct {
	Root, Path, SHA string
	Canonical       []byte
	Created         bool
}

// discardFrozenCopy removes the copy a Release call froze when that call recorded no intent. It touches only a file the call created, and removes it only when nothing live names it: see removeCopy.
// cause is the reason the call ended without an intent (the refusal's reason).
func (s *Scheduler) discardFrozenCopy(ctx context.Context, c *frozenCopy, plan, node, cause string) (bool, error) {
	if c == nil || !c.Created {
		return false, nil
	}
	var out CopyOutcome
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.removeCopy(txCtx, s.Store.Q(txCtx), c.Root, c.Path, c.SHA, "", plan, node, cause, "", false)
		return err
	})
	return out.Removed, err
}

// discardAfterRelease is the deferred end of a Release call that froze a copy and recorded no intent. It never replaces the call's own error: a cleanup that cannot run leaves the file.
func (s *Scheduler) discardAfterRelease(ctx context.Context, c *frozenCopy, plan, node string, cause error) {
	why := "concurrent"
	if cause != nil {
		var refused *store.RefusedError
		if errors.As(cause, &refused) {
			why = refused.Reason
		} else {
			why = "error"
		}
	}
	_, _ = s.discardFrozenCopy(context.WithoutCancel(ctx), c, plan, node, why)
}

// CopyOutcome is what became of the frozen copy of an intent: the path its frozen request names and whether the file is gone.
type CopyOutcome struct {
	Path    string
	Removed bool
}

// removeCopy removes one frozen manifest copy under the rules of the cleanup, inside the caller's write transaction (so no intent can commit between the check and the unlink), and journals what it did.
// The file is reached through store.UnlinkPinned: every component of the root and of the directory is opened by descriptor with O_NOFOLLOW, the file is opened relative to the directory's descriptor and unlinked
// there, so a link planted in any component, or a component swapped meanwhile, cannot lead the removal elsewhere, and a FIFO cannot hold the transaction. It must hash to its name. It is kept when a live intent's
// frozen request names it, or when a stored manifest holds exactly these bytes (a bound release, an accepted node, a prepared or recorded correction). ownRequest is an intent that does not count (the one
// being closed); at close the stored manifest counts only when it is bound to an execution, accepted, or an open relationship owns the issue, because the abandoned intent's own stored body is what the copy holds.
// The unlink is the last act before the journal row: a failure after it loses that row (of a file nothing referenced), never a referenced file. decide runs on the transaction's own querier.
func (s *Scheduler) removeCopy(ctx context.Context, q store.Querier, root, path, sha, issue, plan, node, cause, ownRequest string, atClose bool) (CopyOutcome, error) {
	out := CopyOutcome{Path: path}
	size := 0
	keep := func(why string) (CopyOutcome, error) {
		return out, s.journalCopy(ctx, q, JournalCopyKept, plan, node, path, sha, size, why, cause)
	}
	name := sha + ".json"
	if filepath.Clean(path) != filepath.Join(filepath.Clean(root), frozenManifestDir, name) {
		return keep("name_differs")
	}
	keptWhy := ""
	var decideErr error
	removal, err := store.UnlinkPinned(filepath.Clean(root), frozenManifestDir, name, maxCopyBytes, func(raw []byte) (bool, error) {
		size = len(raw)
		if shaOf(raw) != sha {
			keptWhy = "bytes_differ"
			return false, nil
		}
		why, err := s.copyReliedOn(ctx, q, sha, raw, issue, ownRequest, atClose)
		if err != nil {
			decideErr = err
			return false, err
		}
		if why != "" {
			keptWhy = why
			return false, nil
		}
		if testBeforeUnlink != nil {
			if err := testBeforeUnlink(); err != nil {
				keptWhy = "remove_failed"
				return false, nil
			}
		}
		return true, nil
	})
	if decideErr != nil {
		return out, decideErr
	}
	var refused *store.RefusedError
	switch {
	case errors.Is(err, store.ErrPinnedTooLarge):
		return keep("too_large")
	case errors.Is(err, store.ErrPinnedChanged):
		return keep("changed")
	case errors.As(err, &refused):
		switch refused.Reason {
		case store.ReasonSymlinkComponent:
			return keep("link_in_path")
		case store.ReasonUnverifiablePathBinding:
			return keep("unsupported_platform")
		}
		return keep("not_a_copy")
	case err != nil:
		return keep("remove_failed")
	}
	switch removal {
	case store.PinnedAbsent:
		out.Removed = true
		return out, nil
	case store.PinnedKept:
		return keep(keptWhy)
	}
	out.Removed = true
	if testAfterUnlink != nil {
		if err := testAfterUnlink(); err != nil {
			return out, err
		}
	}
	return out, s.journalCopy(ctx, q, JournalCopyRemoved, plan, node, path, sha, size, cause, cause)
}

// copyReliedOn says why a copy must stay ("" when nothing relies on it).
func (s *Scheduler) copyReliedOn(ctx context.Context, q store.Querier, sha string, raw []byte, issue, ownRequest string, atClose bool) (string, error) {
	name := sha + ".json"
	var one int
	if live, err := queryOne(ctx, q, "SELECT 1 FROM dag_release_requests r JOIN dag_releases d ON d.plan_id = r.plan_id AND d.node_id = r.node_id AND d.manifest_digest = r.manifest_digest"+
		" WHERE instr(r.request_json, ?) > 0 AND d.managed_request_id <> ?"+
		" AND NOT EXISTS (SELECT 1 FROM dag_release_recoveries c WHERE c.plan_id = d.plan_id AND c.node_id = d.node_id AND c.manifest_digest = d.manifest_digest AND c.abandoned_request_id = d.managed_request_id AND c.action = 'closed') LIMIT 1",
		[]any{name, ownRequest}, &one); err != nil || live {
		return "relied_on", err
	}
	if live, err := queryOne(ctx, q, "SELECT 1 FROM dag_release_recoveries x WHERE x.action = 'rereleased' AND instr(x.request_json, ?) > 0 AND x.successor_request_id <> ?"+
		" AND NOT EXISTS (SELECT 1 FROM dag_release_recoveries c WHERE c.plan_id = x.plan_id AND c.node_id = x.node_id AND c.manifest_digest = x.manifest_digest AND c.abandoned_request_id = x.successor_request_id AND c.action = 'closed') LIMIT 1",
		[]any{name, ownRequest}, &one); err != nil || live {
		return "relied_on", err
	}
	var copied map[string]any
	if json.Unmarshal(raw, &copied) != nil {
		return "", nil
	}
	digest, _ := copied["manifest_digest"].(string)
	if digest == "" {
		return "", nil
	}
	body, stored, err := dag.ReadManifestOn(ctx, q, digest)
	if err != nil {
		return "reliance_unknown", nil
	}
	if !stored || shaOf([]byte(dag.Canonical(body))) != sha {
		return "", nil
	}
	if !atClose {
		return "relied_on", nil
	}
	if bound, err := queryOne(ctx, q, "SELECT 1 FROM dag_node_executions WHERE manifest_digest = ? UNION ALL SELECT 1 FROM dag_acceptances WHERE manifest_digest = ? LIMIT 1", []any{digest, digest}, &one); err != nil || bound {
		return "relied_on", err
	}
	if issue != "" {
		if owned, err := queryOne(ctx, q, "SELECT 1 FROM relationships WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL LIMIT 1", []any{issue}, &one); err != nil || owned {
			return "relied_on", err
		}
	}
	return "", nil
}

// journalCopy records what the cleanup did with a copy.
func (s *Scheduler) journalCopy(ctx context.Context, q store.Querier, kind, plan, node, path, sha string, size int, why, cause string) error {
	detail, err := json.Marshal(map[string]any{"path": path, "sha256": sha, "bytes": size, "why": why, "cause": cause})
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", s.now(), kind, plan+"/"+node, string(detail))
	return err
}

var copyNamedInPrompt = regexp.MustCompile("stored at (.+?) \\(sha256 ([0-9a-f]{64})\\)")

// frozenCopyNamedBy is the copy a frozen release request tells the child to read: its path and the first artifact root it lies under. ok is false when the prompt carries the manifest inline or names
// a file that is not <root>/dag-input-manifests/<sha256>.json.
func frozenCopyNamedBy(raw string) (path string, ok bool) {
	var request struct {
		Prompt        string   `json:"prompt"`
		ArtifactRoots []string `json:"artifactRoots"`
	}
	if json.Unmarshal([]byte(raw), &request) != nil || len(request.ArtifactRoots) == 0 {
		return "", false
	}
	m := copyNamedInPrompt.FindStringSubmatch(request.Prompt)
	if m == nil || filepath.Clean(m[1]) != filepath.Join(filepath.Clean(request.ArtifactRoots[0]), frozenManifestDir, m[2]+".json") {
		return "", false
	}
	return m[1], true
}

// CloseResult is what CloseRelease answers: the closed request, the slot it returned, what became of its frozen copy and, when the node was released again since, the successor request.
type CloseResult struct {
	PlanID, NodeID, ManifestDigest, RequestID, SlotID, SuccessorRequestID string
	Replayed, SlotReleased                                                bool
	Copy                                                                  *CopyOutcome
}

// CloseRelease ends an abandoned release explicitly (dag-release-close): the node's open intent is under digest and its managed start was released before it created a child. In one transaction the closed
// row is recorded and the slot is returned (reason dag_release_closed; one an operator already returned is skipped). Afterwards, in a transaction of its own and without failing the close, the intent's own frozen
// copy is removed unless something live names it. The same close again answers the recorded closure, also after the node was released again (the answer then names the successor request): request names the
// intent a close is for (it is required), so a retry of an old close never closes a newer abandoned intent of the same manifest. It creates no child and refuses anything that is not an abandoned
// intent, so a running or in-flight release is never ended by it.
func (s *Scheduler) CloseRelease(ctx context.Context, plan, node, actor, digest, reason, request string) (CloseResult, error) {
	out := CloseResult{PlanID: plan, NodeID: node, ManifestDigest: digest}
	if strings.TrimSpace(reason) == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "closing a release needs the reason it is closed")
	}
	if strings.TrimSpace(request) == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "closing a release names the managed request it closes (--request-id)")
	}
	issue := ""
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
		issue = n.IssueKey
		parents, err := projectParents(txCtx, tx, snap.ProjectKey)
		if err != nil {
			return err
		}
		if !contains(parents, actor) {
			return refuse(contract.RefusalScopeRoleMismatch, "task %s is not a registered parent of project %s: only the parent that owns the slot closes a release", actor, snap.ProjectKey)
		}
		open, found, err := latestRelease(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		managedState := ""
		if found && open.Digest == digest && request == open.Request {
			if _, err := queryOne(txCtx, tx, "SELECT state FROM managed_start_requests WHERE request_id = ?", []any{open.Request}, &managedState); err != nil {
				return err
			}
			var bound int
			if _, err := queryOne(txCtx, tx, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ?", []any{plan, node}, &bound); err != nil {
				return err
			}
			if managedState == "released" && bound == 0 {
				return s.closeIntent(txCtx, tx, snap, node, actor, reason, open, &out)
			}
		}
		replayed, err := closureOf(txCtx, tx, plan, node, digest, request)
		if err != nil {
			return err
		}
		if replayed != nil {
			out = *replayed
			out.Replayed = true
			return nil
		}
		if found && open.Digest == digest && request == open.Request {
			return refuse(contract.RefusalDispositionConflict, "the release of %s under manifest %s is not abandoned (its managed start %s is %q): only a start that was released before it created a child is closed", node, digest, request, managedState)
		}
		return refuse(contract.RefusalDispositionConflict, "request %s is neither the abandoned release of %s under manifest %s nor one that was closed", request, node, digest)
	})
	if err != nil {
		return out, err
	}
	s.settleCopy(ctx, &out, issue, plan, node)
	return out, nil
}

// closeIntent records the closure of an abandoned intent: the slot is returned and the closed row is written with what the slot did and the copy the intent's own frozen request names.
func (s *Scheduler) closeIntent(ctx context.Context, tx store.Querier, snap dag.Snapshot, node, actor, reason string, open releaseRow, out *CloseResult) error {
	plan := snap.PlanID
	var slotID, holder, heldProject string
	held, err := queryOne(ctx, tx, "SELECT slot_id, parent_task_id, project_key FROM execution_slots WHERE subject_kind = ? AND subject_key = ? AND state = 'held'", []any{SlotSubjectKind, SlotSubjectKey(plan, node)}, &slotID, &holder, &heldProject)
	if err != nil {
		return err
	}
	if held && (holder != actor || heldProject != snap.ProjectKey) {
		return refuse(contract.RefusalDispositionConflict, "the slot of %s is held by %s for project %s, not by %s for project %s, so this release cannot be closed by it", node, holder, heldProject, actor, snap.ProjectKey)
	}
	released := false
	if held {
		if released, err = s.releaseSlot(ctx, tx, plan, node, actor, SlotReasonReleaseClosed); err != nil {
			return err
		}
	} else {
		slotID = ""
	}
	var copied *CopyOutcome
	var copyPath any
	if frozen, ok, err := frozenRequestOf(ctx, tx, plan, node, open); err != nil {
		return err
	} else if ok {
		if path, named := frozenCopyNamedBy(frozen.Raw); named {
			copied, copyPath = &CopyOutcome{Path: path}, path
		}
	}
	releasedFlag := 0
	if released {
		releasedFlag = 1
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO dag_release_recoveries (plan_id, node_id, manifest_digest, abandoned_request_id, action, slot_id, slot_released, copy_path, reason, recorded_by, coordinator_epoch, recorded_at)"+
		" VALUES (?,?,?,?,'closed',?,?,?,?,?,0,?)", plan, node, open.Digest, open.Request, nilIfEmpty(slotID), releasedFlag, copyPath, strings.TrimSpace(reason), actor, s.now()); err != nil {
		return err
	}
	out.RequestID, out.SlotID, out.SlotReleased, out.Copy = open.Request, slotID, released, copied
	return nil
}

// settleCopy takes the frozen copy of a closed intent, in a transaction of its own after the closure committed. It never fails the close: a copy it cannot remove stays, and a repeat of the close tries again.
// The answer says whether the file is gone.
func (s *Scheduler) settleCopy(ctx context.Context, result *CloseResult, issue, plan, node string) {
	c := result.Copy
	if c == nil {
		return
	}
	root, sha := filepath.Dir(filepath.Dir(c.Path)), strings.TrimSuffix(filepath.Base(c.Path), ".json")
	_ = s.Store.Compose(context.WithoutCancel(ctx), func(txCtx context.Context, _ *sql.Conn) error {
		_, err := s.removeCopy(txCtx, s.Store.Q(txCtx), root, c.Path, sha, issue, plan, node, "closed", result.RequestID, true)
		return err
	})
	_, err := os.Lstat(c.Path)
	c.Removed = errors.Is(err, os.ErrNotExist)
}

// closureOf is the recorded answer of the closure of one request of a manifest digest of the node, or nil: the request that was closed, what it did, and the successor request when the node was released again.
func closureOf(ctx context.Context, q store.Querier, plan, node, digest, request string) (*CloseResult, error) {
	var slot, copyPath sql.NullString
	var slotReleased int
	var closed string
	found, err := queryOne(ctx, q, "SELECT abandoned_request_id, slot_id, slot_released, copy_path FROM dag_release_recoveries WHERE plan_id = ? AND node_id = ? AND manifest_digest = ? AND abandoned_request_id = ? AND action = 'closed'",
		[]any{plan, node, digest, request}, &closed, &slot, &slotReleased, &copyPath)
	if err != nil || !found {
		return nil, err
	}
	out := &CloseResult{PlanID: plan, NodeID: node, ManifestDigest: digest, RequestID: closed, SlotID: slot.String, SlotReleased: slotReleased == 1}
	if copyPath.Valid {
		out.Copy = &CopyOutcome{Path: copyPath.String}
	}
	var successor string
	if had, err := queryOne(ctx, q, "SELECT successor_request_id FROM dag_release_recoveries WHERE plan_id = ? AND node_id = ? AND manifest_digest = ? AND abandoned_request_id = ? AND action = 'rereleased'",
		[]any{plan, node, digest, closed}, &successor); err != nil {
		return nil, err
	} else if had {
		out.SuccessorRequestID = successor
	}
	return out, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Object is the result as the relay prints it (dag-release-close).
func (r CloseResult) Object() contract.OrderedObject {
	var copied any
	if r.Copy != nil {
		copied = contract.OrderedObject{{Key: "path", Value: r.Copy.Path}, {Key: "removed", Value: r.Copy.Removed}}
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: "dag-release-close/1"}, {Key: "plan_id", Value: r.PlanID}, {Key: "node_id", Value: r.NodeID},
		{Key: "manifest_digest", Value: r.ManifestDigest}, {Key: "request_id", Value: r.RequestID}, {Key: "closed", Value: true}, {Key: "replayed", Value: r.Replayed},
		{Key: "slot_id", Value: optionalText(r.SlotID)}, {Key: "slot_released", Value: r.SlotReleased}, {Key: "successor_request_id", Value: optionalText(r.SuccessorRequestID)},
		{Key: "frozen_copy", Value: copied},
	}
}
