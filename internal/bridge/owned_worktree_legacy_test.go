package bridge

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

func Test_test_a_retained_receipt_survives_validation_this_version_added(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.RequestID, input.Model, input.Effort = "legacy-worktree", "", ""
	input.Policy = map[string]any{"type": "readOnly", "networkAccess": true}
	params := map[string]any{"source_repository": input.Source, "starting_revision": input.Revision, "destination": input.Destination, "worktree_mode": input.Mode, "sandbox": input.Sandbox, "expected_sandbox_policy": input.Policy, "prompt": nil, "title": nil, "model": nil, "reasoning_effort": nil, "app_server_project_id": nil}
	_, receipt, err := b.Ledger.Begin(context.Background(), input.RequestID, "create_worktree_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt["status"], receipt["threadId"], receipt["recoveryRequired"] = "outcome_unknown", "older-thread", true
	if _, err := b.Ledger.Save(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	replay, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || replay["threadId"] != "older-thread" || replay["recoveryRequired"] != true {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	input.RequestID, input.Model, input.Effort = "fresh-worktree", "explicit-model", "high"
	_, err = b.CreateWorktreeThread(context.Background(), input)
	var refused *settings.UntransmittableError
	if !errors.As(err, &refused) || refused.Code() != settings.Untransmittable || host.Count("thread/start") != 0 {
		t.Fatalf("fresh error=%v calls=%v", err, host.Requests())
	}
	if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
		t.Fatalf("checkout created: %v", err)
	}
}
