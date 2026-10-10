package appserver

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func TestCall_retires_stalled_transmit_inside_its_bound(t *testing.T) {
	// Given: a completed handshake and a write which cannot drain until cancelled.
	host := fakehost.Start(t)
	entered := make(chan struct{})
	// The handshake runs under generous bounds: the short transmit bound is the subject of the call
	// below, and the two handshake writes before it are setup a loaded host can stretch (CRW-1161).
	bounds := PhaseBounds{Establish: 30 * time.Second, Transmit: 30 * time.Second, Ack: 30 * time.Second}
	client := New(host.SocketPath, bounds)
	defer client.Close()
	if err := client.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	bounds.Transmit = 40 * time.Millisecond
	client.bounds = bounds
	client.mu.Lock()
	old := client.conn
	client.mu.Unlock()
	client.writeFrame = func(_ *websocket.Conn, ctx context.Context, _ websocket.MessageType, _ []byte) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	// When: transmit expires, without waiting on the peer's close handshake.
	started := time.Now()
	_, err := client.Call(context.Background(), "thread/read", map[string]any{})
	elapsed := time.Since(started)
	// Then: the caller owns its transmit bound and the partial stream cannot be reused.
	select {
	case <-entered:
	default:
		t.Fatal("write was not attempted")
	}
	var phase *PhaseTimeout
	if !errors.As(err, &phase) || phase.Phase != "transmit" || phase.Method != "thread/read" {
		t.Fatalf("wrong transmit failure: %v", err)
	}
	// The transmit bound is 40 ms and the error above is its phase timeout; the 1 s is a regression ceiling
	// (25 times the bound, and below the 2 s close handshake budget a transmit that waited on the peer's
	// close would take), with room for a loaded host.
	if elapsed >= time.Second {
		t.Fatalf("transmit took %s against %s bound (ceiling 1s)", elapsed, bounds.Transmit)
	}
	client.mu.Lock()
	current := client.conn
	client.mu.Unlock()
	if current == old {
		t.Fatal("stalled connection remained current")
	}
	if host.Count("thread/read") != 0 {
		t.Fatal("partial request was delivered or retried")
	}
}

func TestClose_bounds_stalled_handshake_to_two_seconds(t *testing.T) {
	// Given: a real connection (dialled outside the bubble, so the host's sockets do not hold the virtual clock still)
	// whose close handshake never returns until released.
	host := fakehost.Start(t)
	client, err := Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	// Virtual time: the budget is read off the clock the timer runs on, not off a host that may be loaded, so it is
	// asserted exactly (a 60-second close timeout, or five seconds, or one, moves it).
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		done := make(chan struct{})
		client.closeFrame = func(conn *websocket.Conn) error {
			defer close(done)
			<-release
			return conn.Close(websocket.StatusNormalClosure, "")
		}
		defer func() { close(release); <-done }()
		started := time.Now()
		result := make(chan error, 1)
		// When
		go func() { result <- client.Close() }()
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		// Then: the wait is still on one nanosecond short of the budget, and over at the budget.
		select {
		case err := <-result:
			t.Fatalf("close returned %s short of its two-second budget: %v", time.Nanosecond, err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-result:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("close error: %v", err)
			}
		default:
			t.Fatalf("close still waiting at its budget, after %s", time.Since(started))
		}
		if elapsed := time.Since(started); elapsed != 2*time.Second {
			t.Fatalf("close bound: %s", elapsed)
		}
	})
}

func TestReceive_fails_only_requests_owned_by_retired_reader(t *testing.T) {
	// Given: two pending requests carried by different connections.
	old := &websocket.Conn{}
	replacement := &websocket.Conn{}
	mine := make(chan outcome, 1)
	theirs := make(chan outcome, 1)
	client := &Client{pending: map[string]pending{"old": {conn: old, done: mine}, "new": {conn: replacement, done: theirs}}, conn: replacement}
	// When: the old reader unwinds after its connection fails.
	// Its finalizer is exercised with an already-closed connection, not a wall-clock race.
	client.failReader(old, errors.New("retired"))
	// Then: the old request fails; the replacement remains live and pending.
	select {
	case result := <-mine:
		if result.err == nil {
			t.Fatal("old request succeeded")
		}
	default:
		t.Fatal("old request not failed")
	}
	select {
	case <-theirs:
		t.Fatal("replacement request failed")
	default:
	}
	if client.conn != replacement {
		t.Fatal("old reader retired replacement")
	}
}
