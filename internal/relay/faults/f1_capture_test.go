package faults

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestF1WholeOutput(t *testing.T) {
	goldenParent(t)
	for _, action := range []string{"claim", "operation", "reconcile", "complete_refusal", "complete", "reconcile_ended", "sweep", "sweep_readings", "operation_retarget", "claim_backoff"} {
		t.Run(action, func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "f1-whole-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			root, e := filepath.Abs("../../..")
			if e != nil {
				t.Fatal(e)
			}
			raw := pyAnswer(t, "f1_capture.py "+action, []string{action}, pyRunPaths(t, home), func() ([]byte, error) {
				cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/faults/testdata/f1_capture.py"), filepath.Join(home, "py"), action)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch", "TMPDIR=/dev/shm")
				raw, e := cmd.CombinedOutput()
				if e != nil {
					return nil, fmt.Errorf("python: %v %s", e, raw)
				}
				return recordEvidenceDigests(raw)
			})
			raw, e = replayEvidenceDigests(raw)
			if e != nil {
				t.Fatalf("recorded Python answer: %v", e)
			}
			var expected map[string]any
			if e := json.Unmarshal(raw, &expected); e != nil {
				t.Fatal(e)
			}
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", home+"/xs")
			t.Setenv("XDG_CONFIG_HOME", home+"/xc")
			t.Setenv("CODEX_HOME", home+"/ch")
			ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{entropy: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7})})
			s, e := store.Open(ctx, filepath.Join(home, "go", "relay.sqlite3"), "")
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			if _, e = l.SetTarget(ctx, "crw", "P", "team", "project-P"); e != nil {
				t.Fatal(e)
			}
			o := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "f1"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}
			if _, e = l.Record(ctx, o); e != nil {
				t.Fatal(e)
			}
			id := FaultID("crw", "report_omitted", o.Signature)
			pub := publicationID(id, openRecord, triggerOpen)
			if action != "claim" && action != "claim_backoff" && action != "sweep" && action != "sweep_readings" {
				_, e = s.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET state='claimed',claim_token='token',lease_owner='operator',lease_until=?,attempts=1 WHERE publication_id=?", float64(100300), pub)
				if e != nil {
					t.Fatal(e)
				}
				_, e = s.Q(ctx).ExecContext(ctx, "INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts) VALUES(?,1,'operator',0,?,?)", pub, l.Clock.ISO(), l.Clock.Now())
				if e != nil {
					t.Fatal(e)
				}
			}
			var answer any
			switch action {
			case "sweep", "sweep_readings":
				sw := &Sweeper{Store: s, HostRecordPath: testHostRecordPath(), Now: l.Clock.ISO, Installation: Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: relayPackageLocation}}
				var batch Batch
				if action == "sweep_readings" {
					batch, e = sw.SweepReadings(ctx, "crw", "", []any{map[string]any{"schema": "reporting-observation/1", "relationshipId": "rel-1", "selectors": map[string]any{"turn": "turn-1"}, "reportingState": "unreported"}}, 0)
				} else {
					batch, e = sw.Sweep(ctx, "crw")
				}
				if e != nil {
					t.Fatal(e)
				}
				var counts RecordedSweep
				counts, e = sw.RecordAll(ctx, l, batch)
				answer = map[string]any{"read": counts.Read, "recorded": counts.Recorded, "queued": counts.Queued, "gaps": counts.Gaps, "results": counts.Results}
			case "claim":
				answer, e = f1Claim(ctx, l, map[string]string{"--publication": pub, "--owner": "operator"})
			case "claim_backoff":
				_, e = s.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET next_attempt_at=? WHERE publication_id=?", float64(100100), pub)
				if e != nil {
					t.Fatal(e)
				}
				answer, e = f1Claim(ctx, l, map[string]string{"--publication": pub, "--owner": "operator"})
				if e != nil {
					reason, detail, _ := strings.Cut(strings.TrimPrefix(e.Error(), "transaction body: "), ": ")
					answer = map[string]any{"error": "FaultRefused", "reason": reason, "detail": reason + ": " + detail}
					e = nil
				}
			case "operation":
				answer, e = f1Operation(ctx, l, map[string]string{"--publication": pub, "--claim-token": "token"})
			case "operation_retarget":
				_, e = l.SetTarget(ctx, "crw", "P", "other", "project-other")
				if e != nil {
					t.Fatal(e)
				}
				answer, e = f1Operation(ctx, l, map[string]string{"--publication": pub, "--claim-token": "token"})
				if e != nil {
					reason, detail, _ := strings.Cut(strings.TrimPrefix(e.Error(), "transaction body: "), ": ")
					answer = map[string]any{"error": "FaultRefused", "reason": reason, "detail": reason + ": " + detail}
					e = nil
				}
			case "reconcile":
				answer, e = f1Reconcile(ctx, l, map[string]string{"--publication": pub, "--searched": "true"})
			case "reconcile_ended":
				_, e = s.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET state='uncertain',claim_token=NULL,lease_owner=NULL,lease_until=NULL WHERE publication_id=?", pub)
				if e != nil {
					t.Fatal(e)
				}
				answer, e = f1Reconcile(ctx, l, map[string]string{"--publication": pub, "--searched": "true", "--prior-ended": "true", "--reason": "request ended"})
			case "complete":
				operation, opErr := f1Operation(ctx, l, map[string]string{"--publication": pub, "--claim-token": "token"})
				if opErr != nil {
					t.Fatal(opErr)
				}
				answer, e = f1Complete(ctx, l, map[string]string{"--publication": pub, "--claim-token": "token", "--readback": operation.(map[string]any)["block"].(string), "--external-ref": "REL-5", "--project-ref": "project-P"})
			case "complete_refusal":
				answer, e = f1Complete(ctx, l, map[string]string{"--publication": pub, "--claim-token": "token", "--readback": "not a block"})
				if e != nil {
					reason, detail, _ := strings.Cut(strings.TrimPrefix(e.Error(), "transaction body: "), ": ")
					answer = map[string]any{"error": "FaultRefused", "reason": reason, "detail": reason + ": " + detail}
					e = nil
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			tables := map[string]any{}
			names, e := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name")
			if e != nil {
				t.Fatal(e)
			}
			for _, name := range names {
				key := text(name, "name")
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
			checkGoldenEvidence(t, "reply and fault tables", []string{action}, runPathsOf(t, home), got)
			if !reflect.DeepEqual(got, expected) {
				keys := []string{}
				for key, want := range expected["tables"].(map[string]any) {
					if !reflect.DeepEqual(got["tables"].(map[string]any)[key], want) {
						keys = append(keys, key)
					}
				}
				sort.Strings(keys)
				t.Errorf("different tables %v; reply go=%v python=%v", keys, got["reply"], expected["reply"])
				for _, key := range keys {
					t.Logf("%s go=%v python=%v", key, got["tables"].(map[string]any)[key], expected["tables"].(map[string]any)[key])
				}
			}
		})
	}
}
