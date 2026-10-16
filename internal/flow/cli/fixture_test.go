package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var ctx = context.Background()

// world is a relay state directory with a store in it, and the temporary location its starts record (TMPDIR).
type world struct {
	t     *testing.T
	state string
	tmp   string
	db    string
	st    *store.Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, state: t.TempDir(), tmp: t.TempDir()}
	w.db = filepath.Join(w.state, "relay.sqlite3")
	w.open()
	t.Cleanup(w.close)
	return w
}

func (w *world) open() {
	st, err := store.Open(ctx, w.db, "")
	if err != nil {
		w.t.Fatal(err)
	}
	w.st = st
}

func (w *world) close() {
	if w.st != nil {
		_ = w.st.Close()
		w.st = nil
	}
}

// run is a run the world's journal holds, with rows appended through the journal as a run process appends them.
type run struct {
	w  *world
	id string
	j  *journal.Journal
}

// newRun starts the journal of a run owned by owner, with its started row.
func (w *world) newRun(id, owner string) *run {
	w.t.Helper()
	j, err := journal.Open(ctx, w.st, id, journal.Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: owner, EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		w.t.Fatal(err)
	}
	r := &run{w: w, id: id, j: j}
	r.start()
	return r
}

func (r *run) start() {
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
}

func (r *run) finish(status, kind string) {
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": status, "error_kind": kind}})
}

func (r *run) phase(title string) {
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindFlow, Event: "phase", Data: map[string]any{"title": title}})
}

func (r *run) flow(event string, data map[string]any) engine.Event {
	return r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindFlow, Event: event, Data: data})
}

// call appends the rows of an agent call: events is the list of event names in order; the data of a row is given by name.
func (r *run) call(callID, role, node string, attempt int, events []string, data map[string]map[string]any) {
	for _, ev := range events {
		d := map[string]any{}
		for k, v := range data[ev] {
			d[k] = v
		}
		if ev == engine.EvPrepared {
			d["role"], d["node"], d["attempt"] = role, node, attempt
		}
		r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindCall, CallID: callID, Path: "root", Position: 1, Op: "agent", Label: role + ":" + node, Event: ev, Data: d})
	}
}

// completed appends a call that ended completed on a thread, with tokens.
func (r *run) completed(callID, role, node, thread string, tokens int64) {
	r.call(callID, role, node, 1, []string{engine.EvPrepared, engine.EvThreadRequested, engine.EvThreadStarted, engine.EvTurnRequested, engine.EvTurnStarted, engine.EvCompleted},
		map[string]map[string]any{engine.EvThreadStarted: {"thread_id": thread}, engine.EvCompleted: {"thread_id": thread, "tokens": map[string]any{"total": tokens}}})
}

// hold appends a call whose creation request got no answer and that reconcile left unknown: the node is in recovery_hold.
func (r *run) hold(callID, role, node string) {
	r.call(callID, role, node, 1, []string{engine.EvPrepared, engine.EvThreadRequested, engine.EvOutcomeUnknown},
		map[string]map[string]any{engine.EvThreadRequested: {"cwd": "/w/a1"}, engine.EvOutcomeUnknown: {"reason": engine.ReasonDispatchUnknown}})
}

// op appends the completed row of a lane operation.
func (r *run) op(callID, op string, data map[string]any) {
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindCall, CallID: callID, Path: "root", Position: 2, Op: op, Label: op, Event: engine.EvCompleted, Data: data})
}

func (r *run) close() {
	if err := r.j.Close(); err != nil {
		r.w.t.Fatal(err)
	}
}

// deps are the dependencies of a test: the real reader and writer on the world's state, a spawner that records, and a prober
// with the answers the test sets.
type testDeps struct {
	*Deps
	w       *world
	spawned []SpawnRequest
	order   *bytes.Buffer
	facts   []Fact
}

func (w *world) deps() *testDeps {
	td := &testDeps{w: w, order: &bytes.Buffer{}}
	for _, name := range Facts {
		td.facts = append(td.facts, Fact{Name: name, Pass: true, Reading: "fine"})
	}
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	td.Deps = &Deps{
		Now:    func() time.Time { return now },
		Getenv: w.withTemp(func(k string) (string, bool) { return "", false }),
		Reader: func(ctx context.Context, state, socket string) (*Reader, error) {
			return OpenReader(ctx, w.state, socket)
		},
		Writer: func(ctx context.Context, state, socket string) (*store.Store, string, error) {
			return OpenWriter(ctx, w.state, socket)
		},
		Spawn: func(ctx context.Context, req SpawnRequest) (int, error) {
			td.spawned = append(td.spawned, req)
			if td.order != nil {
				td.order.WriteString("<<spawned>>\n")
			}
			return 4242, nil
		},
		Probe:  ProberFunc(func(ctx context.Context, req PreflightRequest) []Fact { return td.facts }),
		Signal: func(pid int, sig syscall.Signal) error { return nil },
	}
	return td
}

// withTemp is getenv with TMPDIR answered by the world's temporary location (CRW-1570): a start records it, and so a test's run never
// depends on where or how long the TMPDIR of the test process is.
func (w *world) withTemp(getenv func(string) (string, bool)) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == "TMPDIR" {
			return w.tmp, true
		}
		return getenv(k)
	}
}

// do runs a command line and returns its exit status and output.
func (td *testDeps) do(args ...string) (int, string, string) {
	var errOut bytes.Buffer
	td.order.Reset()
	// the relay store has one writer: the world's own is closed while a command that writes opens its own
	td.w.close()
	code := RunWith(ctx, td.Deps, args, strings.NewReader(""), td.order, &errOut)
	td.w.open()
	return code, td.order.String(), errOut.String()
}

func openJournal(w *world, id string) (*journal.Journal, error) {
	return journal.Open(ctx, w.st, id, journal.Identity{}, journal.Options{Immediate: true})
}

func loadRun(w *world, id string) ([]engine.Event, bool, error) {
	return journal.Load(ctx, w.st, id)
}

// doKeep is do without closing the world's own store: for a command that only reads, while the test goes on writing.
func (td *testDeps) doKeep(args ...string) (int, string, string) {
	var errOut bytes.Buffer
	td.order.Reset()
	code := RunWith(ctx, td.Deps, args, strings.NewReader(""), td.order, &errOut)
	return code, td.order.String(), errOut.String()
}
