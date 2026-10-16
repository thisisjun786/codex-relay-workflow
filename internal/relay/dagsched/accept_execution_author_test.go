package dagsched

import (
	"context"
	"testing"
)

// TestAgentReleaseRecordsTheAuthorPairTheAcceptanceJudges (CRW-1354): a release can record the pair that wrote the node's work when it is
// known as the release is decided - a head made outside a run, whose authoring subagent the run read back from the host - in the
// execution row's author_model and author_effort (AgentReleaseOptions). The agent acceptance keeps that pair in its author-pair digest
// (authorPairs, unchanged), while model-family overlap and missing author metadata do not block a verified review. A pair with white space
// around it is refused as malformed; the stored form is unchanged.
func TestAgentReleaseRecordsTheAuthorPairTheAcceptanceJudges(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec {
		return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}, unreleased: true}
	}
	k := newAgentKit(t, one("land"), one("same"), one("none"))
	criteria := releaseCriteriaDigest()
	ctx := context.Background()
	release := func(node, model, effort string) error {
		res, err := k.sched.ReleaseAgent(ctx, "g", node, "parent", k.agentRequest(true), AgentReleaseOptions{RunID: "run-1", Worktree: k.worktrees[node], AuthorModel: model, AuthorEffort: effort})
		if err == nil {
			k.releases[node] = res
		}
		return err
	}
	if err := release("land", " "+authorModel, "high"); err == nil {
		t.Fatal("an author pair with white space around it is released")
	}
	if err := release("land", authorModel, "high"); err != nil {
		t.Fatal(err)
	}
	var model, effort string
	if err := k.s.DB.QueryRow("SELECT author_model, author_effort FROM dag_agent_executions WHERE node_id = 'land'").Scan(&model, &effort); err != nil || model != authorModel || effort != "high" {
		t.Fatalf("the execution row records %q %q: %v", model, effort, err)
	}
	k.review("land", reviewerModel, "verified", criteria)
	res, err := k.accept("land")
	if err != nil || res.AcceptanceID == "" {
		t.Fatalf("a reviewer of another family than the recorded author: %+v %v", res, err)
	}
	row, found, err := loadAgentAcceptanceByID(ctx, k.s.Q(ctx), res.AcceptanceID)
	if err != nil || !found || row.AuthorPairsDigest != authorPairsDigest([]agentPair{{authorModel, "high"}}) {
		t.Fatalf("the acceptance row: %+v %v %v", row, found, err)
	}

	if err := release("same", "gpt-fake-worker", "high"); err != nil {
		t.Fatal(err)
	}
	k.review("same", reviewerModel, "verified", criteria)
	if got, err := k.accept("same"); err != nil || got.AcceptanceID == "" {
		t.Fatalf("same-family reviewer acceptance: %+v %v", got, err)
	}

	if err := release("none", "", ""); err != nil {
		t.Fatal(err)
	}
	k.review("none", reviewerModel, "verified", criteria)
	if got, err := k.accept("none"); err != nil || got.AcceptanceID == "" {
		t.Fatalf("missing author model metadata must not block acceptance: %+v %v", got, err)
	}
}
