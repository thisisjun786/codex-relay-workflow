package report

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// The report row is written before the delivery is tried: when the sender is asked, the row is on record with the digest of the body and
// the recipient the walk named, and the delivery row follows it.
func TestReportUpWritesRowBeforeDelivery(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	var atSend []Row
	w.sender.onSupervisor = func(SupervisorSend) { atSend = w.rows(KindReport, KindDelivery) }
	res := w.up(KindMilestoneReport, "M1", "milestone M1 is closed")
	if len(atSend) != 1 || atSend[0].Kind != KindReport {
		t.Fatalf("rows at the time of the send: %+v", atSend)
	}
	row := atSend[0]
	if row.Str("report_id") != res.ReportID || row.Str("report_kind") != KindMilestoneReport || row.Str("run") != testRun || row.Str("milestone") != "M1" ||
		row.Str("body_digest") != Digest([]byte("milestone M1 is closed")) || row.Str("recipient_state") != RecipientInitiative || row.Str("recipient_thread") != testLead || row.Str("recipient_initiative") != testInitiative {
		t.Errorf("the report row is %+v", row.Payload)
	}
	if got := readBody(t, row.Str("body_path")); got != "milestone M1 is closed" {
		t.Errorf("the artifact holds %q", got)
	}
	sent := w.sender.supervisorSent()
	if len(sent) != 1 || sent[0].Thread != testLead || sent[0].LogicalID != testRun+":M1:report" || sent[0].ReportID != res.ReportID || sent[0].Initiative != testInitiative || !strings.Contains(sent[0].Text, "milestone M1 is closed") {
		t.Fatalf("sent %+v", sent)
	}
	rows := w.rows(KindReport, KindDelivery)
	if len(rows) != 2 || rows[1].Kind != KindDelivery || rows[1].Str("result") != ResultAccepted || rows[1].Str("report_id") != res.ReportID || rows[1].Str("request_id") == "" {
		t.Fatalf("rows %+v", rows)
	}
	if res.Standing || res.Delivery != ResultAccepted {
		t.Errorf("result %+v", res)
	}
}

// A delivery that fails to run leaves the report row behind, and the next report-up of the same report delivers it.
func TestReportUpDeliveryThatNeverRanLeavesTheRow(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) { return SendResult{}, errTest }
	if _, err := w.writer().Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("b")}); err == nil {
		t.Fatal("the failed delivery was not reported")
	}
	if rows := w.rows(KindReport, KindDelivery); len(rows) != 1 || rows[0].Kind != KindReport {
		t.Fatalf("rows %+v", rows)
	}
	w.sender.replySupervisor = nil
	res := w.up(KindMilestoneReport, "M1", "b")
	if !res.Replayed || res.Delivery != ResultAccepted || len(w.rows(KindReport)) != 1 || len(w.rows(KindDelivery)) != 1 {
		t.Fatalf("result %+v rows %+v", res, w.rows(KindReport, KindDelivery))
	}
}

var errTest = &testError{"the send-parent command could not run"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

// The recipient is the live supervisor of the initiative above the project's parent at the time of the report: a handover between two
// reports sends the second to the new supervisor, and what the first report named stays what it was.
func TestReportUpRecipientFollowsHandover(t *testing.T) {
	w := newWorld(t)
	w.supervise("lead-old")
	first := w.up(KindMilestoneReport, "M1", "first report")
	w.handover("lead-old", "lead-new")
	second := w.up(KindMilestoneReport, "M2", "second report")
	sent := w.sender.supervisorSent()
	if len(sent) != 2 || sent[0].Thread != "lead-old" || sent[1].Thread != "lead-new" {
		t.Fatalf("sent %+v", sent)
	}
	if first.Recipient.Thread != "lead-old" || second.Recipient.Thread != "lead-new" {
		t.Errorf("recipients %+v %+v", first.Recipient, second.Recipient)
	}
	views, err := Reports(ctx, w.st.DB, testInitiative, false)
	if err != nil || len(views) != 2 || views[0].Recipient.Thread != "lead-old" || views[1].Recipient.Thread != "lead-new" {
		t.Errorf("views %+v %v", views, err)
	}
}

// No initiative supervises the project: the report is a row and nothing is sent; it stays standing, visible in the unread list.
func TestReportUpUnregisteredScopeStaysStanding(t *testing.T) {
	w := newWorld(t)
	res := w.up(KindMilestoneReport, "M1", "nobody is above")
	if len(w.sender.supervisorSent()) != 0 {
		t.Fatalf("sent %+v", w.sender.supervisorSent())
	}
	if res.Recipient.State != RecipientUnregistered || res.Recipient.Reason != "unregistered_scope" || !res.Standing || res.Delivery != "" {
		t.Fatalf("result %+v", res)
	}
	rows := w.rows(KindReport, KindDelivery)
	if len(rows) != 1 || rows[0].Str("recipient_state") != RecipientUnregistered {
		t.Fatalf("rows %+v", rows)
	}
	views, err := Reports(ctx, w.st.DB, "", true)
	if err != nil || len(views) != 1 || !views[0].Standing || views[0].ID != res.ReportID {
		t.Errorf("unread %+v %v", views, err)
	}
	// the same report again: still one row, still nothing sent
	again := w.up(KindMilestoneReport, "M1", "nobody is above")
	if !again.Replayed || len(w.rows(KindReport)) != 1 || len(w.sender.supervisorSent()) != 0 {
		t.Errorf("again %+v", again)
	}
}

// A project whose initiative has no supervisor but whose store has a seat: the walk folds the seat in, which is no initiative lead, and
// no bridge delivery goes to it.
func TestReportUpStoreSeatFoldStaysStanding(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	res := w.up(KindMilestoneReport, "M1", "only the seat is above")
	if len(w.sender.supervisorSent()) != 0 {
		t.Fatalf("a report was sent to the seat: %+v", w.sender.supervisorSent())
	}
	if res.Recipient.State != RecipientUnregistered || !strings.Contains(res.Recipient.Detail, "store seat") && !strings.Contains(res.Recipient.Detail, "store") || !res.Standing {
		t.Fatalf("result %+v", res)
	}
	rows := w.rows(KindReport)
	if len(rows) != 1 || rows[0].Str("recipient_state") != RecipientUnregistered || rows[0].Str("recipient_thread") != "" {
		t.Fatalf("rows %+v", rows)
	}
}

// The lead's thread is busy or not loaded: the sender queued the notice. The delivery row says queued with the observation, and the
// report stays standing until the pump's delivery is accepted.
func TestReportUpQueuedWhenSupervisorBusy(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) {
		return SendResult{Class: "queued", Observation: "notLoaded"}, nil
	}
	res := w.up(KindMilestoneReport, "M1", "the lead's thread is not open")
	if res.Delivery != ResultQueued || res.Observation != "notLoaded" || !res.Standing {
		t.Fatalf("result %+v", res)
	}
	rows := w.rows(KindDelivery)
	if len(rows) != 1 || rows[0].Str("result") != ResultQueued || rows[0].Str("observation") != "notLoaded" {
		t.Fatalf("rows %+v", rows)
	}
	// the pump delivers later and tells report-delivered: an accepted row follows, and the report is no longer standing
	written, err := Delivered(ctx, w.st.DB, func(run string) Appender { return ForeignAppender{Store: w.st, Run: run, Now: fixedNow} }, res.ReportID, ResultAccepted, "req-9", "")
	if err != nil || !written {
		t.Fatalf("delivered: %v %v", written, err)
	}
	if again, err := Delivered(ctx, w.st.DB, func(run string) Appender { return ForeignAppender{Store: w.st, Run: run} }, res.ReportID, ResultAccepted, "req-9", ""); err != nil || again {
		t.Errorf("the same delivery row twice: %v %v", again, err)
	}
	views, err := Reports(ctx, w.st.DB, "", false)
	if err != nil || len(views) != 1 || views[0].Standing || views[0].LastDelivery != ResultAccepted || views[0].RequestID != "req-9" {
		t.Errorf("views %+v %v", views, err)
	}
	if _, err := Delivered(ctx, w.st.DB, func(run string) Appender { return ForeignAppender{Store: w.st, Run: run} }, res.ReportID, "delivered", "x", ""); err == nil {
		t.Error("a result that is none was accepted")
	}
	if _, err := Delivered(ctx, w.st.DB, func(run string) Appender { return ForeignAppender{Store: w.st, Run: run} }, "rep_missing", ResultAccepted, "x", ""); err == nil {
		t.Error("a delivery to a report that does not exist was accepted")
	}
}

// A report whose row exists is not sent again on a replay, whatever the delivery came to (accepted, queued, refused); a delivery whose
// outcome is unknown is asked again under the same logical id.
func TestReportUpNotResentOnReplay(t *testing.T) {
	for _, class := range []string{"accepted", "queued", "refused"} {
		w := newWorld(t)
		w.supervise(testLead)
		w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) { return SendResult{Class: class, RequestID: "r1"}, nil }
		first := w.up(KindMilestoneReport, "M1", "once")
		second := w.up(KindMilestoneReport, "M1", "once")
		if len(w.sender.supervisorSent()) != 1 || !second.Replayed || second.Delivery != first.Delivery || len(w.rows(KindReport)) != 1 || len(w.rows(KindDelivery)) != 1 {
			t.Errorf("%s: sent %d times, rows %d/%d, second %+v", class, len(w.sender.supervisorSent()), len(w.rows(KindReport)), len(w.rows(KindDelivery)), second)
		}
	}
	w := newWorld(t)
	w.supervise(testLead)
	w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) { return SendResult{Class: "unknown"}, nil }
	w.up(KindMilestoneReport, "M1", "maybe")
	w.sender.replySupervisor = nil
	again := w.up(KindMilestoneReport, "M1", "maybe")
	sent := w.sender.supervisorSent()
	if len(sent) != 2 || sent[0].LogicalID != sent[1].LogicalID || again.Delivery != ResultAccepted || len(w.rows(KindDelivery)) != 2 {
		t.Errorf("unknown: sent %+v result %+v", sent, again)
	}
}

// A decision only Jun can make is a report row addressed to the store seat; nothing goes through the bridge, the pump tells the manager.
func TestJunDecisionReportIsARowForTheStoreSeat(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	w.seat("manager-seat")
	res, err := w.writer().Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.sender.supervisorSent()) != 0 || res.Recipient.State != RecipientStore || res.Recipient.Thread != "manager-seat" || !res.Standing {
		t.Fatalf("result %+v sent %+v", res, w.sender.supervisorSent())
	}
	rows := w.rows(KindReport)
	if len(rows) != 1 || rows[0].Str("report_kind") != KindJunDecisionRequest || rows[0].Str("subject") != "ship before the review?" || rows[0].Str("recipient_state") != RecipientStore {
		t.Fatalf("rows %+v", rows)
	}
}

// The unread list: a report is unread until its lead acknowledges it in a turn; the acknowledgement is one row, written once for a turn.
func TestReportAckAppendsReadRow(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	a := w.up(KindMilestoneReport, "M1", "one")
	b := w.up(KindMilestoneReport, "M2", "two")
	unread, err := Reports(ctx, w.st.DB, testInitiative, true)
	if err != nil || len(unread) != 2 {
		t.Fatalf("unread %+v %v", unread, err)
	}
	app := func(run string) Appender { return ForeignAppender{Store: w.st, Run: run, Now: fixedNow} }
	written, err := Ack(ctx, w.st.DB, app, a.ReportID, "turn-lead-5")
	if err != nil || !written {
		t.Fatalf("ack: %v %v", written, err)
	}
	if again, err := Ack(ctx, w.st.DB, app, a.ReportID, "turn-lead-5"); err != nil || again {
		t.Errorf("the same turn twice: %v %v", again, err)
	}
	rows := w.rows(KindRead)
	if len(rows) != 1 || rows[0].Str("report_id") != a.ReportID || rows[0].Str("turn") != "turn-lead-5" || rows[0].Run != testRun {
		t.Fatalf("read rows %+v", rows)
	}
	unread, err = Reports(ctx, w.st.DB, testInitiative, true)
	if err != nil || len(unread) != 1 || unread[0].ID != b.ReportID {
		t.Errorf("unread after the ack: %+v %v", unread, err)
	}
	all, _ := Reports(ctx, w.st.DB, "", false)
	if len(all) != 2 || !all[0].Read || all[0].ReadTurn != "turn-lead-5" || all[1].Read {
		t.Errorf("all %+v", all)
	}
	if _, err := Ack(ctx, w.st.DB, app, "rep_missing", "turn"); err == nil {
		t.Error("an acknowledgement of a report that does not exist was accepted")
	}
	if _, err := Ack(ctx, w.st.DB, app, a.ReportID, " "); err == nil {
		t.Error("an acknowledgement with no turn was accepted")
	}
}

// The reporter of the run's operation crw.reportUp writes the milestone report from the close and answers the id the operation records.
func TestReporterWritesTheMilestoneReportOfAClose(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	r := Reporter{W: w.writer(), Run: testRun}
	rep := ops.Report{Plan: testPlan, Milestone: "M1", Range: ops.CloseRange{Base: "b1", Tip: "t1"}, Close: ops.CloseOutcome{Wired: true, Gate: []byte(`{"ok": true}`), Final: []byte(`{"verdict":  "ALIGNED"}`)}}
	id, err := r.ReportUp(ctx, rep)
	if err != nil || !strings.HasPrefix(id, "rep_") {
		t.Fatalf("id %q err %v", id, err)
	}
	again, err := r.ReportUp(ctx, rep)
	if err != nil || again != id || len(w.rows(KindReport)) != 1 || len(w.sender.supervisorSent()) != 1 {
		t.Errorf("replay: %q %v rows %d sent %d", again, err, len(w.rows(KindReport)), len(w.sender.supervisorSent()))
	}
	row := w.rows(KindReport)[0]
	body := readBody(t, row.Str("body_path"))
	for _, want := range []string{"Milestone M1 of plan plan-1 is closed", "b1..t1", `{"ok": true}`, `{"verdict": "ALIGNED"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lacks %q: %s", want, body)
		}
	}
	if row.Str("logical_id") != testRun+":M1:report" {
		t.Errorf("logical id %q", row.Str("logical_id"))
	}
}

// A run writes its report row through its own journal: the row has the journal's sequence number and is committed before the delivery.
func TestReportRowsOfARunGoThroughItsJournal(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	wr := w.writer()
	wr.Rows = JournalAppender{Journal: w.journal, RunID: testRun, Store: w.st}
	var seqAtSend int64
	w.sender.onSupervisor = func(SupervisorSend) {
		rows := w.rows(KindReport)
		if len(rows) == 1 {
			seqAtSend = rows[0].Seq
		}
	}
	if _, err := wr.Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if seqAtSend == 0 || seqAtSend >= ForeignSeqBase {
		t.Errorf("the report row had sequence number %d when the delivery was tried", seqAtSend)
	}
	var kinds []string
	for _, ev := range w.journal.Events() {
		if ev.Kind == engine.KindFlow {
			kinds = append(kinds, ev.Event)
		}
	}
	if strings.Join(kinds, ",") != "report,report_delivery" {
		t.Errorf("flow rows of the journal: %v", kinds)
	}
}

// A milestone that is closed again over a tip that moved on is a new report with another body. The delivery layer keys a message by its
// logical id and refuses other text under an id it holds, so the revised report goes under an id of its own, and the first report keeps
// <run>:<milestone>:report (also when it is replayed). (post-evaluation d1)
func TestRevisedMilestoneReportGoesUnderItsOwnLogicalID(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	// the delivery layer's rule: one text per logical id
	texts := map[string]string{}
	w.sender.replySupervisor = func(s SupervisorSend) (SendResult, error) {
		if old, ok := texts[s.LogicalID]; ok && old != s.Text {
			return SendResult{Class: "refused", Reason: "the logical id already belongs to another message"}, nil
		}
		texts[s.LogicalID] = s.Text
		return SendResult{Class: "accepted", RequestID: "req-" + s.LogicalID}, nil
	}
	r := Reporter{W: w.writer(), Run: testRun}
	first := ops.Report{Plan: testPlan, Milestone: "M1", Range: ops.CloseRange{Base: "b1", Tip: "t1"}}
	moved := ops.Report{Plan: testPlan, Milestone: "M1", Range: ops.CloseRange{Base: "b1", Tip: "t2"}}
	id1, err := r.ReportUp(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.ReportUp(ctx, moved)
	if err != nil || id2 == id1 {
		t.Fatalf("revised report: %q (first %q) %v", id2, id1, err)
	}
	sent := w.sender.supervisorSent()
	if len(sent) != 2 || sent[0].LogicalID != testRun+":M1:report" || sent[1].LogicalID == sent[0].LogicalID || !strings.HasPrefix(sent[1].LogicalID, testRun+":M1:report") {
		t.Fatalf("sent %+v", sent)
	}
	rows := w.rows(KindReport, KindDelivery)
	if len(rows) != 4 || rows[3].Str("result") != ResultAccepted || rows[3].Str("report_id") != id2 || rows[3].Str("logical_id") != sent[1].LogicalID {
		t.Fatalf("rows %+v", rows)
	}
	// replays send nothing and change no id
	if again, err := r.ReportUp(ctx, moved); err != nil || again != id2 || len(w.sender.supervisorSent()) != 2 {
		t.Errorf("replay of the revised report: %q %v sent %d", again, err, len(w.sender.supervisorSent()))
	}
	if again, err := r.ReportUp(ctx, first); err != nil || again != id1 || len(w.sender.supervisorSent()) != 2 {
		t.Errorf("replay of the first report: %q %v sent %d", again, err, len(w.sender.supervisorSent()))
	}
	// a delivery that never ran is retried under the id the report row recorded
	third := ops.Report{Plan: testPlan, Milestone: "M1", Range: ops.CloseRange{Base: "b1", Tip: "t3"}}
	w.sender.replySupervisor = func(SupervisorSend) (SendResult, error) { return SendResult{}, errTest }
	if _, err := r.ReportUp(ctx, third); err == nil {
		t.Fatal("the failed delivery was not reported")
	}
	recorded := w.rows(KindReport)[2].Str("logical_id")
	w.sender.replySupervisor = nil
	if _, err := r.ReportUp(ctx, third); err != nil {
		t.Fatal(err)
	}
	if got := w.sender.supervisorSent(); got[len(got)-1].LogicalID != recorded || recorded == sent[0].LogicalID || recorded == sent[1].LogicalID {
		t.Errorf("retry under %q, the row recorded %q", got[len(got)-1].LogicalID, recorded)
	}
}
