package managed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitApp is the scripted App Server with a standby turn that ends when the test says so: statuses are
// the successive answers of ReadTurn (the last one repeats), and doneAfter, when set, makes the turn end
// that long after its first read. events is the order of what the engine asked of the host.
type waitApp struct {
	*scriptedApp
	statuses  []string
	doneAfter time.Duration
	first     time.Time
	reads     int
	events    []string
}

func (a *waitApp) ReadTurn(_ context.Context, _, turn string) (*Turn, error) {
	a.reads++
	a.events = append(a.events, "read")
	if a.first.IsZero() {
		a.first = time.Now()
	}
	if a.doneAfter > 0 && time.Since(a.first) >= a.doneAfter {
		return &Turn{ID: turn, Status: "completed"}, nil
	}
	if len(a.statuses) == 0 {
		return &Turn{ID: turn, Status: "inProgress"}, nil
	}
	status := a.statuses[0]
	if len(a.statuses) > 1 {
		a.statuses = a.statuses[1:]
	}
	return &Turn{ID: turn, Status: status}, nil
}

func (a *waitApp) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	if strings.HasPrefix(in.RequestID, "managed-standby-") {
		a.events = append(a.events, "standby-send")
	} else {
		a.events = append(a.events, "business")
	}
	return a.scriptedApp.SendMessage(ctx, in)
}

func (a *waitApp) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	a.events = append(a.events, "create")
	return a.scriptedApp.CreateThread(ctx, in)
}

func (a *waitApp) count(event string) int {
	n := 0
	for _, e := range a.events {
		if e == event {
			n++
		}
	}
	return n
}

func (a *waitApp) index(event string) int {
	for i, e := range a.events {
		if e == event {
			return i
		}
	}
	return -1
}

func waitKit(t *testing.T, wait, poll time.Duration, statuses ...string) (*reconcileKit, *waitApp) {
	t.Helper()
	k := newReconcileKit(t)
	app := &waitApp{scriptedApp: k.host, statuses: statuses}
	k.start.Adapter = app
	k.start.StandbyWait, k.start.StandbyPoll = wait, poll
	return k, app
}

// heldApp is a waitApp whose creation hands over its turn watch: the watch reports turn/completed on the
// completedAfter-th question, and every question and the release are events.
type heldApp struct {
	*waitApp
	completedAfter int
	checks         int
	released       int
	// onWatch runs on every question to the watch.
	onWatch func()
	// afterRelease, when set, is what the host's own record of the turn says once the watch has been let go.
	afterRelease string
}

func (a *heldApp) ReadTurn(ctx context.Context, thread, turn string) (*Turn, error) {
	if a.released > 0 && a.afterRelease != "" {
		a.events = append(a.events, "read")
		return &Turn{ID: turn, Status: a.afterRelease}, nil
	}
	return a.waitApp.ReadTurn(ctx, thread, turn)
}

type testHold struct{ app *heldApp }

func (h testHold) TurnCompleted(string) bool {
	h.app.checks++
	h.app.events = append(h.app.events, "watch")
	if h.app.onWatch != nil {
		h.app.onWatch()
	}
	return h.app.checks >= h.app.completedAfter
}

func (h testHold) Release() {
	h.app.released++
	h.app.events = append(h.app.events, "release")
}

func (a *heldApp) CreateThreadHeld(ctx context.Context, in CreateThreadRequest) (map[string]any, CreationHold, error) {
	receipt, err := a.waitApp.CreateThread(ctx, in)
	return receipt, testHold{app: a}, err
}

func heldKit(t *testing.T, wait, poll time.Duration, completedAfter int) (*reconcileKit, *heldApp) {
	t.Helper()
	k, app := waitKit(t, wait, poll)
	held := &heldApp{waitApp: app, completedAfter: completedAfter}
	k.start.Adapter = held
	return k, held
}

// A standby turn that ends inside the bound is waited for in the call: the business turn follows it and
// the caller never sees standby_incomplete.
func TestStartWaitsForStandbyCompletion(t *testing.T) {
	t.Parallel()
	k, app := waitKit(t, 5*time.Second, 5*time.Millisecond, "inProgress", "inProgress", "inProgress", "completed")
	got := k.run()
	if got["state"] != "admitted" || got["reason"] != nil {
		t.Fatalf("a standby that ends inside the bound is waited for: %v/%v", got["state"], got["reason"])
	}
	if app.reads < 4 {
		t.Fatalf("the turn was read until it ended (4 reads), got %d", app.reads)
	}
	if business := app.index("business"); business < 1 || app.events[business-1] != "read" {
		t.Fatalf("the business turn follows the read that saw the end: %v", app.events)
	}
	k.effects(1, 1)
}

// standby_incomplete is the answer past the bound and never before it: a turn that ends later than half the
// bound is still waited for, and one that never ends is answered once the bound has passed.
func TestStandbyIncompleteOnlyAfterBound(t *testing.T) {
	t.Parallel()
	t.Run("ends inside the bound", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 2*time.Second, 5*time.Millisecond)
		app.doneAfter = 150 * time.Millisecond
		started := time.Now()
		got := k.run()
		if got["state"] != "admitted" || time.Since(started) < 150*time.Millisecond {
			t.Fatalf("waited for the turn that ended after 150ms: %v/%v after %v", got["state"], got["reason"], time.Since(started))
		}
	})
	t.Run("never ends", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 200*time.Millisecond, 10*time.Millisecond)
		started := time.Now()
		got := k.run()
		elapsed := time.Since(started)
		if got["state"] != "incomplete" || got["stage"] != "standby" || got["reason"] != "standby_incomplete" {
			t.Fatalf("a standby past the bound is standby_incomplete: %v/%v/%v", got["state"], got["stage"], got["reason"])
		}
		if elapsed < 200*time.Millisecond {
			t.Fatalf("standby_incomplete came after %v, before the 200ms bound", elapsed)
		}
		if app.reads < 3 {
			t.Fatalf("the turn was read at the poll interval inside the bound: %d reads", app.reads)
		}
		if app.count("business") != 0 || app.count("standby-send") != 0 {
			t.Fatalf("nothing is sent to a child whose standby has not ended: %v", app.events)
		}
	})
}

// The creation watch stays open while the call waits and is let go after the wait, before the business
// guard; the watch's turn/completed is used before the polling read, and the watch is let go on every
// way out of the call.
func TestWatchHeldDuringWait(t *testing.T) {
	t.Parallel()
	t.Run("completed through the watch", func(t *testing.T) {
		t.Parallel()
		k, app := heldKit(t, 5*time.Second, 5*time.Millisecond, 3)
		app.afterRelease = "completed"
		got := k.run()
		if got["state"] != "admitted" {
			t.Fatalf("admitted: %v/%v", got["state"], got["reason"])
		}
		release := app.index("release")
		if app.released != 1 || release < 0 {
			t.Fatalf("let go once: %d (%v)", app.released, app.events)
		}
		// Three questions to the watch, each answered by the poll between them until the watch reports the turn: the host's record is the fallback.
		before := app.events[:release]
		if watches := strings.Count(strings.Join(before, " "), "watch"); watches != 3 {
			t.Fatalf("three questions to the watch before it is let go: %v", app.events)
		}
		if business := app.index("business"); release > business {
			t.Fatalf("the watch is let go after the wait and before the business turn: %v", app.events)
		}
		k.effects(1, 1)
	})
	t.Run("the watch first", func(t *testing.T) {
		t.Parallel()
		k, app := heldKit(t, 5*time.Second, 5*time.Millisecond, 1)
		app.afterRelease = "completed"
		got := k.run()
		if got["state"] != "admitted" || strings.Contains(strings.Join(app.events[:app.index("release")], " "), "read") {
			t.Fatalf("the watch's turn/completed ends the wait with no read of the turn: %v/%v %v", got["state"], got["reason"], app.events)
		}
	})
	t.Run("polling read when the watch reports nothing", func(t *testing.T) {
		t.Parallel()
		k, app := heldKit(t, 5*time.Second, 5*time.Millisecond, 1000)
		app.statuses = []string{"inProgress", "completed"}
		got := k.run()
		if got["state"] != "admitted" || app.count("read") < 2 || app.count("watch") < 2 || app.released != 1 {
			t.Fatalf("the read is the fallback while the watch is silent: %v/%v %v released %d", got["state"], got["reason"], app.events, app.released)
		}
	})
	t.Run("past the bound", func(t *testing.T) {
		t.Parallel()
		k, app := heldKit(t, 100*time.Millisecond, 10*time.Millisecond, 1000)
		got := k.run()
		if got["reason"] != "standby_incomplete" || app.released != 1 {
			t.Fatalf("let go once when the bound passes: %v released %d", got["reason"], app.released)
		}
		for i, event := range app.events {
			if event == "watch" && i > app.index("release") {
				t.Fatalf("let go only after the wait: %v", app.events)
			}
		}
	})
	t.Run("caller cancelled", func(t *testing.T) {
		t.Parallel()
		k, app := heldKit(t, 10*time.Second, 5*time.Millisecond, 1000)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		app.onWatch = cancel // the caller gives up while the wait is under way
		if _, err := k.start.Run(ctx, k.raw); err == nil {
			t.Fatal("a cancelled caller ends the wait with its error")
		}
		if app.released != 1 {
			t.Fatalf("the watch is let go on every way out: %d (%v)", app.released, app.events)
		}
	})
}

// Repeating a request whose standby wait ended once adds nothing: the creation, the standby and the
// business turn happen once, and a repeat answers the same admitted receipt.
func TestReplayAfterWaitIdempotent(t *testing.T) {
	t.Parallel()
	k, app := waitKit(t, 60*time.Millisecond, 10*time.Millisecond)
	first := k.run()
	if first["reason"] != "standby_incomplete" {
		t.Fatalf("the first call ends at the bound: %v/%v", first["state"], first["reason"])
	}
	app.doneAfter = time.Nanosecond
	var admitted map[string]any
	for range 3 {
		admitted = k.run()
		if admitted["state"] != "admitted" {
			t.Fatalf("a repeat after the end of the standby admits: %v/%v", admitted["state"], admitted["reason"])
		}
	}
	if app.count("create") != 1 || app.count("business") != 1 || app.count("standby-send") != 0 {
		t.Fatalf("one creation, one business turn, no second standby: %v", app.events)
	}
	if admitted["requestId"] != first["requestId"] || admitted["childTaskId"] == nil {
		t.Fatalf("the repeat is the same request: %v %v", admitted["requestId"], admitted["childTaskId"])
	}
	k.effects(1, 1)
}

// A standby turn that is still running when the bound passes leaves a registered child: it cannot be archived and
// replaced as an unrecorded one is. The call that finds the turn ended judges it all the same, before any business
// turn: a child the host holds loaded under other settings, with nothing but its standby turn, is held (no business
// send whose failure would stay on the request), and the request goes on once the host has unloaded the child.
func TestTimedOutStandbyIsJudgedWhenItEnds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"model", "mcp"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			k, app := collapseKit(t)
			if kind == "model" {
				app.wrong["t-1"] = true
			} else {
				app.mcpWrong = map[string]bool{"t-1": true}
			}
			k.host.standby = "inProgress"
			first := k.run()
			k.expect(first, "incomplete", "standby_incomplete", "")
			if first["childTaskId"] != "t-1" || first["relationshipId"] == nil || k.host.sent != 0 || len(app.archiveCalls) != 0 {
				t.Fatalf("the first call registers the child and sends nothing: child %v relationship %v sent %d archives %v", first["childTaskId"], first["relationshipId"], k.host.sent, app.archiveCalls)
			}
			k.host.standby = "completed"
			for round := range 3 {
				k.expect(k.run(), "incomplete", "recipient_not_idle", "standby_orphan_held")
				if round == 0 {
					k.restart()
				}
			}
			if k.host.sent != 0 || len(app.archiveCalls) != 0 || len(app.unarchiveCalls) != 0 || app.creates != 1 {
				t.Fatalf("held: nothing sent, archived or created: sent %d archives %v creates %d", k.host.sent, app.archiveCalls, app.creates)
			}
			// The host unloads the child (another client closed it): the resume applies the recorded settings.
			k.host.threads["t-1"].status = "notLoaded"
			app.wrong["t-1"], app.mcpWrong = false, nil
			k.expect(k.run(), "admitted", "", "")
			k.effects(1, 1)
		})
	}
}

// A child that stands as asked and whose standby ended after the first call is admitted by the next one, with one creation and one business turn.
func TestTimedOutStandbyThenCompletedAdmitsOnce(t *testing.T) {
	t.Parallel()
	k, app := collapseKit(t)
	k.host.standby = "inProgress"
	k.expect(k.run(), "incomplete", "standby_incomplete", "")
	k.host.standby = "completed"
	for range 2 {
		k.expect(k.run(), "admitted", "", "")
	}
	if app.creates != 1 || len(app.archiveCalls) != 0 {
		t.Fatalf("one creation, no archive: %d %v", app.creates, app.archiveCalls)
	}
	k.effects(1, 1)
}

// The creation watch proved the end of the standby turn: the proof is kept for the business step, whatever the host's
// own record says once the watch is let go (a turn not listed yet, or listed as still running).
func TestWatchTerminalSurvivesRelease(t *testing.T) {
	t.Parallel()
	for _, after := range []string{"inProgress", "absent"} {
		t.Run(after, func(t *testing.T) {
			t.Parallel()
			k, app := heldKit(t, 10*time.Second, time.Millisecond, 1)
			if after == "absent" {
				k.start.Adapter = &absentAfterReleaseApp{heldApp: app}
			}
			app.afterRelease = "inProgress"
			started := time.Now()
			got := k.run()
			if got["state"] != "admitted" || time.Since(started) > 5*time.Second {
				t.Fatalf("the watch's turn/completed ends the standby for good: %v/%v after %v", got["state"], got["reason"], time.Since(started))
			}
			k.effects(1, 1)
		})
	}
}

// absentAfterReleaseApp lists no standby turn once the creation watch has been let go.
type absentAfterReleaseApp struct{ *heldApp }

func (a *absentAfterReleaseApp) ReadTurn(ctx context.Context, thread, turn string) (*Turn, error) {
	if a.released > 0 {
		return nil, nil
	}
	return a.heldApp.ReadTurn(ctx, thread, turn)
}

// slowReadApp answers ReadTurn after delay, or when its context ends, whichever is first.
type slowReadApp struct {
	*waitApp
	delay time.Duration
}

func (a *slowReadApp) ReadTurn(ctx context.Context, thread, turn string) (*Turn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(a.delay):
	}
	return a.waitApp.ReadTurn(ctx, thread, turn)
}

// The bound is one deadline over the whole wait, the host reads included: a slow read is cut at the bound and the call answers
// standby_incomplete; the caller's own cancellation stays the caller's error.
func TestStandbyBoundCoversReadTurn(t *testing.T) {
	t.Parallel()
	t.Run("the bound cuts a slow read", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 30*time.Millisecond, time.Millisecond)
		k.start.Adapter = &slowReadApp{waitApp: app, delay: 2 * time.Second}
		started := time.Now()
		got := k.run()
		if got["reason"] != "standby_incomplete" || time.Since(started) > time.Second {
			t.Fatalf("a 30ms bound over a 2s read: %v/%v after %v", got["state"], got["reason"], time.Since(started))
		}
	})
	t.Run("the caller's cancellation is not the bound", func(t *testing.T) {
		t.Parallel()
		k, app := waitKit(t, 10*time.Second, time.Millisecond)
		k.start.Adapter = &slowReadApp{waitApp: app, delay: 2 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := k.start.Run(ctx, k.raw); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the caller's deadline is the caller's error: %v", err)
		}
	})
}
