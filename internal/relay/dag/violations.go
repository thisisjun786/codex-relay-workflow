package dag

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Rule codes of a rejected revision. A violation names the rule it breaks, where in the document, and
// why; the same document against the same parent always yields the same list in the same order.
const (
	RuleDuplicateKey               = "duplicate_key"
	RuleNotAnObject                = "not_an_object"
	RuleWrongType                  = "wrong_type"
	RuleUnknownField               = "unknown_field"
	RuleMissingField               = "missing_field"
	RuleEmptyValue                 = "empty_value"
	RuleBadIdentifier              = "bad_identifier"
	RuleBadDigest                  = "bad_digest"
	RuleBadSchema                  = "bad_schema"
	RuleValueTooLong               = "value_too_long"
	RuleDuplicateValue             = "duplicate_value"
	RuleUnknownOp                  = "unknown_op"
	RuleUnknownNodeKind            = "unknown_node_kind"
	RuleUnknownEdgeKind            = "unknown_edge_kind"
	RuleEmptyChanges               = "empty_changes"
	RuleLimitExceeded              = "limit_exceeded"
	RuleProjectMismatch            = "project_mismatch"
	RuleEmptyGraph                 = "empty_graph"
	RuleDuplicateNodeID            = "duplicate_node_id"
	RuleDuplicateEdgeID            = "duplicate_edge_id"
	RuleUnknownNode                = "unknown_node"
	RuleUnknownEdge                = "unknown_edge"
	RuleMissingPredecessor         = "missing_predecessor"
	RuleMissingSuccessor           = "missing_successor"
	RuleSelfReference              = "self_reference"
	RuleCycle                      = "cycle"
	RuleEdgeFieldMissing           = "edge_field_missing"
	RuleEdgeFieldNotApplicable     = "edge_field_not_applicable"
	RuleOutputIntegratedNeedsImpl  = "output_integrated_needs_implementation"
	RuleOutputDecisionNeedsNonPR   = "output_decision_needs_non_pr"
	RuleBadText                    = "bad_text"
	RuleConflictingChanges         = "conflicting_changes"
	RuleInvalidLifecycleTransition = "invalid_lifecycle_transition"
)

// Violation is one broken rule.
type Violation struct {
	Rule   string
	Path   string
	Detail string
}

func (v Violation) String() string { return "[" + v.Rule + "] " + v.Path + ": " + v.Detail }

// maxShown bounds the violations a refusal prints; the count says how many there were.
const maxShown = 20

// PlanRejected is a revision that breaks the plan rules: nothing was written. It is the refusal
// malformed_receipt of the relay exit-code contract (D-02: no new reason for it), with every
// violation in its detail.
type PlanRejected struct {
	Violations []Violation
}

func (e *PlanRejected) Error() string { return e.refusal().Error() }

// Unwrap exposes the refusal, so a caller that looks for the relay's refusal finds it.
func (e *PlanRejected) Unwrap() error { return e.refusal() }

func (e *PlanRejected) refusal() *store.RefusedError {
	shown := e.Violations
	more := ""
	if len(shown) > maxShown {
		more = fmt.Sprintf("; and %d more", len(shown)-maxShown)
		shown = shown[:maxShown]
	}
	parts := make([]string, len(shown))
	for i, v := range shown {
		parts[i] = v.String()
	}
	return &store.RefusedError{
		Reason: string(contract.RefusalMalformedReceipt),
		Detail: fmt.Sprintf("the plan revision was rejected, %d violation(s): %s%s", len(e.Violations), strings.Join(parts, "; "), more),
	}
}

// Conflict is the one new refusal reason this feature adds: a plan write that lost to another one
// (a stale expected parent) or that reuses a request id for a different request.
func conflict(format string, args ...any) error {
	return &store.RefusedError{Reason: string(contract.RefusalPlanRevisionConflict), Detail: fmt.Sprintf(format, args...)}
}

// NotFound is a plan or revision that is not there, or a store that holds no DAG zone.
func notFound(format string, args ...any) error {
	return &store.RefusedError{Reason: string(contract.RefusalUnregisteredScope), Detail: fmt.Sprintf(format, args...)}
}

// CorruptError is a stored plan that does not agree with itself: a digest that does not match the
// rows it covers, a log that does not replay to its materialization. It is the host's failure (the
// relay exit code 3), never a refusal and never a partial plan.
type CorruptError struct{ Detail string }

func (e *CorruptError) Error() string { return "the stored DAG plan is corrupt: " + e.Detail }
