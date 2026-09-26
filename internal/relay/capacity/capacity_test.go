package capacity

import (
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// test_capacity.py properties CAP-1..CAP-9 (.omo/ulw-execute/todo27-properties.md). Every step
// is compared whole, as json.dumps(indent=2) text or as the refusal, with the Python answer to
// the same calls (testdata/python_capacity.json, from testdata/gen_capacity.py).

const slots = "SELECT tenure, state, release_reason FROM execution_slots WHERE subject_key = ? ORDER BY tenure"

func Test27_CAP1_a_slot_neither_leaks_nor_returns_twice(t *testing.T) {
	e := newEnv(t)
	e.take("REL-1")
	again := e.take("REL-1")
	first := e.report()
	e.giveBack("REL-1", "completed", alpha, 0)
	back := e.giveBack("REL-1", "completed", alpha, 0)
	second := e.report()
	e.take("REL-2")
	for range 3 {
		e.giveBack("REL-2", "completed", alpha, 0)
	}
	released := e.rows("SELECT * FROM execution_slots WHERE subject_key = ? AND state = 'released'", "REL-2")
	e.rows("SELECT kind, subject, detail FROM journal WHERE kind LIKE 'slot_%' ORDER BY seq")
	e.sameAsPython("cap1")
	if field(again, "alreadyHeld") != true || field(first, "total") != 1 {
		t.Fatalf("replayed reservation: %v / total %v", field(again, "alreadyHeld"), field(first, "total"))
	}
	if field(back, "alreadyReleased") != true || field(second, "total") != 0 {
		t.Fatalf("replayed release: %v / total %v", field(back, "alreadyReleased"), field(second, "total"))
	}
	if n := len(released.([]any)); n != 1 {
		t.Fatalf("%d released rows, want 1", n)
	}
}

func Test27_CAP2_release_identity_is_the_subject_its_reason_and_its_tenure(t *testing.T) {
	e := newEnv(t)
	e.take("REL-1")
	e.giveBack("REL-1", "completed", alpha, 0)
	e.giveBack("REL-1", "failed", alpha, 0)
	kept, _ := e.cap.Slot(ctx(), assignment, "REL-1")
	e.step(kept, nil)
	e.conflicts("REL-1")
	e.giveBack("REL-NEVER", "completed", alpha, 0)
	e.take("REL-2")
	e.take("REL-2", beta, projectB)
	e.conflicts("REL-2")
	e.take("REL-1")
	e.giveBack("REL-1", "completed", alpha, 0)
	e.report()
	replay := e.giveBack("REL-1", "completed", alpha, 1)
	still := e.report()
	e.giveBack("REL-1", "completed", alpha, 2)
	after := e.report()
	e.giveBack("REL-1", "completed", alpha, 7)
	e.sameAsPython("cap2")
	for i, want := range map[int]string{2: "disposition_conflict", 5: "slot_unknown", 7: "disposition_conflict", 10: "disposition_conflict", 16: "slot_unknown"} {
		if got := e.reason(i); got != want {
			t.Errorf("step %d: %s, want %s", i, got, want)
		}
	}
	// REL-2 stays held throughout, so tenure 2 of REL-1 is the second held slot until it is named.
	if field(kept, "releaseReason") != "completed" || field(replay, "alreadyReleased") != true || field(still, "total") != 2 || field(after, "total") != 1 {
		t.Fatalf("kept %v replay %v total %v then %v", field(kept, "releaseReason"), field(replay, "alreadyReleased"), field(still, "total"), field(after, "total"))
	}
}

func Test27_CAP3_a_resume_opens_a_second_tenure_and_keeps_the_first(t *testing.T) {
	e := newEnv(t)
	e.take("REL-1")
	e.giveBack("REL-1", "completed", alpha, 0)
	e.take("REL-1")
	rows := e.rows(slots, "REL-1").([]any)
	report := e.report()
	e.sameAsPython("cap3")
	if len(rows) != 2 || field(rows[0], "state") != "released" || field(rows[1], "state") != "held" || field(report, "total") != 1 {
		t.Fatalf("tenures %v total %v", rows, field(report, "total"))
	}
}

func Test27_CAP4_authority_over_slots_ceilings_and_usage(t *testing.T) {
	e := newEnv(t)
	e.take("REL-1")
	e.giveBack("REL-1", "completed", beta, 0)
	e.report()
	e.take("REL-2", beta, projectA)
	e.conflicts("REL-2")
	e.take("REL-3", alpha, "PRJ-NONE")
	e.conflicts("REL-3")
	freed := e.giveBack("REL-1", "parent stopped answering", supervisor, 0)
	e.ceiling("runs", 99, "project", projectA, "runs", true, "task-stranger")
	e.ceiling("runs", 99, "initiative", "INIT-1", "runs", true, "task-stranger")
	e.ceiling("runs", 99, "store", "store", "runs", true, "task-stranger")
	e.observe("file_descriptors", 1, "project", projectA, "task-stranger", "claimed")
	e.ceiling("runs", 9, "initiative", "INIT-1", "runs", true, "")
	e.headroom("initiative", "INIT-1")
	e.sameAsPython("cap4")
	for i, want := range map[int]string{1: "scope_role_mismatch", 3: "scope_role_mismatch", 5: "unregistered_scope",
		8: "scope_role_mismatch", 9: "scope_role_mismatch", 10: "scope_role_mismatch", 11: "scope_role_mismatch"} {
		if got := e.reason(i); got != want {
			t.Errorf("step %d: %s, want %s", i, got, want)
		}
	}
	if field(freed, "state") != "released" {
		t.Fatalf("supervisor release: %v", freed)
	}
}

func Test27_CAP5_input_validation_is_refused_link_not_active(t *testing.T) {
	e := newEnv(t)
	e.observe("runs", 99, "project", projectA, alpha, "claimed")
	e.observe("file_descriptors", 1, "galaxy", projectA, alpha, "probe")
	for _, bad := range []float64{nan(), inf(), -1} {
		e.ceiling("runs", bad, "project", projectA, "runs", true, "")
	}
	e.observe("file_descriptors", nan(), "project", projectA, alpha, "probe")
	e.ceiling("runs", 1, "store", "global", "runs", true, supervisor)
	e.ceiling("runs", 1, "galaxy", "x", "runs", true, supervisor)
	e.observe("a|b", 1, "project", projectA, alpha, "probe")
	e.observe("file_descriptors", 1, "project", projectA, alpha, " ")
	e.ceiling("a|b", 1, "project", projectA, "runs", true, "")
	e.rows("SELECT COUNT(*) AS n FROM execution_limits")
	e.sameAsPython("cap5")
	for i := range 8 {
		if got := e.reason(i); got != "link_not_active" {
			t.Errorf("step %d: %s, want link_not_active", i, got)
		}
	}
}

func Test27_CAP6_ceilings_and_counts_are_separate_facts(t *testing.T) {
	t.Run("per project and store total", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("runs", 1, "project", projectA, "runs", true, "")
		e.ceiling("runs", 3, "store", "store", "runs", true, "")
		e.take("REL-1")
		e.take("REL-2")
		e.take("REL-3", beta, projectB)
		report := e.report()
		e.conflicts("REL-2")
		e.headroom("store", "store")
		e.sameAsPython("cap6")
		per := field(report, "perParent").(contract.OrderedObject)
		if e.reason(3) != "capacity_exhausted" || field(report, "total") != 2 || len(per) != 2 {
			t.Fatalf("report %v", report)
		}
	})
	t.Run("canonical store ceiling applies", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("runs", 1, "store", "store", "runs", true, "")
		e.take("REL-1")
		e.take("REL-2", beta, projectB)
		e.sameAsPython("cap6_store")
		if e.reason(2) != "capacity_exhausted" {
			t.Fatal(e.steps[2])
		}
	})
	t.Run("lowering revokes nothing", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("runs", 5, "project", projectA, "runs", true, "")
		e.take("REL-1")
		e.take("REL-2")
		e.take("REL-3")
		lowered := e.ceiling("runs", 1, "project", projectA, "runs", true, "")
		report := e.report()
		e.take("REL-4")
		e.rows("SELECT kind, subject, detail FROM journal WHERE kind = 'execution_limit_declared' ORDER BY seq")
		e.sameAsPython("cap6_lowered")
		if field(lowered, "overBy") != 2.0 || field(lowered, "state") != "over_ceiling" || field(lowered, "revision") != int64(2) ||
			field(report, "total") != 3 || e.reason(6) != "capacity_exhausted" {
			t.Fatalf("lowered %v", lowered)
		}
	})
	t.Run("unenforced never refuses", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("model_cost", 1, "project", projectA, "usd", false, "")
		e.take("REL-1")
		room := e.headroom("project", projectA)
		e.sameAsPython("cap6_unenforced")
		if field(field(room, "dimensions").([]any)[0], "enforce") != false {
			t.Fatal(room)
		}
	})
}

func Test27_CAP7_how_a_number_was_reached(t *testing.T) {
	t.Run("run count is no evidence about descriptors", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("file_descriptors", 100, "project", projectA, "fds", false, "")
		empty := e.headroom("project", projectA)
		e.take("REL-1")
		e.take("REL-2")
		e.take("REL-3")
		e.headroom("project", projectA)
		e.sameAsPython("cap7")
		entry := field(empty, "dimensions").([]any)[0]
		if field(entry, "used") != nil || field(entry, "proof") != "unmeasured" || e.steps[1]["ok"] != e.steps[5]["ok"] {
			t.Fatalf("empty %v loaded %v", e.steps[1], e.steps[5])
		}
	})
	t.Run("enforced unmeasured refuses", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("model_cost", 50, "project", projectA, "usd", true, "")
		e.take("REL-1")
		e.conflicts("REL-1")
		e.sameAsPython("cap7_unmeasured")
		if e.reason(1) != "capacity_unmeasured" {
			t.Fatal(e.steps[1])
		}
	})
	t.Run("only an observation gives a value", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("file_descriptors", 100, "project", projectA, "fds", true, "")
		e.observe("file_descriptors", 99, "project", projectA, alpha, "counted /proc/<pid>/fd")
		room := e.headroom("project", projectA)
		e.observe("file_descriptors", 120, "project", projectA, alpha, "counted /proc/<pid>/fd")
		e.headroom("project", projectA)
		e.take("REL-1")
		e.sameAsPython("cap7_observed")
		entry := field(room, "dimensions").([]any)[0]
		if field(entry, "used") != 99.0 || field(entry, "proof") != "observed" || field(entry, "state") != "within" || e.reason(5) != "capacity_exhausted" {
			t.Fatal(entry)
		}
	})
	t.Run("runs says derived from slots", func(t *testing.T) {
		e := newEnv(t)
		e.ceiling("runs", 5, "project", projectA, "runs", true, "")
		e.take("REL-1")
		room := e.headroom("project", projectA)
		e.sameAsPython("cap7_runs")
		entry := field(room, "dimensions").([]any)[0]
		if field(entry, "used") != int64(1) || field(entry, "proof") != "derived_from_slots" || field(entry, "note") != nil {
			t.Fatal(entry)
		}
	})
}

func Test27_CAP8_no_answer_changes_because_time_passed(t *testing.T) {
	e := newEnv(t)
	e.ceiling("runs", 5, "project", projectA, "runs", true, "")
	e.ceiling("file_descriptors", 100, "project", projectA, "fds", false, "")
	e.take("REL-1")
	e.report()
	e.headroom("project", projectA)
	e.clock.now = e.clock.now.Add(1_000_000 * time.Second)
	e.report()
	e.headroom("project", projectA)
	e.sameAsPython("cap8")
	if e.steps[3]["ok"] != e.steps[5]["ok"] || e.steps[4]["ok"] != e.steps[6]["ok"] {
		t.Fatal("an answer changed because time passed")
	}
}

// CAP-9 races two subjects from two Stores on one database file, as the Python test's threads
// do; a start barrier lines them up, and the assertions hold under every interleaving.
func Test27_CAP9_a_ceiling_of_one_admits_exactly_one_of_two_racing_subjects(t *testing.T) {
	e := newEnv(t)
	e.ceiling("runs", 1, "store", "store", "runs", true, "")
	var start, done sync.WaitGroup
	start.Add(1)
	type result struct {
		answer contract.OrderedObject
		err    error
	}
	results := make([]result, 2)
	for i, who := range [][2]string{{alpha, projectA}, {beta, projectB}} {
		other := openAgain(t, e)
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			answer, err := other.Reserve(ctx(), Reservation{SubjectKind: assignment, SubjectKey: "REL-" + string(rune('1'+i)),
				ParentTask: who[0], Project: who[1], ReservedBy: who[0]})
			results[i] = result{answer, err}
		}()
	}
	start.Done()
	done.Wait()
	admitted, refused := 0, 0
	for _, r := range results {
		switch {
		case r.err == nil:
			admitted++
		case reasonOf(r.err) == "capacity_exhausted":
			refused++
		default:
			t.Fatalf("unexpected: %v", r.err)
		}
	}
	report, err := e.cap.Report(ctx(), ReportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if admitted != 1 || refused != 1 || field(report, "total") != 1 {
		t.Fatalf("admitted %d refused %d total %v", admitted, refused, field(report, "total"))
	}
}
