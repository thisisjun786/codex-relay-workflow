// Package lint is `crw flow lint`: it checks a workflow script against the rules the engine cannot enforce early. The
// static rules read the script's text (an AST of the body with scopes resolved); the dry-run rules read the journal of
// the script run on the fake backend. A finding both find is reported once.
package lint

import (
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// Options configure a check.
type Options struct {
	// Policy resolves the roles of the script's calls; nil is the fake backend's placeholder table.
	Policy *engine.Policy
	// DryRun runs the script on the fake backend and reads the journal.
	DryRun bool
	// Args is the JSON the dry run gives the script as args.
	Args json.RawMessage
}

// Report is what a check found in one script.
type Report struct {
	Script      string       `json:"script"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	// DryRun says how the dry run went: "ran", "skipped" or a note on why it ended early.
	DryRun string `json:"dryRun"`
}

// OK reports whether the script has no finding that fails lint. Same-thread review is a warning recorded for the operator.
func (r Report) OK() bool {
	for _, d := range r.Diagnostics {
		if d.Rule != RuleReviewerThreadSame {
			return false
		}
	}
	return true
}

// Check lints one script.
func Check(ctx context.Context, src script.Source, opts Options) Report {
	report := Report{Script: src.Name, Diagnostics: []Diagnostic{}, DryRun: "skipped"}
	policy := opts.Policy
	if policy == nil {
		policy = engine.FakePolicy()
	}
	prog, err := script.Load(src)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Rule: RuleLoad, Message: err.Error(), Source: sourceStatic})
		return report
	}
	var found []Diagnostic
	a, err := parse(prog.Body, prog.Imports)
	if err != nil {
		found = append(found, Diagnostic{Rule: RuleParse, Message: "the static rules could not read the script: " + err.Error(), Source: sourceStatic})
	} else {
		s := &scanner{a: a, policy: policy, out: &found}
		s.scan(a.prog, nil)
		s.flow()
	}
	found = dedupe(found)
	if opts.DryRun {
		traced, note := dryRun(ctx, prog, src, policy, opts.Args)
		report.DryRun = note
		for _, d := range traced {
			if !covered(found, d) {
				found = append(found, d)
			}
		}
	}
	sort.SliceStable(found, func(i, k int) bool {
		if found[i].Source != found[k].Source {
			return found[i].Source == sourceStatic
		}
		return found[i].Line < found[k].Line
	})
	report.Diagnostics = append(report.Diagnostics, found...)
	return report
}

// dedupe drops the findings that repeat another's rule, line and message (a loop body is walked more than once).
func dedupe(in []Diagnostic) []Diagnostic {
	seen := map[Diagnostic]bool{}
	var out []Diagnostic
	for _, d := range in {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// covered reports whether a static finding already says what a trace finding says: the same rule on the same label,
// or, for a finding of the whole run (it names no call), the same rule anywhere.
func covered(static []Diagnostic, d Diagnostic) bool {
	for _, s := range static {
		if s.Rule == d.Rule && (s.Message == d.Message || d.Label == "" || s.Label != "" && s.Label == d.Label) {
			return true
		}
	}
	return false
}

// dryRun runs the script on the fake backend and reads what the journal shows. A script that ends in an error the rules
// do not name (it reads an arg the dry run was not given, it calls a crw.* operation this build does not have) is not a
// finding: the trace up to that point is still read, and the note says how the run ended.
func dryRun(ctx context.Context, prog *script.Program, src script.Source, policy *engine.Policy, args json.RawMessage) ([]Diagnostic, string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	journal := engine.NewMemoryJournal()
	cfg := engine.Config{
		Backend: engine.NewFake(), Policy: policy, Journal: journal, Args: args, OmitSeq: true,
		Workflows: func(name, scriptPath string) (*script.Program, error) { return sibling(src.Dir, name, scriptPath) },
	}
	res, err := engine.Run(ctx, prog, cfg)
	if err != nil {
		return nil, "skipped: " + err.Error()
	}
	diags := traceDiagnostics(res, journal.Events(), policy)
	note := "ran"
	if res.Status != engine.RunCompleted {
		note = "ended " + res.Status + " (" + res.ErrorKind + "): " + res.Error
	}
	return diags, note
}

// sibling resolves workflow(name) and workflow({scriptPath}) inside the script's own directory, as crw flow run does.
func sibling(dir fs.FS, name, scriptPath string) (*script.Program, error) {
	if dir == nil {
		return nil, errNoDir
	}
	rel := scriptPath
	if rel == "" {
		rel = name
		if path.Ext(rel) != ".js" {
			rel += ".js"
		}
	}
	rel = path.Clean(rel)
	if !fs.ValidPath(rel) {
		return nil, errOutside(rel)
	}
	text, err := fs.ReadFile(dir, rel)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(dir, path.Dir(rel))
	if err != nil {
		return nil, err
	}
	return script.Load(script.Source{Name: rel, Text: text, Dir: sub})
}
