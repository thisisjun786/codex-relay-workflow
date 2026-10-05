package goalplan

// CXC v0.2.40 goalplan.ts:1236-1445 (commit 3c1459ac). These functions are pure plan
// transformations: they read a plan and answer a new plan, an unchanged plan with the
// reason it stands, or a refusal. They do no IO and append no ledger; callers publish a
// changed plan through WriteGoalplan under the write lock. The validation order and
// every reason text are the oracle's, including its quirks around duplicate ids.
import (
	"fmt"
	"slices"
	"strings"

	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// GoalplanLifecycleKind is what a lifecycle operation answered.
type GoalplanLifecycleKind string

// The kinds of a lifecycle answer.
const (
	GoalplanLifecycleChanged   GoalplanLifecycleKind = "changed"
	GoalplanLifecycleUnchanged GoalplanLifecycleKind = "unchanged"
	GoalplanLifecycleRejected  GoalplanLifecycleKind = "rejected"
)

// GoalplanLifecycleResult is goalplan.ts's GoalplanLifecycleResult union: changed
// carries the new plan, unchanged the very same plan and why it stands, rejected only
// a reason. The JSON names are the oracle's, for the recorded corpus replay.
type GoalplanLifecycleResult struct {
	Kind   GoalplanLifecycleKind `json:"kind"`
	Plan   *Goalplan             `json:"plan,omitempty"`
	Reason string                `json:"reason,omitempty"`
}

// AskGoalplanDecisionInput is one question put to a plan. A nil Recommendation is
// absent and a pointer to "" is present-empty, which the oracle refuses; a nil Options
// is absent and a non-nil empty slice is present-empty, also refused.
type AskGoalplanDecisionInput struct {
	ID             string
	Question       string
	Recommendation *string
	Options        []string
	WorkPhaseIDs   []string
	AskedAt        string
}

// AddGoalplanTaskInput is one task added to a work phase. A nil or empty DependsOn
// stores no dependency field.
type AddGoalplanTaskInput struct {
	ID        string
	Title     string
	DependsOn []string
}

func goalplanLifecycleChanged(plan *Goalplan) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{Kind: GoalplanLifecycleChanged, Plan: plan}
}

func goalplanLifecycleUnchanged(plan *Goalplan, reason string) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{Kind: GoalplanLifecycleUnchanged, Plan: plan, Reason: reason}
}

func goalplanLifecycleRejected(reason string) GoalplanLifecycleResult {
	return GoalplanLifecycleResult{Kind: GoalplanLifecycleRejected, Reason: reason}
}

// AskGoalplanDecision ports askGoalplanDecision (:1239-1286): validate the question,
// its options and the phases that should await it, then append the open decision and
// the awaitsDecision reference and let the definition integrity decide the rest.
func AskGoalplanDecision(plan *Goalplan, input AskGoalplanDecisionInput) GoalplanLifecycleResult {
	id := jstext.Trim(input.ID)
	question := jstext.Trim(input.Question)
	var recommendation *string
	if input.Recommendation != nil {
		trimmed := jstext.Trim(*input.Recommendation)
		recommendation = &trimmed
	}
	workPhaseIDs := make([]string, 0, len(input.WorkPhaseIDs))
	for _, phaseID := range input.WorkPhaseIDs {
		workPhaseIDs = append(workPhaseIDs, jstext.Trim(phaseID))
	}
	if !isLifecycleID(id) {
		return goalplanLifecycleRejected("decision id must be a short lowercase id, e.g. dec-1")
	}
	if question == "" {
		return goalplanLifecycleRejected("decision question must not be empty")
	}
	if recommendation != nil && *recommendation == "" {
		return goalplanLifecycleRejected("decision recommendation must not be empty")
	}
	var options []string
	if input.Options != nil {
		options = make([]string, 0, len(input.Options))
		for _, option := range input.Options {
			options = append(options, jstext.Trim(option))
		}
		if len(options) == 0 {
			return goalplanLifecycleRejected("decision options must not be empty")
		}
		seen := map[string]bool{}
		for _, option := range options {
			if option == "" {
				return goalplanLifecycleRejected("decision options must be non-empty text")
			}
			if seen[option] {
				return goalplanLifecycleRejected(fmt.Sprintf("duplicate decision option '%s'", option))
			}
			seen[option] = true
		}
		if recommendation != nil && !slices.Contains(options, *recommendation) {
			return goalplanLifecycleRejected("decision recommendation must be one of the options")
		}
	}
	if !validIsoTime(input.AskedAt) {
		return goalplanLifecycleRejected("decision askedAt must be an ISO timestamp")
	}
	for i := range plan.Decisions {
		if plan.Decisions[i].ID == id {
			return goalplanLifecycleRejected(fmt.Sprintf("decision '%s' is already in this plan", id))
		}
	}
	for i := range plan.Decisions {
		if plan.Decisions[i].Status == DecisionOpen && jstext.Trim(plan.Decisions[i].Question) == question {
			return goalplanLifecycleRejected(fmt.Sprintf("question is already open as decision '%s'", plan.Decisions[i].ID))
		}
	}
	for _, phaseID := range workPhaseIDs {
		if phaseID == "" {
			return goalplanLifecycleRejected("--work-phase requires distinct non-empty ids")
		}
	}
	if len(uniqueDependencyIDs(workPhaseIDs)) != len(workPhaseIDs) {
		return goalplanLifecycleRejected("--work-phase requires distinct non-empty ids")
	}
	for _, phaseID := range workPhaseIDs {
		phase := queryFindWorkPhase(plan, phaseID)
		if phase == nil {
			return goalplanLifecycleRejected(fmt.Sprintf("work phase '%s' is not in this plan", phaseID))
		}
		if phase.Status == WorkPhaseDone || phase.Status == WorkPhaseSuperseded {
			return goalplanLifecycleRejected(fmt.Sprintf("work phase '%s' is %s and cannot await a decision", phaseID, phase.Status))
		}
	}
	decision := GoalplanDecision{ID: id, Question: question, Status: DecisionOpen, AskedAt: input.AskedAt}
	if recommendation != nil {
		decision.Recommendation = *recommendation
	}
	if options != nil {
		decision.Options = options
	}
	next := *plan
	next.Decisions = append(slices.Clone(plan.Decisions), decision)
	next.WorkPhases = slices.Clone(plan.WorkPhases)
	named := map[string]bool{}
	for _, phaseID := range workPhaseIDs {
		named[phaseID] = true
	}
	for i := range next.WorkPhases {
		if named[next.WorkPhases[i].ID] {
			next.WorkPhases[i].AwaitsDecision = append(slices.Clone(next.WorkPhases[i].AwaitsDecision), id)
		}
	}
	if reasons := GoalplanDefinitionIntegrityReasons(&next); len(reasons) > 0 {
		return goalplanLifecycleRejected(strings.Join(reasons, "; "))
	}
	return goalplanLifecycleChanged(&next)
}

// DecideGoalplanDecision ports decideGoalplanDecision (:1288-1303). A repeated answer
// is unchanged, a different one is a refusal, and the plan is never touched otherwise.
func DecideGoalplanDecision(plan *Goalplan, id string, answer string, decidedAt string) GoalplanLifecycleResult {
	trimmedID := jstext.Trim(id)
	matches := []*GoalplanDecision{}
	for i := range plan.Decisions {
		if plan.Decisions[i].ID == trimmedID {
			matches = append(matches, &plan.Decisions[i])
		}
	}
	if len(matches) == 0 {
		return goalplanLifecycleRejected(fmt.Sprintf("decision '%s' is not in this plan", trimmedID))
	}
	if len(matches) > 1 {
		return goalplanLifecycleRejected(fmt.Sprintf("decision id '%s' is ambiguous (%d entries); repair the plan first", trimmedID, len(matches)))
	}
	decision := matches[0]
	if jstext.Trim(answer) == "" {
		return goalplanLifecycleRejected("decision answer must not be empty")
	}
	if !validIsoTime(decidedAt) {
		return goalplanLifecycleRejected("decision decidedAt must be an ISO timestamp")
	}
	if decision.Status == DecisionDecided {
		if decision.Answer == jstext.Trim(answer) {
			return goalplanLifecycleUnchanged(plan, fmt.Sprintf("decision '%s' is already decided", trimmedID))
		}
		return goalplanLifecycleRejected(fmt.Sprintf("decision '%s' already has a different answer", trimmedID))
	}
	next := *plan
	next.Decisions = slices.Clone(plan.Decisions)
	for i := range next.Decisions {
		if next.Decisions[i].ID == decision.ID {
			next.Decisions[i].Status = DecisionDecided
			next.Decisions[i].Answer = jstext.Trim(answer)
			next.Decisions[i].DecidedAt = decidedAt
		}
	}
	return goalplanLifecycleChanged(&next)
}

// AddGoalplanTask ports addGoalplanTask (:1305-1349): validate the task, find the
// phase among the plan's phases (the append follows every phase of that id, as the
// oracle's map does, and the definition integrity then refuses a duplicated one).
func AddGoalplanTask(plan *Goalplan, workPhaseID string, input AddGoalplanTaskInput) GoalplanLifecycleResult {
	id := jstext.Trim(input.ID)
	title := jstext.Trim(input.Title)
	dependsOn := make([]string, 0, len(input.DependsOn))
	for _, dependencyID := range input.DependsOn {
		dependsOn = append(dependsOn, jstext.Trim(dependencyID))
	}
	if !isLifecycleID(id) {
		return goalplanLifecycleRejected("task id must be a short lowercase id, e.g. t-1")
	}
	if title == "" {
		return goalplanLifecycleRejected("task title must not be empty")
	}
	for _, dependencyID := range dependsOn {
		if dependencyID == "" {
			return goalplanLifecycleRejected("task dependencies must be non-empty task ids")
		}
	}
	if len(uniqueDependencyIDs(dependsOn)) != len(dependsOn) {
		return goalplanLifecycleRejected("task dependencies must not contain duplicate ids")
	}
	target := queryFindWorkPhase(plan, workPhaseID)
	if target == nil {
		return goalplanLifecycleRejected(fmt.Sprintf("work phase '%s' is not in this plan", workPhaseID))
	}
	if target.Status == WorkPhaseDone || target.Status == WorkPhaseSuperseded {
		return goalplanLifecycleRejected(fmt.Sprintf("work phase '%s' is %s and cannot accept a new task", workPhaseID, target.Status))
	}
	for i := range target.Tasks {
		if target.Tasks[i].ID == id {
			return goalplanLifecycleRejected(fmt.Sprintf("task '%s/%s' is already in this work phase", workPhaseID, id))
		}
	}
	task := GoalplanTask{ID: id, Title: title, Status: TaskPending}
	if len(dependsOn) > 0 {
		task.DependsOn = dependsOn
	}
	next := *plan
	next.WorkPhases = slices.Clone(plan.WorkPhases)
	for i := range next.WorkPhases {
		if next.WorkPhases[i].ID == workPhaseID {
			next.WorkPhases[i].Tasks = append(slices.Clone(next.WorkPhases[i].Tasks), task)
		}
	}
	if reasons := GoalplanDefinitionIntegrityReasons(&next); len(reasons) > 0 {
		return goalplanLifecycleRejected(strings.Join(reasons, "; "))
	}
	return goalplanLifecycleChanged(&next)
}

// CompleteGoalplanTask ports completeGoalplanTask (:1351-1382). Readiness is the
// already-ported readyTasks: a task waits for its own dependencies, its phase's
// dependencies and every open decision that phase awaits.
func CompleteGoalplanTask(plan *Goalplan, workPhaseID string, taskID string, outcomeText string) GoalplanLifecycleResult {
	outcome := jstext.Trim(outcomeText)
	if outcome == "" {
		return goalplanLifecycleRejected("task outcome must not be empty")
	}
	var target *GoalplanTask
	if phase := queryFindWorkPhase(plan, workPhaseID); phase != nil {
		target = queryFindTask(phase, taskID)
	}
	if target == nil {
		return goalplanLifecycleRejected(fmt.Sprintf("task '%s/%s' is not in this plan", workPhaseID, taskID))
	}
	if target.Status == TaskDone {
		return goalplanLifecycleUnchanged(plan, fmt.Sprintf("task '%s/%s' is already done", workPhaseID, taskID))
	}
	ready := false
	for _, entry := range ReadyTasks(plan) {
		if entry.WorkPhaseID == workPhaseID && entry.Task.ID == taskID {
			ready = true
			break
		}
	}
	if !ready {
		return goalplanLifecycleRejected(fmt.Sprintf("task '%s/%s' is not ready", workPhaseID, taskID))
	}
	next := *plan
	next.WorkPhases = slices.Clone(plan.WorkPhases)
	for i := range next.WorkPhases {
		if next.WorkPhases[i].ID != workPhaseID {
			continue
		}
		next.WorkPhases[i].Tasks = slices.Clone(next.WorkPhases[i].Tasks)
		for j := range next.WorkPhases[i].Tasks {
			if next.WorkPhases[i].Tasks[j].ID == taskID {
				next.WorkPhases[i].Tasks[j].Status = TaskDone
				next.WorkPhases[i].Tasks[j].Outcome = outcome
			}
		}
	}
	return goalplanLifecycleChanged(&next)
}

// MeetGoalplanCriterion ports meetGoalplanCriterion (:1384-1406): the trimmed evidence
// becomes the captured evidence of every criterion of that id, and a met one stands.
func MeetGoalplanCriterion(plan *Goalplan, criterionID string, evidenceText string) GoalplanLifecycleResult {
	evidence := jstext.Trim(evidenceText)
	if evidence == "" {
		return goalplanLifecycleRejected("criterion evidence must not be empty")
	}
	var target *GoalplanCriterion
	for i := range plan.Criteria {
		if plan.Criteria[i].ID == criterionID {
			target = &plan.Criteria[i]
			break
		}
	}
	if target == nil {
		return goalplanLifecycleRejected(fmt.Sprintf("criterion '%s' is not in this plan", criterionID))
	}
	if target.Status == CriterionMet {
		return goalplanLifecycleUnchanged(plan, fmt.Sprintf("criterion '%s' is already met", criterionID))
	}
	next := *plan
	next.Criteria = slices.Clone(plan.Criteria)
	for i := range next.Criteria {
		if next.Criteria[i].ID == criterionID {
			next.Criteria[i].CapturedEvidence = &evidence
			next.Criteria[i].Status = CriterionMet
		}
	}
	return goalplanLifecycleChanged(&next)
}

// UnmetCriteria is unmetCriteria (:1408-1410): the criteria still open, in order.
func UnmetCriteria(plan *Goalplan) []*GoalplanCriterion {
	out := []*GoalplanCriterion{}
	for i := range plan.Criteria {
		if plan.Criteria[i].Status == CriterionOpen {
			out = append(out, &plan.Criteria[i])
		}
	}
	return out
}

// DoneWorkPhasesWithPendingTasks is doneWorkPhasesWithPendingTasks (:1415-1424):
// phases marked done while still holding a task that is not done
// (CYCLE-COMPLETION-01).
func DoneWorkPhasesWithPendingTasks(plan *Goalplan) []*GoalplanWorkPhase {
	out := []*GoalplanWorkPhase{}
	for i := range plan.WorkPhases {
		phase := &plan.WorkPhases[i]
		if phase.Status != WorkPhaseDone {
			continue
		}
		for j := range phase.Tasks {
			if phase.Tasks[j].Status != TaskDone {
				out = append(out, phase)
				break
			}
		}
	}
	return out
}

// IsGoalplanComplete is isGoalplanComplete (:1426-1437): no remaining work phases, no
// unmet criteria, and no done phase hiding open tasks.
func IsGoalplanComplete(plan *Goalplan) bool {
	return len(RemainingWorkPhases(plan)) == 0 &&
		len(UnmetCriteria(plan)) == 0 &&
		len(DoneWorkPhasesWithPendingTasks(plan)) == 0
}
