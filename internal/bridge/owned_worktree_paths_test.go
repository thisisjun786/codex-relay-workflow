package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_launch_preserves_trailing_whitespace_in_paths(t *testing.T) {
	for _, field := range []string{"source", "destination"} {
		for _, suffix := range []string{" ", "\t", "\n\n"} {
			t.Run(field+strings.ReplaceAll(suffix, "\n", "newline"), func(t *testing.T) {
				b, host := testBridge(t)
				input := worktreeInput(t)
				// Python's repository fixture leaves the source staged and then edited again.
				if err := os.WriteFile(filepath.Join(input.Source, "tracked"), []byte("staged\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitAt(t, input.Source, "add", "tracked")
				if err := os.WriteFile(filepath.Join(input.Source, "tracked"), []byte("unstaged\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if field == "source" {
					next := input.Source + suffix
					if err := os.Rename(input.Source, next); err != nil {
						t.Fatal(err)
					}
					input.Source = next
				} else {
					input.Destination += suffix
				}
				worktreeHost(host, input.Destination)
				input.Prompt = "READY"
				receipt, err := b.CreateWorktreeThread(context.Background(), input)
				if err != nil || receipt["status"] != "accepted" || object(receipt["worktree"])["sourceRepository"] != input.Source || object(receipt["worktree"])["checkout"] != input.Destination || object(receipt["creation"])["cwd"] != input.Destination || object(receipt["checkoutBeforeDispatch"])["checkout"] != input.Destination {
					t.Fatalf("receipt=%v err=%v", receipt, err)
				}
				if data, err := os.ReadFile(filepath.Join(input.Destination, "tracked")); err != nil || string(data) != "base\n" {
					t.Fatalf("tracked=%q err=%v", data, err)
				}
				if data, err := os.ReadFile(filepath.Join(input.Source, "tracked")); err != nil || string(data) != "unstaged\n" {
					t.Fatalf("source tracked=%q err=%v", data, err)
				}
				host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
				host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "items": []any{map[string]any{"text": "READY"}}}}}})
				host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"text": "READY"}}}})
				if sent := hostParams(t, host, "turn/start"); host.Count("turn/start") != 1 || object(sent["input"].([]any)[0])["text"] != "READY" {
					t.Fatalf("turn/start=%v", host.Requests())
				}
				read, err := b.ReadThread(context.Background(), text(receipt["threadId"]), 20, nil, 4000)
				if err != nil || len(object(read["turnsPage"])["data"].([]any)) != 1 || object(firstTurn(t, read)["items"].([]any)[0])["text"] != "READY" {
					t.Fatalf("read=%v err=%v", read, err)
				}
				replay, err := b.CreateWorktreeThread(context.Background(), input)
				if err != nil || replay["replayed"] != true || replay["threadId"] != receipt["threadId"] || host.Count("thread/start") != 1 {
					t.Fatalf("replay=%v err=%v", replay, err)
				}
			})
		}
	}
}
