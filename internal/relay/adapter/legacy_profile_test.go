package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// An armed request recorded before the role's policy required an MCP profile is replayed after it did, and the
// replay is not refused with mcp_profile_required. The first start leaves the request create_armed by a creation
// that fails after its thread started: the host refuses the standby turn, so the creation fails with the thread
// retained (attemptedEffects holds no turn/start, so no standby recovery is tried) and the start ends
// incomplete at creation_failed. It was a refused thread/name/set with a refused thread/resume for the standby
// recovery until CRW-1313, which made a refused name a recorded warning and the creation accepted
// (TestManagedStartReachesItsBusinessTurnWhenOnlyNamingIsRefused).
func TestManagedLegacyProfileCLIReplay(t *testing.T) {
	c := newManagedCLIOn(t, noMCPManagedPolicy, "gpt-5.4", "medium")
	raw := c.request(nil)
	c.host.Script("turn/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "standby turn refused"}})
	out, _, code := c.start(raw)
	var first map[string]any
	if err := json.Unmarshal([]byte(out), &first); err != nil || code != contract.ExitRefused || first["reservationState"] != "create_armed" || first["reason"] != "creation_failed" {
		t.Fatalf("arm: %d %s", code, out)
	}
	if err := os.WriteFile(os.Getenv(execution.EnvPolicy), []byte(mcpManagedPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	registry.ResetRolePolicySnapshot()
	mcpScript(c)
	// Re-publish just the synthetic worker's new policy summary without acquiring its locks again.
	policy := registry.EnvironmentRolePolicy()
	path := filepath.Join(c.state, "worker-policy.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["policy"] = plain(policy.Summary())
	bytes, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	out, _, code = c.start(raw)
	var replay map[string]any
	if err := json.Unmarshal([]byte(out), &replay); err != nil || replay["reason"] == "mcp_profile_required" {
		t.Fatalf("legacy preflight: %d %s", code, out)
	}
}

// CRW-1313: a host that refuses only thread/name/set leaves the managed start's creation accepted with the naming
// warning (titleWarning title_not_set) recorded on its receipt; the creation itself sends the standby turn, with no
// standby recovery (no thread/resume before it), and the start reaches its business turn and is admitted.
func TestManagedStartReachesItsBusinessTurnWhenOnlyNamingIsRefused(t *testing.T) {
	c := newManagedCLIOn(t, noMCPManagedPolicy, "gpt-5.4", "medium")
	c.host.Script("thread/name/set", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "name refused"}})
	out, stderr, code := c.start(c.request(nil))
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil || code != contract.ExitOk || answer["state"] != "admitted" || answer["stage"] != "business_accepted" || answer["reservationState"] != "attached" {
		t.Fatalf("start: %d %s %s", code, out, stderr)
	}
	methods := []string{}
	for _, request := range c.host.Requests() {
		methods = append(methods, request.Method)
	}
	named, standby, resumed := -1, -1, -1
	for i, method := range methods {
		switch {
		case method == "thread/name/set" && named < 0:
			named = i
		case method == "turn/start" && standby < 0:
			standby = i
		case method == "thread/resume" && resumed < 0:
			resumed = i
		}
	}
	if c.host.Count("thread/name/set") != 1 || c.host.Count("turn/start") != 2 || named < 0 || standby < named || resumed >= 0 && resumed < standby {
		t.Fatalf("the creation did not send the standby turn itself, before any resume, and then the business turn: %v", methods)
	}
	a, err := Open(c.host.SocketPath, c.state, Options{Policy: registry.EnvironmentRolePolicy().BridgePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	create, _ := managed.OperationIDs("managed-pre")
	receipt, err := (Managed{a}).GetOperation(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	warning, _ := receipt["titleWarning"].(map[string]any)
	if receipt["status"] != "accepted" || receipt["title"] != nil || warning["code"] != "title_not_set" || receipt["turnId"] != "standby" {
		t.Fatalf("creation receipt: %v", receipt)
	}
}

func TestManagedNewAndReservedMissingProfilesStayRefused(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "reserved"}[reserved], func(t *testing.T) {
			c := newManagedCLIOn(t, mcpManagedPolicy, "gpt-5.4", "medium")
			mcpScript(c)
			raw := c.request(nil)
			if reserved {
				a, err := Open(c.host.SocketPath, c.state, Options{Policy: registry.EnvironmentRolePolicy().BridgePolicy()})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				ledger, err := a.LedgerIdentityRecord(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				req, err := managed.ParseRequest(raw)
				if err != nil {
					t.Fatal(err)
				}
				c.withStore(func(s *store.Store) {
					id, err := managed.RequestIdentity(context.Background(), req, s, c.host.SocketPath, c.marker, c.state, ledger)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := (managed.Reservation{Store: s}).Reserve(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				})
			}
			out, _, code := c.start(raw)
			var refusal map[string]any
			if err := json.Unmarshal([]byte(out), &refusal); err != nil || code != contract.ExitRefused || refusal["reason"] != "mcp_profile_required" || c.host.Count("thread/start") != 0 {
				t.Fatalf("missing profile: %d %s", code, out)
			}
			c.withStore(func(s *store.Store) {
				var n int
				if err := s.DB.QueryRow("SELECT COUNT(*) FROM managed_start_requests WHERE state='create_armed'").Scan(&n); err != nil || n != 0 {
					t.Fatalf("refusal armed a request: %d %v", n, err)
				}
			})
			if !reserved {
				corrected := c.request(func(req map[string]any) { req["requestId"] = "corrected-profile"; c.childProfile("minimal")(req) })
				out, _, code = c.start(corrected)
				if code != contract.ExitOk {
					t.Fatalf("corrected request: %d %s", code, out)
				}
			}
		})
	}
}
