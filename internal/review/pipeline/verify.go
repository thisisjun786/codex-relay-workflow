// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/fpfilter/filter.go and internal/fpfilter/prompt.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package pipeline

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

const contextLines = 5

func (s *runState) verify(fs []review.Finding) error {
	for i := range fs {
		f := &fs[i]
		if f.NeedsContext {
			f.Verdict = review.VerdictUnverified
			continue
		}
		file := path.Clean(f.File)
		end := max(f.Line, f.EndLine)
		if !filepath.IsLocal(file) || strings.Contains(file, `\`) || !s.rules.Changed[file] || f.Line < 1 || f.EndLine != 0 && f.EndLine < f.Line {
			continue
		}
		n, err := s.cfg.Head.Lines(file)
		if err != nil || end > n {
			continue
		} // final Rules owns location refusals and Lines errors
		chunk := -1
		for j, c := range s.b.Chunks {
			if slices.Contains(c.Paths, file) {
				chunk = j
				break
			}
		}
		if chunk < 0 {
			s.problems = append(s.problems, "verification context has no chunk for "+file)
			continue
		}
		start, stop := max(1, f.Line-contextLines), end+min(contextLines, n-end)
		code, err := s.cfg.Head.ReadLines(file, start, stop)
		if err != nil || len(code) != stop-start+1 {
			s.problems = append(s.problems, fmt.Sprintf("verification code unavailable at %s:%d: %v", file, f.Line, err))
			continue
		}
		var out struct {
			Verdict      review.Verdict `json:"verdict"`
			NeedsContext bool           `json:"needsContext"`
		}
		ok, err := s.call(review.CallRecord{Stage: "verify", Reviewer: -1, Chunk: chunk}, map[string]any{"chunk": s.b.Chunks[chunk].Text, "finding": f, "startLine": start, "headLines": code}, verifySchema, &out)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		f.NeedsContext = out.NeedsContext
		if out.NeedsContext {
			f.Verdict = review.VerdictUnverified
		} else {
			f.Verdict = out.Verdict
		}
	}
	return nil
}
