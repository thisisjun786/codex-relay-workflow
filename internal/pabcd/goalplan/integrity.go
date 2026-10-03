package goalplan

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Read-only semantic checks from CXC v0.2.40 goalplan.ts:1440-1645,
// 1713-1737,1759-1762 (commit 3c1459ac). Structural revival stays in plan.go.
// Diagnostic order, JS UTF-16 sorting and duplicate-map lookup behavior are kept.
type dependencyNode struct {
	id        string
	dependsOn []string
}

// GoalplanValidation is the result shape of the later full completion validator.
type GoalplanValidation struct {
	OK      bool     `json:"ok"`
	Reasons []string `json:"reasons"`
}

// GoalplanReceiptEvidence is the source/manifest part a validation callback reads.
type GoalplanReceiptEvidence struct {
	SourceIdentity   SourceIdentity
	ArtifactManifest []gate.ArtifactDigest
}

// GoalplanValidationCtx supplies IO to the later validator; these integrity checks
// do not call it. An error is the oracle's receipt-error alternative.
type GoalplanValidationCtx struct {
	Cwd                   string
	CaptureSourceIdentity func(string) SourceIdentity
	CompareSource         func(SourceIdentity, SourceIdentity) source.Comparison
	ReadReceipt           func(string, gate.ReceiptKind) (GoalplanReceiptEvidence, error)
}

// sortedDependencyIDs sorts a copy as JS Array.sort does, by UTF-16 code units.
func sortedDependencyIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
	return out
}

func uniqueDependencyIDs(ids []string) []string {
	out, seen := []string{}, map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

func duplicateIDs(ids []string) []string {
	seen, repeated, out := map[string]bool{}, map[string]bool{}, []string{}
	for _, id := range ids {
		if seen[id] && !repeated[id] {
			out = append(out, id)
			repeated[id] = true
		}
		seen[id] = true
	}
	return sortedDependencyIDs(out)
}

func findDependencyCycle(nodes []dependencyNode) []string {
	byID := map[string]dependencyNode{}
	for _, node := range nodes {
		byID[node.id] = node
	}
	visited, visiting, stack := map[string]bool{}, map[string]int{}, []string{}
	var visit func(string) []string
	visit = func(id string) []string {
		if at, ok := visiting[id]; ok {
			return append(slices.Clone(stack[at:]), id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = len(stack)
		stack = append(stack, id)
		for _, dependencyID := range sortedDependencyIDs(byID[id].dependsOn) {
			if _, exists := byID[dependencyID]; !exists {
				continue
			}
			if cycle := visit(dependencyID); cycle != nil {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	ids := []string{}
	for id := range byID {
		ids = append(ids, id)
	}
	for _, id := range sortedDependencyIDs(ids) {
		if cycle := visit(id); cycle != nil {
			return cycle
		}
	}
	return nil
}

func withoutSelfDependency(id string, dependencies []string) []string {
	out := []string{}
	for _, dependency := range dependencies {
		if dependency != id {
			out = append(out, dependency)
		}
	}
	return out
}

// GoalplanDefinitionIntegrityReasons reports broken references and outcome states,
// without changing the plan. Separate loops preserve the oracle's reason order.
// Outcome != "" models presence for revived and shipped-writer plans. The oracle
// also rejects a directly constructed pending task with outcome: ""; the existing
// Go string field cannot represent that literal separately from absence. Neither
// reviver nor shipped task writer produces it (goalplan.ts:605-607,1336-1341,1357-1376).
func GoalplanDefinitionIntegrityReasons(plan *Goalplan) []string {
	reasons := []string{}
	phaseIDs, phaseNodes, phases := []string{}, []dependencyNode{}, map[string]bool{}
	for _, phase := range plan.WorkPhases {
		phaseIDs = append(phaseIDs, phase.ID)
		phases[phase.ID] = true
		phaseNodes = append(phaseNodes, dependencyNode{phase.ID, withoutSelfDependency(phase.ID, phase.DependsOn)})
	}
	for _, id := range duplicateIDs(phaseIDs) {
		reasons = append(reasons, fmt.Sprintf("duplicate work phase id '%s' makes dependency references ambiguous", id))
	}
	decisionIDs, decisions := []string{}, map[string]GoalplanDecision{}
	for _, decision := range plan.Decisions {
		decisionIDs = append(decisionIDs, decision.ID)
		decisions[decision.ID] = decision
	}
	for _, id := range duplicateIDs(decisionIDs) {
		reasons = append(reasons, fmt.Sprintf("duplicate decision id '%s' makes awaitsDecision references ambiguous", id))
	}
	for _, phase := range plan.WorkPhases {
		for _, id := range duplicateIDs(phase.AwaitsDecision) {
			reasons = append(reasons, fmt.Sprintf("work phase %s awaits decision '%s' more than once", phase.ID, id))
		}
		for _, id := range uniqueDependencyIDs(phase.AwaitsDecision) {
			decision, exists := decisions[id]
			if !exists {
				reasons = append(reasons, fmt.Sprintf("work phase %s awaits unknown decision '%s'", phase.ID, id))
			} else if phase.Status == WorkPhaseDone && decision.Status == DecisionOpen {
				reasons = append(reasons, fmt.Sprintf("work phase %s is done while decision %s is open", phase.ID, id))
			}
		}
	}
	for _, phase := range plan.WorkPhases {
		for _, id := range uniqueDependencyIDs(phase.DependsOn) {
			if id == phase.ID {
				reasons = append(reasons, fmt.Sprintf("work phase %s depends on itself", phase.ID))
			} else if !phases[id] {
				reasons = append(reasons, fmt.Sprintf("work phase %s depends on unknown work phase '%s'", phase.ID, id))
			}
		}
		taskIDs, taskNodes, tasks := []string{}, []dependencyNode{}, map[string]bool{}
		for _, task := range phase.Tasks {
			taskIDs = append(taskIDs, task.ID)
			tasks[task.ID] = true
			taskNodes = append(taskNodes, dependencyNode{task.ID, withoutSelfDependency(task.ID, task.DependsOn)})
		}
		for _, id := range duplicateIDs(taskIDs) {
			reasons = append(reasons, fmt.Sprintf("work phase %s has duplicate task id '%s', so task dependency references are ambiguous", phase.ID, id))
		}
		for _, task := range phase.Tasks {
			for _, id := range uniqueDependencyIDs(task.DependsOn) {
				if id == task.ID {
					reasons = append(reasons, fmt.Sprintf("task %s/%s depends on itself", phase.ID, task.ID))
				} else if !tasks[id] {
					reasons = append(reasons, fmt.Sprintf("task %s/%s depends on unknown task '%s' in the same work phase", phase.ID, task.ID, id))
				}
			}
		}
		if cycle := findDependencyCycle(taskNodes); cycle != nil {
			reasons = append(reasons, fmt.Sprintf("task dependency cycle in work phase %s: %s", phase.ID, strings.Join(cycle, " -> ")))
		}
		if EffectiveSchemaVersion(plan, false) >= 3 {
			for _, task := range phase.Tasks {
				if task.Status == TaskDone && jstext.Trim(task.Outcome) == "" {
					reasons = append(reasons, fmt.Sprintf("task %s/%s is done but has no non-empty outcome", phase.ID, task.ID))
				}
				if task.Status == TaskPending && task.Outcome != "" {
					reasons = append(reasons, fmt.Sprintf("task %s/%s is pending but has outcome", phase.ID, task.ID))
				}
			}
		}
	}
	if cycle := findDependencyCycle(phaseNodes); cycle != nil {
		reasons = append(reasons, "work phase dependency cycle: "+strings.Join(cycle, " -> "))
	}
	criterionIDs, criteria := []string{}, map[string]bool{}
	for _, criterion := range plan.Criteria {
		criterionIDs = append(criterionIDs, criterion.ID)
		criteria[criterion.ID] = true
	}
	for _, id := range duplicateIDs(criterionIDs) {
		reasons = append(reasons, fmt.Sprintf("duplicate criterion id '%s' makes criteriaIds references ambiguous", id))
	}
	for _, phase := range plan.WorkPhases {
		for _, id := range phase.CriteriaIDs {
			if !criteria[id] {
				reasons = append(reasons, fmt.Sprintf("work phase %s references unknown criterion '%s'", phase.ID, id))
			}
		}
	}
	return reasons
}

// GoalplanDependencyCompletionReasons checks done dependents against their own
// authority scope. Unknown targets count as not done; duplicate targets are last-wins.
func GoalplanDependencyCompletionReasons(plan *Goalplan) []string {
	reasons, phases := []string{}, map[string]WorkPhaseStatus{}
	for _, phase := range plan.WorkPhases {
		phases[phase.ID] = phase.Status
	}
	for _, phase := range plan.WorkPhases {
		if phase.Status == WorkPhaseDone {
			open := []string{}
			for _, id := range uniqueDependencyIDs(phase.DependsOn) {
				if phases[id] != WorkPhaseDone {
					open = append(open, id)
				}
			}
			if len(open) > 0 {
				reasons = append(reasons, fmt.Sprintf("work phase %s is done while dependency work phase(s) are not done: %s", phase.ID, strings.Join(open, ", ")))
			}
		}
		tasks := map[string]TaskStatus{}
		for _, task := range phase.Tasks {
			tasks[task.ID] = task.Status
		}
		for _, task := range phase.Tasks {
			if task.Status != TaskDone {
				continue
			}
			open := []string{}
			for _, id := range uniqueDependencyIDs(task.DependsOn) {
				if tasks[id] != TaskDone {
					open = append(open, id)
				}
			}
			if len(open) > 0 {
				reasons = append(reasons, fmt.Sprintf("task %s/%s is done while dependency task(s) are not done: %s", phase.ID, task.ID, strings.Join(open, ", ")))
			}
		}
	}
	return reasons
}

// EffectiveSchemaVersion defaults absence to 1; a marker can only promote to 2.
func EffectiveSchemaVersion(plan *Goalplan, markerPresent bool) float64 {
	declared := float64(1)
	if plan.SchemaVersion != nil {
		declared = *plan.SchemaVersion
	}
	if markerPresent {
		return math.Max(declared, 2)
	}
	return declared
}

// ComputeQaRequired scans the whole plan, including already-met criteria.
func ComputeQaRequired(plan *Goalplan) bool {
	for _, criterion := range plan.Criteria {
		if criterion.Surface == SurfaceWeb || criterion.Surface == SurfaceTUI || criterion.Surface == SurfaceDesktop {
			return true
		}
	}
	return false
}

func supersededIntegrityReasons(plan *Goalplan) []string {
	out := []string{}
	for _, phase := range plan.WorkPhases {
		if phase.Status != WorkPhaseSuperseded {
			continue
		}
		by := phase.SupersededBy
		if by == nil || jstext.Trim(*by) == "" {
			out = append(out, fmt.Sprintf("work phase %s is superseded but does not name what replaced it (supersededBy)", phase.ID))
			continue
		}
		if *by == phase.ID {
			out = append(out, fmt.Sprintf("work phase %s claims to supersede itself, which would drop it from the remaining work for free", phase.ID))
			continue
		}
		var target *GoalplanWorkPhase
		for i := range plan.WorkPhases {
			if plan.WorkPhases[i].ID == *by {
				target = &plan.WorkPhases[i]
				break
			}
		}
		if target == nil {
			out = append(out, fmt.Sprintf("work phase %s is superseded by '%s', which is not in this plan", phase.ID, *by))
			continue
		}
		if target.Status == WorkPhaseSuperseded {
			out = append(out, fmt.Sprintf("work phase %s is superseded by '%s', which is itself superseded — the work would vanish", phase.ID, *by))
		}
	}
	return out
}

// SchemaMarkerPath delegates the same path/symlink validation as a plan read.
func SchemaMarkerPath(cwd, slug string) (string, error) {
	dir, err := GoalplanDir(cwd, slug)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "schema-v2.marker"), nil
}
