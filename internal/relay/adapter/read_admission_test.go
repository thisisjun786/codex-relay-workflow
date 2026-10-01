package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// These tests pin the lifetime rule the transport gives reads: a read that arrives after Close
// began is refused, a read in flight finishes (or is cancelled once the drain budget is spent)
// before Close closes the shared RPC client and the ledger, and work an admitted send already
// holds keeps its own reads. None of them sleeps: the host holds a call on a channel, and
// "Close has begun" is observed by a refused probe read.

// gatedHost is a fake App Server. A held call blocks until its gate opens or its context ends.
// It records every call and the client's close in order, so a test can say what came before it.
type gatedHost struct {
	mu                  sync.Mutex
	gates               map[string]chan struct{}
	entered             chan string
	events              []string
	inFlight            int
	closedWhileInFlight int
	closeCalled         chan struct{}
}

func newGatedHost() *gatedHost {
	return &gatedHost{gates: map[string]chan struct{}{}, entered: make(chan string, 16), closeCalled: make(chan struct{})}
}

// hold makes the next call of method block. The returned release opens the gate; it also opens
// when the test ends, so a failing test never leaves a call blocked.
func (h *gatedHost) hold(t *testing.T, method string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	h.mu.Lock()
	h.gates[method] = gate
	h.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

func (h *gatedHost) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	h.mu.Lock()
	h.inFlight++
	h.events = append(h.events, "start "+method)
	gate := h.gates[method]
	delete(h.gates, method)
	h.mu.Unlock()
	if gate != nil {
		h.entered <- method
		select {
		case <-gate:
		case <-ctx.Done():
			h.settle("cancelled " + method)
			return nil, ctx.Err()
		}
	}
	h.settle("end " + method)
	return h.answer(method, params)
}

func (h *gatedHost) settle(event string) {
	h.mu.Lock()
	h.inFlight--
	h.events = append(h.events, event)
	h.mu.Unlock()
}

func (h *gatedHost) Close() error {
	h.mu.Lock()
	h.closedWhileInFlight += h.inFlight
	h.events = append(h.events, "close")
	h.mu.Unlock()
	close(h.closeCalled)
	return nil
}

func (h *gatedHost) answer(method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case "thread/read":
		return json.Marshal(map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}})
	case "thread/resume":
		return json.Marshal(resume())
	case "turn/start":
		return json.Marshal(map[string]any{"turn": map[string]any{"id": "turn-1"}})
	case "thread/start":
		cwd, _ := params["cwd"].(string)
		return json.Marshal(map[string]any{"thread": map[string]any{"id": "thread-created-1"}, "cwd": cwd, "approvalPolicy": "never", "model": "gpt-5.4", "reasoningEffort": "medium", "runtimeWorkspaceRoots": []any{cwd}, "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}})
	case "thread/goal/get":
		return json.Marshal(map[string]any{"goal": map[string]any{"status": "active"}})
	case "thread/turns/list":
		if params["cursor"] == nil {
			return json.Marshal(map[string]any{"data": []any{map[string]any{"id": "turn-other", "status": "completed"}}, "nextCursor": "page-2"})
		}
		return json.Marshal(map[string]any{"data": []any{map[string]any{"id": "turn-wanted", "status": "completed", "startedAt": 1.0}}})
	case "thread/list", "thread/items/list":
		return json.Marshal(map[string]any{"data": []any{}})
	}
	return nil, &HostUnavailable{"unexpected method " + method}
}

func (h *gatedHost) log() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

// assertClosedLast fails unless the client's close is the last thing the host saw and no call
// was in flight when it ran: nothing ran before it finished, and nothing reached it afterwards.
func (h *gatedHost) assertClosedLast(t *testing.T) {
	t.Helper()
	events := h.log()
	h.mu.Lock()
	inFlight := h.closedWhileInFlight
	h.mu.Unlock()
	if n := len(events); n == 0 || events[n-1] != "close" {
		t.Errorf("the client must close after every call has ended and before nothing else; events: %v", events)
	}
	if inFlight != 0 {
		t.Errorf("%d call(s) were still in flight when the client closed; events: %v", inFlight, events)
	}
}

func admissionAdapter(t *testing.T, host *gatedHost, drain time.Duration) *Adapter {
	t.Helper()
	return admissionAdapterWith(t, host, drain, func(*Options) {})
}

func admissionAdapterWith(t *testing.T, host *gatedHost, drain time.Duration, set func(*Options)) *Adapter {
	t.Helper()
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{RPC: host, Ledger: l, Drain: drain}
	set(&options)
	a := New(options)
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func closeAsync(a *Adapter) <-chan error {
	done := make(chan error, 1)
	go func() { done <- a.Close() }()
	return done
}

// awaitRefusal probes with real reads until Close has begun refusing them, which is the proof
// that it has started. It fails when the client closes first: that is Close not waiting for a
// read that was in flight. Gosched yields to the Close goroutine; the deadline only bounds a
// failing run.
func awaitRefusal(t *testing.T, a *Adapter, host *gatedHost) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-host.closeCalled:
			t.Fatal("the client closed before Close refused a read: Close did not wait for a read in flight")
		default:
		}
		_, err := a.ReadGoalStatus("probe")
		if errors.Is(err, ErrTransportClosing) {
			return
		}
		if err != nil {
			t.Fatalf("probe read: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never began refusing reads")
		}
		runtime.Gosched()
	}
}

func sendSettings() *delivery.TaskSettings {
	return &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
}

func TestReadAdmission_CloseWaitsForAnInFlightRead(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Hour)
	release := host.hold(t, "thread/read")
	type outcome struct {
		facts delivery.ThreadFacts
		err   error
	}
	read := make(chan outcome, 1)
	go func() {
		facts, err := a.ReadThread("thread-held")
		read <- outcome{facts, err}
	}()
	waitEdge(t, host.entered)
	closed := closeAsync(a)
	awaitRefusal(t, a, host)
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a read was in flight", err)
	case out := <-read:
		t.Fatalf("the held read returned early: %+v", out)
	default:
	}
	release()
	got := waitEdge(t, read)
	if got.err != nil || got.facts.RuntimeStatus != "idle" {
		t.Fatalf("a read in flight must finish with its answer: %+v", got)
	}
	if err := waitEdge(t, closed); err != nil {
		t.Fatal(err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_CloseCancelsAReadThatOutlivesTheDrainBudget(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, -1)
	host.hold(t, "thread/read")
	read := make(chan error, 1)
	go func() {
		_, err := a.ReadThread("thread-held")
		read <- err
	}()
	waitEdge(t, host.entered)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitEdge(t, read); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read cancelled by Close must report the cancellation, got %v", err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_ACancelledScanEndsBeforeTheClientCloses(t *testing.T) {
	host := newGatedHost()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "go", "go-store.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := admissionAdapterWith(t, host, -1, func(o *Options) { o.Store = db; o.Clock = delivery.NewFakeClock() })
	host.hold(t, "thread/list")
	type outcome struct {
		archived *bool
		err      error
	}
	read := make(chan outcome, 1)
	go func() {
		archived, err := a.IsArchived("thread-held", nil)
		read <- outcome{archived, err}
	}()
	waitEdge(t, host.entered)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// A listing call that fails is the scan's own "unknown"; this pins that the calls the cancelled
	// scan still makes all happen before the client closes, and that the cursor the failed first
	// listing records still reaches the relay's own store, which Close does not own.
	if got := waitEdge(t, read); got.err != nil || got.archived != nil {
		t.Fatalf("a cancelled listing scan answers unknown, got %+v", got)
	}
	host.assertClosedLast(t)
	saved, err := db.DiscoveryCursor(context.Background(), "thread-held", "archived")
	if err != nil || saved.Exhausted {
		t.Fatalf("the cancelled scan must record its unfinished cursor: %+v %v", saved, err)
	}
}

func TestReadAdmission_ReadsAfterCloseAreRefused(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Second)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	before := host.log()
	ctx := context.Background()
	managedAdapter := Managed{a}
	reads := []struct {
		name string
		read func() error
	}{
		{"HostCall", func() error { _, err := a.HostCall(ctx, "thread/read", map[string]any{"threadId": "t"}); return err }},
		{"ReadThread", func() error { _, err := a.ReadThread("t"); return err }},
		{"ReadGoalStatus", func() error { _, err := a.ReadGoalStatus("t"); return err }},
		{"ListTurnIDs", func() error { _, err := a.ListTurnIDs("t", 5); return err }},
		{"ReadTurn", func() error { _, err := a.ReadTurn("t", "turn"); return err }},
		{"IsArchived", func() error { _, err := a.IsArchived("t", nil); return err }},
		{"FindToken", func() error { _, err := a.FindToken("t", "token", 5, false); return err }},
		{"RecipientFingerprint", func() error { _, err := a.RecipientFingerprint("t"); return err }},
		{"FindDispatchedTurn", func() error { _, err := a.FindDispatchedTurn("t", "turn", 0); return err }},
		{"FindTokenSince", func() error { _, err := a.FindTokenSince("t", "token", nil, 5); return err }},
		{"FindTokenInTurn", func() error { _, err := a.FindTokenInTurn("t", "token", "turn", 5); return err }},
		{"GetOperation", func() error { _, err := a.GetOperation("operation"); return err }},
		{"Managed.GetOperation", func() error { _, err := managedAdapter.GetOperation(ctx, "operation"); return err }},
		{"Managed.ReadTurn", func() error { _, err := managedAdapter.ReadTurn(ctx, "t", "turn"); return err }},
	}
	for _, c := range reads {
		if err := c.read(); !errors.Is(err, ErrTransportClosing) {
			t.Errorf("%s after Close: want ErrTransportClosing, got %v", c.name, err)
		}
	}
	if after := host.log(); len(after) != len(before) {
		t.Errorf("a refused read reached the host: %v", after[len(before):])
	}
}

func TestReadAdmission_CreatesKeepTheirOwnRefusal(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Second)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	const want = "the relay transport is shutting down; nothing was sent"
	if _, err := a.Create(context.Background(), bridge.CreateThread{RequestID: "late-create", CWD: t.TempDir(), Sandbox: "read-only"}); err == nil || err.Error() != want {
		t.Errorf("Create after Close: want %q, got %v", want, err)
	}
	if _, err := (Managed{a}).CreateThread(context.Background(), managed.CreateThreadRequest{RequestID: "late-managed-create", CWD: t.TempDir(), Sandbox: "read-only"}); err == nil || err.Error() != want {
		t.Errorf("Managed.CreateThread after Close: want %q, got %v", want, err)
	}
}

func TestReadAdmission_AnAdmittedSendKeepsItsGuardReadsWhileCloseDrains(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Hour)
	inGuard := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(proceed) }) }
	t.Cleanup(open)
	guardRead := make(chan error, 1)
	guard := func(ctx context.Context) (map[string]any, error) {
		close(inGuard)
		select {
		case <-proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		_, err := a.HostCall(ctx, "thread/list", map[string]any{"limit": 1})
		guardRead <- err
		return nil, err
	}
	sent := make(chan map[string]any, 1)
	go func() {
		receipt, err := a.Send(context.Background(), "req-guard", "thread-1", "hello", sendSettings(), guard, 1)
		if err != nil {
			sent <- map[string]any{"error": err.Error()}
			return
		}
		sent <- plain(receipt).(map[string]any)
	}()
	waitEdge(t, inGuard)
	closed := closeAsync(a)
	awaitRefusal(t, a, host)
	open()
	if err := waitEdge(t, guardRead); err != nil {
		t.Fatalf("the guard's read inside an admitted send was refused while Close drained: %v", err)
	}
	if receipt := waitEdge(t, sent); receipt["status"] != "accepted" {
		t.Fatalf("the admitted send must complete: %v", receipt)
	}
	if err := waitEdge(t, closed); err != nil {
		t.Fatal(err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_ANestedReadIsCollectedAfterItsSendEnded(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, -1)
	host.hold(t, "thread/list")
	nested := make(chan error, 1)
	guard := func(ctx context.Context) (map[string]any, error) {
		// The read outlives the guard and the send: it keeps the values of the admitted work but
		// not its cancellation.
		go func() {
			_, err := a.HostCall(context.WithoutCancel(ctx), "thread/list", map[string]any{"limit": 1})
			nested <- err
		}()
		select {
		case <-host.entered:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	receipt, err := a.Send(context.Background(), "req-nested", "thread-1", "hello", sendSettings(), guard, 1)
	if err != nil || plain(receipt).(map[string]any)["status"] != "accepted" {
		t.Fatalf("send: %v %v", receipt, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitEdge(t, nested); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close must cancel and collect a nested read that outlived its send, got %v", err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_ARetainedContextIsNoAdmission(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Second)
	stored := make(chan context.Context, 1)
	later := make(chan error, 1)
	laterGate := make(chan struct{})
	guard := func(ctx context.Context) (map[string]any, error) {
		stored <- ctx
		go func() {
			<-laterGate
			_, err := a.HostCall(ctx, "thread/list", map[string]any{"limit": 1})
			later <- err
		}()
		return nil, nil
	}
	if _, err := a.Send(context.Background(), "req-stale", "thread-1", "hello", sendSettings(), guard, 1); err != nil {
		t.Fatal(err)
	}
	stale := <-stored
	// The send answers before its deferred cancel runs, so wait for the original context to end.
	<-stale.Done()
	// Once its send ended, the context admits nothing by itself: an ordinary fresh read is served...
	if _, err := a.HostCall(context.WithoutCancel(stale), "thread/list", map[string]any{"limit": 1}); err != nil {
		t.Fatalf("a stale context's read before Close is an ordinary read: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	before := host.log()
	// ...and after Close it is refused, however the context is kept or carried.
	if _, err := a.HostCall(stale, "thread/list", map[string]any{"limit": 1}); !errors.Is(err, ErrTransportClosing) {
		t.Errorf("stale context: want ErrTransportClosing, got %v", err)
	}
	if _, err := a.HostCall(context.WithoutCancel(stale), "thread/list", map[string]any{"limit": 1}); !errors.Is(err, ErrTransportClosing) {
		t.Errorf("stale context without cancellation: want ErrTransportClosing, got %v", err)
	}
	close(laterGate)
	if err := waitEdge(t, later); !errors.Is(err, ErrTransportClosing) {
		t.Errorf("a guard goroutine reading after its send ended and Close began: want ErrTransportClosing, got %v", err)
	}
	if after := host.log(); len(after) != len(before) {
		t.Errorf("a refused read reached the closed client: %v", after[len(before):])
	}
}

func TestReadAdmission_AnotherTransportsLeaseIsNoLease(t *testing.T) {
	closedHost := newGatedHost()
	closed := admissionAdapter(t, closedHost, time.Second)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	before := closedHost.log()
	liveHost := newGatedHost()
	live := admissionAdapter(t, liveHost, time.Second)
	foreign := make(chan error, 1)
	guard := func(ctx context.Context) (map[string]any, error) {
		// ctx carries a live lease, but of the other adapter's transport.
		_, err := closed.HostCall(ctx, "thread/list", map[string]any{"limit": 1})
		foreign <- err
		return nil, nil
	}
	if _, err := live.Send(context.Background(), "req-foreign", "thread-1", "hello", sendSettings(), guard, 1); err != nil {
		t.Fatal(err)
	}
	if err := waitEdge(t, foreign); !errors.Is(err, ErrTransportClosing) {
		t.Fatalf("a live lease of another adapter must not admit a read on a closed one, got %v", err)
	}
	if after := closedHost.log(); len(after) != len(before) {
		t.Errorf("a refused read reached the closed adapter's host: %v", after[len(before):])
	}
}

func TestReadAdmission_OneAdmissionCoversAMultiPageRead(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Hour)
	release := host.hold(t, "thread/turns/list")
	type outcome struct {
		turn *delivery.TurnInfo
		err  error
	}
	read := make(chan outcome, 1)
	go func() {
		turn, err := a.ReadTurn("thread-1", "turn-wanted")
		read <- outcome{turn, err}
	}()
	waitEdge(t, host.entered)
	closed := closeAsync(a)
	awaitRefusal(t, a, host)
	release()
	got := waitEdge(t, read)
	if got.err != nil || got.turn == nil || got.turn.TurnID != "turn-wanted" {
		t.Fatalf("a read admitted before Close must finish its later pages: %+v", got)
	}
	if err := waitEdge(t, closed); err != nil {
		t.Fatal(err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_ACreateReadsItsReceiptBeforeCloseCanEndTheLedger(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Hour)
	release := host.hold(t, "turn/start")
	type outcome struct {
		receipt map[string]any
		err     error
	}
	created := make(chan outcome, 1)
	go func() {
		receipt, err := (Managed{a}).CreateThread(context.Background(), managed.CreateThreadRequest{RequestID: "create-held", CWD: t.TempDir(), Prompt: "bootstrap", Sandbox: "read-only", Model: "gpt-5.4", ReasoningEffort: "medium"})
		created <- outcome{receipt, err}
	}()
	waitEdge(t, host.entered)
	closed := closeAsync(a)
	awaitRefusal(t, a, host)
	release()
	got := waitEdge(t, created)
	if got.err != nil || got.receipt["threadId"] != "thread-created-1" {
		t.Fatalf("a create admitted before Close must return its retained receipt: %+v", got)
	}
	if err := waitEdge(t, closed); err != nil {
		t.Fatal(err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_ACancelledCallerReleasesItsAdmission(t *testing.T) {
	host := newGatedHost()
	a := admissionAdapter(t, host, time.Hour)
	host.hold(t, "thread/read")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan error, 1)
	go func() {
		_, err := a.HostCall(ctx, "thread/read", map[string]any{"threadId": "thread-held"})
		read <- err
	}()
	waitEdge(t, host.entered)
	cancel()
	if err := waitEdge(t, read); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller's cancellation must reach its read, got %v", err)
	}
	// A leaked admission would hold Close for the whole drain budget, an hour here.
	if err := waitEdge(t, closeAsync(a)); err != nil {
		t.Fatal(err)
	}
	host.assertClosedLast(t)
}

func TestReadAdmission_AnAdapterWithoutATransportStillReads(t *testing.T) {
	host := newGatedHost()
	a := New(Options{RPC: host})
	if status, err := a.ReadGoalStatus("thread-1"); err != nil || status != "active" {
		t.Fatalf("a read-only adapter has no transport to refuse on: %v %v", status, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.closeCalled:
		t.Fatal("an adapter without a transport has no client of its own to close")
	default:
	}
}
