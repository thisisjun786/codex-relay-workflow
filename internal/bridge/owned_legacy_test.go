package bridge

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"os"
	"path/filepath"
	"testing"
)

func Test_test_old_canonical_cwd_fingerprint_still_replays_without_directory(t *testing.T) {
	b, host := testBridge(t)
	cwd := filepath.Join(t.TempDir(), "removed-before-upgrade")
	params := map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": nil, "title": nil}
	_, r, err := b.Ledger.Begin(context.Background(), "old-create", "create_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	r["status"] = "accepted"
	r["threadId"] = "retained-thread"
	if _, err = b.Ledger.Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	replayed, err := b.CreateThread(context.Background(), CreateThread{RequestID: "old-create", CWD: cwd, Sandbox: "read-only"})
	if err != nil || replayed["replayed"] != true || replayed["threadId"] != "retained-thread" || len(hostMethods(host)) != 0 {
		t.Fatalf("replay=%v err=%v", replayed, err)
	}
}
func Test_test_legacy_cwd_symlink_replay_uses_old_fingerprint_only_for_legacy_receipts(t *testing.T) {
	b, host := testBridge(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"cwd": target, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": nil, "title": nil}
	_, r, err := b.Ledger.Begin(context.Background(), "legacy", "create_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	delete(r, "fingerprintVersion")
	r["status"] = "accepted"
	r["threadId"] = "legacy-thread"
	if _, err = b.Ledger.Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	replayed, err := b.CreateThread(context.Background(), CreateThread{RequestID: "legacy", CWD: alias, Sandbox: "read-only"})
	if err != nil || replayed["replayed"] != true || replayed["threadId"] != "legacy-thread" || len(hostMethods(host)) != 0 {
		t.Fatalf("replay=%v err=%v", replayed, err)
	}
	changed := CreateThread{RequestID: "legacy", CWD: alias, Sandbox: "read-only", Prompt: "changed"}
	_, err = b.CreateThread(context.Background(), changed)
	if !errors.Is(err, ledger.ErrConflict) || host.Count("thread/start") != 0 {
		t.Fatalf("changed legacy=%v calls=%v", err, host.Requests())
	}
	host.Respond("thread/start", startReply(target))
	if _, err = b.CreateThread(context.Background(), createInput(target, "new")); err != nil {
		t.Fatal(err)
	}
	_, err = b.CreateThread(context.Background(), createInput(alias, "new"))
	if !errors.Is(err, ledger.ErrConflict) || host.Count("thread/start") != 1 {
		t.Fatalf("error=%v calls=%v", err, host.Requests())
	}
}
func Test_test_lost_creation_response_is_never_retried(t *testing.T) {
	b, host := testBridge(t)
	in := createInput(t.TempDir(), "lost")
	in.Prompt = "hello"
	host.Respond("thread/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	first, err := b.CreateThread(context.Background(), in)
	if err != nil || first["status"] != "outcome_unknown" || first["threadId"] != nil {
		t.Fatalf("first=%v err=%v", first, err)
	}
	again, err := b.CreateThread(context.Background(), in)
	if err != nil || again["replayed"] != true || again["status"] != "outcome_unknown" || host.Count("thread/start") != 1 || host.Count("turn/start") != 0 {
		t.Fatalf("again=%v err=%v calls=%v", again, err, host.Requests())
	}
}
func Test_test_partial_failure_retains_created_id(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("thread/name/set", fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "name rejected"}})
	in := createInput(cwd, "partial")
	in.Prompt = "hello"
	in.Title = "Demo"
	r, err := b.CreateThread(context.Background(), in)
	if err != nil || r["status"] != "failed" || r["threadId"] != "thread-1" || host.Count("turn/start") != 0 {
		t.Fatalf("r=%v err=%v", r, err)
	}
	stored, err := b.GetOperation(context.Background(), "partial")
	if err != nil || stored["threadId"] != "thread-1" {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
}
