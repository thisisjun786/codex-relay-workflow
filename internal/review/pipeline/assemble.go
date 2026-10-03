// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/runner/runner.go and internal/runner/report.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

func (s *runState) assemble() {
	r := &s.a.Reviewers
	normal, timed, auth := make([]bool, r.Run), make([]bool, r.Run), make([]bool, r.Run)
	for _, c := range s.a.Calls {
		if c.Class != "normal" || c.Error != "" {
			s.problems = append(s.problems, fmt.Sprintf("%s reviewer %d chunk %d: %s %s %s", c.Stage, c.Reviewer, c.Chunk, c.Class, c.Reason, c.Error))
		}
		if c.Stage != "review" {
			continue
		}
		normal[c.Reviewer] = normal[c.Reviewer] || c.Class == "normal"
		timed[c.Reviewer] = timed[c.Reviewer] || c.Reason == string(agy.ReasonTimeLimit) || c.Reason == string(agy.ReasonPartialResponse)
		auth[c.Reviewer] = auth[c.Reviewer] || c.Reason == string(agy.ReasonAuthentication)
	}
	for id, ok := range normal {
		if ok {
			continue
		}
		r.Failed++
		if timed[id] {
			r.TimedOut++
		} else if auth[id] {
			r.AuthFailed++
		}
	}
	s.a.Status, s.a.Reason = review.StatusComplete, ""
	if r.Failed == r.Run {
		s.a.Status = review.StatusUnavailable
		s.a.Findings, s.a.Dropped = []review.Finding{}, []review.Drop{}
	} else if len(s.problems) > 0 {
		s.a.Status = review.StatusPartial
	}
	if s.a.Status != review.StatusComplete {
		s.a.Reason = strings.Join(s.problems, "; ")
	}
	s.a.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
}
