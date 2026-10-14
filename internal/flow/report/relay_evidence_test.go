package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1380, Argon V5 F2 of 4d293b7b: the report row and the message that tells of it carry one resolution of the project. A decision
// only Jun can make that falls to the store seat is stored under the project the sender stages it through, so the command the message's
// evidence names (`crw flow reports --project <project> --unread`) lists it; a report whose project cannot be resolved is not staged
// (the channel has no project to stage it through) and is not sent through crw manage send-parent either: it stands for the store
// seat's pump, and `crw flow reports --unread` lists it.

// evidenceFilter is the listing an evidence pointer `crw flow reports [--project P] [--initiative I] [--unread]` asks for.
func evidenceFilter(t *testing.T, pointer string) ReportFilter {
	t.Helper()
	args, ok := strings.CutPrefix(pointer, "crw flow reports")
	if !ok {
		t.Fatalf("the evidence %q is no crw flow reports listing", pointer)
	}
	var f ReportFilter
	fields := strings.Fields(args)
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--unread":
			f.Unread = true
		case "--project", "--initiative":
			if i+1 >= len(fields) {
				t.Fatalf("the evidence %q names %s without a value", pointer, fields[i])
			}
			if fields[i] == "--project" {
				f.Project = fields[i+1]
			} else {
				f.Initiative = fields[i+1]
			}
			i++
		default:
			t.Fatalf("the evidence %q has %q", pointer, fields[i])
		}
	}
	return f
}

// lists says whether the listing holds the report.
func lists(views []ReportView, id string) bool {
	for _, v := range views {
		if v.ID == id {
			return true
		}
	}
	return false
}

func TestRelaySenderJunDecisionForTheStoreSeatIsListedByItsEvidence(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	msgs := w.messages()
	if res.Recipient.State != RecipientStore || res.Delivery != ResultQueued || len(msgs) != 1 || msgs[0].ObligationKind != supervisor.KindFlowJunDecisionRequest ||
		msgs[0].RecipientTaskID != "manager-seat" {
		t.Fatalf("result %+v messages %+v", res, msgs)
	}
	var packet struct {
		Evidence []string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(msgs[0].Packet), &packet); err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, pointer := range packet.Evidence {
		if !strings.HasPrefix(pointer, "crw flow reports") {
			continue
		}
		listed++
		views, err := ReportsMatching(ctx, w.st.DB, evidenceFilter(t, pointer))
		if err != nil {
			t.Fatal(err)
		}
		if !lists(views, res.ReportID) {
			t.Fatalf("the message's evidence %q lists %+v, not the report %s it tells of", pointer, views, res.ReportID)
		}
	}
	if listed == 0 {
		t.Fatalf("the message names no crw flow reports listing: %v", packet.Evidence)
	}
	if rows := w.rows(KindReport); len(rows) != 1 || rows[0].Str("recipient_project") != testProject {
		t.Fatalf("the report row is stored under project %q, want %q (the project the message is staged through)", rows[0].Str("recipient_project"), testProject)
	}
}

func TestRelaySenderLeavesAStoreSeatReportOfNoProjectStandingForThePump(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	const run = "run-noplan"
	seedRun(t, w.st, run, "", testOrch)
	wr := w.relayWriter()
	wr.Rows = ForeignAppender{Store: w.st, Run: run, Now: fixedNow}
	res, err := wr.Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: run, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if sent := w.sender.supervisorSent(); len(sent) != 0 {
		t.Fatalf("a store seat's report went through crw manage send-parent: %+v", sent)
	}
	if res.Recipient.State != RecipientStore || !res.Standing || res.Delivery != "" || len(w.messages()) != 0 || len(w.rows(KindDelivery)) != 0 {
		t.Fatalf("result %+v: %d messages, %d delivery rows", res, len(w.messages()), len(w.rows(KindDelivery)))
	}
	views, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Unread: true})
	if err != nil || !lists(views, res.ReportID) {
		t.Fatalf("crw flow reports --unread lists %+v (%v), not the report %s", views, err, res.ReportID)
	}
}
