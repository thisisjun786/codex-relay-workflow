// Package review ports CXC v0.2.40 review-round.ts:1-397 (commit 3c1459ac).
// Operations copy plans without IO; callers persist them and bind reviewer identity.
// Binding parity uses the existing revived goalplan representation and nonempty
// session identities. It cannot distinguish a raw JS absent owner from an empty
// owner, which revival drops. JSON comparisons use values: JS spread preserves
// history-dependent key order, while Go uses goalplan's existing stored order.
// One departure from the oracle, by decision (CRW-573, known-defects.md): a round id never repeats a stored one, and a
// changed round replaces only itself (reviewNextRoundID, reviewReplaceRound).
package review

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ReviewVerdict reuses the goalplan's stored verdict vocabulary.
type ReviewVerdict = goalplan.Verdict

// ResultKind discriminates a transition outcome.
type ResultKind string

const (
	OK           ResultKind = "ok"
	Stale        ResultKind = "stale"
	CASFailed    ResultKind = "cas_failed"
	NotFound     ResultKind = "not_found"
	InvalidInput ResultKind = "invalid_input"
)

// ReviewRoundResult carries a new plan/round only on success, and Actual on CAS failure.
type ReviewRoundResult struct {
	Kind   ResultKind                 `json:"kind"`
	Plan   *goalplan.Goalplan         `json:"plan,omitempty"`
	Round  *goalplan.ReviewRoundState `json:"round,omitempty"`
	Reason string                     `json:"reason,omitempty"`
	Actual goalplan.ReviewRoundStatus `json:"actual,omitempty"`
}

// Staleness separates an unjudged round from freshness of a terminal round.
type Staleness string

const (
	Fresh     Staleness = "fresh"
	StalePlan Staleness = "stale"
	Open      Staleness = "open"
)

// OpenRoundInput names the document and its caller-computed hash.
type OpenRoundInput struct {
	Purpose              goalplan.ReviewPurpose
	PlanPath, PlanSha256 string
	Now                  func() string
}

// VerdictInput names the launch being judged. Nil metadata keeps earlier values;
// pointers to empty strings record empty strings, as the oracle does.
type VerdictInput struct {
	Purpose                         goalplan.ReviewPurpose
	RoundID, LaunchID               string
	Verdict                         ReviewVerdict
	ArtifactSha256, ReviewerSession *string
	SourceIdentity                  *goalplan.SourceIdentity
	Now                             func() string
	// Blockers and Findings are what a GO-WITH-FIXES sign-off said (CRW-1116); zero and nil record none.
	Blockers int
	Findings []string
}

func reviewTimestamp(now func() string) string {
	if now != nil {
		return now()
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
func reviewTerminal(s goalplan.ReviewRoundStatus) bool {
	return s == goalplan.ReviewApproved || s == goalplan.ReviewChangesRequested || s == goalplan.ReviewInconclusive
}
func reviewCursor(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose) **string {
	if purpose == goalplan.PurposePlanAudit {
		return &p.ActivePlanAuditRoundID
	}
	return &p.ActiveFinalGateRoundID
}

// Numeric prefix, not validation: malformed and infinite ids order as zero.
func reviewRoundOrder(id string) float64 {
	s := text.Trim(strings.TrimPrefix(id, "r"))
	end := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		end++
	}
	start := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	n, _ := strconv.ParseFloat(s[:end], 64)
	if math.IsInf(n, 0) || math.IsNaN(n) {
		return 0
	}
	return n
}

// reviewRoundIDLimit is 2^53, where float64 stops telling an integer from its successor. The oracle mints "highest + 1"
// in float64, so from here on a new id can repeat a stored one (a data-loss defect, port: fixed in known-defects.md).
const reviewRoundIDLimit = 1 << 53

// reviewNextRoundID refuses, in the shape OpenRound already refuses with, when the next id equals a stored id or is not an
// exact integer below 2^53. Below that the answer is the oracle's.
func reviewNextRoundID(p *goalplan.Goalplan) (string, ReviewRoundResult) {
	highest := 0.0
	for _, r := range p.ReviewRounds {
		highest = math.Max(highest, reviewRoundOrder(r.RoundID))
	}
	n := highest + 1
	id := "r" + strconv.FormatFloat(n, 'f', -1, 64)
	for _, r := range p.ReviewRounds {
		if r.RoundID == id {
			return "", ReviewRoundResult{Kind: InvalidInput, Reason: fmt.Sprintf("round id %s is already stored: opening another round would overwrite it", id)}
		}
	}
	if n >= reviewRoundIDLimit {
		return "", ReviewRoundResult{Kind: InvalidInput, Reason: fmt.Sprintf("the next round id %s is not an exact integer below 2^53: it could repeat a stored round id", id)}
	}
	return id, ReviewRoundResult{}
}
func reviewMintLaunchID(id, now string) string {
	var digits strings.Builder
	for _, r := range now {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			if digits.Len() == 14 {
				break
			}
		}
	}
	return id + "-" + digits.String()
}
func reviewWithRounds(p *goalplan.Goalplan, list []goalplan.ReviewRoundState, purpose goalplan.ReviewPurpose, cursor *string) *goalplan.Goalplan {
	out := *p
	out.ReviewRounds = list
	*reviewCursor(&out, purpose) = cursor
	return &out
}

// reviewReplaceRound replaces the one entry that was selected (a pointer into list), so it matches the id, purpose and launch
// id of the updated round by construction. A damaged plan can repeat any of them, even all three, and replacing by a key
// overwrote the other entries that share it.
func reviewReplaceRound(list []goalplan.ReviewRoundState, selected *goalplan.ReviewRoundState, updated goalplan.ReviewRoundState) []goalplan.ReviewRoundState {
	out := make([]goalplan.ReviewRoundState, len(list))
	for i := range list {
		if &list[i] == selected {
			out[i] = updated
		} else {
			out[i] = list[i]
		}
	}
	return out
}

// EffectiveRound trusts a usable cursor, otherwise recovers the highest open id.
func EffectiveRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose) *goalplan.ReviewRoundState {
	cursor := *reviewCursor(p, purpose)
	if cursor != nil && *cursor != "" {
		for i := range p.ReviewRounds {
			r := &p.ReviewRounds[i]
			if r.RoundID == *cursor {
				if r.Purpose == purpose && !reviewTerminal(r.Status) {
					return r
				}
				break
			}
		}
	}
	var best *goalplan.ReviewRoundState
	for i := range p.ReviewRounds {
		r := &p.ReviewRounds[i]
		if r.Purpose == purpose && !reviewTerminal(r.Status) && (best == nil || reviewRoundOrder(r.RoundID) > reviewRoundOrder(best.RoundID)) {
			best = r
		}
	}
	return best
}

// LatestRound includes terminal rounds and keeps the first on numeric ties.
func LatestRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose) *goalplan.ReviewRoundState {
	var best *goalplan.ReviewRoundState
	for i := range p.ReviewRounds {
		r := &p.ReviewRounds[i]
		if r.Purpose == purpose && (best == nil || reviewRoundOrder(r.RoundID) > reviewRoundOrder(best.RoundID)) {
			best = r
		}
	}
	return best
}

// RoundByLaunchID finds the launch independently of the cursor.
func RoundByLaunchID(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, launch string) *goalplan.ReviewRoundState {
	if launch == "" {
		return nil
	}
	for i := range p.ReviewRounds {
		r := &p.ReviewRounds[i]
		if r.Purpose == purpose && r.Lane.LaunchID == launch {
			return r
		}
	}
	return nil
}

// OpenRound refreshes a pending audit of the same document, otherwise retires
// every open round of that purpose and appends a new pending round.
func OpenRound(p *goalplan.Goalplan, in OpenRoundInput) ReviewRoundResult {
	if in.PlanSha256 == "" {
		return ReviewRoundResult{Kind: InvalidInput, Reason: "planSha256 is required: a round without a plan hash cannot be judged fresh or stale"}
	}
	if in.PlanPath == "" {
		return ReviewRoundResult{Kind: InvalidInput, Reason: "planPath is required"}
	}
	now := reviewTimestamp(in.Now)
	if live := EffectiveRound(p, in.Purpose); live != nil && live.Status == goalplan.ReviewPending && live.PlanPath == in.PlanPath {
		r := *live
		r.PlanSha256 = in.PlanSha256
		return ReviewRoundResult{Kind: OK, Plan: reviewWithRounds(p, reviewReplaceRound(p.ReviewRounds, live, r), in.Purpose, &r.RoundID), Round: &r}
	}
	id, refused := reviewNextRoundID(p)
	if refused.Kind != "" {
		return refused
	}
	list := make([]goalplan.ReviewRoundState, len(p.ReviewRounds), len(p.ReviewRounds)+1)
	for i, r := range p.ReviewRounds {
		if r.Purpose == in.Purpose && !reviewTerminal(r.Status) {
			r.Status = goalplan.ReviewInconclusive
			r.ClosedAt = &now
		}
		list[i] = r
	}
	r := goalplan.ReviewRoundState{RoundID: id, Purpose: in.Purpose, PlanPath: in.PlanPath, PlanSha256: in.PlanSha256, Status: goalplan.ReviewPending, Lane: goalplan.ReviewLane{LaunchID: reviewMintLaunchID(id, now)}, OpenedAt: now}
	list = append(list, r)
	return ReviewRoundResult{Kind: OK, Plan: reviewWithRounds(p, list, in.Purpose, &r.RoundID), Round: &r}
}
func reviewRequireRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string) (*goalplan.ReviewRoundState, ReviewRoundResult) {
	if hit := RoundByLaunchID(p, purpose, launch); hit != nil && hit.RoundID == id {
		for _, r := range p.ReviewRounds {
			if r.Purpose == purpose && reviewRoundOrder(r.RoundID) > reviewRoundOrder(hit.RoundID) {
				return nil, ReviewRoundResult{Kind: Stale, Reason: fmt.Sprintf("round %s was superseded before this verdict arrived", id)}
			}
		}
		return hit, ReviewRoundResult{}
	}
	for _, r := range p.ReviewRounds {
		if r.Purpose == purpose && r.RoundID == id {
			return nil, ReviewRoundResult{Kind: Stale, Reason: fmt.Sprintf("launch %s was superseded by %s", launch, r.Lane.LaunchID)}
		}
	}
	return nil, ReviewRoundResult{Kind: NotFound, Reason: fmt.Sprintf("no %s round for launch %s", purpose, launch)}
}
func reviewAdvance(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string, expect goalplan.ReviewRoundStatus, mutate func(goalplan.ReviewRoundState) goalplan.ReviewRoundState, clear bool) ReviewRoundResult {
	r, result := reviewRequireRound(p, purpose, id, launch)
	if r == nil {
		return result
	}
	if r.Status != expect {
		return ReviewRoundResult{Kind: CASFailed, Reason: fmt.Sprintf("round %s is %s, expected %s", id, r.Status, expect), Actual: r.Status}
	}
	updated := mutate(*r)
	cursor := &id
	if clear {
		cursor = nil
	}
	return ReviewRoundResult{Kind: OK, Plan: reviewWithRounds(p, reviewReplaceRound(p.ReviewRounds, r, updated), purpose, cursor), Round: &updated}
}

// MarkLaunching requires pending. Nil workspace preserves the CLI's omitted argument.
func MarkLaunching(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string, workspace *string) ReviewRoundResult {
	return reviewAdvance(p, purpose, id, launch, goalplan.ReviewPending, func(r goalplan.ReviewRoundState) goalplan.ReviewRoundState {
		r.Status = goalplan.ReviewLaunching
		r.Lane.WorkspaceRoot = workspace
		return r
	}, false)
}

// MarkInFlight cannot skip launching.
func MarkInFlight(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string) ReviewRoundResult {
	return reviewAdvance(p, purpose, id, launch, goalplan.ReviewLaunching, func(r goalplan.ReviewRoundState) goalplan.ReviewRoundState {
		r.Status = goalplan.ReviewInFlight
		return r
	}, false)
}

// RecordVerdict checks launch supersession before CAS and clears only its purpose's cursor.
func RecordVerdict(p *goalplan.Goalplan, in VerdictInput) ReviewRoundResult {
	now := reviewTimestamp(in.Now)
	return reviewAdvance(p, in.Purpose, in.RoundID, in.LaunchID, goalplan.ReviewInFlight, func(r goalplan.ReviewRoundState) goalplan.ReviewRoundState {
		r.Lane.Verdict = in.Verdict
		if in.ArtifactSha256 != nil {
			r.Lane.ArtifactSha256 = in.ArtifactSha256
		}
		if in.ReviewerSession != nil {
			r.Lane.ReviewerSession = in.ReviewerSession
		}
		if in.SourceIdentity != nil {
			r.Lane.SourceIdentity = in.SourceIdentity
		}
		r.Lane.Blockers, r.Lane.Findings = in.Blockers, in.Findings
		switch in.Verdict {
		case goalplan.VerdictPass, goalplan.VerdictNearPass:
			r.Status = goalplan.ReviewApproved
		case goalplan.VerdictFail:
			r.Status = goalplan.ReviewChangesRequested
		}
		r.ClosedAt = &now
		return r
	}, true)
}

// SupersedeStaleRounds closes only open rounds owned by session in the earlier epoch.
// With no matching round the original plan pointer is returned.
func SupersedeStaleRounds(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, session, epoch string) (*goalplan.Goalplan, []string) {
	closed := []string{}
	if epoch == "" {
		return p, closed
	}
	now := reviewTimestamp(nil)
	list := make([]goalplan.ReviewRoundState, len(p.ReviewRounds))
	for i, r := range p.ReviewRounds {
		if r.Purpose == purpose && !reviewTerminal(r.Status) && r.OwnerSessionID != "" && r.OwnerSessionID == session && r.PlanEpoch == epoch {
			closed = append(closed, r.RoundID)
			r.Status = goalplan.ReviewInconclusive
			r.ClosedAt = &now
		}
		list[i] = r
	}
	if len(closed) == 0 {
		return p, closed
	}
	return reviewWithRounds(p, list, purpose, nil), closed
}

// ObsoleteRounds lists the open rounds of purpose that session owns and that belong to an epoch other than keep (CRW-1100):
// every round a re-plan to keep strands, where SupersedeStaleRounds names one earlier epoch only. A round without an epoch, a
// round another session owns, another purpose's round and a closed round are not listed.
func ObsoleteRounds(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, session, keep string) []string {
	ids := []string{}
	for _, r := range p.ReviewRounds {
		if r.Purpose == purpose && !reviewTerminal(r.Status) && r.OwnerSessionID != "" && r.OwnerSessionID == session &&
			r.PlanEpoch != "" && r.PlanEpoch != keep {
			ids = append(ids, r.RoundID)
		}
	}
	return ids
}

// SupersedeRounds closes the rounds named in ids that are still what ObsoleteRounds lists for session and keep: open, of
// purpose, owned by session, of another non-empty epoch. A round that changed since it was listed (closed, re-owned, re-bound
// to keep) is left alone, so replaying the same list closes nothing twice. The cursor of purpose is cleared only when it named
// a round this call closed; a cursor on another round, which may hold a valid review, is kept. With nothing to close the
// original plan pointer is returned. Every round it closes is stamped closedAt ("" is the time of the call) and records by, the id of
// the cleanup that closes it, as SupersededBy: a cleanup that must tell its own closures from an abort of the same round, which may
// carry the same stamp, compares that id afterwards.
func SupersedeRounds(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, session, keep string, ids []string, closedAt, by string) (*goalplan.Goalplan, []string) {
	closed := []string{}
	now := closedAt
	if now == "" {
		now = reviewTimestamp(nil)
	}
	list := make([]goalplan.ReviewRoundState, len(p.ReviewRounds))
	for i, r := range p.ReviewRounds {
		if r.Purpose == purpose && !reviewTerminal(r.Status) && r.OwnerSessionID != "" && r.OwnerSessionID == session &&
			r.PlanEpoch != "" && r.PlanEpoch != keep && slices.Contains(ids, r.RoundID) {
			closed = append(closed, r.RoundID)
			r.Status = goalplan.ReviewInconclusive
			r.ClosedAt = &now
			r.SupersededBy = by
		}
		list[i] = r
	}
	if len(closed) == 0 {
		return p, closed
	}
	cursor := *reviewCursor(p, purpose)
	if cursor != nil && slices.Contains(closed, *cursor) {
		cursor = nil
	}
	return reviewWithRounds(p, list, purpose, cursor), closed
}

// AbortRound abandons the selected live round without approving it.
func AbortRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, reason string) ReviewRoundResult {
	live := EffectiveRound(p, purpose)
	if live == nil {
		return ReviewRoundResult{Kind: NotFound, Reason: fmt.Sprintf("no open %s round", purpose)}
	}
	r := *live
	now := reviewTimestamp(nil)
	r.Status = goalplan.ReviewInconclusive
	r.ClosedAt = &now
	if r.Lane.ReviewerSession == nil {
		s := "aborted: " + reason
		r.Lane.ReviewerSession = &s
	}
	return ReviewRoundResult{Kind: OK, Plan: reviewWithRounds(p, reviewReplaceRound(p.ReviewRounds, live, r), purpose, nil), Round: &r}
}

// StalenessOf compares hashes only once the round is terminal, regardless of purpose.
func StalenessOf(p *goalplan.Goalplan, id, sha string) Staleness {
	for _, r := range p.ReviewRounds {
		if r.RoundID == id {
			if !reviewTerminal(r.Status) {
				return Open
			}
			if r.PlanSha256 == sha {
				return Fresh
			}
			return StalePlan
		}
	}
	return Open
}

// StoredIdentity converts a capture to the existing stored form. Captures omit
// empty tree hashes; callers with explicit empty metadata use the stored type directly.
func StoredIdentity(id source.Identity) goalplan.SourceIdentity {
	out := goalplan.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}
