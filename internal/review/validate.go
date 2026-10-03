package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FieldError names the field a refusal is about, as a path with array indexes, for example findings[2].grade.
type FieldError struct{ Field, Problem string }

func (e FieldError) Error() string { return e.Field + ": " + e.Problem }

// ValidationError lists every problem found in an artifact; ParseArtifact, Validate and Marshal return it.
type ValidationError []FieldError

func (e ValidationError) Error() string {
	parts := make([]string, len(e))
	for i, fe := range e {
		parts[i] = fe.Error()
	}
	return "invalid review artifact: " + strings.Join(parts, "; ")
}

var (
	commitID   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	arrayIndex = regexp.MustCompile(`\.(\d+)(\.|$)`) // encoding/json names a field in an array element findings.0.line
)

// ParseArtifact decodes and validates an artifact. The whole input must be one JSON object whose objects have exactly the keys of
// the schema, spelled exactly (encoding/json alone would accept GRADE for grade) and none null; a value of the wrong type is
// refused with its path; then Validate runs. Duplicate keys are not detected: the last wins.
func ParseArtifact(data []byte, expectedHead string) (*Artifact, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, ValidationError{{"", err.Error()}}
	}
	if _, ok := raw.(map[string]any); !ok {
		return nil, ValidationError{{"", "must be a JSON object"}}
	}
	var errs ValidationError
	walkShape(reflect.TypeFor[Artifact](), raw, "", &errs)
	if len(errs) > 0 {
		return nil, errs
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return nil, ValidationError{{arrayIndex.ReplaceAllString(te.Field, "[$1]$2"), fmt.Sprintf("is a JSON %s, want %s", te.Value, te.Type)}}
		}
		return nil, ValidationError{{"", err.Error()}}
	}
	if err := a.Validate(expectedHead); err != nil {
		return nil, err
	}
	return &a, nil
}

// walkShape compares the decoded JSON v with the struct type t: every key must be a json tag and every tag without omitempty must
// be present and not null. Values of the wrong type are left to the typed decode.
func walkShape(t reflect.Type, v any, p string, errs *ValidationError) {
	join := func(k string) string { return strings.TrimPrefix(p+"."+k, ".") }
	add := func(field, problem string) { *errs = append(*errs, FieldError{field, problem}) }
	switch obj, _ := v.(map[string]any); t.Kind() {
	case reflect.Slice:
		items, _ := v.([]any)
		for i, item := range items {
			if item == nil {
				add(fmt.Sprintf("%s[%d]", p, i), "must not be null")
				continue
			}
			walkShape(t.Elem(), item, fmt.Sprintf("%s[%d]", p, i), errs)
		}
	case reflect.Struct:
		known := map[string]bool{}
		for i := range t.NumField() {
			name, opt, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			known[name] = true
			child, present := obj[name]
			switch {
			case !present && opt != "omitempty":
				add(join(name), "is required")
			case present && child == nil && opt != "omitempty":
				add(join(name), "must not be null")
			case present:
				walkShape(t.Field(i).Type, child, join(name), errs)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			if !known[k] {
				add(join(k), "unknown field")
			}
		}
	}
}

type badFn func(field, format string, args ...any)

// Validate checks the contract in the package documentation. expectedHead is the commit the caller reviewed; an artifact for any
// other head is refused. It returns a ValidationError or nil.
func (a *Artifact) Validate(expectedHead string) error {
	var errs ValidationError
	bad := func(field, format string, args ...any) {
		errs = append(errs, FieldError{field, fmt.Sprintf(format, args...)})
	}
	if a.Schema != SchemaV1 {
		bad("schema", "is %q, want %q", a.Schema, SchemaV1)
	}
	for _, s := range []struct{ name, v string }{{"tool", a.Tool}, {"toolVersion", a.ToolVersion}, {"model", a.Model}, {"effort", a.Effort}, {"agyVersion", a.AgyVersion}} {
		if strings.TrimSpace(s.v) == "" {
			bad(s.name, "must not be blank")
		}
	}
	for _, s := range []struct{ name, v string }{{"base", a.Base}, {"head", a.Head}, {"patchId", a.PatchID}} {
		if !commitID.MatchString(s.v) {
			bad(s.name, "must be 40 or 64 lowercase hex digits")
		}
	}
	if !commitID.MatchString(expectedHead) {
		bad("head", "no valid expected head was given")
	} else if a.Head != expectedHead {
		bad("head", "is %q but the expected head is %q", a.Head, expectedHead)
	}
	if min(a.Diff.Files, a.Diff.Additions, a.Diff.Deletions) < 0 {
		bad("diff", "counts must not be negative")
	}
	r := a.Reviewers
	returned := r.Run - r.Failed // the reviewers that gave a result
	if min(r.Run, r.Failed, r.TimedOut, r.AuthFailed) < 0 || r.Failed > r.Run || r.TimedOut > r.Failed-r.AuthFailed {
		bad("reviewers", "need 0 <= timedOut + authFailed <= failed <= run, have %+v", r)
		returned = 0
	}
	switch {
	case a.Status == StatusComplete && (r.Run < 1 || r.Failed != 0):
		bad("status", "complete needs run >= 1 and no failed reviewer")
	case a.Status == StatusPartial && returned < 1:
		bad("status", "partial needs a reviewer that returned a result")
	case a.Status == StatusUnavailable && (returned != 0 || len(a.Findings)+len(a.Dropped) > 0):
		bad("status", "unavailable needs no reviewer that returned a result and no findings")
	case !slices.Contains(allStatuses, a.Status):
		bad("status", "is %q, want one of %v", a.Status, allStatuses)
	}
	if a.Status == StatusComplete && a.Reason != "" {
		bad("reason", "must be empty when the status is complete")
	} else if a.Status != StatusComplete && strings.TrimSpace(a.Reason) == "" {
		bad("reason", "is required unless the status is complete")
	}
	started, errStart := time.Parse(time.RFC3339, a.StartedAt)
	finished, errEnd := time.Parse(time.RFC3339, a.FinishedAt)
	if errStart != nil {
		bad("startedAt", "must be an RFC 3339 time")
	}
	if errEnd != nil {
		bad("finishedAt", "must be an RFC 3339 time")
	} else if errStart == nil && finished.Before(started) {
		bad("finishedAt", "is before startedAt")
	}
	for i, f := range a.Findings {
		checkKept(fmt.Sprintf("findings[%d]", i), f, returned, bad)
	}
	for i, d := range a.Dropped { // a drop keeps the finding as received, so only its grade, verdict and provenance are checked
		p, f := fmt.Sprintf("dropped[%d]", i), d.Finding
		if !slices.Contains(allReasons, d.Reason) {
			bad(p+".reason", "is %q, want one of %v", d.Reason, allReasons)
		}
		if f.Grade != "" && (!slices.Contains(allGrades, f.Grade) || d.Reason == ReasonUnknownGrade) {
			bad(p+".finding.grade", "is %q: P0 to P3, or empty (and always empty for unknown_grade)", f.Grade)
		}
		if !slices.Contains(allVerdicts, f.Verdict) {
			bad(p+".finding.verdict", "is %q, want one of %v", f.Verdict, allVerdicts)
		}
		checkReviewers(p+".finding", f, returned, bad)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// checkReviewers checks the provenance every finding carries: strictly ascending reviewers, and a support equal to their count that
// no more reviewers than returned a result can give.
func checkReviewers(p string, f Finding, returned int, bad badFn) {
	for i, id := range f.Reviewers {
		if id < 0 || i > 0 && id <= f.Reviewers[i-1] {
			bad(p+".reviewers", "must be non-negative and strictly ascending")
			break
		}
	}
	if f.Support != len(f.Reviewers) {
		bad(p+".support", "is %d but %d reviewers are listed", f.Support, len(f.Reviewers))
	} else if f.Support > returned {
		bad(p+".support", "%d exceeds the %d reviewers that returned a result", f.Support, returned)
	}
}

// checkKept is the strict check of a finding in the artifact's findings.
func checkKept(p string, f Finding, returned int, bad badFn) {
	if clean := path.Clean(f.File); f.File == "" || clean != f.File || !filepath.IsLocal(clean) || strings.Contains(f.File, `\`) {
		bad(p+".file", "must be a clean relative slash path inside the repository, have %q", f.File)
	}
	if f.Line < 1 {
		bad(p+".line", "must be at least 1")
	}
	if f.EndLine != 0 && f.EndLine < f.Line {
		bad(p+".endLine", "must be 0 or at least line")
	}
	for _, s := range []struct{ name, v string }{{"title", f.Title}, {"explanation", f.Explanation}, {"perspective", f.Perspective}} {
		if strings.TrimSpace(s.v) == "" {
			bad(p+"."+s.name, "must not be blank")
		}
	}
	if !slices.Contains(allGrades, f.Grade) {
		bad(p+".grade", "is %q, want one of %v", f.Grade, allGrades)
	}
	if !slices.Contains(allVerdicts, f.Verdict) {
		bad(p+".verdict", "is %q, want one of %v", f.Verdict, allVerdicts)
	} else if d := Decide(f.Verdict, f.Support, isSevere(f)); !d.Keep {
		bad(p+".verdict", "%s with support %d is dropped by the confidence threshold (%s), not kept", f.Verdict, f.Support, d.Reason)
	}
	if len(f.Reviewers) == 0 {
		bad(p+".reviewers", "must not be empty")
	}
	checkReviewers(p, f, returned, bad)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Marshal validates a against expectedHead and returns its indented JSON with a final newline. Nil slices are written as empty
// arrays, which the schema requires; a is not modified.
func (a Artifact) Marshal(expectedHead string) ([]byte, error) {
	a.Findings, a.Dropped = slices.Clone(nonNil(a.Findings)), slices.Clone(nonNil(a.Dropped))
	for i := range a.Findings {
		a.Findings[i].Reviewers = nonNil(a.Findings[i].Reviewers)
	}
	for i := range a.Dropped {
		a.Dropped[i].Finding.Reviewers = nonNil(a.Dropped[i].Finding.Reviewers)
	}
	if err := a.Validate(expectedHead); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // the parent reads this file: keep < and & as written
	enc.SetIndent("", "  ")
	err := enc.Encode(a)
	return buf.Bytes(), err
}
