// Package compile is `crw flow compile`: it turns the handoff of crw-plan (the issues of an initiative's
// projects as Linear holds them after the planner's writes and readback: the prerequisites, the edit regions,
// size_fit, size_reason, group and milestone of each issue) into the plan revision each project's DAG takes,
// by folding it in-process through internal/relay/dag, the same rules `crw relay dag-plan-put` applies.
//
// It produces input for dag-plan-put and nothing else: it writes no store, calls no service and makes no date.
// An issue that is new is an add_node, one whose spec changed an update_node, one that left a retire_node (with the
// retire_edge of every edge it had); a node id once used is never used again, so an issue that comes back gets a
// new id. A group is an ordering unit: its members, in the order the handoff declares them, are chained by
// order edges, and each member stays a node of its own. The planner (CRW-1037) decides which issues overlap
// and in which order; compile moves that result into edges and judges the plan it makes: more than 64 issues, a
// cycle, two issues whose edit regions overlap with no order between them, and a group of more than four are
// refused; so are a cycle through prerequisites of more than one project, an issue key held by two projects, and the root of the
// repository as an edit region. size_fit, size_reason and the issue-size answer are carried as they are, and an issue that answers
// over_line with neither a bundle reason nor a split is shown as size_unreviewed (it is never refused).
//
// Three things tie a revision to the relay it is put into. The criteria of an issue are the canonical criteria c1, c2, ...
// (each required, in the order listed), and the node's criteria_set_digest is delivery.SetDigest of them, the digest dag-release
// recomputes. A project names the coordinator_epoch its writing session holds, which the revision document carries, so a claimed plan
// admits the put. And a recompile never retires a decision edge of the base while both of its nodes stay: the handoff cannot state an
// approval, so dropping one would make work ready that waits for it.
package compile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	flowdelivery "github.com/thisisjun786/codex-relay-workflow/internal/flow/delivery"
)

// The schemas of the handoff and of the result.
const (
	InputSchema  = "crw-flow-compile-input/1"
	OutputSchema = "crw-flow-compile/1"
)

// Limits that are compile's own; the graph limits (64 nodes, 256 edges) are the DAG's (dag.MaxNodes, dag.MaxEdges).
const (
	// MaxGroup is the most issues one group names.
	MaxGroup = 4
	// DefaultAuthor is the author_task_id of a revision when the handoff names none.
	DefaultAuthor = "crw-flow-compile"
)

// Input is the handoff of crw-plan.
type Input struct {
	Schema       string    `json:"schema"`
	Initiative   string    `json:"initiative,omitempty"`
	AuthorTaskID string    `json:"author_task_id,omitempty"`
	Projects     []Project `json:"projects"`
}

// Project is one project of the initiative: one plan, one integration target. Key is the project key of the plan.
type Project struct {
	Key       string  `json:"key"`
	PlanID    string  `json:"plan_id,omitempty"`    // the plan this project's revision is for; the key when absent
	RequestID string  `json:"request_id,omitempty"` // derived from the revision when absent, so a repeated compile repeats its request
	Target    *Target `json:"target,omitempty"`
	// CoordinatorEpoch is the epoch the writing session holds on the plan (the epoch its claim raised it to); 0 for a plan nobody has claimed.
	CoordinatorEpoch int64   `json:"coordinator_epoch,omitempty"`
	Issues           []Issue `json:"issues"`
	// Milestones are what the run of a milestone needs that no issue line says (CRW-1277): optional, and once one is given every
	// milestone an issue names has its entry and every entry is named by an issue. They are not part of the plan revision: the run
	// pins the handoff and reads them from it (crw flow start --handoff).
	Milestones []Milestone `json:"milestones,omitempty"`
	// Warnings are what the writer of the handoff found and did not refuse (CRW-1379): `crw flow plan-input` records here that the
	// project key is not the owner's binding. Compile carries them into the plan's warnings and judges nothing by them (a comparison of
	// its own replaces the carried key warnings); they are not part of the plan revision. The author of a handoff does not write them.
	Warnings []InputWarning `json:"warnings,omitempty"`
}

// InputWarning is a warning a handoff carries: Rule names it, Issue the issue it is about ("" for the project) and Detail says it.
type InputWarning struct {
	Rule   string `json:"rule"`
	Issue  string `json:"issue,omitempty"`
	Detail string `json:"detail"`
}

// Milestone is a milestone of a project as the run reads it: its title, its criteria (at least one), the surfaces of its criteria (a
// non-empty subset of logic, web, tui and desktop: web, tui or desktop makes a QA receipt part of the final gate), its work grade
// (C1..C5: from C3 the QA evidence is judged by the two oracle passes) and the required verification commands of its final gate
// (empty: the repository's CI, crw-dev ci local). DeliveryCriteria marks whole criteria for post-publication readback (CRW-1609).
type Milestone struct {
	Key              string                   `json:"key"`
	Title            string                   `json:"title"`
	Criteria         []string                 `json:"criteria"`
	DeliveryCriteria []flowdelivery.Criterion `json:"delivery_criteria,omitempty"`
	Surfaces         []string                 `json:"surfaces"`
	Grade            string                   `json:"grade"`
	RequiredCommands []string                 `json:"required_commands"`
}

// Surfaces are the surfaces a milestone's criteria may name.
var Surfaces = []string{"logic", "web", "tui", "desktop"}

// Target is where the project's issues integrate: the repository, the absolute path of the local checkout a run integrates on, and
// the base ref, the short name of the branch (a refs/heads/ prefix is read as the short name, with a warning). PRBase is the branch a
// bundle's pull request is opened against (CRW-1440); a project without it integrates and lands on the base ref as before. It is read
// by the start check and the pull-request commands only: it enters no edge and no plan digest.
type Target struct {
	Repository string `json:"repository"`
	BaseRef    string `json:"base_ref"`
	PRBase     string `json:"pr_base,omitempty"`
}

// branchPrefix is the prefix a full branch ref carries; the integration path adds it to the short name itself.
const branchPrefix = "refs/heads/"

// Branch is the target's branch as a run integrates on it: the short name, with a refs/heads/ prefix removed (CRW-1317). Every
// reader of the target takes the branch through it, so the plan's edges, the integration request and the observations name the
// same ref.
func (t Target) Branch() string { return strings.TrimPrefix(t.BaseRef, branchPrefix) }

// Issue is one issue line.
type Issue struct {
	Key               string   `json:"key"`
	Title             string   `json:"title"`
	Kind              string   `json:"kind,omitempty"`                // implementation (default) or non_pr
	Criteria          []string `json:"criteria,omitempty"`            // the completion criteria; their digest is the node's criteria_set_digest
	CriteriaSetDigest string   `json:"criteria_set_digest,omitempty"` // the digest itself, in place of the criteria
	Milestone         string   `json:"milestone,omitempty"`
	Regions           []Region `json:"regions,omitempty"`
	After             []string `json:"after,omitempty"` // the prerequisites: issue keys that integrate first
	Group             string   `json:"group,omitempty"` // the group's name; members are chained in the order the issues are declared
	SizeFit           string   `json:"size_fit,omitempty"`
	SizeReason        string   `json:"size_reason,omitempty"`
	IssueSize         string   `json:"issue_size,omitempty"` // the issue-size report's answer
	BundleReason      string   `json:"bundle_reason,omitempty"`
	ExceptionRecord   string   `json:"exception_record,omitempty"`
	// Description and Labels are the issue's body and label names as the Linear tools give them (CRW-1328), optional: the run judges
	// them with the release gate when it starts (the warn fact issue_ready) and the compiler reads neither.
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// Region is one edit region of an issue. In the handoff it is a string (a trailing slash makes it a tree, else it is a
// file) or an object with the fields of a scheduler region.
type Region struct {
	Path      string `json:"path"`
	Kind      string `json:"kind,omitempty"`   // tree, file or symbol
	Key       string `json:"key,omitempty"`    // the symbol, for kind symbol
	Change    string `json:"change,omitempty"` // edit (default), rename or delete
	Exclusive bool   `json:"exclusive,omitempty"`
}

// UnmarshalJSON reads the string form and the object form.
func (r *Region) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		kind := "file"
		if strings.HasSuffix(s, "/") {
			kind = "tree"
		}
		*r = Region{Path: s, Kind: kind}
		return nil
	}
	type plain Region
	var p plain
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("a region is a path or an object {path, kind, key, change, exclusive}: %w", err)
	}
	*r = Region(p)
	return nil
}

// Decode reads a handoff strictly: an unknown field is refused, never skipped.
func Decode(raw []byte) (Input, error) {
	var in Input
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Input{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Input{}, errors.New("more than one JSON value")
	}
	return in, nil
}

// cleanRegion is the region as the scheduler compares it: a clean relative path, a known kind and change.
func cleanRegion(r Region) (Region, error) {
	p := strings.TrimSuffix(r.Path, "/")
	if r.Path == "" || p == "" || strings.HasPrefix(r.Path, "/") || strings.ContainsAny(r.Path, "\x00\\") {
		return r, fmt.Errorf("region path %q must be a relative path inside the repository", r.Path)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return r, fmt.Errorf("region path %q leaves the repository", r.Path)
	}
	if clean == "." {
		return r, fmt.Errorf("region path %q is the root of the repository: name the trees or files the issue edits, as the scheduler takes no region for the whole repository", r.Path)
	}
	r.Path = clean
	if r.Kind == "" {
		r.Kind = "file"
	}
	if r.Change == "" {
		r.Change = "edit"
	}
	switch r.Kind {
	case "tree", "file":
		if r.Key != "" {
			return r, fmt.Errorf("region %q: only a symbol has a key", r.Path)
		}
	case "symbol":
		if r.Key == "" {
			return r, fmt.Errorf("region %q: a symbol region needs its key", r.Path)
		}
	default:
		return r, fmt.Errorf("region %q: kind %q is not tree, file or symbol", r.Path, r.Kind)
	}
	switch r.Change {
	case "edit", "rename", "delete":
	default:
		return r, fmt.Errorf("region %q: change %q is not edit, rename or delete", r.Path, r.Change)
	}
	return r, nil
}

func (r Region) String() string {
	switch r.Kind {
	case "tree":
		return r.Path + "/"
	case "symbol":
		return r.Path + "#" + r.Key
	}
	return r.Path
}
