//go:build dev

package ci

// The verifier's identity (CRW-1590): what implementation answered a record, so a record only
// answers the same verifier. Legacy verification-record/1 was written only by the local verifier.
const localVerifierKind = "local"
const localVerifierRules = "ci-local/1"

// recordStepReuse names where a step's recorded answer came from: the source record, its digest
// and head, the verifier kind, and the job/step index inside that record.
type recordStepReuse struct {
	Record       string `json:"record"`
	Digest       string `json:"digest"`
	HeadCommit   string `json:"headCommit"`
	VerifierKind string `json:"verifierKind"`
	Job          string `json:"job"`
	Step         int    `json:"step"`
}

// localReuseSource is a validated record a run may take steps from.
type localReuseSource struct {
	path   string
	record verificationRecord
}

func localVerifierIdentity(r verificationRecord) (string, string) {
	kind, rules := r.VerifierKind, r.VerifierRules
	// Legacy verification-record/1 was written only by the local verifier.
	// Runner is a caller label (flow/local), not an implementation identity.
	if kind == "" {
		kind = localVerifierKind
	}
	if rules == "" {
		rules = localVerifierRules
	}
	return kind, rules
}

// localMarkReused answers a step from the validated source record: zero seconds of this run and
// the step's provenance.
func localMarkReused(step recordStep, source *localReuseSource, job string, index int) recordStep {
	kind, _ := localVerifierIdentity(source.record)
	step.Seconds = 0
	step.ReusedFrom = &recordStepReuse{Record: source.path, Digest: source.record.Digest,
		HeadCommit: source.record.HeadCommit, VerifierKind: kind, Job: job, Step: index}
	return step
}

// Fresh steps may depend on artifacts made by earlier full steps. Materialize
// that prefix again rather than pretending a cached build left files in a new workspace.
func localLastFreshStep(task localTask) int {
	last, index := -1, 0
	for _, step := range task.job.steps {
		if step.legs != nil && !localContains(step.legs, task.leg) {
			continue
		}
		if step.kind != localNotApplicable && (step.scope != "full" || step.action == localAggregate) {
			last = index
		}
		index++
	}
	return last
}

// localPlanAllReusable is whether every runnable step can come from a record: no range steps, no
// aggregates and nothing not_applicable needs running when it is.
func localPlanAllReusable(plan []localJob) bool {
	for _, job := range localExpectedJobs(plan) {
		for _, step := range job.steps {
			if step.kind != localNotApplicable && (step.scope != "full" || step.action == localAggregate) {
				return false
			}
		}
	}
	return true
}

// localHasReusedSteps is whether any recorded step came from another record.
func localHasReusedSteps(record verificationRecord) bool {
	for _, job := range record.Jobs {
		for _, step := range job.Steps {
			if step.ReusedFrom != nil {
				return true
			}
		}
	}
	return false
}
