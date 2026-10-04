package adjudication

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

var (
	sha        = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	hash       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func (p PR) validate() error {
	if !repository.MatchString(p.Repository) || p.Number < 1 || !sha.MatchString(p.Head) {
		return fmt.Errorf("invalid PR identity: %+v", p)
	}
	return nil
}

func (r Reviewer) validate() error {
	if !slices.Contains([]Source{Independent, Devin, Codex}, r.Source) || strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("invalid reviewer: %+v", r)
	}
	return nil
}

func grade(g review.Grade) bool {
	return slices.Contains([]review.Grade{review.P0, review.P1, review.P2, review.P3}, g)
}
func text(s string) bool { return strings.TrimSpace(s) != "" }

func (r Record) validate() error {
	if r.Schema != Schema || !text(r.ID) || (r.Run == nil) == (r.Judgment == nil) {
		return fmt.Errorf("record needs schema, id and exactly one of run/judgment")
	}
	if r.Run != nil {
		return r.Run.validate()
	}
	j := r.Judgment
	if !text(j.RunID) || !text(j.FindingID) || !text(j.Parent) || !text(j.Problem) || !text(j.Note) || !grade(j.Grade) || !slices.Contains([]Verdict{Correct, FalsePositive, Minor}, j.Verdict) {
		return fmt.Errorf("judgment needs finding reference, parent, problem, verdict, grade and note")
	}
	changed := j.CodeChanged != nil && *j.CodeChanged
	if changed && !sha.MatchString(j.ChangeCommit) || !changed && j.ChangeCommit != "" {
		return fmt.Errorf("changeCommit must be present exactly when codeChanged is true")
	}
	return nil
}

func (r Run) validate() error {
	if err := r.PR.validate(); err != nil {
		return err
	}
	if err := r.Reviewer.validate(); err != nil {
		return err
	}
	if !text(r.ID) || !slices.Contains([]string{"complete", "partial", "invalid", "unavailable"}, r.Status) || (r.Status == "complete") != (r.Reason == "") {
		return fmt.Errorf("invalid run identity/status/reason")
	}
	if r.Status != "complete" && !text(r.Reason) {
		return fmt.Errorf("failed/partial run needs a reason")
	}
	if r.Source == Independent && !hash.MatchString(r.ArtifactSHA256) || r.Source != Independent && r.ArtifactSHA256 != "" {
		return fmt.Errorf("artifactSha256 required only for crw_review")
	}
	if r.Reviewers != nil && *r.Reviewers < 0 || r.Findings == nil || (r.Status == "invalid" || r.Status == "unavailable") && len(r.Findings) > 0 {
		return fmt.Errorf("invalid reviewer count or findings for run status")
	}
	ids := map[string]bool{}
	for _, f := range r.Findings {
		if !text(f.ID) || ids[f.ID] || !text(f.Title) || f.Line < 1 || !grade(f.Grade) || !filepath.IsLocal(f.File) || path.Clean(f.File) != f.File || strings.Contains(f.File, `\`) {
			return fmt.Errorf("invalid or duplicate finding %q", f.ID)
		}
		ids[f.ID] = true
	}
	for _, call := range r.Calls {
		c, t := call.Record, call.Record.Tokens
		if !slices.Contains([]string{"normal", "invalid", "unavailable"}, c.Class) || (c.Class == "normal") != (c.Reason == "") || c.Class != "normal" && !text(c.Reason) || min(c.ElapsedMillis, t.Input, t.Output, t.Thinking, t.CacheRead, t.Total) < 0 {
			return fmt.Errorf("invalid call class/reason/accounting")
		}
	}
	return nil
}

type findingRef struct{ run, finding string }
type problemRef struct {
	pr      PR
	problem string
}
type ledger struct {
	runs      map[string]Run
	findings  map[findingRef]Finding
	judgments map[findingRef]Judgment
}

func replay(records []Record) (ledger, error) {
	l := ledger{map[string]Run{}, map[findingRef]Finding{}, map[findingRef]Judgment{}}
	ids := map[string]bool{}
	artifacts := map[struct {
		PR   PR
		Hash string
	}]bool{}
	var totals [6]int64
	var reviewers int
	for i, r := range records {
		if err := r.validate(); err != nil {
			return l, fmt.Errorf("record %d: %w", i+1, err)
		}
		if ids[r.ID] {
			return l, fmt.Errorf("duplicate record id %q", r.ID)
		}
		ids[r.ID] = true
		if r.Run != nil {
			run := *r.Run
			if run.Reviewers != nil {
				if *run.Reviewers > int(^uint(0)>>1)-reviewers {
					return l, fmt.Errorf("reviewer total overflows")
				}
				reviewers += *run.Reviewers
			}
			for _, call := range run.Calls {
				c, t := call.Record, call.Record.Tokens
				for i, v := range [6]int64{c.ElapsedMillis, t.Input, t.Output, t.Thinking, t.CacheRead, t.Total} {
					if v > (1<<63-1)-totals[i] {
						return l, fmt.Errorf("call accounting total overflows")
					}
					totals[i] += v
				}
			}
			if _, exists := l.runs[run.ID]; exists {
				return l, fmt.Errorf("duplicate run id %q", run.ID)
			}
			key := struct {
				PR   PR
				Hash string
			}{run.PR, run.ArtifactSHA256}
			if run.Source == Independent && artifacts[key] {
				return l, fmt.Errorf("duplicate artifact for PR %+v", run.PR)
			}
			if run.Source == Independent {
				artifacts[key] = true
			}
			l.runs[run.ID] = run
			for _, f := range run.Findings {
				l.findings[findingRef{run.ID, f.ID}] = f
			}
			continue
		}
		j := *r.Judgment
		ref := findingRef{j.RunID, j.FindingID}
		if _, exists := l.findings[ref]; !exists {
			return l, fmt.Errorf("judgment references unknown finding %+v", ref)
		}
		if j.CodeChanged != nil && *j.CodeChanged && j.ChangeCommit == l.runs[j.RunID].PR.Head {
			return l, fmt.Errorf("changeCommit is the reviewed head, not a later change")
		}
		l.judgments[ref] = j
	}
	truth := map[problemRef]Judgment{}
	// Check final effective judgments, not historical intermediate states in a correction batch.
	for _, r := range records {
		if r.Judgment == nil {
			continue
		}
		j := l.judgments[findingRef{r.Judgment.RunID, r.Judgment.FindingID}]
		if j.Verdict != Correct {
			continue
		}
		key := problemRef{l.runs[j.RunID].PR, j.Problem}
		if old, exists := truth[key]; exists && (old.Grade != j.Grade || old.Security != j.Security) {
			return l, fmt.Errorf("conflicting parent severity for problem %q", j.Problem)
		}
		truth[key] = j
	}
	return l, nil
}
