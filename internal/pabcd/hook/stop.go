// The Stop continuation leg of the pabcd-state hooks: CXC v0.2.40 pabcd-state/src/hook.ts (commit
// 3c1459ac) handleStop (:1805-1873) with the stagnation guard it stands on (MAX_STOP_BLOCKS,
// MAX_STOP_BLOCKS_TOTAL, observeProgress, bumpStopCounter :1403-1521), the block texts
// (stopNextCommand, buildStopBlock, buildGoalIdleBlock, buildPlateauDivergeBlock :1523-1771) and the
// render advisory (renderGroundingAdvisoryForStop :1899-1917), under the CRW names of
// contract/schema/cxc/name-substitution.json. The registration is the harness leg row
// stop-checking-pabcd-continuation. Harness parses the payload as cli.ts does (parse.ts), so the
// event name and the required string fields are checked when StopHandle runs.
//
// What Stop decides, in the oracle's order: phase I is never driven; with no cycle in flight a plain
// session releases, and only an ACTIVE host goal with a bound goalplan whose remaining work is not
// waiting on an open decision gets the goal-idle arming block; with a cycle in flight only an ACTIVE
// goal blocks (an interactive session may get the render advisory as additionalContext). A transcript
// inside a compaction's recovery window releases (CRW-1090: a compaction record, not the oracle's pressure
// phrases anywhere in the tail, which a quote also matched; the window ends at the next user prompt, and a
// hook that sees the compaction records it so the window outlives the tail, compaction_recovery.go,
// docs/port-cxc/known-defects/CRW-1090.md). Every block goes through one counter:
// three consecutive blocks at the same phase and work phase release (progress recharges the budget),
// and 24 per user turn release for good, the second of them with a systemMessage. The counter is
// written before the block is answered, so a block the counter could not record is never answered.
//
// Differences from the oracle, all recorded in docs/port-cxc/known-defects/CRW-192.md: the counter is
// read, judged and written inside the session lock on the state the lock found (the oracle writes the
// whole state back from an unlocked read, which loses a participating writer's update), a change of
// the phase, the binding or the user turn stamp between the first read and the lock releases instead of
// blocking on a stale phase, a pause of the goal or a decision opened in that window releases too (the
// goal, the goalplan wait and the payload's turn_id are judged again inside the lock), a cycle in flight whose bound goalplan waits only on open decisions releases as the idle path
// does, and the friction advisory line is not ported (its ledger writer is a deprecated, unregistered
// hook of the oracle, so no CRW store feeds it).
package hook

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The budgets of the Stop loop (hook.ts:1403-1411). StopMaxBlocks bounds consecutive blocks at one
// phase and recharges on progress; StopMaxBlocksTotal bounds the blocks of one user turn and is never
// recharged by progress (the UserPromptSubmit turn stamp resets it).
const (
	StopMaxBlocks            = 3
	StopMaxBlocksTotal       = 24
	stopPlateauMetricRecords = 2
	stopPlateauNoiseFloor    = 0
)

// stopTotalCapMessage is the systemMessage of the total-cap release (hook.ts:1832,1853) after R25.
const stopTotalCapMessage = "CRW Stop continuation cap (24) reached for this user turn; releasing."

// StopPayload is what the harness hands StopHandle: the Stop fields handleStop reads. TranscriptPath
// is the payload's transcript_path, and TurnID the payload's turn_id; each is empty when it is null,
// absent or not a string. TurnID is held against the turn stamp of the state (not in the oracle).
type StopPayload struct{ Cwd, SessionID, TranscriptPath, TurnID string }

// StopAnswer is what a Stop leg answers. Stdout is the finished line (a block, or the systemMessage of
// the total cap); Context is the additionalContext of an interactive session's render advisory, which
// harness wraps in the Stop envelope (buildContextOutput), as it does for the other hooks. At most one
// of them is set.
type StopAnswer struct{ Stdout, Context string }

// StopHandle is handleStop. platform is a Node platform spelling ("win32" selects the PowerShell
// attest-file commands); empty selects this host's platform. env resolves the goals database and the
// crw invocation of the commands in the block.
func StopHandle(p StopPayload, platform string, env host.LookupEnv) StopAnswer {
	return stopHandle(p, platform, env, state.WithSessionLock)
}

// stopHandle takes the session lock as an argument so that a test can land a participating writer's
// update, or a lock that cannot be taken, where the oracle's unlocked write would have been.
func stopHandle(p StopPayload, platform string, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error) StopAnswer {
	if env == nil {
		env = os.LookupEnv
	}
	// A session id the state file name would rewrite ("s/1" is the file and the lock of "s-1") is not a session this writer may
	// count against: the counter would be charged to, and the sessionId stamped over, another session's file. The oracle writes
	// under the same sanitised name; the Go writers refuse such an id (state.IsCanonicalSessionID), and so does Stop: it releases.
	if !state.IsCanonicalSessionID(p.SessionID) {
		return StopAnswer{}
	}
	st := state.ReadState(p.Cwd, p.SessionID)
	// guard 2a': the autonomous Stop loop is PABCD-only; the Interview is HITL-only and Stop never drives it.
	if st.Phase == state.PhaseI {
		return StopAnswer{}
	}
	goalActive := sessionHookGoalStatus(p.SessionID, env) == host.GoalActive
	inFlight := st.OrchestrationActive && st.Phase != state.PhaseIdle

	if !inFlight {
		if !goalActive {
			return StopAnswer{}
		}
		plan := stopSafeReadBoundGoalplan(p.Cwd, st.Slug)
		if plan == nil || goalplan.RemainingWorkAwaitsDecisions(plan) {
			return StopAnswer{}
		}
		if stopContextPressure(p) {
			return StopAnswer{}
		}
		return stopCounted(p, st, platform, env, lock, stopIdleDue, func(fresh state.State) string {
			return stopGoalIdleBlock(p.Cwd, fresh, p.SessionID, platform, env)
		})
	}

	// C-RENDER-GROUNDING-01 advisory: fail-open, for goal and interactive sessions alike, never a block.
	renderAdvisory := stopRenderAdvisory(p.Cwd, st.Phase, p.SessionID, st.Slug)

	// guard 2b: only an ACTIVE goal arms the autonomous loop (interactive sessions pause).
	if !goalActive {
		return StopAnswer{Context: renderAdvisory}
	}
	// A bound plan whose remaining work waits only on open decisions is waiting on the user: no block.
	if plan := stopSafeReadBoundGoalplan(p.Cwd, st.Slug); plan != nil && goalplan.RemainingWorkAwaitsDecisions(plan) {
		return StopAnswer{}
	}
	if stopContextPressure(p) {
		return StopAnswer{}
	}
	return stopCounted(p, st, platform, env, lock, stopInFlightDue, func(fresh state.State) string {
		if plateau := stopObjectivePlateau(p.Cwd, p.SessionID); plateau.Flat {
			return stopPlateauDivergeBlock(fresh.Phase, plateau, p.Cwd, p.SessionID, renderAdvisory)
		}
		reason := stopBuildBlockReason(fresh.Phase, stopReadWorkContext(p.Cwd, fresh), p.SessionID, platform, env)
		if renderAdvisory != "" {
			reason += "\n\n" + renderAdvisory
		}
		return stopEnvelope(reason)
	})
}

// stopWriteState is the counter write, a variable so that a test can fail it before and after the publication
// (state.Published), which a file system rarely does on demand. Production leaves it state.WriteState.
var stopWriteState = state.WriteState

// stopDue judges again, inside the lock, what the decision outside it stood on and that the user can
// change meanwhile: the goal is still ACTIVE (a pause releases), the bound goalplan still reads and does
// not wait on an open decision (idle: it must still read and not wait; in flight: it must not wait), and
// the event still belongs to the user turn the state is stamped with.
type stopDue func(p StopPayload, fresh state.State, env host.LookupEnv) bool

func stopHeld(p StopPayload, fresh state.State, env host.LookupEnv) (*goalplan.Goalplan, bool) {
	if sessionHookGoalStatus(p.SessionID, env) != host.GoalActive {
		return nil, false
	}
	if p.TurnID != "" && fresh.StopBlockTurnID != nil && *fresh.StopBlockTurnID != p.TurnID {
		return nil, false
	}
	return stopSafeReadBoundGoalplan(p.Cwd, fresh.Slug), true
}

func stopIdleDue(p StopPayload, fresh state.State, env host.LookupEnv) bool {
	plan, ok := stopHeld(p, fresh, env)
	return ok && plan != nil && !goalplan.RemainingWorkAwaitsDecisions(plan)
}

func stopInFlightDue(p StopPayload, fresh state.State, env host.LookupEnv) bool {
	plan, ok := stopHeld(p, fresh, env)
	return ok && !(plan != nil && goalplan.RemainingWorkAwaitsDecisions(plan))
}

// stopCounted bumps the stop counter under the session lock and, when the bump leaves a block, builds
// it from the state the lock found. judged is the state the decision was made on; if the phase, the
// cycle, the binding or the user turn changed since, or due no longer holds, the event is stale and
// releases without writing.
func stopCounted(p StopPayload, judged state.State, platform string, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error, due stopDue, build func(fresh state.State) string) StopAnswer {
	var answer StopAnswer
	err := lock(p.Cwd, p.SessionID, func() error {
		fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable || !stopSameBinding(judged, fresh) || !promptSubmitRewritable(p.Cwd, p.SessionID, fresh) || !due(p, fresh, env) {
			return nil
		}
		next, outcome := stopBump(p.Cwd, fresh)
		if err := stopWriteState(p.Cwd, next); err != nil && !state.Published(err) {
			return nil
		}
		switch outcome {
		case stopBumpBlock:
			answer = StopAnswer{Stdout: build(fresh)}
		case stopBumpTotalCap:
			answer = StopAnswer{Stdout: stopSystemMessage(stopTotalCapMessage)}
		}
		return nil
	})
	if err != nil && !state.Published(err) {
		return StopAnswer{}
	}
	return answer
}

// stopSameBinding is whether the state the lock found still has the phase, the cycle, the goalplan
// binding and the user turn stamp the decision was made on.
func stopSameBinding(a, b state.State) bool {
	return a.Phase == b.Phase && a.OrchestrationActive == b.OrchestrationActive && a.Slug == b.Slug &&
		stopSameText(a.StopBlockTurnID, b.StopBlockTurnID)
}

type stopBumpOutcome int

const (
	stopBumpBlock       stopBumpOutcome = iota // the counter allows a block
	stopBumpPhaseCap                           // consecutive blocks at one phase exceeded the budget: release
	stopBumpTotalCap                           // the turn's total exceeded the budget, first time: release with a systemMessage
	stopBumpTotalSilent                        // the turn's total exceeded the budget, already announced: release
)

// stopBump is bumpStopCounter (hook.ts:1497-1521) on a state read inside the lock: the state to write
// and what the counter decided. The cursor and the total advance on every Stop, block or release.
func stopBump(cwd string, st state.State) (state.State, stopBumpOutcome) {
	obs := stopObserveProgress(cwd, st)
	nextCount := st.StopBlockCount + 1
	if obs.progressed {
		nextCount = 1
	}
	nextTotal := st.StopBlockTotal + 1
	next := st
	next.StopMetricCursor, next.StopBlockTotal = obs.metricCursor, nextTotal
	if nextCount > StopMaxBlocks || nextTotal > StopMaxBlocksTotal {
		totalCap := nextTotal > StopMaxBlocksTotal
		alreadyNotified := st.StopBlockCapNotified
		next.StopBlockPhase, next.StopBlockWorkPhaseID, next.StopBlockCount = nil, nil, 0
		if totalCap {
			next.StopBlockCapNotified = true
		}
		switch {
		case !totalCap:
			return next, stopBumpPhaseCap
		case alreadyNotified:
			return next, stopBumpTotalSilent
		}
		return next, stopBumpTotalCap
	}
	phase := st.Phase
	next.StopBlockPhase, next.StopBlockWorkPhaseID, next.StopBlockCount = &phase, obs.workPhaseID, nextCount
	return next, stopBumpBlock
}

type stopProgress struct {
	progressed   bool
	metricCursor float64
	workPhaseID  *string
}

// stopObserveProgress is observeProgress (hook.ts:1465-1490): did a phase transition, a work-phase
// switch or a new, better metric row happen since the last Stop. Fail-open: an unreadable ledger or
// goalplan is no progress.
func stopObserveProgress(cwd string, st state.State) stopProgress {
	improved := false
	rows := metric.ReadObjectiveMetrics(cwd, st.SessionID)
	// High-water: a hand-truncated ledger must not let restored rows replay as new observations.
	cursor := math.Max(st.StopMetricCursor, float64(len(rows)))
	if float64(len(rows)) > st.StopMetricCursor {
		improved = !metric.CheckObjectivePlateau(cwd, st.SessionID, metric.PlateauOptions{MinRecords: stopPlateauMetricRecords, NoiseFloor: stopPlateauNoiseFloor}).Flat
	}
	var workPhaseID *string
	if plan := stopSafeReadBoundGoalplan(cwd, st.Slug); plan != nil {
		workPhaseID = goalplan.EffectiveActiveWorkPhaseID(plan)
	}
	phaseChanged := st.StopBlockPhase == nil || *st.StopBlockPhase != st.Phase
	workChanged := !stopSameText(st.StopBlockWorkPhaseID, workPhaseID)
	return stopProgress{progressed: phaseChanged || workChanged || improved, metricCursor: cursor, workPhaseID: workPhaseID}
}

func stopSameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// stopSafeReadBoundGoalplan is safeReadBoundGoalplan (hook.ts:1430-1436): the session-bound goalplan,
// refusing a slug that could escape its directory.
func stopSafeReadBoundGoalplan(cwd, slug string) *goalplan.Goalplan {
	if slug == "" || slug == "." || slug == ".." || !stopSlugPattern.MatchString(slug) {
		return nil
	}
	return goalplan.ReadGoalplan(cwd, slug)
}

var stopSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// stopObjectivePlateau is objectivePlateau (hook.ts:1773-1782): flat only for a maximize objective.
func stopObjectivePlateau(cwd, sessionID string) metric.PlateauCheck {
	if metric.ReadObjectiveKind(cwd, sessionID) != metric.Maximize {
		return metric.PlateauCheck{Values: []float64{}}
	}
	return metric.CheckObjectivePlateau(cwd, sessionID, metric.PlateauOptions{MinRecords: stopPlateauMetricRecords, NoiseFloor: stopPlateauNoiseFloor})
}

// stopEnvelope is `${JSON.stringify({decision:"block",reason})}\n`.
func stopEnvelope(reason string) string {
	return stopMarshal(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", reason})
}

func stopSystemMessage(message string) string {
	return stopMarshal(struct {
		SystemMessage string `json:"systemMessage"`
	}{message})
}

func stopMarshal(v any) string {
	b, err := role.Stringify(v, "")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// stopNodePlatform is the Node spelling of platform; empty selects this host's.
func stopNodePlatform(platform string) string {
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform == "windows" {
		return "win32"
	}
	return platform
}

// stopNextCommands is STOP_NEXT_COMMAND (hook.ts:1523-1537) under the name substitution (R33 and the
// cli table: `cxc orchestrate` is `crw pabcd orchestrate`). These are the commands an agent copies out
// of a Stop block, so each example carries every key the edge requires; the bound-only keys are shown
// with a marker.
var stopNextCommands = map[state.Phase]string{
	state.PhaseI: "`crw pabcd orchestrate P --attest '{\"from\":\"I\",\"to\":\"P\",\"did\":\"interview complete with recorded requirements\"}'`",
	state.PhaseP: "`crw pabcd orchestrate A --attest '{\"from\":\"P\",\"to\":\"A\",\"did\":\"diff-level plan written with files and acceptance criteria\",\"planUnit\":\"devlog/_plan/YYMMDD_slug\",\"workPhaseId\":\"<bound goalplan only>\"}'`",
	state.PhaseA: "`crw pabcd orchestrate B --attest '{\"from\":\"A\",\"to\":\"B\",\"did\":\"audit loop closed: blockers folded into plan\",\"auditOutput\":\"<reviewer verdict tail>\",\"auditVerdict\":\"pass|near-pass\",\"auditResidual\":\"<near-pass only: residual blockers + disposition>\",\"workPhaseId\":\"<bound goalplan only>\"}'`",
	state.PhaseB: "`crw pabcd orchestrate C --attest '{\"from\":\"B\",\"to\":\"C\",\"did\":\"implementation completed and verifier reviewed it\",\"workPhaseId\":\"<bound goalplan only>\"}'`",
	state.PhaseC: "`crw pabcd orchestrate D --attest '{\"from\":\"C\",\"to\":\"D\",\"did\":\"checks passed\",\"checkOutput\":\"<test tail>\",\"exitCode\":0,\"testReceiptPath\":\"<bound goalplan only: crw pabcd receipt test output path>\",\"workPhaseId\":\"<bound goalplan only>\"}'`",
	state.PhaseD: "`crw pabcd orchestrate reset` after the DONE summary is recorded",
}

var (
	stopAttestJSON      = regexp.MustCompile(`--attest '(\{.*\})'`)
	stopVerb            = regexp.MustCompile(`crw pabcd orchestrate (\S+)`)
	stopOrchestrateVerb = regexp.MustCompile(`crw pabcd orchestrate (\w+)`)
)

// stopNextCommand is stopNextCommand (hook.ts:1539-1553): win32 cannot pass the attest JSON inline, so
// it points at the file flag there. The second result is false for a phase with no entry.
func stopNextCommand(phase state.Phase, platform string) (string, bool) {
	posix, ok := stopNextCommands[phase]
	if !ok || stopNodePlatform(platform) != "win32" {
		return posix, ok
	}
	attest := stopAttestJSON.FindStringSubmatch(posix)
	verb := stopVerb.FindStringSubmatch(posix)
	if attest == nil || verb == nil {
		return posix, true
	}
	const q = "`"
	write := q + "'" + attest[1] + "' | Set-Content -Encoding utf8 " + crwdir.DirName + "/attest.json" + q
	run := q + "crw pabcd orchestrate " + verb[1] + " --attest-file " + crwdir.DirName + "/attest.json" + q
	return write + " then " + run, true
}

// stopWorkContext is StopWorkContext (hook.ts:1561-1575): the next concrete work of the bound goalplan.
type stopWorkContext struct {
	readyWorkPhases  []stopReadyPhase
	readyTasks       []stopReadyTask
	expectedEvidence string
	waitingOn        []string
	ledgerPath       string
}

type stopReadyPhase struct{ id, title string }
type stopReadyTask struct{ workPhaseID, id, title string }

// stopReadWorkContext is readStopWorkContext (hook.ts:1639-1692): pure and fail-safe, keyed strictly on
// the session-bound slug; a missing slug or an absent or unreadable goalplan gives nil, so the block is
// the phase-only one.
func stopReadWorkContext(cwd string, st state.State) *stopWorkContext {
	slug := st.Slug
	if slug == "" {
		return nil
	}
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		return nil
	}
	ledger := crwdir.DirName + "/goalplans/" + slug + "/ledger.jsonl"
	// `loop ready` refuses an invalid graph; so does this, rather than naming an id twice.
	integrity := append(goalplan.GoalplanDefinitionIntegrityReasons(plan), goalplan.GoalplanDependencyCompletionReasons(plan)...)
	if len(integrity) > 0 {
		return &stopWorkContext{waitingOn: []string{"the plan is not a valid graph: " + strings.Join(integrity, "; ")}, ledgerPath: ledger}
	}
	phases, tasks, unmet := goalplan.ReadyWorkPhases(plan), goalplan.ReadyTasks(plan), goalplan.UnmetCriteria(plan)
	// A global deadlock and a partial wait are different states; report whichever applies.
	var waitingOn []string
	if deadlock := goalplan.DetectDependencyDeadlock(plan); deadlock != nil {
		waitingOn = deadlock.Reasons
	} else {
		waitingOn = goalplan.DependencyWaitReasons(plan)
	}
	if len(phases) == 0 && len(tasks) == 0 && len(unmet) == 0 && len(waitingOn) == 0 {
		return nil // nothing remaining -> no enrichment
	}
	work := &stopWorkContext{waitingOn: waitingOn, ledgerPath: ledger}
	for _, wp := range phases {
		work.readyWorkPhases = append(work.readyWorkPhases, stopReadyPhase{wp.ID, wp.Title})
	}
	for _, t := range tasks {
		work.readyTasks = append(work.readyTasks, stopReadyTask{t.WorkPhaseID, t.Task.ID, t.Task.Title})
	}
	if len(unmet) > 0 {
		work.expectedEvidence = unmet[0].ExpectedEvidence
	}
	return work
}

// lines is the enrichment lines of a work context, in the oracle's order. They are appended after the
// command and never replace it.
func (w *stopWorkContext) lines() []string {
	var out []string
	if len(w.readyWorkPhases) > 0 {
		parts := make([]string, len(w.readyWorkPhases))
		for i, wp := range w.readyWorkPhases {
			parts[i] = wp.id + " (" + wp.title + ")"
		}
		out = append(out, "Ready work phases: "+strings.Join(parts, ", "))
	}
	if len(w.readyTasks) > 0 {
		parts := make([]string, len(w.readyTasks))
		for i, t := range w.readyTasks {
			parts[i] = t.workPhaseID + "/" + t.id + " (" + t.title + ")"
		}
		out = append(out, "Ready tasks: "+strings.Join(parts, "; "))
	}
	if len(w.waitingOn) > 0 {
		out = append(out, "Waiting on: "+strings.Join(w.waitingOn, "; "))
	}
	if w.expectedEvidence != "" {
		out = append(out, "Required evidence: "+w.expectedEvidence)
	}
	if w.ledgerPath != "" {
		out = append(out, "Record progress in: "+w.ledgerPath)
	}
	return out
}

// stopBuildBlockReason is the reason of buildStopBlock (hook.ts:1577-1631) without the friction line
// (see the file comment): the session id is inserted after the verb of the command, before the
// invocation is resolved at emission.
func stopBuildBlockReason(phase state.Phase, work *stopWorkContext, sessionID, platform string, env host.LookupEnv) string {
	next, ok := stopNextCommand(phase, platform)
	if !ok {
		next = "`crw pabcd orchestrate status`"
	}
	if sessionID != "" {
		// Mutating verbs require --session, so the continuation command must carry it. Only the first
		// occurrence is rewritten, and the id is inserted as text.
		if loc := stopOrchestrateVerb.FindStringSubmatchIndex(next); loc != nil {
			next = next[:loc[3]] + " --session " + sessionID + next[loc[3]:]
		}
	}
	lines := []string{
		fmt.Sprintf("[crw — continue PABCD] You are mid-cycle at %s (%s) with an active goal.", phase, stageLabel(phase)),
		"Do the real work of this phase, then self-advance with the concrete next command:",
		next,
	}
	if work != nil {
		lines = append(lines, work.lines()...)
	}
	lines = append(lines, "C→D requires checkOutput+exitCode. D is not a resting state; close the cycle back to IDLE.")
	return ResolveCRWInDirective(strings.Join(lines, "\n"), env)
}

// stopGoalIdleBlock is buildGoalIdleBlock (hook.ts:1694-1738): the Stop block for a goal ACTIVE with a
// bound goalplan but no PABCD cycle in flight. The reason names the two honest exits: arm the next
// work-phase, or close the goal for real.
func stopGoalIdleBlock(cwd string, st state.State, sessionID, platform string, env host.LookupEnv) string {
	// Inline JSON cannot survive PowerShell argument parsing, so win32 gets the write-then-attest pair.
	startNext := "Either start the next work-phase now: `crw pabcd orchestrate P --session " + sessionID + " --attest '{\"from\":\"IDLE\",\"to\":\"P\",\"did\":\"<diff-level plan for the next work-phase>\"}'`"
	if stopNodePlatform(platform) == "win32" {
		startNext = "Either start the next work-phase now: write the JSON with `'{\"from\":\"IDLE\",\"to\":\"P\",\"did\":\"<diff-level plan for the next work-phase>\"}' | Set-Content -Encoding utf8 " + crwdir.DirName + "/attest.json` then run `crw pabcd orchestrate P --session " + sessionID + " --attest-file " + crwdir.DirName + "/attest.json`"
	}
	lines := []string{
		"[crw — goal continuation] A host goal is ACTIVE but no PABCD cycle is in flight.",
		"GOAL-IDLE-CONTINUE-01: IDLE is not the end while the goal is active (LOOP-CONTINUE-01). Do not end the turn here.",
		startNext,
		"or close the goal honestly: `update_goal` status \"complete\" (only when the recorded criteria are proven — the E8 gate checks a bound goalplan) or status \"blocked\" for an external blocker.",
		"LOOP-UNIT-CHAIN-01: work-phases chain HETEROGENEOUS units in one session — an independent feature/plan discovered mid-loop is simply the NEXT work-phase (append it to the goalplan, then orchestrate P). \"Needs its own PABCD\" is a plan statement, not a session boundary; do not close the goal while naming remaining features that fit the objective.",
	}
	var plan *goalplan.Goalplan
	if st.Slug != "" {
		plan = goalplan.ReadGoalplan(cwd, st.Slug)
	}
	switch work := stopReadWorkContext(cwd, st); {
	case work != nil:
		lines = append(lines, work.lines()...)
	case plan != nil && len(plan.WorkPhases) == 0 && len(plan.Criteria) == 0:
		lines = append(lines, "The bound goalplan '"+st.Slug+"' is EMPTY: register workPhases[]/criteria[] in "+crwdir.DirName+"/goalplans/"+st.Slug+"/goalplan.json (schema in $crw-loop) so remaining work is durable and the E8 gate can pass.")
	case st.Slug == "":
		lines = append(lines, "No goalplan is bound to this session: run `crw pabcd loop init --objective \"<the goal objective>\" --session "+sessionID+"` and register workPhases[]/criteria[] before the next work-phase.")
	}
	return stopEnvelope(ResolveCRWInDirective(strings.Join(lines, "\n"), env))
}

// stopPlateauDivergeBlock is buildPlateauDivergeBlock (hook.ts:1740-1771): the block that replaces the
// continuation when a maximize objective's latest metric values do not improve. advisory is the render/native observation
// advisory of the same Stop ("" when none applies); it follows the block text.
func stopPlateauDivergeBlock(phase state.Phase, plateau metric.PlateauCheck, cwd, sessionID, advisory string) string {
	values := "n/a"
	if len(plateau.Values) > 0 {
		texts := make([]string, len(plateau.Values))
		for i, v := range plateau.Values {
			texts[i] = stopNumberText(v)
		}
		values = strings.Join(texts, " -> ")
	}
	name := "objective"
	if plateau.MetricName != nil {
		name = *plateau.MetricName
	}
	candidates := []metric.DivergenceCandidate{}
	if cwd != "" && sessionID != "" {
		candidates = metric.ReadDivergenceCandidates(cwd, sessionID)
	}
	streak := metric.DiscardStreak(candidates)
	discarded := []metric.DivergenceCandidate{}
	for _, c := range candidates {
		if c.Status == metric.StatusDiscarded {
			discarded = append(discarded, c)
		}
	}
	slices.SortStableFunc(discarded, func(a, b metric.DivergenceCandidate) int { return metric.CompareTimestamps(a.TS, b.TS) })
	if len(discarded) > 5 {
		discarded = discarded[len(discarded)-5:]
	}
	lines := []string{
		fmt.Sprintf("[crw — objective plateau] You are mid-cycle at %s (%s) with an active maximize goal.", phase, stageLabel(phase)),
		fmt.Sprintf("The latest %d %s metric value(s) are non-improving: %s.", stopPlateauMetricRecords, name, values),
		"Step back and re-plan with divergence: record at least two grounded candidate approaches, choose the collapse point, then continue PABCD.",
		"Do not ask the user while the goal is active; record assumptions or an unresolved-tie note for later review.",
	}
	if streak.ChangeClass != "" && streak.Length >= 3 {
		lines = append([]string{fmt.Sprintf("FORBIDDEN: another %s candidate — %d consecutive %s candidates were discarded. Your next candidates MUST be state-space-redesign or evaluator-change, or the next work-phase MUST target the evaluation gate itself (LOOP-PHASE-DEATH-01 / GATE-ORACLE-VALIDITY-01).", streak.ChangeClass, streak.Length, streak.ChangeClass)}, lines...)
	}
	if len(discarded) > 0 {
		lines = append(lines, "Recent discarded candidates:")
		for _, c := range discarded {
			class := string(c.ChangeClass)
			if class == "" {
				class = "unclassified"
			}
			lines = append(lines, c.Title+" ["+class+"]")
		}
	}
	lines = append(lines, "Anchor rule (LOOP-CANDIDATE-ANCHOR-01): source candidates from domain-state evidence (logs, trajectories, instance analysis) — a candidate list of threshold/guard tweaks on existing levers is parameter-space anchoring; regenerate. Quote the previous cycle's D conclusion before proposing (LOOP-CONTINUITY-01). Record each candidate WITH its changeClass. Check whether evaluation instances are fixed/enumerable (LOOP-INSTANCE-CHECK-01).")
	reason := strings.Join(lines, "\n")
	// Port deviation (known-defects CRW-192): the oracle returns this block before it appends the render advisory, so the soft
	// warning of the same Stop was lost exactly when the model is told to re-plan; it follows the block like every other reason.
	if advisory != "" {
		reason += "\n\n" + advisory
	}
	return stopEnvelope(reason)
}

// stopNumberText is a finite number as JavaScript prints it in a template literal.
func stopNumberText(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	case f == 0:
		return "0"
	}
	text, err := json.Marshal(f)
	if err != nil {
		return fmt.Sprint(f)
	}
	return string(text)
}

// stopRenderAdvisory is renderGroundingAdvisoryForStop (hook.ts:1899-1917): at phase C, the native-surface
// advisory and the render-artifact advisory, joined by a blank line. Both fail open to "".
func stopRenderAdvisory(cwd string, phase state.Phase, sessionID, boundSlug string) (advisory string) {
	if phase != state.PhaseC {
		return ""
	}
	defer func() {
		if recover() != nil {
			advisory = ""
		}
	}()
	var parts []string
	if plan := stopSafeReadBoundGoalplan(cwd, boundSlug); plan != nil {
		if ids := stopPresentedNativeCriteria(plan); len(ids) > 0 && !NativeObservationLedgerMalformed(cwd) && !HasNativeObservation(cwd, sessionID) {
			parts = append(parts, "[crw advisory — D5.2] The active desktop criteria "+strings.Join(ids, ", ")+
				" declare presented: \"native\", but no native-observation row was recorded for this session."+
				" Before C->D, inspect the native app with computer-use or record a declared QA screenshot"+
				" so the native surface has an explicit observation signal. This is a soft advisory and does not block the turn.")
		}
	}
	if HasRenderArtifactModified(cwd, sessionID) && !HasRenderObservation(cwd, sessionID) {
		parts = append(parts, RenderGroundingAdvisory())
	}
	return strings.Join(parts, "\n\n")
}

// stopPresentedNativeCriteria is relevantPresentedNativeCriteria (hook.ts:1876-1886): the ids of the open
// desktop criteria declared presented "native" that the active work phase links (all, when it links none).
func stopPresentedNativeCriteria(plan *goalplan.Goalplan) []string {
	var linked map[string]bool
	if id := goalplan.EffectiveActiveWorkPhaseID(plan); id != nil {
		for _, wp := range plan.WorkPhases {
			if wp.ID == *id {
				if len(wp.CriteriaIDs) > 0 {
					linked = map[string]bool{}
					for _, c := range wp.CriteriaIDs {
						linked[c] = true
					}
				}
				break
			}
		}
	}
	var ids []string
	for _, c := range plan.Criteria {
		if (linked == nil || linked[c.ID]) && c.Status == goalplan.CriterionOpen && c.Surface == goalplan.SurfaceDesktop && c.Presented == goalplan.PresentedNative {
			ids = append(ids, c.ID)
		}
	}
	return ids
}
