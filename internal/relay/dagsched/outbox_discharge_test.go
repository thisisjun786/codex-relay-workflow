package dagsched

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1270 (follow-up of CRW-1216): the supervisor's discharge of a plan relationship's verdict, run through the writers that make the record in this order and nothing seeded by hand:
// Ack.RecordVerdict (the verdict, stamped by the delivery clock) -> Scheduler.EnqueueSummary (the entry, stamped by the scheduler clock) -> ClaimSummary -> CompleteSummary -> the three readers of
// the supervisor (Reportable, ReportableCurrent, Selection). Both clocks are injected, so the instant of the verdict and the instant of the entry are the test's and the order
// created_at >= decided_at that delivery.PlanSummaryCarriesVerdict relies on is stated, not left to the wall clock. The stores are temporary.

const dischargeTargetRef = "doc-1"

// verdictClock is the delivery clock of the test: it never moves, so the verdict is decided at one named instant.
type verdictClock struct{ at time.Time }

func (c verdictClock) Now() float64 { return float64(c.at.UnixMicro()) / 1e6 }
func (c verdictClock) ISO() string  { return c.at.UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// schedulerClock is the Scheduler.Now of the test: the instant it starts at, one microsecond more at every reading (it never goes back).
type schedulerClock struct{ next time.Time }

func (c *schedulerClock) now() string {
	at := c.next
	c.next = c.next.Add(time.Microsecond)
	return at.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

type dischargeKit struct {
	f        *fixture
	ack      *delivery.Ack
	channel  *supervisor.Channel
	rid      string
	event    string
	criteria string
	decided  time.Time
}

// newDischargeKit is a plan relationship ready to be ruled: the relationship of node impl-a of plan p1 with its final report acknowledged (the rows the intake and the ack leave), the coordination
// document target recorded as targetRef, and no verdict yet.
func newDischargeKit(t *testing.T, targetRef string) *dischargeKit {
	t.Helper()
	ctx := context.Background()
	f := summaryFixture(t)
	r := f.reportNode("p1", "impl-a", acceptOpts{})
	n, _ := nodeOf(f.snapshot("p1"), "impl-a")
	rid, event := r.Acceptance.RelationshipID, r.Event
	// reportNode seeds the ruling too; the ruling is what this test makes, so it is taken out
	f.exec("DELETE FROM verdict_context WHERE event_id = ?", event)
	f.exec("DELETE FROM verdicts WHERE event_id = ?", event)
	for _, stmt := range []string{
		"INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('" + rid + "','coordination_document','" + targetRef + "','t')",
		"INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at)" +
			" VALUES ('" + event + "',1," + "'" + rid + "',1,'" + r.Acceptance.RevisionHash + "','owner/repo','DONE','proved','v1','the work is done','merge','t')",
	} {
		f.exec(stmt)
	}
	if err := storeseed.RecordRelationshipScope(ctx, f.s, rid, "P-TEST", "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{
		{BindingID: "b-child", Role: "child", ScopeKind: "issue", ScopeKey: n.IssueKey, TaskID: "child-impl-a", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
		{BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "supervisor", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
	} {
		if err := storeseed.InsertScopeBinding(ctx, f.s, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []store.ScopeLinksRow{
		{LinkID: "lnk-issue", LinkKind: "execution", UpperKind: "project", UpperKey: "P-TEST", UpperTaskID: "parent", LowerKind: "issue", LowerKey: n.IssueKey, LowerTaskID: "child-impl-a", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
		{LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-1", UpperTaskID: "supervisor", LowerKind: "project", LowerKey: "P-TEST", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
	} {
		if err := storeseed.InsertScopeLink(ctx, f.s, l); err != nil {
			t.Fatal(err)
		}
	}
	decided := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := verdictClock{at: decided}
	ack := delivery.NewAck(delivery.NewService(f.s, clock))
	// the hook the command wires (delivery/cli.go): a plan relationship enqueues no sync_outbox row
	ack.Sync = delivery.VerdictSync(f.s, clock)
	return &dischargeKit{f: f, ack: ack, channel: &supervisor.Channel{Store: f.s, Linkage: supervisor.StoreLinkage{Store: f.s}, Program: "/usr/bin/codex-session-relay"},
		rid: rid, event: event, criteria: n.CriteriaSetDigest, decided: decided}
}

// rule is the verified ruling through Ack.RecordVerdict.
func (k *dischargeKit) rule() {
	k.f.t.Helper()
	findings := []any{delivery.Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}, {Key: "note", Value: "the criterion holds"}}}
	if _, err := k.ack.RecordVerdict(context.Background(), k.event, "verified", "verdict-turn-1", nil, findings, nil, k.criteria); err != nil {
		k.f.t.Fatalf("RecordVerdict: %v", err)
	}
	var decidedAt string
	want := verdictClock{at: k.decided}.ISO()
	if err := k.f.s.DB.QueryRow("SELECT decided_at FROM verdicts WHERE event_id = ?", k.event).Scan(&decidedAt); err != nil || decidedAt != want {
		k.f.t.Fatalf("the verdict is decided at %q (%v), want the delivery clock's %q", decidedAt, err, want)
	}
}

// owed asks the three readers of the supervisor whether the verdict of the event still stands undischarged; they must agree.
func (k *dischargeKit) owed(label string) bool {
	k.f.t.Helper()
	ctx := context.Background()
	o, err := k.channel.FromEvent(ctx, k.event)
	if err != nil || o == nil {
		k.f.t.Fatalf("%s: obligation %v: %v", label, o, err)
	}
	current, currentReason, err := k.channel.ReportableCurrent(ctx, *o)
	if err != nil {
		k.f.t.Fatalf("%s: ReportableCurrent: %v", label, err)
	}
	reportable, reportableReason, err := k.channel.Reportable(ctx, *o)
	if err != nil {
		k.f.t.Fatalf("%s: Reportable: %v", label, err)
	}
	now := float64(k.decided.Add(time.Hour).UnixMicro()) / 1e6
	answer, err := k.channel.Selection(ctx, *o, "", &now)
	if err != nil {
		k.f.t.Fatalf("%s: Selection: %v", label, err)
	}
	standing := answer["standing"] != "discharged"
	if current != reportable || current != standing {
		k.f.t.Fatalf("%s: the readers disagree: ReportableCurrent %v (%q), Reportable %v (%q), Selection standing %v (%v)", label, current, currentReason, reportable, reportableReason, answer["standing"], answer["dischargeReason"])
	}
	if !current {
		const want = "already_in_the_record_the_supervisor_reads"
		if currentReason != want || reportableReason != want || answer["reason"] != want {
			k.f.t.Fatalf("%s: discharged for another reason: %q, %q, %v", label, currentReason, reportableReason, answer["reason"])
		}
	}
	return current
}

func (k *dischargeKit) syncRows() int {
	k.f.t.Helper()
	return k.f.count("SELECT COUNT(*) FROM sync_outbox WHERE relationship_id = ?", k.rid)
}

// The real order: the verdict, the entry, the claim, the confirmation. Until the entry is confirmed the verdict stands; the confirmed entry, stamped after the verdict, discharges it in all three readers, and
// the verdict made no sync_outbox row (the summary entry is its record). An entry stamped before the verdict, or one written to a document that is not the target as spelled, does not.
func TestPlanSummaryDischargesTheVerdictThroughTheRecordingWriters(t *testing.T) {
	t.Parallel()
	const document = "doc-1"
	for _, tc := range []struct {
		name      string
		offset    time.Duration // the scheduler clock's first reading against the instant the verdict was decided
		document  string
		discharge bool
	}{
		{"entry stamped after the verdict", time.Second, document, true},
		{"entry stamped at the instant of the verdict", 0, document, true},
		{"entry stamped before the verdict", -time.Second, document, false},
		// the two spellings are compared as the text they are: a document named by its URL is not the target recorded by its id (the standing verdict is then reported once more, the safe side)
		{"document spelled otherwise than the target", time.Second, "https://linear.app/team/document/doc-1-abc123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newDischargeKit(t, dischargeTargetRef)
			clock := &schedulerClock{next: k.decided.Add(tc.offset)}
			k.f.sched.Now = clock.now
			if !k.owed("before the ruling") {
				t.Fatal("the obligation was discharged before any verdict")
			}
			k.rule()
			if n := k.syncRows(); n != 0 {
				t.Fatalf("the verdict of a plan relationship made %d sync_outbox rows", n)
			}
			if !k.owed("verdict recorded") {
				t.Fatal("the verdict discharged the obligation before any summary entry was made")
			}
			flow := newParentFlow(k.f, newFakeLinear(), tc.document)
			enq := flow.enqueue()
			if enq.Entry.State != SummaryPending || !strings.Contains(enq.Entry.Summary, "verified") {
				t.Fatalf("the entry = %+v, want a pending entry that states the verdict", enq.Entry)
			}
			if !k.owed("entry pending") {
				t.Fatal("a pending entry discharged the obligation")
			}
			claim, ok := flow.claimNewest()
			if !ok || claim.Entry.SummaryID != enq.Entry.SummaryID {
				t.Fatalf("claim = %+v %v", claim, ok)
			}
			if !k.owed("entry claimed") {
				t.Fatal("a claimed entry discharged the obligation")
			}
			done := flow.proceed(claim)
			if done.Step != "wrote" || done.Entry.State != SummaryConfirmed {
				t.Fatalf("the parent's turn = %+v", done)
			}
			if got := entryBySeq(k.f, tc.document, 1); got.State != SummaryConfirmed {
				t.Fatalf("the entry is %+v", got)
			}
			if owed := k.owed("entry confirmed"); owed == tc.discharge {
				t.Fatalf("after the confirmation the verdict is owed=%v, want discharged=%v", owed, tc.discharge)
			}
			if n := k.syncRows(); n != 0 {
				t.Fatalf("the drain made %d sync_outbox rows", n)
			}
		})
	}
}
