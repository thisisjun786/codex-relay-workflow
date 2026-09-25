package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pythonDeliveryRecords drives Python's DeliveryService/AckService through each conformance
// scenario of test_schema_conformance.py (one fresh case per scenario) and returns the stores.
const pythonDeliveryRecordsScript = `
import json
from tests.test_schema_conformance import Attempts, Acknowledgements, Verdicts, ReverseDirectionExclusion
from tests.support import PARENT, CHILD
from codex_session_relay import identity

class Case(Attempts, Acknowledgements, Verdicts, ReverseDirectionExclusion):
    def runTest(self): pass

def fresh():
    c = Case(); c.setUp(); return c

out = []
def done(c, kind, **fields):
    c.store.close()
    out.append(dict(kind=kind, db=str(c.store.path), **fields))

for outcome, expected, issue in [(None,"dispatched","REL-dispatch"),("busy","deferred_busy","REL-busy"),("read_fail","withheld_pre_send","REL-read"),("resume_fail","withheld_pre_send","REL-resume"),("turn_start_fail","held_uncertain","REL-turnstart"),("initialize_fail","held_uncertain","REL-init"),("transport_unknown","held_uncertain","REL-transport"),("in_progress","held_uncertain","REL-unfinished"),("approval_policy","inbox_only","REL-inbox")]:
    c = fresh()
    r = c._attempt(outcome, issue=issue, approval="on-request" if outcome == "approval_policy" else "never")
    done(c, "attempt", key=r["requestId"], expect=expected)
c = fresh(); r = c._attempt("in_progress", issue="REL-recon")
c.adapter.start_turn(PARENT, status="completed", text=f"...{r['requestId']}...")
c.reconciler.reconcile_attempt(r["requestId"], c.adapter)
done(c, "reconciled", key=r["requestId"], expect="turn_found")
c = fresh(); _rel, ev, turn = c._dispatched("REL-ack-yes")
c.ack.acknowledge(ev, ack_turn_id=turn.turn_id, ack_proof=identity.ack_proof(ev, turn.turn_id), accepted=True, adapter=c.adapter)
done(c, "ack", key=ev, expect="accepted")
c = fresh(); rel, ev, turn = c._dispatched("REL-ack-no")
c.registry.open_generation(rel["relationshipId"], dispatch_request_id="later", reason="needs_changes_revision", dispatch_turn_id="later-turn")
c.ack.acknowledge(ev, ack_turn_id=turn.turn_id, ack_proof=identity.ack_proof(ev, turn.turn_id), accepted=False, rejection_reason="stale_generation", adapter=c.adapter)
done(c, "ack", key=ev, expect="stale_generation")
for v in ("verified", "unverified", "aborted"):
    c = fresh(); _r, e = c._acknowledged(f"REL-v-{v}")
    c.ack.record_verdict(e, verdict=v, verdict_turn_id=f"verdict-{v}")
    done(c, "verdict", key=e, expect=v)
c = fresh(); rel, e = c._acknowledged("REL-v-needs", recipients=[PARENT, CHILD])
c.ack.record_verdict(e, verdict="needs_changes", verdict_turn_id="verdict-needs")
done(c, "verdict", key=e, expect="needs_changes", relationship=rel["relationshipId"], child=CHILD)
print(json.dumps(out))
`

type deliveryRecordStore struct {
	Kind         string `json:"kind"`
	DB           string `json:"db"`
	Key          string `json:"key"`
	Expect       string `json:"expect"`
	Relationship string `json:"relationship"`
	Child        string `json:"child"`
}

func pythonDeliveryRecords(t *testing.T) []deliveryRecordStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("TMPDIR", root)
	out := pythonStoreValueIn(t, filepath.Join(repositoryRoot(t), "packages/codex-session-relay"), pythonDeliveryRecordsScript)
	var stores []deliveryRecordStore
	if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n")+1:]), &stores); err != nil {
		t.Fatalf("python output %q: %v", out, err)
	}
	return stores
}

func openRecorded(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSchemaConformance_python_properties(t *testing.T) {
	ctx := context.Background()
	var cases []schemaCase
	test := ""
	positive := func(schema, label, record string) {
		cases = append(cases, schemaCase{Test: test, Schema: schema, Label: label, Instance: json.RawMessage(record), Valid: true})
	}
	negative := func(schema, label string, instance json.RawMessage) {
		cases = append(cases, schemaCase{Test: test, Schema: schema, Label: label, Instance: instance})
	}
	// Replaced only when the needs_changes verdict is recorded; otherwise the scenario never ran.
	excluded := func(t *testing.T) { t.Helper(); t.Fatal("the needs_changes verdict scenario did not run") }

	// Relationships: both anchor states, from the Go registry.
	test = "test_both_anchor_states_validate"
	f := newIntakeFixture(t)
	pending := f.register(registerOptions{issue: "REL-PENDING"})
	pendingRecord, err := f.store.RelationshipRecord(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	positive("relationship", "anchor_pending relationship", pendingRecord)
	if !strings.Contains(pendingRecord, `"anchorState": "anchor_pending", "dispatchTurnId": null`) {
		t.Fatalf("pending record %s", pendingRecord)
	}
	if err := f.store.BindAnchor(ctx, pending.ID, 1, fixtureTurn, "dispatch_receipt", fixtureNow); err != nil {
		t.Fatal(err)
	}
	boundRecord, err := f.store.RelationshipRecord(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	positive("relationship", "bound relationship", boundRecord)
	if !strings.Contains(boundRecord, `"anchorState": "bound", "dispatchTurnId": "`+fixtureTurn+`", "openedAt": "`+fixtureNow+`", "boundAt": "`+fixtureNow+`"`) {
		t.Fatalf("bound record %s", boundRecord)
	}
	test = "test_negatives_are_rejected/relationship"
	relationshipNegatives := []struct {
		label  string
		change func(map[string]any)
	}{
		{"bound anchor with no turn id", func(r map[string]any) { r["generations"].([]any)[0].(map[string]any)["dispatchTurnId"] = nil }},
		{"malformed relationship id", func(r map[string]any) { r["relationshipId"] = "not-a-relationship-id" }},
		{"unknown property", func(r map[string]any) { r["surprise"] = 1 }},
		{"empty artifact roots", func(r map[string]any) { r["authorizedScope"].(map[string]any)["artifactRoots"] = []any{} }},
	}
	for _, n := range relationshipNegatives {
		negative("relationship", n.label, mutated(t, boundRecord, n.change))
	}

	// Receipts: every producer and outcome branch, as persisted by the Go intake.
	test = "test_every_producer_and_outcome_branch_validates"
	receipts := f.conformanceReceipts(t)
	for label, record := range receipts {
		positive("completion-receipt", label, record)
	}
	ready := receipts["child ready_for_review"]
	test = "test_negatives_are_rejected/completion-receipt"
	receiptNegatives := []struct {
		label  string
		change func(map[string]any)
	}{
		{"ready with a null manifest", func(r map[string]any) { r["manifest"] = nil }},
		{"ready with the sentinel digest", func(r map[string]any) { r["revisionHash"] = NoDeliverable }},
		{"zero attempt", func(r map[string]any) { r["attempt"] = 0 }},
		{"boolean generation", func(r map[string]any) { r["executionGeneration"] = true }},
		{"malformed event id", func(r map[string]any) { r["eventId"] = "nope" }},
		{"unknown outcome", func(r map[string]any) { r["outcome"] = "finished" }},
		{"unknown property", func(r map[string]any) { r["surprise"] = 1 }},
		{"relative manifest path", func(r map[string]any) { r["manifest"].([]any)[0].(map[string]any)["path"] = "relative/path" }},
		{"unknown turn status", func(r map[string]any) { r["turnRef"].(map[string]any)["turnStatus"] = "gone" }},
	}
	for _, n := range receiptNegatives {
		negative("completion-receipt", n.label, mutated(t, ready, n.change))
	}
	daemon := receipts["daemon failed"]
	negative("completion-receipt", "daemon observation with an attempt", mutated(t, daemon, func(r map[string]any) { r["attempt"] = 1 }))
	negative("completion-receipt", "daemon failed on an interrupted turn", mutated(t, daemon, func(r map[string]any) { r["turnRef"].(map[string]any)["turnStatus"] = "interrupted" }))

	// Attempts, acknowledgements and verdicts are produced by the delivery layer (todo 21);
	// here Go reads what the real Python services persisted and validates the stored records.
	var dispatched, acceptedAck, verifiedVerdict string
	for _, recorded := range pythonDeliveryRecords(t) {
		s := openRecorded(t, recorded.DB)
		switch recorded.Kind {
		case "attempt":
			test = "test_every_delivery_state_branch_validates"
			attempt, err := s.Attempt(ctx, recorded.Key)
			if err != nil || attempt.State.String != recorded.Expect || !strings.Contains(attempt.Record.String, `"deliveryState": "`+recorded.Expect+`"`) {
				t.Fatalf("%s attempt %+v: %v", recorded.Expect, attempt, err)
			}
			positive("delivery-attempt", recorded.Expect+" attempt", attempt.Record.String)
			if recorded.Expect == "dispatched" {
				dispatched = attempt.Record.String
			}
			if recorded.Expect == "inbox_only" && !strings.Contains(attempt.Record.String, `"recipientApprovalPolicy": "on-request"`) {
				t.Fatalf("inbox record %s", attempt.Record.String)
			}
		case "reconciled":
			test = "test_a_reconciled_attempt_keeps_a_valid_record"
			attempt, err := s.Attempt(ctx, recorded.Key)
			if err != nil {
				t.Fatal(err)
			}
			positive("delivery-attempt", "reconciled held_uncertain attempt", attempt.Record.String)
			var stored struct {
				Reconciliation struct {
					AffirmativeEvidence string `json:"affirmativeEvidence"`
				} `json:"reconciliation"`
			}
			if err := json.Unmarshal([]byte(attempt.Record.String), &stored); err != nil || stored.Reconciliation.AffirmativeEvidence != recorded.Expect {
				t.Fatalf("reconciled record %s: %v", attempt.Record.String, err)
			}
		case "ack":
			test = "test_accepted_and_rejected_branches_validate"
			ack, err := s.Ack(ctx, recorded.Key)
			if err != nil {
				t.Fatal(err)
			}
			positive("acknowledgement", recorded.Expect+" acknowledgement", ack.Record)
			if recorded.Expect == "accepted" {
				acceptedAck = ack.Record
				if !ack.Accepted || !strings.Contains(ack.Record, `"rejectionReason": null`) {
					t.Fatalf("accepted ack %+v", ack)
				}
			} else if ack.Accepted || ack.RejectionReason.String != recorded.Expect {
				t.Fatalf("rejected ack %+v", ack)
			}
		case "verdict":
			test = "test_all_four_verdicts_validate"
			verdict, err := s.Verdict(ctx, recorded.Key)
			if err != nil || verdict.Decision != recorded.Expect {
				t.Fatalf("verdict %+v: %v", verdict, err)
			}
			positive("verification-verdict", recorded.Expect+" verdict", verdict.Record)
			if recorded.Expect == "verified" {
				verifiedVerdict = verdict.Record
			}
			if recorded.Expect == "needs_changes" {
				excluded = func(t *testing.T) { requireExcludedRevision(t, s, recorded, verdict) }
			}
		}
	}
	test = "test_negatives_are_rejected/delivery-attempt"
	attemptNegatives := []struct {
		label  string
		change func(map[string]any)
	}{
		{"dispatched with no turn", func(r map[string]any) { r["turnId"] = nil }},
		{"uncertain but retry safe", func(r map[string]any) { r["deliveryState"], r["retrySafe"] = "held_uncertain", true }},
		{"retry safe after turn/start", func(r map[string]any) {
			r["retrySafe"], r["sendAttempted"], r["transportReceiptStatus"], r["deliveryState"], r["failedOperation"] = true, "no", "failed", "withheld_pre_send", "turn/start"
		}},
		{"unfinished but certain", func(r map[string]any) {
			r["transportReceiptStatus"], r["sendAttempted"] = "in_progress_or_unknown", "yes"
		}},
		{"malformed request id", func(r map[string]any) { r["requestId"] = "del-x-a1" }},
		{"unknown property", func(r map[string]any) { r["surprise"] = 1 }},
	}
	for _, n := range attemptNegatives {
		negative("delivery-attempt", n.label, mutated(t, dispatched, n.change))
	}
	test = "test_negatives_are_rejected/acknowledgement"
	ackNegatives := []struct {
		label  string
		change func(map[string]any)
	}{
		{"accepted with a reason", func(r map[string]any) { r["rejectionReason"] = "duplicate_event" }},
		{"rejected with no reason", func(r map[string]any) { r["accepted"] = false }},
		{"malformed proof", func(r map[string]any) { r["ackProof"] = "short" }},
		{"empty ack turn", func(r map[string]any) { r["ackTurnId"] = "" }},
		{"unknown property", func(r map[string]any) { r["surprise"] = 1 }},
	}
	for _, n := range ackNegatives {
		negative("acknowledgement", n.label, mutated(t, acceptedAck, n.change))
	}
	test = "test_negatives_are_rejected/verification-verdict"
	verdictNegatives := []struct {
		label  string
		change func(map[string]any)
	}{
		{"unknown verdict", func(r map[string]any) { r["verdict"] = "maybe" }},
		{"missing turn", func(r map[string]any) { delete(r, "verdictTurnId") }},
		{"malformed event id", func(r map[string]any) { r["eventId"] = "nope" }},
		{"unknown property", func(r map[string]any) { r["surprise"] = 1 }},
	}
	for _, n := range verdictNegatives {
		negative("verification-verdict", n.label, mutated(t, verifiedVerdict, n.change))
	}

	failures, counts := validateAgainstSchemas(t, cases)
	for _, name := range []string{
		"test_both_anchor_states_validate", "test_negatives_are_rejected/relationship",
		"test_every_producer_and_outcome_branch_validates", "test_negatives_are_rejected/completion-receipt",
		"test_every_delivery_state_branch_validates", "test_a_reconciled_attempt_keeps_a_valid_record", "test_negatives_are_rejected/delivery-attempt",
		"test_accepted_and_rejected_branches_validate", "test_negatives_are_rejected/acknowledgement",
		"test_all_four_verdicts_validate", "test_negatives_are_rejected/verification-verdict",
	} {
		t.Run(name, func(t *testing.T) {
			for _, failure := range failures[name] {
				t.Error(failure)
			}
		})
	}
	t.Run("test_the_excluded_records_are_counted_and_described", func(t *testing.T) { excluded(t) })
	t.Run("test_conformance_actually_ran", func(t *testing.T) {
		// Every schema passed Draft7Validator.check_schema in the validator run, and no schema
		// may finish having validated nothing: a skip is not a pass.
		for _, name := range recordSchemas {
			if counts[name] == 0 {
				t.Errorf("no instance was validated against %s", name)
			}
		}
		if counts["delivery-attempt"] != 10 || counts["verification-verdict"] != 4 || counts["acknowledgement"] != 2 || counts["relationship"] != 2 || counts["completion-receipt"] != 7 {
			t.Errorf("validated counts %v", counts)
		}
	})
}

// conformanceReceipts accepts every producer/outcome branch through the Go intake and returns
// each persisted receipt record.
func (f *intakeFixture) conformanceReceipts(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	records := map[string]string{}
	stored := func(label string, got StoredReceipt, err error) {
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		event, err := f.store.Event(ctx, got.EventID)
		if err != nil {
			t.Fatal(err)
		}
		records[label] = event.Receipt
	}
	payload := f.readyPayload(f.relationship, []string{f.artifact("ready.txt", "deliverable completed")}, 1, assignedTurn("completed"))
	got, err := f.acceptPayload(payload)
	stored("child ready_for_review", got, err)
	staged := f.register(registerOptions{issue: "REL-STAGED", dispatchTurn: nullString(fixtureTurn)})
	payload = f.readyPayload(staged, []string{f.artifact("staged.txt", "deliverable inProgress")}, 1, assignedTurn("inProgress"))
	got, err = f.acceptPayload(payload)
	stored("staged inProgress", got, err)
	for _, branch := range []struct{ outcome, status string }{{"failed", "failed"}, {"interrupted", "interrupted"}, {"blocked_needs_input", "completed"}} {
		other := f.register(registerOptions{issue: "REL-" + branch.outcome, dispatchTurn: nullString(fixtureTurn)})
		got, err := f.acceptPayload(f.executionPayload(other, branch.outcome, assignedTurn(branch.status)))
		stored("child "+branch.outcome, got, err)
	}
	for _, status := range []string{"failed", "interrupted"} {
		observed := f.register(registerOptions{issue: "REL-daemon-" + status, dispatchTurn: nullString(fixtureTurn)})
		got, err := f.intake.DaemonObservation(ctx, observed.ID, assignedTurn(status))
		stored("daemon "+status, got, err)
		for _, want := range []string{`"attempt": null`, `"manifest": null`, `"revisionHash": "` + NoDeliverable + `"`, `"turnStatus": "` + status + `"`} {
			if !strings.Contains(records["daemon "+status], want) {
				t.Fatalf("daemon %s record %s lacks %s", status, records["daemon "+status], want)
			}
		}
	}
	return records
}

// requireExcludedRevision is test_the_excluded_records_are_counted_and_described: the
// needs_changes verdict queued a revision request outside the parent-facing schemas.
func requireExcludedRevision(t *testing.T, s *Store, recorded deliveryRecordStore, verdict Verdict) {
	t.Helper()
	ctx := context.Background()
	if !verdict.NextGeneration.Valid || verdict.NextGeneration.Int64 != 2 {
		t.Fatalf("next generation %+v", verdict.NextGeneration)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT event_id,kind,recipient_task_id FROM deliveries WHERE relationship_id=?`, recorded.Relationship)
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct{ event, kind, recipient string }
	var deliveries []delivery
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.event, &d.kind, &d.recipient); err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	// The store has one connection, so each event is read after the cursor is closed.
	var excluded, included int
	for _, d := range deliveries {
		event, kind, recipient := d.event, d.kind, d.recipient
		switch kind {
		case "revision_request":
			excluded++
			row, err := s.Event(ctx, event)
			if err != nil || row.Producer != "relay" || row.Outcome != "revision_request" || row.Generation != 2 || recipient != recorded.Child {
				t.Fatalf("revision event %+v recipient %s: %v", row, recipient, err)
			}
			var receipt struct {
				SupersedesEvent string `json:"supersedesEvent"`
				Note            string `json:"note"`
			}
			if err := json.Unmarshal([]byte(row.Receipt), &receipt); err != nil || receipt.SupersedesEvent != recorded.Key || !strings.Contains(receipt.Note, "contract v1 defines no record") {
				t.Fatalf("revision receipt %s: %v", row.Receipt, err)
			}
		case "completion_event":
			included++
		default:
			t.Fatalf("an unrecognised delivery kind %q fails rather than passing", kind)
		}
	}
	if excluded == 0 || included == 0 {
		t.Fatalf("excluded %d included %d", excluded, included)
	}
}

// The Go relationship record is byte-identical to Python's registry.contract_record over the
// same database, so the conformance above is about the record Python would have produced.
func TestRelationshipRecord_matches_python_contract_record(t *testing.T) {
	f := newIntakeFixture(t)
	got, err := f.store.RelationshipRecord(context.Background(), f.relationship.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store.DB = openRecorded(t, f.store.Path).DB
	want := pythonStoreValue(t, `import json, sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.registry import Registry, contract_record
from codex_session_relay.store import Store
store = Store(sys.argv[1])
print(json.dumps(contract_record(Registry(store, FakeClock()).get(sys.argv[2]))))
store.close()`, f.store.Path, f.relationship.ID)
	if got != want {
		t.Fatalf("go\n%s\npython\n%s", got, want)
	}
}

func TestSchemaFiles_python_packaged_contract(t *testing.T) {
	schema := filepath.Join(repositoryRoot(t), "contract", "schema")
	t.Run("test_the_packaged_schemas_match_the_contract", func(t *testing.T) {
		expected := map[string]string{"acknowledgement.json": "193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c", "completion-receipt.json": "8111438e60b46b209a33902dd9080426953dfaaf2a7025cb47c2336d72b49317", "delivery-attempt.json": "e821647e35bee9179321332d8b7df06f0fc61f2da49c7b74fb6128b8802650d1", "relationship.json": "c8ebaf4559fa1ac6df26d98c4214938caf78bd8c61659bce026da6a3595a4b90", "verification-verdict.json": "0b3f8f4b061cff2992fc60a7c1f45dec6f803116894735751c40df3a8d356af9"}
		for name, want := range expected {
			data, err := os.ReadFile(filepath.Join(schema, name))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != want {
				t.Fatalf("schema drift: %s", name)
			}
		}
	})
	t.Run("test_the_record_is_closed_and_its_criteria_items_are_not", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(schema, "verification-verdict.json"))
		if err != nil {
			t.Fatal(err)
		}
		var verdict struct {
			AdditionalProperties *bool `json:"additionalProperties"`
			Properties           struct {
				Criteria struct {
					Items map[string]json.RawMessage `json:"items"`
				} `json:"criteria"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(data, &verdict); err != nil {
			t.Fatal(err)
		}
		if verdict.AdditionalProperties == nil || *verdict.AdditionalProperties {
			t.Fatal("the verdict record must be closed")
		}
		if _, closed := verdict.Properties.Criteria.Items["additionalProperties"]; closed {
			t.Fatal("criteria items were closed")
		}
	})
}
