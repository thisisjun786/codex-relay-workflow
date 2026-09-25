package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_interrupted_dispatch_is_retained_and_never_repeated(t *testing.T) {
	for _, stage := range []string{"thread/start", "turn/start"} {
		for _, interruption := range []string{"cancel", "timeout"} {
			t.Run(stage+"/"+interruption, func(t *testing.T) {
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
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if interruption == "timeout" {
					b.RPC.(*appserver.Client).BoundAck(100 * time.Millisecond)
				}
				result := make(chan map[string]any, 1)
				go func() { receipt, _ := b.CreateWorktreeThread(ctx, input); result <- receipt }()
				select {
				case <-paused:
				case <-time.After(5 * time.Second):
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
				case <-time.After(5 * time.Second):
					t.Fatal("interrupted launch hung")
				}
				stored, err := b.GetOperation(context.Background(), input.RequestID)
				if err != nil || stored["status"] != "outcome_unknown" || object(stored["worktree"])["state"] != "created" {
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
}
