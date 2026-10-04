package delivery

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-470: a receipt the relay suppressed (the turn that staged it ended failed or interrupted)
// is not a revision, so a child that restarted and named it with --supersedes-revision named
// nothing the head could find, and the whole generation read unknown_predecessor for good. The
// head now reads such a naming through the suppressed receipt, to what that receipt itself
// replaced. These tests state what the head reads for each shape of it, and what emit does.

const suppressedBecause = "the turn ended interrupted, so the staged claim is not promoted"

// spWorld is a relationship with one generation in which receipts are emitted and suppressed.
type spWorld struct {
	*fixture
	hash map[string]string
}

func newSPWorld(t *testing.T) *spWorld {
	t.Helper()
	f := newFixture(t, "")
	f.register(regOpts{})
	return &spWorld{fixture: f, hash: map[string]string{}}
}

// emit stores one reviewable receipt of the generation, over the file holding text. A reviewable
// event is named by its revision, so one revision is one event whatever turn or attempt brings it
// again. supersedes is the revision it names ("" for none); status is the turn status the receipt
// carried (inProgress stores it staged); a turn that is not the generation's anchor is admitted
// by the continuation claim the child's emit carries. The revision hash is kept under name.
func (w *spWorld) emit(name string, generation int64, turn, status, file, text string, supersedes string) string {
	w.t.Helper()
	payload := w.readyPayload(w.rid, generation, []string{w.artifact(file, text)}, 1, turnRef{child, turn, status})
	w.hash[name] = pyjson.Text(payload.Get("revisionHash"))
	return w.store1(payload, turn, generation, supersedes).EventID
}

func (w *spWorld) store1(payload Obj, turn string, generation int64, supersedes string) store.StoredReceipt {
	w.t.Helper()
	options := store.AcceptOptions{}
	if supersedes != "" {
		options.SupersedesRevision = &supersedes
	}
	if anchor := w.one("SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", w.rid, generation).S("dispatch_turn_id"); turn != anchor {
		options.Continuation = []byte(dumps(Obj{{Key: "anchorTurnId", Value: anchor}, {Key: "actor", Value: child}, {Key: "reason", Value: "the task restarted"}}))
	}
	stored, err := w.accept(payload, options)
	mustDo(w.t, err)
	return stored
}

// suppress is what the daemon writes when it observes the end of the turn a claim was staged in.
func (w *spWorld) suppress(event string) {
	w.t.Helper()
	_, err := execSQL(w.ctx, w.store, "UPDATE events SET stage = 'suppressed', suppressed_reason = ? WHERE event_id = ?", suppressedBecause, event)
	mustDo(w.t, err)
}

func (w *spWorld) openGeneration(turn string) {
	w.t.Helper()
	mustDo(w.t, w.store.Transaction(w.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, w.store, w.clock, w.rid, "newer-execution", "needs_changes_revision", turn)
		return err
	}))
}

// reads checks the head of the generation: the evidence, and the event it stands on ("" for none).
// The assignment view reads the head with its own copy of the judgment (registry.HeadRevision), so
// it is held to the same answer.
func (w *spWorld) reads(generation int64, evidence, event string) Obj {
	w.t.Helper()
	head, err := HeadRevision(w.ctx, w.store, w.rid, generation)
	mustDo(w.t, err)
	if got := pyjson.Text(head.Get("evidence")); got != evidence {
		w.t.Fatalf("the head reads %q, want %q: %s", got, evidence, dumps(head))
	}
	got, _ := head.Lookup("eventId")
	switch {
	case event == "" && got != nil:
		w.t.Fatalf("the generation has no head, it reads %v: %s", got, dumps(head))
	case event != "" && got != event:
		w.t.Fatalf("the head is %v, want %s: %s", got, event, dumps(head))
	}
	view, err := registry.HeadRevision(w.ctx, w.store, w.rid, generation)
	mustDo(w.t, err)
	if view.EventID != event || view.Evidence != evidence || view.Detail != pyjson.Text(head.Get("detail")) {
		w.t.Fatalf("the assignment view reads %+v, the delivery side %s", view, dumps(head))
	}
	return head
}

// c1: the case that stopped a coordinator. The receipt the child emitted before the restart is
// suppressed; the one it emits after names it.
func TestAReEmitNamingItsSuppressedPredecessorIsTheHead(t *testing.T) {
	t.Parallel()
	w := newSPWorld(t)
	first := w.emit("first", 1, dispatchTurn, "inProgress", "out.txt", "before the restart", "")
	w.suppress(first)
	second := w.emit("second", 1, "turn-after-restart", "inProgress", "out.txt", "after the restart", w.hash["first"])
	w.reads(1, Sole, second)
	if reason, err := w.delivery.SupersessionReason(w.ctx, second); err != nil || reason != "" {
		t.Fatalf("the re-emit is current: %q %v", reason, err)
	}
}

// The child names the receipt while it still counts (staged, in a turn nobody has seen end); the
// daemon suppresses it afterwards. No emit-time check can see that coming, so the reading cannot
// depend on when the suppression happened.
func TestANamedPredecessorSuppressedAfterTheNamingReadsTheSame(t *testing.T) {
	t.Parallel()
	w := newSPWorld(t)
	first := w.emit("first", 1, dispatchTurn, "inProgress", "out.txt", "before the restart", "")
	second := w.emit("second", 1, "turn-after-restart", "inProgress", "out.txt", "after the restart", w.hash["first"])
	w.reads(1, Chain, second)
	w.suppress(first)
	w.reads(1, Sole, second)
}

// A suppressed receipt that replaced a revision which still counts stays transparent: naming it is
// naming what it replaced, so the correction lands on the revision the child meant.
func TestNamingASuppressedReceiptReplacesWhatItReplaced(t *testing.T) {
	t.Parallel()
	w := newSPWorld(t)
	live := w.emit("live", 1, dispatchTurn, "completed", "live.txt", "the delivered revision", "")
	cut := w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off by the restart", w.hash["live"])
	w.suppress(cut)
	third := w.emit("third", 1, "turn-after-restart", "completed", "third.txt", "after the restart", w.hash["cut"])
	w.reads(1, Chain, third)
	if reason, err := w.delivery.SupersessionReason(w.ctx, live); err != nil || reason != SupersededRevision {
		t.Fatalf("the revision it replaced is superseded by the re-emit: %q %v", reason, err)
	}
	t.Run("through two suppressed receipts in a row", func(t *testing.T) {
		w := newSPWorld(t)
		w.emit("live", 1, dispatchTurn, "completed", "live.txt", "the delivered revision", "")
		a := w.emit("a", 1, "turn-a", "inProgress", "a.txt", "first cut", w.hash["live"])
		b := w.emit("b", 1, "turn-b", "inProgress", "b.txt", "second cut", w.hash["a"])
		w.suppress(a)
		w.suppress(b)
		last := w.emit("last", 1, "turn-after-restart", "completed", "last.txt", "after the restart", w.hash["b"])
		w.reads(1, Chain, last)
	})
}

// c2: what stays ambiguous.
func TestANamingNoSuppressedReceiptCanResolveStaysAmbiguous(t *testing.T) {
	t.Parallel()
	t.Run("two suppressed receipts, each named by a re-emit", func(t *testing.T) {
		w := newSPWorld(t)
		a := w.emit("a", 1, "turn-a", "inProgress", "a.txt", "first cut", "")
		b := w.emit("b", 1, "turn-b", "inProgress", "b.txt", "second cut", "")
		w.suppress(a)
		w.suppress(b)
		w.emit("afterA", 1, "turn-after-restart", "completed", "afterA.txt", "after a", w.hash["a"])
		w.emit("afterB", 1, "turn-after-restart", "completed", "afterB.txt", "after b", w.hash["b"])
		w.reads(1, Fork, "")
	})
	t.Run("two suppressed receipts hold the named revision", func(t *testing.T) {
		// The store never writes this (a reviewable event is named by its revision, so one revision
		// is one event); the case is the guard of the rule that a hash read through is held by one.
		w := newSPWorld(t)
		cut := w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", "")
		w.suppress(cut)
		_, err := execSQL(w.ctx, w.store, "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at) SELECT 'second-holder', relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at FROM events WHERE event_id = ?", cut)
		mustDo(t, err)
		w.emit("after", 1, "turn-after-restart", "completed", "after.txt", "after the restart", w.hash["cut"])
		w.reads(1, UnknownPredecessor, "")
	})
	t.Run("the suppressed receipt replaced the re-emit itself", func(t *testing.T) {
		w := newSPWorld(t)
		after := w.readyPayload(w.rid, 1, []string{w.artifact("after.txt", "after the restart")}, 1, turnRef{child, "turn-after-restart", "completed"})
		afterHash := pyjson.Text(after.Get("revisionHash"))
		w.suppress(w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", afterHash))
		w.store1(after, "turn-after-restart", 1, w.hash["cut"])
		w.reads(1, Cycle, "")
	})
	t.Run("two re-emits name the same suppressed receipt", func(t *testing.T) {
		w := newSPWorld(t)
		w.suppress(w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", ""))
		w.emit("one", 1, "turn-after-restart", "completed", "one.txt", "one", w.hash["cut"])
		w.emit("two", 1, "turn-after-restart", "completed", "two.txt", "two", w.hash["cut"])
		w.reads(1, Fork, "")
	})
	t.Run("the named revision is a suppressed receipt of another generation", func(t *testing.T) {
		w := newSPWorld(t)
		old := w.emit("old", 1, dispatchTurn, "inProgress", "out.txt", "the first generation", "")
		w.suppress(old)
		w.openGeneration("newer-turn")
		w.emit("new", 2, "newer-turn", "inProgress", "new.txt", "the second generation", w.hash["old"])
		w.reads(2, UnknownPredecessor, "")
	})
	t.Run("the named revision was never emitted", func(t *testing.T) {
		w := newSPWorld(t)
		w.suppress(w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", ""))
		w.emit("after", 1, "turn-after-restart", "inProgress", "after.txt", "after the restart", strings.Repeat("f", 64))
		w.reads(1, UnknownPredecessor, "")
	})
	t.Run("the suppressed receipt itself named a revision nobody holds", func(t *testing.T) {
		w := newSPWorld(t)
		cut := w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", strings.Repeat("e", 64))
		w.suppress(cut)
		w.emit("after", 1, "turn-after-restart", "inProgress", "after.txt", "after the restart", w.hash["cut"])
		head := w.reads(1, UnknownPredecessor, "")
		if detail := pyjson.Text(head.Get("detail")); !strings.Contains(detail, w.hash["cut"]) {
			t.Fatalf("the detail names what the child named, not what was read through it: %s", detail)
		}
	})
	t.Run("a suppressed receipt that replaced nothing does not join a live revision", func(t *testing.T) {
		w := newSPWorld(t)
		w.emit("live", 1, dispatchTurn, "completed", "live.txt", "the delivered revision", "")
		w.suppress(w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", ""))
		w.emit("after", 1, "turn-after-restart", "completed", "after.txt", "after the restart", w.hash["cut"])
		w.reads(1, Fork, "")
	})
}

// The same bytes again are the same revision, hence the same event: the receipt stays suppressed
// and nothing counts, so a child whose artifact did not change has nothing to emit again.
func TestAReEmitOfTheSameBytesIsADuplicateOfTheSuppressedReceipt(t *testing.T) {
	t.Parallel()
	w := newSPWorld(t)
	first := w.emit("first", 1, dispatchTurn, "inProgress", "out.txt", "before the restart", "")
	w.suppress(first)
	payload := w.readyPayload(w.rid, 1, []string{w.artifact("out.txt", "before the restart")}, 1, turnRef{child, "turn-after-restart", "inProgress"})
	again := w.store1(payload, "turn-after-restart", 1, "")
	if again.EventID != first || !again.Duplicate || again.Stage != store.StageSuppressed {
		t.Fatalf("the same revision is on record already, suppressed: %+v", again)
	}
	w.reads(1, NoRevision, "")
}

// c3: emit. A naming of a suppressed receipt is accepted and recorded as the child stated it; the
// head reads it as above. The command is run as the child runs it after a restart, in a turn
// admitted by a continuation claim, and the head is read back with revision-head.
func TestEmitAcceptsARevisionNamingASuppressedReceipt(t *testing.T) {
	w := newSPWorld(t)
	previous := CommandClock
	CommandClock = w.clock
	t.Cleanup(func() { CommandClock = previous })
	run := func(command string, flags ...string) Obj {
		t.Helper()
		registered, ok := dispatch.Lookup(command)
		if !ok {
			t.Fatalf("%s is not registered", command)
		}
		parsed := argparse.Parse(command, flags)
		if parsed.Message != "" {
			t.Fatalf("%s %v: %s", command, flags, parsed.Message)
		}
		out, err := registered.Run(w.ctx, dispatch.Services{Selection: store.StateSelection{Path: filepath.Join(w.tree, "gostate"), Source: "flag"}}, dispatch.Args{Parsed: parsed, Defaults: registered.Defaults})
		mustDo(t, err)
		return out.(Obj)
	}
	emit := func(turn, text, supersedes string) Obj {
		t.Helper()
		mustDo(t, os.WriteFile(filepath.Join(w.root, "out.txt"), []byte(text), 0o644))
		flags := []string{"--relationship", w.rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", turn, "--artifact", filepath.Join(w.root, "out.txt")}
		if turn != dispatchTurn {
			flags = append(flags, "--continues-anchor", dispatchTurn, "--continuation-actor", child, "--continuation-reason", "the task restarted")
		}
		if supersedes != "" {
			flags = append(flags, "--supersedes-revision", supersedes)
		}
		return run("emit", flags...)
	}
	first := emit(dispatchTurn, "before the restart", "")
	firstEvent := pyjson.Text(sub(first, "receipt").Get("eventId"))
	firstHash := pyjson.Text(sub(first, "receipt").Get("revisionHash"))
	if field(first, "stage") != "staged" {
		t.Fatalf("a receipt emitted with no host to observe the turn is staged: %s", dumps(first))
	}
	w.suppress(firstEvent)

	// The same bytes again: on record already, still suppressed, and the answer says so.
	same := emit("turn-after-restart", "before the restart", "")
	if field(same, "stage") != "suppressed" || field(same, "duplicate") != true {
		t.Fatalf("the same revision again is a duplicate of the suppressed receipt: %s", dumps(same))
	}

	second := emit("turn-after-restart", "after the restart", firstHash)
	if field(second, "stage") != "staged" || field(second, "duplicate") != false {
		t.Fatalf("emit refused or replayed a revision naming a suppressed receipt: %s", dumps(second))
	}
	secondEvent := pyjson.Text(sub(second, "receipt").Get("eventId"))
	lineage := w.one("SELECT supersedes_hash, declared_by FROM revision_lineage WHERE event_id = ?", secondEvent)
	if lineage.S("supersedes_hash") != firstHash || lineage.S("declared_by") != "child_declared" {
		t.Fatalf("the naming is recorded as the child stated it: %v", lineage)
	}
	head := run("revision-head", "--relationship", w.rid)
	if read := sub(head, "head"); field(read, "eventId") != secondEvent || field(read, "evidence") != Sole {
		t.Fatalf("revision-head after the re-emit: %s", dumps(head))
	}
}

// The way through suppressed receipts may end at the predecessor a needs_changes ruling requested:
// the correction's first receipt was cut off after naming it, and the re-emit names that receipt.
func TestReadingThroughASuppressedReceiptMayEndAtTheRequestedPredecessor(t *testing.T) {
	t.Parallel()
	g := newGraphStore(t)
	c := g.seed(0, graphCase{current: 2, requested: true, events: []graphEvent{
		{id: "cut", hash: "hcut", declared: "hp", generation: 2, suppressed: true},
		{id: "again", hash: "hagain", declared: "hcut", generation: 2},
	}})
	head, err := HeadRevisionFrom(g.ctx, g.store.Q(g.ctx), c.rid, 2)
	mustDo(t, err)
	if got, _ := head.Lookup("eventId"); got != c.prefix+"again" || pyjson.Text(head.Get("evidence")) != Chain {
		t.Fatalf("the re-emit stands on the requested predecessor: %s", dumps(head))
	}
	view, err := registry.HeadRevision(g.ctx, g.store, c.rid, 2)
	mustDo(t, err)
	if view.EventID != c.prefix+"again" || view.Evidence != Chain {
		t.Fatalf("the assignment view reads %+v", view)
	}
}

// statementCounter is a querier that keeps what it was asked.
type statementCounter struct {
	store.Querier
	asked []string
}

func (c *statementCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.asked = append(c.asked, query)
	return c.Querier.QueryContext(ctx, query, args...)
}

// The revisions and the suppressed receipts a naming may be read through come from one statement,
// so that what is live and what a suppressed receipt declared are one moment's answer: a read in
// two steps, on a connection that is not inside a transaction, could pair a first answer with a
// second one that a later emit changed.
func TestTheSuppressedReceiptsAreReadWithTheRevisionsInOneStatement(t *testing.T) {
	t.Parallel()
	w := newSPWorld(t)
	w.suppress(w.emit("cut", 1, "turn-cut", "inProgress", "cut.txt", "cut off", ""))
	w.emit("after", 1, "turn-after-restart", "completed", "after.txt", "after the restart", w.hash["cut"])
	counter := &statementCounter{Querier: w.store.Q(w.ctx)}
	head, err := HeadRevisionFrom(w.ctx, counter, w.rid, 1)
	mustDo(t, err)
	if pyjson.Text(head.Get("evidence")) != Sole {
		t.Fatalf("the re-emit is the only revision: %s", dumps(head))
	}
	reads := 0
	for _, query := range counter.asked {
		if strings.Contains(query, "revision_lineage") {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("lineage was read by %d statements: %q", reads, counter.asked)
	}
}
