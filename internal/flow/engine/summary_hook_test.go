package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// recordingHook records the engine's calls of the upward hook.
type recordingHook struct {
	mu    sync.Mutex
	calls []string
	fail  string
	// onBatch appends a row to the journal from inside BatchCommitted, as the real hook does.
	journal Journal
}

func (h *recordingHook) note(point string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, point)
	if h.fail == point {
		return errors.New(point + " failed")
	}
	return nil
}

func (h *recordingHook) RunStarted(context.Context, string) error { return h.note("started") }
func (h *recordingHook) BatchCommitted(context.Context, string) error {
	err := h.note("batch")
	if h.journal != nil {
		h.journal.Append(Event{RunID: "run-hook", Kind: KindFlow, Event: "from_hook"})
	}
	return err
}
func (h *recordingHook) RunFinished(_ context.Context, _, status string) error {
	return h.note("finished:" + status)
}

// committingJournal is a MemoryJournal that tells its commit notifier after every row, as a journal that commits a batch for each row.
type committingJournal struct {
	*MemoryJournal
	mu     sync.Mutex
	notify func()
}

func (j *committingJournal) SetOnCommit(f func()) {
	j.mu.Lock()
	j.notify = f
	j.mu.Unlock()
}

func (j *committingJournal) Append(e Event) Event {
	out := j.MemoryJournal.Append(e)
	j.mu.Lock()
	f := j.notify
	j.mu.Unlock()
	if f != nil {
		f()
	}
	return out
}

func hookRunScript(t *testing.T, cfg Config) *Result {
	t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte("export const meta = { name: 't' }\nconst a = await agent.call('hi', { agentType: 'executor' })\nreturn a.status\n")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Backend, cfg.Policy, cfg.RunID = NewFake(), FakePolicy(), "run-hook"
	res, err := Run(context.Background(), prog, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The engine calls the upward hook when the run starts, after each batch the journal committed - never for the batch the hook's own rows
// make, and no more once the run has finished - and when the run finishes with its status; a failing call is a row of the journal and
// does not fail the run.
func TestEngineCallsTheUpwardHookAtStartBatchesAndFinish(t *testing.T) {
	j := &committingJournal{MemoryJournal: NewMemoryJournal()}
	h := &recordingHook{journal: j, fail: "batch"}
	res := hookRunScript(t, Config{Journal: j, Upward: h})
	if res.Status != RunCompleted {
		t.Fatalf("run %+v", res)
	}
	if len(h.calls) < 3 || h.calls[0] != "started" || h.calls[len(h.calls)-1] != "finished:completed" {
		t.Fatalf("calls %v", h.calls)
	}
	batches := 0
	for _, c := range h.calls {
		if c == "batch" {
			batches++
		}
	}
	if batches == 0 {
		t.Fatalf("no batch was told: %v", h.calls)
	}
	var failed, fromHook int
	for _, ev := range j.Events() {
		if ev.Kind == KindFlow && ev.Event == EvUpwardHookFailed {
			failed++
		}
		if ev.Event == "from_hook" {
			fromHook++
		}
	}
	if failed != batches || fromHook != batches {
		t.Errorf("failure rows %d, hook rows %d, batches %d (the hook's own rows must not be told again)", failed, fromHook, batches)
	}
	if j.notify != nil {
		t.Error("the commit notifier stays installed after the run")
	}
}

// A journal that cannot tell commits still gets the start and the finish; a run without a hook calls nothing.
func TestEngineHookWithAJournalThatCannotNotify(t *testing.T) {
	h := &recordingHook{}
	hookRunScript(t, Config{Journal: NewMemoryJournal(), Upward: h})
	if strings.Join(h.calls, ",") != "started,finished:completed" {
		t.Errorf("calls %v", h.calls)
	}
	hookRunScript(t, Config{Journal: NewMemoryJournal()})
}

// The summary document the command line names is a row of the run's journal before the script runs, once for the same document and
// again for another.
func TestRunRecordsTheSummaryDocument(t *testing.T) {
	code, out, errOut := runFlow(t, "run", "--backend", "fake", "--journal", "--summary-document", "doc-9", helloScript)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d %q", code, errOut)
	}
	var res struct {
		Journal []Event `json:"journal"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	var docs []string
	var startedAt, docAt int
	for i, ev := range res.Journal {
		if ev.Kind == KindRun && ev.Event == "started" {
			startedAt = i
		}
		if ev.Kind == KindFlow && ev.Event == EvSummaryDocument {
			docs = append(docs, ev.Data["document"].(string))
			docAt = i
		}
	}
	if len(docs) != 1 || docs[0] != "doc-9" || docAt < startedAt {
		t.Errorf("document rows %v (at %d, started at %d)", docs, docAt, startedAt)
	}
	// the same document on a resumed journal is not recorded twice
	j := NewMemoryJournal()
	hookRunScript(t, Config{Journal: j, SummaryDocument: "doc-1"})
	hookRunScript(t, Config{Journal: j, SummaryDocument: "doc-1"})
	hookRunScript(t, Config{Journal: j, SummaryDocument: "doc-2"})
	var got []string
	for _, ev := range j.Events() {
		if ev.Event == EvSummaryDocument {
			got = append(got, ev.Data["document"].(string))
		}
	}
	if strings.Join(got, ",") != "doc-1,doc-2" {
		t.Errorf("documents %v", got)
	}
}

// The rows a command writes into a run are numbered from ForeignSeqBase; the run counts after its own.
func TestLastRunSeqIgnoresTheRowsOfCommands(t *testing.T) {
	rows := []Event{{Seq: 3}, {Seq: 9}, {Seq: int(ForeignSeqBase) + 4}, {Seq: 5}}
	if got := lastRunSeq(rows); got != 9 {
		t.Errorf("lastRunSeq = %d", got)
	}
	if got := lastRunSeq(nil); got != 0 {
		t.Errorf("lastRunSeq(nil) = %d", got)
	}
}
