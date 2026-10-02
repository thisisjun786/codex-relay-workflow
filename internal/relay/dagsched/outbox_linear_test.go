package dagsched

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The test doubles of the summary outbox: a Linear document that behaves as the connector the skill names (save_document with patch: an atomic replacement whose
// old_string must match the current content exactly once), with failures injected on request, and the parent's turn written as the crw-run skill writes it. The relay under test is
// the real Go store and scheduler; only Linear is a double.

var (
	errLinearDown    = errors.New("linear: service unavailable")
	errLinearLost    = errors.New("linear: the response was lost")
	errLinearNoMatch = errors.New("linear: old_string does not match the current content exactly once")
)

var seqHeader = regexp.MustCompile("(?m)^seq: ([0-9]+)$")

// seqIn is the sequence number of the summary block a text carries, 0 when it carries none.
func seqIn(text string) int64 {
	m := seqHeader.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// fakeLinear is one Linear document and the books a test keeps of what was done to it. overwrites counts the landed writes that put a summary older than one that had already
// landed: the number the criteria say must be 0.
type fakeLinear struct {
	doc        string
	maxLanded  int64
	overwrites int
	conflicts  int
	calls      map[int64]int // write calls per sequence number, whatever became of them
	landed     map[int64]int // writes that changed the document, per sequence number
	failFirst  map[int64]int // per sequence number: the next n write calls fail before they land
	loseFirst  map[int64]int // per sequence number: the next n write calls land and then report an error
	// beforePatch runs inside a write call, before the condition is judged: where a concurrent editor gets in.
	beforePatch func()
}

func newFakeLinear() *fakeLinear {
	return &fakeLinear{calls: map[int64]int{}, landed: map[int64]int{}, failFirst: map[int64]int{}, loseFirst: map[int64]int{}}
}

func (l *fakeLinear) read() string { return l.doc }

// initContainer is the one creation of a plan's container, with an empty body (an append the first time, nothing after).
func (l *fakeLinear) initContainer(container string) {
	if !strings.Contains(l.doc, container) {
		l.doc += container + "\n"
	}
}

func (l *fakeLinear) record(text string) {
	seq := seqIn(text)
	if seq == 0 {
		return
	}
	if seq < l.maxLanded {
		l.overwrites++
	}
	l.maxLanded = max(l.maxLanded, seq)
}

// patch is save_document with patch [{op: replace, old_string, new_string}]: atomic, and refused whole when old_string is not in the current content exactly once.
func (l *fakeLinear) patch(seq int64, old, replacement string) error {
	l.calls[seq]++
	if l.failFirst[seq] > 0 {
		l.failFirst[seq]--
		return errLinearDown
	}
	if l.beforePatch != nil {
		hook := l.beforePatch
		l.beforePatch = nil
		hook()
	}
	if old == "" || strings.Count(l.doc, old) != 1 {
		l.conflicts++
		return errLinearNoMatch
	}
	l.doc = strings.Replace(l.doc, old, replacement, 1)
	l.landed[seq]++
	l.record(replacement)
	if l.loseFirst[seq] > 0 {
		l.loseFirst[seq]--
		return errLinearLost
	}
	return nil
}

// overwrite is a whole-document save (content: ...), the shape the skill forbids for a summary: it is not conditioned on what the document held.
func (l *fakeLinear) overwrite(doc string) {
	l.doc = doc
	l.record(doc)
}

// humanEdit is a person editing the document: it changes text that is there, without a condition.
func (l *fakeLinear) humanEdit(old, replacement string) {
	l.doc = strings.Replace(l.doc, old, replacement, 1)
}

// containerIn is the exact text of the container a document holds, "" when it holds none.
func containerIn(doc, start, end string) string {
	i := strings.Index(doc, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(doc[i:], end)
	if j < 0 {
		return ""
	}
	return doc[i : i+j+len(end)]
}

// parentFlow is the parent's turn for one document: the procedure of the crw-run skill, command by command.
type parentFlow struct {
	t        *testing.T
	f        *fixture
	lin      *fakeLinear
	plan     string
	actor    string
	document string
}

func newParentFlow(f *fixture, lin *fakeLinear, document string) *parentFlow {
	return &parentFlow{t: f.t, f: f, lin: lin, plan: "p1", actor: "parent", document: document}
}

func (p *parentFlow) ctx() context.Context { return context.Background() }

// enqueue records the plan's summary for the document as it stands now.
func (p *parentFlow) enqueue() SummaryEnqueued {
	p.t.Helper()
	out, err := p.f.sched.EnqueueSummary(p.ctx(), p.plan, p.actor, p.document, false)
	if err != nil {
		p.t.Fatalf("enqueue: %v", err)
	}
	return out
}

// newest is the entry the plan owes the document now (or the confirmed one it last wrote).
func (p *parentFlow) newest() (SummaryEntry, bool) {
	p.t.Helper()
	st, err := p.f.sched.SummaryStatus(p.ctx(), p.plan, p.document, false)
	if err != nil {
		p.t.Fatalf("status: %v", err)
	}
	if len(st.Streams) == 0 {
		return SummaryEntry{}, false
	}
	return st.Streams[0].Newest, true
}

// flowResult is where one turn of the parent ended.
type flowResult struct {
	Entry SummaryEntry
	// Step is what the turn did: idle (nothing owed), wrote (the entry was written and confirmed), reconciled (the document already carried it: confirmed without a write),
	// failed (the write failed and the failure was recorded).
	Step string
	Err  error
}

// drain is one turn of the parent for the document: take the newest entry, write it to Linear, read it back, confirm it in the relay, and on failure retry only that entry.
func (p *parentFlow) drain() flowResult {
	p.t.Helper()
	entry, ok := p.newest()
	if !ok || entry.State == SummaryConfirmed {
		return flowResult{Entry: entry, Step: "idle"}
	}
	if entry.State == SummaryFailed {
		if _, err := p.f.sched.RetrySummary(p.ctx(), entry.SummaryID, p.actor); err != nil {
			p.t.Fatalf("retry: %v", err)
		}
	}
	claim, err := p.f.sched.ClaimSummary(p.ctx(), entry.SummaryID, p.actor)
	if err != nil {
		p.t.Fatalf("claim %s: %v", entry.SummaryID, err)
	}
	op := claim.Operation
	observed := p.lin.read()
	rec, err := p.f.sched.ReconcileSummary(p.ctx(), entry.SummaryID, observed)
	if err != nil {
		p.t.Fatalf("reconcile: %v", err)
	}
	step := "wrote"
	switch rec.Outcome {
	case "already_written":
		step = "reconciled"
	case "absent", "stale":
		if containerIn(observed, op.ContainerStart, op.ContainerEnd) == "" {
			p.lin.initContainer(op.EmptyContainer)
		}
		old := containerIn(p.lin.read(), op.ContainerStart, op.ContainerEnd)
		if werr := p.lin.patch(claim.Entry.Seq, old, op.Container); werr != nil {
			failed, ferr := p.f.sched.FailSummary(p.ctx(), entry.SummaryID, p.actor, claim.Token, werr.Error())
			if ferr != nil {
				p.t.Fatalf("fail: %v", ferr)
			}
			return flowResult{Entry: failed, Step: "failed", Err: werr}
		}
	default:
		p.t.Fatalf("the document reads %s (%s)", rec.Outcome, rec.Detail)
	}
	done, err := p.f.sched.CompleteSummary(p.ctx(), entry.SummaryID, p.actor, claim.Token, p.document, p.lin.read())
	if err != nil {
		p.t.Fatalf("complete: %v", err)
	}
	return flowResult{Entry: done.Entry, Step: step}
}

// preparedWrite is a write a parent has prepared (it claimed the entry and read the document) and not yet sent.
type preparedWrite struct {
	entry SummaryEntry
	claim SummaryClaim
	old   string
}

// prepare claims the entry and reads the document, as drain does up to the write.
func (p *parentFlow) prepare(id string) preparedWrite {
	p.t.Helper()
	claim, err := p.f.sched.ClaimSummary(p.ctx(), id, p.actor)
	if err != nil {
		p.t.Fatalf("claim %s: %v", id, err)
	}
	op := claim.Operation
	if containerIn(p.lin.read(), op.ContainerStart, op.ContainerEnd) == "" {
		p.lin.initContainer(op.EmptyContainer)
	}
	return preparedWrite{entry: claim.Entry, claim: claim, old: containerIn(p.lin.read(), op.ContainerStart, op.ContainerEnd)}
}

// send applies the prepared write to Linear: the replacement of the container text the parent read.
func (w preparedWrite) send(l *fakeLinear) error {
	return l.patch(w.entry.Seq, w.old, w.claim.Operation.Container)
}

// bump moves the plan on by one revision (a node is added), so its progress differs from every earlier state.
func (f *fixture) bump(n int) {
	f.t.Helper()
	snap := f.snapshot("p1")
	f.putPlan("p1", int(snap.Revision), fmt.Sprintf("bump-%d", n), addNode(fmt.Sprintf("x%d", n), dag.NodeNonPR))
}

// summaryFixture is a store with the fork/join plan p1 and its project's parent registered.
func summaryFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	return f
}
