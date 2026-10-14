package report

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1380 (CRW-1314 lane D2, design §3): the flow's reports, escalations and completion digest go
// up through the supervisor channel's direct staging (RelaySender). Staging writes only queued; the
// accepted or refused delivery is the daemon's settle (internal/relay/adapter's end-to-end tests).

// relayWriter is the world's writer over the channel, with a fallback that records what reached it.
func (w *world) relayWriter() *Writer {
	return &Writer{Store: w.st, Rows: ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}, Resolver: StoreResolver{Store: w.st},
		Sender: RelaySender{Store: w.st, Fallback: w.sender}, ArtifactDir: w.t.TempDir()}
}

// messages are the supervisor messages of the store.
func (w *world) messages() []store.SupervisorMessagesRow {
	w.t.Helper()
	rows, err := w.st.All(ctx, "SELECT message_id FROM supervisor_messages ORDER BY staged_at, message_id")
	if err != nil {
		w.t.Fatal(err)
	}
	out := []store.SupervisorMessagesRow{}
	for _, r := range rows {
		m, err := w.st.SupervisorMessage(ctx, r.Text("message_id"))
		if err != nil {
			w.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestRelaySenderStagesTheReportForTheManagementSeatAndWritesOnlyQueued(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.manage(testManagement)
	res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("milestone M1 is closed")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivery != ResultQueued || !res.Standing || res.Recipient.State != RecipientManagement {
		t.Fatalf("result %+v", res)
	}
	msgs := w.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.ObligationKind != supervisor.KindFlowMilestoneReport || m.RelationshipID != "project:"+testProject || m.RecipientTaskID != testManagement ||
		m.State != "queued" || m.Subject != testRun+"/"+res.ReportID || !strings.Contains(m.Packet, "milestone M1 is closed") {
		t.Fatalf("message %+v", m)
	}
	rows := w.rows(KindDelivery)
	if len(rows) != 1 || rows[0].Str("result") != ResultQueued || rows[0].Str("request_id") != m.MessageID {
		t.Fatalf("delivery rows %+v", rows)
	}
	if len(w.sender.supervisorSent())+len(w.sender.parentSent()) != 0 {
		t.Fatal("the report went through send-parent")
	}
	// the same report again (a resume): one message, one queued row
	again, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("milestone M1 is closed")})
	if err != nil || !again.Replayed || len(w.messages()) != 1 || len(w.rows(KindDelivery)) != 1 {
		t.Fatalf("again %+v (%v): %d messages, %d rows", again, err, len(w.messages()), len(w.rows(KindDelivery)))
	}
}

// D1c d2: a report that falls to the store seat is staged for it too.
func TestRelaySenderStagesAStoreSeatReport(t *testing.T) {
	w := newWorld(t)
	w.seat("01manager-seat")
	res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindProjectBlocked, Run: testRun, Subject: "blocked on review", Body: []byte("blocked")})
	if err != nil {
		t.Fatal(err)
	}
	msgs := w.messages()
	if res.Recipient.State != RecipientStore || res.Delivery != ResultQueued || len(msgs) != 1 || msgs[0].RecipientTaskID != "01manager-seat" ||
		msgs[0].ObligationKind != supervisor.KindFlowProjectBlocked {
		t.Fatalf("result %+v messages %+v", res, msgs)
	}
}

// A decision only Jun can make goes to the management seat above the project through the channel;
// under a legacy lead (no management seat) the channel would name the lead, who does not decide for
// Jun: nothing is staged and the report stands for the store seat, as before.
func TestRelaySenderJunDecision(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	msgs := w.messages()
	if res.Delivery != ResultQueued || len(msgs) != 1 || msgs[0].RecipientTaskID != testManagement || msgs[0].ObligationKind != supervisor.KindFlowJunDecisionRequest {
		t.Fatalf("result %+v messages %+v", res, msgs)
	}
	lead := newWorld(t)
	lead.supervise(testLead)
	lead.seat("manager-seat")
	res, err = lead.relayWriter().Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if len(lead.messages()) != 0 || !res.Standing || res.Delivery != "" || len(lead.rows(KindDelivery)) != 0 || res.Recipient.State != RecipientStore {
		t.Fatalf("under a lead: result %+v messages %d rows %d", res, len(lead.messages()), len(lead.rows(KindDelivery)))
	}
}

// Nobody above the project: the channel refuses, nothing is staged and no delivery row is written;
// the report stands and the refusal is its reason.
func TestRelaySenderNobodyAboveLeavesTheReportStanding(t *testing.T) {
	w := newWorld(t)
	res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("nobody")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Standing || len(w.messages()) != 0 || len(w.rows(KindDelivery)) != 0 {
		t.Fatalf("result %+v", res)
	}
}

// The run's escalations and its completion digest go to the level above the project as
// flow_escalation and flow_completion; the summary notice, which the channel has no kind for, keeps
// the fallback (crw manage send-parent).
func TestRelaySenderCarriesTheRunHooksNotices(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	h := w.hook()
	h.Sender = RelaySender{Store: w.st, Fallback: w.sender}
	w.escalate("esc_1", "tiers_exhausted", "A", "the executor-t1 tier ended with the same finding twice")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	if err := h.RunFinished(ctx, testRun, engine.RunCompleted); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, m := range w.messages() {
		if m.RecipientTaskID != testManagement || m.State != "queued" || !strings.HasPrefix(m.Subject, testRun+"/") {
			t.Errorf("message %+v", m)
		}
		kinds[m.ObligationKind]++
	}
	if kinds[supervisor.KindFlowEscalation] != 1 || kinds[supervisor.KindFlowCompletion] != 1 || len(kinds) != 2 {
		t.Fatalf("message kinds %v", kinds)
	}
	if rows := w.rows(KindEscalationNotice); len(rows) != 1 || rows[0].Str("result") != "queued" {
		t.Fatalf("escalation rows %+v", rows)
	}
	if rows := w.rows(KindCompletionNotice); len(rows) != 1 || rows[0].Str("result") != "queued" {
		t.Fatalf("completion rows %+v", rows)
	}
	if len(w.sender.parentSent()) != 0 {
		t.Fatalf("a notice went through send-parent: %+v", w.sender.parentSent())
	}
	// a second reconcile tells nothing again
	if err := h.BatchCommitted(ctx, testRun); err != nil || len(w.messages()) != 2 {
		t.Fatalf("again: %v, %d messages", err, len(w.messages()))
	}
	// the summary notice has no channel kind: it goes through the fallback
	if _, err := (RelaySender{Store: w.st, Fallback: w.sender}).SendParent(ctx, ParentSend{Thread: testOrch, LogicalID: "summary-s1", Text: "owed", Queue: true}); err != nil {
		t.Fatal(err)
	}
	if sent := w.sender.parentSent(); len(sent) != 1 || sent[0].LogicalID != "summary-s1" {
		t.Fatalf("the summary notice went %+v", sent)
	}
}

// The settle writes the run's rows with the sequence numbers a command takes.
func TestFlowForeignSeqBaseIsTheEngines(t *testing.T) {
	if supervisor.FlowForeignSeqBase != engine.ForeignSeqBase {
		t.Fatalf("supervisor.FlowForeignSeqBase %d, engine.ForeignSeqBase %d", supervisor.FlowForeignSeqBase, engine.ForeignSeqBase)
	}
}
