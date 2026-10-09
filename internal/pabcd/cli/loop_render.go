// Loop plan renderers and the loop help text: goalplan-cli.ts (:297, :679-750) at
// v0.2.40. Command registration and the loop verbs are CRW-646; this file ports only
// the renderers those verbs and the parser share.
package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// RenderLoopPlan is goalplan-cli.ts renderPlan (:297-299): the plan summary a loop verb
// prints. The optional lock status is the writeLock line show adds.
func RenderLoopPlan(plan *goalplan.Goalplan, lock *goalplan.GoalplanLockStatus) string {
	return RenderLoopPlanLines(plan, lock)
}

// RenderLoopPlanLines is renderPlanLines (:679-710). The line order is the oracle's:
// banner, objective, host, the work-phase and criteria counts, complete, the optional
// writeLock line, then one line per work phase, per criterion and per open decision
// (the decision's options and the phases waiting on it included), and last the port's
// own lines for the annotate notes steering batches recorded (CRW-1111).
func RenderLoopPlanLines(plan *goalplan.Goalplan, lock *goalplan.GoalplanLockStatus) string {
	lines := []string{
		"[crw loop: " + plan.Slug + "]",
		"objective: " + plan.Objective,
		fmt.Sprintf("host: armed=%t source=%s", plan.Host.Armed, plan.Host.Source),
		fmt.Sprintf("workPhases: %d (remaining %d)", len(plan.WorkPhases), len(goalplan.RemainingWorkPhases(plan))),
		fmt.Sprintf("criteria: %d (unmet %d)", len(plan.Criteria), len(goalplan.UnmetCriteria(plan))),
		fmt.Sprintf("complete: %t", goalplan.IsGoalplanComplete(plan)),
	}
	if lock != nil {
		// A stuck lock used to be invisible from the CLI; the age tells a live holder
		// from an abandoned one. A present lock without a recorded age prints "null".
		if lock.Exists {
			age := "null"
			if lock.AgeMs != nil {
				age = metricCliNumberText(*lock.AgeMs)
			}
			lines = append(lines, fmt.Sprintf("writeLock: present path=%s ageMs=%s", lock.Path, age))
		} else {
			lines = append(lines, "writeLock: absent path="+lock.Path)
		}
	}
	for i := range plan.WorkPhases {
		wp := &plan.WorkPhases[i]
		lines = append(lines, fmt.Sprintf("  - %s [%s] %s", wp.ID, wp.Status, wp.Title))
	}
	for i := range plan.Criteria {
		c := &plan.Criteria[i]
		lines = append(lines, fmt.Sprintf("  - %s [%s] %s", c.ID, c.Status, c.Scenario))
	}
	for i := range plan.Decisions {
		decision := &plan.Decisions[i]
		if decision.Status != goalplan.DecisionOpen {
			continue
		}
		lines = append(lines, fmt.Sprintf("  - %s [open] %s", decision.ID, decision.Question))
		if decision.Options != nil {
			suffix := ""
			if decision.Recommendation != "" {
				suffix = " (recommended: " + decision.Recommendation + ")"
			}
			lines = append(lines, "    options: "+strings.Join(decision.Options, " | ")+suffix)
		}
		waiting := []string{}
		for j := range plan.WorkPhases {
			if slices.Contains(plan.WorkPhases[j].AwaitsDecision, decision.ID) {
				waiting = append(waiting, plan.WorkPhases[j].ID)
			}
		}
		joined := strings.Join(waiting, ", ")
		if joined == "" {
			joined = "none"
		}
		lines = append(lines, "    waiting: "+joined)
	}
	// CRW-1111: an annotate note a steering batch recorded is part of the plan's record, so show lists it
	// with the key of the batch that carried it. The oracle drops the note when it applies the batch.
	for i := range plan.SteeringLog {
		entry := &plan.SteeringLog[i]
		for j := range entry.Ops {
			if entry.Ops[j].Kind == goalplan.SteerOpAnnotate {
				lines = append(lines, "  - note "+entry.IdempotencyKey+": "+entry.Ops[j].Note)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// RenderLoopHelp is renderGoalplanHelp (:712-750): the loop usage block in crw names.
// The text is copied verbatim from the oracle after the repository's name substitution
// (cxc -> crw, [codexclaw -> [crw, .codexclaw -> .crw, cxc loop -> crw pabcd loop); the
// replayer's own table produces the same bytes, which TestLoopHelpOracle checks.
func RenderLoopHelp() string { return loopHelp }

const loopHelp = `crw pabcd loop — durable goalplan for a multi-cycle PABCD loop

Usage:
  crw pabcd loop init --objective <text> [--session <id>] [--criterion <text>]... [--schema-version <n>] [--cwd <path>]
  crw pabcd loop show (--slug <slug> | --objective <text> | --session <id>) [--cwd <path>]
  crw pabcd loop validate (--slug <slug> | --objective <text> | --session <id>) [--cwd <path>]
  crw pabcd loop steer --session <id> --batch-json <path-or-json> [--cwd <path>]
  crw pabcd loop add-criterion --session <id> --criterion <text> [--surface logic|web|tui|desktop] [--presented native] [--cwd <path>]
  crw pabcd loop add-work-phase --session <id> --id <id> --title <text> [--depends-on <id>]... [--cwd <path>]
  crw pabcd loop ready (--slug <slug> | --objective <text> | --session <id>) [--json] [--cwd <path>]
  crw pabcd loop add-task --session <id> --work-phase <id> --id <id> --title <text> [--depends-on <task-id>]... [--cwd <path>]
  crw pabcd loop complete-task --session <id> --work-phase <id> --id <id> --outcome <text> [--cwd <path>]
  crw pabcd loop meet-criterion --session <id> --id <id> --evidence <text> [--cwd <path>]
  crw pabcd loop ask --session <id> --id <id> --question <text> [--recommendation <text>] [--option <text>]... [--work-phase <id>]... [--cwd <path>]
  crw pabcd loop decide --session <id> --id <id> --answer <text> [--cwd <path>]
  crw pabcd loop --help

Notes:
  Mutating verbs require --session <id>; show, validate, and ready are read-only.
  Unknown flags, stray positionals, missing values, and flags on the wrong verb are rejected before dispatch.
  Every value flag also accepts --flag=value; use it for a value that starts with --.
  The goalplan lives at <cwd>/.crw/goalplans/<slug>/goalplan.json, so --cwd
  matters when the process cwd is not the workspace you are planning in.
  Repeat --depends-on once per prerequisite; add-task accepts only existing task ids
  from the same work phase; comma-separated values are one id.
  complete-task requires non-empty outcome evidence and never replaces a stored outcome.
  init declares schemaVersion 1 unless --schema-version says otherwise. 2 and 3
  additionally require an approved finalGate, and no verb in this build opens a
  final-gate review round, so opt in only if you can record that gate yourself.
  meet-criterion requires non-empty captured evidence for the same reason.
  Send the question through the host first, then record it with ask; ask never sends a message.
  Record the user's reply with decide. It changes only the decision record.
  Repeat --option once per offered option; the recommendation must be one of them, and the answer stays free text.

steer --batch-json expects an object with:
  { "idempotencyKey": "<unique>", "rationale": "<why>", "evidence": "<proof>",
    "ops": [ { "kind": "annotate", "note": "..." } ] }
  op kinds: annotate | add-criterion | add-work-phase (all additive — steering
  cannot weaken a completion criterion).`
