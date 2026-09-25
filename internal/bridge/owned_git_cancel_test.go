package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/worktrees"
)

func Test_test_cancellation_after_git_creation_retains_checkout_without_starting_task(t *testing.T) {
	for _, stage := range []string{"worktree", "checkout-index"} {
		t.Run(stage, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Prompt = "WITHHOLD"
			arrived, release := make(chan struct{}), make(chan struct{})
			worktrees.AfterGit = func(command string) {
				if command == stage {
					close(arrived)
					<-release
				}
			}
			t.Cleanup(func() {
				worktrees.AfterGit = nil
				select {
				case <-release:
				default:
					close(release)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan map[string]any, 1)
			go func() { receipt, _ := b.CreateWorktreeThread(ctx, input); result <- receipt }()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("Git stage not reached")
			}
			cancel()
			close(release)
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("launch did not stop")
			}
			if _, err := os.Stat(filepath.Join(input.Destination)); err != nil {
				t.Fatalf("checkout not retained: %v", err)
			}
			receipt, err := b.GetOperation(context.Background(), input.RequestID)
			if err != nil || receipt["status"] != "outcome_unknown" || object(receipt["worktree"])["requestedRevision"] != input.Revision || host.Count("thread/start") != 0 {
				t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
			}
			if stage == "worktree" {
				if receipt["phase"] != "creating_worktree" {
					t.Fatalf("phase=%v", receipt["phase"])
				}
				if _, err := os.Stat(filepath.Join(input.Destination, "tracked")); !os.IsNotExist(err) {
					t.Fatalf("tracked unexpectedly present: %v", err)
				}
			} else {
				if receipt["phase"] != "checking_out_worktree" {
					t.Fatalf("phase=%v", receipt["phase"])
				}
				data, err := os.ReadFile(filepath.Join(input.Destination, "tracked"))
				if err != nil || string(data) != "base\n" {
					t.Fatalf("tracked=%q err=%v", data, err)
				}
			}
			replay, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || replay["replayed"] != true || len(hostMethods(host)) != 0 {
				t.Fatalf("replay=%v err=%v", replay, err)
			}
		})
	}
}
