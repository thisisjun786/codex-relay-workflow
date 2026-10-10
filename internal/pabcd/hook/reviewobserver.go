package hook

import (
	"encoding/json"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// reviewObserverUnparsed is the ledger event of a child that exited with no parseable sign-off while a plan_audit round was
// waiting. The goalplan package names the other events of the oracle's ledger; this one is only written here.
const reviewObserverUnparsed goalplan.GoalplanLedgerEvent = "review_signoff_unparsed"

// HandleReviewObserver ports CXC v0.2.40 review-observer.ts:57-164 (3c1459ac), the SubagentStop observer that owns plan-audit
// approval: the first parseable sign-off of a child binds the round to that child's agent id, and nothing else writes an
// approval. It never blocks and answers "" always. Every parse, IO or lock failure is swallowed, so a missed recording costs one
// more audit round and never an unrelated subagent's exit.
//
// The raw payload is read here rather than through the harness's SubagentStop parser, which requires agent_type: a v1 spawn has
// none (review-deadlock.test.ts:111-122), and the observer must still name the round. The caller applies the PABCD gate
// (cli.ts:427, PABCD_DISABLED_EVENTS).
func HandleReviewObserver(raw string) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil || payload == nil {
		return ""
	}
	field := func(key string) string {
		s, _ := payload[key].(string)
		return s
	}
	if field("hook_event_name") != "SubagentStop" {
		return ""
	}
	// An executor or worker exit belongs to the receipt gate; everything else is decided by the sign-off. Not "== explorer":
	// a v1 child arrives as "default".
	if evidence.IsGatedAgentType(field("agent_type")) {
		return ""
	}
	cwd, sessionID := field("cwd"), field("session_id")
	if cwd == "" || sessionID == "" {
		return ""
	}
	// last_assistant_message only: reading the child transcript would scan bytes without knowing whose they are, so a
	// LAUNCH/VERDICT example inside the dispatch packet could sign off on itself.
	//
	// CRW-1116 (port: fixed): the verdict line is the grammar of review.ParseSignoff, the one the reviewer skill and the relay's
	// report renderer share, so `GO-WITH-FIXES (blockers=N)` is a near-pass that keeps N, and the oracle's "no sign-off at all"
	// for it (review-round.ts:372) is gone. A count the grammar refuses is still no sign-off.
	signoff := review.ParseSignoff(field("last_assistant_message"))
	// CRW-564 d1 (port: fixed): the oracle reads the state and writes the verdict without the session lock, so a FAIL could land
	// between the A>B transition's review check and its publication, both of which run inside that lock. The session lock comes
	// first and the goalplan lock second, the order every writer of this tree follows, and the state is read under it. A held
	// session lock is a missed recording, as a held goalplan lock is: the lock gives up after about 250 ms.
	// A workspace with no session state is left alone: the lock would create the state directory (baseline_empty_workspace).
	if state.ReadState(cwd, sessionID).Slug == "" {
		return ""
	}
	_ = reviewObserverSessionLock(cwd, sessionID, func() error {
		st := state.ReadState(cwd, sessionID)
		if st.Slug == "" {
			return nil
		}
		observer := reviewObserver{cwd: cwd, slug: st.Slug}
		_, _ = goalplan.WithGoalplanWriteLock(cwd, st.Slug, func(plan *goalplan.Goalplan) (string, error) {
			observer.observe(plan, st, sessionID, field("agent_id"), signoff)
			return "", nil
		}, nil)
		return nil
	})
	return ""
}

// reviewObserverSessionLock is the observer's session-lock entry. It is state.WithSessionLock; a test replaces it (export_test.go)
// with a wait that reports when the observer is provably blocked on a held lock and that gives up on the test's own budget, so
// the interleaving does not depend on the wall clock. Production never assigns it.
var reviewObserverSessionLock = state.WithSessionLock

type reviewObserver struct{ cwd, slug string }

// note appends a ledger row. The oracle's note is wrapped in try/catch, and its ignore is not; both end the same way here, as an
// append that failed leaves the plan as it was and the exit unbroken.
func (o reviewObserver) note(event goalplan.GoalplanLedgerEvent, detail string, roundID, launchID *string) {
	_ = goalplan.AppendGoalplanLedger(o.cwd, o.slug, goalplan.GoalplanLedgerEntry{
		Ts: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Slug: o.slug, Event: event, Detail: detail, RoundID: roundID, LaunchID: launchID})
}

func (o reviewObserver) observe(plan *goalplan.Goalplan, st state.State, sessionID, agentID string, signoff *review.ReviewSignoff) {
	if signoff == nil {
		for _, round := range plan.ReviewRounds {
			if round.Purpose == goalplan.PurposePlanAudit && round.Status == goalplan.ReviewInFlight {
				// Once a session is in A with a live round waiting, a child that exits without a parseable sign-off is worth one
				// line: the difference between "the reviewer said nothing usable" and "the gate is broken".
				if st.Phase == state.PhaseA {
					o.note(reviewObserverUnparsed, "a subagent exited with no parseable sign-off while a plan_audit round was in flight; "+
						"the closing two lines must be exactly LAUNCH then VERDICT (PASS, FAIL, NEAR-PASS or GO-WITH-FIXES (blockers=N))", nil, nil)
				}
				return
			}
		}
		return
	}
	launch := signoff.LaunchID
	round := review.RoundByLaunchID(plan, goalplan.PurposePlanAudit, launch)
	if round == nil {
		o.note(goalplan.EventReviewSignoffIgnored,
			string(signoff.Verdict)+" sign-off named launch "+launch+", which belongs to no plan_audit round", nil, &launch)
		return
	}
	roundID := round.RoundID
	ignore := func(reason string) {
		o.note(goalplan.EventReviewSignoffIgnored, string(signoff.Verdict)+" sign-off was not recorded: "+reason, &roundID, &launch)
	}
	// An empty stored binding is an absent one (revival drops it), and the oracle compares an absent value with !==, so it never
	// equals the session, the epoch or the active work-phase, a null one included.
	switch {
	case st.Phase != state.PhaseA:
		ignore("the session left A before the reviewer finished")
		return
	case round.OwnerSessionID != sessionID:
		ignore("the round belongs to another session")
		return
	case round.PlanEpoch == "" || st.PlanEpoch == nil || round.PlanEpoch != *st.PlanEpoch:
		ignore("the plan was re-planned after this round opened")
		return
	case agentID == "":
		// CRW-564 d2 (port: fixed): the oracle takes `agent_id ?? ""`, so a child it cannot name would close the round with an
		// empty reviewerSession that the A>B check spends as an approval and that locks out the real reviewer.
		ignore("the child named no agent id")
		return
	case round.Lane.ReviewerSession != nil && *round.Lane.ReviewerSession != agentID:
		ignore("round " + round.RoundID + " was already signed by " + *round.Lane.ReviewerSession)
		return
	}
	active := goalplan.EffectiveActiveWorkPhaseID(plan)
	if active == nil || round.WorkPhaseID == "" || round.WorkPhaseID != *active {
		audited, current := "none", "none"
		if round.WorkPhaseID != "" {
			audited = round.WorkPhaseID
		}
		if active != nil {
			current = *active
		}
		ignore("the round audited work-phase " + audited + ", but " + current + " is active")
		return
	}
	recorded := review.RecordVerdict(plan, review.VerdictInput{Purpose: goalplan.PurposePlanAudit, RoundID: round.RoundID, LaunchID: launch,
		Verdict: signoff.Verdict, ReviewerSession: &agentID, Blockers: signoff.Blockers, Findings: signoff.Findings})
	if recorded.Kind != review.OK {
		reason := recorded.Reason
		if reason == "" {
			reason = string(recorded.Kind)
		}
		ignore(reason)
		return
	}
	_ = goalplan.WriteGoalplan(o.cwd, recorded.Plan)
}
