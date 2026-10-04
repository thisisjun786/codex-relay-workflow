package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func TestUnloadedNonProfiledRoleKeepsItsSendPath(t *testing.T) {
	b, host, cwd := mcpBridge(t, true)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	resumed := startReply(cwd)
	resumed.Result["model"], resumed.Result["reasoningEffort"] = parentModel, parentEffort
	host.Respond("thread/resume", resumed)
	in := mcpSend("parent-send", "")
	in.Role, in.Expected["model"], in.Expected["reasoning_effort"] = "parent", parentModel, parentEffort
	receipt, err := b.SendMessageToThread(context.Background(), in)
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 || host.Count("config/read") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
	}
}

func TestUnloadedThreadWithoutProfilePolicyKeepsLegacySend(t *testing.T) {
	b, host := rolesBridge(t)
	mcpHost(host, t.TempDir(), mcpConfigured, true)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	in := mcpSend("legacy-send", "")
	in.Role = ""
	receipt, err := b.SendMessageToThread(context.Background(), in)
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 || host.Count("config/read") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
	}
}

func TestProfileGuardKeepsBusyRefusalAndRequestIdentity(t *testing.T) {
	b, host, _ := mcpBridge(t, true)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	busy, err := b.SendMessageToThread(context.Background(), mcpSend("busy-send", ""))
	if err != nil || pyjson.Map(busy["rpcError"])["code"] != "thread_busy" || host.Count("thread/resume") != 0 {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	first, err := b.SendMessageToThread(context.Background(), mcpSend("profile-less", ""))
	if err != nil || first["status"] != "failed" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	reads := host.Count("thread/read")
	replay, err := b.SendMessageToThread(context.Background(), mcpSend("profile-less", ""))
	if err != nil || replay["replayed"] != true || host.Count("thread/read") != reads || host.Count("thread/resume") != 0 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, hostMethods(host))
	}
	if _, err := b.SendMessageToThread(context.Background(), mcpSend("profile-less", "ui-qa")); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed settings under the same id: %v", err)
	}
	corrected, err := b.SendMessageToThread(context.Background(), mcpSend("corrected-profile", "ui-qa"))
	if err != nil || corrected["status"] != "accepted" || host.Count("turn/start") != 1 {
		t.Fatalf("corrected=%v err=%v", corrected, err)
	}
}
