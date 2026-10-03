package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-283, review findings: the write protocol must never put a summary into a document that holds a newer one, never make two containers, and never prescribe a repair that cannot work.

// An entry that is overtaken between its claim and its read is not written: the relay says so (the entry is superseded, so it is not writable; the document holds the newer summary) and the
// parent that follows the protocol stops there. The old claimant reads the document AFTER the newer summary landed, so a write of its container text would match what it just read and pass the
// connector's condition: only the rule 'never write when the entry is not writable or the document holds a newer one' stops it.
func TestSummaryAnOvertakenClaimantThatReadsTheNewerSummaryWritesNothing(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	e1 := flow.enqueue().Entry
	claimA, ok := flow.claimNewest()
	if !ok || claimA.Entry.SummaryID != e1.SummaryID {
		t.Fatalf("claim %+v", claimA)
	}
	f.bump(1)
	e2 := flow.enqueue().Entry
	if got := flow.drain(); got.Step != "wrote" || got.Entry.SummaryID != e2.SummaryID {
		t.Fatalf("the newer summary: %+v", got)
	}
	// the old claimant now reads the document: it carries summary 2
	rec, err := f.sched.ReconcileSummary(ctx, e1.SummaryID, lin.read())
	if err != nil || rec.Outcome != "stale" || rec.Relation != "newer" || rec.Writable || rec.Repair != "none" || !strings.Contains(rec.Detail, "do not write") {
		t.Fatalf("reconcile for the overtaken entry: %+v %v", rec, err)
	}
	if got := flow.proceed(claimA); got.Step != "overtaken" {
		t.Fatalf("the old claimant: %+v", got)
	}
	// the same when the newer summary has not landed yet: the entry is superseded, so it is not writable whatever the document holds
	lin2 := newFakeLinear()
	flow2 := newParentFlow(f, lin2, "doc-2")
	g1 := flow2.enqueue().Entry
	claimB, _ := flow2.claimNewest()
	f.bump(2)
	flow2.enqueue()
	rec, err = f.sched.ReconcileSummary(ctx, g1.SummaryID, lin2.read())
	if err != nil || rec.Writable || rec.Repair != "none" || rec.Outcome != "absent" {
		t.Fatalf("an overtaken entry over an empty document: %+v %v", rec, err)
	}
	if got := flow2.proceed(claimB); got.Step != "overtaken" || lin2.calls[1] != 0 || lin2.read() != "" {
		t.Fatalf("an overtaken entry wrote: %+v %v %q", got, lin2.calls, lin2.read())
	}
	if lin.overwrites != 0 || lin.calls[1] != 0 || seqIn(lin.read()) != 2 {
		t.Fatalf("overwrites %d, calls %v, the document carries %d", lin.overwrites, lin.calls, seqIn(lin.read()))
	}
}

// Two claimants that both read a document with no container must not make two: the container is created by a conditional replacement of the whole document, so the second is refused whole
// and reads again. Nothing in the protocol is an append.
func TestSummaryTwoClaimantsThatBothFindNoContainerMakeOne(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	lin.doc = "# Coordination\n\nSome text.\n"
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	e1 := flow.enqueue().Entry
	claimA, err := f.sched.ClaimSummary(ctx, e1.SummaryID, "parent")
	if err != nil {
		t.Fatal(err)
	}
	readA := lin.read()
	claimB, err := f.sched.ClaimSummary(ctx, e1.SummaryID, "parent") // a second session of the parent: the first token is dead
	if err != nil || claimB.Token == claimA.Token {
		t.Fatalf("second claim %+v %v", claimB, err)
	}
	readB := lin.read()
	for _, read := range []string{readA, readB} {
		rec, err := f.sched.ReconcileSummary(ctx, e1.SummaryID, read)
		if err != nil || rec.Outcome != "absent" || rec.Repair != "initialize" || rec.Container != "absent" {
			t.Fatalf("a document with no container: %+v %v", rec, err)
		}
	}
	op := claimB.Operation
	if err := lin.initialize(readA, op.EmptyContainer); err != nil {
		t.Fatalf("the first creation: %v", err)
	}
	if err := lin.initialize(readB, op.EmptyContainer); err != errLinearNoMatch {
		t.Fatalf("the second creation was not refused: %v", err)
	}
	if n := strings.Count(lin.read(), op.ContainerStart); n != 1 {
		t.Fatalf("the document holds %d containers", n)
	}
	// the live claimant carries on from a fresh read and finishes
	if got := flow.proceed(claimB); got.Step != "wrote" || got.Entry.State != SummaryConfirmed || lin.overwrites != 0 {
		t.Fatalf("%+v overwrites %d", got, lin.overwrites)
	}
	if strings.Count(lin.read(), op.ContainerStart) != 1 || !strings.HasPrefix(lin.read(), "# Coordination\n\nSome text.\n") {
		t.Fatalf("the document: %q", lin.read())
	}
}

// What one replacement of the container can repair, and what it cannot: a document with the container once and surplus inside it (a block twice, other text) is repaired and confirmed through
// the protocol; markers that appear twice, a container or block that is not closed and a block outside its container are not, so the relay says manual, writes nothing, and the failure is
// recorded for a person.
func TestSummaryRepairsWhatOneReplacementCanAndSendsTheRestToAPerson(t *testing.T) {
	cases := []struct {
		name   string
		build  func(op SummaryOperation) string
		repair string
		step   string
	}{
		{"a block twice inside the one container", func(op SummaryOperation) string {
			return "page\n" + strings.Replace(op.Container, op.ContainerEnd, op.Block+"\n"+op.ContainerEnd, 1) + "\n"
		}, "replace_container", "wrote"},
		{"other text inside the container", func(op SummaryOperation) string {
			return "page\n" + strings.Replace(op.Container, op.ContainerEnd, "a person's note\n"+op.ContainerEnd, 1) + "\n"
		}, "replace_container", "wrote"},
		{"an empty container", func(op SummaryOperation) string { return "page\n" + op.EmptyContainer + "\n" }, "replace_container", "wrote"},
		{"no container", func(op SummaryOperation) string { return "page\n" }, "initialize", "wrote"},
		{"two containers", func(op SummaryOperation) string { return "page\n" + op.Container + "\n" + op.Container + "\n" }, "manual", "manual"},
		{"two empty containers", func(op SummaryOperation) string {
			return "page\n" + op.EmptyContainer + "\n" + op.EmptyContainer + "\n"
		}, "manual", "manual"},
		{"a block outside the container", func(op SummaryOperation) string { return "page\n" + op.EmptyContainer + "\n" + op.Block + "\n" }, "manual", "manual"},
		{"a block that is not closed", func(op SummaryOperation) string {
			return "page\n" + strings.Replace(op.Container, strings.Split(op.Block, "\n")[len(strings.Split(op.Block, "\n"))-1], "", 1) + "\n"
		}, "manual", "manual"},
		{"a container that is not closed", func(op SummaryOperation) string { return "page\n" + op.ContainerStart + "\n" }, "manual", "manual"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := summaryFixture(t)
			lin := newFakeLinear()
			flow := newParentFlow(f, lin, "doc-1")
			e1 := flow.enqueue().Entry
			claim, ok := flow.claimNewest()
			if !ok {
				t.Fatal("nothing to claim")
			}
			lin.doc = c.build(claim.Operation)
			before := lin.doc
			rec, err := f.sched.ReconcileSummary(context.Background(), e1.SummaryID, before)
			if err != nil || rec.Repair != c.repair {
				t.Fatalf("repair %q, want %q: %+v %v", rec.Repair, c.repair, rec, err)
			}
			got := flow.proceed(claim)
			if got.Step != c.step {
				t.Fatalf("%+v", got)
			}
			if c.step == "manual" {
				if lin.doc != before || len(lin.calls) != 0 || lin.conflicts != 0 {
					t.Fatalf("a manual repair wrote: %q calls %v", lin.doc, lin.calls)
				}
				if got.Entry.State != SummaryPending || got.Entry.Attempts != 1 || !strings.Contains(got.Entry.LastError, "malformed") {
					t.Fatalf("the failure was not recorded for a person: %+v", got.Entry)
				}
				c2, err := f.sched.ClaimSummary(context.Background(), e1.SummaryID, "parent")
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.sched.CompleteSummary(context.Background(), e1.SummaryID, "parent", c2.Token, "doc-1", before)
				wantRefusal(t, err, contract.RefusalReadbackMismatch)
				return
			}
			if got.Entry.State != SummaryConfirmed || lin.overwrites != 0 || strings.Count(lin.read(), claim.Operation.ContainerStart) != 1 || strings.Count(lin.read(), claim.Operation.Block) != 1 {
				t.Fatalf("not repaired through the protocol: %+v\n%s", got, lin.read())
			}
		})
	}
}
