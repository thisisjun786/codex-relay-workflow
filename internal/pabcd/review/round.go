// Package review ports CXC v0.2.40 review-round.ts (3c1459ac): pure review
// lifecycle operations over existing goalplan records. Callers own IO and identity.
package review

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

type ReviewVerdict = goalplan.Verdict
type ResultKind string

const (
	OK           ResultKind = "ok"
	Stale        ResultKind = "stale"
	CASFailed    ResultKind = "cas_failed"
	NotFound     ResultKind = "not_found"
	InvalidInput ResultKind = "invalid_input"
)

type ReviewRoundResult struct {
	Kind   ResultKind                 `json:"kind"`
	Plan   *goalplan.Goalplan         `json:"plan,omitempty"`
	Round  *goalplan.ReviewRoundState `json:"round,omitempty"`
	Reason string                     `json:"reason,omitempty"`
	Actual goalplan.ReviewRoundStatus `json:"actual,omitempty"`
}
type Staleness string

const (
	Fresh     Staleness = "fresh"
	StalePlan Staleness = "stale"
	Open      Staleness = "open"
)

type OpenRoundInput struct {
	Purpose              goalplan.ReviewPurpose
	PlanPath, PlanSha256 string
	Now                  func() string
}
type VerdictInput struct {
	Purpose                         goalplan.ReviewPurpose
	RoundID, LaunchID               string
	Verdict                         ReviewVerdict
	ArtifactSha256, ReviewerSession *string
	SourceIdentity                  *goalplan.SourceIdentity
	Now                             func() string
}

func EffectiveRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose) *goalplan.ReviewRoundState {
	return nil
}
func LatestRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose) *goalplan.ReviewRoundState {
	return nil
}
func RoundByLaunchID(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, launch string) *goalplan.ReviewRoundState {
	return nil
}
func OpenRound(p *goalplan.Goalplan, in OpenRoundInput) ReviewRoundResult {
	return ReviewRoundResult{Kind: NotFound}
}
func MarkLaunching(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string, workspace *string) ReviewRoundResult {
	return ReviewRoundResult{Kind: NotFound}
}
func MarkInFlight(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, id, launch string) ReviewRoundResult {
	return ReviewRoundResult{Kind: NotFound}
}
func RecordVerdict(p *goalplan.Goalplan, in VerdictInput) ReviewRoundResult {
	return ReviewRoundResult{Kind: NotFound}
}
func SupersedeStaleRounds(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, session, epoch string) (*goalplan.Goalplan, []string) {
	return p, []string{}
}
func AbortRound(p *goalplan.Goalplan, purpose goalplan.ReviewPurpose, reason string) ReviewRoundResult {
	return ReviewRoundResult{Kind: NotFound}
}
func StalenessOf(p *goalplan.Goalplan, id, sha string) Staleness { return Open }
func StoredIdentity(id source.Identity) goalplan.SourceIdentity  { return goalplan.SourceIdentity{} }
