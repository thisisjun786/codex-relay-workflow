package faults

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// FLT-26: a failed issued write is uncertain and stays write-once, with an
// ended attempt only when the holder attests a definitive connector refusal.
func TestF2WholeOutput(t *testing.T) {
	goldenParent(t)
	for _, action := range []string{"fail", "fail_issued", "fail_ended", "move", "adopt", "update", "update_owned", "update_project"} {
		t.Run(action, func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "f2-whole-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			ctx := context.Background()
			s, e := store.Open(ctx, filepath.Join(home, "go", "relay.sqlite3"), "")
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			o := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "f2"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "old"}}
			recorded, e := l.Record(ctx, o)
			if e != nil || !recorded {
				t.Fatalf("record: %v %v", recorded, e)
			}
			id := FaultID("crw", "report_omitted", o.Signature)
			pub := publicationID(id, openRecord, triggerOpen)
			var args map[string]string
			var name string
			switch action {
			case "fail", "fail_issued", "fail_ended":
				state := "claimed"
				var issued any
				if action != "fail" {
					state = "issued"
					issued = l.Clock.ISO()
				}
				_, e = s.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET state=?,claim_token='token',attempts=1,issued_at=? WHERE publication_id=?", state, issued, pub)
				if e != nil {
					t.Fatal(e)
				}
				_, e = s.Q(ctx).ExecContext(ctx, "INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts,issued_at) VALUES(?,1,'holder',0,?,?,?)", pub, l.Clock.ISO(), l.Clock.Now(), issued)
				if e != nil {
					t.Fatal(e)
				}
				name = "fault-fail"
				args = map[string]string{"--publication": pub, "--claim-token": "token", "--error": "connector refused"}
				if action == "fail_ended" {
					args["--ended"] = "true"
				}
			case "move":
				name = "fault-move"
				args = map[string]string{"--fault": id, "--scope": `{"projectKey":"new"}`}
			case "adopt":
				name = "fault-adopt"
				args = map[string]string{"--fault": id, "--external-ref": "CRW-1", "--scope": `{"projectKey":"new"}`}
			case "update":
				name = "fault-update"
				args = map[string]string{"--fault": id, "--op": "add_label", "--value": `"urgent"`}
			case "update_owned", "update_project":
				_, e = executeF2(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": "CRW-1", "--scope": `{"projectKey":"old"}`})
				if e != nil {
					t.Fatal(e)
				}
				name = "fault-update"
				args = map[string]string{"--fault": id, "--op": "add_label", "--value": `"urgent"`}
				if action == "update_project" {
					if _, e = l.SetTarget(ctx, "crw", "old", "team", "project-1"); e != nil {
						t.Fatal(e)
					}
					if e = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
						return dRelinkOne(ctx, l, id, "project-1", l.Clock.ISO())
					}); e != nil {
						t.Fatal(e)
					}
					args["--op"] = "set_project"
					args["--value"] = `"project-1"`
				}
			}
			answer, e := executeF2(ctx, l, name, args)
			if e != nil {
				t.Fatal(e)
			}
			tables := map[string]any{}
			names, e := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name")
			if e != nil {
				t.Fatal(e)
			}
			for _, r := range names {
				key := text(r, "name")
				rows, e := s.All(ctx, "SELECT * FROM "+key+" ORDER BY rowid")
				if e != nil {
					t.Fatal(e)
				}
				if len(rows) == 0 {
					continue
				}
				entries := []any{}
				for _, r := range rows {
					entry := map[string]any{}
					for _, c := range r {
						entry[c.Name] = c.Value
					}
					entries = append(entries, entry)
				}
				tables[key] = entries
			}
			normalized, e := json.Marshal(map[string]any{"reply": answer, "tables": tables})
			if e != nil {
				t.Fatal(e)
			}
			var got map[string]any
			if e = json.Unmarshal(normalized, &got); e != nil {
				t.Fatal(e)
			}
			checkGolden(t, "reply and fault tables", []string{action}, runPathsOf(t, home), got)
		})
	}
}
