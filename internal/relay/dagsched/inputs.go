package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// BlockedFinding is one violated path of contract 4.4: the B-code, the closed reason a reading gives and what was found.
type BlockedFinding struct{ Code, Reason, Detail string }

// consumed is one artifact an input edge hands a node, as the predecessor's receipt declared it.
type consumed struct {
	Path, SHA256 string
	Bytes        *int64
}

// inputVerdict is what the input checks of a node found: the first violated path, if any, and the hashes read from disk (folded into the reading's
// input digest, so a changed byte changes the digest).
type inputVerdict struct {
	Finding *BlockedFinding
	Hashes  []string
}

// checkInputs verifies what a node whose incoming edges are all satisfied would consume (contract 4.4 B-03, B-04, B-05, B-14, B-17): every file an
// accepted predecessor declared is still there, still hashes to what the receipt said and still lies under the predecessor's artifact roots, and the
// acceptances behind the node's inputs agree about every node they share (the Frankenbuild closure). edges are the node's incoming edges in id order and
// statuses their (satisfied) statuses. skipBytes leaves the files unread (the store half only).
func (s *Scheduler) checkInputs(ctx context.Context, q store.Querier, plan string, edges []dag.SnapEdge, statuses map[string]EdgeStatus, skipBytes bool) (inputVerdict, error) {
	var verdict inputVerdict
	for _, e := range edges {
		if e.Kind != dag.EdgeArtifactVerified || e.PinsCodeHead {
			continue
		}
		finding, hashes, err := s.checkArtifacts(ctx, q, statuses[e.EdgeID].AcceptanceID, skipBytes)
		if err != nil {
			return inputVerdict{}, err
		}
		verdict.Hashes = append(verdict.Hashes, hashes...)
		if finding != nil {
			verdict.Finding = finding
			return verdict, nil
		}
	}
	finding, err := s.closure(ctx, q, edges, statuses)
	if err != nil {
		return inputVerdict{}, err
	}
	verdict.Finding = finding
	sort.Strings(verdict.Hashes)
	return verdict, nil
}

// acceptedEntries are the artifacts the accepted event's receipt declared and the roots of the predecessor's relationship, with a finding when the list cannot be used: an absent,
// empty or malformed list, or roots that do not read, is not an input a successor can consume.
func (s *Scheduler) acceptedEntries(ctx context.Context, q store.Querier, a Acceptance) (entries []consumed, roots []string, ref string, finding *BlockedFinding, err error) {
	var receipt, rootsJSON string
	found, err := queryOne(ctx, q, "SELECT e.receipt, COALESCE(e.manifest_ref, ''), r.artifact_roots FROM events e JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.event_id = ?",
		[]any{a.EventID}, &receipt, &ref, &rootsJSON)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if !found {
		return nil, nil, "", &BlockedFinding{"B-17", BlockedInputMissing, "the accepted event " + a.EventID + " is not in the store"}, nil
	}
	entries, ok := receiptEntries(receipt)
	if !ok || len(entries) == 0 {
		return nil, nil, "", &BlockedFinding{"B-17", BlockedInputMissing, "the receipt of the accepted event declares no readable list of artifacts"}, nil
	}
	if json.Unmarshal([]byte(rootsJSON), &roots) != nil {
		return nil, nil, "", &BlockedFinding{"B-05", BlockedInputOutOfScope, "the artifact roots of the predecessor's relationship are unreadable"}, nil
	}
	return entries, roots, ref, nil, nil
}

// checkRevision is the omission check: the artifact list a receipt declares must hash to the revision the parent accepted (MANIFEST-CANON-01, the digest receipt intake bound the
// event to). A list from which an artifact was removed, or to which one was added, or whose path or digest was altered, no longer does, whatever the files that remain still hash to.
func checkRevision(entries []consumed, a Acceptance) *BlockedFinding {
	declared := make([]store.ManifestEntry, len(entries))
	for i, e := range entries {
		declared[i] = store.ManifestEntry{Path: e.Path, SHA256: e.SHA256, Bytes: e.Bytes}
	}
	revision, err := store.ManifestRevision(declared)
	if err != nil {
		return &BlockedFinding{"B-17", BlockedInputMissing, "the artifact list of the accepted receipt cannot be digested: " + err.Error()}
	}
	if revision != a.RevisionHash {
		return &BlockedFinding{"B-17", BlockedInputMissing, "the artifact list of the accepted receipt digests to " + revision + " and the parent accepted revision " + a.RevisionHash + ": an artifact was left out, added or changed"}
	}
	return nil
}

// checkArtifacts re-reads the files of one accepted predecessor the way receipt intake read them: by the roots of the predecessor's relationship, within them, hashed again, and
// the list itself checked against the accepted revision. An absent, empty or unreadable list is B-17: an artifact edge that hands over nothing is not a success. skipBytes leaves
// the files and the frozen copy unread (the store half) and keeps the list check.
func (s *Scheduler) checkArtifacts(ctx context.Context, q store.Querier, acceptanceID string, skipBytes bool) (*BlockedFinding, []string, error) {
	a, err := loadAcceptanceByID(ctx, q, acceptanceID)
	if err != nil {
		return nil, nil, err
	}
	entries, roots, ref, finding, err := s.acceptedEntries(ctx, q, a)
	if err != nil || finding != nil {
		return finding, nil, err
	}
	if skipBytes {
		return checkRevision(entries, a), nil, nil
	}
	if ref != "" {
		// the frozen copy is read, parsed and compared with the declared entries the way receipt intake reads it; a directory, a document that is not a
		// manifest or one that disagrees with the receipt is not a copy the successor can rely on.
		declared := make([]store.ManifestEntry, len(entries))
		for i, e := range entries {
			declared[i] = store.ManifestEntry{Path: e.Path, SHA256: e.SHA256, Bytes: e.Bytes}
		}
		if finding, err := frozenFinding(ctx, ref, declared); err != nil || finding != nil {
			return finding, nil, err
		}
	}
	var hashes []string
	for _, entry := range entries {
		finding, digest, err := hashEntry(ctx, entry, roots)
		if err != nil {
			return nil, nil, err
		}
		if digest != "" {
			hashes = append(hashes, entry.Path+":"+digest)
		}
		if finding != nil {
			return finding, hashes, nil
		}
	}
	return checkRevision(entries, a), hashes, nil
}

// frozenFinding reads the frozen copy of a manifest and answers B-17 when it is not one the successor can rely on. A context that ended
// during the read ended the blob reads it reports as problems, so that is the stop and no evidence about the copy: it answers the context's error,
// as hashEntry does for the files.
func frozenFinding(ctx context.Context, ref string, declared []store.ManifestEntry) (*BlockedFinding, error) {
	_, problems, _, err := store.VerifyFrozenDetailed(ctx, ref, declared)
	if stopped := ctx.Err(); stopped != nil {
		return nil, stopped
	}
	switch {
	case err != nil:
		return &BlockedFinding{"B-17", BlockedInputMissing, "the frozen copy of the manifest, " + ref + ", cannot be read: " + err.Error()}, nil
	case len(problems) > 0:
		return &BlockedFinding{"B-17", BlockedInputMissing, "the frozen copy of the manifest, " + ref + ", does not hold what the receipt declared: " + problems[0]}, nil
	}
	return nil, nil
}

// hashEntry reads one declared artifact again: through the roots, hashed, compared with what was declared. A finding names the violated path of contract 4.4; digest
// is what was read when anything was.
func hashEntry(ctx context.Context, entry consumed, roots []string) (*BlockedFinding, string, error) {
	digest, size, _, err := store.HashArtifact(ctx, entry.Path, roots, false)
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			switch refused.Reason {
			case store.ReasonScopeEscape, store.ReasonPathRelocated, store.ReasonSymlinkComponent:
				return &BlockedFinding{"B-05", BlockedInputOutOfScope, entry.Path + ": " + err.Error()}, "", nil
			case store.ReasonArtifactMutated, store.ReasonArtifactLeaseBroken:
				// the bytes changed under the read: they are not the bytes the receipt declared.
				return &BlockedFinding{"B-04", BlockedInputHashMismatch, entry.Path + ": " + err.Error()}, "", nil
			}
		}
		if ctx.Err() != nil {
			return nil, "", err
		}
		return &BlockedFinding{"B-03", BlockedInputMissing, entry.Path + ": " + err.Error()}, "", nil
	}
	if digest != entry.SHA256 {
		return &BlockedFinding{"B-04", BlockedInputHashMismatch, entry.Path + ": the bytes hash to " + digest + " and the receipt declared " + entry.SHA256}, digest, nil
	}
	if entry.Bytes != nil && *entry.Bytes != size {
		return &BlockedFinding{"B-04", BlockedInputHashMismatch, entry.Path + ": the file is " + itoa64(size) + " bytes and the receipt declared " + itoa64(*entry.Bytes)}, digest, nil
	}
	return nil, digest, nil
}

// receiptEntries reads the manifest of a stored receipt: the list of {path, sha256, bytes}. ok is false for a receipt that is not an object, has no
// list, or holds an entry without a path or a digest.
func receiptEntries(receipt string) ([]consumed, bool) {
	var wire struct {
		Manifest *[]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Bytes  *int64 `json:"bytes"`
		} `json:"manifest"`
	}
	if json.Unmarshal([]byte(receipt), &wire) != nil || wire.Manifest == nil {
		return nil, false
	}
	out := make([]consumed, 0, len(*wire.Manifest))
	for _, m := range *wire.Manifest {
		if m.Path == "" || m.SHA256 == "" {
			return nil, false
		}
		out = append(out, consumed{Path: m.Path, SHA256: m.SHA256, Bytes: m.Bytes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, true
}

// closure is B-14: following every consumed acceptance down through the manifests it was accepted on, one node must never appear under two acceptance ids,
// or a node would be built from two versions of the same predecessor (E-25).
func (s *Scheduler) closure(ctx context.Context, q store.Querier, edges []dag.SnapEdge, statuses map[string]EdgeStatus) (*BlockedFinding, error) {
	seen := map[string]string{} // acceptance id -> node
	byNode := map[string]string{}
	var queue []string
	for _, e := range edges {
		if id := statuses[e.EdgeID].AcceptanceID; id != "" {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, done := seen[id]; done {
			continue
		}
		a, err := loadAcceptanceByID(ctx, q, id)
		if errors.Is(err, errNoAcceptance) {
			return &BlockedFinding{"B-06", BlockedInputUnaccepted, "acceptance " + id + ", consumed down the chain, is not in the store"}, nil
		} else if err != nil {
			return nil, err
		}
		seen[id] = a.NodeID
		if other, clash := byNode[a.NodeID]; clash && other != id {
			return &BlockedFinding{"B-14", BlockedInconsistentInputs, "node " + a.NodeID + " is consumed under two acceptances, " + other + " and " + id}, nil
		}
		byNode[a.NodeID] = id
		body, found, err := dag.ReadManifestOn(ctx, q, a.ManifestDigest)
		var corrupt *dag.CorruptError
		switch {
		case errors.As(err, &corrupt):
			return &BlockedFinding{"B-02", BlockedManifestTampered, corrupt.Detail}, nil
		case err != nil:
			return nil, err
		case !found:
			return &BlockedFinding{"B-08", BlockedAcceptanceIncomplete, "the manifest acceptance " + id + " consumed is not stored"}, nil
		}
		inputs, _ := body["inputs"].([]any)
		for _, item := range inputs {
			if in, ok := item.(map[string]any); ok {
				if next, _ := in["acceptance_id"].(string); next != "" {
					queue = append(queue, next)
				}
			}
		}
	}
	return nil, nil
}
