package appserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-1181: BoundAckOn gives the ack bound to one method alone. A request of another method that is
// answered later than that bound is still answered, and the staged method times out on a held answer
// with its own bound in the error.
func TestBoundAckOn_applies_to_its_method_alone(t *testing.T) {
	host := fakehost.Start(t)
	const bound = 200 * time.Millisecond
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}, Delay: 2 * bound})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("turn/start", fakehost.Reply{Paused: entered, Release: release})
	c, err := Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); close(release) })
	c.BoundAckOn("turn/start", bound)
	if _, err := c.Call(context.Background(), "thread/read", map[string]any{"threadId": "thread-1"}); err != nil {
		t.Fatalf("a method without a stage bound expired on another method's bound: %v", err)
	}
	_, err = c.Call(context.Background(), "turn/start", map[string]any{"threadId": "thread-1"})
	var timeout *PhaseTimeout
	if !errors.As(err, &timeout) || timeout.Phase != "ack" || timeout.Bound != bound {
		t.Fatalf("the staged method did not time out on its own bound: %v", err)
	}
	c.BoundAckOn("turn/start", 0)
	if got := c.ackBound(context.Background(), "turn/start"); got != c.bounds.Ack {
		t.Fatalf("a cleared stage bound still applies: %v", got)
	}
}
