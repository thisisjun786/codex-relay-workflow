package dagsched

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// premergeWithRecord is the CRW-952 test fixture: an acceptance of an implementation node that names no pre-merge
// record gets a passing one, built for the node (its issue key and the criteria the plan holds now) and for the head
// the input accepts (the commit, or the pull request head the forge reports). A record the call names is kept. Tests
// route their Accept calls through it with gofmt -r; the gate's own tests name their records explicitly.
func premergeWithRecord(s *Scheduler, ctx context.Context, plan, node, actor string, in AcceptInput) AcceptInput {
	if in.Premerge != nil {
		return in
	}
	snap, _, err := dag.SnapshotAt(ctx, s.Store.Q(ctx), plan, 0)
	if err != nil {
		return in
	}
	n, ok := nodeOf(snap, node)
	if !ok || n.Kind != dag.NodeImplementation {
		return in
	}
	// a pure replay of an acceptance whose stored record is current names no record (answer 2)
	var stored string
	if current, err := queryOne(ctx, s.Store.Q(ctx), "SELECT p.record_json FROM dag_acceptances a JOIN dag_acceptance_premerge p ON p.acceptance_id = a.acceptance_id WHERE a.plan_id = ? AND a.node_id = ? AND a.state = 'active'", []any{plan, node}, &stored); err == nil && current {
		if rec, err := premerge.Decode([]byte(stored)); err == nil && rec.CriteriaDigest == n.CriteriaSetDigest {
			return in
		}
	}
	var head string
	switch {
	case in.Commit != nil:
		head = in.Commit.Head
	case in.PullRequest != nil && s.PRs != nil:
		pr, err := s.PRs(ctx, in.PullRequest.Repository, in.PullRequest.Number)
		if err != nil {
			return in
		}
		head = pr.HeadSHA
	default:
		return in
	}
	score := 9.0
	raw, err := json.Marshal(premerge.Record{Schema: premerge.RecordSchema, Issue: n.IssueKey, Node: node, Head: head,
		Dev: strings.Repeat("a", 40), CriteriaDigest: n.CriteriaSetDigest,
		Grader: premerge.Grader{Model: "fixture", Effort: "none", PromptDigest: "sha256:fixture"}, GradedAt: "2026-10-08T12:00:00Z",
		Criteria: map[string]premerge.Criterion{"c1": {Verdict: "PASS", Evidence: "fixture"}}, Defects: []premerge.Defect{},
		Score: &score, Summary: "fixture", Dispositions: premerge.Dispositions{By: "fixture"}})
	if err != nil {
		return in
	}
	in.Premerge = raw
	return in
}
