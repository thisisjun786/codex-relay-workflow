package registry

import (
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_registry.py properties (REG-1..REG-11). Every refusal and record is compared whole with
// the Python registry's answer for the same steps (testdata/python_registry.json).

func identityScenario(t *testing.T) []map[string]any {
	r := newRegistry(t)
	first, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	var steps []map[string]any
	steps = append(steps, outcome(t, first.FullRecord(), nil), outcome(t, first.ContractRecord(), nil))
	again, err := r.Register(ctx(), fixture())
	steps = append(steps, outcome(t, again.ContractRecord(), err))
	other := fixture()
	other.ArtifactRoots = []string{"/somewhere/else"}
	_, err = r.Register(ctx(), other)
	steps = append(steps, outcome(t, nil, err))
	_, err = r.Get(ctx(), "rel-0000000000000000")
	return append(steps, outcome(t, nil, err))
}

func Test25_REG1_routing_key_is_the_pair_of_actual_task_ids_and_carries_no_title(t *testing.T) {
	steps := identityScenario(t)
	sameAsPython(t, "identity", steps)
	record := steps[1]["ok"].(map[string]any)
	if record["relationshipId"] != "rel-46d5b5ac690ef861" {
		t.Fatalf("relationship id %v", record["relationshipId"])
	}
	flat := strings.ToLower(steps[1]["ok"].(map[string]any)["relationshipId"].(string))
	for _, forbidden := range []string{"title", "latest", "name"} {
		for key := range record {
			if strings.Contains(strings.ToLower(key), forbidden) || strings.Contains(flat, forbidden) {
				t.Fatalf("contract record carries %q", forbidden)
			}
		}
	}
}

func Test25_REG2_reregistration_is_idempotent_and_a_different_scope_conflicts(t *testing.T) {
	steps := identityScenario(t)
	sameAsPython(t, "identity", steps)
	again := steps[2]["ok"].(map[string]any)
	if again["executionGeneration"] != float64(1) || len(again["generations"].([]any)) != 1 {
		t.Fatalf("re-registration opened a generation: %v", again)
	}
	if steps[3]["refused"].(map[string]any)["reason"] != string(contract.RefusalRelationshipConflict) {
		t.Fatalf("different scope: %v", steps[3])
	}
}

func Test25_REG3_cxc_bindings_are_kept_and_never_in_the_contract_record(t *testing.T) {
	steps := identityScenario(t)
	sameAsPython(t, "identity", steps)
	bindings := steps[0]["ok"].(map[string]any)["_bindings"].(map[string]any)
	if bindings["parentCxcSession"] != "cxc-parent" || bindings["childCxcSession"] != "cxc-child" {
		t.Fatalf("bindings %v", bindings)
	}
	if _, found := steps[1]["ok"].(map[string]any)["_bindings"]; found {
		t.Fatal("_bindings reached the contract record")
	}
}

func Test25_REG4_an_unregistered_relationship_is_refused(t *testing.T) {
	steps := identityScenario(t)
	sameAsPython(t, "identity", steps)
	if steps[4]["refused"].(map[string]any)["reason"] != string(contract.RefusalUnregisteredRelationship) {
		t.Fatalf("%v", steps[4])
	}
}

func Test25_REG5_a_replayed_dispatch_opens_no_generation_and_every_generation_is_retained(t *testing.T) {
	r := newRegistry(t)
	x, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	rid := x.ID
	var steps []map[string]any
	g, err := r.OpenGeneration(ctx(), rid, "dispatch-2", "needs_changes_revision", sql.NullString{})
	steps = append(steps, outcome(t, g.Record(), err))
	g, err = r.OpenGeneration(ctx(), rid, "dispatch-2", "needs_changes_revision", sql.NullString{})
	steps = append(steps, outcome(t, g.Record(), err))
	g, err = r.OpenGeneration(ctx(), rid, "d3", "needs_changes_revision", ns("t3"))
	steps = append(steps, outcome(t, g.Record(), err))
	_, err = r.OpenGeneration(ctx(), rid, "d4", "bogus", sql.NullString{})
	steps = append(steps, outcome(t, nil, err))
	_, err = r.OpenGeneration(ctx(), rid, "d4", "needs_changes_revision", ns(" "))
	steps = append(steps, outcome(t, nil, err))
	record, err := r.Get(ctx(), rid)
	steps = append(steps, outcome(t, record.ContractRecord(), err))
	sameAsPython(t, "generations", steps)
	if len(record.Generations) != 3 || record.Generations[0].DispatchTurnID.String != dispatchTurn {
		t.Fatalf("generations %v", record.Generations)
	}
}

func Test25_REG6_an_anchor_binds_only_from_a_dispatch_receipt_to_one_exact_turn(t *testing.T) {
	r := newRegistry(t)
	in := fixture()
	in.DispatchTurnID = sql.NullString{}
	x, err := r.Register(ctx(), in)
	if err != nil {
		t.Fatal(err)
	}
	rid := x.ID
	var steps []map[string]any
	g, ok, err := r.GenerationOf(ctx(), rid, 1)
	steps = append(steps, outcome(t, generationValue(g, ok), err))
	for _, c := range []struct{ turn, source string }{{"t", "latest_turn"}, {"", "dispatch_receipt"}, {"turn-exact", "dispatch_receipt"},
		{"turn-exact", "dispatch_receipt"}, {"some-other-turn", "dispatch_receipt"}} {
		g, err := r.BindAnchor(ctx(), rid, 1, c.turn, c.source)
		steps = append(steps, outcome(t, g.Record(), err))
	}
	_, err = r.BindAnchor(ctx(), rid, 7, "x", "dispatch_receipt")
	steps = append(steps, outcome(t, nil, err))
	sameAsPython(t, "anchors", steps)
}

func lifecycleSteps(t *testing.T) []map[string]any {
	r := newRegistry(t)
	x, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	rid := x.ID
	var steps []map[string]any
	record := func(x Relationship, err error) map[string]any { return outcome(t, x.ContractRecord(), err) }
	for _, status := range []string{"paused", "cancelled", "archived"} {
		steps = append(steps, record(r.SetStatus(ctx(), rid, status, "test")))
		_, err := r.RequireActive(ctx(), rid)
		steps = append(steps, outcome(t, nil, err))
		_, err = r.OpenGeneration(ctx(), rid, "after", "needs_changes_revision", ns("ta"))
		steps = append(steps, outcome(t, nil, err))
		steps = append(steps, record(r.Resume(ctx(), rid, 1, []string{root}, []string{parent}, "test")))
	}
	g, err := r.OpenGeneration(ctx(), rid, "after", "needs_changes_revision", ns("ta"))
	steps = append(steps, outcome(t, g.Record(), err))
	steps = append(steps, record(r.SetStatus(ctx(), rid, "paused", "t")))
	_, err = r.SetStatus(ctx(), rid, "active", "t")
	steps = append(steps, outcome(t, nil, err))
	steps = append(steps, record(r.SetStatus(ctx(), rid, "cancelled", "t")))
	_, err = r.SetStatus(ctx(), rid, "paused", "t")
	return append(steps, outcome(t, nil, err))
}

func Test25_REG7_inactive_is_never_active_opens_nothing_and_status_only_deactivates(t *testing.T) {
	steps := lifecycleSteps(t)
	sameAsPython(t, "lifecycle", steps)
	for i := 0; i < 3; i++ {
		for _, at := range []int{4*i + 1, 4*i + 2} {
			if steps[at]["refused"].(map[string]any)["reason"] != string(contract.RefusalRelationshipNotActive) {
				t.Fatalf("step %d: %v", at, steps[at])
			}
		}
	}
	if steps[12]["ok"].(map[string]any)["executionGeneration"] != float64(2) {
		t.Fatalf("generation after resume: %v", steps[12])
	}
	for _, at := range []int{14, 16} {
		if steps[at]["refused"].(map[string]any)["reason"] != string(contract.RefusalRelationshipNotActive) {
			t.Fatalf("reactivation step %d: %v", at, steps[at])
		}
	}
}

func Test25_REG8_resume_restates_the_generation_and_the_whole_scope(t *testing.T) {
	r := newRegistry(t)
	in := fixture()
	in.AllowedRecipients = []string{parent, child}
	x, err := r.Register(ctx(), in)
	if err != nil {
		t.Fatal(err)
	}
	rid := x.ID
	if _, err := r.SetStatus(ctx(), rid, "paused", "test"); err != nil {
		t.Fatal(err)
	}
	var steps []map[string]any
	_, err = r.Resume(ctx(), rid, 1, []string{root}, []string{parent}, "t")
	steps = append(steps, outcome(t, nil, err))
	_, err = r.Resume(ctx(), rid, 99, []string{"/elsewhere"}, []string{child}, "t")
	steps = append(steps, outcome(t, nil, err))
	_, err = r.Resume(ctx(), "rel-0000000000000000", 1, []string{root}, []string{parent}, "t")
	steps = append(steps, outcome(t, nil, err))
	resumed, err := r.Resume(ctx(), rid, 1, []string{root}, []string{parent, child}, "t")
	steps = append(steps, outcome(t, resumed.ContractRecord(), err))
	sameAsPython(t, "resume_restatement", steps)
	if resumed.Status != Active {
		t.Fatal(resumed.Status)
	}
}

func Test25_REG9_a_replacement_supersedes_and_preserves_the_original(t *testing.T) {
	r := newRegistry(t)
	original, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := r.Register(ctx(), Registration{Parent: Endpoint{TaskID: "01new-parent", HostID: host}, Child: Endpoint{TaskID: child, HostID: host},
		IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{"01new-parent"},
		DispatchRequestID: "dispatch-replacement", DispatchTurnID: ns("turn-replacement"), Supersedes: original.ID})
	if err != nil {
		t.Fatal(err)
	}
	var steps []map[string]any
	old, err := r.Get(ctx(), original.ID)
	steps = append(steps, outcome(t, old.FullRecord(), err), outcome(t, replacement.ContractRecord(), nil))
	_, err = r.Resume(ctx(), original.ID, 1, []string{root}, []string{parent}, "t")
	steps = append(steps, outcome(t, nil, err))
	sameAsPython(t, "supersession", steps)

	s := newRegistry(t)
	x, err := s.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Supersede(ctx(), x.ID, "rel-newnewnewnew00"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Resume(ctx(), x.ID, 1, []string{root}, []string{parent}, "t")
	sameAsPython(t, "superseded_resume", []map[string]any{outcome(t, nil, err)})
}

func replacementOf(childTask string) Registration {
	return Registration{Parent: Endpoint{TaskID: parent, HostID: host}, Child: Endpoint{TaskID: childTask, HostID: host},
		IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
		DispatchRequestID: "replacement-after-cancel", DispatchTurnID: ns("new-child-turn")}
}

func Test25_REG10_resume_after_reassignment_is_refused_and_leaves_one_owner(t *testing.T) {
	r := newRegistry(t)
	x, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	original := x.ID
	if _, err := r.SetStatus(ctx(), original, "cancelled", "authorized cancel"); err != nil {
		t.Fatal(err)
	}
	other, err := r.Register(ctx(), replacementOf("different-child"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []map[string]any
	_, err = r.Resume(ctx(), original, 1, []string{root}, []string{parent}, "old")
	steps = append(steps, outcome(t, nil, err))
	if _, err := r.SetStatus(ctx(), other.ID, "paused", "user pause"); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resume(ctx(), original, 1, []string{root}, []string{parent}, "old")
	steps = append(steps, outcome(t, nil, err))
	third := fixture()
	third.Child = Endpoint{TaskID: "third-child", HostID: host}
	third.DispatchRequestID = "d9"
	_, err = r.Register(ctx(), third)
	steps = append(steps, outcome(t, nil, err))
	if err := r.Supersede(ctx(), other.ID, "rel-0000000000000000"); err != nil {
		t.Fatal(err)
	}
	resumed, err := r.Resume(ctx(), original, 1, []string{root}, []string{parent}, "ok")
	steps = append(steps, outcome(t, resumed.ContractRecord(), err))
	sameAsPython(t, "resume_ownership", steps)

	// A dead identity registered again without naming a predecessor is named, not silent.
	s := newRegistry(t)
	y, err := s.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStatus(ctx(), y.ID, "cancelled", "t"); err != nil {
		t.Fatal(err)
	}
	var tenure []map[string]any
	_, err = s.Register(ctx(), fixture())
	tenure = append(tenure, outcome(t, nil, err))
	z, err := s.Register(ctx(), replacementOf("different-child"))
	tenure = append(tenure, outcome(t, z.ContractRecord(), err))
	sameAsPython(t, "returning_tenure", tenure)
}

// REG-11: two independent stores race a stale generation-1 resume against resume + advance +
// pause. Whatever the interleaving, generation 2 is never left active.
func Test25_REG11_a_stale_resume_never_leaves_the_newer_generation_active(t *testing.T) {
	for round := 0; round < 10; round++ {
		r := newRegistry(t)
		x, err := r.Register(ctx(), fixture())
		if err != nil {
			t.Fatal(err)
		}
		rid := x.ID
		if _, err := r.SetStatus(ctx(), rid, "paused", parent); err != nil {
			t.Fatal(err)
		}
		open := func() *Registry {
			other, err := store.Open(ctx(), r.Store.Path, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = other.Close() })
			return &Registry{Store: other, Now: r.Now, Policy: r.Policy}
		}
		stale, advance := open(), open()
		var staleErr, advanceErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, staleErr = stale.Resume(ctx(), rid, 1, []string{root}, []string{parent}, "stale generation-one resume")
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, advanceErr = advance.Resume(ctx(), rid, 1, []string{root}, []string{parent}, "other"); advanceErr != nil {
				return
			}
			if _, advanceErr = advance.OpenGeneration(ctx(), rid, "g2", "needs_changes_revision", ns("generation-two-turn")); advanceErr != nil {
				return
			}
			_, advanceErr = advance.SetStatus(ctx(), rid, "paused", "later user pause of generation two")
		}()
		close(start)
		wg.Wait()
		if advanceErr != nil {
			t.Fatalf("the legitimate sequence must succeed: %v", advanceErr)
		}
		final, err := r.Get(ctx(), rid)
		if err != nil {
			t.Fatal(err)
		}
		if final.Generation != 2 || final.Status != "paused" {
			t.Fatalf("final %d/%s (stale: %v)", final.Generation, final.Status, staleErr)
		}
		if staleErr != nil {
			mustReason(t, staleErr, contract.RefusalRelationshipNotActive)
		}
	}
}
