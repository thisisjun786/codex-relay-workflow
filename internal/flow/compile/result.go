package compile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// Result is what compile makes of a handoff: one plan result per project, in the order the handoff lists them.
type Result struct {
	Schema     string `json:"schema"`
	Initiative string `json:"initiative,omitempty"`
	Plans      []Plan `json:"plans"`
}

// Plan is the result for one project.
type Plan struct {
	PlanID       string `json:"plan_id"`
	ProjectKey   string `json:"project_key"`
	Status       string `json:"status"`        // "changes": a revision follows; "unchanged": the plan already holds this handoff
	BaseRevision int64  `json:"base_revision"` // the revision the handoff was compared with; 0 for a plan that does not exist yet
	// Warnings are what compile read in the form a run needs and did not refuse (CRW-1317).
	Warnings []Warning `json:"warnings,omitempty"`
	// Revision is the dag-plan-revision/1 document `crw relay dag-plan-put --request @file` takes. It is left out of a dry run.
	Revision json.RawMessage `json:"revision,omitempty"`
	Preview  Preview         `json:"preview"`

	// Change is the revision as the DAG package holds it, for a Go caller (nil when unchanged).
	Change *dag.Revision `json:"-"`
}

// Status values of a plan.
const (
	StatusChanges   = "changes"
	StatusUnchanged = "unchanged"
)

// Preview is what a dry run shows of a plan: the changes, the issues beside the size fields they carry, and the shape
// of the graph. Nothing in it is a date.
type Preview struct {
	Nodes             int              `json:"nodes"` // issues of the plan after the revision
	Edges             int              `json:"edges"`
	Changes           ChangeCounts     `json:"changes"`
	ParallelWidth     int              `json:"parallel_width"` // the most issues that can run at once: the widest set none of which waits on another
	CriticalPath      []string         `json:"critical_path"`  // the longest chain of issues, by key
	CriticalPathLen   int              `json:"critical_path_length"`
	Waves             [][]string       `json:"waves"` // issues by the earliest wave each can start in
	Issues            []IssuePreview   `json:"issues"`
	Groups            []GroupPreview   `json:"groups"`
	Milestones        []MilestoneCount `json:"milestones"`
	SizeUnreviewed    []string         `json:"size_unreviewed"` // over_line with neither a bundle reason nor a split: shown, never refused
	ExternalPrereqs   []ExternalAfter  `json:"external_after"`  // prerequisites in another project of the handoff: not edges of this plan
	RetiredIssues     []string         `json:"retired"`         // issues the plan held that the handoff no longer has
	RequestID         string           `json:"request_id,omitempty"`
	ExpectedParentRev int64            `json:"expected_parent_revision"`
}

// ChangeCounts count the changes of a revision by op.
type ChangeCounts struct {
	AddNode    int `json:"add_node"`
	UpdateNode int `json:"update_node"`
	RetireNode int `json:"retire_node"`
	AddEdge    int `json:"add_edge"`
	RetireEdge int `json:"retire_edge"`
	// KeepDecisionEdge is how many decision edges of the base the revision leaves alone: the handoff cannot state an approval, so compile never retires one.
	KeepDecisionEdge int `json:"keep_decision_edge,omitempty"`
}

// IssuePreview is one issue with what compile carried for it.
type IssuePreview struct {
	Key             string   `json:"key"`
	NodeID          string   `json:"node_id"`
	Op              string   `json:"op"` // add, update, keep
	Kind            string   `json:"kind"`
	Milestone       string   `json:"milestone,omitempty"`
	Regions         []string `json:"regions"`
	After           []string `json:"after"`
	Group           string   `json:"group,omitempty"`
	SizeFit         string   `json:"size_fit"`
	SizeReason      string   `json:"size_reason"`
	IssueSize       string   `json:"issue_size,omitempty"`
	BundleReason    string   `json:"bundle_reason,omitempty"`
	ExceptionRecord string   `json:"exception_record,omitempty"`
	SizeUnreviewed  bool     `json:"size_unreviewed,omitempty"`
}

// GroupPreview is a group and the order its members run in.
type GroupPreview struct {
	Group string   `json:"group"`
	Order []string `json:"order"`
}

// MilestoneCount is how many issues a milestone holds.
type MilestoneCount struct {
	Milestone string `json:"milestone"`
	Issues    int    `json:"issues"`
}

// ExternalAfter is a prerequisite that is an issue of another project of the same handoff.
type ExternalAfter struct {
	Issue   string `json:"issue"`
	After   string `json:"after"`
	Project string `json:"project"`
}

// Refusal is one reason the handoff is refused. Rule is a compile rule (input, over_64, group_cap, region_order_missing, after_unknown, size_value)
// or a rule of the DAG (cycle, limit_exceeded, ...), exactly as dag-plan-put would name it.
type Refusal struct {
	Project string `json:"project"`
	Rule    string `json:"rule"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail"`
}

// Warning is one thing the handoff states in a form a run does not take as written, which compile reads as a run needs it and
// does not refuse: Rule is target_repository (a repository that is not an absolute path) or target_base_ref (a refs/heads/ prefix
// removed).
type Warning struct {
	Project string `json:"project"`
	Rule    string `json:"rule"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail"`
}

// Refused is the error of a handoff that is refused: no plan of the handoff is compiled.
type Refused struct{ Refusals []Refusal }

func (e *Refused) Error() string {
	parts := make([]string, 0, len(e.Refusals))
	for i, r := range e.Refusals {
		if i == 20 {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Refusals)-20))
			break
		}
		where := r.Project
		if r.Path != "" {
			where += " " + r.Path
		}
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", r.Rule, strings.TrimSpace(where), r.Detail))
	}
	return fmt.Sprintf("the handoff is refused, %d reason(s): %s", len(e.Refusals), strings.Join(parts, "; "))
}

// Has is whether some refusal names rule.
func (e *Refused) Has(rule string) bool {
	for _, r := range e.Refusals {
		if r.Rule == rule {
			return true
		}
	}
	return false
}
