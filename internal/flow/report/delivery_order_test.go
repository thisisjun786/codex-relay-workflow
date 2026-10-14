package report

import (
	"testing"
)

// CRW-1380, Argon V5 F4 of 4d293b7b: a queued delivery row is the writer's record of its staging, and the daemon's settle (accepted)
// or its cap hold (refused) can commit before the writer appends it. A queued row never supersedes a settled row (accepted or refused)
// that came before it, for the writer's replay and crw flow reports alike; in the other order the settled row supersedes the queued
// one as any later row does.

var deliveryOrders = []struct{ first, second, want string }{
	{ResultRefused, ResultQueued, ResultRefused},
	{ResultQueued, ResultRefused, ResultRefused},
	{ResultAccepted, ResultQueued, ResultAccepted},
	{ResultQueued, ResultAccepted, ResultAccepted},
}

func TestSupersedesDeliveryKeepsASettledRowOverALaterQueuedOne(t *testing.T) {
	for _, c := range deliveryOrders {
		got := c.second
		if !SupersedesDelivery(c.first, c.second) {
			got = c.first
		}
		if got != c.want {
			t.Errorf("%s then %s reads %s, want %s", c.first, c.second, got, c.want)
		}
	}
}

func TestAReportReadsItsSettledDeliveryOverAQueuedRowInEitherOrder(t *testing.T) {
	for _, c := range deliveryOrders {
		w := newWorld(t)
		rows := ForeignAppender{Store: w.st, Run: testRun, Now: fixedNow}
		id := "rep_" + c.first + "_then_" + c.second
		if _, _, err := rows.AppendOnce(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"report_id": id, "report_kind": KindMilestoneReport, "run": testRun,
			"milestone": "M1", "logical_id": testRun + ":M1:report", "recipient_state": RecipientManagement, "recipient_thread": testManagement,
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
		// the replay of the report: it answers what the delivery came to and sends nothing again
		res, err := w.relayWriter().Up(ctx, UpRequest{ID: id, Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("milestone M1 is closed")})
		if err != nil || !res.Replayed || res.Delivery != c.want || len(w.messages()) != 0 {
			t.Fatalf("%s then %s: the replay answered %+v (%v) with %d messages, want %s", c.first, c.second, res, err, len(w.messages()), c.want)
		}
	}
}
