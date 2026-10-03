package dagsched

import (
	"path/filepath"
	"testing"
)

// CRW-411 through the built binary, as the parent uses it: JSON on stdout, exit 0 for an answer and 2 for a refusal with its reason.

// policyCLIState is a plan with a running holder, a candidate that overlaps it locally, and one node that landed after a conflict that took 30 minutes to settle; the store is closed so the binary is the
// only user.
func policyCLIState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	w := newPolicyWorldOn(t, newFixtureAt(t, filepath.Join(state, "relay.sqlite3")), "l1")
	w.land("l1", 5, 1800)
	if err := w.f.s.Close(); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestCLIReleasePolicyLandingResultAndMeasurements(t *testing.T) {
	state := policyCLIState(t)

	// the measurements before any policy, any pass or any result: absent where nothing was read
	before := summaryRun(t, state, 0, "dag-measurements", "--plan", "p")
	if before["schema"] != "dag-measurements/1" || before["landed_nodes"] != float64(1) {
		t.Fatalf("measurements = %v", before)
	}
	if got := objectOf(t, before, "parallelism"); got["absent"] != "no_recorded_pass" || got["held_slots"] != nil {
		t.Fatalf("parallelism = %v, want it absent", got)
	}
	if got := objectOf(t, before, "conflict_handling"); got["absent"] != nil || got["median_seconds"] != float64(1800) {
		t.Fatalf("handling = %v", got)
	}
	if got := objectOf(t, before, "post_merge"); got["absent"] != "none_recorded" {
		t.Fatalf("post merge = %v, want it absent", got)
	}

	// no policy: the candidate is released beside the local holder and the reading prints no release_policy
	ready := summaryRun(t, state, 0, "dag-ready", "--plan", "p")
	if _, printed := ready["release_policy"]; printed {
		t.Fatalf("a plan with no policy prints a release policy: %v", ready["release_policy"])
	}

	policy := []string{"dag-release-policy-record", "--plan", "p", "--actor", "parent", "--window", "3", "--handling-seconds", "600", "--red-merges", "1", "--clean-run", "2"}
	if got := summaryRun(t, state, 0, policy...); got["schema"] != "dag-release-policy-record/1" || got["policy_seq"] != float64(1) || got["replayed"] != false || got["window"] != float64(3) {
		t.Fatalf("policy = %v", got)
	}
	if got := summaryRun(t, state, 0, policy...); got["replayed"] != true || got["policy_seq"] != float64(1) {
		t.Fatalf("the same policy again = %v, want a replay", got)
	}
	if out, code := crw(t, state, "dag-release-policy-record", "--plan", "p", "--actor", "parent", "--window", "0", "--handling-seconds", "600", "--red-merges", "1", "--clean-run", "1"); code != 2 || reasonOf(t, out) != "malformed_receipt" {
		t.Fatalf("a window of 0 (exit %d):\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-release-policy-record", "--plan", "p", "--actor", "someone", "--window", "3", "--handling-seconds", "600", "--red-merges", "1", "--clean-run", "1"); code != 2 || reasonOf(t, out) != "scope_role_mismatch" {
		t.Fatalf("a task that is not the parent (exit %d):\n%s", code, out)
	}

	// the 30-minute conflict is over the 10 minutes the policy allows: local-optimistic release is off and the candidate is held back, with the basis in the reading
	ready = summaryRun(t, state, 0, "dag-ready", "--plan", "p")
	rp := objectOf(t, ready, "release_policy")
	if rp["local_optimistic"] != "off" || rp["landings"] != float64(1) || objectOf(t, rp, "settings")["handling_seconds"] != float64(600) {
		t.Fatalf("release_policy = %v", rp)
	}
	switches := rp["transitions"].([]any)
	if len(switches) != 1 || switches[0].(map[string]any)["to"] != "off" || switches[0].(map[string]any)["node_id"] != "l1" {
		t.Fatalf("transitions = %v", switches)
	}
	var cand map[string]any
	for _, n := range ready["nodes"].([]any) {
		if n.(map[string]any)["node_id"] == "cand" {
			cand = n.(map[string]any)
		}
	}
	if cand["disposition"] != "defer" || cand["reason"] != "defer:edit_overlap" || objectOf(t, cand, "release")["held_by_policy"] != true || objectOf(t, cand, "release")["rule"] != "defer" {
		t.Fatalf("the candidate = %v", cand)
	}

	// a recorded pass keeps the state, and the measurements then have a pass to read
	if got := summaryRun(t, state, 0, "dag-ready", "--plan", "p", "--record", "--actor", "parent"); got["pass_seq"] != float64(1) {
		t.Fatalf("recorded pass = %v", got["pass_seq"])
	}
	if got := summaryRun(t, state, 0, "dag-measurements", "--plan", "p"); objectOf(t, got, "parallelism")["absent"] != nil || objectOf(t, got, "parallelism")["samples"] != float64(1) {
		t.Fatalf("parallelism after a pass = %v", got["parallelism"])
	}

	// the parent states a green dev for the landing; it records the statement and nothing about it is checked
	result := []string{"dag-landing-result-record", "--plan", "p", "--node", "l1", "--actor", "parent", "--kind", "dev_green", "--commit", "abcdef1", "--evidence", "dev-gate run 99"}
	if got := summaryRun(t, state, 0, result...); got["schema"] != "dag-landing-result-record/1" || got["kind"] != "dev_green" || got["replayed"] != false || got["commit"] != "abcdef1" {
		t.Fatalf("result = %v", got)
	}
	if got := summaryRun(t, state, 0, result...); got["replayed"] != true {
		t.Fatalf("the same statement again = %v", got)
	}
	if out, code := crw(t, state, "dag-landing-result-record", "--plan", "p", "--node", "l1", "--actor", "parent", "--kind", "great", "--evidence", "x"); code != 2 || reasonOf(t, out) != "malformed_receipt" {
		t.Fatalf("an unknown kind (exit %d):\n%s", code, out)
	}
	after := summaryRun(t, state, 0, "dag-measurements", "--plan", "p")
	if got := objectOf(t, after, "post_merge"); got["absent"] != nil || got["dev_green"] != float64(1) || got["dev_red"] != float64(0) {
		t.Fatalf("post merge = %v", got)
	}

	// a plan nobody registered
	if out, code := crw(t, state, "dag-measurements", "--plan", "nope"); code != 2 || reasonOf(t, out) != "unregistered_scope" {
		t.Fatalf("measurements of an unknown plan (exit %d):\n%s", code, out)
	}
}

func TestCLIRegionDeclareNarrowsAndRefusesWidening(t *testing.T) {
	state := policyCLIState(t)
	declare := func(regions string) (string, int) {
		return crw(t, state, "dag-region-declare", "--plan", "p", "--node", "hold", "--actor", "parent", "--regions", regions)
	}
	// hold declared a.go as local and its child runs: the same declaration is a replay, a narrowing is accepted and says so, a widening is refused
	if out, code := declare(`[{"repository":"owner/repo","path":"a.go","kind":"file","grade":"local"}]`); code != 0 || parseOut(t, out)["replayed"] != true || parseOut(t, out)["narrowed"] != nil {
		t.Fatalf("the same declaration (exit %d):\n%s", code, out)
	}
	if out, code := declare(`[{"repository":"owner/repo","path":"a.go","kind":"symbol","key":"Foo","grade":"local"}]`); code != 0 || parseOut(t, out)["narrowed"] != true || parseOut(t, out)["declaration_seq"] != float64(2) || parseOut(t, out)["replayed"] != false {
		t.Fatalf("a narrowing (exit %d):\n%s", code, out)
	}
	if out, code := declare(`[{"repository":"owner/repo","path":"a.go","kind":"file","grade":"local"}]`); code != 2 || reasonOf(t, out) != "disposition_conflict" {
		t.Fatalf("a widening back to the file (exit %d):\n%s", code, out)
	}
	if out, code := declare(`[{"repository":"owner/repo","path":"b.go","kind":"file","grade":"local"}]`); code != 2 || reasonOf(t, out) != "disposition_conflict" {
		t.Fatalf("a region beside it (exit %d):\n%s", code, out)
	}
}
