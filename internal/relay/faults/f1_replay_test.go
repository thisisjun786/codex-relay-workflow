package faults

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Every replay compares CLI bytes and every SQLite table with the golden, including refused
// transitions that commit cancellation/repointing before returning the refusal.
func TestF1ReplayWholeCLI(t *testing.T) {
	goldenParent(t)
	type replay struct {
		name, state, kind string
		sql               []string
		args              []string
	}
	cases := []replay{
		{name: "claim_empty_owner", args: []string{"fault-claim", "--publication", "$pub", "--owner", ""}},
		{name: "claim_empty_publication", args: []string{"fault-claim", "--publication", "", "--owner", "operator"}},
		{name: "operation_empty_token", state: "claimed", args: []string{"fault-operation", "--publication", "$pub", "--claim-token", ""}},
		{name: "claim_backoff_before_target", sql: []string{"DELETE FROM fault_targets", "UPDATE fault_publications SET next_attempt_at=100100"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "operation_cancel_stale_relink", state: "claimed", kind: "update_record", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'", `UPDATE fault_publication_payloads SET payload='{"op":"set_project","value":"old-project"}'`}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_cancel_removed_target", state: "claimed", kind: "update_record", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'", "DELETE FROM fault_target_projects", `UPDATE fault_publication_payloads SET payload='{"op":"set_project","value":"old-project"}'`}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_cancel_keeps_backoff", state: "claimed", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'", "UPDATE fault_publications SET next_attempt_at=100100", "UPDATE fault_publication_payloads SET hold_reason='prior',updated_at='earlier'"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "complete_prior_cycle_operation", state: "claimed", kind: "append_comment", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE',cycle=2"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "claim", args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_blank_owner", args: []string{"fault-claim", "--publication", "$pub", "--owner", " "}},
		{name: "claim_owned", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_unregistered", kind: "project_create", args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_no_issue", kind: "append_comment", args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_no_target", sql: []string{"DELETE FROM fault_targets"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_contested", sql: []string{"UPDATE fault_target_projects SET product='other'"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_backoff", sql: []string{"UPDATE fault_publications SET next_attempt_at=100100"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_budget", sql: []string{"INSERT INTO fault_limits VALUES('crw','open_record',0,3600,'stamp')"}, args: []string{"fault-claim", "--publication", "$pub", "--owner", "operator"}},
		{name: "claim_writer", state: "pending", args: []string{"fault-claim", "--publication", "$pub", "--owner", "other"}},
		{name: "claim_takeover", state: "pending", args: []string{"fault-claim", "--publication", "$pub", "--owner", "other", "--takeover"}},
		{name: "operation", state: "claimed", args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_owned", state: "claimed", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_no_target", state: "claimed", sql: []string{"DELETE FROM fault_targets"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_contested", state: "claimed", sql: []string{"UPDATE fault_target_projects SET product='other'"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_unregistered", state: "claimed", kind: "project_create", args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_no_issue", state: "claimed", kind: "append_comment", args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_comment", state: "claimed", kind: "append_comment", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "operation_reused_attempt", state: "claimed", sql: []string{"INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts,outcome,ended) SELECT publication_id,1,'operator',0,claimed_at,claimed_ts,NULL,0 FROM fault_publication_attempts", "UPDATE fault_publication_attempts SET outcome='retargeted',ended=1 WHERE attempt_id=1"}, args: []string{"fault-operation", "--publication", "$pub", "--claim-token", "token"}},
		{name: "reconcile_present", state: "uncertain", args: []string{"fault-reconcile", "--publication", "$pub", "--observed", "$block"}},
		{name: "reconcile_duplicate", state: "uncertain", args: []string{"fault-reconcile", "--publication", "$pub", "--observed", "$block\n$block"}},
		{name: "reconcile_malformed", state: "uncertain", args: []string{"fault-reconcile", "--publication", "$pub", "--observed", "<!-- relay-fault:$pub -->\n"}},
		{name: "reconcile_live", state: "issued", args: []string{"fault-reconcile", "--publication", "$pub", "--searched"}},
		{name: "reconcile_ended", state: "uncertain", args: []string{"fault-reconcile", "--publication", "$pub", "--searched", "--prior-ended", "--reason", "request ended"}},
		{name: "reconcile_unregistered", state: "uncertain", kind: "project_create", args: []string{"fault-reconcile", "--publication", "$pub"}},
		{name: "complete", state: "issued", args: []string{"fault-complete", "--publication", "$pub", "--claim-token", "token", "--readback", "$block", "--external-ref", "ISSUE", "--project-ref", "project-P"}},
		{name: "complete_missing_reference", state: "issued", args: []string{"fault-complete", "--publication", "$pub", "--claim-token", "token", "--readback", "$block"}},
		{name: "complete_missing_project", state: "issued", args: []string{"fault-complete", "--publication", "$pub", "--claim-token", "token", "--readback", "$block", "--external-ref", "ISSUE"}},
		{name: "complete_no_owned_issue", state: "uncertain", kind: "append_comment", args: []string{"fault-complete", "--publication", "$pub", "--readback", "$block"}},
		{name: "complete_wrong_issue", state: "uncertain", kind: "append_comment", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE'"}, args: []string{"fault-complete", "--publication", "$pub", "--readback", "$block", "--external-ref", "OTHER"}},
		{name: "complete_comment", state: "uncertain", kind: "append_comment", sql: []string{"UPDATE fault_ledger SET external_ref='ISSUE',cycle=2"}, args: []string{"fault-complete", "--publication", "$pub", "--readback", "$block"}},
		{name: "complete_duplicate", state: "uncertain", args: []string{"fault-complete", "--publication", "$pub", "--readback", "$block\n$block"}},
		{name: "complete_unregistered", state: "uncertain", kind: "project_create", args: []string{"fault-complete", "--publication", "$pub"}},
	}
	for _, op := range []string{"set_project", "reopen", "add_label", "add_relation"} {
		payload := `{"op":"` + op + `","value":"project-P"}`
		seed := []string{"UPDATE fault_ledger SET external_ref='ISSUE'", "UPDATE fault_publication_payloads SET payload='" + payload + "'"}
		fields := map[string]any{"issue": "ISSUE", "projectId": "project-P", "open": true, "labels": []string{"project-P"}, "relations": []string{"project-P"}}
		raw, _ := json.Marshal(fields)
		for _, command := range []string{"operation", "reconcile", "complete"} {
			args := []string{"fault-" + command, "--publication", "$pub"}
			if command != "reconcile" {
				args = append(args, "--claim-token", "token")
			}
			cases = append(cases, replay{name: command + "_" + op, state: "claimed", kind: "update_record", sql: seed, args: args})
			if command != "operation" {
				cases[len(cases)-1].args = append(cases[len(cases)-1].args, "--observed-fields", string(raw))
			}
		}
		for _, fields := range []string{`{}`, `{"issue":"ISSUE"}`, `[]`} {
			cases = append(cases, replay{name: "complete_" + op + "_mismatch_" + fields, state: "uncertain", kind: "update_record", sql: seed, args: []string{"fault-complete", "--publication", "$pub", "--observed-fields", fields}})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "f1-replay-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			for k, v := range map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/xs", "XDG_CONFIG_HOME": home + "/xc", "CODEX_HOME": home + "/ch"} {
				t.Setenv(k, v)
			}
			ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: 100000}, entropy: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7})})
			gd := home + "/go"
			f1Twin(t, gd)
			s, e := store.Open(ctx, gd+"/relay.sqlite3", "")
			if e != nil {
				t.Fatal(e)
			}
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			if _, e = l.SetTarget(ctx, "crw", "P", "team", "project-P"); e != nil {
				t.Fatal(e)
			}
			sig := map[string]any{"turn": "replay"}
			if _, e = l.Record(ctx, Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: sig, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}); e != nil {
				t.Fatal(e)
			}
			id := FaultID("crw", "report_omitted", sig)
			pub := publicationID(id, openRecord, triggerOpen)
			if tc.state != "" {
				if _, e = l.exec(ctx, "UPDATE fault_publications SET state=?,claim_token='token',lease_owner='operator',lease_until=100300,attempts=1", tc.state); e != nil {
					t.Fatal(e)
				}
				if _, e = l.exec(ctx, "INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts) VALUES(?,1,'operator',0,?,100000)", pub, l.Clock.ISO()); e != nil {
					t.Fatal(e)
				}
			}
			if tc.kind != "" {
				if _, e = l.exec(ctx, "UPDATE fault_publications SET kind=?", tc.kind); e != nil {
					t.Fatal(e)
				}
			}
			for _, sql := range tc.sql {
				if _, e = l.exec(ctx, sql); e != nil {
					t.Fatal(e)
				}
			}
			r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", pub)
			if e != nil {
				t.Fatal(e)
			}
			f, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", id)
			if e != nil {
				t.Fatal(e)
			}
			block := f1RenderBlock(r, f)
			if e = s.Close(); e != nil {
				t.Fatal(e)
			}
			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				args[i] = strings.NewReplacer("$pub", pub, "$block", block).Replace(a)
			}
			f1ReplayCLI(t, ctx, gd, args)
		})
	}
}

// f1ReplayCLI runs one command line on the store in gd and compares its whole answer - its
// streams, exit and every table after it - with the golden, which holds the rows that changed
// since the previous command of the test on gd (f1Delta).
func f1ReplayCLI(t *testing.T, ctx context.Context, gd string, args []string) map[string]any {
	t.Helper()
	if inputs, ok := ctx.Value(f1InputsKey{}).(f1Inputs); ok {
		inputs.entropy = bytes.NewReader(bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7}, 128))
		ctx = context.WithValue(ctx, f1InputsKey{}, inputs)
	}
	var stdout, stderr bytes.Buffer
	code := executeAsCLI(ctx, append([]string{"--state", gd, "--json"}, args...), &stdout, &stderr)
	s, e := store.Open(ctx, gd+"/relay.sqlite3", "")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	names, e := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if e != nil {
		t.Fatal(e)
	}
	tables := map[string]any{}
	for _, name := range names {
		key := text(name, "name")
		rows, e := s.All(ctx, "SELECT * FROM "+key+" ORDER BY rowid")
		if e != nil {
			t.Fatal(e)
		}
		entries := []any{}
		for _, r := range rows {
			m := map[string]any{}
			for _, c := range r {
				m[c.Name] = c.Value
			}
			entries = append(entries, m)
		}
		raw, e := json.Marshal(entries)
		if e != nil {
			t.Fatal(e)
		}
		var got any
		if e = json.Unmarshal(raw, &got); e != nil {
			t.Fatal(e)
		}
		if key == "schema_meta" {
			// The store names its owning runtime: compared as the runtime-neutral owner.
			got = ownerNeutralRows(t, testsupport.Go, got)
		}
		tables[key] = got
	}
	delta := f1GoldenDelta(t, gd, code, stdout.String(), stderr.String(), tables)
	checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, filepath.Dir(gd)), json.RawMessage(delta))
	var answer map[string]any
	if e = json.Unmarshal(stdout.Bytes(), &answer); e != nil {
		t.Fatal(e)
	}
	return answer
}

// f1Snapshots holds, for each store a running test replays commands on, every table its last
// answer named: its rows in rowid order, each as canonical JSON.
var f1Snapshots = struct {
	sync.Mutex
	tables map[string]map[string][]json.RawMessage
}{tables: map[string]map[string][]json.RawMessage{}}

// f1Snapshot is what the last answer on the store in dir held (nothing before its first).
func f1Snapshot(t testing.TB, dir string) map[string][]json.RawMessage {
	f1Snapshots.Lock()
	defer f1Snapshots.Unlock()
	tables, ok := f1Snapshots.tables[dir]
	if !ok {
		tables = map[string][]json.RawMessage{}
		f1Snapshots.tables[dir] = tables
		t.Cleanup(func() {
			f1Snapshots.Lock()
			delete(f1Snapshots.tables, dir)
			f1Snapshots.Unlock()
		})
	}
	return maps.Clone(tables)
}

// f1Delta is one answer as a golden keeps it: the CLI's streams and exit, and each of the store's
// tables that differs from the previous answer's (changed) or that it no longer has (dropped).
type f1Delta struct {
	Stdout  string                  `json:"stdout"`
	Stderr  string                  `json:"stderr"`
	Exit    int                     `json:"exit"`
	Changed map[string]f1TableDelta `json:"changed"`
	Dropped []string                `json:"dropped,omitempty"`
}

// f1TableDelta is a changed table: its row count and each row, by its position in rowid order,
// that differs from the row the previous answer had there.
type f1TableDelta struct {
	Length int                        `json:"length"`
	Rows   map[string]json.RawMessage `json:"rows,omitempty"`
}

// f1Canonical is value re-encoded one way (sorted keys, numbers as written), so an unchanged row
// reads the same in every answer.
func f1Canonical(value json.RawMessage) (json.RawMessage, error) {
	decoded, err := decodeNumbers(value)
	if err != nil {
		return nil, err
	}
	return encodeNumbers(decoded)
}

// f1GoldenDelta is Go's whole answer to one command line on the store in gd - its streams, exit
// and every table, evidence digests as their occurrences' placeholders - as the rows that changed
// since Go's previous answer on gd (f1Delta), and keeps the answer's tables as gd's latest.
func f1GoldenDelta(t *testing.T, gd string, code int, stdout, stderr string, tables map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"stdout": stdout, "stderr": stderr, "exit": code, "tables": tables})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = evidencePlaceholders(raw); err != nil {
		t.Fatal(err)
	}
	var whole struct {
		Stdout string                       `json:"stdout"`
		Stderr string                       `json:"stderr"`
		Exit   int                          `json:"exit"`
		Tables map[string][]json.RawMessage `json:"tables"`
	}
	if err = json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	before := f1Snapshot(t, gd)
	after := map[string][]json.RawMessage{}
	delta := f1Delta{Stdout: whole.Stdout, Stderr: whole.Stderr, Exit: whole.Exit, Changed: map[string]f1TableDelta{}}
	for name, rows := range whole.Tables {
		old, had := before[name]
		after[name] = []json.RawMessage{}
		change := f1TableDelta{Length: len(rows), Rows: map[string]json.RawMessage{}}
		for i, row := range rows {
			canonical, err := f1Canonical(row)
			if err != nil {
				t.Fatal(err)
			}
			after[name] = append(after[name], canonical)
			if i >= len(old) || !bytes.Equal(old[i], canonical) {
				change.Rows[strconv.Itoa(i)] = canonical
			}
		}
		if !had || len(rows) != len(old) || len(change.Rows) > 0 {
			delta.Changed[name] = change
		}
	}
	for name := range before {
		if _, ok := whole.Tables[name]; !ok {
			delta.Dropped = append(delta.Dropped, name)
		}
	}
	sort.Strings(delta.Dropped)
	f1Snapshots.Lock()
	f1Snapshots.tables[gd] = after
	f1Snapshots.Unlock()
	encoded, err := encodeNumbers(delta)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
