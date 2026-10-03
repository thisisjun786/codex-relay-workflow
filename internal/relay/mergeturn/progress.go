package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Progress records and the holding limit (CRW-408).
//
// A holder that works inside its turn records each step it takes (base refresh, CI start,
// polling, CI result, merge attempt). merge-turn-show reads the newest sign of life and says
// when a turn has been silent for the holding limit, so another parent can tell a holder that is
// working from one whose session is gone. The limit is a rule of the lane, kept in this file and
// shown in the target reading and copied into every pass record and return notice; it is not a
// store column.

// LongestCISeconds is the longest a hosted CI job may run: the largest timeout-minutes of any
// job in .github/workflows/ci.yml (15 minutes). A test reads the workflow, so a change to it
// forces a deliberate change here.
const LongestCISeconds = 900

// HoldingMarginSeconds is the margin added to it for a holder that polls, refreshes and records.
const HoldingMarginSeconds = 300

// HoldingLimitSeconds is how long a holding turn may stay silent before it reads as stalled. It
// measures silence between records, not a CI budget: a holder that records ci_polled while a
// long CI runs is not silent.
const HoldingLimitSeconds = LongestCISeconds + HoldingMarginSeconds

// ProgressSteps are the steps a holder records progress for.
var ProgressSteps = []string{"base_refresh", "ci_started", "ci_polled", "ci_result", "merge_attempt"}

const isoLayout = "2006-01-02T15:04:05.000000Z07:00"

// signOfLife is the newest thing a holder did inside its turn: what the turn's clock is read from.
type signOfLife struct {
	Kind     string // "held" (the grant itself) or the evidence kind of the holder's own ledger entry
	Step     string // progress_recorded only
	Sequence int64  // progress_recorded only
	At       string
}

// signRank orders signs of life that carry the same time: an explicit progress record outranks
// the other things a holder does to its turn, which outrank the grant itself.
func signRank(kind string) int {
	switch kind {
	case "progress_recorded":
		return 2
	case "grant_acknowledged", "candidate_head_changed", "readiness_declared", "readiness_withdrawn", "currency_confirmed":
		return 1
	}
	return 0
}

func (a signOfLife) beats(b signOfLife) bool {
	if a.At != b.At {
		return a.At > b.At
	}
	if ra, rb := signRank(a.Kind), signRank(b.Kind); ra != rb {
		return ra > rb
	}
	return a.Sequence > b.Sequence
}

// record is the sign as merge-turn-show prints it.
func (a signOfLife) record() map[string]any {
	var step, sequence any
	if a.Kind == "progress_recorded" {
		step, sequence = a.Step, a.Sequence
	}
	return map[string]any{"evidenceKind": a.Kind, "step": step, "sequence": sequence, "recordedAt": a.At}
}

// describe is how a pass record names the sign.
func (a signOfLife) describe() string {
	if a.Step != "" {
		return a.Step
	}
	return a.Kind
}

// progressBody is the envelope a progress record keeps, in canonical JSON.
func progressBody(turn, step, evidence string, sequence int64) string {
	return canonicalJSON(map[string]any{"evidence": evidence, "sequence": sequence, "step": step, "turnId": turn})
}

// progressEntry reads one progress record back: its step and sequence, or ok false.
func progressEntry(evidence string) (step string, sequence int64, ok bool) {
	var envelope struct {
		Step     string      `json:"step"`
		Sequence json.Number `json:"sequence"`
	}
	if json.Unmarshal([]byte(evidence), &envelope) != nil || envelope.Step == "" {
		return "", 0, false
	}
	n, err := envelope.Sequence.Int64()
	return envelope.Step, n, err == nil
}

// lastSignOfLife is the newest sign of life of a turn's holder: the grant (held_at) or the newest
// ledger entry the holder itself wrote for this tenure, decided by time, then rank, then sequence.
// Entries written before the turn was granted (a waiting claim's readiness) are older than the
// grant and never win.
func lastSignOfLife(r store.MergeTurnsRow, entries []store.MergeTurnLedgerRow) signOfLife {
	best := signOfLife{Kind: "held", At: r.RequestedAt}
	if r.HeldAt.Valid {
		best.At = r.HeldAt.String
	}
	for _, e := range entries {
		if signRank(e.EvidenceKind) == 0 || e.ActorTaskID != r.HolderTaskID {
			continue
		}
		candidate := signOfLife{Kind: e.EvidenceKind, At: e.RecordedAt}
		if e.EvidenceKind == "progress_recorded" {
			if step, sequence, ok := progressEntry(e.Evidence); ok {
				candidate.Step, candidate.Sequence = step, sequence
			}
		}
		if candidate.beats(best) {
			best = candidate
		}
	}
	return best
}

// stall is a turn's silence read against the holding limit.
type stall struct {
	Last           signOfLife
	Readable       bool
	ElapsedSeconds int64
	StallsAt       string
	Stalled        bool
}

// stallOf reads how long the holder has been silent. A time that cannot be read, or a clock that
// is behind the last sign, is no evidence of silence: the turn does not read as stalled. Only a
// holding or merging turn can stall; an unknown outcome waits for an observation, not for the holder.
func stallOf(r store.MergeTurnsRow, entries []store.MergeTurnLedgerRow, now string) stall {
	out := stall{Last: lastSignOfLife(r, entries)}
	last, err := time.Parse(isoLayout, out.Last.At)
	if err != nil {
		return out
	}
	current, err := time.Parse(isoLayout, now)
	if err != nil {
		return out
	}
	out.Readable = true
	out.StallsAt = registry.ISO(last.Add(HoldingLimitSeconds * time.Second))
	out.ElapsedSeconds = max(0, int64(current.Sub(last)/time.Second))
	out.Stalled = (r.State == Holding || r.State == Merging) && out.ElapsedSeconds >= HoldingLimitSeconds
	return out
}

// Progress is the holder recording a step it took inside its turn.
func (s *Service) Progress(ctx context.Context, turn, actor, step, evidence string) (map[string]any, error) {
	if !slices.Contains(ProgressSteps, step) {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "a progress record names the step it records, one of " + strings.Join(ProgressSteps, ", ") + ", not " + fmt.Sprintf("%q", step)}
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	var sequence int64
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		switch {
		case r.HolderTaskID != actor:
			refusal = notHolder(r, actor, "record progress on")
		case r.State != Holding && r.State != Merging:
			refusal = wrongState(r, actor, "recording progress")
		}
		if refusal == nil {
			owner, other, err := s.ownership(tx, r.ProjectKey, r.TargetKey, actor)
			if err != nil {
				return err
			}
			refusal = other
			if refusal == nil && owner != actor {
				refusal = staleOwner(r, owner, true, actor)
			}
		}
		if refusal == nil {
			status, err := s.ownerStatus(tx, r.ProjectKey)
			if err != nil {
				return err
			}
			if status != "active" {
				refusal = paused(r, actor, "record progress")
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		entries, err := s.Store.MergeLedger(tx, turn)
		if err != nil {
			return err
		}
		sequence = 1
		for _, e := range entries {
			if e.EvidenceKind == "progress_recorded" {
				sequence++
			}
		}
		return s.ledger(tx, turn, "attestation", r.State, "", "progress_recorded", actor, progressBody(turn, step, evidence, sequence), fmt.Sprintf("progress:%d", sequence), at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	answer, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	recorded := map[string]any{"step": step, "sequence": sequence, "recordedAt": at, "holdingLimitSeconds": int64(HoldingLimitSeconds)}
	if t, err := time.Parse(isoLayout, at); err == nil {
		recorded["stallsAt"] = registry.ISO(t.Add(HoldingLimitSeconds * time.Second))
	}
	answer["progress"] = recorded
	return answer, nil
}
