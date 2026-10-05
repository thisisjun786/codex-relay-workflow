package goalplan

// Red-first stub: the declarations below are the API the tests in lifecycle_test.go
// drive; the ported bodies and their oracle line references arrive in the next commit
// (goalplan.ts:1236-1445, commit 3c1459ac). Bodies return zero values on purpose.

// GoalplanLifecycleKind is what a lifecycle operation answered.
type GoalplanLifecycleKind string

// The kinds of a lifecycle answer.
const (
	GoalplanLifecycleChanged   GoalplanLifecycleKind = "changed"
	GoalplanLifecycleUnchanged GoalplanLifecycleKind = "unchanged"
	GoalplanLifecycleRejected  GoalplanLifecycleKind = "rejected"
)

// GoalplanLifecycleResult is goalplan.ts's GoalplanLifecycleResult: changed carries
// the new plan, unchanged the same plan and why it stands, rejected only a reason.
type GoalplanLifecycleResult struct {
	Kind   GoalplanLifecycleKind `json:"kind"`
	Plan   *Goalplan             `json:"plan,omitempty"`
	Reason string                `json:"reason,omitempty"`
}

// AskGoalplanDecisionInput is one question put to a plan.
type AskGoalplanDecisionInput struct {
	ID             string
	Question       string
	Recommendation *string
	Options        []string
	WorkPhaseIDs   []string
	AskedAt        string
}

// AddGoalplanTaskInput is one task added to a work phase.
type AddGoalplanTaskInput struct {
	ID        string
	Title     string
	DependsOn []string
}

func AskGoalplanDecision(*Goalplan, AskGoalplanDecisionInput) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{}
}

func DecideGoalplanDecision(*Goalplan, string, string, string) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{}
}

func AddGoalplanTask(*Goalplan, string, AddGoalplanTaskInput) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{}
}

func CompleteGoalplanTask(*Goalplan, string, string, string) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{}
}

func MeetGoalplanCriterion(*Goalplan, string, string) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{}
}

func UnmetCriteria(*Goalplan) []*GoalplanCriterion { return nil }

func DoneWorkPhasesWithPendingTasks(*Goalplan) []*GoalplanWorkPhase { return nil }

func IsGoalplanComplete(*Goalplan) bool { return false }
