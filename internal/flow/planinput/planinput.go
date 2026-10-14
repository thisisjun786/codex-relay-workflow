// Package planinput is `crw flow plan-input`: it builds the handoff `crw flow compile` reads (crw-flow-compile-input/1) from one
// milestone of the tracking repository, through internal/tracking.
//
// A project owner takes one milestone; the plan's project key is the milestone ref (OWNER/NAME#N, the identifier form of the tracking
// package), each open issue of the milestone is one issue line keyed by its ref (OWNER/NAME#N), and the order of the issues is the
// "Blocked by:" lines of their bodies. The command only reads: the Reader it takes has a Milestone method and nothing that writes, and
// the tracking adapter sends GET requests for it.
//
// The issue body is read for the fields of an issue line from a small fixed set of headings (see body.go):
//
//	criteria  a bullet list under 끝 조건, Done when, Acceptance or Acceptance criteria; with none the issue title is the one criterion
//	regions   a bullet list under 수정 범위, Edit regions or Regions (a path in backticks, or the first word; a trailing slash is a tree);
//	          with none the issue has no region and the compile default applies
//	size      "size_fit: V", "size_reason: V" and "issue_size: V" lines under 크기, Size or Size fit; with none compile's defaults apply
//
// What cannot be mapped is a warning, never a refusal: a missing criteria section, a region compile would refuse, a size value outside
// its closed set, a blocker that is closed, outside the milestone or the issue itself, a milestone that is closed or has no open issue,
// and a project key the owner thread holds no live parent binding of (the comparison `crw flow start` makes as project_key_bound,
// CRW-1317). Only an input error (a Ref that is not OWNER/NAME#N, a milestone that does not exist) stops the command.
package planinput

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// EnvelopeSchema is the schema of the report `--envelope` prints.
const EnvelopeSchema = "crw-flow-plan-input/1"

// Reader is the read-only part of tracking.Tracker the command uses.
type Reader interface {
	Milestone(ctx context.Context, ref tracking.Ref) (tracking.Milestone, []tracking.Issue, error)
}

// Warning is something the mapping could not carry over, or a comparison that did not hold.
type Warning struct {
	Rule   string `json:"rule"`
	Issue  string `json:"issue,omitempty"`
	Detail string `json:"detail"`
}

// Binding is what the project key is compared with: the scope keys of the live parent bindings of the owner thread. Err is why they
// could not be read.
type Binding struct {
	Thread string
	Scopes []string
	Err    error
}

// Options are what the milestone does not say.
type Options struct {
	// Target is the project's integration target (the absolute path of the checkout and the branch); nil gives the project none.
	Target *compile.Target
	// Binding is compared with the project key; nil compares nothing.
	Binding *Binding
}

// Result is the compile input and the warnings of building it.
type Result struct {
	Input    compile.Input
	Warnings []Warning
}

// Envelope is the report `--envelope` prints: the input beside the warnings array that records what the mapping left out.
type Envelope struct {
	Schema    string        `json:"schema"`
	Milestone string        `json:"milestone"`
	Input     compile.Input `json:"input"`
	Warnings  []Warning     `json:"warnings"`
}

// Build reads the milestone and builds the compile input for it. An error is an input error, worded with what to pass.
func Build(ctx context.Context, r Reader, ref tracking.Ref, opts Options) (Result, error) {
	ms, all, err := r.Milestone(ctx, ref)
	if err != nil {
		switch {
		case errors.Is(err, tracking.ErrNotFound):
			return Result{}, fmt.Errorf("there is no milestone %s in the tracking repository (or the token cannot see it): pass --milestone OWNER/NAME#N with the number of a milestone that exists: %w", ref, err)
		case errors.Is(err, tracking.ErrUnauthorized), errors.Is(err, tracking.ErrForbidden), errors.Is(err, tracking.ErrNoToken):
			return Result{}, fmt.Errorf("the tracking repository refused the read of %s: set GH_TOKEN or GITHUB_TOKEN, or log in with gh auth login: %w", ref, err)
		}
		return Result{}, fmt.Errorf("reading milestone %s: %w", ref, err)
	}
	key := ref.String()
	var warnings []Warning
	warn := func(rule, issue, format string, a ...any) {
		warnings = append(warnings, Warning{Rule: rule, Issue: issue, Detail: fmt.Sprintf(format, a...)})
	}

	byRef := map[string]tracking.Issue{}
	var open []tracking.Issue
	for _, is := range all {
		if is.Milestone != ref.Number {
			continue
		}
		byRef[refKey(is.Ref)] = is
		if is.State == tracking.StateOpen {
			open = append(open, is)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].Ref.Number < open[j].Ref.Number })

	if ms.State == tracking.StateClosed {
		warn("milestone_closed", "", "milestone %s is closed", key)
	}
	if len(open) == 0 {
		warn("no_open_issue", "", "milestone %s has no open issue: compile refuses a project with no issue", key)
	}

	project := compile.Project{Key: key, Target: opts.Target, Issues: []compile.Issue{}}
	ordered := false
	for _, is := range open {
		line, w := issueLine(is, byRef, key)
		warnings = append(warnings, w...)
		ordered = ordered || len(line.After) > 0
		project.Issues = append(project.Issues, line)
	}
	if ordered && opts.Target == nil {
		warn("target_missing", "", "compile refuses an order between issues that integrate code when the project has no target: pass --target-repository (the absolute path of the checkout) and --base-ref (the branch)")
	}
	if b := opts.Binding; b != nil {
		if w, ok := keyWarning(key, *b); ok {
			warnings = append(warnings, w)
		}
	}
	return Result{
		Input:    compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{project}},
		Warnings: warnings,
	}, nil
}

// refKey is a Ref as a map key: the repository compared without regard to case.
func refKey(r tracking.Ref) string { return strings.ToLower(r.Repo) + "#" + fmt.Sprint(r.Number) }

// issueLine maps one open issue to its issue line and the warnings of the mapping.
func issueLine(is tracking.Issue, byRef map[string]tracking.Issue, project string) (compile.Issue, []Warning) {
	var warnings []Warning
	key := is.Ref.String()
	warn := func(rule, format string, a ...any) {
		warnings = append(warnings, Warning{Rule: rule, Issue: key, Detail: fmt.Sprintf(format, a...)})
	}
	line := compile.Issue{Key: key, Title: is.Title}
	secs := sections(is.Body)

	line.Criteria = criteria(secs)
	if len(line.Criteria) == 0 {
		line.Criteria = []string{is.Title}
		warn("criteria_missing", "the body has no bullet list under 끝 조건, Done when, Acceptance or Acceptance criteria: the title is the one criterion")
	}
	if items, _ := linesOf(secs, regionHeadings); len(items) > 0 {
		seen := map[string]bool{}
		for _, item := range bullets(items) {
			r, ok := regionOf(item)
			switch {
			case !ok:
				warn("region_invalid", "the edit region %q is not a relative path inside the repository: it is left out", r.Path)
			case !seen[r.Path]:
				seen[r.Path] = true
				line.Regions = append(line.Regions, r)
			}
		}
	}
	fields, bad := sizeOf(secs)
	line.SizeFit, line.SizeReason, line.IssueSize = fields["size_fit"], fields["size_reason"], fields["issue_size"]
	for _, b := range bad {
		warn("size_value", "%q is not in the closed set of its field: it is left out", b)
	}
	for _, b := range is.BlockedBy {
		dep, inMilestone := byRef[refKey(b)]
		switch {
		case refKey(b) == refKey(is.Ref):
			warn("blocker_self", "%s lists itself as a blocker", key)
		case !inMilestone:
			warn("blocker_outside_milestone", "blocker %s is not an issue of milestone %s: it is not a prerequisite", b, project)
		case dep.State != tracking.StateOpen:
			warn("blocker_closed", "blocker %s is closed: it is not a prerequisite", b)
		default:
			line.After = append(line.After, dep.Ref.String())
		}
	}
	return line, warnings
}

// keyWarning compares the project key with the scope keys of the owner's live parent bindings. A key the thread is bound to answers
// nothing.
func keyWarning(key string, b Binding) (Warning, bool) {
	switch {
	case b.Thread == "":
		return Warning{Rule: "project_key_unchecked", Detail: "the project key is not compared with a binding: pass --thread (or run in a session that has CODEX_THREAD_ID) to name the owner thread"}, true
	case b.Err != nil:
		return Warning{Rule: "project_key_unchecked", Detail: fmt.Sprintf("the live parent bindings of thread %s cannot be read, so the project key %s is not compared: %v", b.Thread, key, b.Err)}, true
	}
	for _, s := range b.Scopes {
		if strings.EqualFold(s, key) {
			return Warning{}, false
		}
	}
	holds := "thread " + b.Thread + " holds no live parent binding"
	if len(b.Scopes) > 0 {
		holds = "thread " + b.Thread + " is the live parent of project " + strings.Join(b.Scopes, ", ")
	}
	return Warning{Rule: "project_key_bound", Detail: fmt.Sprintf("the plan's project key is %s; %s: a plan's project key is the scope_key of the orchestrator's parent binding, and the run's first claim is refused (scope_role_mismatch) under another key", key, holds)}, true
}
