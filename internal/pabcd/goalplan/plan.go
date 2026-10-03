package goalplan

import (
	"math"
	"slices"

	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The revival of a whole stored plan (goalplan.ts:468-676). The functions take the decoded JSON value of a plan (an object is a
// map[string]any, a list a []any) and rebuild it from the keys they know, as revive.go does for the sub-records. A list reviver
// reads its key out of the object itself, because the oracle tells an absent key from a present one (only "undefined" is absent;
// null is a present value of the wrong shape): it returns a nil list for an absent key, a non-nil one (empty included) for a
// present valid list, and false for the oracle's "invalid", which fails the whole plan.

// reviveSteeringLog is reviveSteeringLog: every entry must hold five non-empty texts.
func reviveSteeringLog(m map[string]any, key string) ([]SteeringEntry, bool) {
	raw, present := m[key]
	if !present {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := []SteeringEntry{}
	for _, entry := range list {
		e, ok := entry.(map[string]any)
		var f [5]string
		for i, k := range [...]string{"idempotencyKey", "rationale", "evidence", "appliedAt", "summary"} {
			f[i], _ = text(e, k)
			ok = ok && f[i] != ""
		}
		if !ok {
			return nil, false
		}
		out = append(out, SteeringEntry{IdempotencyKey: f[0], Rationale: f[1], Evidence: f[2], AppliedAt: f[3], Summary: f[4]})
	}
	return out, true
}

// declaredSchemaVersion is declaredSchemaVersion: the number a plan names as its schemaVersion, 1 when it names none.
func declaredSchemaVersion(m map[string]any) float64 {
	if f, ok := jsNumber(m["schemaVersion"]); ok {
		return f
	}
	return 1
}

// reviveDependsOn is reviveDependsOn: a list of ids, none blank after a JavaScript trim; a partly dropped list would silently
// widen what the scheduler counts as ready, so any other shape fails the plan.
func reviveDependsOn(m map[string]any, key string) ([]string, bool) {
	raw, present := m[key]
	if !present {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok || jstext.Trim(s) == "" {
			return nil, false
		}
		ids = append(ids, s)
	}
	return ids, true
}

// reviveDecisionOptions is reviveDecisionOptions: absent stays absent; a present list is non-empty, every option non-blank, and
// the options distinct after trim. The options are kept as written.
func reviveDecisionOptions(m map[string]any, key string) ([]string, bool) {
	raw, present := m[key]
	if !present {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	options, seen := make([]string, 0, len(list)), map[string]bool{}
	for _, item := range list {
		s, ok := item.(string)
		if !ok || jstext.Trim(s) == "" || seen[jstext.Trim(s)] {
			return nil, false
		}
		seen[jstext.Trim(s)] = true
		options = append(options, s)
	}
	return options, true
}

// reviveDecisions is reviveDecisions: each decision has a lifecycle id, a question and an askedAt, an optional recommendation that
// is one of its options when it has any, and either is open with no answer or decidedAt, or is decided with a non-blank answer and a
// decidedAt. Anything else fails the plan.
func reviveDecisions(m map[string]any, key string) ([]GoalplanDecision, bool) {
	raw, present := m[key]
	if !present {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := []GoalplanDecision{}
	for _, item := range list {
		d, ok := item.(map[string]any)
		id, _ := text(d, "id")
		question, _ := text(d, "question")
		asked, _ := d["askedAt"].(string)
		recommendation, hasRecommendation := d["recommendation"]
		recommended, _ := recommendation.(string)
		options, optionsOK := reviveDecisionOptions(d, "options")
		if !ok || !isLifecycleID(id) || jstext.Trim(question) == "" || !validIsoTime(d["askedAt"]) || !optionsOK ||
			hasRecommendation && jstext.Trim(recommended) == "" ||
			options != nil && hasRecommendation && !slices.ContainsFunc(options, func(o string) bool { return jstext.Trim(o) == jstext.Trim(recommended) }) {
			return nil, false
		}
		decision := GoalplanDecision{ID: id, Question: question, AskedAt: asked, Options: options}
		_, hasAnswer := d["answer"]
		_, hasDecidedAt := d["decidedAt"]
		switch status, _ := text(d, "status"); DecisionStatus(status) {
		case DecisionOpen:
			if hasAnswer || hasDecidedAt {
				return nil, false
			}
			decision.Status = DecisionOpen
		case DecisionDecided:
			answer, _ := text(d, "answer")
			decidedAt, _ := d["decidedAt"].(string)
			if jstext.Trim(answer) == "" || !validIsoTime(d["decidedAt"]) {
				return nil, false
			}
			decision.Status, decision.Answer, decision.DecidedAt = DecisionDecided, answer, decidedAt
		default:
			return nil, false
		}
		if hasRecommendation {
			decision.Recommendation = recommended
		}
		out = append(out, decision)
	}
	return out, true
}

// reviveGoalplan is reviveGoalplan: nil (the oracle's null) unless parsed is an object with an objective and a valid slug
// that matches expectedSlug when one is given, a schemaVersion this build reads, and well-formed work phases, criteria,
// decisions and steering log. Below that, a task that is not an object or lacks an id or title is skipped, a bad task dependsOn
// fails the plan, an unknown status reads as the first one, and a sub-record that cannot be trusted is dropped by its own reviver
// (revive.go). The lists are never nil, so an empty one is written as [].
func reviveGoalplan(parsed any, expectedSlug *string) *Goalplan {
	o, ok := parsed.(map[string]any)
	objective, hasObjective := text(o, "objective")
	slug, hasSlug := text(o, "slug")
	if !ok || !hasObjective || !hasSlug {
		return nil
	}
	if _, err := ValidateGoalplanSlug(slug); err != nil || expectedSlug != nil && slug != *expectedSlug || declaredSchemaVersion(o) > SupportedMaxSchemaVersion {
		return nil
	}
	phases, phasesOK := o["workPhases"].([]any)
	rawCriteria, criteriaOK := o["criteria"].([]any)
	if !phasesOK || !criteriaOK {
		return nil
	}

	workPhases := make([]GoalplanWorkPhase, 0, len(phases))
	for _, wp := range phases {
		w, ok := wp.(map[string]any)
		id, hasID := text(w, "id")
		title, hasTitle := text(w, "title")
		dependsOn, dependsOK := reviveDependsOn(w, "dependsOn")
		awaitsDecision, awaitsOK := reviveDependsOn(w, "awaitsDecision")
		if !ok || !hasID || !hasTitle || !dependsOK || !awaitsOK {
			return nil
		}
		phase := GoalplanWorkPhase{ID: id, Title: title, Status: WorkPhasePending, Tasks: []GoalplanTask{}, CriteriaIDs: []string{},
			DependsOn: dependsOn, AwaitsDecision: awaitsDecision, BlockedReason: textPtr(w, "blockedReason"), SupersededBy: textPtr(w, "supersededBy")}
		switch status, _ := text(w, "status"); WorkPhaseStatus(status) {
		case WorkPhaseInProgress, WorkPhaseDone, WorkPhaseBlocked, WorkPhaseSuperseded:
			phase.Status = WorkPhaseStatus(status)
		}
		tasks, _ := w["tasks"].([]any)
		for _, t := range tasks {
			tt, ok := t.(map[string]any)
			id, hasID := text(tt, "id")
			title, hasTitle := text(tt, "title")
			if !ok || !hasID || !hasTitle {
				continue
			}
			taskDependsOn, ok := reviveDependsOn(tt, "dependsOn")
			if !ok {
				return nil
			}
			task := GoalplanTask{ID: id, Title: title, Status: TaskPending, DependsOn: taskDependsOn}
			if tt["status"] == string(TaskDone) {
				task.Status = TaskDone
			}
			// A blank outcome is no evidence at all, so it stays absent rather than reading as recorded.
			if outcome, _ := text(tt, "outcome"); jstext.Trim(outcome) != "" {
				task.Outcome = jstext.Trim(outcome)
			}
			phase.Tasks = append(phase.Tasks, task)
		}
		ids, _ := w["criteriaIds"].([]any)
		for _, id := range ids {
			if s, ok := id.(string); ok {
				phase.CriteriaIDs = append(phase.CriteriaIDs, s)
			}
		}
		workPhases = append(workPhases, phase)
	}

	criteria := make([]GoalplanCriterion, 0, len(rawCriteria))
	for _, c := range rawCriteria {
		cc, ok := c.(map[string]any)
		id, hasID := text(cc, "id")
		scenario, hasScenario := text(cc, "scenario")
		if !ok || !hasID || !hasScenario {
			return nil
		}
		expected, _ := text(cc, "expectedEvidence")
		criterion := GoalplanCriterion{ID: id, Scenario: scenario, ExpectedEvidence: expected, CapturedEvidence: textPtr(cc, "capturedEvidence"), Status: CriterionOpen}
		if cc["status"] == string(CriterionMet) {
			criterion.Status = CriterionMet
		}
		// Only a known surface is kept: a missing one and an unknown one both stay absent.
		switch surface, _ := text(cc, "surface"); CriterionSurface(surface) {
		case SurfaceLogic, SurfaceWeb, SurfaceTUI, SurfaceDesktop:
			criterion.Surface = CriterionSurface(surface)
		}
		if cc["presented"] == string(PresentedNative) {
			criterion.Presented = PresentedNative
		}
		criteria = append(criteria, criterion)
	}

	hostRaw, _ := o["host"].(map[string]any)
	host := GoalplanHostLink{Armed: hostRaw["armed"] == true, ArmedAt: textPtr(hostRaw, "armedAt"), Source: HostSourceNone}
	if hostRaw["source"] == string(HostSourceFreeze) {
		host.Source = HostSourceFreeze
	}

	decisions, decisionsOK := reviveDecisions(o, "decisions")
	steeringLog, steeringOK := reviveSteeringLog(o, "steeringLog")
	if !decisionsOK || !steeringOK {
		return nil
	}
	plan := &Goalplan{
		Objective: objective, Slug: slug, CreatedAt: timestamp(o, "createdAt"), UpdatedAt: timestamp(o, "updatedAt"),
		ActiveWorkPhaseID: textPtr(o, "activeWorkPhaseId"), WorkPhases: workPhases, Criteria: criteria, Host: host,
		ReviewRounds: reviveReviewRounds(o["reviewRounds"]), Decisions: decisions,
		ActivePlanAuditRoundID: textPtr(o, "activePlanAuditRoundId"), ActiveFinalGateRoundID: textPtr(o, "activeFinalGateRoundId"),
		FinalGate: reviveFinalGate(o["finalGate"]), SteeringLog: steeringLog,
	}
	// The version is floored; adding 0 turns a negative zero into 0, as JSON.stringify prints it. A number that is not finite is dropped.
	if f, ok := jsNumber(o["schemaVersion"]); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
		version := math.Floor(f) + 0
		plan.SchemaVersion = &version
	}
	return plan
}
