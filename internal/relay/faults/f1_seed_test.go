package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Replay a publication's stable state through the CLI, including refusals
// that require an existing claimed, issued or uncertain write.
func TestF1SeededCLIOracle(t *testing.T) {
	goldenParent(t)
	for _, tc := range []struct {
		name, state, token string
		args               func(string) []string
	}{
		{"claim_uncertain", "uncertain", "", func(pub string) []string { return []string{"fault-claim", "--publication", pub, "--owner", "operator"} }},
		{"claim_claimed", "claimed", "token", func(pub string) []string { return []string{"fault-claim", "--publication", pub, "--owner", "operator"} }},
		{"operation_pending", "pending", "", func(pub string) []string {
			return []string{"fault-operation", "--publication", pub, "--claim-token", "token"}
		}},
		{"operation_stale", "claimed", "token", func(pub string) []string {
			return []string{"fault-operation", "--publication", pub, "--claim-token", "wrong"}
		}},
		{"complete_pending", "pending", "", func(pub string) []string { return []string{"fault-complete", "--publication", pub} }},
		{"complete_stale", "claimed", "token", func(pub string) []string {
			return []string{"fault-complete", "--publication", pub, "--claim-token", "wrong"}
		}},
		{"reconcile_unattested", "claimed", "token", func(pub string) []string { return []string{"fault-reconcile", "--publication", pub} }},
		{"reconcile_issued", "issued", "token", func(pub string) []string { return []string{"fault-reconcile", "--publication", pub, "--searched"} }},
		{"reconcile_unproven", "uncertain", "", func(pub string) []string { return []string{"fault-reconcile", "--publication", pub, "--searched"} }},
		{"reconcile_end_reason", "uncertain", "", func(pub string) []string {
			return []string{"fault-reconcile", "--publication", pub, "--searched", "--prior-ended"}
		}},
		{"complete_absent", "pending", "", func(pub string) []string {
			return []string{"fault-complete", "--publication", pub, "--readback", "text"}
		}},
		{"complete_mismatch", "uncertain", "", func(pub string) []string {
			return []string{"fault-complete", "--publication", pub, "--readback", "text"}
		}},
		{"complete_confirmed", "confirmed", "", func(pub string) []string { return []string{"fault-complete", "--publication", pub} }},
		{"reconcile_empty_fields", "uncertain", "", func(pub string) []string {
			return []string{"fault-reconcile", "--publication", pub, "--observed-fields", `{"issue":"wrong"}`}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "f1-seeded-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: 100000}})
			goDir := filepath.Join(home, "go")
			s, err := store.Open(ctx, filepath.Join(goDir, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			_, err = l.Record(ctx, Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "seed"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}})
			if err != nil {
				t.Fatal(err)
			}
			pub := publicationID(FaultID("crw", "report_omitted", map[string]any{"turn": "seed"}), openRecord, triggerOpen)
			_, err = s.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET state=?,claim_token=?,attempts=1,lease_until=? WHERE publication_id=?", tc.state, nilIfEmpty(tc.token), float64(100300), pub)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Q(ctx).ExecContext(ctx, "INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts) VALUES(?,1,'operator',0,?,?)", pub, l.Clock.ISO(), l.Clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			args := tc.args(pub)
			var got, stderr bytes.Buffer
			goCode, handled := executeAsCLI(ctx, append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			if !handled {
				t.Fatal("unhandled")
			}
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: goCode, Stdout: got.String()})
		})
	}
}
