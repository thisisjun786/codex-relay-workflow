package managed

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1313 (Argon V5 on the CRW-1353 lane, K5): create_thread saves its receipt once more right before turn/start, and
// that save is the only trace a process killed after turn/start leaves, since attemptedEffects is written when the
// mutation ends. Since the creator names the thread by the title table and a refused thread/name/set is a warning
// rather than a refusal, that save carries the name (title) or the naming warning (titleWarning). The reconciliation of
// an unknown creation reads either as "turn/start may have been sent" and never sends it again (I-473).

const firstTurnModel, firstTurnEffort = "anthropic/claude-opus-5-5", "xhigh"

// bridgeRowAtFirstTurn runs a real bridge's create_thread for a `child` session with the given title against a fake host and
// returns the row its ledger holds at the moment turn/start reaches the host: what a process killed right after sending
// turn/start leaves.
func bridgeRowAtFirstTurn(t *testing.T, title string, refuseName bool) map[string]any {
	t.Helper()
	ctx := context.Background()
	host := fakehost.Start(t)
	client, err := appserver.Dial(ctx, host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	policy, err := execution.FromBytes([]byte(`{"allowed":[{"model":"`+firstTurnModel+`","efforts":["`+firstTurnEffort+`"]}],"roles":{"child":{"model":"`+firstTurnModel+`","reasoningEffort":"`+firstTurnEffort+`"}}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "t-1"}, "cwd": cwd, "model": firstTurnModel, "reasoningEffort": firstTurnEffort, "approvalPolicy": "never", "sandbox": map[string]any{"type": "dangerFullAccess"}}})
	if refuseName {
		host.Respond("thread/name/set", fakehost.Reply{Error: &fakehost.RPCError{Code: -32603, Message: "name store unavailable"}})
	} else {
		host.Respond("thread/name/set", fakehost.Reply{Result: map[string]any{}})
	}
	const requestID = "managed-create-first-turn"
	rows := make(chan map[string]any, 1)
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		row, err := store.Get(context.Background(), requestID)
		if err != nil {
			t.Errorf("reading the ledger at turn/start: %v", err)
		}
		rows <- row
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "standby"}}}
	})
	in := bridge.CreateThread{RequestID: requestID, CWD: cwd, Prompt: bootstrap, Title: title, Role: "child", Sandbox: "danger-full-access", Model: firstTurnModel, Effort: firstTurnEffort}
	if _, err := bridge.New(client, store, policy).CreateThread(ctx, in); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	select {
	case row = <-rows:
	default:
		t.Fatalf("turn/start never reached the host: %v", host.Requests())
	}
	if _, recorded := row["attemptedEffects"]; recorded || row["status"] != "in_progress_or_unknown" || row["threadId"] != "t-1" {
		t.Fatalf("the row at turn/start is not the save before the first turn: %v", row)
	}
	return row
}

func TestReconcileReadsTheSaveBeforeTheFirstTurnWhateverTheName(t *testing.T) {
	for _, c := range []struct {
		name, title string
		refuse      bool
		check       func(map[string]any) bool
	}{
		{"the host refused thread/name/set", "REL-MANAGED · Verify", true, func(row map[string]any) bool {
			return row["title"] == nil && pyjson.Map(row["titleWarning"])["code"] == "title_not_set"
		}},
		{"the creation had no title to give", "", false, func(row map[string]any) bool {
			return row["title"] == nil && pyjson.Map(row["titleWarning"])["code"] == "title_unset"
		}},
		{"the host named the thread", "REL-MANAGED · Verify", false, func(row map[string]any) bool {
			return row["title"] == "REL-MANAGED · Verify" && row["titleWarning"] == nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := bridgeRowAtFirstTurn(t, c.title, c.refuse)
			if !c.check(row) {
				t.Fatalf("the save before the first turn carries title %v, titleWarning %v", row["title"], row["titleWarning"])
			}
			for _, thread := range []string{"the thread has no turn", "the host cannot read the thread"} {
				t.Run(thread, func(t *testing.T) {
					k := newReconcileKit(t, "bridge-saved", "accept")
					k.host.saved = row
					if thread == "the host cannot read the thread" {
						k.host.failures["thread/read|t-1"] = errors.New("thread/read: thread not found")
					}
					for range 2 {
						k.expect(k.run(), "incomplete", "creation_unknown", "standby_turn_unknown")
					}
					// One creation, no standby send, no second creation: turn/start may have reached the host.
					k.effects(1, 0)
				})
			}
		})
	}
}
