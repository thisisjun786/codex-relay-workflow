package ops

import "slices"

// The rule ids of the spawn policy (docs/flow/script.md, the design's spawnPolicy). This file is the one source of the strings:
// no other file of internal/flow holds an "SP-" literal (TestRuleIDsHaveOneSource reads the sources to check it), and a script
// names a rule by its key (RuleKey), never by the id, so a rule renamed here is renamed everywhere. A rule is the reason a
// row of flow_spawn_decisions or flow_step_decisions gives for the decision it records.
const (
	// The structural spawns.
	RuleExec1  = "SP-EXEC-1"  // an executor for every issue node crw.dag releases, one per attempt
	RuleRev1   = "SP-REV-1"   // a reviewer for every executor head with a passing verification, one per round
	RuleRev2   = "SP-REV-2"   // the candidate batch of low-tier nodes (CRW-1225)
	RuleArch1  = "SP-ARCH-1"  // the architect, once per milestone
	RuleRevF1  = "SP-REVF-1"  // reviewer-final at the milestone close (F13)
	RuleChild1 = "SP-CHILD-1" // a `child` session, released instead of an executor
	RuleQA1    = "SP-QA-1"    // the QA capture call of the final gate (F13)
	RuleQA2    = "SP-QA-2"    // the two oracle passes of the final gate (F13)

	// The signals.
	RuleExpl1  = "SP-EXPL-1"  // an explorer for an open question of the architect, or for the background question of a milestone node
	RuleExpl2  = "SP-EXPL-2"  // an explorer for the research question of an executor
	RuleExpl3  = "SP-EXPL-3"  // an explorer for the evidence_question of a reviewer defect
	RuleTier1  = "SP-TIER-1"  // independent failure evidence on a tier: the step to executor-t1
	RuleTier2  = "SP-TIER-2"  // the tier step is exhausted: the node escalates
	RuleChild2 = "SP-CHILD-2" // size_fit child: the node is released as a `child` session
	RuleArch2  = "SP-ARCH-2"  // a re-plan (F09)
	RuleArch3  = "SP-ARCH-3"  // the architect's reflection at the milestone close (F09)
	RuleNat1   = "SP-NAT-1"   // a native spawn of a flow-bound thread that is no step: an engine call's explorer, or a helper of the child

	// The work the run does itself.
	RuleInl1 = "SP-INL-1" // a research question inside the asker's regions, or with at most three files, is answered by the asker; never an open question of a DESIGN
	RuleInl2 = "SP-INL-2" // a question already answered in the run reuses its answer
	RuleInl3 = "SP-INL-3" // compile, commit, verification, premerge assembly, accept and integrate are code steps
	RuleInl4 = "SP-INL-4" // the child is never woken for one issue's event; it is woken for a step of its run

	// RuleAudit is the design audit of the F08 body: a reviewer of another model family than the architect audits the DESIGN
	// before the first executor starts.
	RuleAudit = "SP-AUD-1"
)

// RuleKeys are the names a script uses for the rules. A script passes the key; the id a row records is looked up here.
var RuleKeys = map[string]string{
	"EXEC_1": RuleExec1, "REV_1": RuleRev1, "REV_2": RuleRev2, "ARCH_1": RuleArch1, "REVF_1": RuleRevF1, "CHILD_1": RuleChild1,
	"QA_1": RuleQA1, "QA_2": RuleQA2, "EXPL_1": RuleExpl1, "EXPL_2": RuleExpl2, "EXPL_3": RuleExpl3, "TIER_1": RuleTier1,
	"TIER_2": RuleTier2, "CHILD_2": RuleChild2, "ARCH_2": RuleArch2, "ARCH_3": RuleArch3, "NAT_1": RuleNat1,
	"INL_1": RuleInl1, "INL_2": RuleInl2, "INL_3": RuleInl3, "INL_4": RuleInl4, "AUDIT": RuleAudit,
}

// RuleID is the id of the rule a script names by key.
func RuleID(key string) (string, bool) {
	id, ok := RuleKeys[key]
	return id, ok
}

// RuleKey is the key of a rule id ("" for an id that is no rule).
func RuleKey(id string) string {
	for k, v := range RuleKeys {
		if v == id {
			return k
		}
	}
	return ""
}

// RuleIDs lists every rule id, sorted.
func RuleIDs() []string {
	out := make([]string, 0, len(RuleKeys))
	for _, id := range RuleKeys {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// The decisions a flow_spawn_decisions row records.
const (
	DecisionSpawn   = "spawn"
	DecisionInline  = "inline"
	DecisionReuse   = "reuse"
	DecisionSkip    = "skip"
	DecisionRefused = "refused"
	// DecisionOrchestrator hands a question to the orchestrator thread as a design question: the explorer cap of the milestone is
	// spent.
	DecisionOrchestrator = "orchestrator"
)

// The kinds of a flow_spawn_decisions row this issue writes (the native rows have their own kinds, native.go).
const (
	KindStructural = "structural"
	KindSignal     = "signal"
	KindInline     = "inline"
)

// The evidence rules of SP-TIER-1, the letter a flow_step_decisions row names.
const (
	TierEvidenceFinding = "a" // needs_changes in two consecutive rounds with a repeated finding
	TierEvidenceVerify  = "b" // the verification failed on two consecutive committed heads
	TierEvidenceNull    = "c" // null_model on two consecutive executor calls of the tier
	TierEvidenceRounds  = "d" // four reviewed rounds on the tier without a verified verdict
)

// The thresholds of the signals.
const (
	// ExplorerConcurrency is the explorers an SP-EXPL-1 fan-out runs at once.
	ExplorerConcurrency = 3
	// ExplorersPerMilestone is the explorers a milestone spawns for the open questions of its architect; the rest go to the
	// orchestrator thread as design questions. The background explorers of the milestone's nodes (CRW-1466) are outside it.
	ExplorersPerMilestone = 6
	// InlineFiles is the files_needed at or below which a research question is answered by the asker itself (SP-INL-1; an open
	// question of a DESIGN goes to an explorer whatever its files_needed).
	InlineFiles = 3
	// ResearchPerTier is the research continuations a node gets on one tier before it escalates as a scope question.
	ResearchPerTier = 2
	// EvidenceExplorersPerNode is the explorers a node gets for evidence_question defects.
	EvidenceExplorersPerNode = 2
	// TierRounds is the reviewed rounds on one tier without a verified verdict that step the tier.
	TierRounds = 4
	// DesignAuditRounds is the audit rounds a milestone's DESIGN gets before the milestone escalates.
	DesignAuditRounds = 3
)

// The scope notations of the four kinds of open question that are outside the architect's authority (the architect overlay,
// internal/flow/roles/architect.md, gives them to the architect). An open question of a DESIGN whose scope starts with one goes to
// the orchestrator as a non-blocking ask and to no explorer (crw.exploreRoute, AuthorityScope); the scope stays a free string of
// the DESIGN schema.
const (
	ScopeRepoRule    = "authority:repo-rule"    // an exception to a rule of the repository
	ScopeOtherRecord = "authority:other-record" // the record of another issue
	ScopePolicy      = "authority:policy"       // a policy decision
	ScopeEnvironment = "authority:environment"  // a prerequisite of the execution environment
)

// AuthorityScopes are the four notations, in the order the overlay names their kinds.
var AuthorityScopes = []string{ScopeRepoRule, ScopeOtherRecord, ScopePolicy, ScopeEnvironment}
