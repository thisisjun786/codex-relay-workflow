package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// These tests drive a whole daemon tick against the production adapter and a fake App Server that
// stops answering, and read what the fake host sees: whether the call it holds is ended when the
// tick's budget runs out and when the daemon stops. The wait for that end is only a bound on a
// failing run; a passing run waits for the end itself.

const hostEndBound = 3 * time.Second

// hangingHost answers its first answers calls and holds every call after them. A held call ends
// when its context ends, which it reports on ended, or when the test releases it, which stands
// for a host that is still answering when the caller has given up. With delay set, a held call
// instead answers by itself once the delay has passed, as a slow host does.
type hangingHost struct {
	mu      sync.Mutex
	calls   int
	answers int
	delay   time.Duration
	held    chan struct{}
	ended   chan error
	release chan struct{}
	once    sync.Once
}

func newHangingHost(t *testing.T, answers int, delay time.Duration) *hangingHost {
	t.Helper()
	h := &hangingHost{answers: answers, delay: delay, held: make(chan struct{}), ended: make(chan error, 8), release: make(chan struct{})}
	t.Cleanup(h.letGo)
	return h
}

func (h *hangingHost) letGo() { h.once.Do(func() { close(h.release) }) }

func (h *hangingHost) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls++
	hold := h.calls > h.answers
	h.mu.Unlock()
	if hold {
		select {
		case <-h.held:
		default:
			close(h.held)
		}
		var later <-chan time.Time
		if h.delay > 0 {
			later = time.After(h.delay)
		}
		select {
		case <-ctx.Done():
			h.ended <- ctx.Err()
			return nil, ctx.Err()
		case <-later:
		case <-h.release:
			return nil, errors.New("the test let the held call go")
		}
	}
	return json.Marshal(map[string]any{"data": []any{
		map[string]any{"id": "anchor-00", "status": "inProgress"},
		map[string]any{"id": "anchor-01", "status": "inProgress"},
	}})
}

func (h *hangingHost) awaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(hostEndBound):
		t.Fatal("the tick never reached the host call the test holds")
	}
}

// awaitEnd waits for the held call to end by its context and returns the context's error. A call
// that is not ended in time fails the test and is let go, so the tick it belongs to can finish.
func (h *hangingHost) awaitEnd(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.ended:
		return err
	case <-time.After(hostEndBound):
		h.letGo()
		t.Errorf("the host call in flight was not ended within %v of the tick's context ending", hostEndBound)
		return nil
	}
}

// observingDaemon is a daemon over a store holding n relationships, each waiting on its anchor
// turn anchor-NN, whose host is the production adapter over host.
func observingDaemon(t *testing.T, host *hangingHost, n int) *daemon.Daemon {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := range n {
		suffix := (strconv.Itoa(100 + i))[1:]
		rid, anchor := "rel-"+suffix, "anchor-"+suffix
		if _, err := s.DB.Exec("INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES(?,?,'active',?,'host','/parent',?,'host','/child',1,'[]',?, '2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", rid, "REL-1", "parent-"+suffix, "child-"+suffix, `["parent-`+suffix+`"]`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec("INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES(?,1,?,'bound',?,'initial_assignment','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", rid, "dispatch-"+rid, anchor); err != nil {
			t.Fatal(err)
		}
	}
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "go.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: host, Ledger: l, Drain: -1})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return daemon.New(s, a, &delivery.FakeClock{T: 1700000000 + 86400}, nil)
}

type tickResult struct {
	report daemon.Report
	err    error
}

func tickAsync(d *daemon.Daemon, ctx context.Context) <-chan tickResult {
	done := make(chan tickResult, 1)
	go func() {
		report, err := d.Tick(ctx)
		done <- tickResult{report, err}
	}()
	return done
}

func awaitTick(t *testing.T, done <-chan tickResult) tickResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(hostEndBound + 2*time.Second):
		t.Fatal("the tick did not finish")
		return tickResult{}
	}
}

// The daemon stopping ends the call the tick has in flight on the host.
func TestDaemonStopEndsTheHostCallInFlight(t *testing.T) {
	host := newHangingHost(t, 0, 0)
	d := observingDaemon(t, host, 1)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := tickAsync(d, ctx)
	host.awaitHeld(t)
	stop()
	if err := host.awaitEnd(t); !errors.Is(err, context.Canceled) {
		t.Errorf("the host call in flight ended with %v, want the daemon's cancellation", err)
	}
	awaitTick(t, done)
}

// The time the observation pass may spend (Policy.MaxObserveSeconds) running out ends the call the
// pass has in flight on the host, while the tick's own context is still live. The pass's first read
// is made whatever the clock says, so the held call is the second.
func TestTickBudgetEndsTheHostCallInFlight(t *testing.T) {
	host := newHangingHost(t, 1, 0)
	d := observingDaemon(t, host, 2)
	d.Policy.MaxObserveSeconds = 0.2
	started := time.Now()
	done := tickAsync(d, context.Background())
	host.awaitHeld(t)
	if err := host.awaitEnd(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the host call in flight ended with %v, want the pass's time bound", err)
	}
	result := awaitTick(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if took := time.Since(started); took < 150*time.Millisecond {
		t.Errorf("the call was ended after %v, before the 0.2 s the pass may spend", took)
	}
	var failed, stopped bool
	for _, note := range result.report.Notes {
		failed = failed || strings.HasPrefix(note, "turn read failed for anchor-01")
		stopped = stopped || strings.HasPrefix(note, "observation stopped after")
	}
	if !failed || !stopped {
		t.Errorf("notes %q: want the failed read of the second turn and the stop of the pass", result.report.Notes)
	}
}

// The first read of a pass is made whatever the clock says, so the time bound does not end it: a
// host that answers after the bound has run out is still read.
func TestTheFirstReadOfAPassOutlivesTheTimeBound(t *testing.T) {
	host := newHangingHost(t, 0, 300*time.Millisecond)
	d := observingDaemon(t, host, 1)
	d.Policy.MaxObserveSeconds = 0.05
	result := awaitTick(t, tickAsync(d, context.Background()))
	if result.err != nil {
		t.Fatal(result.err)
	}
	for _, note := range result.report.Notes {
		t.Errorf("unexpected note %q: the first read should have been answered", note)
	}
	select {
	case err := <-host.ended:
		t.Errorf("the first read was ended with %v", err)
	default:
	}
}
