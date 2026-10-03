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

func TestManagedLegacyProfileCLIReplay(t *testing.T) {
	c := newManagedCLIOn(t, noMCPManagedPolicy, "gpt-5.4", "medium")
	raw := c.request(nil)
	c.host.Script("thread/name/set", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "name refused"}})
	c.host.Script("thread/resume", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "temporarily refused"}})
	out, _, code := c.start(raw)
	var first map[string]any
	if err := json.Unmarshal([]byte(out), &first); err != nil || code != contract.ExitRefused || first["reservationState"] != "create_armed" {
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
