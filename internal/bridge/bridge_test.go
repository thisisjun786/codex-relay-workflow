package bridge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func testBridge(t *testing.T) (*Bridge, *fakehost.Server) {
	t.Helper()
	host := fakehost.Start(t)
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(client, store, executionPolicy()), host
}
func executionPolicy() execution.Policy { return execution.Policy{} }
func Test_CreateThread_replays_receipt_without_second_dispatch(t *testing.T) {
	// Given
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	in := CreateThread{RequestID: "stable", CWD: cwd, Prompt: "hello", Sandbox: "read-only", Model: "explicit-model", Effort: "high"}
	// When
	first, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Then
	if first["status"] != "accepted" || first["turnId"] != "turn-1" || second["replayed"] != true || host.Count("thread/start") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("first=%v replay=%v calls=%v", first, second, host.Requests())
	}
}
func Test_test_a_creation_without_a_stated_pair_sends_nothing(t *testing.T) {
	for _, scenario := range []struct{ model, effort, code, field string }{
		{"", "high", execution.Missing, "model"},
		{"explicit-model", "", execution.Missing, "reasoning_effort"},
		{"   ", "high", execution.Invalid, "model"},
		{"explicit-model", "   ", execution.Invalid, "reasoning_effort"},
	} {
		t.Run(scenario.field+scenario.model+scenario.effort, func(t *testing.T) {
			b, host := testBridge(t)
			_, err := b.CreateThread(context.Background(), CreateThread{RequestID: "missing", CWD: t.TempDir(), Sandbox: "read-only", Model: scenario.model, Effort: scenario.effort})
			var refusal *execution.Refusal
			if !errors.As(err, &refusal) || refusal.Code != scenario.code || refusal.Field != scenario.field || len(hostMethods(host)) != 0 {
				t.Fatalf("error=%v requests=%v", err, host.Requests())
			}
		})
	}
}
func Test_test_a_refusal_leaves_the_request_id_usable(t *testing.T) {
	// Given
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	in := CreateThread{RequestID: "corrected", CWD: cwd, Sandbox: "read-only", Prompt: "work"}
	// When
	_, refused := b.CreateThread(context.Background(), in)
	_, missing := b.GetOperation(context.Background(), "corrected")
	in.Model, in.Effort = "explicit-model", "high"
	accepted, err := b.CreateThread(context.Background(), in)
	// Then
	var denial *execution.Refusal
	if !errors.As(refused, &denial) || denial.Code != execution.Missing || missing == nil || err != nil || accepted["status"] != "accepted" || host.Count("thread/start") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("refused=%v accepted=%v error=%v calls=%v", refused, accepted, err, host.Requests())
	}
}

func Test_test_a_resume_without_a_full_pair_never_reads_or_resumes_the_thread(t *testing.T) {
	for _, settings := range []map[string]any{nil, {}, {"model": "explicit-model"}, {"reasoning_effort": "high"}, {"model": "explicit-model", "reasoning_effort": " "}} {
		b, host := testBridge(t)
		_, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "missing-resume", ThreadID: "thread-1", Message: "hello", Expected: settings})
		var refusal *execution.Refusal
		if !errors.As(err, &refusal) || len(hostMethods(host)) != 0 {
			t.Fatalf("settings=%v error=%v requests=%v", settings, err, host.Requests())
		}
	}
}

func Test_test_a_worktree_launch_without_a_stated_pair_creates_nothing(t *testing.T) {
	for _, absent := range []string{"model", "reasoning_effort"} {
		t.Run(absent, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			if absent == "model" {
				input.Model = ""
			} else {
				input.Effort = ""
			}
			_, err := b.CreateWorktreeThread(context.Background(), input)
			var refusal *execution.Refusal
			if !errors.As(err, &refusal) || refusal.Code != execution.Missing || refusal.Field != absent || len(hostMethods(host)) != 0 {
				t.Fatalf("error=%v requests=%v", err, host.Requests())
			}
			if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
				t.Fatalf("worktree created: %v", err)
			}
			if strings.Count(gitAt(t, input.Source, "worktree", "list", "--porcelain"), "worktree ") != 1 {
				t.Fatal("Git worktree was registered")
			}
		})
	}
}

func Test_SteerThread_refuses_inactive_thread_without_sending(t *testing.T) {
	// Given
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	// When
	receipt, err := b.SteerThread(context.Background(), "steer-1", "thread-1", "turn-1", "hello")
	// Then
	if err != nil || receipt["status"] != "failed" || host.Count("turn/steer") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}
func Test_CreateWorktreeThread_creates_locked_detached_checkout_when_host_accepts(t *testing.T) {
	// Given
	b, host := testBridge(t)
	root, err := os.MkdirTemp("/dev/shm", "crw-bridge-worktree-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source := filepath.Join(root, "source")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}} {
		cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	if err = os.WriteFile(filepath.Join(source, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked"}, {"commit", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	output, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(output))
	destination := filepath.Join(root, "isolated")
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": destination}, "cwd": destination, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "runtimeWorkspaceRoots": []any{destination}}})
	in := CreateWorktree{RequestID: "worktree-1", Source: source, Revision: revision, Destination: destination, Mode: "bridge-managed-retained", Sandbox: "read-only", Policy: map[string]any{"type": "readOnly", "networkAccess": false}, Model: "explicit-model", Effort: "high"}
	// When
	receipt, err := b.CreateWorktreeThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Then
	if receipt["status"] != "accepted" || receipt["phase"] != "complete" || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v calls=%v", receipt, host.Requests())
	}
	stat, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0700 {
		t.Fatalf("mode %o", stat.Mode().Perm())
	}
	checkout, err := os.ReadFile(filepath.Join(destination, "tracked"))
	if err != nil || string(checkout) != "base\n" {
		t.Fatalf("checkout=%q err=%v", checkout, err)
	}
	common, err := exec.Command("git", "-C", destination, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(strings.TrimSpace(string(common)), "worktrees", "isolated", "locked")); err != nil {
		t.Fatalf("worktree not locked: %v", err)
	}
}
func Test_CreateWorktreeThread_refuses_nested_destination_before_git_effect(t *testing.T) {
	// Given
	b, host := testBridge(t)
	source, err := os.MkdirTemp("/dev/shm", "crw-bridge-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	cmd := exec.Command("git", "-C", source, "init")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	destination := filepath.Join(source, "nested")
	// When
	receipt, err := b.CreateWorktreeThread(context.Background(), CreateWorktree{RequestID: "nested", Source: source, Revision: "0000000000000000000000000000000000000000", Destination: destination, Mode: "bridge-managed-retained", Sandbox: "read-only", Policy: map[string]any{"type": "readOnly", "networkAccess": false}, Model: "explicit-model", Effort: "high"})
	// Then
	if err != nil || receipt["status"] != "failed" || host.Count("thread/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if _, err = os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination touched: %v", err)
	}
}
