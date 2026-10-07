package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ReviewRoundRunOptions are the seams of RunReviewRoundCli. Nil means the oracle's own: the process environment for the CODEX_HOME probe
// of the open packet, and the lock's default retry delays (5, 10, 20 and 40 ms; a non-nil empty list tries once).
type ReviewRoundRunOptions struct {
	Env  host.LookupEnv
	Lock *goalplan.GoalplanWriteLockOptions

	// WriteGoalplan is the CRW-823 plan-write seam of the open and abort verbs. Nil means
	// goalplan.WriteGoalplan, the writer a production call uses; a caller that passes one drives the
	// published-but-unsynced path. It is an argument, never package state, so one test cannot fault
	// another's write.
	WriteGoalplan func(string, *goalplan.Goalplan) error
}

// cliPublishedWriteGoalplan is the configured plan write, defaulting to the real one.
func cliPublishedWriteGoalplan(o *ReviewRoundRunOptions) func(string, *goalplan.Goalplan) error {
	if o != nil && o.WriteGoalplan != nil {
		return o.WriteGoalplan
	}
	return goalplan.WriteGoalplan
}

// RunReviewRoundCli ports runReviewRoundCli (review-round-cli.ts:210-306, CXC v0.2.40, 3c1459ac) on the parser, the plan-file
// collection and the open packet of review_round_args.go. The verb row that calls it is not part of this file. A refusal is a result
// with code 1; the error is where the oracle throws: an unreadable plan file or working directory, a slug that is no slug, a
// goalplan that cannot be written, and a home the open packet cannot resolve (which the oracle reports after the write).
func RunReviewRoundCli(args ReviewRoundCliArgs, o *ReviewRoundRunOptions) (ReviewRoundCliResult, error) {
	if args.Verb == ReviewRoundVerbHelp {
		return ReviewRoundCliResult{Output: RenderReviewRoundHelp()}, nil
	}
	if o == nil {
		o = &ReviewRoundRunOptions{}
	}
	session := ""
	if args.Session != nil {
		session = text.Trim(*args.Session)
	}
	if session == "" {
		return reviewRoundRunRefuse("review-round: --session <id> is required"), nil
	}
	// CRW-871: open and abort write, so a non-canonical id is refused after the trim and before the
	// state read: state.ReadState sanitises the key, so a raw id would judge and rewrite a DIFFERENT
	// session's plan. show only reads and keeps the oracle's behaviour.
	if args.Verb != ReviewRoundVerbShow && !state.IsCanonicalSessionID(session) {
		return reviewRoundRunRefuse("review-round " + string(args.Verb) + ": " + sessionAliasRefusalText), nil
	}
	st := state.ReadState(args.Cwd, session)
	switch args.Verb {
	case ReviewRoundVerbOpen:
		return reviewRoundRunOpen(args, session, st, o)
	case ReviewRoundVerbAbort:
		return reviewRoundRunAbort(args, st, o)
	}
	return reviewRoundRunShow(args, st)
}

func reviewRoundRunRefuse(output string) ReviewRoundCliResult {
	return ReviewRoundCliResult{Code: 1, Output: output}
}

// reviewRoundRunReason is "reason" in result ? result.reason : result.kind.
func reviewRoundRunReason(r review.ReviewRoundResult) string {
	if r.Reason != "" {
		return r.Reason
	}
	return string(r.Kind)
}

// reviewRoundRunActiveWorkPhase ports effectiveActiveWorkPhaseId (goalplan.ts:2249-2267), which the goalplan package does not carry
// yet; "" is its null. The cursor wins only when it names a runnable phase, otherwise the first runnable in-progress phase wins, then
// the first runnable pending one. A candidate to move into the goalplan package.
func reviewRoundRunActiveWorkPhase(plan *goalplan.Goalplan) string {
	if cursor := plan.ActiveWorkPhaseID; cursor != nil && *cursor != "" {
		if i := slices.IndexFunc(plan.WorkPhases, func(p goalplan.GoalplanWorkPhase) bool { return p.ID == *cursor }); i >= 0 && goalplan.IsRunnablePhase(plan, &plan.WorkPhases[i]) {
			return *cursor
		}
	}
	for _, status := range []goalplan.WorkPhaseStatus{goalplan.WorkPhaseInProgress, goalplan.WorkPhasePending} {
		for i := range plan.WorkPhases {
			if p := &plan.WorkPhases[i]; p.Status == status && goalplan.IsRunnablePhase(plan, p) {
				return p.ID
			}
		}
	}
	return ""
}

// reviewRoundRunRoundsIntact reports whether every review round stored in the goalplan survived revival. Revival drops a round it
// cannot read and the write that follows would delete it from the file, which the oracle does silently (known-defects.md, a record
// data-loss defect fixed here by refusing). The stored rounds are counted from the file as the reader decodes it (exact key, last
// duplicate wins), read through a handle on the plan directory without following a link and without waiting on a special file; the
// caller holds the lock. Anything it cannot read counts as not intact.
func reviewRoundRunRoundsIntact(cwd, slug string, plan *goalplan.Goalplan) bool {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	if info, err := root.Lstat(goalplan.GoalplanFile); err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := root.OpenFile(goalplan.GoalplanFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return false
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	var stored map[string]json.RawMessage
	var rounds []json.RawMessage
	if json.Unmarshal([]byte(source.DecodeUTF8(raw)), &stored) != nil {
		return false
	}
	if value, present := stored["reviewRounds"]; present && string(value) != "null" && json.Unmarshal(value, &rounds) != nil {
		return false
	}
	return len(rounds) == len(plan.ReviewRounds)
}

// reviewRoundRunBound is the plan with the binding of REVIEW-BINDING-01 on the opened round. Only the entry the library selects by
// purpose and launch id changes; the oracle matches by roundId alone (review-round-cli.ts:246), which also rewrites every other entry
// that shares the id (known-defects.md, a record data-loss defect fixed here). The plan it is given is not touched.
func reviewRoundRunBound(plan *goalplan.Goalplan, launch, session, workPhase, unit, epoch string, files []goalplan.PlanFileHash) (*goalplan.Goalplan, bool) {
	bound := *plan
	bound.ReviewRounds = slices.Clone(plan.ReviewRounds)
	round := review.RoundByLaunchID(&bound, goalplan.PurposePlanAudit, launch)
	if round == nil {
		return nil, false
	}
	round.OwnerSessionID, round.WorkPhaseID, round.PlanUnit, round.PlanEpoch, round.PlanFiles = session, workPhase, unit, epoch, files
	return &bound, true
}

// reviewRoundRunLocked runs fn on the bound goalplan under its write lock. A lock that cannot be taken and a plan that cannot be read
// are the oracle's "<reason>; retry" refusal; an error of the lock or of fn is returned.
func reviewRoundRunLocked(args ReviewRoundCliArgs, verb, slug string, o *ReviewRoundRunOptions, fn func(*goalplan.Goalplan) (ReviewRoundCliResult, error)) (ReviewRoundCliResult, error) {
	intact := func(plan *goalplan.Goalplan) (ReviewRoundCliResult, error) {
		if !reviewRoundRunRoundsIntact(args.Cwd, slug, plan) {
			return reviewRoundRunRefuse("review-round " + verb + ": the goalplan holds review rounds that this build cannot read; refusing to rewrite it"), nil
		}
		return fn(plan)
	}
	locked, err := goalplan.WithGoalplanWriteLock(args.Cwd, slug, intact, o.Lock)
	if err != nil {
		return ReviewRoundCliResult{}, err
	}
	if locked.Kind != "ok" {
		return reviewRoundRunRefuse("review-round " + verb + ": " + locked.Reason + "; retry"), nil
	}
	return *locked.Value, nil
}

// reviewRoundRunOpen is the open verb (:213-282). The plan files are collected before the lock is taken and the work-phase is read
// inside it, the order the oracle's messages depend on. The packet is rendered after the write.
func reviewRoundRunOpen(args ReviewRoundCliArgs, session string, st state.State, o *ReviewRoundRunOptions) (ReviewRoundCliResult, error) {
	const prefix = "review-round open: "
	switch {
	case st.Phase != state.PhaseA:
		return reviewRoundRunRefuse(fmt.Sprintf(prefix+"session is at %s, not A — a plan audit is opened during Audit", st.Phase)), nil
	case st.Slug == "":
		return reviewRoundRunRefuse(prefix + "this session has no bound goalplan"), nil
	case st.PlanUnit == nil || *st.PlanUnit == "" || st.PlanEpoch == nil || *st.PlanEpoch == "":
		return reviewRoundRunRefuse(prefix + "no plan binding on this session — enter A through `crw pabcd orchestrate A` so P>A records the unit it validated"), nil
	}
	unit, epoch := *st.PlanUnit, *st.PlanEpoch
	files, refusal, err := reviewRoundArgsCollectPlanFiles(args.Cwd, unit, args.PlanPaths)
	if err != nil {
		return ReviewRoundCliResult{}, err
	}
	if refusal != "" {
		return reviewRoundRunRefuse(prefix + refusal), nil
	}
	return reviewRoundRunLocked(args, "open", st.Slug, o, func(plan *goalplan.Goalplan) (ReviewRoundCliResult, error) {
		workPhase := reviewRoundRunActiveWorkPhase(plan)
		if workPhase == "" {
			return reviewRoundRunRefuse(prefix + "the bound goalplan has no active work-phase"), nil
		}
		opened := review.OpenRound(plan, review.OpenRoundInput{Purpose: goalplan.PurposePlanAudit, PlanPath: unit, PlanSha256: PlanFilesHash(files)})
		if opened.Kind != review.OK {
			return reviewRoundRunRefuse(prefix + reviewRoundRunReason(opened)), nil
		}
		round := *opened.Round
		bound, ok := reviewRoundRunBound(opened.Plan, round.Lane.LaunchID, session, workPhase, unit, epoch, files)
		if !ok {
			return reviewRoundRunRefuse(prefix + "no plan_audit round for launch " + round.Lane.LaunchID), nil
		}
		launching := review.MarkLaunching(bound, goalplan.PurposePlanAudit, round.RoundID, round.Lane.LaunchID, nil)
		if launching.Kind != review.OK {
			return reviewRoundRunRefuse(prefix + reviewRoundRunReason(launching)), nil
		}
		inFlight := review.MarkInFlight(launching.Plan, goalplan.PurposePlanAudit, round.RoundID, round.Lane.LaunchID)
		if inFlight.Kind != review.OK {
			return reviewRoundRunRefuse(prefix + reviewRoundRunReason(inFlight)), nil
		}
		// A plan write that published at the final path and then failed the directory sync is a
		// written plan: the round is in flight in the plan every reader sees, so the launch goes ahead
		// and the durability failure is carried as a warning. A failure before the rename published
		// nothing and stays an error.
		warning := ""
		if err := cliPublishedWriteGoalplan(o)(args.Cwd, inFlight.Plan); err != nil {
			if !state.Published(err) {
				return ReviewRoundCliResult{}, err
			}
			warning = cliPublishedGoalplanWarning(st.Slug, err)
		}
		packet, err := reviewRoundArgsRenderOpenPacket(round, len(files), o.Env)
		if err != nil {
			return ReviewRoundCliResult{Output: packet}, err
		}
		if warning != "" {
			packet += "\n" + warning
		}
		return ReviewRoundCliResult{Output: packet}, nil
	})
}

// reviewRoundRunAbort is the abort verb (:284-297): the live round is closed as inconclusive and never approved.
func reviewRoundRunAbort(args ReviewRoundCliArgs, st state.State, o *ReviewRoundRunOptions) (ReviewRoundCliResult, error) {
	if st.Slug == "" {
		return reviewRoundRunRefuse("review-round abort: this session has no bound goalplan"), nil
	}
	reason := "aborted by the agent"
	if args.Reason != nil {
		reason = *args.Reason
	}
	return reviewRoundRunLocked(args, "abort", st.Slug, o, func(plan *goalplan.Goalplan) (ReviewRoundCliResult, error) {
		aborted := review.AbortRound(plan, goalplan.PurposePlanAudit, reason)
		if aborted.Kind != review.OK {
			return reviewRoundRunRefuse("review-round abort: " + reviewRoundRunReason(aborted)), nil
		}
		warning := ""
		if err := cliPublishedWriteGoalplan(o)(args.Cwd, aborted.Plan); err != nil {
			if !state.Published(err) {
				return ReviewRoundCliResult{}, err
			}
			warning = cliPublishedGoalplanWarning(st.Slug, err)
		}
		out := "review-round abort: " + aborted.Round.RoundID + " closed as inconclusive"
		if warning != "" {
			out += "\n" + warning
		}
		return ReviewRoundCliResult{Output: out}, nil
	})
}

// cliPublishedGoalplanWarning is CRW-823's plan durability warning, the goalplan counterpart of
// cliPublishedStateWarning and the wording CRW-793 uses in the goalplan package: the plan at the
// final path is the new one, so the verb stands, but the directory that holds it could not be synced
// and the publication may not survive a crash. The oracle never syncs that directory, so it has no
// counterpart; the wording is the issue's own.
func cliPublishedGoalplanWarning(slug string, err error) string {
	return "goalplan '" + slug + "' was published but its directory could not be synced: " + cliErrorMessage(err)
}

// reviewRoundRunShown is the --json answer of show in the oracle's key order; an absent verdict, work-phase or epoch is null.
type reviewRoundRunShown struct {
	RoundID     string                     `json:"roundId"`
	Status      goalplan.ReviewRoundStatus `json:"status"`
	Staleness   review.Staleness           `json:"staleness"`
	LaunchID    string                     `json:"launchId"`
	Verdict     *string                    `json:"verdict"`
	WorkPhaseID *string                    `json:"workPhaseId"`
	PlanEpoch   *string                    `json:"planEpoch"`
}

func reviewRoundRunNullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// reviewRoundRunShow is the show verb (:299-306). A round that records its files is judged against them read again; one that does not
// (a pre-binding round, or a list that revival dropped) stays "open".
func reviewRoundRunShow(args ReviewRoundCliArgs, st state.State) (ReviewRoundCliResult, error) {
	if st.Slug == "" {
		return reviewRoundRunRefuse("review-round show: this session has no bound goalplan"), nil
	}
	plan := goalplan.ReadGoalplan(args.Cwd, st.Slug)
	if plan == nil {
		return reviewRoundRunRefuse("review-round show: the bound goalplan could not be read"), nil
	}
	round := review.LatestRound(plan, goalplan.PurposePlanAudit)
	if round == nil {
		return ReviewRoundCliResult{Output: "review-round show: no plan_audit round yet"}, nil
	}
	fresh := review.Open
	if round.PlanFiles != nil {
		fresh = review.StalenessOf(plan, round.RoundID, PlanFilesHash(Recomputed(args.Cwd, round.PlanFiles)))
	}
	if args.JSON {
		out, err := statusJSON(reviewRoundRunShown{round.RoundID, round.Status, fresh, round.Lane.LaunchID, reviewRoundRunNullable(string(round.Lane.Verdict)),
			reviewRoundRunNullable(round.WorkPhaseID), reviewRoundRunNullable(round.PlanEpoch)})
		return ReviewRoundCliResult{Output: out}, err
	}
	verdict := "-"
	if round.Lane.Verdict != "" {
		verdict = string(round.Lane.Verdict)
	}
	return ReviewRoundCliResult{Output: fmt.Sprintf("review-round %s: status=%s staleness=%s verdict=%s launch=%s", round.RoundID, round.Status, fresh, verdict, round.Lane.LaunchID)}, nil
}
