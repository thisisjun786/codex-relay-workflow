package goalplan

// CXC v0.2.40 goalplan.ts:1031-1235 (commit 3c1459ac). These queries do no IO,
// mutate no record and append no ledger. Returned record pointers refer to the
// input's slices and remain valid while callers keep those backing arrays.
import (
	"fmt"
	"slices"
	"strings"

	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// RemainingWorkPhases keeps blocked work and omits done or superseded work.
func RemainingWorkPhases(plan *Goalplan) []*GoalplanWorkPhase {
	out := []*GoalplanWorkPhase{}
	for i := range plan.WorkPhases {
		p := &plan.WorkPhases[i]
		if p.Status != WorkPhaseDone && p.Status != WorkPhaseSuperseded {
			out = append(out, p)
		}
	}
	return out
}
func queryFindWorkPhase(plan *Goalplan, id string) *GoalplanWorkPhase {
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == id {
			return &plan.WorkPhases[i]
		}
	}
	return nil
}
func queryFindTask(phase *GoalplanWorkPhase, id string) *GoalplanTask {
	for i := range phase.Tasks {
		if phase.Tasks[i].ID == id {
			return &phase.Tasks[i]
		}
	}
	return nil
}

// WorkPhaseDependenciesMet checks direct targets only; the first matching ID wins.
func WorkPhaseDependenciesMet(plan *Goalplan, phase *GoalplanWorkPhase) bool {
	for _, id := range phase.DependsOn {
		p := queryFindWorkPhase(plan, id)
		if p == nil || p.Status != WorkPhaseDone {
			return false
		}
	}
	return true
}

// TaskDependenciesMet resolves task IDs only inside this phase.
func TaskDependenciesMet(phase *GoalplanWorkPhase, task *GoalplanTask) bool {
	for _, id := range task.DependsOn {
		t := queryFindTask(phase, id)
		if t == nil || t.Status != TaskDone {
			return false
		}
	}
	return true
}
func queryDecisionMatches(plan *Goalplan, id string) (count int, status DecisionStatus) {
	for _, d := range plan.Decisions {
		if d.ID == id {
			count++
			status = d.Status
		}
	}
	return
}

// OpenDecisionIDsForPhase releases only unique decided targets. Missing and
// duplicate decision IDs stay waiting; references are deduplicated in order.
func OpenDecisionIDsForPhase(plan *Goalplan, phase *GoalplanWorkPhase) []string {
	out := []string{}
	for _, id := range uniqueDependencyIDs(phase.AwaitsDecision) {
		count, status := queryDecisionMatches(plan, id)
		if count != 1 || status != DecisionDecided {
			out = append(out, id)
		}
	}
	return out
}

// WorkPhaseReadyConditionsMet combines direct dependencies and decision answers.
func WorkPhaseReadyConditionsMet(plan *Goalplan, phase *GoalplanWorkPhase) bool {
	return WorkPhaseDependenciesMet(plan, phase) && len(OpenDecisionIDsForPhase(plan, phase)) == 0
}

// IsRunnablePhase admits pending and in-progress phases with ready conditions.
func IsRunnablePhase(plan *Goalplan, phase *GoalplanWorkPhase) bool {
	return (phase.Status == WorkPhasePending || phase.Status == WorkPhaseInProgress) && WorkPhaseReadyConditionsMet(plan, phase)
}

// ReadyWorkPhases returns runnable phases in declared order.
func ReadyWorkPhases(plan *Goalplan) []*GoalplanWorkPhase {
	out := []*GoalplanWorkPhase{}
	for i := range plan.WorkPhases {
		p := &plan.WorkPhases[i]
		if IsRunnablePhase(plan, p) {
			out = append(out, p)
		}
	}
	return out
}

// ReadyGoalplanTask identifies a ready task in its work phase.
type ReadyGoalplanTask struct {
	WorkPhaseID string        `json:"workPhaseId"`
	Task        *GoalplanTask `json:"task"`
}

// ReadyTasks preserves phase order and each phase's task order.
func ReadyTasks(plan *Goalplan) []ReadyGoalplanTask {
	out := []ReadyGoalplanTask{}
	for _, p := range ReadyWorkPhases(plan) {
		for i := range p.Tasks {
			t := &p.Tasks[i]
			if t.Status == TaskPending && TaskDependenciesMet(p, t) {
				out = append(out, ReadyGoalplanTask{p.ID, t})
			}
		}
	}
	return out
}

// NextOpenGoalplanTask is the nullable nextOpenTask result shape.
type NextOpenGoalplanTask struct {
	WP   *GoalplanWorkPhase `json:"wp"`
	Task *GoalplanTask      `json:"task"`
}

// NextOpenTask re-finds the phase by ID, including the oracle's first-match
// pairing when duplicate phase IDs associate a later task with an earlier phase.
func NextOpenTask(plan *Goalplan) *NextOpenGoalplanTask {
	ready := ReadyTasks(plan)
	if len(ready) == 0 {
		return nil
	}
	next := ready[0]
	if p := queryFindWorkPhase(plan, next.WorkPhaseID); p != nil {
		return &NextOpenGoalplanTask{p, next.Task}
	}
	return nil
}

// DependencyDeadlock is derived diagnosis, never persisted by these queries.
type DependencyDeadlock struct {
	Reasons []string `json:"reasons"`
}

func queryDescribePhaseDependency(plan *Goalplan, id string) string {
	status := "missing"
	if p := queryFindWorkPhase(plan, id); p != nil {
		status = string(p.Status)
	}
	return fmt.Sprintf("work-phase %s (%s)", id, status)
}
func queryDescribeTaskDependency(phase *GoalplanWorkPhase, id string) string {
	status := "missing"
	if t := queryFindTask(phase, id); t != nil {
		status = string(t.Status)
	}
	return fmt.Sprintf("task %s/%s (%s)", phase.ID, id, status)
}
func queryDependencyWaitReason(subject string, dependencies []string) string {
	return subject + " waits for " + strings.Join(dependencies, ", ")
}
func queryUnmetPhaseDependencyIDs(plan *Goalplan, phase *GoalplanWorkPhase) []string {
	out := []string{}
	for _, id := range uniqueDependencyIDs(phase.DependsOn) {
		p := queryFindWorkPhase(plan, id)
		if p == nil || p.Status != WorkPhaseDone {
			out = append(out, id)
		}
	}
	return out
}
func queryUnmetTaskDependencyIDs(phase *GoalplanWorkPhase, task *GoalplanTask) []string {
	out := []string{}
	for _, id := range uniqueDependencyIDs(task.DependsOn) {
		t := queryFindTask(phase, id)
		if t == nil || t.Status != TaskDone {
			out = append(out, id)
		}
	}
	return out
}
func queryPhaseWaitReason(plan *Goalplan, phase *GoalplanWorkPhase, ids []string) string {
	descriptions := []string{}
	for _, id := range ids {
		descriptions = append(descriptions, queryDescribePhaseDependency(plan, id))
	}
	return queryDependencyWaitReason("work-phase "+phase.ID, descriptions)
}
func queryDecisionWaitReasons(plan *Goalplan, phase *GoalplanWorkPhase) []string {
	ids := OpenDecisionIDsForPhase(plan, phase)
	if len(ids) == 0 {
		return []string{}
	}
	return []string{"work-phase " + phase.ID + " awaits decision " + strings.Join(ids, ", ")}
}
func queryTaskWaitReasons(phase *GoalplanWorkPhase) []string {
	out := []string{}
	for i := range phase.Tasks {
		task := &phase.Tasks[i]
		if task.Status != TaskPending {
			continue
		}
		ids := queryUnmetTaskDependencyIDs(phase, task)
		if len(ids) == 0 {
			continue
		}
		descriptions := []string{}
		for _, id := range ids {
			descriptions = append(descriptions, queryDescribeTaskDependency(phase, id))
		}
		out = append(out, queryDependencyWaitReason("task "+phase.ID+"/"+task.ID, descriptions))
	}
	return out
}

// DependencyWaitReasons reports direct waits even when independent work is ready.
func DependencyWaitReasons(plan *Goalplan) []string {
	out := []string{}
	for _, p := range RemainingWorkPhases(plan) {
		if ids := queryUnmetPhaseDependencyIDs(plan, p); len(ids) > 0 {
			out = append(out, queryPhaseWaitReason(plan, p, ids))
		}
		out = append(out, queryDecisionWaitReasons(plan, p)...)
		if p.Status != WorkPhasePending && p.Status != WorkPhaseInProgress {
			continue
		}
		out = append(out, queryTaskWaitReasons(p)...)
	}
	return out
}

// DetectDependencyDeadlock is dependencyDeadlock; its verb avoids a Go name
// collision with the DependencyDeadlock result type. Empty runnable phases are closable.
func DetectDependencyDeadlock(plan *Goalplan) *DependencyDeadlock {
	remaining := RemainingWorkPhases(plan)
	if len(remaining) == 0 {
		return nil
	}
	for _, p := range ReadyWorkPhases(plan) {
		allDone := true
		for i := range p.Tasks {
			t := &p.Tasks[i]
			if t.Status == TaskPending && TaskDependenciesMet(p, t) {
				return nil
			}
			if t.Status != TaskDone {
				allDone = false
			}
		}
		if allDone {
			return nil
		}
	}
	out := []string{}
	for _, p := range remaining {
		if p.Status == WorkPhaseBlocked {
			reason := "work-phase " + p.ID + " is blocked"
			if p.BlockedReason != nil && *p.BlockedReason != "" {
				reason += " (" + *p.BlockedReason + ")"
			}
			out = append(out, reason)
			out = append(out, queryDecisionWaitReasons(plan, p)...)
			continue
		}
		ids := queryUnmetPhaseDependencyIDs(plan, p)
		if len(ids) > 0 {
			out = append(out, queryPhaseWaitReason(plan, p, ids))
		}
		out = append(out, queryDecisionWaitReasons(plan, p)...)
		if len(ids) > 0 {
			continue
		}
		out = append(out, queryTaskWaitReasons(p)...)
	}
	if len(out) == 0 {
		return nil
	}
	return &DependencyDeadlock{out}
}

// queryStructuralValid composes goalplan.ts:1744-1757 for its boolean-only
// consumer, reusing the existing checks. The later full structural validator
// owns diagnostic strings; no substitute validator API is introduced here.
func queryStructuralValid(plan *Goalplan) bool {
	if plan.SchemaVersion != nil && *plan.SchemaVersion > SupportedMaxSchemaVersion {
		return false
	}
	if len(GoalplanDefinitionIntegrityReasons(plan)) > 0 || len(GoalplanDependencyCompletionReasons(plan)) > 0 {
		return false
	}
	for _, c := range plan.Criteria {
		if c.Status == CriterionMet && (c.CapturedEvidence == nil || jstext.Trim(*c.CapturedEvidence) == "") {
			return false
		}
	}
	for _, p := range plan.WorkPhases {
		if p.Status == WorkPhaseDone {
			for _, t := range p.Tasks {
				if t.Status != TaskDone {
					return false
				}
			}
		}
	}
	return len(supersededIntegrityReasons(plan)) == 0
}

// RemainingWorkAwaitsDecisions permits yielding only when actual open decisions
// account for every remaining phase and every unmet criterion in a sound plan.
func RemainingWorkAwaitsDecisions(plan *Goalplan) bool {
	if !queryStructuralValid(plan) {
		return false
	}
	remaining := RemainingWorkPhases(plan)
	if len(remaining) == 0 {
		return false
	}
	byID := map[string]*GoalplanWorkPhase{}
	for i := range plan.WorkPhases {
		p := &plan.WorkPhases[i]
		byID[p.ID] = p
	} // Map construction is last-wins, unlike .find.
	var waiting func(*GoalplanWorkPhase, map[string]bool) bool
	waiting = func(p *GoalplanWorkPhase, visiting map[string]bool) bool {
		if p.Status != WorkPhasePending && p.Status != WorkPhaseInProgress {
			return false
		}
		for _, id := range p.AwaitsDecision {
			count, status := queryDecisionMatches(plan, id)
			if count == 1 && status == DecisionOpen {
				return true
			}
		}
		if visiting[p.ID] {
			return false
		}
		visiting[p.ID] = true
		defer delete(visiting, p.ID)
		for _, id := range p.DependsOn {
			if dependency := byID[id]; dependency != nil && dependency.Status != WorkPhaseDone && waiting(dependency, visiting) {
				return true
			}
		}
		return false
	}
	for _, p := range remaining {
		if !waiting(p, map[string]bool{}) {
			return false
		}
	}
	for _, c := range plan.Criteria {
		if c.Status != CriterionOpen {
			continue
		}
		covered := false
		for _, p := range remaining {
			if slices.Contains(p.CriteriaIDs, c.ID) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}
