package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// capturePythonFault executes the real Python ledger in a disposable state tree (its recorded
// answer on replay). The complete answer and every populated fault_* table are the oracle, not
// a hand-selected subset of fields or reason strings.
func capturePythonFault(t *testing.T, observation map[string]any) (any, map[string]any) {
	return capturePythonFaults(t, []map[string]any{observation})
}
func capturePythonFaults(t *testing.T, observations []map[string]any) (any, map[string]any) {
	t.Helper()
	home, err := os.MkdirTemp("/dev/shm", "crw-fault-python-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	raw, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	out := pyAnswer(t, "capture.py", []string{string(raw)}, pyRunPaths(t, home), func() ([]byte, error) {
		state := filepath.Join(home, "relay")
		root, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			return nil, err
		}
		script := filepath.Join(root, "internal", "relay", "faults", "testdata", "capture.py")
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, state, string(raw))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("python capture: %v\n%s", err, out)
		}
		return out, nil
	})
	var expected map[string]any
	if err = json.Unmarshal(out, &expected); err != nil {
		t.Fatal(err)
	}
	goStore, err := store.Open(context.Background(), filepath.Join(home, "go", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer goStore.Close()
	clock := &testClock{now: 100000}
	l := &Ledger{Store: goStore, Clock: clock}
	var replies []any
	for _, observation := range observations {
		if seed, ok := observation["adoptionSeed"].(map[string]any); ok {
			_, e := goStore.Q(context.Background()).ExecContext(context.Background(), "INSERT INTO fault_adoptions (fault_id, external_ref, scope, state, created_at, updated_at) VALUES (?,?,?,?,?,?)", seed["faultId"], seed["externalRef"], dumps(seed["scope"], false), pending, clock.ISO(), clock.ISO())
			if e != nil {
				t.Fatal(e)
			}
			continue
		}
		if policy, ok := observation["policy"].(map[string]any); ok {
			args := map[string]string{"--product": policy["product"].(string), "--fault-class": policy["fault_class"].(string), "--severity": policy["severity"].(string), "--reason": policy["reason"].(string)}
			if threshold, exists := policy["threshold"]; exists {
				args["--threshold"] = fmt.Sprint(threshold)
			}
			if _, e := dSetPolicy(context.Background(), l, args); e != nil {
				t.Fatal(e)
			}
			continue
		}
		if advance, ok := observation["advanceSeconds"].(float64); ok {
			clock.now += advance
			continue
		}
		if advance, ok := observation["advanceSeconds"].(int); ok {
			clock.now += float64(advance)
			continue
		}
		o, e := parseObservation(observation)
		if e != nil {
			t.Fatal(e)
		}
		workspace, _ := o.Scope["workspace"].(string)
		publication := publicationID(FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace), openRecord, triggerOpen)
		adoptedPublication := publicationID(FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace), appendComment, triggerOpen)
		adoptedBefore, e := goStore.One(context.Background(), "SELECT state FROM fault_publications WHERE publication_id = ?", adoptedPublication)
		if e != nil {
			t.Fatal(e)
		}
		before, e := goStore.One(context.Background(), "SELECT state FROM fault_publications WHERE publication_id = ?", publication)
		if e != nil {
			t.Fatal(e)
		}
		recorded, e := l.Record(context.Background(), o)
		if e != nil {
			reason, _, _ := strings.Cut(e.Error(), ": ")
			replies = append(replies, map[string]any{"error": "FaultRefused", "reason": reason, "detail": e.Error()})
			continue
		}
		id := FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace)
		if alias, e := goStore.One(context.Background(), "SELECT fault_id FROM fault_aliases WHERE alias_id = ?", id); e != nil {
			t.Fatal(e)
		} else if alias != nil {
			id = text(alias, "fault_id")
		}
		row, e := goStore.One(context.Background(), "SELECT state,cycle,severity,occurrence_count,suppression FROM fault_ledger WHERE fault_id = ?", id)
		if e != nil {
			t.Fatal(e)
		}
		var reply any
		if row == nil && o.Cleared {
			reply = map[string]any{"faultId": id, "recorded": false, "state": nil, "occurrenceCount": 0, "publication": nil, "reason": "a clearing observation for a fault that was never recorded"}
		} else if !recorded {
			reply = map[string]any{"faultId": id, "recorded": false, "state": textRow(row, "state"), "occurrenceCount": numberRow(row, "occurrence_count"), "reason": "this occurrence was already recorded in this episode", "publication": nil}
		} else {
			var published any
			if before == nil || text(before, "state") == cancelled {
				published = publicationAnswer(context.Background(), l, id, recorded, before != nil)
			}
			if adoptedBefore == nil {
				if adopted, e := goStore.One(context.Background(), "SELECT 1 FROM fault_publications WHERE publication_id = ?", adoptedPublication); e != nil {
					t.Fatal(e)
				} else if adopted != nil {
					published = map[string]any{"publicationId": adoptedPublication, "kind": appendComment, "trigger": triggerOpen, "queued": true, "awaitingTarget": false, "awaitingRecord": false, "reason": "queued"}
				}
			}
			reply = map[string]any{"faultId": id, "recorded": recorded, "state": textRow(row, "state"), "cycle": numberRow(row, "cycle"), "severity": textRow(row, "severity"), "occurrenceCount": numberRow(row, "occurrence_count"), "suppression": loadsMap(textRow(row, "suppression")), "publication": published}
		}
		replies = append(replies, reply)
	}
	var reply any = replies
	if len(replies) == 1 {
		reply = replies[0]
	}
	goRows, err := goStore.All(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]any{}
	for _, table := range goRows {
		name := text(table, "name")
		rows, e := goStore.All(context.Background(), "SELECT * FROM "+name+" ORDER BY rowid")
		if e != nil {
			t.Fatal(e)
		}
		if len(rows) == 0 {
			continue
		}
		entries := make([]any, len(rows))
		for i, r := range rows {
			entry := map[string]any{}
			for _, col := range r {
				entry[col.Name] = col.Value
			}
			entries[i] = entry
		}
		tables[name] = entries
	}
	tableRaw, err := json.Marshal(tables)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err = json.Unmarshal(tableRaw, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, expected["tables"]) {
		var differing []string
		for name, want := range expected["tables"].(map[string]any) {
			if !reflect.DeepEqual(normalized.(map[string]any)[name], want) {
				differing = append(differing, name)
			}
		}
		t.Fatalf("fault table differences (complete rows compared): %s\nGo: %v\nPython: %v", strings.Join(differing, ","), normalized.(map[string]any)["fault_publications"], expected["tables"].(map[string]any)["fault_publications"])
	}
	goRaw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err = json.Unmarshal(goRaw, &got); err != nil {
		t.Fatal(err)
	}
	return got, expected
}
func captureOriginalAssertions(t *testing.T, module, class, method string) []any {
	t.Helper()
	home, err := os.MkdirTemp("/dev/shm", "crw-fault-python-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	// The values the original Python unittest asserted, run in its own disposable tree.
	output := pyAnswer(t, "capture_case.py "+module+"."+class+"."+method, []string{module, class, method}, pyRunPaths(t, home), func() ([]byte, error) {
		root, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			return nil, err
		}
		script := filepath.Join(root, "internal", "relay", "faults", "testdata", "capture_case.py")
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, module, class, method)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(root, "packages", "codex-session-relay", "src")+":"+filepath.Join(root, "packages", "codex-session-relay"))
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("Python test capture: %v: %s", err, output)
		}
		return output, nil
	})
	var result struct {
		Assertions []any
		Problems   []string
	}
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatalf("capture: %s: %v", output, err)
	}
	if len(result.Problems) > 0 {
		t.Fatalf("Python test failed: %s", strings.Join(result.Problems, "\n"))
	}
	return result.Assertions
}
func Test22_FLT_21_PendingAdoptionMaterializesWholeRows(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Notice, "signature": map[string]any{"turn": "adoption"}, "occurrenceKey": "first", "scope": map[string]any{}}
	id := FaultID("crw", "report_omitted", base["signature"].(map[string]any))
	seed := map[string]any{"adoptionSeed": map[string]any{"faultId": id, "externalRef": "CRW-99", "scope": map[string]any{}}}
	second := map[string]any{}
	for k, v := range base {
		second[k] = v
	}
	second["severity"] = Broken
	second["occurrenceKey"] = "second"
	got, want := capturePythonFaults(t, []map[string]any{base, seed, second})
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("adoption replies: Go=%v Python=%v", got, want["reply"])
	}
}

func Test22_FLT_20_RescopeWholeRows(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"turn": "rescope"}, "occurrenceKey": "first", "scope": map[string]any{"workspace": "w", "projectKey": "P1"}}
	moved := map[string]any{}
	for k, v := range base {
		moved[k] = v
	}
	moved["occurrenceKey"] = "second"
	moved["scope"] = map[string]any{"workspace": "w", "projectKey": "P2"}
	got, want := capturePythonFaults(t, []map[string]any{base, moved})
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("whole replies: Go=%v Python=%v", got, want["reply"])
	}
}

func Test22_WorkspaceObservationWholeRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"turn": "legacy"}, "occurrenceKey": "initial", "scope": map[string]any{"workspace": "worktree-1"}}
	// The first write uses the workspace id. The second uses the same id and
	// exercises the canonical alias lookup once an alias exists in the store.
	got, want := capturePythonFaults(t, []map[string]any{o, o})
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("whole replies: Go=%v Python=%v", got, want["reply"])
	}
}

func Test22_FLT_16_PythonPolicyOverrideWholeRows(t *testing.T) {
	policy := map[string]any{"policy": map[string]any{"product": "crw", "fault_class": "report_omitted", "severity": "degraded", "threshold": 1, "reason": "operator override"}}
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": "degraded", "signature": map[string]any{"turn": "override"}, "occurrenceKey": "override-1", "scope": map[string]any{"projectKey": "CRW"}, "detail": "override observation"}
	got, want := capturePythonFaults(t, []map[string]any{policy, o})
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("override whole reply: Go=%v Python=%v", got, want["reply"])
	}
}

func Test22_PythonBuiltInPolicyWholeRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": "degraded", "signature": map[string]any{"turn": "policy"}, "occurrenceKey": "policy-1", "scope": map[string]any{"projectKey": "CRW"}, "detail": "policy observation"}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("policy whole reply: Go=%v Python=%v", got, want["reply"])
	}
}

func Test22_FLT_1_PythonRepeatedReceiptAndRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "an admitted turn settled without a report", "evidence": []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
	got, want := capturePythonFaults(t, []map[string]any{o, o})
	if !reflect.DeepEqual(got, want["reply"]) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want["reply"], "", "  ")
		t.Fatalf("whole repeated receipts differ\ngo: %s\npython: %s", g, w)
	}
}
func Test22_FLT_4_PythonThresholdWholeReceipts(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "delivery_stalled", "severity": Degraded, "signature": map[string]any{"recipient": "01parent-task", "cause": "channel_closed", "attemptState": "withheld_pre_send"}, "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "held", "evidence": []any{map[string]any{"kind": "row", "ref": "deliveries:e1", "observed": map[string]any{"state": "queued"}}}}
	var observations []map[string]any
	for _, key := range []string{"delivery:1", "delivery:2", "delivery:3", "delivery:4"} {
		copy := map[string]any{}
		for name, value := range base {
			copy[name] = value
		}
		copy["occurrenceKey"] = key
		observations = append(observations, copy)
	}
	got, want := capturePythonFaults(t, observations)
	if !reflect.DeepEqual(got, want["reply"]) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want["reply"], "", "  ")
		t.Fatalf("threshold replies differ\ngo: %s\npython: %s", g, w)
	}
}
func Test22_FLT_4_PythonSlidingWindowWholeReceipts(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "delivery_stalled", "severity": Degraded, "signature": map[string]any{"recipient": "01parent-task", "cause": "channel_closed", "attemptState": "withheld_pre_send"}, "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "held", "evidence": []any{map[string]any{"kind": "row", "ref": "deliveries:e1", "observed": map[string]any{"state": "queued"}}}}
	var requests []map[string]any
	for _, key := range []string{"delivery:1", "delivery:2", "delivery:3"} {
		if key == "delivery:2" {
			requests = append(requests, map[string]any{"advanceSeconds": 21601})
		}
		copy := map[string]any{}
		for k, v := range base {
			copy[k] = v
		}
		copy["occurrenceKey"] = key
		requests = append(requests, copy)
	}
	got, want := capturePythonFaults(t, requests)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("whole window receipts differ: Go=%v Python=%v", got, want["reply"])
	}
}
func Test22_FLT_8_PythonUnfiledClearAndPublishedClear(t *testing.T) {
	for _, severity := range []string{Notice, Broken} {
		t.Run(severity, func(t *testing.T) {
			o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": severity, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "active", "scope": map[string]any{"projectKey": "CRW"}}
			clear := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": severity, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "clear", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
			got, want := capturePythonFaults(t, []map[string]any{o, clear})
			if !reflect.DeepEqual(got, want["reply"]) {
				t.Fatalf("whole clear replies differ: go=%v python=%v", got, want["reply"])
			}
		})
	}
}

func Test22_FLT_19_PythonClearSameKeyAndRecurrence(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "same", "scope": map[string]any{"projectKey": "CRW"}}
	clear := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "same", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
	got, want := capturePythonFaults(t, []map[string]any{o, clear, o, o})
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("whole recurrence replies differ: go=%v python=%v", got, want["reply"])
	}
}

func Test22_FLT_15_PythonHealthyReadingHasNoFault(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Notice, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "healthy", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("healthy reading differs: go=%v python=%v", got, want["reply"])
	}
}

func Test22_FLT_4_PythonNoticeWholeReceiptAndRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "observation_unmeasured", "severity": Notice, "signature": map[string]any{"relationship": "rel-1"}, "occurrenceKey": "u1", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("notice whole reply differs: Go=%v Python=%v", got, want["reply"])
	}
}
func Test22_FLT_3_PythonEvidenceDropsNonObjectsBeforeBounding(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "evidence-mixed", "scope": map[string]any{"projectKey": "CRW"}, "detail": "mixed evidence", "evidence": []any{nil, "ignored", map[string]any{"kind": "row", "ref": "first"}, 17, map[string]any{"kind": "row", "ref": "second"}}}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("whole mixed-evidence receipt differs: go=%v python=%v", got, want["reply"])
	}
}

func Test22_FLT_3_PythonEvidenceEntryAndCountBounds(t *testing.T) {
	for name, evidence := range map[string][]any{
		"count": func() []any {
			entries := make([]any, 10)
			for i := range entries {
				entries[i] = map[string]any{"kind": "row", "ref": "record"}
			}
			return entries
		}(),
		"bytes": []any{map[string]any{"kind": "row", "ref": strings.Repeat("x", 4097)}},
	} {
		t.Run(name, func(t *testing.T) {
			o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": name, "scope": map[string]any{"projectKey": "CRW"}, "evidence": evidence}
			got, want := capturePythonFault(t, o)
			if !reflect.DeepEqual(got, want["reply"]) {
				t.Fatalf("whole bounded-evidence receipt differs: go=%v python=%v", got, want["reply"])
			}
		})
	}
}

func Test22_FLT_3_PythonEvidenceWholeRows(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "evidence": []any{map[string]any{"kind": "row", "ref": "sync_outbox:s1", "observed": map[string]any{"state": "failed"}}}}
	got, want := capturePythonFault(t, base)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("evidence reply differs Go=%v Python=%v", got, want["reply"])
	}
}
func Test22_FLT_2_PythonUnregisteredClassWholeRefusal(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "invented", "severity": Broken, "signature": map[string]any{"a": 1}, "occurrenceKey": "a"}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		t.Fatalf("refusal differs Go=%v Python=%v", got, want["reply"])
	}
}
func Test22_FLT_2_PythonOriginalClassClearAssertions(t *testing.T) {
	want := captureOriginalAssertions(t, "test_faults", "TheFaultPathReachesNoNetwork", "test_every_registered_class_declares_what_clears_it")
	got := []any{len(classNames("")) > 0}
	for _, name := range []string{"delivery_stalled", "record_sync_failed", "observation_stalled", "report_omitted", "observation_unmeasured", "delivery_refused", "managed_start_failed"} {
		policy, _ := classLookup(name)
		got = append(got, policy.clears != "")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("class declarations: Go=%v Python=%v", got, want)
	}
}
func Test22_FLT_3_PythonOriginalEvidenceAssertions(t *testing.T) {
	want := captureOriginalAssertions(t, "test_faults", "Evidence", "test_an_occurrence_keeps_what_was_observed_rather_than_a_pointer")
	items := []any{map[string]any{"kind": "row", "ref": "sync_outbox:s1", "observed": map[string]any{"state": "failed"}}}
	got := []any{items[0].(map[string]any)["observed"].(map[string]any)["state"], evidenceDigest(items)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Python assertions: Go=%v Python=%v", got, want)
	}
}
func Test22_FLT_1_PythonOriginalSignatureAssertion(t *testing.T) {
	got := FaultID("crw", "report_omitted", map[string]any{"a": 1, "b": 2})
	want := captureOriginalAssertions(t, "test_faults", "Identity", "test_signature_order_does_not_change_identity")
	if len(want) != 1 || got != want[0] {
		t.Fatalf("signature hash Go %s Python %+v", got, want)
	}
}
func Test22_FLT_1_LivePythonWholeReceipt(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "an admitted turn settled without a report", "evidence": []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
	got, want := capturePythonFault(t, o)
	if !reflect.DeepEqual(got, want["reply"]) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want["reply"], "", "  ")
		t.Fatalf("whole receipt differs\ngo: %s\npython: %s", g, w)
	}
}
