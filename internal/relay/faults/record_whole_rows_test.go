package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// recordFault records one observation, or a list of steps, through Go's ledger in a disposable
// state tree and compares the complete reply and every populated fault_* table with the golden,
// not a hand-selected subset of fields or reason strings.
func recordFault(t *testing.T, observation map[string]any) {
	recordFaults(t, []map[string]any{observation})
}
func recordFaults(t *testing.T, observations []map[string]any) {
	t.Helper()
	home, err := os.MkdirTemp("/dev/shm", "crw-fault-rows-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	raw, err := json.Marshal(observations)
	if err != nil {
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
	tableRaw, err := json.Marshal(testsupport.NonEmpty(testsupport.TableRows(t, goStore.DB, "name LIKE 'fault_%'")))
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err = json.Unmarshal(tableRaw, &normalized); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "fault tables", []string{string(raw)}, runPathsOf(t, home), normalized)
	goRaw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err = json.Unmarshal(goRaw, &got); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "reply", []string{string(raw)}, runPathsOf(t, home), got)
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
	recordFaults(t, []map[string]any{base, seed, second})
}

func Test22_FLT_20_RescopeWholeRows(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"turn": "rescope"}, "occurrenceKey": "first", "scope": map[string]any{"workspace": "w", "projectKey": "P1"}}
	moved := map[string]any{}
	for k, v := range base {
		moved[k] = v
	}
	moved["occurrenceKey"] = "second"
	moved["scope"] = map[string]any{"workspace": "w", "projectKey": "P2"}
	recordFaults(t, []map[string]any{base, moved})
}

func Test22_WorkspaceObservationWholeRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"turn": "legacy"}, "occurrenceKey": "initial", "scope": map[string]any{"workspace": "worktree-1"}}
	// The first write uses the workspace id. The second uses the same id and
	// exercises the canonical alias lookup once an alias exists in the store.
	recordFaults(t, []map[string]any{o, o})
}

func Test22_FLT_16_PythonPolicyOverrideWholeRows(t *testing.T) {
	policy := map[string]any{"policy": map[string]any{"product": "crw", "fault_class": "report_omitted", "severity": "degraded", "threshold": 1, "reason": "operator override"}}
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": "degraded", "signature": map[string]any{"turn": "override"}, "occurrenceKey": "override-1", "scope": map[string]any{"projectKey": "CRW"}, "detail": "override observation"}
	recordFaults(t, []map[string]any{policy, o})
}

func Test22_PythonBuiltInPolicyWholeRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": "degraded", "signature": map[string]any{"turn": "policy"}, "occurrenceKey": "policy-1", "scope": map[string]any{"projectKey": "CRW"}, "detail": "policy observation"}
	recordFault(t, o)
}

func Test22_FLT_1_PythonRepeatedReceiptAndRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "an admitted turn settled without a report", "evidence": []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
	recordFaults(t, []map[string]any{o, o})
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
	recordFaults(t, observations)
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
	recordFaults(t, requests)
}
func Test22_FLT_8_PythonUnfiledClearAndPublishedClear(t *testing.T) {
	goldenParent(t)
	for _, severity := range []string{Notice, Broken} {
		t.Run(severity, func(t *testing.T) {
			o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": severity, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "active", "scope": map[string]any{"projectKey": "CRW"}}
			clear := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": severity, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "clear", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
			recordFaults(t, []map[string]any{o, clear})
		})
	}
}

func Test22_FLT_19_PythonClearSameKeyAndRecurrence(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "same", "scope": map[string]any{"projectKey": "CRW"}}
	clear := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "same", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
	recordFaults(t, []map[string]any{o, clear, o, o})
}

func Test22_FLT_15_PythonHealthyReadingHasNoFault(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Notice, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "healthy", "scope": map[string]any{"projectKey": "CRW"}, "cleared": true}
	recordFault(t, o)
}

func Test22_FLT_4_PythonNoticeWholeReceiptAndRows(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "observation_unmeasured", "severity": Notice, "signature": map[string]any{"relationship": "rel-1"}, "occurrenceKey": "u1", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}}
	recordFault(t, o)
}
func Test22_FLT_3_PythonEvidenceDropsNonObjectsBeforeBounding(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "evidence-mixed", "scope": map[string]any{"projectKey": "CRW"}, "detail": "mixed evidence", "evidence": []any{nil, "ignored", map[string]any{"kind": "row", "ref": "first"}, 17, map[string]any{"kind": "row", "ref": "second"}}}
	recordFault(t, o)
}

func Test22_FLT_3_PythonEvidenceEntryAndCountBounds(t *testing.T) {
	goldenParent(t)
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
			recordFault(t, o)
		})
	}
}

func Test22_FLT_3_PythonEvidenceWholeRows(t *testing.T) {
	base := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "evidence": []any{map[string]any{"kind": "row", "ref": "sync_outbox:s1", "observed": map[string]any{"state": "failed"}}}}
	recordFault(t, base)
}
func Test22_FLT_2_PythonUnregisteredClassWholeRefusal(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "invented", "severity": Broken, "signature": map[string]any{"a": 1}, "occurrenceKey": "a"}
	recordFault(t, o)
}
func Test22_FLT_2_PythonOriginalClassClearAssertions(t *testing.T) {
	got := []any{len(classNames("")) > 0}
	for _, name := range []string{"delivery_stalled", "record_sync_failed", "observation_stalled", "report_omitted", "observation_unmeasured", "delivery_refused", "managed_start_failed"} {
		policy, _ := classLookup(name)
		got = append(got, policy.clears != "")
	}
	checkGolden(t, "assertions", nil, nil, got)
}
func Test22_FLT_3_PythonOriginalEvidenceAssertions(t *testing.T) {
	items := []any{map[string]any{"kind": "row", "ref": "sync_outbox:s1", "observed": map[string]any{"state": "failed"}}}
	got := []any{items[0].(map[string]any)["observed"].(map[string]any)["state"], evidenceDigest(items)}
	checkGolden(t, "assertions", nil, nil, got)
}
func Test22_FLT_1_PythonOriginalSignatureAssertion(t *testing.T) {
	got := FaultID("crw", "report_omitted", map[string]any{"a": 1, "b": 2})
	checkGolden(t, "assertions", nil, nil, []any{got})
}
func Test22_FLT_1_LivePythonWholeReceipt(t *testing.T) {
	o := map[string]any{"schema": SchemaObservation, "product": "crw", "faultClass": "report_omitted", "severity": Broken, "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "an admitted turn settled without a report", "evidence": []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
	recordFault(t, o)
}
