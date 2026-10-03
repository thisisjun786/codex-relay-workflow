package dagsched

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-283: the project summary outbox. The tests run the real Go store and scheduler over temporary stores and put a double only where the relay has none: the Linear document
// (outbox_linear_test.go), which behaves as the connector the skill names does, and fails when a test says it does.

func wantRefusal(t *testing.T, err error, reason contract.RefusalReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want the refusal %s", reason)
	}
	if got := refusalReason(err); got != string(reason) {
		t.Fatalf("got %v (reason %q), want the refusal %s", err, got, reason)
	}
}

func summaryRows(f *fixture) []SummaryEntry {
	f.t.Helper()
	st, err := f.sched.SummaryStatus(context.Background(), "p1", "", true)
	if err != nil {
		f.t.Fatal(err)
	}
	var all []SummaryEntry
	for _, stream := range st.Streams {
		all = append(all, stream.History...)
	}
	return all
}

func entryBySeq(f *fixture, document string, seq int64) SummaryEntry {
	f.t.Helper()
	for _, e := range summaryRows(f) {
		if e.Document == document && e.Seq == seq {
			return e
		}
	}
	f.t.Fatalf("no entry %s #%d", document, seq)
	return SummaryEntry{}
}

// Criterion c1, the key: an entry is identified by the decision it states (the progress digest), the document, the plan revision and a sequence number that only goes up, and the
// same state of the plan is one entry however often it is asked for.
func TestSummaryEnqueueIsKeyedAndOrdered(t *testing.T) {
	f := summaryFixture(t)
	flow := newParentFlow(f, newFakeLinear(), "doc-1")
	ctx := context.Background()
	progress, err := f.sched.ReadProgress(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	first := flow.enqueue()
	e1 := first.Entry
	if first.Replayed || e1.Seq != 1 || e1.State != SummaryPending || e1.Document != "doc-1" || e1.PlanID != "p1" || e1.ProjectKey != "P-TEST" {
		t.Fatalf("first entry %+v", e1)
	}
	if e1.SubjectDigest != progress.Digest || e1.PlanRevision != progress.Reading.PlanRevision || e1.StateDigest != progress.Reading.StateDigest {
		t.Fatalf("the entry does not state the progress it was made from: %+v vs %s at revision %d", e1, progress.Digest, progress.Reading.PlanRevision)
	}
	if !strings.HasPrefix(e1.SummaryID, "sum-") || e1.EnqueuedBy != "parent" || e1.CoordinatorEpoch != 0 {
		t.Fatalf("identity %+v", e1)
	}
	// the same state of the plan is the same entry
	again := flow.enqueue()
	if !again.Replayed || again.Entry.SummaryID != e1.SummaryID || len(summaryRows(f)) != 1 {
		t.Fatalf("asking twice made a second entry: %+v", again)
	}
	// another state: the next sequence number, a plan revision that did not go back, the older entry superseded
	f.bump(1)
	second := flow.enqueue()
	e2 := second.Entry
	if second.Replayed || e2.Seq != 2 || e2.PlanRevision <= e1.PlanRevision || e2.SubjectDigest == e1.SubjectDigest || e2.SummaryID == e1.SummaryID {
		t.Fatalf("second entry %+v after %+v", e2, e1)
	}
	if !reflect.DeepEqual(second.Superseded, []string{e1.SummaryID}) {
		t.Fatalf("superseded %v", second.Superseded)
	}
	if got := entryBySeq(f, "doc-1", 1); got.State != SummarySuperseded || got.ClaimToken != "" {
		t.Fatalf("the older entry is %+v", got)
	}
	// a document is a stream of its own: it starts again at 1 and supersedes nothing of the other
	other, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-2", false)
	if err != nil || other.Entry.Seq != 1 || len(other.Superseded) != 0 || entryBySeq(f, "doc-1", 2).State != SummaryPending {
		t.Fatalf("second document: %+v %v", other, err)
	}
	if other.Entry.SummaryID == e1.SummaryID {
		t.Fatal("two documents share an entry id")
	}
}

// A summary the plan already states and the parent confirmed is not owed again, unless the parent says the document lost it (--again): then the same state is a new entry, while an
// entry that is still owed is never doubled.
func TestSummaryEnqueueAgainOnlyReassertsAConfirmedSummary(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	flow.enqueue()
	if _, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-1", true); err != nil {
		t.Fatal(err)
	}
	if n := len(summaryRows(f)); n != 1 {
		t.Fatalf("an entry that is still owed was doubled by --again: %d entries", n)
	}
	if got := flow.drain(); got.Step != "wrote" || got.Entry.State != SummaryConfirmed {
		t.Fatalf("drain %+v", got)
	}
	plain, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-1", false)
	if err != nil || !plain.Replayed || len(summaryRows(f)) != 1 {
		t.Fatalf("a confirmed summary was queued again without --again: %+v %v", plain, err)
	}
	redo, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-1", true)
	if err != nil || redo.Replayed || redo.Entry.Seq != 2 || redo.Entry.SubjectDigest != plain.Entry.SubjectDigest {
		t.Fatalf("--again: %+v %v", redo, err)
	}
	if len(redo.Superseded) != 0 || entryBySeq(f, "doc-1", 1).State != SummaryConfirmed {
		t.Fatalf("--again touched the confirmed entry: %+v", redo.Superseded)
	}
}

func TestSummaryEnqueueRefusals(t *testing.T) {
	f := summaryFixture(t)
	ctx := context.Background()
	if _, err := f.sched.EnqueueSummary(ctx, "nope", "parent", "doc-1", false); err == nil {
		t.Fatal("a plan the store does not hold")
	} else {
		wantRefusal(t, err, contract.RefusalUnregisteredScope)
	}
	_, err := f.sched.EnqueueSummary(ctx, "p1", "intruder", "doc-1", false)
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	for name, document := range map[string]string{"empty": "", "padded": " doc-1", "newline": "doc\n1", "tab": "doc\t1", "over long": strings.Repeat("d", 513)} {
		_, err := f.sched.EnqueueSummary(ctx, "p1", "parent", document, false)
		if err == nil {
			t.Fatalf("%s document accepted", name)
		}
		wantRefusal(t, err, contract.RefusalMalformedReceipt)
	}
	if n := f.count("SELECT COUNT(*) FROM dag_summary_outbox"); n != 0 {
		t.Fatalf("a refused enqueue left %d rows", n)
	}
}

func claimRefusal(t *testing.T, f *fixture, id string) error {
	t.Helper()
	_, err := f.sched.ClaimSummary(context.Background(), id, "parent")
	return err
}

// The rules of one entry's life: who may claim, what a token fences, what a failed document does, and what a confirmation makes final.
func TestSummaryClaimCompleteAndFailRules(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	e1 := flow.enqueue().Entry
	_, err := f.sched.ClaimSummary(ctx, e1.SummaryID, "intruder")
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	_, err = f.sched.ClaimSummary(ctx, "sum-unknown", "parent")
	wantRefusal(t, err, contract.RefusalUnregisteredScope)
	// a claim rotates the token: the old one is dead
	c1, err := f.sched.ClaimSummary(ctx, e1.SummaryID, "parent")
	if err != nil || c1.Token == "" || c1.Entry.State != SummaryClaimed {
		t.Fatalf("claim %+v %v", c1, err)
	}
	c2, err := f.sched.ClaimSummary(ctx, e1.SummaryID, "parent")
	if err != nil || c2.Token == c1.Token {
		t.Fatalf("a second claim kept the token: %+v %v", c2, err)
	}
	_, err = f.sched.FailSummary(ctx, e1.SummaryID, "parent", c1.Token, "late")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	_, err = f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", c1.Token, "doc-1", "")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	// the wrong document, and a readback that does not carry the block, are refused and leave the entry claimed
	op := c2.Operation
	lin.initContainer(op.EmptyContainer)
	_, err = f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", c2.Token, "doc-9", lin.read())
	wantRefusal(t, err, contract.RefusalSyncTargetMismatch)
	_, err = f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", c2.Token, "doc-1", lin.read())
	wantRefusal(t, err, contract.RefusalReadbackMismatch)
	if got := entryBySeq(f, "doc-1", 1); got.State != SummaryClaimed || got.LastError == "" || got.Attempts != 0 {
		t.Fatalf("after a readback that carried nothing: %+v", got)
	}
	if err := lin.patch(1, op.EmptyContainer, op.Container); err != nil {
		t.Fatal(err)
	}
	done, err := f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", c2.Token, "doc-1", lin.read())
	if err != nil || done.Replayed || done.Entry.State != SummaryConfirmed || done.Entry.ConfirmedAt == "" || done.Entry.ClaimToken != "" || done.Entry.Readback != op.Block || done.Entry.LastError != "" {
		t.Fatalf("complete %+v %v", done, err)
	}
	// a confirmation is monotonic: completing again answers the record, failing or claiming is refused
	again, err := f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", "any-token", "doc-1", lin.read())
	if err != nil || !again.Replayed || again.Entry.ConfirmedAt != done.Entry.ConfirmedAt {
		t.Fatalf("complete again %+v %v", again, err)
	}
	_, err = f.sched.FailSummary(ctx, e1.SummaryID, "parent", c2.Token, "x")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	wantRefusal(t, claimRefusal(t, f, e1.SummaryID), contract.RefusalSyncNotClaimable)

	// a claim of an entry that is not the newest is refused
	f.bump(1)
	e2 := flow.enqueue().Entry
	wantRefusal(t, claimRefusal(t, f, e1.SummaryID), contract.RefusalSyncNotClaimable)
	f.bump(2)
	e3 := flow.enqueue().Entry
	if got := entryBySeq(f, "doc-1", e2.Seq); got.State != SummarySuperseded {
		t.Fatalf("e2 is %+v", got)
	}
	wantRefusal(t, claimRefusal(t, f, e2.SummaryID), contract.RefusalSyncNotClaimable)

	// a failure goes back to pending with the failure kept; the eighth makes the entry failed, which waits for an explicit retry
	for i := 1; i <= MaxSummaryAttempts; i++ {
		c, err := f.sched.ClaimSummary(ctx, e3.SummaryID, "parent")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		failed, err := f.sched.FailSummary(ctx, e3.SummaryID, "parent", c.Token, fmt.Sprintf("write %d failed", i))
		if err != nil || failed.Attempts != int64(i) || failed.LastError != fmt.Sprintf("write %d failed", i) || failed.ClaimToken != "" {
			t.Fatalf("fail %d: %+v %v", i, failed, err)
		}
		want := SummaryPending
		if i == MaxSummaryAttempts {
			want = SummaryFailed
		}
		if failed.State != want {
			t.Fatalf("after %d failures the entry is %s, want %s", i, failed.State, want)
		}
	}
	wantRefusal(t, claimRefusal(t, f, e3.SummaryID), contract.RefusalSyncNotClaimable)
	retried, err := f.sched.RetrySummary(ctx, e3.SummaryID, "parent")
	if err != nil || retried.Replayed || retried.Entry.State != SummaryPending || retried.Entry.Attempts != 0 {
		t.Fatalf("retry %+v %v", retried, err)
	}
	if again, err := f.sched.RetrySummary(ctx, e3.SummaryID, "parent"); err != nil || !again.Replayed {
		t.Fatalf("a retry of an entry that is not failed answers the record: %+v %v", again, err)
	}
	_, err = f.sched.RetrySummary(ctx, e1.SummaryID, "parent")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	_, err = f.sched.RetrySummary(ctx, e2.SummaryID, "parent")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	if _, err := f.sched.FailSummary(ctx, e3.SummaryID, "parent", "", "x"); err == nil {
		t.Fatal("a failure with no claim")
	}
	if _, err := f.sched.ClaimSummary(ctx, e3.SummaryID, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sched.FailSummary(ctx, e3.SummaryID, "parent", "wrong", "x"); err == nil {
		t.Fatal("a token that was not issued")
	}
}

// Criterion c2: only the failed entries are retried, and an entry is retried alone. Two documents each have an entry that failed eight times (so each is failed); the real retry of
// one leaves every field of the other exactly as it was.
func TestSummaryRetryTouchesOnlyTheEntryItNames(t *testing.T) {
	f := summaryFixture(t)
	ctx := context.Background()
	var ids []string
	for _, document := range []string{"doc-1", "doc-2"} {
		e, err := f.sched.EnqueueSummary(ctx, "p1", "parent", document, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.Entry.SummaryID)
		for i := 0; i < MaxSummaryAttempts; i++ {
			c, err := f.sched.ClaimSummary(ctx, e.Entry.SummaryID, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.sched.FailSummary(ctx, e.Entry.SummaryID, "parent", c.Token, "down"); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := summaryRows(f)
	for _, e := range before {
		if e.State != SummaryFailed {
			t.Fatalf("%+v is not failed", e)
		}
	}
	if _, err := f.sched.RetrySummary(ctx, ids[0], "parent"); err != nil {
		t.Fatal(err)
	}
	after := summaryRows(f)
	for i, e := range after {
		switch e.SummaryID {
		case ids[0]:
			if e.State != SummaryPending || e.Attempts != 0 {
				t.Fatalf("the retried entry is %+v", e)
			}
		default:
			if !reflect.DeepEqual(e, before[i]) {
				t.Fatalf("a retry of another entry changed %+v to %+v", before[i], e)
			}
		}
	}
}

type loopCounts struct{ confirmed, superseded []int64 }

// Criterion c2: with entries that are overtaken before they are written, writes that fail, a write whose response is lost, a stale claimant that comes back late and a write
// prepared before a newer summary landed, a summary older than one already in the document is never put there, and only the entries whose writes failed are written again.
func TestSummaryReorderedEntriesAndWriteFailuresNeverOverwriteANewerSummary(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	lin.failFirst[2] = 2 // entry 2: the write fails twice before it lands
	lin.loseFirst[3] = 1 // entry 3: the write lands and the response is lost
	// entry 1 is claimed and its write prepared (the parent read the document), and then it is overtaken by entry 2
	e1 := flow.enqueue().Entry
	stale := flow.prepare(e1.SummaryID)
	f.bump(1)
	e2 := flow.enqueue().Entry
	if got := entryBySeq(f, "doc-1", 1); got.State != SummarySuperseded {
		t.Fatalf("entry 1 is %+v", got)
	}
	// the stale claimant comes back: every move of it is refused by the relay
	wantRefusal(t, claimRefusal(t, f, e1.SummaryID), contract.RefusalSyncNotClaimable)
	_, err := f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", stale.claim.Token, "doc-1", lin.read())
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)
	_, err = f.sched.FailSummary(ctx, e1.SummaryID, "parent", stale.claim.Token, "late")
	wantRefusal(t, err, contract.RefusalSyncNotClaimable)

	// entry 2 fails twice and lands the third time: the failed entry, and only it, is written again
	for i, want := range []string{"failed", "failed", "wrote"} {
		if got := flow.drain(); got.Step != want || got.Entry.SummaryID != e2.SummaryID {
			t.Fatalf("drain %d of entry 2: %+v", i+1, got)
		}
	}
	if lin.calls[2] != 3 || lin.landed[2] != 1 {
		t.Fatalf("entry 2: %d write calls, %d landed", lin.calls[2], lin.landed[2])
	}
	// the write prepared for entry 1 now reaches Linear: its replacement no longer matches the container, so Linear refuses it whole
	if err := stale.send(lin); err == nil {
		t.Fatal("a write prepared before a newer summary landed was applied")
	}

	// entry 3: the write lands and the response is lost; the next turn finds the block in the document and confirms without writing
	f.bump(2)
	e3 := flow.enqueue().Entry
	if got := flow.drain(); got.Step != "failed" || got.Entry.SummaryID != e3.SummaryID || got.Entry.Attempts != 1 {
		t.Fatalf("entry 3, first turn: %+v", got)
	}
	if got := flow.drain(); got.Step != "reconciled" || got.Entry.State != SummaryConfirmed {
		t.Fatalf("entry 3, second turn: %+v", got)
	}
	if lin.calls[3] != 1 || lin.landed[3] != 1 {
		t.Fatalf("entry 3: %d write calls, %d landed", lin.calls[3], lin.landed[3])
	}

	// entry 4 is written at once; entries 5 and 6 are queued before the parent comes back, so entry 5 is overtaken and never written
	f.bump(3)
	flow.enqueue()
	if got := flow.drain(); got.Step != "wrote" {
		t.Fatalf("entry 4: %+v", got)
	}
	f.bump(4)
	e5 := flow.enqueue().Entry
	f.bump(5)
	flow.enqueue()
	if got := flow.drain(); got.Step != "wrote" || got.Entry.Seq != 6 {
		t.Fatalf("entry 6: %+v", got)
	}
	wantRefusal(t, claimRefusal(t, f, e5.SummaryID), contract.RefusalSyncNotClaimable)

	if lin.overwrites != 0 {
		t.Fatalf("an older summary overwrote a newer one %d times", lin.overwrites)
	}
	if got := seqIn(lin.read()); got != 6 {
		t.Fatalf("the document carries summary %d, want the newest, 6", got)
	}
	// every entry was written exactly as many times as it failed, plus once: entries nobody failed were never written again
	wantCalls := map[int64]int{1: 1 /* the stale prepared write, refused by Linear */, 2: 3, 3: 1, 4: 1, 5: 0, 6: 1}
	if !reflect.DeepEqual(lin.calls, withoutZero(wantCalls)) {
		t.Fatalf("write calls per entry %v, want %v", lin.calls, withoutZero(wantCalls))
	}
	if !reflect.DeepEqual(lin.landed, map[int64]int{2: 1, 3: 1, 4: 1, 6: 1}) {
		t.Fatalf("writes that landed %v", lin.landed)
	}
	var got loopCounts
	for _, e := range summaryRows(f) {
		switch e.State {
		case SummaryConfirmed:
			got.confirmed = append(got.confirmed, e.Seq)
		case SummarySuperseded:
			got.superseded = append(got.superseded, e.Seq)
		default:
			t.Fatalf("entry %d is %s", e.Seq, e.State)
		}
	}
	if !reflect.DeepEqual(got, loopCounts{confirmed: []int64{2, 3, 4, 6}, superseded: []int64{1, 5}}) {
		t.Fatalf("final states %+v", got)
	}
	if lin.conflicts != 1 {
		t.Fatalf("Linear refused %d writes, want the one stale write", lin.conflicts)
	}
}

func withoutZero(m map[int64]int) map[int64]int {
	out := map[int64]int{}
	for k, v := range m {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

// Criterion c2, a concurrent edit: a person changes the container between the parent's read and its write. The replacement no longer matches, Linear refuses it, and the next turn retries
// that entry (and no other) against the document as it is then.
func TestSummaryConcurrentEditIsRecoveredByRetryingOnlyThatEntry(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	e1 := flow.enqueue().Entry
	if got := flow.drain(); got.Step != "wrote" {
		t.Fatalf("%+v", got)
	}
	f.bump(1)
	e2 := flow.enqueue().Entry
	confirmed := entryBySeq(f, "doc-1", e1.Seq)
	lin.beforePatch = func() {
		// the person edits the block that is in the document, inside the container
		lin.humanEdit("a node", "a person's note")
		lin.humanEdit("seq: 1", "seq: 1\nedited: yes")
	}
	first := flow.drain()
	if first.Step != "failed" || first.Err != errLinearNoMatch || first.Entry.SummaryID != e2.SummaryID || first.Entry.Attempts != 1 {
		t.Fatalf("the turn the edit interrupted: %+v", first)
	}
	if got := entryBySeq(f, "doc-1", 1); !reflect.DeepEqual(got, confirmed) {
		t.Fatalf("the confirmed entry changed: %+v, was %+v", got, confirmed)
	}
	second := flow.drain()
	if second.Step != "wrote" || second.Entry.SummaryID != e2.SummaryID || second.Entry.State != SummaryConfirmed {
		t.Fatalf("the retry: %+v", second)
	}
	if lin.calls[1] != 1 || lin.calls[2] != 2 || lin.landed[2] != 1 || lin.overwrites != 0 || seqIn(lin.read()) != 2 {
		t.Fatalf("calls %v landed %v overwrites %d", lin.calls, lin.landed, lin.overwrites)
	}
}

// A parent that is replaced between its claim and its write: the old parent's relay calls are refused (it is no longer the project's parent), the write it had prepared is refused by
// Linear once the new parent's summary has landed, and the new parent claims the entry again and finishes it.
func TestSummaryReplacedParentIsFencedAtBothEnds(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	e1 := flow.enqueue().Entry
	old := flow.prepare(e1.SummaryID)
	// the project's parent is replaced
	if err := storeseed.ArchiveScopeBinding(ctx, f.s, "bind-parent", "archived", "bind-parent-2", f.clock()); err != nil {
		t.Fatal(err)
	}
	now := f.clock()
	if err := storeseed.InsertScopeBinding(ctx, f.s, store.ScopeBindingsRow{BindingID: "bind-parent-2", Role: "parent", ScopeKind: "project", ScopeKey: "P-TEST", TaskID: "parent-2", HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, err := f.sched.FailSummary(ctx, e1.SummaryID, "parent", old.claim.Token, "x")
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	_, err = f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", old.claim.Token, "doc-1", lin.read())
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	_, err = f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-1", false)
	wantRefusal(t, err, contract.RefusalScopeRoleMismatch)
	flow.actor = "parent-2"
	if got := flow.drain(); got.Step != "wrote" || got.Entry.SummaryID != e1.SummaryID {
		t.Fatalf("the new parent: %+v", got)
	}
	// the old parent's write, prepared before, is refused by Linear: the container is not the text it read
	if err := old.send(lin); err == nil || lin.overwrites != 0 {
		t.Fatalf("the replaced parent's prepared write: %v, overwrites %d", err, lin.overwrites)
	}
}

// The limit of the guarantee, shown: a writer that does not follow the protocol (a whole-document save, not a conditioned replacement) CAN put an older summary over a newer one, and the
// counter sees it. The relay then reports it (reconcile) and the parent re-asserts the summary with enqueue --again: the document ends with the newest summary.
func TestSummaryAWriterOutsideTheProtocolIsDetectedAndRepaired(t *testing.T) {
	f := summaryFixture(t)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	ctx := context.Background()
	e1 := flow.enqueue().Entry
	flow.drain()
	older := lin.read() // the document as it was with summary 1
	f.bump(1)
	e2 := flow.enqueue().Entry
	flow.drain()
	if lin.overwrites != 0 || seqIn(lin.read()) != 2 {
		t.Fatalf("the protocol overwrote: %d, %d", lin.overwrites, seqIn(lin.read()))
	}
	lin.overwrite(older)
	if lin.overwrites != 1 {
		t.Fatalf("the control did not count the stale overwrite: %d", lin.overwrites)
	}
	rec, err := f.sched.ReconcileSummary(ctx, e2.SummaryID, lin.read())
	if err != nil || rec.Outcome != "stale" || rec.Relation != "older" || rec.DocumentSeq != 1 || rec.DocumentSummaryID != e1.SummaryID || rec.State != SummaryConfirmed || rec.Writable || !rec.Again {
		t.Fatalf("reconcile of the newest confirmed entry: %+v %v", rec, err)
	}
	redo, err := f.sched.EnqueueSummary(ctx, "p1", "parent", "doc-1", true)
	if err != nil || redo.Replayed || redo.Entry.Seq != 3 {
		t.Fatalf("enqueue --again: %+v %v", redo, err)
	}
	if got := flow.drain(); got.Step != "wrote" || got.Entry.Seq != 3 {
		t.Fatalf("the repair: %+v", got)
	}
	if seqIn(lin.read()) != 3 || lin.overwrites != 1 {
		t.Fatalf("after the repair the document carries %d, overwrites %d", seqIn(lin.read()), lin.overwrites)
	}
}

// Criterion c3, in process: a lost response, a failure, a stale claimant and the retry change the summary table and nothing else: every other table of the store, the journal included,
// is the same afterwards. The store holds a running child, an accepted result and a delivery, so the tables the criterion names are not empty.
func TestSummaryRecoveryTouchesNothingButTheSummary(t *testing.T) {
	f := summaryFixture(t)
	seedExecutionLedger(f)
	lin := newFakeLinear()
	flow := newParentFlow(f, lin, "doc-1")
	lin.loseFirst[1] = 1
	ctx := context.Background()
	check := func(what string, run func()) {
		t.Helper()
		before := dumpStore(t, f.path)
		run()
		after := dumpStore(t, f.path)
		for key := range after {
			if key != "rows dag_summary_outbox" && after[key] != before[key] {
				t.Errorf("%s changed %s", what, key)
			}
		}
		for key := range before {
			if _, ok := after[key]; !ok {
				t.Errorf("%s removed %s", what, key)
			}
		}
	}
	var e1 SummaryEntry
	check("enqueue", func() { e1 = flow.enqueue().Entry })
	var claim SummaryClaim
	check("claim", func() {
		var err error
		if claim, err = f.sched.ClaimSummary(ctx, e1.SummaryID, "parent"); err != nil {
			t.Fatal(err)
		}
	})
	lin.initContainer(claim.Operation.EmptyContainer)
	// the write lands and its response is lost: the parent can only record a failure
	var lost error
	check("write", func() {
		lost = lin.patch(1, claim.Operation.EmptyContainer, claim.Operation.Container)
	})
	if lost != errLinearLost {
		t.Fatalf("the injected loss: %v", lost)
	}
	check("fail", func() {
		if _, err := f.sched.FailSummary(ctx, e1.SummaryID, "parent", claim.Token, lost.Error()); err != nil {
			t.Fatal(err)
		}
	})
	check("retry turn", func() {
		if got := flow.drain(); got.Step != "reconciled" || got.Entry.State != SummaryConfirmed {
			t.Fatalf("%+v", got)
		}
	})
	check("replayed confirmation", func() {
		if _, err := f.sched.CompleteSummary(ctx, e1.SummaryID, "parent", "stale", "doc-1", lin.read()); err != nil {
			t.Fatal(err)
		}
	})
	check("status and reconcile", func() {
		if _, err := f.sched.SummaryStatus(ctx, "p1", "", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.sched.ReconcileSummary(ctx, e1.SummaryID, lin.read()); err != nil {
			t.Fatal(err)
		}
	})
	if lin.landed[1] != 1 || lin.calls[1] != 1 {
		t.Fatalf("the lost response was written again: %v %v", lin.calls, lin.landed)
	}
}

// seedExecutionLedger gives the store what the execution ledger is made of: a running child with its generation, an accepted result with its report, and a delivery.
func seedExecutionLedger(f *fixture) {
	f.t.Helper()
	f.startNode("p1", "design")
	f.acceptNode("p1", "research", acceptOpts{})
	f.exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES ('evt-delivery-1', 'rel-p1-research', 'completion', 'parent', 'thread-parent', 'delivered', 1, '2026-10-02T00:00:00Z', '2026-10-02T00:00:00Z')")
	for table, min := range map[string]int{"relationships": 2, "generations": 2, "events": 1, "deliveries": 1, "dag_node_executions": 2, "dag_acceptances": 1} {
		if n := f.count("SELECT COUNT(*) FROM " + table); n < min {
			f.t.Fatalf("%s holds %d rows, the ledger needs %d", table, n, min)
		}
	}
}
