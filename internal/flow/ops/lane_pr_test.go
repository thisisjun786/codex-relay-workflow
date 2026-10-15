package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "ms_" + ms, Op: OpMilestone, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": ms, "plan": "plan-1", "result": "{}"}})
	}
	return p
}

// open sets the pull request to be open at head with green checks at head.
func (p *prWorld) open(head string) {
	p.prs.pr = tracking.PullRequest{Ref: tracking.Ref{Repo: "o/r", Number: 7}, State: tracking.StateOpen, HeadSHA: head, BaseRef: "dev"}
	p.prs.checks = tracking.Checks{PullRequest: p.prs.pr.Ref, Head: head, State: tracking.CheckSuccess}
}

func review(bots string, unresolved int) map[string]any {
	return map[string]any{"bots": bots, "unresolved": unresolved}
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
			p.prs.checks.State = tc.state
			out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("connected", 0)})
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
		out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("connected", 0)})
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
		wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("connected", 0)}), "pr_not_open")
		p.prs.pr.State, p.prs.pr.Merged = tracking.StateOpen, true
		wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("connected", 0)}), "pr_not_open")
		if len(p.rep.sent) != 0 {
			t.Error("a pull request that is not open was requested")
		}
	})
}

func TestMergeRequestRefusesAHeadThatMovedFromTheCrosscheckedOne(t *testing.T) {
	arg := map[string]any{"pr": prRef, "review": review("connected", 0)}
	t.Run("pull request head is not the crosschecked one", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadB) // M1 is verified at A, the pull request is at B, green
		wantRefused(t, p.mustRequest("M1", arg), "head_moved")
		if len(p.rep.sent) != 0 {
			t.Error("sent")
		}
	})
	t.Run("checks read a head the pull request no longer has", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		p.prs.checks.Head = prHeadB
		wantRefused(t, p.mustRequest("M1", arg), "head_moved")
		if len(p.rep.sent) != 0 {
			t.Error("sent")
		}
	})
	t.Run("a verified row of the head the verdict names elsewhere", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA) // M4's row is at A, its verdict object names C
		wantRefused(t, p.mustRequest("M4", arg), "crosscheck_not_accepted")
	})
	t.Run("needs_changes and evidence are not accepted", func(t *testing.T) {
		p := newPRWorld(t)
		p.open("dddddddddddddddddddddddddddddddddddddddd")
		wantRefused(t, p.mustRequest("M2", arg), "crosscheck_not_accepted")
		p.open(prHeadA)
		wantRefused(t, p.mustRequest("M5", arg), "crosscheck_not_accepted") // held: no verdict object
	})
	t.Run("a milestone with no row", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		wantRefused(t, p.mustRequest("M9", arg), "crosscheck_missing")
	})
	t.Run("the verified round after an evidence round is accepted", func(t *testing.T) {
		p := newPRWorld(t)
		p.open(prHeadA)
		if out := p.mustRequest("M6", arg); out["state"] != "requested" {
			t.Fatalf("M6: %v", out)
		}
	})
}

func TestMergeRequestRefusesUnresolvedComments(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	wantRefused(t, p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("connected", 2)}), "comments_unresolved")
	if len(p.rep.sent) != 0 {
		t.Fatal("unresolved comments were requested")
	}
	// the bots are not connected: there is nothing to resolve, the request goes out and says so
	out := p.mustRequest("M1", map[string]any{"pr": prRef, "review": review("not_connected", 0)})
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
	if _, err := p2.request("M1", map[string]any{"pr": prRef}); err == nil {
		t.Error("a request with no review argument was not an error")
	}
	if _, err := p2.request("M1", map[string]any{"pr": prRef, "review": review("sometimes", 0)}); err == nil {
		t.Error("a review with an unknown bots value was not an error")
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
	arg := map[string]any{"pr": prRef, "review": review("connected", 0)}
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
	upper := p.mustRequest("M1", map[string]any{"pr": "O/R#7", "review": review("connected", 0)})
	if upper["state"] != "requested" || upper["repeated"] != true || len(p.rep.sent) != 1 {
		t.Fatalf("another case: %v, sent %d", upper, len(p.rep.sent))
	}
	// the head moved to B, and B has a verified row of M3: a second send
	p.open(prHeadB)
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
	if _, err := p.request("M3", map[string]any{"pr": "o/r#8", "review": review("connected", 0)}); err == nil {
		t.Error("no reporter was not an error")
	}
	// a send that fails is the op's error and writes no requested row
}

func TestMergeRequestFailedSendIsAnErrorAndRecordsNothing(t *testing.T) {
	p := newPRWorld(t)
	p.open(prHeadA)
	p.world.lane.d.Reporter = &stubMergeReporter{err: errors.New("down")}
	if _, err := p.request("M1", map[string]any{"pr": prRef, "review": review("connected", 0)}); err == nil {
		t.Fatal("a failed send was not an error")
	}
	if got := rows(p.j.Events()).opRows(OpMergeRequest); len(got) != 0 {
		t.Errorf("a failed send left %d row(s)", len(got))
	}
}
