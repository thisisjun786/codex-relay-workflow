package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"testing"
)

func Test_test_concurrent_requests_cannot_adopt_same_destination(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	worktreeHost(host, input.Destination)
	type outcome struct {
		id      string
		receipt map[string]any
		err     error
	}
	// A pause after reservation lets the other ID contend while the first request is in flight.
	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	host.Script("thread/start", fakehost.Reply{Result: worktreeStart(input.Destination), Paused: paused, Release: release})
	results := make(chan outcome, 3)
	var group sync.WaitGroup
	group.Go(func() {
		request := input
		request.RequestID = "first"
		receipt, err := b.CreateWorktreeThread(context.Background(), request)
		results <- outcome{"first", receipt, err}
	})
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("first did not reserve destination")
	}
	// Python's mutation lock serializes one bridge, so exclusivity of the reservation itself is
	// proved by an independent bridge (own ledger and host) contending while the first is paused.
	other, otherHost := testBridge(t)
	worktreeHost(otherHost, input.Destination)
	rival := input
	rival.RequestID = "rival"
	rivalReceipt, err := other.CreateWorktreeThread(context.Background(), rival)
	if err != nil || rivalReceipt["status"] != "failed" || !strings.Contains(text(rivalReceipt["error"]), "destination must be absent") || otherHost.Count("thread/start") != 0 {
		t.Fatalf("rival adopted a reserved destination: receipt=%v err=%v calls=%v", rivalReceipt, err, otherHost.Requests())
	}
	for _, id := range []string{"first", "second"} {
		group.Go(func() {
			request := input
			request.RequestID = id
			receipt, err := b.CreateWorktreeThread(context.Background(), request)
			results <- outcome{id, receipt, err}
		})
	}
	close(release)
	group.Wait()
	close(results)
	accepted, replayed, failed := 0, 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("%s: %v", result.id, result.err)
		}
		switch result.receipt["status"] {
		case "accepted":
			if result.receipt["replayed"] == true {
				replayed++
			} else {
				accepted++
			}
		case "failed":
			if result.id == "first" {
				t.Fatalf("same id failed: %v", result.receipt)
			}
			failed++
		default:
			t.Fatalf("receipt=%v", result.receipt)
		}
	}
	if accepted != 1 || replayed != 1 || failed != 1 || host.Count("thread/start") != 1 {
		t.Fatalf("accepted=%d replayed=%d failed=%d calls=%v", accepted, replayed, failed, host.Requests())
	}
}

func Test_test_inherited_git_environment_cannot_redirect_checkout(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	worktreeHost(host, input.Destination)
	t.Setenv("GIT_DIR", "wrong-git")
	t.Setenv("GIT_WORK_TREE", "wrong-tree")
	t.Setenv("GIT_INDEX_FILE", "wrong-index")
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	data, err := os.ReadFile(filepath.Join(input.Destination, "tracked"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("checkout=%q err=%v", data, err)
	}
	for _, name := range []string{"wrong-tree", "wrong-index"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(input.Source), name)); !os.IsNotExist(err) {
			t.Fatalf("%s created: %v", name, err)
		}
	}
}

func Test_test_destination_conditional_filters_are_disabled_before_checkout(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	if err := os.WriteFile(filepath.Join(input.Source, ".gitattributes"), []byte("tracked filter=conditional\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, input.Source, "add", ".gitattributes")
	gitAt(t, input.Source, "commit", "-m", "conditional attributes")
	input.Revision = gitAt(t, input.Source, "rev-parse", "HEAD")
	marker := filepath.Join(filepath.Dir(input.Source), "unexpected-filter-effect")
	config := filepath.Join(filepath.Dir(input.Source), "conditional-config")
	if err := os.WriteFile(config, []byte("[filter \"conditional\"]\n    smudge = touch "+marker+"; cat\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, input.Source, "config", "includeIf.gitdir:"+input.Source+"/.git/worktrees/.path", config)
	worktreeHost(host, input.Destination)
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("conditional filter ran: %v", err)
	}
}

func Test_test_git_hooks_filters_and_fsmonitor_are_not_run(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	marker := filepath.Join(filepath.Dir(input.Source), "unexpected-effect")
	filterMarker := filepath.Join(filepath.Dir(input.Source), "unexpected-filter-effect")
	hooks := filepath.Join(input.Source, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Source, ".gitattributes"), []byte("tracked filter=conditional\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, input.Source, "add", ".gitattributes")
	gitAt(t, input.Source, "commit", "-m", "conditional attributes")
	input.Revision = gitAt(t, input.Source, "rev-parse", "HEAD")
	gitAt(t, input.Source, "config", "core.fsmonitor", "touch "+marker)
	config := filepath.Join(filepath.Dir(input.Source), "conditional-config")
	if err := os.WriteFile(config, []byte("[filter \"conditional\"]\n    smudge = touch "+filterMarker+"; cat\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, input.Source, "config", "includeIf.gitdir:"+input.Source+"/.git/worktrees/.path", config)
	worktreeHost(host, input.Destination)
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("hook or fsmonitor ran: %v", err)
	}
	if _, err := os.Lstat(filterMarker); !os.IsNotExist(err) {
		t.Fatalf("conditional filter ran: %v", err)
	}
}
