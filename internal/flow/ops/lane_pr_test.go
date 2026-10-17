package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

const (
	prHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	prRef   = "o/r#7"
)

// stubPullRequests answers the pull request and the checks a test sets, and counts the reads.
type stubPullRequests struct {
	pr        tracking.PullRequest
	checks    tracking.Checks
	err       error
	reads     int
	checkRead int
}

func (s *stubPullRequests) PullRequest(context.Context, tracking.Ref) (tracking.PullRequest, error) {
	s.reads++
	return s.pr, s.err
}

func (s *stubPullRequests) Checks(context.Context, tracking.Ref) (tracking.Checks, error) {
	s.checkRead++
	return s.checks, s.err
}

// stubMergeReporter is a Reporter that is also a MergeRequester, and remembers what it was asked to send.
type stubMergeReporter struct {
	sent []MergeRequest
	err  error
}

func (s *stubMergeReporter) ReportUp(context.Context, Report) (string, error) { return "", nil }

func (s *stubMergeReporter) MergeRequest(_ context.Context, r MergeRequest) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.sent = append(s.sent, r)
	return "rep_sent_" + r.Head[:4], nil
}

// prWorld is a world whose journal holds the crosscheck rows of the shared fixture and whose lane reads pull requests from a stub.
type prWorld struct {
	*world
	prs *stubPullRequests
	rep *stubMergeReporter
}

func newPRWorld(t *testing.T) *prWorld {
	t.Helper()
	w := newWorld(t)
	raw, err := os.ReadFile("../pullrequest/testdata/crosscheck_rows.json")
	if err != nil {
		t.Fatalf("the shared crosscheck fixture: %v", err)
	}
	var fixture []struct {
		Op    string         `json:"op"`
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for i, f := range fixture {
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "cc_" + string(rune('a'+i)), Op: f.Op, Event: f.Event, Data: f.Data})
	}
	p := &prWorld{world: w, prs: &stubPullRequests{}, rep: &stubMergeReporter{}}
	w.lane.d.PullRequests = p.prs
	w.lane.d.Reporter = p.rep
	// the milestones the fixture names are in the plan of the run
	for _, ms := range []string{"M1", "M2", "M3", "M4", "M5", "M6"} {
		row, _ := json.Marshal(Milestone{ID: ms, Plan: "plan-1"})
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "ms_" + ms, Op: OpMilestone, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": ms, "plan": "plan-1", "result": string(row)}})
	}
	return p
}

// close seeds the journal's closing acceptance of ms at head: a passed finalGate row, the milestoneClose that recorded it
// (gate, tip and gate_head equal, a verified closing review) and its reportUp — what crw.mergeRequest and crw.prOpen require
// instead of a standalone crw.crosscheck row. The relay's integration tip follows.
func (p *prWorld) close(ms, head string) {
	p.t.Helper()
	gate, _ := json.Marshal(GateOutcome{Milestone: ms, Plan: "plan-1", Head: head, State: GatePass})
	cc, _ := json.Marshal(map[string]any{"verdict": "verified", "head": head, "milestone": ms, "engine_family": "gpt", "model": "gpt-6-astra", "round": 1})
	closed, _ := json.Marshal(CloseResultOutcome{Milestone: ms, Base: "dev", Tip: head, Gate: gate, Crosscheck: cc, GateHead: head})
	p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "fg_" + ms + head[:4], Op: "crw." + NameFinalGate, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": ms, "plan": "plan-1", "head": head, "state": GatePass, "result": string(gate)}})
	p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "mc_" + ms + head[:4], Op: OpMilestoneClose, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": ms, "plan": "plan-1", "tip": head, "gate_head": head, "moved": false, "result": string(closed)}})
	p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "ru_" + ms + head[:4], Op: OpReportUp, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": ms, "tip": head, "report_id": "rep_" + ms, "delivered": true, "result": "{}"}})
	p.relay.tip = head
}

// open sets the pull request to be open at head with green checks at head.
func (p *prWorld) open(head string) {
	p.prs.pr = tracking.PullRequest{Ref: tracking.Ref{Repo: "o/r", Number: 7}, State: tracking.StateOpen, HeadSHA: head, BaseRef: "dev"}
	p.prs.checks = tracking.Checks{PullRequest: p.prs.pr.Ref, Head: head, State: tracking.CheckSuccess}
}

// review writes the crw-pr-review/1 record the op reads ({record: path}) for the pull request at head: bots connected with
// unresolved open items (state pending) or none (complete), or a not_connected record when bots is not_connected.
func (p *prWorld) review(head, bots string, unresolved int) map[string]any {
	p.t.Helper()
	rec := tracking.PRReviewRecord{Schema: tracking.PRReviewSchema, PR: prRef, Head: head, Bots: bots, Round: 1, Items: []tracking.PRReviewItem{}}
	switch {
	case bots == "not_connected":
		rec.State = "not_connected"
		rec.Note = "리뷰 봇 미연결"
	case unresolved > 0:
		rec.State = "pending"
		for i := 0; i < unresolved; i++ {
			rec.Items = append(rec.Items, tracking.PRReviewItem{Key: "k" + strconv.Itoa(i),
				Comments: []tracking.PRReviewComment{{ID: "issue:" + strconv.Itoa(i+1), Bot: "devin", Body: "remark"}}})
		}
	default:
		rec.State = "complete"
	}
	data, err := json.Marshal(rec)
	if err != nil {
		p.t.Fatal(err)
	}
	path := filepath.Join(p.t.TempDir(), "review.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return map[string]any{"record": path}
}

// request runs crw.mergeRequest and decodes the answer.
func (p *prWorld) request(ms string, arg map[string]any) (map[string]any, error) {
	p.t.Helper()
	raw, err := p.world.op(NameMergeRequest, ms, arg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		p.t.Fatalf("the answer is not an object: %s", raw)
	}
	return out, nil
}

func (p *prWorld) mustRequest(ms string, arg map[string]any) map[string]any {
	p.t.Helper()
	out, err := p.request(ms, arg)
	if err != nil {
		p.t.Fatalf("crw.mergeRequest: %v", err)
	}
	return out
}

func wantRefused(t *testing.T, out map[string]any, reason string) {
	t.Helper()
	if out["state"] != "refused" || out["reason"] != reason {
		t.Fatalf("want refused %q, got %v", reason, out)
	}
}

func TestMergeRequestRefusesRedChecks(t *testing.T) {
	for _, tc := range []struct {
		state  tracking.CheckState
		reason string
	}{
		{tracking.CheckFailure, "checks_failed"},
		{tracking.CheckPending, "checks_pending"},
		{tracking.CheckNone, "checks_none"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			p := newPRWorld(t)
			p.open(prHeadA)
			p.close("M1", prHeadA)
			p.prs.checks.State = tc.state
			out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)})
			wantRefused(t, out, tc.reason)
			if out["pr"] != prRef || out["head"] != prHeadA {
				t.Errorf("the refusal names the pull request and the head: %v", out)
			}
			if len(p.rep.sent) != 0 {
				t.Errorf("a refused request sent %d report(s)", len(p.rep.sent))
			}
		})
	}
	t.Run("green", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.close("M1", prHeadA)
		out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)})
		if out["state"] != "requested" || len(p.rep.sent) != 1 {
			t.Fatalf("green checks at the crosschecked head are requested: %v, sent %d", out, len(p.rep.sent))
		}
		got := p.rep.sent[0]
		if got.Milestone != "M1" || got.Plan != "plan-1" || got.PR != prRef || got.Head != prHeadA || got.Base != "dev" || got.Checks != "success" ||
			got.Crosscheck.Verdict != "verified" || got.Crosscheck.Head != prHeadA || got.Crosscheck.EngineFamily != "gpt" ||
			got.Crosscheck.Model != "gpt-6-astra" || got.Crosscheck.Round != 1 || got.Unresolved != 0 || got.BotsNotConnected ||
			!strings.HasPrefix(got.RequestID, "rep_") {
			t.Errorf("the request: %+v", got)
		}
	})
	t.Run("not open", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.prs.pr.State = tracking.StateClosed
		wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}), "pr_not_open")
		p.prs.pr.State, p.prs.pr.Merged = tracking.StateOpen, true
		wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}), "pr_not_open")
		if len(p.rep.sent) != 0 {
			t.Error("a pull request that is not open was requested")
		}
	})
}

func TestMergeRequestRefusesAHeadThatMovedFromTheCrosscheckedOne(t *testing.T) {
	t.Run("pull request head is not the crosschecked one", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadB) // M1 closed at A, the pull request is at B, green
		p.close("M1", prHeadA)
		arg := map[string]any{"pr": prRef, "review": p.review(prHeadB, "connected", 0)}
		wantRefused(t, p.mustRequest("M1", arg), "head_moved")
		if len(p.rep.sent) != 0 {
			t.Error("sent")
		}
	})
	t.Run("checks read a head the pull request no longer has", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.close("M1", prHeadA)
		p.prs.checks.Head = prHeadB
		arg := map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
		wantRefused(t, p.mustRequest("M1", arg), "head_moved")
		if len(p.rep.sent) != 0 {
			t.Error("sent")
		}
	})
	t.Run("a verified row of the head the verdict names elsewhere", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.close("M4", prHeadA)
		arg := map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
		if out := p.mustRequest("M4", arg); out["state"] != "requested" {
			t.Fatalf("a closed milestone is requested: %v", out)
		}
	})
	t.Run("needs_changes and evidence are not accepted", func(t *testing.T) {
		p := newPRWorld(t)
		p.open("dddddddddddddddddddddddddddddddddddddddd")
		arg := map[string]any{"pr": prRef, "review": p.review("dddddddddddddddddddddddddddddddddddddddd", "connected", 0)}
		wantRefused(t, p.mustRequest("M2", arg), "milestone_not_closed")
		p.open(prHeadA)
		arg = map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
		wantRefused(t, p.mustRequest("M5", arg), "milestone_not_closed") // no close rows at all
	})
	t.Run("a milestone with no row", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		arg := map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
		wantRefused(t, p.mustRequest("M9", arg), "milestone_not_closed")
	})
	t.Run("the verified round after an evidence round is accepted", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.close("M6", prHeadA)
		arg := map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
		if out := p.mustRequest("M6", arg); out["state"] != "requested" {
			t.Fatalf("M6: %v", out)
		}
	})
}

func TestMergeRequestRefusesUnresolvedComments(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	p.close("M1", prHeadA)
	wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 2)}), "comments_unresolved")
	if len(p.rep.sent) != 0 {
		t.Fatal("unresolved comments were requested")
	}
	// the bots are not connected: there is nothing to resolve, the request goes out and says so
	out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "not_connected", 0)})
	if out["state"] != "requested" || out["bots_not_connected"] != true || len(p.rep.sent) != 1 || !p.rep.sent[0].BotsNotConnected {
		t.Fatalf("not_connected: %v, sent %+v", out, p.rep.sent)
	}
	row, ok := rows(p.j.Events()).opRows(OpMergeRequest).last(func(engine.Event) bool { return true })
	if !ok || row.Data["bots_not_connected"] != true {
		t.Errorf("the row says it too: %+v", row.Data)
	}
	// no review argument: refuse by default, with a Go error
	p2 := newPRWorld(t)
	p2.open(prHeadA)
	p2.close("M1", prHeadA)
	if _, err := p2.request("M1", map[string]any{"pr": prRef}); err == nil {
		t.Error("a request with no review argument was not an error")
	}
	if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": map[string]any{"bots": "sometimes"}}); err == nil {
		t.Error("a review argument without a record was not an error")
	}
	// a record the pull request has moved past is an error, and a missing or malformed one too
	if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": map[string]any{"record": filepath.Join(t.TempDir(), "none.json")}}); err == nil {
		t.Error("a missing record was not an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": map[string]any{"record": bad}}); err == nil {
		t.Error("a malformed record was not an error")
	}
	// a record written for another head is stale, never trusted
	if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": p2.review(prHeadB, "connected", 0)}); err == nil {
		t.Error("a record of another head was not an error")
	}
	// legacy scalar fields never approve: the record says one item is unresolved, the scalars claim zero and bots off — the
	// record wins and the request is refused
	out2 := p2.mustRequest("M1", map[string]any{"pr": prRef, "review": map[string]any{"record": p2.review(prHeadA, "connected", 1)["record"], "bots": "not_connected", "unresolved": 0}})
	if out2["state"] != "refused" || out2["reason"] != "comments_unresolved" {
		t.Errorf("stale scalars approved a record that holds unresolved comments: %v", out2)
	}
	// connected bots with no count (absent or null) is not a zero
	for _, rv := range []map[string]any{{"bots": "connected"}, {"bots": "connected", "unresolved": nil}} {
		if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": rv}); err == nil {
			t.Errorf("review %v without a count was not an error", rv)
		}
	}
	if len(p2.rep.sent) != 0 {
		t.Error("sent")
	}
}

func TestMergeRequestIsIdempotentPerHead(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	p.close("M1", prHeadA)
	arg := map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}
	// a refused row does not block a later request
	p.prs.checks.State = tracking.CheckPending
	wantRefused(t, p.mustRequest("M1", arg), "checks_pending")
	p.prs.checks.State = tracking.CheckSuccess
	first := p.mustRequest("M1", arg)
	if first["state"] != "requested" || first["repeated"] == true || len(p.rep.sent) != 1 {
		t.Fatalf("first: %v, sent %d", first, len(p.rep.sent))
	}
	second := p.mustRequest("M1", arg)
	if second["state"] != "requested" || second["repeated"] != true || second["report_id"] != first["report_id"] || len(p.rep.sent) != 1 {
		t.Fatalf("second: %v, sent %d", second, len(p.rep.sent))
	}
	// the same pull request named in another case is the same pull request
	upper := p.mustRequest("M1", map[string]any{"pr": "O/R#7", "review": p.review(prHeadA, "connected", 0)})
	if upper["state"] != "requested" || upper["repeated"] != true || len(p.rep.sent) != 1 {
		t.Fatalf("another case: %v, sent %d", upper, len(p.rep.sent))
	}
	// the head moved to B, and B has a verified row of M3: a second send (the record is of the new head)
	p.open(prHeadB)
	p.close("M3", prHeadB)
	arg = map[string]any{"pr": prRef, "review": p.review(prHeadB, "connected", 0)}
	wantRefused(t, p.mustRequest("M1", arg), "head_moved")
	third := p.mustRequest("M3", arg)
	if third["state"] != "requested" || third["repeated"] == true || len(p.rep.sent) != 2 {
		t.Fatalf("third: %v, sent %d", third, len(p.rep.sent))
	}
	if p.rep.sent[0].RequestID == p.rep.sent[1].RequestID {
		t.Error("two heads share one request id")
	}
	// a nil reader or a reporter that cannot take a merge request is a Go error
	p.world.lane.d.PullRequests = nil
	if _, err := p.request("M3", arg); err == nil {
		t.Error("no pull request reader was not an error")
	}
	p.world.lane.d.PullRequests = p.prs
	p.world.lane.d.Reporter = nil
	if _, err := p.request("M3", map[string]any{"pr": "o/r#8", "review": p.review(prHeadB, "connected", 0)}); err == nil {
		t.Error("no reporter was not an error")
	}
	// a send that fails is the op's error and writes no requested row
}

func TestMergeRequestFailedSendIsAnErrorAndRecordsNothing(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	p.close("M1", prHeadA)
	p.world.lane.d.Reporter = &stubMergeReporter{err: errors.New("down")}
	if _, err := p.request("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}); err == nil {
		t.Fatal("a failed send was not an error")
	}
	if got := rows(p.j.Events()).opRows(OpMergeRequest); len(got) != 0 {
		t.Errorf("a failed send left %d row(s)", len(got))
	}
}

// TestMergeRequestReadsTheRowTheCrosscheckOpWrites (CRW-1451, D-0 gate): the row the landed crw.crosscheck op writes is the row
// mergeCrosscheckOf reads, and the shared fixture the readers of the bundle's pull request are tested with has no member the op's row
// lacks. A verified round at the gate head is accepted with its engine and round; the same milestone at another head is head_moved.
func TestMergeRequestReadsTheRowTheCrosscheckOpWrites(t *testing.T) {
	w := newCCWorld(t)
	_, row := w.crosscheck(ccHead, nil)
	if row.Op != OpCrosscheck {
		t.Fatalf("the row's op is %q, not %q", row.Op, OpCrosscheck)
	}
	got, reason := mergeCrosscheckOf(rows(w.j.Events()), "M1", ccHead)
	if reason != "" || got.Verdict != "verified" || got.Head != ccHead || got.EngineFamily != "gpt" || got.Model != "gpt-6-astra" || got.Round != 1 {
		t.Errorf("mergeCrosscheckOf %+v, reason %q", got, reason)
	}
	if _, reason := mergeCrosscheckOf(rows(w.j.Events()), "M1", prHeadB); reason != mergeReasonHeadMoved {
		t.Errorf("another head: reason %q, want %q", reason, mergeReasonHeadMoved)
	}

	raw, err := os.ReadFile("../pullrequest/testdata/crosscheck_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	real, ok := row.Data["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("the row's verdict is %T, not an object", row.Data["verdict"])
	}
	for i, f := range fixture {
		for k := range f.Data {
			if _, ok := row.Data[k]; !ok && k != "hold" { // hold is a member of a held row only
				t.Errorf("fixture row %d has the member %q, which the crw.crosscheck row does not carry", i, k)
			}
		}
		fv, _ := f.Data["verdict"].(map[string]any)
		for k := range fv {
			if _, ok := real[k]; !ok {
				t.Errorf("fixture row %d has the verdict member %q, which V does not carry", i, k)
			}
		}
	}
}

// ---- crw.prOpen (CRW-1423, step 3) ----

// bundle seams a pull request target onto the milestone's plan and counts the parent's opener calls.
func (p *prWorld) bundle(target BundlePRTarget, open func(BundlePROpenRequest) (BundlePRResult, error)) *int {
	calls := 0
	p.world.lane.d.PRTarget = func(context.Context, string) (BundlePRTarget, error) { return target, nil }
	p.world.lane.d.PROpen = func(_ context.Context, req BundlePROpenRequest) (BundlePRResult, error) {
		calls++
		if open != nil {
			return open(req)
		}
		return BundlePRResult{State: "opened", PR: "o/r#9", Head: req.Close.Tip, Base: req.Target.BaseRef, ReviewRecord: "/tmp/review.json"}, nil
	}
	return &calls
}

// closeWith variants of close let a case corrupt one part of the closing acceptance.
func (p *prWorld) closeWith(ms, head string, edit func(gate, cc, closed *map[string]any), editRows func(ev []engine.Event) []engine.Event) {
	p.t.Helper()
	var gate, cc, closed map[string]any
	_ = json.Unmarshal([]byte(`{"milestone":"`+ms+`","plan":"plan-1","head":"`+head+`","state":"pass","commands":[]}`), &gate)
	_ = json.Unmarshal([]byte(`{"verdict":"verified","head":"`+head+`","milestone":"`+ms+`"}`), &cc)
	_ = json.Unmarshal([]byte(`{"milestone":"`+ms+`","base":"dev","tip":"`+head+`","gate_head":"`+head+`","moved":false}`), &closed)
	gate["state"] = GatePass
	if edit != nil {
		edit(&gate, &cc, &closed)
	}
	gb, _ := json.Marshal(gate)
	cb, _ := json.Marshal(cc)
	closed["gate"] = gate
	closed["crosscheck"] = cc
	xb, _ := json.Marshal(closed)
	_ = cb
	evs := []engine.Event{
		{RunID: "run-1", Kind: engine.KindCall, CallID: "fg_" + ms, Op: "crw." + NameFinalGate, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": ms, "plan": "plan-1", "head": head, "state": GatePass, "result": string(gb)}},
		{RunID: "run-1", Kind: engine.KindCall, CallID: "mc_" + ms, Op: OpMilestoneClose, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": ms, "plan": "plan-1", "tip": head, "gate_head": closed["gate_head"], "moved": closed["moved"], "result": string(xb)}},
		{RunID: "run-1", Kind: engine.KindCall, CallID: "ru_" + ms, Op: OpReportUp, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": ms, "tip": head, "report_id": "rep_" + ms, "delivered": true, "result": "{}"}},
	}
	if editRows != nil {
		evs = editRows(evs)
	}
	for _, ev := range evs {
		p.j.Append(ev)
	}
	p.relay.tip = head
}

func (p *prWorld) prOpen(ms string) (map[string]any, error) {
	p.t.Helper()
	raw, err := p.world.op(NamePROpen, ms)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		p.t.Fatalf("the answer is not an object: %s", raw)
	}
	return out, nil
}

// TestPROpenRequiresLatestClosedAcceptance: crw.prOpen opens only on the journal-proven closing acceptance — the latest
// unsuperseded close of the milestone at the live tip, its gate passed at that tip after the close, the closing review
// accepted and the close reported after it. Every weakening of that proof must refuse, never open.
func TestPROpenRequiresLatestClosedAcceptance(t *testing.T) {
	head := prHeadA
	for name, seed := range map[string]func(p *prWorld){
		"accepted": func(p *prWorld) { p.closeWith("M1", head, nil, nil) },
		"latest final gate pending": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { (*g)["state"] = "pending" },
				func(evs []engine.Event) []engine.Event { evs[0].Data["state"] = "pending"; return evs })
		},
		"latest final gate failed": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { (*g)["state"] = "fail" },
				func(evs []engine.Event) []engine.Event { evs[0].Data["state"] = "fail"; return evs })
		},
		"missing close": func(p *prWorld) {
			p.relay.tip = head
			p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "ru_x", Op: OpReportUp, Event: engine.EvCompleted,
				Data: map[string]any{"milestone": "M1", "tip": head, "report_id": "r", "delivered": true, "result": "{}"}})
		},
		"moved target": func(p *prWorld) { p.closeWith("M1", head, nil, nil); p.relay.tip = prHeadB },
		"stale head": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { (*cl)["tip"] = prHeadB },
				func(evs []engine.Event) []engine.Event { evs[1].Data["tip"] = prHeadB; return evs })
		},
		"missing verdict head": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { delete(*c, "head") }, nil)
		},
		"wrong verdict milestone": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { (*c)["milestone"] = "M9" }, nil)
		},
		"blank gate milestone": func(p *prWorld) {
			p.closeWith("M1", head, func(g, c, cl *map[string]any) { (*g)["milestone"] = "" }, nil)
		},
		"data moved disagrees with result": func(p *prWorld) {
			p.closeWith("M1", head, nil, func(evs []engine.Event) []engine.Event { evs[1].Data["moved"] = true; return evs })
		},
		"superseded close": func(p *prWorld) {
			p.closeWith("M1", head, nil, nil)
			p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "mc_M1", Op: OpMilestoneClose, Event: engine.EvSuperseded})
		},
		"superseded gate": func(p *prWorld) {
			p.closeWith("M1", head, nil, nil)
			p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "fg_M1", Op: "crw." + NameFinalGate, Event: engine.EvSuperseded})
		},
		"report only before close": func(p *prWorld) {
			p.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "ru_old", Op: OpReportUp, Event: engine.EvCompleted,
				Data: map[string]any{"milestone": "M1", "tip": head, "report_id": "old", "delivered": true, "result": "{}"}})
			p.closeWith("M1", head, nil, func(evs []engine.Event) []engine.Event { return evs[:2] }) // drop the fresh report
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPRWorld(t)
			calls := p.bundle(BundlePRTarget{Repo: "o/r", Checkout: "/w", HeadRef: "crw/m1", BaseRef: "dev", Remote: "origin", ReviewRecord: "d"}, nil)
			seed(p)
			out, err := p.prOpen("M1")
			if err != nil {
				t.Fatal(err)
			}
			if name == "accepted" {
				if out["state"] != "opened" || *calls != 1 {
					t.Fatalf("the proven close opens once: %v, %d call(s)", out, *calls)
				}
				return
			}
			if out["state"] != "refused" || *calls != 0 {
				t.Fatalf("%s: %v, %d call(s), want a refusal and no open", name, out, *calls)
			}
		})
	}
}

// TestPROpenNotRequiredAndDirect plans without a bundle handoff, or a nil PRTarget seam, answer not_required without external
// calls.
func TestPROpenNotRequiredAndDirect(t *testing.T) {
	p := newPRWorld(t)
	out, err := p.prOpen("M1")
	if err != nil || out["state"] != "not_required" {
		t.Fatalf("no target seam: %v %v", out, err)
	}
	calls := p.bundle(BundlePRTarget{}, nil) // no base: a direct landing
	p.close("M1", prHeadA)
	out, err = p.prOpen("M1")
	if err != nil || out["state"] != "not_required" || *calls != 0 {
		t.Fatalf("a direct landing: %v %v, %d call(s)", out, err, *calls)
	}
}

// TestPROpenProvenReReadsThePullRequest: an opened row stays proven only while the recorded pull request keeps its identity.
func TestPROpenProvenProvesIdentity(t *testing.T) {
	p := newPRWorld(t)
	calls := p.bundle(BundlePRTarget{Repo: "o/r", Checkout: "/w", HeadRef: "crw/m1", BaseRef: "dev", Remote: "origin", ReviewRecord: "d"}, nil)
	p.close("M1", prHeadA)
	if _, err := p.prOpen("M1"); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("%d opens, want 1", *calls)
	}
	row := rows(p.j.Events()).opRows(OpPROpen)[0]
	op := p.world.lane.Ops()[NamePROpen].(*prOpenOp)
	for name, pr := range map[string]tracking.PullRequest{
		"same open":       {Ref: tracking.Ref{Repo: "o/r", Number: 9}, HeadSHA: prHeadA, HeadRef: "crw/m1", HeadRepo: "o/r", BaseRef: "dev", State: tracking.StateOpen},
		"same merged":     {Ref: tracking.Ref{Repo: "o/r", Number: 9}, HeadSHA: prHeadA, HeadRef: "crw/m1", HeadRepo: "o/r", BaseRef: "dev", State: tracking.StateClosed, Merged: true},
		"closed unmerged": {Ref: tracking.Ref{Repo: "o/r", Number: 9}, HeadSHA: prHeadA, HeadRef: "crw/m1", HeadRepo: "o/r", BaseRef: "dev", State: tracking.StateClosed},
		"moved head":      {Ref: tracking.Ref{Repo: "o/r", Number: 9}, HeadSHA: prHeadB, HeadRef: "crw/m1", HeadRepo: "o/r", BaseRef: "dev", State: tracking.StateOpen},
		"other repo":      {Ref: tracking.Ref{Repo: "o/other", Number: 9}, HeadSHA: prHeadA, HeadRef: "crw/m1", HeadRepo: "o/other", BaseRef: "dev", State: tracking.StateOpen},
		"other head ref":  {Ref: tracking.Ref{Repo: "o/r", Number: 9}, HeadSHA: prHeadA, HeadRef: "crw/m2", HeadRepo: "o/r", BaseRef: "dev", State: tracking.StateOpen},
	} {
		t.Run(name, func(t *testing.T) {
			p.prs.pr = pr
			p.prs.err = nil
			ok, _, err := op.Proven(context.Background(), p.j.Events(), row)
			want := name == "same open" || name == "same merged"
			if ok != want || err != nil {
				t.Errorf("Proven = %v, %v, want %v", ok, err, want)
			}
		})
	}
}

// TestMergeRequestProvenFollowsTheRow: a requested row is proven, a refused one is not (a resume re-judges it).
func TestMergeRequestProvenFollowsTheRow(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	p.close("M1", prHeadA)
	op := p.world.lane.Ops()[NameMergeRequest].(*mergeRequestOp)
	// refused: the checks are none
	p.prs.checks.State = tracking.CheckNone
	if _, err := p.request("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}); err != nil {
		t.Fatal(err)
	}
	refused := rows(p.j.Events()).opRows(OpMergeRequest)[0]
	if ok, _, err := op.Proven(context.Background(), p.j.Events(), refused); ok || err != nil {
		t.Fatalf("a refused row is not proven: %v %v", ok, err)
	}
	p.prs.checks.State = tracking.CheckSuccess
	if _, err := p.request("M1", map[string]any{"pr": prRef, "review": p.review(prHeadA, "connected", 0)}); err != nil {
		t.Fatal(err)
	}
	all := rows(p.j.Events()).opRows(OpMergeRequest)
	if ok, _, err := op.Proven(context.Background(), p.j.Events(), all[len(all)-1]); !ok || err != nil {
		t.Fatalf("a requested row is proven: %v %v", ok, err)
	}
}
