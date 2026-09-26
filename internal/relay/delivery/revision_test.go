package delivery

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_revision_roundtrip.py RVR-1..RVR-5.

func (v *vcu) requestCorrection(event string) Obj {
	_, err := v.ack.RecordVerdict(v.ctx, event, "needs_changes", "review-"+event, nil, []any{finding("c1", "needs_changes", "fix the output")}, nil, nil)
	mustDo(v.t, err)
	r, err := LoadRelationship(v.ctx, v.store, v.rid)
	mustDo(v.t, err)
	req := v.one("SELECT * FROM events WHERE relationship_id = ? AND execution_generation = ? AND outcome = 'revision_request'", v.rid, r.Generation)
	v.mustAttempt(req.S("event_id"), nil)
	bound, err := v.ack.BindDispatchedRevision(v.ctx, req.S("event_id"))
	mustDo(v.t, err)
	if str(bound, "anchorState") != "bound" || v.host.sends[len(v.host.sends)-1].thread != child {
		v.t.Fatalf("bound %v", bound)
	}
	v.host.finishTurn(child, str(bound, "dispatchTurnId"), "completed")
	return loadsObj(req.S("receipt"))
}

func (v *vcu) emitCorrection(predecessor any, text string) Obj {
	if text == "" {
		text = "corrected output"
	}
	r, err := LoadRelationship(v.ctx, v.store, v.rid)
	mustDo(v.t, err)
	turn := turnRef{child, r.generation(r.Generation).S("dispatch_turn_id"), "completed"}
	payload := v.readyPayload(v.rid, r.Generation, []string{v.artifact("out.txt", text)}, 1, turn)
	var supersedes *string
	if s, ok := predecessor.(string); ok {
		supersedes = &s
	}
	_, err = v.accept(payload, store.AcceptOptions{SupersedesRevision: supersedes})
	mustDo(v.t, err)
	return payload
}

func (v *vcu) acknowledgeCorrection(payload Obj) {
	e := str(payload, "eventId")
	_, err := v.delivery.Enqueue(v.ctx, e, "", "")
	mustDo(v.t, err)
	v.mustAttempt(e, nil)
	v.clock.Advance(5)
	turn := v.host.startTurn(parent, "", "inProgress", "")
	_, err = v.ack.Acknowledge(v.ctx, e, turn.TurnID, AckProof(e, turn.TurnID), true, nil, v.host)
	mustDo(v.t, err)
	if v.one("SELECT verified FROM acks WHERE event_id = ?", e).S("verified") != "verified" {
		v.t.Fatal("the correction's acknowledgement is verified")
	}
}

func (v *vcu) headOf(g int64) Obj {
	h, err := HeadRevision(v.ctx, v.store, v.rid, g)
	mustDo(v.t, err)
	return h
}

func runRVR(t *testing.T, mode string, goSide func(v *vcu, out map[string]any), args ...string) {
	tree := t.TempDir()
	python := runPython(t, tree, "rvr", append([]string{mode}, args...)...)
	v := newVCU(t, tree)
	out := map[string]any{}
	goSide(v, out)
	for k, want := range python.Out {
		requireSameJSON(t, mode+"."+k, out[k], want)
	}
	requireSameTables(t, v.fixture, python)
}

func TestRVR01_a_correction_by_the_same_child_supersedes_then_verifies_and_replays(t *testing.T) {
	verified := []any{finding("c1", "verified", "")}
	t.Run("roundtrip and replay", func(t *testing.T) {
		runRVR(t, "roundtrip", func(v *vcu, out map[string]any) {
			first := v.acknowledged("")
			req := v.requestCorrection(first)
			corr := v.emitCorrection(str(req, "supersedesRevisionHash"), "")
			v.acknowledgeCorrection(corr)
			out["final"] = v.verdict(str(corr, "eventId"), "verified", "review-corrected", verified, nil, nil)
			out["head"] = v.headOf(2)
			out["replay"] = v.verdict(first, "needs_changes", "replayed-review", nil, nil, nil)
			r, _ := LoadRelationship(v.ctx, v.store, v.rid)
			out["generation"] = r.Generation
			if str(out["head"].(Obj), "evidence") != Chain || v.count("SELECT COUNT(*) AS c FROM events WHERE outcome = 'revision_request'") != 1 {
				t.Fatal("declared chain, one revision request")
			}
		})
	})
	t.Run("repeated corrections chain to the immediate request", func(t *testing.T) {
		runRVR(t, "repeated", func(v *vcu, out map[string]any) {
			first := v.acknowledged("")
			req := v.requestCorrection(first)
			second := v.emitCorrection(str(req, "supersedesRevisionHash"), "second output")
			v.acknowledgeCorrection(second)
			req2 := v.requestCorrection(str(second, "eventId"))
			third := v.emitCorrection(str(req2, "supersedesRevisionHash"), "third output")
			v.acknowledgeCorrection(third)
			out["final"] = v.verdict(str(third, "eventId"), "verified", "review-third", verified, nil, nil)
		})
	})
	t.Run("a current-generation chain extends the correction", func(t *testing.T) {
		runRVR(t, "extend", func(v *vcu, out map[string]any) {
			req := v.requestCorrection(v.acknowledged(""))
			corr := v.emitCorrection(str(req, "supersedesRevisionHash"), "first fix")
			latest := v.emitCorrection(str(corr, "revisionHash"), "refined fix")
			v.acknowledgeCorrection(latest)
			out["final"] = v.verdict(str(latest, "eventId"), "verified", "final-review", verified, nil, nil)
		})
	})
}

func TestRVR02_only_the_requested_result_anchors_a_correction(t *testing.T) {
	for _, tc := range []struct {
		mode string
		run  func(v *vcu)
	}{
		{"historical", func(v *vcu) { first := v.acknowledged(""); v.advance(); v.emitCorrection(v.revisionHash(first), "") }},
		{"unknown", func(v *vcu) {
			v.requestCorrection(v.acknowledged(""))
			v.emitCorrection("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "")
		}},
		{"earlier", func(v *vcu) {
			first := v.acknowledged("")
			h := v.revisionHash(first)
			s := v.emitCorrection(h, "successor")
			v.acknowledgeCorrection(s)
			v.requestCorrection(str(s, "eventId"))
			v.emitCorrection(h, "")
		}},
		{"other_rel", func(v *vcu) {
			first := v.acknowledged("first relationship output")
			h := v.revisionHash(first)
			v.requestCorrection(first)
			v.rid = v.register(regOpts{issue: "REL-OTHER", dispatchRequest: "other-assignment", recipients: []string{parent, child}})
			initial := v.emitCorrection(nil, "other relationship output")
			v.acknowledgeCorrection(initial)
			v.requestCorrection(str(initial, "eventId"))
			v.emitCorrection(h, "")
		}},
		{"suppressed", func(v *vcu) {
			req := v.requestCorrection(v.acknowledged(""))
			_, err := execSQL(v.ctx, v.store, "UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", str(req, "eventId"))
			mustDo(v.t, err)
			v.emitCorrection(str(req, "supersedesRevisionHash"), "")
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			runRVR(t, tc.mode, func(v *vcu, out map[string]any) {
				tc.run(v)
				out["head"] = v.headOf(2)
				if str(out["head"].(Obj), "evidence") != UnknownPredecessor {
					t.Fatalf("head %v", out["head"])
				}
			})
		})
	}
}

func TestRVR03_two_corrections_of_the_requested_result_are_a_fork(t *testing.T) {
	runRVR(t, "fork", func(v *vcu, out map[string]any) {
		req := v.requestCorrection(v.acknowledged(""))
		v.emitCorrection(str(req, "supersedesRevisionHash"), "one")
		v.emitCorrection(str(req, "supersedesRevisionHash"), "two")
		out["head"] = v.headOf(2)
		if id, _ := get(out["head"].(Obj), "eventId"); id != nil || str(out["head"].(Obj), "evidence") != Fork {
			t.Fatal("fork with no head")
		}
	})
}

func TestRVR04_an_old_unruled_event_is_not_made_current_by_a_correction(t *testing.T) {
	runRVR(t, "old_unruled", func(v *vcu, out map[string]any) {
		first := v.acknowledged("")
		s := v.emitCorrection(v.revisionHash(first), "successor")
		v.acknowledgeCorrection(s)
		req := v.requestCorrection(str(s, "eventId"))
		v.emitCorrection(str(req, "supersedesRevisionHash"), "")
		out["r"] = v.verdict(first, "verified", "late-review", nil, nil, nil)
		if out["r"].(map[string]any)["reason"] != StaleGeneration {
			t.Fatal("stale_generation")
		}
	})
}

func TestRVR05_the_correction_has_its_own_anchor_in_the_new_generation(t *testing.T) {
	for _, reemit := range []bool{false, true} {
		name := map[bool]string{false: "queued correction, no completion yet", true: "after the child re-emits"}[reemit]
		t.Run(name, func(t *testing.T) {
			args := []string{}
			if reemit {
				args = []string{"reemit"}
			}
			runRVR(t, "projection", func(v *vcu, out map[string]any) {
				first := v.acknowledged("")
				req := v.requestCorrection(first)
				out["request"] = req
				if reemit {
					out["corrected"] = v.emitCorrection(str(req, "supersedesRevisionHash"), "")
				}
				p, err := Projection(v.ctx, v.delivery, v.rid)
				mustDo(t, err)
				out["projection"] = p
			}, args...)
		})
	}
}
