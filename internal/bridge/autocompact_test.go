package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// autoCompactPolicy is one role whose pair carries the limit this issue recommends.
const autoCompactPolicy = `{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high","autoCompactTokenLimit":550000}}}`

// autoCompactRequest returns the parameters of the first request that used method.
func autoCompactRequest(t *testing.T, host *fakehost.Server, method string) (map[string]any, string) {
	t.Helper()
	for _, request := range host.Requests() {
		if request.Method != method {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		return params, string(request.Params)
	}
	t.Fatalf("no %s request", method)
	return nil, ""
}

// autoCompactConfig is the config object the request carried, which is where the limit travels.
func autoCompactConfig(t *testing.T, host *fakehost.Server, method string) map[string]any {
	t.Helper()
	params, _ := autoCompactRequest(t, host, method)
	return pyjson.Map(params["config"])
}

// autoCompactReceiptSettings is the settings receipt of a mutation, whose unobservable list is where
// a transmitted-but-unreported setting is recorded.
func autoCompactReceiptSettings(t *testing.T, receipt map[string]any) map[string]any {
	t.Helper()
	section := pyjson.Map(receipt["settings"])
	if section == nil {
		t.Fatalf("receipt has no settings section: %v", receipt)
	}
	return section
}

// autoCompactListed is whether a receipt list names the field, whichever slice type it was built as.
func autoCompactListed(value any, field string) bool {
	switch list := value.(type) {
	case []string:
		return slices.Contains(list, field)
	case []any:
		return slices.Contains(list, any(field))
	}
	return false
}

// A creation under a pair that declares the limit sends it in thread/start's config, as an integer,
// and records it as sent but not observable.
func TestCreateThreadSendsThePairsAutoCompactLimit(t *testing.T) {
	b, host := policyBridge(t, autoCompactPolicy)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := createInput(cwd, "auto-compact-create")
	input.Role = "child"
	receipt, err := b.CreateThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if got := autoCompactConfig(t, host, "thread/start")[settings.AutoCompactTokenLimitKey]; got != float64(550000) {
		t.Fatalf("thread/start config = %v", autoCompactConfig(t, host, "thread/start"))
	}
	if _, raw := autoCompactRequest(t, host, "thread/start"); !strings.Contains(raw, `"model_auto_compact_token_limit":550000`) {
		t.Fatalf("the limit did not leave as an integer: %s", raw)
	}
	section := autoCompactReceiptSettings(t, receipt)
	if !autoCompactListed(section["unobservable"], settings.AutoCompactTokenLimitKey) {
		t.Fatalf("settings receipt = %v", section)
	}
}

// A resume under the same pair sends the limit again, and a host answer that says nothing about it
// neither withholds the message nor fails the receipt.
func TestSendMessageSendsThePairsAutoCompactLimitAndStillDelivers(t *testing.T) {
	b, host := policyBridge(t, autoCompactPolicy)
	cwd := t.TempDir()
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "auto-compact-send", ThreadID: "thread-1", Message: "work", Role: "child", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
		t.Fatalf("the host's silence about the limit withheld the message: receipt=%v err=%v", receipt, err)
	}
	if got := autoCompactConfig(t, host, "thread/resume")[settings.AutoCompactTokenLimitKey]; got != float64(550000) {
		t.Fatalf("thread/resume config = %v", autoCompactConfig(t, host, "thread/resume"))
	}
	section := autoCompactReceiptSettings(t, receipt)
	if !autoCompactListed(section["unobservable"], settings.AutoCompactTokenLimitKey) {
		t.Fatalf("settings receipt = %v", section)
	}
}

// A policy that declares no limit sends no such key and reports none, so every host keeps exactly
// the behaviour it had.
func TestAPolicyWithoutTheLimitSendsNoAutoCompactKey(t *testing.T) {
	b, host := policyBridge(t, `{"roles":{"child":{"model":"explicit-model","reasoningEffort":"high"}}}`)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := createInput(cwd, "auto-compact-absent")
	input.Role = "child"
	receipt, err := b.CreateThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if _, present := autoCompactConfig(t, host, "thread/start")[settings.AutoCompactTokenLimitKey]; present {
		t.Fatalf("thread/start config = %v", autoCompactConfig(t, host, "thread/start"))
	}
	if autoCompactListed(autoCompactReceiptSettings(t, receipt)["unobservable"], settings.AutoCompactTokenLimitKey) {
		t.Fatalf("settings receipt = %v", receipt["settings"])
	}
}
