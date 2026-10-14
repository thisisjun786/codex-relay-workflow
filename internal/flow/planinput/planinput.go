// Package planinput is `crw flow plan-input`: it builds the handoff `crw flow compile` reads (crw-flow-compile-input/1) from one
// milestone of the tracking repository, through internal/tracking.
//
// A project owner takes one milestone; the plan's project key is the milestone ref (OWNER/NAME#N, the identifier form of the tracking
// package), each open issue of the milestone is one issue line keyed by its ref (OWNER/NAME#N), and the order of the issues is the
// "Blocked by:" lines of their bodies. The ref is the identity; the compile input spells it OWNER:NAME:N (enc.go), because the relay dag
// reads the keys as identifiers, which a '/' and a '#' are not. The envelope and the warnings show the refs. The command only reads: the
// Reader it takes has a Milestone method and nothing that writes, and the tracking adapter sends GET requests for it.
//
// The issue body is read for the fields of an issue line from a small fixed set of headings (see body.go):
//
//	criteria  a bullet list under 끝 조건, Done when, Acceptance or Acceptance criteria (a bullet indented under another is part of that
//	          criterion); with none the issue title is the one criterion
//	regions   a bullet list under 수정 범위, Edit regions or Regions (a path in backticks, or the first word; a trailing slash is a tree);
//	          with none the issue has no region and the compile default applies
//	size      "size_fit: V", "size_reason: V" and "issue_size: V" lines under 크기, Size or Size fit; with none compile's defaults apply
//
// What cannot be mapped is a warning, never a refusal: a missing criteria section, a region compile would refuse, a size value outside
// its closed set, a blocker that is closed, outside the milestone or the issue itself, a milestone that is closed or has no open issue,
// and a project key the owner thread holds no live parent binding of (the comparison `crw flow start` makes as project_key_bound,
// CRW-1317). Only an input error (a Ref that is not OWNER/NAME#N, a milestone that does not exist, a key the dag cannot hold) stops the command.
package planinput

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
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
	// Refs maps each key of Input (OWNER:NAME:N) to its ref (OWNER/NAME#N).
	Refs map[string]string
}

// Envelope is the report `--envelope` prints: the input beside the warnings array that records what the mapping left out.
type Envelope struct {
	Schema    string        `json:"schema"`
	Milestone string        `json:"milestone"`
	Input     compile.Input `json:"input"`
	// Refs maps each key of the input back to its OWNER/NAME#N ref.
	Refs     map[string]string `json:"refs"`
	Warnings []Warning         `json:"warnings"`
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
	var problems []string
	seenProblem := map[string]bool{}
	refs := map[string]string{}
	encode := func(r tracking.Ref) string {
		k, err := EncodeKey(r)
		if err != nil {
			if !seenProblem[err.Error()] {
				seenProblem[err.Error()] = true
				problems = append(problems, err.Error())
			}
			return ""
		}
		refs[k] = r.String()
		return k
	}
	pkey := encode(ref)

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

	project := compile.Project{Key: pkey, Target: opts.Target, Issues: []compile.Issue{}}
	ordered := false
	for _, is := range open {
		line, w := issueLine(is, byRef, key, encode)
		warnings = append(warnings, w...)
		ordered = ordered || len(line.After) > 0
		project.Issues = append(project.Issues, line)
	}
	// compile names the order edge "e:FROM:TO", and "e:FROM:TO.N" (N from 2) when the plan is revised after the edge was retired; the dag
	// holds both to the same limit as a key, so the id of the first plan must leave room for the ".2" of the first re-plan
	for _, line := range project.Issues {
		for _, a := range line.After {
			if line.Key == "" || a == "" {
				continue
			}
			n := edgeIDLength(a, line.Key)
			switch {
			case n > dag.MaxIDLength:
				problems = append(problems, fmt.Sprintf("the order of %s after %s is the dag edge \"e:%s:%s\" of %d characters, over the %d the relay dag allows for an identifier: use a repository with a shorter owner/name", refs[line.Key], refs[a], a, line.Key, n, dag.MaxIDLength))
			case n+len(ReplanEdgeSuffix) > dag.MaxIDLength:
				problems = append(problems, fmt.Sprintf("the order of %s after %s is the dag edge \"e:%s:%s\" of %d characters, and a re-plan of the plan names the edge again with a %q suffix (%d characters), over the %d the relay dag allows for an identifier: use a repository with a shorter owner/name", refs[line.Key], refs[a], a, line.Key, n, ReplanEdgeSuffix, n+len(ReplanEdgeSuffix), dag.MaxIDLength))
			}
		}
	}
	if len(problems) > 0 {
		return Result{}, fmt.Errorf("milestone %s cannot be written as a plan, nothing is built: %s", key, strings.Join(problems, "; "))
	}
	if ordered && opts.Target == nil {
		warn("target_missing", "", "compile refuses an order between issues that integrate code when the project has no target: pass --target-repository (the absolute path of the checkout) and --base-ref (the branch)")
	}
	if b := opts.Binding; b != nil {
		if w, ok := keyWarning(pkey, key, *b); ok {
			warnings = append(warnings, w)
		}
	}
	return Result{
		Input:    compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{project}},
		Warnings: warnings,
		Refs:     refs,
	}, nil
}

// refKey is a Ref as a map key: the repository compared without regard to case.
func refKey(r tracking.Ref) string { return strings.ToLower(r.Repo) + "#" + fmt.Sprint(r.Number) }

// issueLine maps one open issue to its issue line and the warnings of the mapping. The line is keyed by encode(ref): the dag-safe form.
func issueLine(is tracking.Issue, byRef map[string]tracking.Issue, project string, encode func(tracking.Ref) string) (compile.Issue, []Warning) {
	var warnings []Warning
	key := is.Ref.String()
	warn := func(rule, format string, a ...any) {
		warnings = append(warnings, Warning{Rule: rule, Issue: key, Detail: fmt.Sprintf(format, a...)})
	}
	line := compile.Issue{Key: encode(is.Ref), Title: is.Title}
	secs := sections(is.Body)

	line.Criteria = criteria(secs)
	if len(line.Criteria) == 0 {
		line.Criteria = []string{is.Title}
		warn("criteria_missing", "the body has no bullet list under 끝 조건, Done when, Acceptance or Acceptance criteria: the title is the one criterion")
	}
	if items, _ := linesOf(secs, regionHeadings); len(items) > 0 {
		seen := map[string]bool{}
		for _, item := range bullets(items, false) {
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
			line.After = append(line.After, encode(dep.Ref))
		}
	}
	return line, warnings
}

// keyWarning compares the project key (the encoded form, the scope key `crw flow start` compares) with the scope keys of the owner's
// live parent bindings. A key the thread is bound to answers nothing. ref is the milestone ref the key stands for.
func keyWarning(key, ref string, b Binding) (Warning, bool) {
	switch {
	case b.Thread == "":
		return Warning{Rule: "project_key_unchecked", Detail: "the project key is not compared with a binding: pass --thread (or run in a session that has CODEX_THREAD_ID) to name the owner thread"}, true
	case b.Err != nil:
		return Warning{Rule: "project_key_unchecked", Detail: fmt.Sprintf("the live parent bindings of thread %s cannot be read, so the project key %s (milestone %s) is not compared: %v", b.Thread, key, ref, b.Err)}, true
	}
	// exactly, as `crw flow start` and the coordinator's first claim compare it: a key that differs only by case is another key there
	caseOnly := ""
	for _, s := range b.Scopes {
		switch {
		case s == key:
			return Warning{}, false
		case caseOnly == "" && strings.EqualFold(s, key):
			caseOnly = s
		}
	}
	holds := "thread " + b.Thread + " holds no live parent binding"
	if len(b.Scopes) > 0 {
		shown := make([]string, len(b.Scopes))
		for i, s := range b.Scopes {
			shown[i] = s
			if r, err := DecodeKey(s); err == nil {
				shown[i] = s + " (" + r.String() + ")"
			}
		}
		holds = "thread " + b.Thread + " is the live parent of project " + strings.Join(shown, ", ")
	}
	hint := ""
	if caseOnly != "" {
		hint = fmt.Sprintf(" (the binding %s differs from %s only by case, and the keys are compared exactly: pass --milestone with the same spelling the binding was made under)", caseOnly, key)
	}
	return Warning{Rule: "project_key_bound", Detail: fmt.Sprintf("the plan's project key is %s (milestone %s); %s%s: a plan's project key is the scope_key of the orchestrator's parent binding, and the run's first claim is refused (scope_role_mismatch) under another key", key, ref, holds, hint)}, true
}
