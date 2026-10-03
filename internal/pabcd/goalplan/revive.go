package goalplan

import (
	"path"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// epoch is new Date(0).toISOString(), the time a missing or non-text timestamp is revived as.
const epoch = "1970-01-01T00:00:00.000Z"

// The revive functions take a decoded JSON value (an object is a map[string]any, a list a []any) and rebuild a record from the
// keys they know, exactly as goalplan.ts:333-467 does; a value of the wrong shape is dropped or defaulted by the rule of each,
// never repaired. A string reaches them already decoded, so one holding a lone surrogate escape is U+FFFD by then.

// text reads a string field; ok is false when the key is absent or not a string.
func text(m map[string]any, key string) (s string, ok bool) {
	s, ok = m[key].(string)
	return s, ok
}

// textPtr is a string field the oracle keeps even when it is empty: nil when absent or not a string.
func textPtr(m map[string]any, key string) *string {
	if s, ok := text(m, key); ok {
		return &s
	}
	return nil
}

// timestamp is a time field, defaulted to the epoch when absent or not a string; an empty string stays empty.
func timestamp(m map[string]any, key string) string {
	if s, ok := text(m, key); ok {
		return s
	}
	return epoch
}

// verdictOf is the verdict a value names, or "" when it is not one of the three.
func verdictOf(v any) Verdict {
	s, _ := v.(string)
	switch Verdict(s) {
	case VerdictPass, VerdictNearPass, VerdictFail:
		return Verdict(s)
	}
	return ""
}

// isReviewStatus is REVIEW_STATUSES (goalplan.ts:333): the statuses a stored round may hold.
func isReviewStatus(s string) bool {
	switch ReviewRoundStatus(s) {
	case ReviewPending, ReviewLaunching, ReviewInFlight, ReviewApproved, ReviewChangesRequested, ReviewInconclusive:
		return true
	}
	return false
}

// isGateStatus is GATE_STATUSES (goalplan.ts:413): the statuses a stored final gate may hold, which are not the round statuses.
func isGateStatus(s string) bool {
	switch FinalGateStatus(s) {
	case GatePending, GateInFlight, GateApproved, GateInconclusive:
		return true
	}
	return false
}

// reviveLane rebuilds a round's reviewer launch, or nil when it has no launch id: a lane that cannot be told from another would
// let a sign-off name the wrong round. Text fields are kept when they are strings (empty included), the verdict only when it
// is one of the three, the source identity only when reviveSourceIdentity accepts it; every other key is dropped.
func reviveLane(raw any) *ReviewLane {
	r, ok := raw.(map[string]any)
	id, _ := text(r, "launchId")
	if !ok || id == "" {
		return nil
	}
	return &ReviewLane{
		LaunchID: id, ReviewerSession: textPtr(r, "reviewerSession"), WorkspaceRoot: textPtr(r, "workspaceRoot"),
		ArtifactSha256: textPtr(r, "artifactSha256"), Verdict: verdictOf(r["verdict"]), SourceIdentity: reviveSourceIdentity(r["sourceIdentity"]),
	}
}

// reviveReviewRounds rebuilds the review rounds. A round missing its identity, purpose, plan hash or lane is dropped rather than
// repaired, since a half-round would let a consumer believe a review happened. It returns nil when the field is not a list, so a
// plan without rounds reads back without them, and a non-nil empty slice for a list that held no valid round, which is stored
// as [].
func reviveReviewRounds(raw any) []ReviewRoundState {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := []ReviewRoundState{}
	for _, entry := range list {
		r, ok := entry.(map[string]any)
		id, _ := text(r, "roundId")
		purpose, _ := text(r, "purpose")
		planPath, hasPath := text(r, "planPath")
		planSha, hasSha := text(r, "planSha256")
		status, _ := text(r, "status")
		lane := reviveLane(r["lane"])
		if !ok || id == "" || (purpose != string(PurposePlanAudit) && purpose != string(PurposeFinalGate)) || !hasPath || !hasSha || !isReviewStatus(status) || lane == nil {
			continue
		}
		// The binding fields (REVIEW-BINDING-01) are kept only when well-formed and non-empty: the A to B gate reads a missing
		// one as a refusal, and a half-parsed binding is worse than none.
		owner, _ := text(r, "ownerSessionId")
		workPhase, _ := text(r, "workPhaseId")
		unit, _ := text(r, "planUnit")
		epochID, _ := text(r, "planEpoch")
		out = append(out, ReviewRoundState{
			RoundID: id, Purpose: ReviewPurpose(purpose), PlanPath: planPath, PlanSha256: planSha, Status: ReviewRoundStatus(status), Lane: *lane,
			OpenedAt: timestamp(r, "openedAt"), ClosedAt: textPtr(r, "closedAt"), OwnerSessionID: owner, WorkPhaseID: workPhase,
			PlanUnit: unit, PlanEpoch: epochID, PlanFiles: revivePlanFiles(r["planFiles"]),
		})
	}
	return out
}

// escapesWorkspace reports whether a plan-file path leaves the working directory it is resolved against: an absolute path, or a
// relative one that, cleaned, is ".." or climbs out with "../". It reads the text only: a symlink inside the workspace is not
// followed here, which is the reader of the path's job.
func escapesWorkspace(p string) bool {
	if path.IsAbs(p) {
		return true
	}
	c := path.Clean(p)
	return c == ".." || strings.HasPrefix(c, "../")
}

// revivePlanFiles rebuilds the file list of a round: every entry must have a non-empty sha256 and a non-empty path that stays
// inside the working directory, or the whole list is dropped, because a partial file set would silently narrow what the round
// claims to have covered, and a round without its files is refused by the A to B gate rather than trusted. nil is absent, and
// an empty list is absent too. The oracle keeps any non-empty path, so one naming a file outside the workspace is hashed on
// every staleness check (a review finding of kind security, fixed here on purpose: known-defects.md, port: fixed).
func revivePlanFiles(raw any) []PlanFileHash {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]PlanFileHash, 0, len(list))
	for _, entry := range list {
		f, ok := entry.(map[string]any)
		file, _ := text(f, "path")
		sum, _ := text(f, "sha256")
		if !ok || file == "" || sum == "" || escapesWorkspace(file) {
			return nil
		}
		out = append(out, PlanFileHash{Path: file, Sha256: sum})
	}
	return out
}

// reviveSourceIdentity rebuilds a source identity, or nil when its kind is not "resolved" or "unavailable" or its source root is
// present and not an absolute path. Unlike the strict rebuild of a session state (state.reconstructSourceIdentity) it fills in
// what is missing: an absent or non-text commit id is "", an absent or non-text capture time the epoch, and a dirty that is not
// the boolean true is false; a tree hash is kept when it is a string, empty included, and a dirty identity need not have one.
func reviveSourceIdentity(raw any) *SourceIdentity {
	s, ok := raw.(map[string]any)
	kind, _ := text(s, "kind")
	if !ok || (kind != string(source.KindResolved) && kind != string(source.KindUnavailable)) {
		return nil
	}
	commit, _ := text(s, "commitSha")
	id := SourceIdentity{Kind: source.Kind(kind), CommitSha: commit, Dirty: s["dirty"] == true, CapturedAt: timestamp(s, "capturedAt"), TreeHash: textPtr(s, "treeHash")}
	if v, present := s["sourceRoot"]; present {
		root, isText := v.(string)
		if !isText || !path.IsAbs(root) {
			return nil
		}
		id.SourceRoot = &root
	}
	return &id
}

// reviveFinalGate rebuilds the final gate. A gate whose status or qaRequired cannot be trusted is dropped entirely, so a malformed
// gate reads as no gate on a plan of schema 1 and as a schema violation on one of schema 2 or more, never as a passing gate.
func reviveFinalGate(raw any) *FinalGateState {
	g, ok := raw.(map[string]any)
	status, _ := text(g, "status")
	qaRequired, hasQA := g["qaRequired"].(bool)
	if !ok || !isGateStatus(status) || !hasQA {
		return nil
	}
	return &FinalGateState{
		Status: FinalGateStatus(status), QaRequired: qaRequired, UpdatedAt: timestamp(g, "updatedAt"),
		ReviewRoundID: textPtr(g, "reviewRoundId"), TestReceiptPath: textPtr(g, "testReceiptPath"), QaReceiptPath: textPtr(g, "qaReceiptPath"),
		Verdict: verdictOf(g["verdict"]), SourceIdentity: reviveSourceIdentity(g["sourceIdentity"]),
	}
}
