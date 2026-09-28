// Package cxc ports the public CXC vocabulary and validation seam, not its lifecycle.
package cxc

const (
	Package             = "codexclaw"
	Version             = "0.2.28+codex.20260914090142"
	Done                = "DONE"
	Noop                = "NOOP"
	Blocked             = "BLOCKED"
	Unsafe              = "UNSAFE"
	NeedsHuman          = "NEEDS_HUMAN"
	BudgetExhausted     = "BUDGET_EXHAUSTED"
	Pass                = "PASS"
	GoWithFixes         = "GO-WITH-FIXES"
	Fail                = "FAIL"
	Prefix              = "VERDICT: "
	BlockersMax         = 9999
	Progress            = "progress"
	SuspectedStagnation = "suspected_stagnation"
	ConfirmedFailure    = "confirmed_failure"
	Unobservable        = "unobservable"
	InputNeeded         = "input_needed"
	TimedOut            = "timed_out"
	DecisionBoundary    = "DECISION BOUNDARY"
)

type Source struct {
	Rule   string `json:"rule"`
	Path   string `json:"path"`
	Anchor string `json:"anchor"`
	SHA256 string `json:"sha256"`
}

var Sources = []Source{
	{"report-outcomes-are-not-phases", "skills/loop/SKILL.md", "146-148", "61167d152d3c84f01b4d05ccb7ded5457db07ec42a84ae85e911eb75a44e5e2b"},
	{"terminal-state-vocabulary", "skills/pabcd/references/loop-engineering.md", "32-40", "c5fb8c4e9fdd7dd9ce675a712cc9eca5b8e21301e8bfe49f7e27a9185db67fc7"},
	{"REVIEW-SYNTHESIS-01", "skills/pabcd/references/loop-engineering.md", "41-60", "c5fb8c4e9fdd7dd9ce675a712cc9eca5b8e21301e8bfe49f7e27a9185db67fc7"},
	{"DISPATCH-TASK-01", "skills/pabcd/references/delegation.md", "20-22", "ba773dbd5bbe3fd975bfca9f149205907ec9ca4c5a24bd2ce3c86d52d0c578b2"},
	{"plan-output-nine-concepts", "skills/pabcd/references/plan-output.md", "7-20", "4535762ade768fad5aafb121e9c3b44b7cf6df17eb6b2a04cbfaa2dc366c7868"},
	{"REVIEW-OUTPUT-01", "skills/dev-code-reviewer/SKILL.md", "86-95", "1d66f37c7754d852931bcf958063f30b2ab261a583ec080d280daf912e5d2d28"},
	{"ATTEST-EVIDENCE-01", "skills/pabcd/references/phase-control.md", "45-48", "6982abddc1ceb52d046403b2a16a01ea22fab3fc30900d20c26a04a35a8b476a"},
	{"LOOP-WAIT-EVIDENCE-01", "skills/loop/references/waiting.md", "31-63", "988451dee609d3949fa21f68fb2453a210f8fbbd06819db4219e710ab15150c8"},
	{"DISPATCH-SURFACE-01", "skills/pabcd/references/dispatch-surfaces.md", "10-20", "dd3e6721718f05a45ac0031d861823441bba15592640dbe7785cae6611c26e98"},
}
var ReportStatuses = []string{Done, Noop, Blocked, Unsafe, NeedsHuman, BudgetExhausted}
var Meaning = map[string]string{
	Done:            "the child proved every recorded criterion against its own work",
	Noop:            "nothing needed doing, and the finding that established that is the deliverable",
	Blocked:         "an external dependency is in the way",
	Unsafe:          "a human risk decision is required before this can proceed",
	NeedsHuman:      "a judgment only the user can make",
	BudgetExhausted: "a bound the plan actually stated ran out; best-so-far is adopted",
}
var CompatibleOutcomes = map[string][]string{
	Done: {"ready_for_review"}, Noop: {"ready_for_review"},
	Blocked: {"blocked_needs_input"}, Unsafe: {"blocked_needs_input"}, NeedsHuman: {"blocked_needs_input"},
	BudgetExhausted: {"interrupted", "failed"},
}
var NotVerification = map[string]string{
	"cxc_done":              "a DONE report is the child proving its own criteria, not the parent's verdict",
	"cxc_report":            "a child report is the child describing its own execution, not a verdict",
	"pull_request_opened":   "an open pull request is a place to review, not a review",
	"review_pass":           "a review PASS is one reviewer's judgment, not the parent's disposition",
	"required_checks_green": "a green required check is evidence for a verdict, not a verdict",
	"turn_completed":        "a completed turn is the trigger to look, as protocol v1 section 2 says",
	"dispatched":            "a dispatched delivery is not an acknowledgement and not a verification",
}
var VerdictKinds = []string{Pass, GoWithFixes, Fail}
var WaitStates = []string{Progress, SuspectedStagnation, ConfirmedFailure, Unobservable, InputNeeded, TimedOut}
var NeverAuthorisesRerun = []string{TimedOut, SuspectedStagnation, Unobservable, InputNeeded}
var DispatchFields = []string{"TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT"}
var DispatchSections = []string{"TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT", DecisionBoundary}
var CorrectionSections = []string{"VIOLATED CRITERION", "WHAT CHANGED", "FIX SCOPE", "PRESERVE", "REVERIFY AND RETURN"}
var SkillPointers = map[string]string{
	"loop":          "codexclaw:cxc-loop with codexclaw:cxc-pabcd",
	"development":   "codexclaw:cxc-dev",
	"pull-request":  "codexclaw:cxc-dev references/stacked-prs.md",
	"review-repair": "REVIEW-SYNTHESIS-01, cxc-pabcd references/loop-engineering.md 11.3",
	"lost-context":  "codexclaw:cxc-recall",
}
