package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// designReuseOp is crw.designReuse(milestone): the decision of a project lane's first move (CRW-1329). When the same plan's
// earlier project-lane run of the same repository recorded an approved DESIGN over the same node slices and the same milestone
// digest, the run reuses it: its ledger's first revision is an import of that DESIGN, and the run's own architect, audit and
// background phase never run. The op only searches and decides; crw.designLedger({reuse:true}) installs what it found, so no
// design row stands without this decision.
//
// A source is eligible only on its own standing records: the latest current ledger revision of its run, whose DESIGN decodes,
// whose candidate digest is the ledger's and whose own approved audit - an independent critic's, made between the revision before
// it and it - stands. A revision another run imported (a ledger with a source_run) is not an approval of its own: the search goes
// on to the run that actually audited. Every reason a source is not used is recorded in the decision's attempts.
type designReuseOp struct{ l *Lane }

// DesignRun is one earlier run the reuse search reads: its id and its journal's events, or Reason/Detail why it could not be one
// (source_unreadable, repository_mismatch).
type DesignRun struct {
	Run    string
	Events []engine.Event
	Reason string
	Detail string
}

// DesignReuseAttempt is why one source run was not reused.
type DesignReuseAttempt struct {
	SourceRun      string `json:"source_run"`
	SourceRevision string `json:"source_revision,omitempty"`
	Reason         string `json:"reason"`
	Detail         string `json:"detail,omitempty"`
}

// DesignReuseResult is the answer of crw.designReuse.
type DesignReuseResult struct {
	Reused          bool                 `json:"reused"`
	Reason          string               `json:"reason"`
	Milestone       string               `json:"milestone"`
	SourceRun       string               `json:"source_run,omitempty"`
	SourceRevision  string               `json:"source_revision,omitempty"`
	SourceAuditCall string               `json:"source_audit_call,omitempty"`
	ArchitectModel  string               `json:"architect_model,omitempty"`
	CriticModel     string               `json:"critic_model,omitempty"`
	Digest          string               `json:"digest,omitempty"`
	BaseTip         string               `json:"base_tip,omitempty"`
	MilestoneDigest string               `json:"milestone_digest,omitempty"`
	Slices          map[string]string    `json:"slices,omitempty"`
	Design          json.RawMessage      `json:"design,omitempty"`
	Answers         json.RawMessage      `json:"answers,omitempty"`
	Attempts        []DesignReuseAttempt `json:"attempts"`
}

// The reasons a reuse decision records.
const (
	ReuseApproved         = "approved_design_reused"
	ReuseNoApproved       = "no_approved_design"
	ReuseMissing          = "design_missing"
	ReuseNotApproved      = "design_not_approved"
	ReuseInvalid          = "design_invalid"
	ReuseSlicesChanged    = "node_slices_changed"
	ReuseMilestoneChanged = "milestone_digest_changed"
	ReuseSourceUnreadable = "source_unreadable"
	ReuseRepoMismatch     = "repository_mismatch"
)

func (o *designReuseOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designReuse(milestone): %w", err)
	}
	r := rows(req.Journal.Events())
	m, plan, err := milestoneOf(r, ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designReuse: %w", err)
	}
	live, mileDigest := liveSlices(m), digestOf(m)
	out := DesignReuseResult{Milestone: ms, Attempts: []DesignReuseAttempt{}}
	finish := func() (engine.OpResult, error) {
		data := map[string]any{}
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &data)
		data["plan"], data["milestone"] = plan, ms
		return engine.OpResult{Result: result(out), Data: data}, nil
	}
	if o.l.d.DesignHistory == nil {
		out.Reason = ReuseNoApproved
		return finish()
	}
	sources, err := o.l.d.DesignHistory(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	for _, src := range sources {
		if src.Run == "" || src.Run == req.RunID {
			continue
		}
		att := DesignReuseAttempt{SourceRun: src.Run}
		if src.Reason != "" {
			att.Reason, att.Detail = src.Reason, src.Detail
			out.Attempts = append(out.Attempts, att)
			continue
		}
		sr := rows(src.Events)
		led, ok := currentLedger(sr, ms)
		if !ok {
			att.Reason = ReuseMissing
			out.Attempts = append(out.Attempts, att)
			continue
		}
		att.SourceRevision = dataStr(led, "revision")
		if dataStr(led, "source_run") != "" {
			// an imported revision is no audit of its own: the run that audited the DESIGN is found by the search itself
			att.Reason, att.Detail = ReuseNotApproved, "the revision was itself imported from run "+dataStr(led, "source_run")
			out.Attempts = append(out.Attempts, att)
			continue
		}
		stored, storedDigest := ledgerSlices(led), dataStr(led, "milestone_digest")
		// the slices first (a node set that changed is not the plan that was designed), then the milestone's own digest
		sc := sliceChange(stored, live)
		for node, digest := range live {
			if digest == "" {
				sc.Changed = append(sc.Changed, node)
			}
		}
		if len(sc.Changed) != 0 {
			att.Reason, att.Detail = ReuseSlicesChanged, fmt.Sprintf("the slices of %v changed", sc.Changed)
			out.Attempts = append(out.Attempts, att)
			continue
		}
		if storedDigest != mileDigest {
			att.Reason, att.Detail = ReuseMilestoneChanged, "the milestone's digest is "+storedDigest+", not "+mileDigest
			out.Attempts = append(out.Attempts, att)
			continue
		}
		rawDesign, _ := json.Marshal(led.Data["design"])
		rawAnswers, _ := json.Marshal(led.Data["answers"])
		cand := Candidate{Design: rawDesign, Answers: rawAnswers}
		whyInvalid := func(detail string) {
			att.Reason, att.Detail = ReuseInvalid, detail
			out.Attempts = append(out.Attempts, att)
		}
		design, derr := DecodeDesign(rawDesign)
		switch {
		case derr != nil:
			whyInvalid("the DESIGN of the revision does not decode: " + derr.Error())
		case cand.digest() != dataStr(led, "digest"):
			whyInvalid("the recorded candidate is not the digest the ledger stands on")
		default:
			want := map[string]bool{}
			for _, n := range m.Nodes {
				want[n.ID] = true
			}
			got := map[string]bool{}
			for _, is := range design.Issues {
				if got[is.Node] {
					whyInvalid("the DESIGN plans node " + is.Node + " twice")
				}
				got[is.Node] = true
				delete(want, is.Node)
			}
			if att.Reason == "" && len(want) != 0 {
				whyInvalid(fmt.Sprintf("the DESIGN plans no issue of the nodes %v", want))
			}
			if att.Reason == "" {
				for node := range got {
					if _, live := live[node]; !live {
						whyInvalid("the DESIGN plans node " + node + ", which the milestone does not have")
					}
				}
			}
		}
		if att.Reason != "" {
			continue
		}
		// the audit the revision stands on: approved, of this candidate, independent of its architect, between the revision
		// before it and it
		after := 0
		for _, ev := range ledgerRows(sr, ms) {
			if ev.Seq == led.Seq {
				break
			}
			after = ev.Seq
		}
		audit, ok := sr.current(OpDesignAudit).last(func(ev engine.Event) bool {
			return ev.Seq > after && ev.Seq < led.Seq && dataStr(ev, "milestone") == ms && dataStr(ev, "candidate_digest") == cand.digest() && dataStr(ev, "state") == AuditApproved
		})
		var approved AuditResult
		if !ok || json.Unmarshal(resultOf(audit), &approved) != nil || approved.State != AuditApproved || approved.CandidateDigest != cand.digest() ||
			dataStr(audit, "critic_call") == "" || att.SourceRevision == "" {
			att.Reason, att.Detail = ReuseNotApproved, "the revision has no approved independent audit of its own"
			out.Attempts = append(out.Attempts, att)
			continue
		}
		out.Reused, out.Reason = true, ReuseApproved
		out.SourceRun, out.SourceRevision, out.SourceAuditCall = src.Run, att.SourceRevision, dataStr(audit, "critic_call")
		out.ArchitectModel, out.CriticModel = dataStr(audit, "architect_model"), dataStr(audit, "critic_model")
		out.Digest, out.BaseTip = dataStr(led, "digest"), dataStr(led, "base_tip")
		out.MilestoneDigest, out.Slices = mileDigest, stored
		out.Design, out.Answers = rawDesign, rawAnswers
		return finish()
	}
	if out.Reason == "" {
		out.Reason = ReuseNoApproved
	}
	return finish()
}

func (o *designReuseOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// reusedDesign is the recorded reuse decision of the run: the last current crw.designReuse of the milestone that answered reused,
// and the row that recorded it.
func reusedDesign(r rows, ms string) (engine.Event, DesignReuseResult, bool) {
	row, ok := r.current(OpDesignReuse).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return engine.Event{}, DesignReuseResult{}, false
	}
	var d DesignReuseResult
	if json.Unmarshal(resultOf(row), &d) != nil || !d.Reused || d.Milestone != ms {
		return engine.Event{}, DesignReuseResult{}, false
	}
	if d.SourceRun == "" || d.SourceRevision == "" || d.SourceAuditCall == "" || d.Digest == "" || d.MilestoneDigest == "" {
		return engine.Event{}, DesignReuseResult{}, false
	}
	return row, d, true
}

// reusedLedgerApproved says whether the ledger revision led is an import the run's own reuse decision proves: the revision names
// the decision's call (reuse_call) and the decision stands - same milestone, source run, revision, audit call and digest - and the
// revision's own data agrees with it (the candidate's digest, the slices and the milestone's digest). A ledger row that names a
// run is never approval by itself.
func reusedLedgerApproved(r rows, ms string, led engine.Event) bool {
	call := dataStr(led, "reuse_call")
	if call == "" || dataStr(led, "revision") != FirstRevision {
		return false
	}
	reuse, ok := r.current(OpDesignReuse).last(func(ev engine.Event) bool { return ev.CallID == call })
	if !ok {
		return false
	}
	var d DesignReuseResult
	if json.Unmarshal(resultOf(reuse), &d) != nil || !d.Reused || d.Milestone != ms {
		return false
	}
	if reuse.Seq >= led.Seq || d.SourceRun == "" || d.SourceRevision == "" ||
		d.ArchitectModel != dataStr(led, "architect_model") || d.CriticModel != dataStr(led, "critic_model") ||
		d.BaseTip != dataStr(led, "base_tip") {
		return false
	}
	if d.SourceRun != dataStr(led, "source_run") || d.SourceRevision != dataStr(led, "source_revision") ||
		d.SourceAuditCall == "" || d.SourceAuditCall != dataStr(led, "source_audit_call") {
		return false
	}
	rawDesign, _ := json.Marshal(led.Data["design"])
	rawAnswers, _ := json.Marshal(led.Data["answers"])
	if (Candidate{Design: rawDesign, Answers: rawAnswers}).digest() != dataStr(led, "digest") || d.Digest != dataStr(led, "digest") {
		return false
	}
	if !maps.Equal(ledgerSlices(led), d.Slices) || d.MilestoneDigest != dataStr(led, "milestone_digest") {
		return false
	}
	return true
}
