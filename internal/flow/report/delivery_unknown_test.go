package report

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-1390, Argon V5 D3 (P1) of 118a891c: an unknown delivery row (the daemon's record of a send nobody heard back from, held_uncertain)
// is a settled row like accepted and refused. A queued row the writer appends after it - the daemon claimed, sent and recovered the message
// before the writer appended the queued row of its staging - does not hide it: the report reads unknown for crw flow reports, the writer's
// answer and its replay, and the pump (internal/manage) still raises the urgent jun_decision. Only a later settled row supersedes it: the
// readback's accepted. A replay that finds the message read (its readback settled the unknown send) stages nothing and writes no queued row
// over the accepted one, and answers accepted.

var unknownDeliveryOrders = []struct{ first, second, want string }{
	{ResultUnknown, ResultQueued, ResultUnknown},
	{ResultQueued, ResultUnknown, ResultUnknown},
	{ResultUnknown, ResultAccepted, ResultAccepted},
}

func TestSupersedesDeliveryKeepsAnUnknownRowOverALaterQueuedOne(t *testing.T) {
	for _, c := range unknownDeliveryOrders {
		got := c.second
		if !SupersedesDelivery(c.first, c.second) {
			got = c.first
		}
		if got != c.want {
			t.Errorf("%s then %s reads %s, want %s", c.first, c.second, got, c.want)
		}
	}
}

func TestAReportReadsItsUnknownDeliveryOverALaterQueuedRow(t *testing.T) {
	for _, c := range unknownDeliveryOrders {
		w := newWorld(t)
		rows := ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}
		id := "rep_" + c.first + "_then_" + c.second
		if _, _, err := rows.AppendOnce(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"report_id": id, "report_kind": KindJunDecisionRequest, "run": testRun,
			"subject": "decide", "logical_id": testRun + ":jun:report", "recipient_state": RecipientManagement, "recipient_thread": testManagement,
			"recipient_project": testProject}}, "report_id"); err != nil {
			t.Fatal(err)
		}
		for _, result := range []string{c.first, c.second} {
			if _, err := rows.Append(ctx, NewRow{Kind: KindDelivery, Payload: map[string]any{"report_id": id, "result": result}}); err != nil {
				t.Fatal(err)
			}
		}
		views, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Project: testProject})
		if err != nil || len(views) != 1 || views[0].LastDelivery != c.want || views[0].Standing != (c.want != ResultAccepted) {
			t.Fatalf("%s then %s: crw flow reports reads %+v (%v), want %s", c.first, c.second, views, err, c.want)
		}
		h, err := ReportHistory(ctx, w.st.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		if last := h.lastDelivery(); last == nil || last.Str("result") != c.want {
			t.Fatalf("%s then %s: the writer's history reads %+v, want %s", c.first, c.second, last, c.want)
		}
	}
}

// daemonAppender does what the relay daemon does at one moment of a writer's report-up: just before the writer appends its delivery row
// (the daemon claims, sends and settles or recovers the staged message first).
type daemonAppender struct {
	Appender
	before func()
}

func (a daemonAppender) Append(ctx context.Context, row NewRow) (int64, error) {
	if row.Kind == KindDelivery && a.before != nil {
		a.before()
	}
	return a.Appender.Append(ctx, row)
}

// daemonSender does what the relay daemon (or the recipient's readback) does between the writer's read of a report's rows and its staging.
type daemonSender struct {
	RelaySender
	before func()
}

func (s daemonSender) SendSupervisor(ctx context.Context, m SupervisorSend) (SendResult, error) {
	if s.before != nil {
		s.before()
	}
	return s.RelaySender.SendSupervisor(ctx, m)
}

// settleMessage moves the one supervisor message of the world to state and appends the delivery row the daemon writes in the same
// transaction (result, with the message's state as its observation).
func (w *world) settleMessage(id, state, result, reason string) {
	w.t.Helper()
	msgs := w.messages()
	if len(msgs) != 1 {
		w.t.Fatalf("%d messages, want 1", len(msgs))
	}
	if _, err := w.st.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state=? WHERE message_id=?", state, msgs[0].MessageID); err != nil {
		w.t.Fatal(err)
	}
	daemon := ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}
	if _, err := daemon.Append(ctx, NewRow{Kind: KindDelivery, Payload: map[string]any{"report_id": id, "result": result, "request_id": msgs[0].MessageID,
		"message_id": msgs[0].MessageID, "observation": state, "reason": reason}}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) deliveryResults(id string) []string {
	w.t.Helper()
	var out []string
	for _, r := range w.rows(KindDelivery) {
		if r.Str("report_id") == id {
			out = append(out, r.Str("result"))
		}
	}
	return out
}

func junDecision() UpRequest {
	return UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("the evidence")}
}

// The daemon recovers the staged decision as held_uncertain (appending unknown) before the writer appends its queued row: the report
// reads unknown and standing for the writer's answer, crw flow reports and a replay, which stages nothing and writes nothing.
func TestAnUncertainSendBeforeTheQueuedRowReadsUnknown(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	wr := w.relayWriter()
	id := ReportID(KindJunDecisionRequest, testRun, "", junDecision().Subject, Digest(junDecision().Body))
	wr.Rows = daemonAppender{Appender: wr.Rows, before: func() { w.settleMessage(id, delivery.HeldUncertain, ResultUnknown, "") }}
	res, err := wr.Up(ctx, junDecision())
	if err != nil {
		t.Fatal(err)
	}
	if got := w.deliveryResults(id); len(got) != 2 || got[0] != ResultUnknown || got[1] != ResultQueued {
		t.Fatalf("the delivery rows are %v, want the daemon's unknown before the writer's queued", got)
	}
	if res.ReportID != id || res.Delivery != ResultUnknown || !res.Standing {
		t.Fatalf("the report-up answered %+v, want unknown and standing", res)
	}
	views, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Project: testProject})
	if err != nil || len(views) != 1 || views[0].LastDelivery != ResultUnknown || !views[0].Standing {
		t.Fatalf("crw flow reports reads %+v (%v), want unknown and standing", views, err)
	}
	again, err := w.relayWriter().Up(ctx, junDecision())
	if err != nil || !again.Replayed || again.Delivery != ResultUnknown || !again.Standing {
		t.Fatalf("the replay answered %+v (%v), want unknown and standing", again, err)
	}
	if got := w.deliveryResults(id); len(got) != 2 || len(w.messages()) != 1 {
		t.Fatalf("after the replay the delivery rows are %v with %d messages", got, len(w.messages()))
	}
}

// A replay reads the report's rows (unknown), and before it stages, the recipient's verified readback settles the send (the message is
// read, accepted is appended). The replay stages nothing and writes no queued row over the accepted one; it answers accepted, not
// standing, as crw flow reports reads it.
func TestAReplayWhoseUncertainSendTheReadbackSettlesAnswersAccepted(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	first, err := w.relayWriter().Up(ctx, junDecision())
	if err != nil || first.Delivery != ResultQueued {
		t.Fatalf("the report-up answered %+v (%v)", first, err)
	}
	w.settleMessage(first.ReportID, delivery.HeldUncertain, ResultUnknown, "")
	wr := w.relayWriter()
	wr.Sender = daemonSender{RelaySender: wr.Sender.(RelaySender), before: func() { w.settleMessage(first.ReportID, "read", ResultAccepted, "readback") }}
	again, err := wr.Up(ctx, junDecision())
	if err != nil {
		t.Fatal(err)
	}
	if got := w.deliveryResults(first.ReportID); len(got) != 3 || got[0] != ResultQueued || got[1] != ResultUnknown || got[2] != ResultAccepted {
		t.Fatalf("the delivery rows are %v, want queued, unknown and the readback's accepted (no queued row after it)", got)
	}
	if !again.Replayed || again.Delivery != ResultAccepted || again.Standing {
		t.Fatalf("the replay answered %+v, want accepted and not standing", again)
	}
	views, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Project: testProject})
	if err != nil || len(views) != 1 || views[0].LastDelivery != ResultAccepted || views[0].Standing {
		t.Fatalf("crw flow reports reads %+v (%v), want accepted", views, err)
	}
}
