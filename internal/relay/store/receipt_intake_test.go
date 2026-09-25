package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// test_receipts.py: ReadyForReview, ExecutionOnly, DaemonObservation, DuplicateAndRevision,
// GenerationAndScopeRefusals, PathBindingMinimum and Artifacts, each through the intake.
func TestReceiptIntake_python_ready_for_review(t *testing.T) {
	t.Run("test_a_valid_receipt_over_real_bytes_is_accepted_and_stored", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "the deliverable")}, 1, assignedTurn("completed"))
		stored, err := f.acceptPayload(payload)
		if err != nil || stored.Duplicate || !strings.Contains(stored.Record, `"outcome": "ready_for_review"`) {
			t.Fatalf("stored %+v: %v", stored, err)
		}
		event, err := f.store.Event(context.Background(), payload.EventID)
		if err != nil || event.PathBinding.String != string(BestEffortDetection) || event.Receipt != stored.Record {
			t.Fatalf("event %+v: %v", event, err)
		}
	})
	for _, tc := range []struct{ name, rewrite string }{
		{"test_a_completion_phrase_with_no_matching_bytes_is_refused", "something else entirely"},
		{"test_a_truncated_artifact_is_refused", "the full"},
		{"test_a_missing_artifact_is_refused", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeFixture(t)
			path := f.artifact("out.txt", "the full deliverable contents")
			payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
			if tc.rewrite == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tc.rewrite), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := f.acceptPayload(payload)
			requireReason(t, err, ReasonManifestUnverified)
		})
	}
	t.Run("test_a_reviewable_receipt_with_no_manifest_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.Manifest = nil
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonManifestRequired)
	})
	t.Run("test_a_reviewable_receipt_carrying_the_sentinel_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.RevisionHash = NoDeliverable
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonOutcomeInconsistent)
	})
	t.Run("test_a_forged_digest_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.RevisionHash = strings.Repeat("f", 64)
		f.rederive(&payload)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonRevisionMismatch)
	})
	t.Run("test_a_forged_event_id_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.EventID = strings.Repeat("0", 32)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonEventIDMismatch)
	})
	t.Run("test_a_turnref_that_disagrees_with_the_observation_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		_, err := f.accept(payload.bytes(t), TurnReference{ThreadID: fixtureChild, TurnID: "a-different-turn", Status: "completed"})
		requireReason(t, err, ReasonTurnRefMismatch)
	})
	t.Run("test_every_refusal_is_recorded_for_an_operator", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.EventID = strings.Repeat("0", 32)
		if _, err := f.acceptPayload(payload); err == nil {
			t.Fatal("forged event accepted")
		}
		refusals, err := f.store.Refusals(context.Background(), f.relationship.ID)
		if err != nil || len(refusals) != 1 || refusals[0].Reason != ReasonEventIDMismatch || refusals[0].EventID.String != payload.EventID {
			t.Fatalf("refusals %+v: %v", refusals, err)
		}
	})
}

func TestReceiptIntake_python_execution_only_and_daemon(t *testing.T) {
	ctx := context.Background()
	t.Run("test_a_child_failure_is_accepted_with_the_sentinel", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "failed", assignedTurn("failed"))
		stored, err := f.acceptPayload(payload)
		if err != nil || stored.Duplicate || !strings.Contains(stored.Record, `"revisionHash": "`+NoDeliverable+`"`) {
			t.Fatalf("stored %+v: %v", stored, err)
		}
	})
	t.Run("test_a_child_failure_carrying_a_manifest_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		entries, err := BuildManifest([]string{f.artifact("out.txt", "payload")}, []string{f.root})
		if err != nil {
			t.Fatal(err)
		}
		payload := f.executionPayload(f.relationship, "failed", assignedTurn("failed"))
		payload.Manifest = entries
		_, err = f.acceptPayload(payload)
		requireReason(t, err, ReasonManifestForbidden)
	})
	t.Run("test_a_child_interruption_carrying_a_digest_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "interrupted", assignedTurn("interrupted"))
		payload.RevisionHash = strings.Repeat("a", 64)
		f.rederive(&payload)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonOutcomeInconsistent)
	})
	t.Run("test_a_child_receipt_without_its_rerun_counter_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "failed", assignedTurn("failed"))
		payload.Attempt = nil
		f.rederive(&payload)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonOutcomeInconsistent)
	})
	t.Run("test_a_daemon_may_synthesize_failure_and_interruption", func(t *testing.T) {
		f := newIntakeFixture(t)
		for _, status := range []string{"failed", "interrupted"} {
			stored, err := f.intake.DaemonObservation(ctx, f.relationship.ID, assignedTurn(status))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"producer": "daemon_observation"`, `"outcome": "` + status + `"`, `"attempt": null`, `"revisionHash": "` + NoDeliverable + `"`, `"manifest": null`} {
				if !strings.Contains(stored.Record, want) {
					t.Fatalf("%s receipt %s lacks %s", status, stored.Record, want)
				}
			}
		}
	})
	t.Run("test_a_daemon_cannot_synthesize_a_reviewable_result", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, err := f.intake.DaemonObservation(ctx, f.relationship.ID, assignedTurn("completed"))
		requireReason(t, err, ReasonProducerNotPermitted)
	})
	t.Run("test_a_daemon_cannot_claim_a_task_is_waiting_for_approval", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "blocked_needs_input", assignedTurn("completed"))
		payload.Producer = ProducerDaemon
		payload.Attempt = nil
		f.rederive(&payload)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonProducerNotPermitted)
	})
	t.Run("test_the_daemon_observation_key_deduplicates_its_own_stream", func(t *testing.T) {
		f := newIntakeFixture(t)
		rid := f.relationship.ID
		for range 3 {
			if err := f.intake.RecordObservation(ctx, assignedTurn("failed"), Failed, nullString(rid)); err != nil {
				t.Fatal(err)
			}
		}
		if n := f.count(`SELECT COUNT(*) FROM observations`); n != 1 {
			t.Fatalf("observations %d", n)
		}
	})
}

func TestReceiptIntake_python_duplicates_generations_and_scope(t *testing.T) {
	ctx := context.Background()
	t.Run("test_re_observing_one_revision_collapses", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		first, err := f.acceptPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		second, err := f.acceptPayload(payload)
		if err != nil || first.Duplicate || !second.Duplicate {
			t.Fatalf("first %+v second %+v: %v", first, second, err)
		}
		if n := f.count(`SELECT COUNT(*) FROM events`); n != 1 {
			t.Fatalf("events %d", n)
		}
		if n := f.count(`SELECT observation_count FROM events WHERE event_id=?`, payload.EventID); n != 2 {
			t.Fatalf("observation count %d", n)
		}
	})
	t.Run("test_a_new_revision_is_a_separate_retained_verification_target", func(t *testing.T) {
		f := newIntakeFixture(t)
		path := f.artifact("out.txt", "first revision")
		first := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
		if _, err := f.acceptPayload(first); err != nil {
			t.Fatal(err)
		}
		reference := filepath.Join(f.tmp, "frozen-first")
		if err := FreezeManifest(first.Manifest, reference); err != nil {
			t.Fatal(err)
		}
		f.artifact("out.txt", "second revision")
		second := f.readyPayload(f.relationship, []string{path}, 2, assignedTurn("completed"))
		if _, err := f.acceptPayload(second); err != nil {
			t.Fatal(err)
		}
		if first.EventID == second.EventID || f.count(`SELECT COUNT(*) FROM events`) != 2 {
			t.Fatalf("revisions collapsed: %s %s", first.EventID, second.EventID)
		}
		if problems := VerifyFrozen(reference, first.Manifest); len(problems) != 0 {
			t.Fatalf("older revision no longer verifiable: %v", problems)
		}
		if revision, err := ManifestRevision(first.Manifest); err != nil || revision != first.RevisionHash {
			t.Fatalf("frozen revision %s: %v", revision, err)
		}
	})
	t.Run("test_an_unregistered_relationship_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		payload.RelationshipID = "rel-ffffffffffffffff"
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonUnregisteredRelationship)
	})
	t.Run("test_a_stale_generation_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		stale := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		if _, err := f.store.OpenGeneration(ctx, f.relationship.ID, "d2", ReasonNeedsChanges, nullString("t2"), fixtureNow); err != nil {
			t.Fatal(err)
		}
		_, err := f.acceptPayload(stale)
		requireReason(t, err, ReasonStaleGeneration)
	})
	t.Run("test_an_unknown_generation_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		relationship := f.relationship
		relationship.Generation = 7
		payload := f.readyPayload(relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonUnknownGeneration)
	})
	t.Run("test_an_unbound_generation_is_refused_and_stays_reportable", func(t *testing.T) {
		f := newIntakeFixture(t)
		pending := f.register(registerOptions{issue: "REL-PENDING"})
		payload := f.readyPayload(pending, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonUnboundGeneration)
		generation, err := f.store.RegistryGeneration(ctx, pending.ID, 1)
		if err != nil || generation.AnchorState != AnchorPending {
			t.Fatalf("generation %+v: %v", generation, err)
		}
	})
	t.Run("test_an_inactive_relationship_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		if err := f.store.SetStatus(ctx, f.relationship.ID, "paused", fixtureNow); err != nil {
			t.Fatal(err)
		}
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonRelationshipNotActive)
	})
	t.Run("test_a_manifest_path_outside_the_artifact_roots_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		outside := filepath.Join(f.tmp, "outside.txt")
		if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
			t.Fatal(err)
		}
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		size := int64(9)
		payload.Manifest = append(payload.Manifest, ManifestEntry{Path: outside, SHA256: strings.Repeat("a", 64), Bytes: &size})
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonScopeEscape)
	})
}

func TestReceiptIntake_python_path_binding_and_artifacts(t *testing.T) {
	t.Run("test_a_store_requiring_an_enforced_binding_refuses_a_best_effort_receipt", func(t *testing.T) {
		f := newIntakeFixture(t)
		strict := f.intake
		strict.Minimum = LeaseEnforced
		path := f.artifact("out.txt", "payload")
		payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
		// A pre-existing writable open makes a read lease unobtainable, so the read can only
		// reach the best-effort tier and a store demanding the enforced tier must refuse it.
		writable, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer writable.Close()
		_, err = strict.AcceptChildReceipt(context.Background(), payload.bytes(t), payload.TurnRef)
		requireReason(t, err, ReasonInsufficientPathBinding)
	})
	t.Run("lease_enforced_minimum_accepts_a_leased_read", func(t *testing.T) {
		// The success half of the enforced tier: with no writer the lease is taken and held.
		f := newIntakeFixture(t)
		strict := f.intake
		strict.Minimum = LeaseEnforced
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		stored, err := strict.AcceptChildReceipt(context.Background(), payload.bytes(t), payload.TurnRef)
		if err != nil || stored.PathBinding.String != string(LeaseEnforced) {
			t.Fatalf("leased read %+v: %v", stored, err)
		}
		event, err := f.store.Event(context.Background(), payload.EventID)
		if err != nil || event.PathBinding.String != string(LeaseEnforced) {
			t.Fatalf("persisted binding %+v: %v", event, err)
		}
	})
	t.Run("test_the_default_accepts_a_best_effort_binding_and_records_it", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		stored, err := f.acceptPayload(payload)
		if err != nil || stored.PathBinding.String != string(BestEffortDetection) {
			t.Fatalf("stored %+v: %v", stored, err)
		}
	})
	t.Run("test_artifacts_are_never_opened_for_writing", func(t *testing.T) {
		f := newIntakeFixture(t)
		path := f.artifact("out.txt", "original bytes")
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
		if _, err := f.acceptPayload(payload); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		b, a := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || b.Ino != a.Ino {
			t.Fatalf("artifact changed: %+v %+v", before, after)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "original bytes" {
			t.Fatalf("artifact bytes %q: %v", data, err)
		}
	})
	t.Run("test_the_contract_record_carries_no_internal_annotations", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		stored, err := f.acceptPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		record, err := decodeOrdered([]byte(stored.Record))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range record.object {
			if strings.HasPrefix(field.key, "_") {
				t.Fatalf("internal field %q", field.key)
			}
		}
	})
}

func nullString(text string) sql.NullString { return sql.NullString{String: text, Valid: true} }
