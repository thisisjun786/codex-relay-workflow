package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// interruptedWait bounds only a step that is already under way. The worktree is made with git
// subprocesses and the ledger is written before the stage under test is sent, so a bound of a few
// seconds taken from the start of the call failed on a loaded host (CRW-1181). Nothing here is
// expected to take this long; it only turns a hang into a failure.
const interruptedWait = 90 * time.Second

// interruptedAck is the ack bound of the timeout cases. It is the interruption under test, so it
// is given to the stage under test alone (BoundAckOn on the real client, so the call keeps the
// client's subscription watch path): the client would otherwise apply it to every
// request of the call, and thread/start before a turn/start stage expired on it under load, so the
// stage was never sent (CRW-1181). The held answer outlasts any bound, so the case takes this long
// and no longer.
const interruptedAck = time.Second

func Test_test_interrupted_dispatch_is_retained_and_never_repeated(t *testing.T) {
	for _, c := range []struct {
		stage, interruption string
		slowPrior           bool
	}{
		{"thread/start", "cancel", false}, {"thread/start", "timeout", false},
		{"turn/start", "cancel", false}, {"turn/start", "timeout", false},
		// The earlier thread/start answers later than the bound the stage is given: the bound is the stage's alone, so the
		// stage is still sent, times out on its own held answer and is never repeated.
		{"turn/start", "timeout", true},
	} {
		stage, interruption := c.stage, c.interruption
		name := stage + "/" + interruption
		if c.slowPrior {
			name += "/after a slow thread/start"
		}
		t.Run(name, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Prompt = "READY"
			worktreeHost(host, input.Destination)
			paused, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			response := fakehost.Reply{Paused: paused, Release: release}
			if stage == "thread/start" {
				response.Result = worktreeStart(input.Destination)
			} else {
				response.Result = map[string]any{"turn": map[string]any{"id": "turn-1"}}
			}
			host.Script(stage, response)
			if c.slowPrior {
				host.Script("thread/start", fakehost.Reply{Result: worktreeStart(input.Destination), Delay: 2 * interruptedAck})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if interruption == "timeout" {
				b.RPC.(*appserver.Client).BoundAckOn(stage, interruptedAck)
			}
			result := make(chan map[string]any, 1)
			go func() { receipt, _ := b.CreateWorktreeThread(ctx, input); result <- receipt }()
			select {
			case <-paused:
			case <-time.After(interruptedWait):
				t.Fatal("stage did not reach host")
			}
			if interruption == "cancel" {
				cancel()
			}
			select {
			case receipt := <-result:
				if receipt["status"] != "outcome_unknown" {
					t.Fatalf("receipt=%v", receipt)
				}
			case <-time.After(interruptedWait):
				t.Fatal("interrupted launch hung")
			}
			// watchSubscription pins and finishes the created thread's turn watch only for the real client, so a
			// double standing in for b.RPC would run the interrupted call with no watch at all (CRW-1181 d1).
			if _, ok := b.RPC.(*appserver.Client); !ok {
				t.Fatalf("b.RPC is %T: the call did not run on the real client's subscription watch path", b.RPC)
			}
			stored, err := b.GetOperation(context.Background(), input.RequestID)
			if err != nil || stored["status"] != "outcome_unknown" || pyjson.Map(stored["worktree"])["state"] != "created" {
				t.Fatalf("stored=%v err=%v", stored, err)
			}
			before := len(host.Requests())
			replay, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || replay["replayed"] != true || len(host.Requests()) != before || host.Count(stage) != 1 {
				t.Fatalf("replay=%v err=%v", replay, err)
			}
			close(release)
		})
	}
}
