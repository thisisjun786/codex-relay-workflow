package interview

// Red-first stub of the freeze port (CRW-437): the types and constants are final, the behaviour is not written yet.

const (
	PlanSubdir         = "plan"
	FreezeManifestDir  = "interview"
	FreezeManifestFile = "freeze.json"
)

// PlanFileHash is one plan file: its path relative to the plan slug directory and the sha256 of its content.
type PlanFileHash struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// EvidenceBundle is the structured interview evidence carried into the goal handoff.
type EvidenceBundle struct {
	Dimensions         *Dimensions     `json:"dimensions"`
	OpenAssumptions    []string        `json:"openAssumptions"`
	Contradictions     []Contradiction `json:"contradictions"`
	AcceptanceCriteria []string        `json:"acceptanceCriteria"`
	ResearchReportRef  *string         `json:"researchReportRef"`
}

// FreezeManifest is the content of .crw/interview/freeze.json.
type FreezeManifest struct {
	FrozenAt       string         `json:"frozenAt"`
	PlanFiles      []PlanFileHash `json:"planFiles"`
	PlanHash       string         `json:"planHash"`
	Objective      string         `json:"objective"`
	Slug           string         `json:"slug"`
	EvidenceBundle EvidenceBundle `json:"evidenceBundle"`
}

// BuildManifestInput is what BuildFreezeManifest shapes into a manifest; Now defaults to the current time.
type BuildManifestInput struct {
	Objective      string
	PlanFiles      []PlanFileHash
	EvidenceBundle EvidenceBundle
	Now            func() string
}

// StaleCheckResult is the verdict of CheckStale.
type StaleCheckResult struct {
	Stale        bool     `json:"stale"`
	ChangedFiles []string `json:"changedFiles"`
	Reason       string   `json:"reason"`
}

func Sha256(content string) string { return "" }

func ComputePlanHash(files []PlanFileHash) string { return "" }

func DeriveSlug(objective string) string { return "" }

func BuildFreezeManifest(in BuildManifestInput) FreezeManifest { return FreezeManifest{} }

func CheckStale(manifest FreezeManifest, current []PlanFileHash) StaleCheckResult {
	return StaleCheckResult{}
}

func localeCompare(a, b string) int { return 0 }

// GoalActivationDirective is GOAL_ACTIVATION_DIRECTIVE under CRW's names.
const GoalActivationDirective = "[crw: FREEZE -> goal handoff]\n" +
	"Interview is ready and the plan is frozen. To start execution under a native goal:\n" +
	"1. Call get_goal to confirm no goal is already active for this thread.\n" +
	"2. Call create_goal with objective ONLY (no token_budget \u2014 the L3 gate denies budgeted goals).\n" +
	"3. Verify a goal row was actually created (codex owns goal lifecycle in goals_1.sqlite).\n" +
	"The frozen plan under .crw/plan/ is the READ-ONLY spec the goal consumes; do not reopen\n" +
	"Interview once the goal is active (L11 hard-deny). If create_goal fails, report that goal mode\n" +
	"did not start \u2014 do not proceed as if it did. On goal start, recompute planHash and compare to\n" +
	".crw/interview/freeze.json; on mismatch, re-freeze the current plan before proceeding."
