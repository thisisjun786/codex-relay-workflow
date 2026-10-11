package managed

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// An orphan is replaced only when the host would let a business turn start on it. A child whose goal the host holds paused,
// usage- or budget-limited, or that takes no direct input, holds the request as it held the business turn, whatever way
// its standby turn ended: it is neither archived nor replaced, and it is judged again when the hold has gone.
func TestOrphanReplacementKeepsTheChildsHost(t *testing.T) {
	t.Parallel()
	for _, hold := range []struct {
		name   string
		reason string
		set    func(*collapseApp)
		clear  func(*collapseApp)
	}{
		{"paused", "recipient_paused", func(a *collapseApp) { a.goal = map[string]string{"t-1": "paused"} }, func(a *collapseApp) { a.goal = nil }},
		{"usage limited", "recipient_usage_limited", func(a *collapseApp) { a.goal = map[string]string{"t-1": "usageLimited"} }, func(a *collapseApp) { a.goal = nil }},
		{"budget limited", "recipient_budget_limited", func(a *collapseApp) { a.goal = map[string]string{"t-1": "budgetLimited"} }, func(a *collapseApp) { a.goal = nil }},
		{"no direct input", "recipient_cannot_accept_input", func(a *collapseApp) { a.noInput = map[string]bool{"t-1": true} }, func(a *collapseApp) { a.noInput = nil }},
	} {
		for _, standby := range []string{"completed", "interrupted", "failed"} {
			t.Run(hold.name+"/"+standby, func(t *testing.T) {
				t.Parallel()
				k, app := collapseKit(t, "t-1")
				k.host.standby = standby
				hold.set(app)
				for round := range 3 {
					got := k.run()
					k.expect(got, "incomplete", hold.reason, "standby_orphan_held")
					if got["stage"] != "business" {
						t.Fatalf("round %d: the hold is the business stage's: %v", round, got["stage"])
					}
					if round == 1 {
						k.restart()
					}
				}
				if len(app.archiveCalls) != 0 || app.creates != 1 || k.host.sent != 0 {
					t.Fatalf("nothing archived, created or sent while the host holds the child: archives %v creates %d sent %d", app.archiveCalls, app.creates, k.host.sent)
				}
				hold.clear(app)
				got := k.run()
				k.expect(got, "admitted", "", "recreated")
				if got["childTaskId"] != "t-2" || !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) {
					t.Fatalf("with the hold gone the orphan is replaced once: child %v archives %v", got["childTaskId"], app.archiveCalls)
				}
			})
		}
	}
}

// Another client starts a turn on the orphan between its judgement and the archive: the archive is not sent, and the request is
// held retryable (recipient_not_idle, as the unload recovery answered it) instead of registering the busy child and sending the
// business turn to it, which would fail for good. When the child is idle again the next call replaces it.
func TestOrphanBecameActiveBeforeArchiveHoldsTheRequest(t *testing.T) {
	t.Parallel()
	k, app := collapseKit(t, "t-1")
	flipped := false
	app.onRead = func(thread string) {
		if thread != "t-1" || len(k.orphanRows("managed_standby_orphan")) == 0 {
			return
		}
		// The other client's turn is running at the last read after the mark and has ended by the next one.
		if !flipped {
			flipped = true
			k.host.threads["t-1"].status = "active"
		} else if k.host.threads["t-1"].status == "active" {
			k.host.threads["t-1"].status = "idle"
		}
	}
	got := k.run()
	k.expect(got, "incomplete", "recipient_not_idle", "standby_orphan_held")
	if got["stage"] != "standby" {
		t.Fatalf("stage %v", got["stage"])
	}
	if len(app.archiveCalls) != 0 || app.creates != 1 || k.host.sent != 0 {
		t.Fatalf("nothing archived, created or sent: %v %d %d", app.archiveCalls, app.creates, k.host.sent)
	}
	k.effects(1, 0)
	if rows := k.orphanRows("managed_standby_orphan"); len(rows) != 2 || !strings.Contains(rows[1], `"archive":"skipped"`) {
		t.Fatalf("the mark is closed skipped: %v", rows)
	}
	k.restart()
	got = k.run()
	k.expect(got, "admitted", "", "recreated")
	if got["childTaskId"] != "t-2" || !reflect.DeepEqual(app.archiveCalls, []string{"t-1"}) {
		t.Fatalf("once idle the orphan is replaced: child %v archives %v", got["childTaskId"], app.archiveCalls)
	}
}

// pollErrorApp fails every ReadTurn with the App Server client's own phase timeout while the wait's deadline is still open (the
// read's RPC bound is shorter than the wait's) and reports the standby's end on its creation watch.
type pollErrorApp struct{ *waitApp }

func (a *pollErrorApp) ReadTurn(context.Context, string, string) (*Turn, error) {
	time.Sleep(80 * time.Millisecond) // the read is still running when the watch reports the end
	return nil, &appserver.PhaseTimeout{Method: "thread/turns/list", Phase: "ack", Bound: 20 * time.Millisecond}
}

func (a *pollErrorApp) CreateThreadHeld(ctx context.Context, in CreateThreadRequest) (map[string]any, CreationHold, error) {
	receipt, err := a.waitApp.CreateThread(ctx, in)
	return receipt, timedHold{at: time.Now(), after: 30 * time.Millisecond}, err
}

// A failed host read does not discard the proof the creation watch already gave: when the watch has the standby's end the call goes
// on; without it the read's error is the call's error, and the caller's cancellation is still the caller's.
func TestWatchCompletionBeatsAFailedRead(t *testing.T) {
	t.Parallel()
	t.Run("the watch has the end", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 10*time.Second, 5*time.Millisecond)
		k.start.Adapter = &pollErrorApp{waitApp: app}
		started := time.Now()
		got, err := k.runErr()
		if err != nil || got["state"] != "admitted" || time.Since(started) > 5*time.Second {
			t.Fatalf("the watch's turn/completed ends the standby although the read failed: %v %v after %v", got["state"], err, time.Since(started))
		}
		k.effects(1, 1)
	})
	t.Run("the watch has nothing", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 10*time.Second, 5*time.Millisecond)
		k.start.Adapter = &noWatchPollErrorApp{waitApp: app}
		if _, err := k.runErr(); err == nil || !strings.Contains(err.Error(), "thread/turns/list") {
			t.Fatalf("a failed read with no watch evidence is the call's error: %v", err)
		}
	})
	t.Run("the caller's cancellation stays the caller's", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 10*time.Second, 5*time.Millisecond)
		k.start.Adapter = &cancelReadApp{pollErrorApp: &pollErrorApp{waitApp: app}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		k.start.Adapter.(*cancelReadApp).cancel = cancel
		if _, err := k.start.Run(ctx, k.raw); !errors.Is(err, context.Canceled) {
			t.Fatalf("the caller's cancellation is the caller's error: %v", err)
		}
	})
}

// noWatchPollErrorApp fails the read and hands over a watch that never reports the end.
type noWatchPollErrorApp struct{ *waitApp }

func (a *noWatchPollErrorApp) ReadTurn(context.Context, string, string) (*Turn, error) {
	return nil, &appserver.PhaseTimeout{Method: "thread/turns/list", Phase: "ack", Bound: 20 * time.Millisecond}
}

func (a *noWatchPollErrorApp) CreateThreadHeld(ctx context.Context, in CreateThreadRequest) (map[string]any, CreationHold, error) {
	receipt, err := a.waitApp.CreateThread(ctx, in)
	return receipt, timedHold{at: time.Now(), after: time.Hour}, err
}

// cancelReadApp cancels the caller's context during the read and reports the watch's end: the caller's cancellation wins.
type cancelReadApp struct {
	*pollErrorApp
	cancel context.CancelFunc
}

func (a *cancelReadApp) ReadTurn(context.Context, string, string) (*Turn, error) {
	a.cancel()
	return nil, errors.New("read failed after the caller gave up")
}
