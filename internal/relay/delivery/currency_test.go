package delivery

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_verification_currency.py VCU-1..VCU-13 (VCU-5 is python-internal: an inspect.signature
// check; Go's RecordVerdict has no bypass, force or skip parameter by construction).

type vcu struct {
	*fixture
	ack      *Ack
	lastAck  Obj
	criteria *Criteria
}

func newVCU(t *testing.T, tree string) *vcu {
	f := newFixture(t, tree)
	a := NewAck(f.delivery)
	return &vcu{fixture: f, ack: a, criteria: a.Criteria}
}

var criteriaSet = []any{Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "the endpoint returns the agreed shape"}}, Obj{{Key: "id", Value: "c2"}, {Key: "title", Value: "a malformed request is refused"}}}

func (v *vcu) acknowledged(text string) string {
	if text == "" {
		text = "the deliverable"
	}
	rid := v.register(regOpts{recipients: []string{parent, child}})
	payload := v.readyPayload(rid, 1, []string{v.artifact("out.txt", text)}, 1, assigned("completed"))
	_, err := v.accept(payload, store.AcceptOptions{})
	mustDo(v.t, err)
	event := str(payload, "eventId")
	_, err = v.delivery.Enqueue(v.ctx, event, "", "")
	mustDo(v.t, err)
	v.mustAttempt(event, nil)
	v.clock.Advance(5)
	turn := v.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err = v.ack.Acknowledge(v.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, v.host)
	mustDo(v.t, err)
	return event
}

func (v *vcu) advance() {
	mustDo(v.t, v.store.Transaction(v.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := OpenGenerationIn(ctx, v.store, v.clock, v.rid, "newer-execution", "needs_changes_revision", "newer-turn")
		return err
	}))
}

func (v *vcu) accepted(text string, supersedes *string) Obj {
	r, err := LoadRelationship(v.ctx, v.store, v.rid)
	mustDo(v.t, err)
	payload := v.readyPayload(v.rid, r.Generation, []string{v.artifact("out.txt", text)}, 1, assigned("completed"))
	_, err = v.accept(payload, store.AcceptOptions{SupersedesRevision: supersedes})
	mustDo(v.t, err)
	return payload
}

func (v *vcu) revisionHash(event string) string {
	return v.one("SELECT revision_hash FROM events WHERE event_id = ?", event).S("revision_hash")
}

func outcome(record Obj, err error) map[string]any {
	if err != nil {
		return refusalOf(err)
	}
	return map[string]any{"ok": record}
}

func (v *vcu) verdict(event, verdict, turn string, findings []any, reason, expected any) map[string]any {
	return outcome(v.ack.RecordVerdict(v.ctx, event, verdict, turn, nil, findings, reason, expected))
}

func finding(id, verdict, note string) Obj {
	o := Obj{{Key: "id", Value: id}, {Key: "verdict", Value: verdict}}
	if note != "" {
		o = append(o, F{Key: "note", Value: note})
	}
	return o
}

func (v *vcu) head() Obj {
	h, err := HeadRevision(v.ctx, v.store, v.rid, 1)
	mustDo(v.t, err)
	return h
}

// run executes one Python scenario of vcu.py and its Go twin, then compares out["r"] and the store.
func runVCU(t *testing.T, mode string, goSide func(v *vcu) any, wantReason string) {
	tree := t.TempDir()
	python := runPython(t, tree, "vcu", mode)
	v := newVCU(t, tree)
	got := goSide(v)
	requireSameJSON(t, mode, got, python.Out["r"])
	if m, ok := got.(map[string]any); ok && wantReason != "" && m["reason"] != wantReason {
		t.Fatalf("%s: want %s, got %v", mode, wantReason, m)
	}
	requireSameTables(t, v.fixture, python)
}

func TestVCU01_a_verified_verdict_needs_the_current_head(t *testing.T) {
	t.Run("generation advanced", func(t *testing.T) {
		runVCU(t, "advanced", func(v *vcu) any {
			e := v.acknowledged("")
			v.advance()
			return v.verdict(e, "verified", "v1", nil, nil, nil)
		}, StaleGeneration)
	})
	t.Run("two undeclared revisions", func(t *testing.T) {
		runVCU(t, "ambiguous", func(v *vcu) any {
			e := v.acknowledged("")
			v.accepted("a different revision", nil)
			return v.verdict(e, "verified", "v1", nil, nil, nil)
		}, RevisionAmbiguous)
	})
	t.Run("declared successor", func(t *testing.T) {
		runVCU(t, "superseded", func(v *vcu) any {
			e := v.acknowledged("")
			h := v.revisionHash(e)
			v.accepted("the corrected revision", &h)
			return v.verdict(e, "verified", "v1", nil, nil, nil)
		}, SupersededRevision)
	})
	t.Run("advance after the caller's preflight read", func(t *testing.T) {
		runVCU(t, "advanced", func(v *vcu) any {
			e := v.acknowledged("")
			r, _ := LoadRelationship(v.ctx, v.store, v.rid)
			if r.Generation != 1 {
				t.Fatal("preflight reads generation 1")
			}
			v.advance()
			return v.verdict(e, "verified", "v1", nil, nil, nil)
		}, StaleGeneration)
	})
}

func TestVCU02_a_paused_relationship_refuses_every_verdict(t *testing.T) {
	runVCU(t, "paused", func(v *vcu) any {
		e := v.acknowledged("")
		v.setStatusBy("paused", parent)
		var out []any
		for _, verdict := range []string{"verified", "needs_changes", "unverified", "aborted"} {
			r := v.verdict(e, verdict, "v1", nil, "stopping", nil)
			if r["reason"] != RelationshipNotActive {
				t.Fatalf("%s: %v", verdict, r)
			}
			out = append(out, r)
		}
		return out
	}, "")
}

func TestVCU03_needs_changes_on_a_stale_event_is_refused_unverified_is_recorded(t *testing.T) {
	t.Run("needs_changes", func(t *testing.T) {
		runVCU(t, "nc_stale", func(v *vcu) any {
			e := v.acknowledged("")
			v.advance()
			return v.verdict(e, "needs_changes", "v1", []any{finding("c1", "needs_changes", "still wrong")}, nil, nil)
		}, StaleGeneration)
	})
	t.Run("unverified", func(t *testing.T) {
		runVCU(t, "unv_stale", func(v *vcu) any {
			e := v.acknowledged("")
			v.advance()
			r := v.verdict(e, "unverified", "v1", nil, "could not reach it", nil)
			if v.one("SELECT currency FROM verdict_context WHERE event_id = ?", e).S("currency") != StaleGeneration {
				t.Fatal("currency stale_generation")
			}
			return r
		}, "")
	})
}

func TestVCU04_a_replay_returns_the_historical_verdict(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "vcu", "replay")
	v := newVCU(t, tree)
	e := v.acknowledged("")
	first := v.verdict(e, "needs_changes", "v1", []any{finding("c1", "needs_changes", "fix it")}, nil, nil)
	again := v.verdict(e, "verified", "v2", nil, nil, nil)
	requireSameJSON(t, "first", first, python.Out["first"])
	requireSameJSON(t, "replay", again, python.Out["r"])
	rec := again["ok"].(Obj)
	if v, _ := get(rec, "_replay"); v != true || str(rec, "verdict") != "needs_changes" || str(rec, "verdictTurnId") != "v1" {
		t.Fatalf("replay %v", rec)
	}
	requireSameTables(t, v.fixture, python)
}

func TestVCU06_head_revision_lineage_evidence(t *testing.T) {
	for _, tc := range []struct {
		mode, evidence string
		build          func(v *vcu)
	}{
		{"sole", Sole, func(v *vcu) { v.acknowledged("") }},
		{"chain", Chain, func(v *vcu) { e := v.acknowledged(""); h := v.revisionHash(e); v.accepted("second revision", &h) }},
		{"fork_undeclared", Fork, func(v *vcu) { v.acknowledged(""); v.accepted("a different revision", nil) }},
		{"fork_shared", Fork, func(v *vcu) {
			e := v.acknowledged("")
			h := v.revisionHash(e)
			v.accepted("branch one", &h)
			v.accepted("branch two", &h)
		}},
		{"unknown_pred", UnknownPredecessor, func(v *vcu) {
			v.acknowledged("")
			h := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			v.accepted("claims to replace something we never saw", &h)
		}},
		{"cycle", Cycle, func(v *vcu) {
			e := v.acknowledged("")
			h := v.revisionHash(e)
			s := v.accepted("second revision", &h)
			_, err := execSQL(v.ctx, v.store, "UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?", str(s, "revisionHash"), e)
			mustDo(v.t, err)
		}},
		{"reversed", Fork, func(v *vcu) { v.acknowledged("a different revision"); v.accepted("the deliverable", nil) }},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			runVCU(t, tc.mode, func(v *vcu) any {
				tc.build(v)
				h := v.head()
				if str(h, "evidence") != tc.evidence {
					t.Fatalf("head %v", h)
				}
				return h
			}, "")
		})
	}
}

func TestVCU07_a_revision_cannot_declare_itself(t *testing.T) {
	runVCU(t, "self", func(v *vcu) any {
		rid := v.register(regOpts{})
		payload := v.readyPayload(rid, 1, []string{v.artifact("out.txt", "self referential")}, 1, assigned("completed"))
		h := str(payload, "revisionHash")
		_, err := v.accept(payload, store.AcceptOptions{SupersedesRevision: &h})
		return refusalOf(err)
	}, store.ReasonRevisionLineageInvalid)
}

func TestVCU08_criteria_coverage_on_verdicts(t *testing.T) {
	both := []any{finding("c1", "verified", ""), finding("c2", "verified", "")}
	claimed := func(v *vcu) string {
		e := v.acknowledged("")
		_, err := v.criteria.Register(v.ctx, v.rid, criteriaSet, "https://linear.app/doc/1")
		mustDo(v.t, err)
		_, err = v.ack.ClaimVerification(v.ctx, e, "ack-turn")
		mustDo(v.t, err)
		return e
	}
	for _, tc := range []struct {
		mode, reason string
		run          func(v *vcu) any
	}{
		{"managed_none", CriteriaUnregistered, func(v *vcu) any {
			e := v.acknowledged("")
			_, err := v.criteria.SetMode(v.ctx, v.rid, Managed)
			mustDo(v.t, err)
			return v.verdict(e, "verified", "v1", nil, nil, nil)
		}},
		{"legacy", "", func(v *vcu) any { return v.verdict(v.acknowledged(""), "verified", "v1", nil, nil, nil) }},
		{"missing", CriteriaNotCovered, func(v *vcu) any {
			return v.verdict(claimed(v), "verified", "v1", []any{finding("c1", "verified", "")}, nil, nil)
		}},
		{"covered", "", func(v *vcu) any { return v.verdict(claimed(v), "verified", "v1", both, nil, nil) }},
		{"no_note", FindingsRequired, func(v *vcu) any {
			return v.verdict(claimed(v), "needs_changes", "v1", []any{finding("c1", "needs_changes", "")}, nil, nil)
		}},
		{"unknown_id", UnknownCriterion, func(v *vcu) any {
			return v.verdict(claimed(v), "verified", "v1", []any{finding("nope", "verified", "")}, nil, nil)
		}},
		{"bad_disposition", DispositionConflict, func(v *vcu) any {
			return v.verdict(claimed(v), "verified", "v1", []any{finding("c1", "regressed", "")}, nil, nil)
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) { runVCU(t, tc.mode, tc.run, tc.reason) })
	}
}

func TestVCU09_criteria_currency_binds_the_review(t *testing.T) {
	both := []any{finding("c1", "verified", ""), finding("c2", "verified", "")}
	for _, tc := range []struct {
		mode, reason string
		run          func(v *vcu) any
	}{
		{"edited", CriteriaSetChanged, func(v *vcu) any {
			e := v.acknowledged("")
			_, err := v.criteria.Register(v.ctx, v.rid, criteriaSet, "https://linear.app/doc/1")
			mustDo(v.t, err)
			_, err = v.ack.ClaimVerification(v.ctx, e, "ack-turn")
			mustDo(v.t, err)
			_, err = v.criteria.Register(v.ctx, v.rid, []any{Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "the endpoint returns a COMPLETELY different shape"}}, Obj{{Key: "id", Value: "c2"}, {Key: "title", Value: "a malformed request is refused"}}}, "https://linear.app/doc/1")
			mustDo(v.t, err)
			return v.verdict(e, "verified", "v1", both, nil, nil)
		}},
		{"wrong_digest", CriteriaSetChanged, func(v *vcu) any {
			e := v.acknowledged("")
			_, err := v.criteria.Register(v.ctx, v.rid, criteriaSet, "https://linear.app/doc/1")
			mustDo(v.t, err)
			_, err = v.ack.ClaimVerification(v.ctx, e, "ack-turn")
			mustDo(v.t, err)
			return v.verdict(e, "verified", "v1", both, nil, "0000000000000000000000000000000000000000000000000000000000000000")
		}},
		{"no_claim", ReviewNotBound, func(v *vcu) any {
			e := v.acknowledged("")
			_, err := v.criteria.Register(v.ctx, v.rid, criteriaSet, "https://linear.app/doc/1")
			mustDo(v.t, err)
			return v.verdict(e, "verified", "v1", both, nil, nil)
		}},
		{"explicit", "", func(v *vcu) any {
			e := v.acknowledged("")
			reg, err := v.criteria.Register(v.ctx, v.rid, criteriaSet, "https://linear.app/doc/1")
			mustDo(v.t, err)
			return v.verdict(e, "verified", "v1", both, nil, str(reg, "setDigest"))
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) { runVCU(t, tc.mode, tc.run, tc.reason) })
	}
}

func TestVCU10_the_set_digest_resists_delimiter_injection(t *testing.T) {
	a := SetDigest([]Criterion{{"a", "b|c", true}})
	b := SetDigest([]Criterion{{"a|b", "c", true}})
	python := pythonValue(t, "from codex_session_relay.criteria import set_digest; print(set_digest([{'id': 'a', 'title': 'b|c', 'required': True}]), set_digest([{'id': 'a|b', 'title': 'c', 'required': True}]))")
	if a == b || python != a+" "+b {
		t.Fatalf("go %s %s python %s", a, b, python)
	}
}
