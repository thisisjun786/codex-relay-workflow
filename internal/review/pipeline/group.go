// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/summarizer/summarizer.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package pipeline

import (
	"path"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

func (s *runState) group(raw []review.Finding) ([]review.Finding, bool, error) {
	if len(raw) < 2 {
		return raw, true, nil
	}
	var out struct {
		Groups [][]int `json:"groups"`
	}
	material := []string{}
	for _, c := range s.b.Chunks {
		material = append(material, c.Text)
	}
	ok, err := s.call(review.CallRecord{Stage: "group", Reviewer: -1, Chunk: -1}, map[string]any{"bundle": material, "findings": raw}, groupSchema, &out)
	if err != nil || !ok {
		return raw, false, err
	}
	seen := make([]bool, len(raw))
	grouped := []review.Finding{}
	for _, ids := range out.Groups {
		members := []review.Finding{}
		for _, id := range ids {
			if id < 0 || id >= len(raw) || seen[id] {
				return s.badPartition(raw)
			}
			seen[id] = true
			members = append(members, raw[id])
		}
		if len(members) == 0 {
			return s.badPartition(raw)
		}
		first := members[0]
		for _, f := range members {
			if path.Clean(f.File) != path.Clean(first.File) || f.NeedsContext != first.NeedsContext {
				return s.badPartition(raw)
			}
		}
		if len(members) > 1 {
			eligible, err := s.rules.Apply(members)
			if err != nil {
				return nil, false, err
			}
			if len(eligible.Dropped) > 0 {
				return s.badPartition(raw)
			}
		}
		first.Reviewers = nil
		for _, f := range members {
			first.Reviewers = append(first.Reviewers, f.Reviewers...)
			if f.Grade != "" && (first.Grade == "" || f.Grade < first.Grade) {
				first.Grade = f.Grade
			}
			first.Security = first.Security || f.Security
		}
		slices.Sort(first.Reviewers)
		first.Reviewers = slices.Compact(first.Reviewers)
		first.Support = len(first.Reviewers)
		grouped = append(grouped, first)
	}
	if slices.Contains(seen, false) {
		return s.badPartition(raw)
	}
	return grouped, true, nil
}
func (s *runState) badPartition(raw []review.Finding) ([]review.Finding, bool, error) {
	s.invalidLast("invalid_partition")
	return raw, false, nil
}
