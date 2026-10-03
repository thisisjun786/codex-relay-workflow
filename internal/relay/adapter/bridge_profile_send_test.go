package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fake remembers the first load and ignores later overrides while loaded,
// reproducing the host behavior that can strand the next relay delivery.
type profileLoadState struct {
	mu     sync.Mutex
	loaded bool
	config map[string]any
}

type profileSendFixture struct {
	bridge *bridge.Bridge
	relay  *Adapter
	host   *fakehost.Server
	client *appserver.Client
	record *delivery.TaskSettings
}

func newProfileSendFixture(t *testing.T) profileSendFixture {
	t.Helper()
	host := fakehost.Start(t)
	state := &profileLoadState{}
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		state.mu.Lock()
		defer state.mu.Unlock()
		status := "notLoaded"
		if state.loaded {
			status = "idle"
		}
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}}}}
	})
	host.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.loaded {
			var params map[string]any
			if err := json.Unmarshal(raw, &params); err != nil {
				t.Errorf("resume parameters: %v", err)
			}
			state.config = pyjson.Map(params["config"])
			state.loaded = true
		}
		return fakehost.Reply{Result: resume()}
	})
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{
		"gemini_notebook": map[string]any{}, "node_repl": map[string]any{}, "oracle": map[string]any{},
	}}}})
	host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		state.mu.Lock()
		defer state.mu.Unlock()
		rows := []any{}
		for _, name := range []string{"gemini_notebook", "node_repl", "oracle"} {
			status := "connected"
			if pyjson.Map(pyjson.Map(state.config["mcp_servers"])[name])["enabled"] == false {
				status = "disabled"
			}
			rows = append(rows, map[string]any{"name": name, "pluginId": nil, "runtimeStatus": status})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "finished-business-turn"}}})
	// Another client's subscription keeps this thread loaded until operator recovery.
	host.Respond("thread/unsubscribe", fakehost.Reply{})
	// These calls are made by the test's operator, never by the bridge or relay.
	host.Handle("thread/archive", func(json.RawMessage) fakehost.Reply {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.loaded, state.config = false, nil
		return fakehost.Reply{}
	})
	host.Respond("thread/unarchive", fakehost.Reply{})
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	t.Cleanup(func() { _ = client.Close() })
	openLedger := func(name string) *ledger.Ledger {
		l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), name), ledger.Options{Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	policy := relayPolicy(t)
	b := bridge.New(client, openLedger("bridge.sqlite3"), policy)
	a := New(Options{RPC: client, Ledger: openLedger("delivery.sqlite3"), Policy: policy})
	t.Cleanup(func() { _ = a.Close() })
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &registry.Registry{Store: s}
	if _, err := r.RecordSettings(t.Context(), "thread-1", contract.OrderedObject(childRecord(false, "ui-qa").Data), "creation_result", "child", registry.Citation{}); err != nil {
		t.Fatal(err)
	}
	recorded, ok, err := r.LoadSettings(t.Context(), "thread-1")
	if err != nil || !ok || recorded.Data.Get("mcpProfile") != "ui-qa" {
		t.Fatalf("profile record = %v, found=%v, error=%v", recorded, ok, err)
	}
	return profileSendFixture{b, a, host, client, &delivery.TaskSettings{Data: delivery.Obj(recorded.Data)}}
}

func (f profileSendFixture) send(t *testing.T, request, role, profile string) ledger.Receipt {
	t.Helper()
	expected := map[string]any{"model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	if profile != "" {
		expected["mcp_profile"] = profile
	}
	receipt, err := f.bridge.SendMessageToThread(t.Context(), bridge.SendMessage{RequestID: request, ThreadID: "thread-1", Message: "fallback", Role: role, Expected: expected})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestProfilelessBridgeSendCannotStrandRecordedChild(t *testing.T) {
	for _, role := range []string{"child", ""} {
		t.Run("role="+role, func(t *testing.T) {
			f := newProfileSendFixture(t)
			receipt := f.send(t, "fallback", role, "")
			rpc := pyjson.Map(receipt["rpcError"])
			if receipt["status"] != "failed" || rpc["code"] != execution.Missing || receipt["delivery"] != "not_delivered" ||
				f.host.Count("thread/resume") != 0 || f.host.Count("turn/start") != 0 {
				t.Errorf("unsafe profile-less fallback: status=%v code=%v delivery=%v resumes=%d turns=%d", receipt["status"], rpc["code"], receipt["delivery"], f.host.Count("thread/resume"), f.host.Count("turn/start"))
			}
			message, _ := rpc["message"].(string)
			for _, part := range []string{"expected_settings.mcp_profile", "child.settings.mcpProfile", "ui-qa", "NEW request id", "relay"} {
				if !strings.Contains(message, part) {
					t.Errorf("refusal lacks %q: %q", part, message)
				}
			}
			next := sendRecord(t, f.relay, "next-relay-delivery", f.record)
			if next["status"] != "accepted" {
				t.Errorf("next relay delivery withheld: %v", next["rpcError"])
			}
		})
	}
}

func TestExplicitReleasedProfileAllowsFallbackAndNextRelayDelivery(t *testing.T) {
	f := newProfileSendFixture(t)
	if receipt := f.send(t, "profiled-fallback", "child", "ui-qa"); receipt["status"] != "accepted" || pyjson.Map(receipt["mcpProfile"])["name"] != "ui-qa" {
		t.Fatalf("fallback = %v", receipt)
	}
	if next := sendRecord(t, f.relay, "next-relay-delivery", f.record); next["status"] != "accepted" {
		t.Fatalf("next relay delivery = %v", next)
	}
}

func TestLoadedMCPMismatchNamesRecoveryUnderRecordedProfile(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-pair", true: "settings-free"}[free], func(t *testing.T) {
			f := newProfileSendFixture(t)
			// An independent client already loaded this resumable business thread wrongly.
			if _, err := f.client.Call(t.Context(), "thread/resume", map[string]any{"threadId": "thread-1"}); err != nil {
				t.Fatal(err)
			}
			f.record.SettingsFreeResume = free
			refused := sendRecord(t, f.relay, "wrongly-loaded", f.record)
			rpc := pyjson.Map(refused["rpcError"])
			if refused["status"] != "failed" || rpc["code"] != registry.SettingsNotPreserved || f.host.Count("turn/start") != 0 {
				t.Fatalf("hold = %v", refused)
			}
			message, _ := rpc["message"].(string)
			for _, part := range []string{"mcpServers", "ui-qa", "notLoaded", "thread/archive", "thread/unarchive", "next relay delivery", "never-run", "resumable"} {
				if !strings.Contains(message, part) {
					t.Errorf("hold lacks recovery %q: %q", part, message)
				}
			}
			if f.host.Count("thread/archive") != 0 || f.host.Count("thread/unarchive") != 0 {
				t.Fatal("the relay attempted automatic recovery")
			}
			for _, method := range []string{"thread/archive", "thread/unarchive"} {
				if _, err := f.client.Call(context.Background(), method, map[string]any{"threadId": "thread-1"}); err != nil {
					t.Fatal(err)
				}
			}
			if next := sendRecord(t, f.relay, "after-operator-recovery", f.record); next["status"] != "accepted" {
				t.Fatalf("recovered relay delivery = %v", next)
			}
		})
	}
}
