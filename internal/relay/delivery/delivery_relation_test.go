package delivery

import (
	"context"
	"os"
	"testing"
)

// test_delivery_relation.py DRL-1..DRL-4. DRL-0 (a drift guard between two Python spellings of
// the scope constants) is python-internal: Go has one constant.

type stubReader struct {
	answer Obj
	asked  []any
}

func (r *stubReader) Up(_ context.Context, rid string) (Obj, error) {
	r.asked = append(r.asked, Obj{{Key: "relationship_id", Value: rid}})
	return r.answer, nil
}

func relationCases(t *testing.T) Obj {
	raw, err := os.ReadFile("testdata/drl_answers.json")
	mustDo(t, err)
	v, err := loads(string(raw))
	mustDo(t, err)
	return v.(Obj)
}

func relationFixture() Relationship {
	return Relationship{ID: "rel-1", Parent: Endpoint{TaskID: "parent-task"}, Child: Endpoint{TaskID: "child-task"}}
}

func resolveCase(t *testing.T, f *fixture, cases Obj, name string) map[string]any {
	c, _ := get(cases, name)
	answer, _ := get(c.(Obj), "answer")
	reader := &stubReader{answer: answer.(Obj)}
	service := NewService(f.store, f.clock)
	service.Linkage = reader
	who, how, err := service.ResolveRecipient(f.ctx, relationFixture(), str(c.(Obj), "kind"))
	var got map[string]any
	if err != nil {
		got = refusalOf(err)
	} else {
		got = map[string]any{"ok": []any{who, how}}
	}
	got["asked"] = reader.asked
	return got
}

func TestDRL01_an_unwired_service_uses_the_relationship_row(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "drl")
	f := newFixture(t, tree)
	for _, kind := range []string{Completion, Revision} {
		who, how, err := f.delivery.ResolveRecipient(f.ctx, relationFixture(), kind)
		mustDo(t, err)
		name := map[string]string{Completion: "unwired_completion", Revision: "unwired_revision"}[kind]
		requireSameJSON(t, name, []any{who, how}, python.Out[name])
	}
}

func TestDRL02_an_agreeing_owner_resolves_verified_at_the_right_level(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "drl")
	f := newFixture(t, tree)
	cases := relationCases(t)
	for _, name := range []string{"agree_completion", "agree_revision"} {
		got := resolveCase(t, f, cases, name)
		requireSameJSON(t, name, got, python.Out[name])
		if got["ok"] == nil {
			t.Fatalf("%s refused: %v", name, got)
		}
	}
}

func TestDRL03_linkage_refusals_keep_distinct_reasons_and_never_fall_back(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "drl")
	f := newFixture(t, tree)
	cases := relationCases(t)
	for name, reason := range map[string]string{
		"owner_changed": RelationOwnerDrift, "drift_resolved": RelationOwnerDrift, "live_beside_audit": RelationOwnerDrift,
		"unreadable": RelationUnreadable, "nothing_found": UnregisteredScope, "unregistered": UnregisteredScope,
		"two_owners": DuplicateScopeOwner, "instruction_conflict": LinkConflict, "undefined_direction": NotClaimable,
	} {
		got := resolveCase(t, f, cases, name)
		requireSameJSON(t, name, got, python.Out[name])
		if got["reason"] != reason {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

func TestDRL04_a_retained_audit_conflict_does_not_block_a_healthy_delivery(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "drl")
	f := newFixture(t, tree)
	got := resolveCase(t, f, relationCases(t), "audit_only")
	requireSameJSON(t, "audit_only", got, python.Out["audit_only"])
	if got["ok"] == nil {
		t.Fatal("refused on an audit row")
	}
}
