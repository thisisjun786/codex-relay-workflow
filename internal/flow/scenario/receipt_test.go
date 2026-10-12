package scenario

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/apply"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

const receiptEvent = "00112233445566778899aabbccddeeff"

type receiptEvents struct{ info apply.EventInfo }

func (r receiptEvents) Event(context.Context, string) (apply.EventInfo, error) { return r.info, nil }

// An oversized issue end to end on the fake host, the fake relay and a temporary store: the architect sizes node A for a child, the
// project run releases it as an issue child and goes on with node B, the child runs issue-lane (its executor call makes the commit, the
// run commits and verifies it, a reviewer of another family rules), and in the orchestrator's delivery turn `crw flow apply` waits for
// that verdict, finds the commit attributed to the executor call, and rules on the relay event from the orchestrator's own turn.
func TestFakeOversizedReceiptApplyScenario(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	got := mustRunIncomplete(t, l)
	if state, _ := laneOf(t, got, "A"); state != "child" {
		t.Fatalf("lane A: %s", state)
	}
	if len(l.Relay.Children) != 1 || l.Relay.Children[0].Node != "A" {
		t.Fatalf("children %+v", l.Relay.Children)
	}

	// the child's run
	child := l.NewChild("run-child-A", "child-task-A")
	res := child.Run("A")
	out := Value[struct {
		State, Head, Review string
		Rounds              int
	}](t, res)
	if out.State != "verified" || out.Head == "" || out.Rounds != 1 {
		t.Fatalf("the child's run came to %+v", out)
	}
	var execCalls, reviewCalls []string
	for _, ev := range child.Events() {
		if ev.Op != "agent" || ev.Event != "prepared" {
			continue
		}
		switch str(ev.Data, "role") {
		case "executor":
			execCalls = append(execCalls, ev.CallID)
		case "reviewer":
			reviewCalls = append(reviewCalls, ev.CallID)
		}
	}
	if len(execCalls) != 1 || len(reviewCalls) != 1 {
		t.Fatalf("the child's calls: executors %v reviewers %v", execCalls, reviewCalls)
	}

	// the orchestrator's delivery turn
	plan := l.IssuePlan("A")
	var received []apply.ReceiveInput
	var verdictFile string
	deps := apply.Deps{
		Store:   l.Store,
		Events:  receiptEvents{apply.EventInfo{ID: receiptEvent, Relationship: "rel-A", ChildTask: "child-task-A", ParentTask: "parent", Head: out.Head, CriteriaDigest: plan["criteria_digest"].(string), Plan: "plan-1", Node: "A"}},
		Commits: apply.Git,
		Receive: func(_ context.Context, in apply.ReceiveInput) (json.RawMessage, error) {
			received = append(received, in)
			raw, err := os.ReadFile(in.VerdictFile)
			if err != nil {
				t.Fatal(err)
			}
			verdictFile = string(raw)
			return json.RawMessage(`{"claim":{"proceed":true},"ack":{},"verdict":{}}`), nil
		},
		Repo: l.Repo,
		Now:  func() time.Time { return time.Date(2026, 10, 12, 9, 5, 0, 0, time.UTC) },
	}
	applied, err := apply.Apply(context.Background(), deps, apply.Request{Event: receiptEvent, Turn: "turn-orchestrator-7", Wait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != apply.StateApplied || applied.Run != "run-child-A" || applied.Verdict != "verified" || applied.ReviewerCall != reviewCalls[0] {
		t.Fatalf("apply came to %+v", applied)
	}
	if len(applied.Attributions) != 1 || applied.Attributions[0].Commit != out.Head || applied.Attributions[0].Call != execCalls[0] {
		t.Fatalf("attributions %+v: the commit %s was made by %s", applied.Attributions, out.Head, execCalls)
	}
	if len(received) != 1 || received[0].Turn != "turn-orchestrator-7" || received[0].Event != receiptEvent {
		t.Fatalf("receive %+v", received)
	}
	if !strings.Contains(verdictFile, `"verdict": "verified"`) || !strings.Contains(verdictFile, out.Head) {
		t.Errorf("the verdict file is %s", verdictFile)
	}
	rows, err := report.ReadRows(context.Background(), l.Store.DB, "run-child-A", apply.KindApplied)
	if err != nil || len(rows) != 1 || rows[0].Str("event") != receiptEvent {
		t.Fatalf("applied rows %+v %v", rows, err)
	}
	// the coordinator made no commit: no call of the child's thread appears among the authors, and the head is a commit of the lab's repository
	if l.Git("cat-file", "-t", out.Head) != "commit" {
		t.Errorf("head %s is no commit", out.Head)
	}

	// the same turn again replays; and a commit the child thread itself adds on top of the head is not the executor's
	direct := l.GitIn(l.Repo, "commit-tree", out.Head+"^{tree}", "-p", out.Head, "-m", "made by the child thread")
	deps.Events = receiptEvents{apply.EventInfo{ID: receiptEvent, Relationship: "rel-A", ChildTask: "child-task-A", ParentTask: "parent", Head: direct, CriteriaDigest: plan["criteria_digest"].(string)}}
	if again, err := apply.Apply(context.Background(), deps, apply.Request{Event: receiptEvent, Turn: "turn-orchestrator-8"}); err != nil || again.State != apply.StateNextTurn {
		t.Fatalf("a head with no verdict of the run: %+v %v", again, err)
	}
}
