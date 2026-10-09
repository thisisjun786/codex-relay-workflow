package dagsched_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/childcleanup"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// oneIdleThread is an App Server that holds the child's thread loaded and idle, with no sub-thread, and archives what it is asked to.
type oneIdleThread struct {
	id       string
	archived []string
}

func (h *oneIdleThread) Call(_ context.Context, method string, params map[string]any) (json.RawMessage, error) {
	var out any
	switch method {
	case "thread/loaded/list":
		loaded := []string{}
		if len(h.archived) == 0 {
			loaded = append(loaded, h.id)
		}
		out = map[string]any{"data": loaded, "nextCursor": nil}
	case "thread/read":
		if params["threadId"] != h.id {
			return nil, fmt.Errorf("thread not found: %v", params["threadId"])
		}
		out = map[string]any{"thread": map[string]any{"id": h.id, "parentThreadId": nil, "status": map[string]any{"type": "idle"}}}
	case "thread/archive":
		h.archived = append(h.archived, fmt.Sprint(params["threadId"]))
		out = map[string]any{}
	default:
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	return json.Marshal(out)
}

// CRW-1036: a non_pr node has no head to observe, so the parent's merged mark on the revision its acceptance stands on lets the cleanup of its child go ahead
// (assignment-mark merged, then child-cleanup), where before it was held for an integration observation no command can make.
func TestTheChildOfANonPRNodeIsCleanedUpAfterItsMergedMark(t *testing.T) {
	ctx := context.Background()
	st, rid, parent, event, _ := dagsched.SettledNonPRNodeForTest(t)
	if _, err := childcleanup.Execute(ctx, st, &oneIdleThread{id: "unused"}, rid, parent, true); err == nil {
		t.Fatal("the child of an accepted node with no merged mark was cleaned up")
	}
	reg := &registry.Registry{Store: st, Policy: registry.ResolveRolePolicy(map[string]string{})}
	if _, err := registry.NewAssignmentView(reg).Mark(ctx, rid, "merged", "the decision document is adopted", "test", event); err != nil {
		t.Fatalf("assignment-mark merged: %v", err)
	}
	facts, err := childcleanup.ReadFacts(ctx, st, rid)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Mark == nil || !facts.PlanNode || !facts.Integrated {
		t.Fatalf("facts after the merged mark = %+v; want the plan node integrated on its mark", facts)
	}
	host := &oneIdleThread{id: facts.ChildTaskID}
	report, err := childcleanup.Execute(ctx, st, host, rid, parent, false)
	if err != nil || !report.Complete() || fmt.Sprint(host.archived) != "["+facts.ChildTaskID+"]" {
		t.Fatalf("child-cleanup = %v %+v archived %v; want the child archived", err, report, host.archived)
	}
}
