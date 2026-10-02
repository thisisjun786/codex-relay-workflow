package dagsched

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// openCorrection is what the relay's needs_changes ruling leaves: the next generation of the SAME relationship (reason needs_changes_revision), the ruling's next_generation and the findings it was
// given (the restoration block is the one finding the correction message carries).
func (k *releaseKit) openCorrection(relationship string, findings []map[string]any) {
	k.t.Helper()
	raw, err := json.Marshal(findings)
	if err != nil {
		k.t.Fatal(err)
	}
	now := k.clock()
	k.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, 2, ?, 'bound', 'turn-r2', 'needs_changes_revision', ?, ?)", relationship, "revision-"+relationship, now, now)
	k.exec("UPDATE relationships SET execution_generation = 2 WHERE relationship_id = ?", relationship)
	k.exec("UPDATE verdicts SET verdict = 'needs_changes', next_generation = 2")
	k.exec("UPDATE verdict_context SET findings = ?", string(raw))
}

func (k *releaseKit) correctionKit() (relationship string) {
	k.t.Helper()
	releasePlan(k.fixture, "rp")
	k.mustRelease("rp", "A")
	if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_node_executions").Scan(&relationship); err != nil {
		k.t.Fatal(err)
	}
	k.seedReport(relationship, "A", "rp")
	return relationship
}

func (k *releaseKit) correct(digest string) (CorrectionResult, error) {
	k.t.Helper()
	return k.sched.RecordCorrection(context.Background(), "rp", "A", "parent", digest)
}

func (k *releaseKit) prepare() Prepared {
	k.t.Helper()
	// a volatile snapshot captured for the correction makes the corrected manifest differ from the first (a manifest is content-addressed)
	snapshot := writeFile(k.t, k.root, "corrections.md", "the notes of the correction")
	p, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: snapshot, SHA256: shaOf([]byte("the notes of the correction")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil {
		k.t.Fatal(err)
	}
	return p
}

// Criterion c6: a correction returns to the same child. The prepared manifest is stored, its instruction names it in English, and the ruling's restoration block (the only text the correction
// message is proven to carry) is what binds the generation: the bound digest is derived from it, never from the caller's omission.
func TestCorrectionBindsTheManifestNamedInTheRestorationBlock(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	var previous string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	prepared := k.prepare()
	if prepared.ManifestDigest == "" || !strings.Contains(prepared.Instruction, prepared.ManifestDigest) || !strings.Contains(prepared.Instruction, "blocked_needs_input") || !strings.Contains(prepared.Instruction, "Correction generation 2 of CRW-A") {
		t.Fatalf("prepared = %+v", prepared)
	}
	if _, found, err := k.repo.ReadManifest(context.Background(), prepared.ManifestDigest); err != nil || !found {
		t.Fatalf("the prepared manifest is not stored: %v %v", found, err)
	}
	k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": "please fix the thing", "restoration": false}, {"id": "c2", "verdict": "needs_changes", "note": prepared.Instruction, "restoration": true}})
	// a digest the caller invents is refused; none is derived from the block
	if _, err := k.correct(dig("something else")); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a supplied digest that differs = %v", err)
	}
	res, err := k.correct("")
	if err != nil || res.Replayed || res.CarriedOver || res.ManifestDigest != prepared.ManifestDigest || res.Generation != 2 || res.RelationshipID != rid {
		t.Fatalf("correction = %v %+v", err, res)
	}
	var kind, bound string
	var generation int64
	var request *string
	if err := k.s.DB.QueryRow("SELECT kind, manifest_digest, execution_generation, managed_request_id FROM dag_node_executions WHERE execution_generation = 2").Scan(&kind, &bound, &generation, &request); err != nil ||
		kind != "correction" || bound != prepared.ManifestDigest || bound == previous || request != nil {
		t.Fatalf("row = %s %s %d %v %v", kind, bound, generation, request, err)
	}
	if k.count("SELECT COUNT(*) FROM relationships") != 1 {
		t.Fatal("a correction made another child")
	}
	// recording again is a replay; the supplied digest cross-checks
	if again, err := k.correct(prepared.ManifestDigest); err != nil || !again.Replayed || again.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("replay = %v %+v", err, again)
	}
	if n := k.read("rp").node("A"); n.State == "" {
		t.Fatalf("A = %+v", n)
	}
}

// What the child was NOT told does not bind: a digest in an ordinary finding is never rendered by the relay's correction message, so the previous manifest stays in force; a restoration block that
// names two manifests is refused.
func TestCorrectionDoesNotBindWhatTheChildWasNotTold(t *testing.T) {
	t.Run("the digest is in an ordinary finding", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		var previous string
		_ = k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions").Scan(&previous)
		prepared := k.prepare()
		k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": prepared.Instruction, "restoration": false}})
		res, err := k.correct("")
		if err != nil || !res.CarriedOver || res.ManifestDigest != previous {
			t.Fatalf("correction = %v %+v, want the previous manifest %s", err, res, previous)
		}
	})
	t.Run("no restoration block", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": "fix it"}})
		if res, err := k.correct(""); err != nil || !res.CarriedOver {
			t.Fatalf("correction = %v %+v", err, res)
		}
	})
	t.Run("two manifests in the block", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": "manifest " + dig("one") + " and manifest " + dig("two"), "restoration": true}})
		// the block holds the first match only; two blocks naming different manifests is the refusal
		k.exec("UPDATE verdict_context SET findings = ?", `[{"note":"manifest `+dig("one")+`","restoration":true},{"note":"manifest `+dig("two")+`","restoration":true}]`)
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v", err)
		}
	})
	t.Run("the manifest named is not stored for the node", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": "manifest " + dig("invented"), "restoration": true}})
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v", err)
		}
	})
}

// The preconditions of recording a correction.
func TestCorrectionPreconditions(t *testing.T) {
	t.Run("a generation that no ruling opened", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		k.exec("UPDATE generations SET reason = 'other' WHERE execution_generation = 2")
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v", err)
		}
	})
	t.Run("a ruling that did not open this generation", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		k.exec("UPDATE verdicts SET next_generation = 3")
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v", err)
		}
	})
	t.Run("a gap in the generations", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		// generation 2 reported and was ruled needs_changes, which opened generation 3, but generation 2 was never recorded as an execution: the executions of the node must follow one another
		now := k.clock()
		k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
			" VALUES ('evt-g2', ?, 2, ?, 'ready_for_review', 'child', 'child-A', 'turn-2', 'completed', '{}', 'final', ?, ?)", rid, dig("revision of generation 2"), now, now)
		k.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, next_generation, decided_at) VALUES ('evt-g2', '{}', 'needs_changes', 'verdict-turn-2', 3, ?)", now)
		k.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, findings, recorded_at) VALUES ('evt-g2', ?, '{}', 'current', 'evt-g2', ?, '{}', '[]', ?)",
			releaseCriteriaDigest(), dig("revision of generation 2"), now)
		k.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, 3, 'revision-3', 'bound', 'turn-r3', 'needs_changes_revision', 't', 't')", rid)
		k.exec("UPDATE relationships SET execution_generation = 3 WHERE relationship_id = ?", rid)
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_node_executions WHERE execution_generation = 3") != 0 {
			t.Fatal("a generation was bound over a gap")
		}
	})
	t.Run("no new generation: nothing to record", func(t *testing.T) {
		k := newReleaseKit(t)
		k.correctionKit()
		res, err := k.correct("")
		if err != nil || !res.Replayed {
			t.Fatalf("the current generation is the recorded one: %v %+v", err, res)
		}
	})
	t.Run("a paused relationship", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		k.exec("UPDATE relationships SET status = 'paused'")
		if _, err := k.correct(""); refusalReason(err) != "relationship_not_active" {
			t.Fatalf("correction = %v", err)
		}
	})
	t.Run("another task", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		if _, err := k.sched.RecordCorrection(context.Background(), "rp", "A", "intruder", ""); refusalReason(err) != "scope_role_mismatch" {
			t.Fatalf("correction = %v", err)
		}
	})
	t.Run("the previous acceptance reads stale until the correction is accepted", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		first, err := k.accept("rp", "A", AcceptInput{})
		if err != nil {
			t.Fatal(err)
		}
		k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": "manifest " + dig("n/a")}})
		if _, err := k.correct(""); err != nil {
			t.Fatal(err)
		}
		if st := k.status("rp", "ab"); st.Satisfied || st.Reason != BlockedStaleHead {
			t.Fatalf("the edge after a correction opened = %+v", st)
		}
		if st := k.status("rp", "ab"); st.AcceptanceID != "" && st.AcceptanceID == first.AcceptanceID {
			t.Fatalf("the stale acceptance still opens the edge: %+v", st)
		}
	})
}
