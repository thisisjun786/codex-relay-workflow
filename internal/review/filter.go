// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/filter/filter.go and internal/agent/nonfinding.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package review

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

var nonFindingPhrases = []string{
	"no issues", "no findings", "no bugs", "no problems",
	"looks good", "code looks clean", "code looks correct", "review complete",
}

// IsNonFindingText reports whether text is a reviewer's statement that there is nothing to report ("no issues found") rather than
// a finding. The phrase list and the case-insensitive substring match are ACR's.
func IsNonFindingText(text string) bool {
	lower := strings.ToLower(text)
	return slices.ContainsFunc(nonFindingPhrases, func(phrase string) bool { return strings.Contains(lower, phrase) })
}

// isNonFinding tests the title (the explanation only when there is no title); a finding with neither has nothing to report.
func isNonFinding(f Finding) bool {
	text := f.Title
	if strings.TrimSpace(text) == "" {
		text = f.Explanation
	}
	return strings.TrimSpace(text) == "" || IsNonFindingText(text)
}

var defaultNoisePatterns = []string{
	`(^|/)(go\.sum|package-lock\.json|yarn\.lock|pnpm-lock\.yaml|Cargo\.lock|poetry\.lock|Gemfile\.lock|composer\.lock)$`,
	`(^|/)(vendor|node_modules)/`,
	`\.min\.(js|css)$`,
	`(\.pb\.go|_generated\.go|\.generated\.[a-z]+)$`,
}

// NoiseFilter matches files no review is wanted for: lock files, vendored code, minified and generated files. ACR's filter matched
// the text of a finding against regular expressions; this one matches the finding's file path.
type NoiseFilter struct{ patterns []*regexp.Regexp }

// NewNoiseFilter compiles the built-in patterns and extra, and refuses an extra pattern that is not a valid regular expression.
func NewNoiseFilter(extra ...string) (*NoiseFilter, error) {
	var compiled []*regexp.Regexp
	for _, p := range append(slices.Clone(defaultNoisePatterns), extra...) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, re)
	}
	return &NoiseFilter{patterns: compiled}, nil
}

// Match reports whether path (slash separated, relative to the repository root) is a noise file.
func (f *NoiseFilter) Match(path string) bool {
	return slices.ContainsFunc(f.patterns, func(re *regexp.Regexp) bool { return re.MatchString(path) })
}

// Rules are the deterministic rules applied to the findings after grouping and verification. Changed is the set of files the
// base...head diff changes (clean slash paths relative to the repository root; finding paths are cleaned, these keys are not);
// Head must not be nil; Noise nil means the built-in patterns.
type Rules struct {
	Changed map[string]bool
	Head    HeadReader
	Noise   *NoiseFilter
}

// Apply runs the rules on each finding in order and the first that fires drops it with its reason: non-finding text, noise file,
// location (invalid, not in the diff, missing at head, beyond the end of the file), unknown grade, then the confidence threshold.
// A kept finding has its path cleaned and, if its verdict is not one of the four, the verdict unverified. A drop keeps the finding as
// received. Apply returns an error, and no result, only when Head fails with an error other than not-exist.
func (r Rules) Apply(in []Finding) (Result, error) {
	if r.Head == nil {
		return Result{}, errors.New("review: Rules.Head is nil")
	}
	res := Result{Findings: []Finding{}, Dropped: []Drop{}}
	noise := r.Noise
	if noise == nil {
		noise, _ = NewNoiseFilter() // the built-in patterns compile
	}
	for _, f := range in {
		if !slices.Contains(allVerdicts, f.Verdict) {
			f.Verdict = VerdictUnverified
		}
		reason, detail, err := r.classify(f, noise)
		if err != nil {
			return Result{}, err
		}
		if reason != "" {
			res.Dropped = append(res.Dropped, Drop{Finding: f, Reason: reason, Detail: detail})
			continue
		}
		f.File = path.Clean(f.File)
		res.Findings = append(res.Findings, f)
	}
	return res, nil
}

func (r Rules) classify(f Finding, noise *NoiseFilter) (DropReason, string, error) {
	file := path.Clean(f.File)
	switch {
	case isNonFinding(f):
		return ReasonNonFinding, "", nil
	case noise.Match(file):
		return ReasonNoiseFile, file, nil
	}
	if reason, detail, err := r.locate(file, f); reason != "" || err != nil {
		return reason, detail, err
	}
	if f.Grade == "" {
		return ReasonUnknownGrade, fmt.Sprintf("reported severity %q", f.Severity), nil
	}
	if d := Decide(f.Verdict, f.Support, f.Security || f.Grade == P0 || f.Grade == P1); !d.Keep {
		return d.Reason, fmt.Sprintf("verdict %s, support %d", f.Verdict, f.Support), nil
	}
	return "", "", nil
}
