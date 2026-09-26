package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// test_wp1_regressions.py receipt and scope classes, through the intake.
func TestWP1_python_turn_identity_and_contradiction(t *testing.T) {
	ctx := context.Background()
	payloadFor := func(f *intakeFixture, thread, turn string) receiptPayload {
		return f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, TurnReference{ThreadID: thread, TurnID: turn, Status: "completed"})
	}
	t.Run("test_the_registered_child_on_its_assigned_turn_is_accepted", func(t *testing.T) {
		f := newIntakeFixture(t)
		stored, err := f.acceptPayload(payloadFor(f, fixtureChild, fixtureTurn))
		if err != nil || stored.Duplicate {
			t.Fatalf("stored %+v: %v", stored, err)
		}
	})
	t.Run("test_an_unregistered_child_is_refused_even_when_it_is_self_consistent", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, err := f.acceptPayload(payloadFor(f, "unregistered-task", fixtureTurn))
		requireReason(t, err, ReasonUnassignedTurn)
		if !strings.Contains(err.Error(), "registered child") {
			t.Fatalf("detail %v", err)
		}
	})
	t.Run("test_an_unassigned_turn_is_refused_even_for_the_right_child", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, err := f.acceptPayload(payloadFor(f, fixtureChild, "unassigned-turn"))
		requireReason(t, err, ReasonUnassignedTurn)
		if !strings.Contains(err.Error(), "explicit continuation admission") {
			t.Fatalf("detail %v", err)
		}
	})
	t.Run("test_a_daemon_observation_of_a_foreign_turn_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, err := f.intake.DaemonObservation(ctx, f.relationship.ID, TurnReference{ThreadID: fixtureChild, TurnID: "some-other-turn", Status: "failed"})
		requireReason(t, err, ReasonUnassignedTurn)
	})
	for _, status := range []string{"failed", "interrupted"} {
		name := map[string]string{"failed": "test_a_ready_claim_on_a_failed_turn_is_refused_at_intake", "interrupted": "test_a_ready_claim_on_an_interrupted_turn_is_refused_at_intake"}[status]
		t.Run(name, func(t *testing.T) {
			f := newIntakeFixture(t)
			payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn(status))
			_, err := f.acceptPayload(payload)
			requireReason(t, err, ReasonContradictoryObservation)
		})
	}
	t.Run("test_a_daemon_receipt_may_only_restate_what_it_observed", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "failed", assignedTurn("completed"))
		payload.Producer = ProducerDaemon
		payload.Attempt = nil
		f.rederive(&payload)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonContradictoryObservation)
	})
}

func TestWP1_python_non_regular_and_frozen_artifacts(t *testing.T) {
	t.Run("test_a_named_pipe_is_refused_promptly_rather_than_blocking", func(t *testing.T) {
		f := newIntakeFixture(t)
		fifo := filepath.Join(f.root, "artifact.fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// Bounded: a regression here hangs in open(2) rather than failing.
		done := make(chan error, 1)
		go func() { _, _, _, err := HashArtifact(fifo, []string{f.root}, false); done <- err }()
		select {
		case err := <-done:
			requireReason(t, err, ReasonNotARegularFile)
		case <-time.After(10 * time.Second):
			t.Fatal("opening a FIFO blocked")
		}
	})
	t.Run("test_a_directory_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, _, _, err := HashArtifact(f.root, []string{f.root}, false)
		requireReason(t, err, ReasonNotARegularFile)
	})
	t.Run("test_a_frozen_fallback_cannot_satisfy_an_enforced_minimum", func(t *testing.T) {
		f := newIntakeFixture(t)
		strict := f.intake
		strict.Minimum = LeaseEnforced
		path := f.artifact("out.txt", "first revision")
		payload := f.readyPayload(f.relationship, []string{path}, 1, assignedTurn("completed"))
		reference := filepath.Join(f.tmp, "frozen")
		if err := FreezeManifest(payload.Manifest, reference); err != nil {
			t.Fatal(err)
		}
		payload.ManifestRef = &reference
		f.artifact("out.txt", "a later revision")
		_, err := strict.AcceptChildReceipt(context.Background(), payload.bytes(t), payload.TurnRef)
		requireReason(t, err, ReasonInsufficientPathBinding)
		// The same frozen fallback meets the default minimum, so the refusal above is the tier.
		if _, err := f.acceptPayload(payload); err != nil {
			t.Fatalf("frozen fallback at best effort: %v", err)
		}
	})
	t.Run("test_a_non_digest_blob_name_is_refused_before_any_path_is_built", func(t *testing.T) {
		f := newIntakeFixture(t)
		size := int64(1)
		destination := filepath.Join(f.tmp, "evil")
		err := FreezeManifest([]ManifestEntry{{Path: "/etc/hostname", SHA256: "/etc/passwd", Bytes: &size}}, destination)
		requireReason(t, err, ReasonManifestUnverified)
		if entries, _ := os.ReadDir(filepath.Join(destination, "files")); len(entries) != 0 {
			t.Fatalf("a blob was written: %v", entries)
		}
	})
	t.Run("test_canonicalization_rejects_a_non_hex_digest", func(t *testing.T) {
		_, err := ManifestRevision([]ManifestEntry{{Path: "/a", SHA256: strings.Repeat("z", 64)}})
		requireReason(t, err, ReasonManifestUnverified)
	})
	t.Run("test_frozen_verification_checks_byte_counts", func(t *testing.T) {
		f := newIntakeFixture(t)
		entries, err := BuildManifest([]string{f.artifact("sized.txt", "exactly this")}, []string{f.root})
		if err != nil {
			t.Fatal(err)
		}
		reference := filepath.Join(f.tmp, "frozen-size")
		if err := FreezeManifest(entries, reference); err != nil {
			t.Fatal(err)
		}
		if problems := VerifyFrozen(reference, entries); len(problems) != 0 {
			t.Fatalf("honest sizes refused: %v", problems)
		}
		wrong := *entries[0].Bytes + 5
		if problems := VerifyFrozen(reference, []ManifestEntry{{Path: entries[0].Path, SHA256: entries[0].SHA256, Bytes: &wrong}}); len(problems) == 0 {
			t.Fatal("a wrong byte count verified")
		}
	})
}

func TestWP1_python_malformed_receipts(t *testing.T) {
	mangled := func(t *testing.T, mutate func(map[string]any)) (*intakeFixture, []byte, TurnReference) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, assignedTurn("completed"))
		return f, payload.withFields(t, mutate), payload.TurnRef
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"test_an_unknown_field_is_refused", func(m map[string]any) { m["surprise"] = 1 }},
		{"test_a_missing_required_field_is_refused", func(m map[string]any) { delete(m, "emittedAt") }},
		{"test_an_unknown_producer_is_refused", func(m map[string]any) { m["producer"] = "somebody" }},
		{"test_an_unknown_turn_status_is_refused", func(m map[string]any) { m["turnRef"].(map[string]any)["turnStatus"] = "vanished" }},
		{"test_a_manifest_entry_missing_its_digest_is_refused_not_crashed", func(m map[string]any) {
			m["manifest"] = []any{map[string]any{"path": m["manifest"].([]any)[0].(map[string]any)["path"]}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, payload, turn := mangled(t, tc.mutate)
			_, err := f.accept(payload, turn)
			requireReason(t, err, ReasonMalformedReceipt)
		})
	}
	t.Run("test_a_malformed_receipt_is_recorded_as_a_refusal", func(t *testing.T) {
		f, payload, turn := mangled(t, func(m map[string]any) { m["surprise"] = 1 })
		if _, err := f.accept(payload, turn); err == nil {
			t.Fatal("malformed receipt accepted")
		}
		refusals, err := f.store.Refusals(context.Background(), f.relationship.ID)
		if err != nil || len(refusals) != 1 || refusals[0].Reason != ReasonMalformedReceipt {
			t.Fatalf("refusals %+v: %v", refusals, err)
		}
	})
	t.Run("test_a_daemon_receipt_carrying_a_rerun_counter_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.executionPayload(f.relationship, "failed", assignedTurn("failed"))
		payload.Producer = ProducerDaemon
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonOutcomeInconsistent)
	})
	t.Run("test_a_non_object_receipt_is_refused", func(t *testing.T) {
		f := newIntakeFixture(t)
		_, err := f.accept([]byte(`"not a receipt"`), assignedTurn("completed"))
		requireReason(t, err, ReasonMalformedReceipt)
	})
}

func TestWP1_python_active_turn_self_emission(t *testing.T) {
	ctx := context.Background()
	staged := func(t *testing.T) (*intakeFixture, receiptPayload, StoredReceipt) {
		t.Helper()
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "work in progress")}, 1, assignedTurn("inProgress"))
		stored, err := f.acceptPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		return f, payload, stored
	}
	deliverable := func(t *testing.T, f *intakeFixture, event string) bool {
		t.Helper()
		ok, err := f.store.Deliverable(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	stage := func(t *testing.T, f *intakeFixture, event string) Event {
		t.Helper()
		row, err := f.store.Event(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	t.Run("test_a_claim_from_a_live_turn_is_accepted_but_staged", func(t *testing.T) {
		f, payload, stored := staged(t)
		if stored.Stage != StageStaged || deliverable(t, f, payload.EventID) || stage(t, f, payload.EventID).TurnStatus != "inProgress" {
			t.Fatalf("stored %+v", stored)
		}
	})
	t.Run("test_normal_completion_finalizes_the_staged_claim_exactly_once", func(t *testing.T) {
		f, payload, _ := staged(t)
		result, err := f.intake.ResolveStaged(ctx, assignedTurn("completed"))
		if err != nil || len(result.Finalized) != 1 || result.Finalized[0] != payload.EventID || len(result.Suppressed) != 0 {
			t.Fatalf("result %+v: %v", result, err)
		}
		if !deliverable(t, f, payload.EventID) || f.count(`SELECT COUNT(*) FROM events`) != 1 {
			t.Fatal("finalized claim not deliverable exactly once")
		}
		again, err := f.intake.ResolveStaged(ctx, assignedTurn("completed"))
		if err != nil || len(again.Finalized) != 0 {
			t.Fatalf("again %+v: %v", again, err)
		}
	})
	t.Run("test_a_failed_ending_suppresses_the_staged_claim", func(t *testing.T) {
		f, payload, _ := staged(t)
		result, err := f.intake.ResolveStaged(ctx, assignedTurn("failed"))
		if err != nil || len(result.Suppressed) != 1 || result.Suppressed[0] != payload.EventID {
			t.Fatalf("result %+v: %v", result, err)
		}
		var reason string
		if err := f.store.DB.QueryRowContext(ctx, `SELECT suppressed_reason FROM events WHERE event_id=?`, payload.EventID).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if deliverable(t, f, payload.EventID) || stage(t, f, payload.EventID).Stage != StageSuppressed || !strings.Contains(reason, "not promoted") {
			t.Fatalf("suppression %q", reason)
		}
	})
	t.Run("test_an_interrupted_ending_suppresses_the_staged_claim", func(t *testing.T) {
		f, payload, _ := staged(t)
		if _, err := f.intake.ResolveStaged(ctx, assignedTurn("interrupted")); err != nil {
			t.Fatal(err)
		}
		if deliverable(t, f, payload.EventID) || stage(t, f, payload.EventID).Stage != StageSuppressed {
			t.Fatal("interrupted claim not suppressed")
		}
	})
	t.Run("test_a_still_running_turn_leaves_the_claim_staged", func(t *testing.T) {
		f, payload, _ := staged(t)
		result, err := f.intake.ResolveStaged(ctx, assignedTurn("inProgress"))
		if err != nil || !result.Pending || deliverable(t, f, payload.EventID) || stage(t, f, payload.EventID).Stage != StageStaged {
			t.Fatalf("result %+v: %v", result, err)
		}
	})
	t.Run("test_a_receipt_from_a_completed_turn_is_final_immediately", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "done")}, 1, assignedTurn("completed"))
		stored, err := f.acceptPayload(payload)
		if err != nil || stored.Stage != StageFinal || !deliverable(t, f, payload.EventID) {
			t.Fatalf("stored %+v: %v", stored, err)
		}
	})
	t.Run("test_staging_does_not_relax_the_registry_identity_checks", func(t *testing.T) {
		f := newIntakeFixture(t)
		turn := TurnReference{ThreadID: fixtureChild, TurnID: "unassigned-turn", Status: "inProgress"}
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "payload")}, 1, turn)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonUnassignedTurn)
	})
}
