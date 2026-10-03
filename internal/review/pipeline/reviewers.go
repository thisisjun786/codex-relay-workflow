// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/runner/runner.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package pipeline

import (
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
)

// Perspective chooses fixed instructions; arbitrary caller prompt text is refused.
type Perspective string

const (
	Correctness  Perspective = "correctness"
	Security     Perspective = "security"
	Contracts    Perspective = "contract and compatibility"
	TestValidity Perspective = "test validity"
)

var defaultLenses = []Perspective{Correctness, Security, Contracts, TestValidity, Correctness}

func reviewers(m bundle.Metadata, cfg *Config) ([]Perspective, error) {
	if cfg.Thresholds == [3]int{} {
		cfg.Thresholds = [3]int{100, 400, 1000}
	}
	t := cfg.Thresholds
	if t[0] < 1 || t[1] <= t[0] || t[2] <= t[1] {
		return nil, errors.New("pipeline: thresholds must be positive and ascending")
	}
	lenses := cfg.Perspectives
	if len(lenses) == 0 {
		lenses = defaultLenses
	}
	for _, p := range lenses {
		if !slices.Contains(defaultLenses, p) {
			return nil, errors.New("pipeline: unknown perspective")
		}
	}
	n := 2
	if m.Additions < 0 || m.Deletions < 0 {
		return nil, errors.New("pipeline: negative diff size")
	}
	size := uint64(m.Additions) + uint64(m.Deletions)
	for _, limit := range t {
		if size > uint64(limit) {
			n++
		}
	}
	docs := len(m.Files) > 0
	for _, f := range m.Files {
		if f.Binary || f.Mode == "160000" || !docPath(f.Path) || f.OldPath != "" && !docPath(f.OldPath) {
			docs = false
		}
	}
	if docs {
		n = 1
	}
	selected := make([]Perspective, n)
	for i := range selected {
		selected[i] = lenses[i%len(lenses)]
	}
	return selected, nil
}
func docPath(p string) bool {
	return slices.Contains([]string{".md", ".markdown", ".txt", ".rst", ".adoc"}, strings.ToLower(path.Ext(p)))
}
