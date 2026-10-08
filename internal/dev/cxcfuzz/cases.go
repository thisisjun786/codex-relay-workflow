//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Case is one pinned replay case: an input, both sides' answers, and the tag that says how it is
// judged. A case lives in the target's testdata directory and is replayed with no Node.
type Case struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	Oracle string `json:"oracle"`
	Go     string `json:"go"`
	Tag    string `json:"tag"`
	Record string `json:"record"`
}

const (
	// TagIdentical: the Go side must agree with the oracle's recorded answer.
	TagIdentical = "identical"
	// TagIntentionallyChanged: the Go side must agree with the answer the record names.
	TagIntentionallyChanged = "intentionally-changed"
	// TagOpen: an unfixed difference, pinned at the Go output it prints today.
	TagOpen = "open"
)

// CasesFile is the file a target's cases live in.
const CasesFile = "cases.json"

// LoadCases reads a target's cases; a missing file is no cases.
func LoadCases(dir string) ([]Case, error) {
	path := filepath.Join(dir, CasesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cases, nil
}

// SaveCases writes a target's cases.
func SaveCases(dir string, cases []Case) error {
	raw, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, CasesFile), append(raw, '\n'), 0o644)
}

// CheckCase replays one case through the Go side only, with no Node and no worker, and returns a
// problem description, or "" when the case holds. The case root is prepared exactly as a campaign
// prepares it, so a target that opens a file under one of the homes, or under TMPDIR, sees the
// same directories during replay as it saw when the answer was recorded.
func CheckCase(target Target, c Case) (problem string) {
	input, err := decode(c.Input)
	if err != nil {
		return fmt.Sprintf("the input is not JSON: %v", err)
	}
	root, err := os.MkdirTemp("", "cxcfuzz-case-")
	if err != nil {
		return err.Error()
	}
	// The cleanup error is reported rather than discarded: a case root that survived replay says the
	// harness left a tree on the host, which is not a replay result.
	defer func() {
		if err := CleanupCaseRoot(root); err != nil {
			problem = joinProblem(problem, fmt.Sprintf("the case root was not removed: %v", err))
		}
	}()
	if err := PrepareRoot(root); err != nil {
		return fmt.Sprintf("the case root was not prepared: %v", err)
	}
	if _, err := Scenarios(root, input); err != nil {
		return fmt.Sprintf("the scenario is refused: %v", err)
	}
	value, err := target.Go(input, RootEnv(root))
	if err != nil {
		// The campaign answers a Go failure with the error's value, so replay compares the same
		// shape rather than treating a recorded error as a replay failure.
		value = errorValue(err)
	}
	got := canonical(stripRoot(value, root))
	switch c.Tag {
	case TagIdentical:
		if got != canonicalText(c.Oracle) {
			return fmt.Sprintf("go %s, oracle %s", got, canonicalText(c.Oracle))
		}
	case TagIntentionallyChanged:
		if c.Record == "" {
			return "an intentionally-changed case needs a record"
		}
		if got != canonicalText(c.Go) {
			return fmt.Sprintf("go %s, pinned %s", got, canonicalText(c.Go))
		}
	case TagOpen:
		if got != canonicalText(c.Go) {
			return fmt.Sprintf("go %s, pinned %s", got, canonicalText(c.Go))
		}
	default:
		return fmt.Sprintf("unknown tag %q", c.Tag)
	}
	return ""
}

// joinProblem folds a cleanup problem into a replay's own problem, so neither hides the other.
func joinProblem(problem, cleanup string) string {
	if cleanup == "" {
		return problem
	}
	if problem == "" {
		return cleanup
	}
	return problem + "; " + cleanup
}

// Adopt pins one divergence as a case in the target's cases.json.
func Adopt(target Target, divergencePath, tag, record, name string) error {
	switch tag {
	case TagIdentical, TagIntentionallyChanged, TagOpen:
	default:
		return fmt.Errorf("unknown tag %q", tag)
	}
	raw, err := os.ReadFile(divergencePath)
	if err != nil {
		return err
	}
	var d Divergence
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("%s: %w", divergencePath, err)
	}
	if name == "" {
		name = string(d.Kind)
	}
	dir, err := testdataDir(target.Name)
	if err != nil {
		return err
	}
	cases, err := LoadCases(dir)
	if err != nil {
		return err
	}
	cases = append(cases, Case{Name: name, Input: d.Input, Oracle: d.Oracle, Go: d.Go, Tag: tag, Record: record})
	return SaveCases(dir, cases)
}

// testdataDir is the target's testdata directory in this checkout.
func testdataDir(target string) (string, error) {
	root, err := repositoryRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "internal", "dev", "cxcfuzz", "testdata", target), nil
}

// canonicalText is canonical for JSON text, and the text itself when it is not JSON: a recorded
// answer is stored as the text both sides exchanged, and compared in the same canonical form.
func canonicalText(text string) string {
	value, err := decode(text)
	if err != nil {
		return text
	}
	return canonical(value)
}
