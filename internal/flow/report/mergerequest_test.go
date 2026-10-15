package report

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1457 (D-12): the merge request of a pull request goes up as a report of its own kind (merge_request), through the supervisor
// channel as flow_merge_request, once for each head.

const mergeHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedCrosscheck appends the rows of the shared crosscheck fixture (flow/pullrequest/testdata) and the milestone rows to the world's journal.
func (w *world) seedCrosscheck() {
	w.t.Helper()
	raw, err := os.ReadFile("../pullrequest/testdata/crosscheck_rows.json")
	if err != nil {
		w.t.Fatalf("the shared crosscheck fixture: %v", err)
	}
	var fixture []struct {
		Op    string         `json:"op"`
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		w.t.Fatal(err)
	}
	for i, f := range fixture {
		w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindCall, CallID: "cc_" + string(rune('a'+i)), Op: f.Op, Event: f.Event, Data: f.Data})
	}
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindCall, CallID: "ms_M1", Op: ops.OpMilestone, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": "M1", "plan": testPlan, "result": "{}"}})
	if err := w.journal.Flush(); err != nil {
		w.t.Fatal(err)
	}
}

// mergeRequest runs crw.mergeRequest as the engine would, and writes its completed row (the engine writes it, not Run).
func (w *world) mergeRequest(lane *ops.Lane, call int, pr string) map[string]any {
	w.t.Helper()
	raw, err := json.Marshal([]any{"M1", map[string]any{"pr": pr, "review": map[string]any{"bots": "connected", "unresolved": 0}}})
	if err != nil {
		w.t.Fatal(err)
	}
	callID := "mr_" + string(rune('a'+call))
	res, err := lane.Ops()[ops.NameMergeRequest].Run(ctx, engine.OpRequest{RunID: testRun, CallID: callID, RequestID: engine.RequestID(testRun, callID), Args: raw, Journal: w.journal})
	if err != nil {
		w.t.Fatalf("crw.mergeRequest: %v", err)
	}
	data := map[string]any{"result": string(res.Result)}
	for k, v := range res.Data {
		data[k] = v
	}
	b, _ := json.Marshal(data)
	var round map[string]any
	_ = json.Unmarshal(b, &round)
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindCall, CallID: callID, Op: ops.OpMergeRequest, Event: engine.EvCompleted, Data: round})
	if err := w.journal.Flush(); err != nil {
		w.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		w.t.Fatalf("the answer is not an object: %s", res.Result)
	}
	return out
}

// TestMergeRequestReachesTheParentReport: crw.mergeRequest, run over a Reporter on the supervisor channel, stages one flow_merge_request
// message for the management seat above the project, with one merge_request report row and one queued delivery row, and a second request
// at the same head sends and writes nothing more.
func TestMergeRequestReachesTheParentReport(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.manage(testManagement)
	w.seedCrosscheck()

	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(string) string { return mergeHeadA }
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "milestone-m1", Base: "dev", Title: "M1", Issues: []tracking.Ref{{Repo: "o/r", Number: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	fake.SetChecks(mergeHeadA, []trackingfake.FakeCheckRun{{Name: "build", Status: "completed", Conclusion: "success"}}, nil)

	lane := ops.NewLane(ops.Deps{RunID: testRun, Journal: w.journal, Actor: testOrch, Now: fixedNow,
		Reporter: Reporter{W: w.relayWriter(), Run: testRun}, PullRequests: gh})

	out := w.mergeRequest(lane, 0, pr.Ref.String())
	if out["state"] != "requested" || out["head"] != mergeHeadA {
		t.Fatalf("answer %v, want requested at %s", out, mergeHeadA)
	}
	msgs := w.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.ObligationKind != supervisor.KindFlowMergeRequest || m.RecipientTaskID != testManagement || m.RelationshipID != "project:"+testProject || m.State != "queued" ||
		!strings.Contains(m.Packet, pr.Ref.String()) || !strings.Contains(m.Packet, mergeHeadA) {
		t.Fatalf("message %+v", m)
	}
	reports := w.rows(KindReport)
	if len(reports) != 1 || reports[0].Str("report_kind") != KindMergeRequest {
		t.Fatalf("report rows %+v, want one %s", reports, KindMergeRequest)
	}
	deliveries := w.rows(KindDelivery)
	if len(deliveries) != 1 || deliveries[0].Str("result") != ResultQueued {
		t.Fatalf("delivery rows %+v, want one queued", deliveries)
	}
	if n := len(w.sender.supervisorSent()) + len(w.sender.parentSent()); n != 0 {
		t.Fatalf("%d notices went through the fallback sender, want none", n)
	}

	again := w.mergeRequest(lane, 1, pr.Ref.String())
	if again["repeated"] != true {
		t.Fatalf("second answer %v, want repeated", again)
	}
	if n := len(w.messages()); n != 1 {
		t.Fatalf("%d messages after the second request, want 1", n)
	}
	if n := len(w.rows(KindReport)); n != 1 {
		t.Fatalf("%d report rows after the second request, want 1", n)
	}
	if n := len(w.rows(KindDelivery)); n != 1 {
		t.Fatalf("%d delivery rows after the second request, want 1", n)
	}
}

// TestMergeRequestNotStagedIsAnErrorAndIsRetried: a merge request the channel refused to stage (no delivery row) is an error, so the
// operation does not record it as requested; the retry finds the report row and stages the same request.
func TestMergeRequestNotStagedIsAnErrorAndIsRetried(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.manage(testManagement)
	refuse := true
	w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) {
		if refuse {
			return SendResult{Reason: "relation_owner_drift"}, nil
		}
		return SendResult{Class: "accepted", RequestID: "req-1"}, nil
	}
	rep := Reporter{W: w.writer(), Run: testRun}
	req := ops.MergeRequest{Plan: testPlan, Milestone: "M1", PR: "o/r#7", Head: mergeHeadA, Base: "dev", Checks: "success",
		Crosscheck: ops.MergeCrosscheck{Verdict: "verified", Head: mergeHeadA, EngineFamily: "gpt", Model: "m", Round: 1}, RequestID: "rep_0123456789abcdef01234567"}
	if id, err := rep.MergeRequest(ctx, req); err == nil || id != "" || !strings.Contains(err.Error(), "relation_owner_drift") {
		t.Fatalf("a refused staging answered %q, %v; want an error naming the reason", id, err)
	}
	if n := len(w.rows(KindReport)); n != 1 {
		t.Fatalf("%d report rows, want the one on record", n)
	}
	refuse = false
	id, err := rep.MergeRequest(ctx, req)
	if err != nil || id != req.RequestID {
		t.Fatalf("the retry answered %q, %v", id, err)
	}
	if n, m := len(w.rows(KindReport)), len(w.rows(KindDelivery)); n != 1 || m != 1 {
		t.Fatalf("%d report rows and %d delivery rows after the retry, want 1 and 1", n, m)
	}
	if n := len(w.sender.supervisorSent()); n != 2 {
		t.Fatalf("%d sends, want the refused one and the retry", n)
	}
}
