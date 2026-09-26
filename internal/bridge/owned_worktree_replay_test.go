package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func Test_test_lost_responses_replay_after_restart_without_duplicate_artifacts(t *testing.T) {
	for _, stage := range []string{"thread/start", "thread/name/set", "turn/start"} {
		t.Run(stage, func(t *testing.T) {
			host := fakehost.Start(t)
			client, err := appserver.Dial(context.Background(), host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			ledgerPath := filepath.Join(t.TempDir(), "operations.sqlite3")
			store, err := ledger.Open(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			b := New(client, store, executionPolicy())
			input := worktreeInput(t)
			input.Prompt, input.Title = "  exact\ninitial  ", "Retained"
			worktreeHost(host, input.Destination)
			host.Respond("thread/name/set", fakehost.Reply{})
			initial := map[string]any{}
			switch stage {
			case "thread/start":
				initial = worktreeStart(input.Destination)
			case "thread/name/set":
				initial = map[string]any{}
			case "turn/start":
				initial = map[string]any{"turn": map[string]any{"id": "turn-1"}}
			}
			host.Script(stage, fakehost.Reply{Result: initial, Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
			first, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || first["status"] != "outcome_unknown" || first["recoveryRequired"] != true || object(first["worktree"])["initialRevision"] != input.Revision || (stage == "turn/start" && object(first["initialPrompt"])["state"] != "outcome_unknown") || (stage != "turn/start" && object(first["initialPrompt"])["state"] != "not_sent") {
				t.Fatalf("first=%v err=%v", first, err)
			}
			before := host.Requests()
			tracked, err := os.ReadFile(filepath.Join(input.Destination, "tracked"))
			if err != nil {
				t.Fatal(err)
			}
			nextClient, err := appserver.Dial(context.Background(), host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = nextClient.Close() })
			handshake, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := host.WaitCount(handshake, "initialized", 2); err != nil {
				t.Fatalf("new client handshake not recorded: %v", err)
			}
			// Reopen the same SQLite file: a replay must survive a new ledger process.
			nextStore, err := ledger.Open(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = nextStore.Close() })
			restarted := New(nextClient, nextStore, executionPolicy())
			replay, err := restarted.CreateWorktreeThread(context.Background(), input)
			if err != nil || replay["replayed"] != true || !reflect.DeepEqual(replay["worktree"], first["worktree"]) || host.Count(stage) != 1 {
				t.Fatalf("replay=%v err=%v", replay, err)
			}
			if replay["threadId"] != first["threadId"] || (stage != "thread/start" && first["threadId"] != "thread-1") {
				t.Fatalf("thread identity first=%v replay=%v", first, replay)
			}
			retained, err := b.GetOperation(context.Background(), input.RequestID)
			if err != nil || retained["status"] != "outcome_unknown" {
				t.Fatalf("ledger=%v err=%v", retained, err)
			}
			if len(host.Requests()) != len(before)+2 {
				t.Fatalf("unexpected dispatch after restart: %v", host.Requests())
			}
			if data, err := os.ReadFile(filepath.Join(input.Destination, "tracked")); err != nil || string(data) != string(tracked) {
				t.Fatalf("checkout changed: %q %v", data, err)
			}
			if gitAt(t, input.Source, "worktree", "list", "--porcelain") == "" {
				t.Fatal("registered checkout disappeared")
			}
			if strings.Count(gitAt(t, input.Source, "worktree", "list", "--porcelain"), "worktree ") != 2 {
				t.Fatal("duplicate worktree registered")
			}
			input.Prompt = "changed"
			_, err = restarted.CreateWorktreeThread(context.Background(), input)
			if !errors.Is(err, ledger.ErrConflict) || host.Count(stage) != 1 || len(host.Requests()) != len(before)+2 {
				t.Fatalf("conflict=%v calls=%v", err, host.Requests())
			}
			if _, err := os.Stat(filepath.Join(input.Destination, "tracked")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func worktreeStart(destination string) map[string]any {
	return map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": destination}, "cwd": destination, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "runtimeWorkspaceRoots": []any{destination}}
}
