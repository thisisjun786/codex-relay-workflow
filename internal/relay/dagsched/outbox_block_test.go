package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-283: what a summary says, and how the relay reads a document for it. The block and the container are plain text the connector keeps byte for byte (HTML comment markers and a
// fenced body, the grammar the relationship outbox already relies on); the relay judges a document only by parsing these markers, never by looking for words.

func claimedEntry(t *testing.T, f *fixture, document string) (SummaryEntry, SummaryOperation, string) {
	t.Helper()
	e, err := f.sched.EnqueueSummary(context.Background(), "p1", "parent", document, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.sched.ClaimSummary(context.Background(), e.Entry.SummaryID, "parent")
	if err != nil {
		t.Fatal(err)
	}
	return c.Entry, c.Operation, c.Token
}

func reconcile(t *testing.T, f *fixture, id, doc string) SummaryReconciliation {
	t.Helper()
	rec, err := f.sched.ReconcileSummary(context.Background(), id, doc)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A summary is a function of the progress it states: the same store state renders to the same bytes, and the text names the plan, the revision, the counts, each stage and each node.
func TestSummaryTextStatesTheProgress(t *testing.T) {
	f := summaryFixture(t)
	f.acceptNode("p1", "research", acceptOpts{})
	f.startNode("p1", "design")
	p := f.progress("p1")
	text := SummaryText(p)
	if text != SummaryText(f.progress("p1")) {
		t.Fatal("two readings of one store state render differently")
	}
	for _, want := range []string{"p1", "P-TEST", "revision 1", "accepted 1 of 7", "integrated 0 of 7", "research", "design", "CRW-research", "stage accepted", "stage running"} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary does not say %q:\n%s", want, text)
		}
	}
	for _, node := range p.Nodes {
		if !strings.Contains(text, node.NodeID) {
			t.Errorf("node %s is missing from the summary", node.NodeID)
		}
	}
	if strings.Contains(text, "2026-") {
		t.Errorf("a time is in the summary, which must not depend on one:\n%s", text)
	}
}

// What a document says of an entry. Each outcome is a case a lost response or a concurrent writer produces, and the parent's next move depends on which.
func TestSummaryReconcileReadsTheDocument(t *testing.T) {
	f := summaryFixture(t)
	e1, op, _ := claimedEntry(t, f, "doc-1")
	page := "# Coordination\n\nSome text before.\n\n"
	cases := []struct {
		name      string
		doc       string
		outcome   string
		container string
		relation  string
		repair    string
	}{
		{"no container", page, "absent", "absent", "", "initialize"},
		{"an empty container", page + op.EmptyContainer + "\n", "absent", "present", "", "replace_container"},
		{"the entry's block", page + op.Container + "\nAfter.\n", "already_written", "present", "", "none"},
		{"carriage returns and trailing blanks (a connector that normalises)", strings.ReplaceAll(page+op.Container+"\n", "\n", "  \r\n"), "already_written", "present", "", "none"},
		{"the block with a line changed", page + strings.Replace(op.Container, "projectKey: P-TEST", "projectKey: P-TESX", 1) + "\n", "stale", "present", "same", "replace_container"},
		{"the block twice", page + strings.Replace(op.Container, op.ContainerEnd, op.Block+"\n"+op.ContainerEnd, 1) + "\n", "duplicate", "present", "", "replace_container"},
		{"a block that is not closed", page + strings.Replace(op.Container, "<!-- /relay-dag-summary:"+e1.SummaryID+" -->", "", 1) + "\n", "malformed", "present", "", "manual"},
		{"a container that is not closed", page + op.ContainerStart + "\n" + op.Block + "\n", "malformed", "present", "", "manual"},
		{"the block outside the container", page + op.EmptyContainer + "\n" + op.Block + "\n", "malformed", "present", "", "manual"},
		{"two containers", page + op.Container + "\n" + op.Container + "\n", "malformed", "present", "", "manual"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := reconcile(t, f, e1.SummaryID, c.doc)
			if rec.Outcome != c.outcome || rec.Container != c.container || rec.Relation != c.relation || rec.Repair != c.repair || rec.Detail == "" {
				t.Fatalf("%+v", rec)
			}
			if rec.State != SummaryClaimed || !rec.Writable {
				t.Fatalf("an entry that is claimed and newest is writable: %+v", rec)
			}
			if c.outcome == "already_written" || c.outcome == "stale" {
				if rec.PreviousBlock == "" || rec.DocumentSummaryID != e1.SummaryID {
					t.Fatalf("the block the document holds is not reported: %+v", rec)
				}
			}
			if c.outcome != "already_written" {
				// whatever the document says, completing from it is refused unless it says already_written
				_, err := f.sched.CompleteSummary(context.Background(), e1.SummaryID, "parent", reTokenFor(t, f, e1.SummaryID), "doc-1", c.doc)
				wantRefusal(t, err, contract.RefusalReadbackMismatch)
			}
		})
	}
	// and a document the connector normalised confirms the entry
	normalised := strings.ReplaceAll(page+op.Container+"\n", "\n", "  \r\n")
	done, err := f.sched.CompleteSummary(context.Background(), e1.SummaryID, "parent", reTokenFor(t, f, e1.SummaryID), "doc-1", normalised)
	if err != nil || done.Entry.State != SummaryConfirmed {
		t.Fatalf("complete: %+v %v", done, err)
	}
}

// reTokenFor claims the entry again and returns the fresh token.
func reTokenFor(t *testing.T, f *fixture, id string) string {
	t.Helper()
	c, err := f.sched.ClaimSummary(context.Background(), id, "parent")
	if err != nil {
		t.Fatal(err)
	}
	return c.Token
}

// A document that holds a summary of another entry of the stream: an older one (the entry still has to be written), a newer one (it must not be written: the newest entry is the one
// to take), and the entry's own once it is confirmed.
func TestSummaryReconcileComparesEntriesOfOneStream(t *testing.T) {
	f := summaryFixture(t)
	e1, op1, _ := claimedEntry(t, f, "doc-1")
	f.bump(1)
	e2, op2, _ := claimedEntry(t, f, "doc-1")
	older := "page\n" + op1.Container + "\n"
	newer := "page\n" + op2.Container + "\n"
	rec := reconcile(t, f, e2.SummaryID, older)
	if rec.Outcome != "stale" || rec.Relation != "older" || rec.DocumentSeq != 1 || rec.DocumentSummaryID != e1.SummaryID || !rec.Writable {
		t.Fatalf("the document holds an older summary: %+v", rec)
	}
	rec = reconcile(t, f, e1.SummaryID, newer)
	if rec.Outcome != "stale" || rec.Relation != "newer" || rec.DocumentSeq != 2 || rec.DocumentSummaryID != e2.SummaryID || rec.Writable || rec.State != SummarySuperseded {
		t.Fatalf("the document holds a newer summary than the entry: %+v", rec)
	}
	if rec := reconcile(t, f, e1.SummaryID, older); rec.Outcome != "already_written" || rec.Writable {
		t.Fatalf("a superseded entry is not writable even when the document carries it: %+v", rec)
	}
	_, err := f.sched.ReconcileSummary(context.Background(), "sum-nothing", older)
	wantRefusal(t, err, contract.RefusalUnregisteredScope)
}

// Two plans of one project may write one document: each has its own container, so a readback that carries both containers confirms the entry whose block sits in its own, and a block of
// the other plan is no problem of this one.
func TestSummaryContainersOfTwoPlansDoNotMix(t *testing.T) {
	f := summaryFixture(t)
	f.putPlan("p2", 0, "p2-r1", addNode("a", dag.NodeNonPR))
	ctx := context.Background()
	e1, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-shared", false)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := f.sched.EnqueueSummary(ctx, "p2", "parent", "doc-shared", false)
	if err != nil {
		t.Fatal(err)
	}
	if e1.Entry.Seq != 1 || e2.Entry.Seq != 1 || e1.Entry.SummaryID == e2.Entry.SummaryID {
		t.Fatalf("two plans, one document: %+v %+v", e1.Entry, e2.Entry)
	}
	c1, err := f.sched.ClaimSummary(ctx, e1.Entry.SummaryID, "parent")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := f.sched.ClaimSummary(ctx, e2.Entry.SummaryID, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Operation.ContainerStart == c2.Operation.ContainerStart {
		t.Fatal("two plans share a container")
	}
	doc := "page\n" + c1.Operation.Container + "\n\n" + c2.Operation.Container + "\n"
	if _, err := f.sched.CompleteSummary(ctx, e1.Entry.SummaryID, "parent", c1.Token, "doc-shared", doc); err != nil {
		t.Fatalf("plan 1: %v", err)
	}
	if _, err := f.sched.CompleteSummary(ctx, e2.Entry.SummaryID, "parent", c2.Token, "doc-shared", doc); err != nil {
		t.Fatalf("plan 2: %v", err)
	}
}

// A summary whose text holds backtick fences (a node title) still sits in a fence of its own that it cannot close, and reads back as itself.
func TestSummaryTextWithFencesStaysInsideItsBlock(t *testing.T) {
	bt := "```"
	f := newFixture(t)
	node := addNode("evil", dag.NodeNonPR)
	node["node"].(doc)["title"] = "x ```text end --> <!-- /relay-dag-summary:sum-x -->"
	f.putPlan("p1", 0, "r1", node)
	f.projectParent()
	e, op, token := claimedEntry(t, f, "doc-1")
	if !strings.Contains(op.Block, "evil") || !strings.Contains(op.Block, bt+"text end") || strings.Contains(op.Block, "\n"+bt+"text end") {
		t.Fatalf("the block does not carry the node: %s", op.Block)
	}
	rec := reconcile(t, f, e.SummaryID, "page\n"+op.Container+"\n")
	if rec.Outcome != "already_written" {
		t.Fatalf("%+v\n%s", rec, op.Block)
	}
	if _, err := f.sched.CompleteSummary(context.Background(), e.SummaryID, "parent", token, "doc-1", "page\n"+op.Container+"\n"); err != nil {
		t.Fatal(err)
	}
}
