package dagsched

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-411, criterion c2: when the last k landings show a slow conflict or a post-merge red beyond the recorded thresholds, local-optimistic release is switched off, and it is switched
// on again after a run of clean landings; each switch and its basis is in the reading and in the recorded pass.

// policyWorld is a plan with a running holder and a candidate that overlaps it locally, and nodes that land one after another. The candidate is released beside the holder while local-optimistic
// release is on and deferred while it is off.
type policyWorld struct {
	t *testing.T
	f *fixture
}

func policyAt(minute int) time.Time { return time.Date(2026, 10, 2, 12, minute, 0, 0, time.UTC) }

func stamp(t time.Time) string { return t.Format(time.RFC3339Nano) }

func newPolicyWorld(t *testing.T, landing ...string) *policyWorld {
	t.Helper()
	return newPolicyWorldOn(t, newFixture(t), landing...)
}

// newPolicyWorldOn is the world over a fixture the caller made (the command-line tests need a store at a path the binary can open).
func newPolicyWorldOn(t *testing.T, f *fixture, landing ...string) *policyWorld {
	t.Helper()
	f.projectParent()
	var changes []doc
	for _, n := range append(append([]string(nil), landing...), "hold", "cand") {
		changes = append(changes, addNode(n, dag.NodeImplementation))
	}
	f.putPlan("p", 0, "p-r1", changes...)
	ctx := context.Background()
	for i, n := range landing {
		if _, err := f.sched.DeclareRegions(ctx, "p", n, "parent", []Region{{Repository: "owner/repo", Path: "land/" + string(rune('a'+i)) + ".go", Kind: "file", Change: "edit"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"hold", "cand"} {
		if _, err := f.sched.DeclareRegions(ctx, "p", n, "parent", []Region{gr("a.go", "local", "")}); err != nil {
			t.Fatal(err)
		}
	}
	f.startNode("p", "hold")
	return &policyWorld{t: t, f: f}
}

func (w *policyWorld) policy(window, handling, reds, clean int64) PolicyRecord {
	w.t.Helper()
	got, err := w.f.sched.RecordReleasePolicy(context.Background(), "p", "parent", PolicyInput{Window: window, HandlingSeconds: handling, RedMerges: reds, CleanRun: clean})
	if err != nil {
		w.t.Fatal(err)
	}
	return got
}

// land accepts a node's pull request and records its landing at the minute given. A positive handling is a conflict sweep of its head against a tip that many seconds before the landing.
func (w *policyWorld) land(node string, minute int, handling int64) accepted {
	w.t.Helper()
	a := w.f.acceptNode("p", node, pinnedOpts)
	w.f.integrate(a, "owner/repo", "dev", true, true)
	w.f.exec("UPDATE dag_integration_observations SET observed_at = ? WHERE acceptance_id = ?", stamp(policyAt(minute)), a.Acceptance.AcceptanceID)
	if handling > 0 {
		w.sweep(node, policyAt(minute).Add(-time.Duration(handling)*time.Second), "observed", 3)
	}
	return a
}

// sweep records a sweep at the time given with one tip member for the node.
func (w *policyWorld) sweep(node string, at time.Time, status string, conflicts int) {
	w.t.Helper()
	seq := w.f.count("SELECT COALESCE(MAX(sweep_seq), 0) + 1 FROM dag_conflict_sweeps WHERE plan_id = 'p'")
	w.f.exec("INSERT INTO dag_conflict_sweeps (plan_id, sweep_seq, trigger_kind, trigger_node, trigger_ref, repository, observed_by, observed_at) VALUES ('p', ?, 'manual', '', '', 'owner/repo', 'parent', ?)", seq, stamp(at))
	w.f.exec("INSERT INTO dag_conflict_sweep_members (plan_id, sweep_seq, member_seq, kind, left_node_id, right_node_id, left_head, right_head, left_head_source, right_head_source, status, reason, observation_id, conflicts)"+
		" VALUES ('p', ?, 1, 'tip', ?, '', ?, '', 'acceptance', '', ?, '', 'dto-seeded', ?)", seq, node, head1, status, conflicts)
}

func (w *policyWorld) result(node, kind string) {
	w.t.Helper()
	if _, err := w.f.sched.RecordLandingResult(context.Background(), "p", node, "parent", ResultInput{Kind: kind, Evidence: "dev-gate run of the merge commit"}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *policyWorld) pass() (Reading, int64) {
	w.t.Helper()
	reading, seq, err := w.f.sched.RecordPass(context.Background(), "p", "parent", ReadyOptions{})
	if err != nil {
		w.t.Fatal(err)
	}
	return reading, seq
}

func (w *policyWorld) recorded(seq int64) string {
	w.t.Helper()
	var text string
	if err := w.f.s.DB.QueryRowContext(context.Background(), "SELECT policy_json FROM dag_pass_release_policy WHERE plan_id = 'p' AND pass_seq = ?", seq).Scan(&text); err != nil {
		w.t.Fatalf("pass %d kept no release policy: %v", seq, err)
	}
	return text
}

// wantOptimism is the candidate's reading while the policy is in the state given: ready beside a local holder when on; deferred as edit overlap, with the rule defer and held_by_policy, when off.
func wantOptimism(t *testing.T, reading Reading, state string) {
	t.Helper()
	cand := reading.node("cand")
	if reading.ReleasePolicy == nil {
		t.Fatal("the reading carries no release policy")
	}
	if got := optimismWord(reading.ReleasePolicy.On); got != state {
		t.Fatalf("local-optimistic release is %s (%s), want %s", got, reading.ReleasePolicy.Reason, state)
	}
	switch state {
	case OptimismOn:
		if cand.Disposition != DispReady || cand.Release == nil || cand.Release.Rule != RuleLocalOptimistic || cand.Release.HeldByPolicy {
			t.Fatalf("the candidate with optimism on = %+v / %+v, want ready under local-optimistic", cand, cand.Release)
		}
	case OptimismOff:
		if cand.Disposition != DispDefer || cand.Reason != DeferEditOverlap || cand.Release == nil || cand.Release.Rule != RuleDefer || !cand.Release.HeldByPolicy {
			t.Fatalf("the candidate with optimism off = %+v / %+v, want defer:edit_overlap held by the policy", cand, cand.Release)
		}
		if !strings.Contains(cand.Detail, "release policy") || !strings.Contains(cand.Detail, "local overlap with hold on a.go") {
			t.Fatalf("the detail does not say what held it: %q", cand.Detail)
		}
		if reading.Pass.DecidingLimit != LimitEditOverlap {
			t.Fatalf("the deciding limit = %s, want edit_overlap", reading.Pass.DecidingLimit)
		}
	}
}

func TestSlowLandingSwitchesOptimismOffAndCleanLandingsSwitchItOnAgain(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2", "l3", "l4")
	if reading := w.f.read("p"); reading.ReleasePolicy != nil || reading.node("cand").Disposition != DispReady {
		t.Fatalf("a plan with no policy = %+v, want a ready candidate and no release policy", reading.ReleasePolicy)
	}
	if got := w.policy(3, 600, 1, 2); got.Replayed || got.Settings.Seq != 1 {
		t.Fatalf("the policy = %+v", got)
	}
	if got := w.policy(3, 600, 1, 2); !got.Replayed || got.Settings.Seq != 1 {
		t.Fatalf("the same values again = %+v, want a replay", got)
	}
	reading, first := w.pass()
	wantOptimism(t, reading, OptimismOn)
	if reading.ReleasePolicy.Landings != 0 || len(reading.ReleasePolicy.Transitions) != 0 {
		t.Fatalf("before any landing: %+v", reading.ReleasePolicy)
	}

	w.land("l1", 1, 0)
	wantOptimism(t, w.f.read("p"), OptimismOn)

	// a landing whose conflict took 1800 seconds to settle, over the 600 the policy allows
	w.land("l2", 2, 1800)
	reading, second := w.pass()
	wantOptimism(t, reading, OptimismOff)
	policy := reading.ReleasePolicy
	if len(policy.Transitions) != 1 || policy.Transitions[0].NodeID != "l2" || policy.Transitions[0].From != OptimismOn || policy.Transitions[0].To != OptimismOff {
		t.Fatalf("the switch = %+v, want l2 on to off", policy.Transitions)
	}
	if !strings.Contains(policy.Reason, "l2 took 1800 seconds") || len(policy.Window) != 2 || policy.Window[1].HandlingSeconds == nil || !policy.Window[1].slow(policy.Settings) {
		t.Fatalf("the basis = %q window %+v", policy.Reason, policy.Window)
	}

	// one clean landing is not a run of two: the slow one is still in the window
	w.land("l3", 3, 0)
	wantOptimism(t, w.f.read("p"), OptimismOff)

	// the second clean landing in a row switches it on again, though the slow one is still in the window
	w.land("l4", 4, 0)
	reading, third := w.pass()
	wantOptimism(t, reading, OptimismOn)
	if got := reading.ReleasePolicy.Transitions; len(got) != 2 || got[1].NodeID != "l4" || got[1].To != OptimismOn {
		t.Fatalf("the switches = %+v, want off at l2 and on at l4", got)
	}

	// each recorded pass kept the state it saw
	for seq, want := range map[int64]struct{ state, notIn string }{first: {state: "\"local_optimistic\":\"on\""}, second: {state: "\"local_optimistic\":\"off\""}, third: {state: "\"local_optimistic\":\"on\""}} {
		if text := w.recorded(seq); !strings.Contains(text, want.state) {
			t.Fatalf("pass %d kept %s, want %s", seq, text, want.state)
		}
	}
	if text := w.recorded(second); !strings.Contains(text, "\"because\":\"landing l2 took 1800 seconds") || !strings.Contains(text, "\"to\":\"off\"") {
		t.Fatalf("the pass that saw the switch off does not carry it: %s", text)
	}
	if text := w.recorded(third); strings.Count(text, "\"from\"") != 2 {
		t.Fatalf("the pass after the second switch does not carry both: %s", text)
	}
	if w.f.count("SELECT COUNT(*) FROM dag_pass_release_policy") != 3 {
		t.Fatal("a pass of a plan with a policy keeps its state, one row each")
	}
	// a pass of a plan with no policy keeps no row (the pass table is as it was)
	if w.f.count("SELECT COUNT(*) FROM dag_passes WHERE plan_id = 'p'") != 3 {
		t.Fatal("three passes were recorded")
	}
}

func TestARedRecordedLateSwitchesOptimismOffWithoutANewLanding(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2")
	w.policy(3, 600, 1, 2)
	w.land("l1", 1, 0)
	w.land("l2", 2, 0)
	before, _ := w.pass()
	wantOptimism(t, before, OptimismOn)

	w.result("l2", ResultDevRed)
	after, seq := w.pass()
	wantOptimism(t, after, OptimismOff)
	if before.InputDigest == after.InputDigest {
		t.Fatal("the digest did not change with the result")
	}
	if got := after.ReleasePolicy.Transitions; len(got) != 1 || got[0].NodeID != "l2" || !strings.Contains(got[0].Because, "red or reverted") {
		t.Fatalf("the switch = %+v, want one at l2 for a red", got)
	}
	if !strings.Contains(w.recorded(seq), "\"post_merge_red\":true") {
		t.Fatalf("the pass does not carry the red: %s", w.recorded(seq))
	}

	// a green statement later changes nothing; a revert counts like a red
	w.result("l2", ResultDevGreen)
	wantOptimism(t, w.f.read("p"), OptimismOff)
}

func TestTwoRedsAreNeededWhenThePolicySaysTwo(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2", "l3")
	w.policy(3, 600, 2, 1)
	w.land("l1", 1, 0)
	w.land("l2", 2, 0)
	w.land("l3", 3, 0)
	w.result("l1", ResultDevRed)
	wantOptimism(t, w.f.read("p"), OptimismOn)
	w.result("l2", ResultReverted)
	// the last landing is clean, and a clean run of one switches it on
	wantOptimism(t, w.f.read("p"), OptimismOn)
	w.result("l3", ResultDevRed)
	wantOptimism(t, w.f.read("p"), OptimismOff)
}

// A landing is ordered by its time and then its node id, whatever order the rows were written in, so two readings of one store agree.
func TestLandingsOfOneInstantAreOrderedByNodeID(t *testing.T) {
	w := newPolicyWorld(t, "a", "b")
	w.policy(2, 600, 1, 1)
	w.land("b", 5, 0)
	w.land("a", 5, 1800)
	reading := w.f.read("p")
	// ordered a (slow), b (clean): the last landing is clean and a clean run of one switches it on
	wantOptimism(t, reading, OptimismOn)
	if got := reading.ReleasePolicy.Window; len(got) != 2 || got[0].NodeID != "a" || got[1].NodeID != "b" {
		t.Fatalf("the window = %+v, want a then b", got)
	}
}

// The conflict handling time starts at the first sweep after the node's first acceptance that measured its head against a tip with a conflict.
func TestHandlingTimeCountsTheSweepsAfterTheHandOver(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *policyWorld)
		want  string // "" for an absent handling time
	}{
		{"a sweep before the first acceptance is not handling time", func(w *policyWorld) {
			w.sweep("l1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "observed", 2)
		}, ""},
		{"a replayed member is a measurement made at the sweep's time", func(w *policyWorld) {
			w.sweep("l1", policyAt(5).Add(-90*time.Second), "replayed", 2)
		}, "90"},
		{"a member without a conflict is not handling time", func(w *policyWorld) {
			w.sweep("l1", policyAt(5).Add(-90*time.Second), "observed", 0)
		}, ""},
		{"a sweep that could not measure is not handling time", func(w *policyWorld) {
			w.f.exec("INSERT INTO dag_conflict_sweeps (plan_id, sweep_seq, trigger_kind, trigger_node, trigger_ref, repository, observed_by, observed_at) VALUES ('p', 1, 'manual', '', '', 'owner/repo', 'parent', ?)", stamp(policyAt(4)))
			w.f.exec("INSERT INTO dag_conflict_sweep_members (plan_id, sweep_seq, member_seq, kind, left_node_id, status, reason, conflicts) VALUES ('p', 1, 1, 'tip', 'l1', 'unmeasured', 'commit_missing', 0)")
		}, ""},
		{"a sweep after the landing is not handling time", func(w *policyWorld) {
			w.sweep("l1", policyAt(6), "observed", 2)
		}, ""},
		{"the first of two sweeps starts the clock", func(w *policyWorld) {
			w.sweep("l1", policyAt(5).Add(-300*time.Second), "observed", 2)
			w.sweep("l1", policyAt(5).Add(-60*time.Second), "observed", 2)
		}, "300"},
		{"another node's sweep is not this node's", func(w *policyWorld) {
			w.sweep("hold", policyAt(5).Add(-300*time.Second), "observed", 2)
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newPolicyWorld(t, "l1")
			w.policy(3, 600, 1, 1)
			w.land("l1", 5, 0)
			c.setup(w)
			reading := w.f.read("p")
			got := reading.ReleasePolicy.Window
			if len(got) != 1 {
				t.Fatalf("the window = %+v", got)
			}
			switch {
			case c.want == "" && got[0].HandlingSeconds != nil:
				t.Fatalf("the handling time = %d, want it absent", *got[0].HandlingSeconds)
			case c.want != "" && (got[0].HandlingSeconds == nil || itoa64(*got[0].HandlingSeconds) != c.want):
				t.Fatalf("the handling time = %v, want %s", got[0].HandlingSeconds, c.want)
			}
		})
	}
}

// A plan with no policy row, or a store whose zone predates the tables, reads and digests as it did before the policy existed.
func TestAPlanWithoutAPolicyReadsAsBefore(t *testing.T) {
	w := newPolicyWorld(t, "l1")
	w.land("l1", 1, 1800)
	withTables := w.f.read("p")
	if withTables.ReleasePolicy != nil {
		t.Fatal("a plan with no policy row has a release policy")
	}
	if strings.Contains(string(marshal(withTables.Object())), "release_policy") {
		t.Fatal("the reading of a plan with no policy prints a release_policy key")
	}
	// a store whose zone predates the tables reads the same
	for _, table := range []string{"dag_pass_release_policy", "dag_release_policy", "dag_landing_results"} {
		w.f.exec("DROP TABLE " + table)
	}
	older := w.f.read("p")
	if string(marshal(older.Object())) != string(marshal(withTables.Object())) || older.InputDigest != withTables.InputDigest {
		t.Fatalf("a store without the CRW-411 tables reads differently:\n%s\n%s", marshal(older.Object()), marshal(withTables.Object()))
	}
	if _, seq, err := w.f.sched.RecordPass(context.Background(), "p", "parent", ReadyOptions{}); err != nil || seq != 1 {
		t.Fatalf("a pass of a plan with no policy = %d, %v", seq, err)
	}
}

func TestPolicyBoundsAndAuthority(t *testing.T) {
	w := newPolicyWorld(t)
	ctx := context.Background()
	for name, in := range map[string]PolicyInput{
		"no window":                  {Window: 0, HandlingSeconds: 1, RedMerges: 1, CleanRun: 1},
		"a window over the bound":    {Window: MaxPolicyWindow + 1, HandlingSeconds: 1, RedMerges: 1, CleanRun: 1},
		"no handling time":           {Window: 3, HandlingSeconds: 0, RedMerges: 1, CleanRun: 1},
		"reds beyond the window":     {Window: 3, HandlingSeconds: 1, RedMerges: 4, CleanRun: 1},
		"no reds that switch it off": {Window: 3, HandlingSeconds: 1, RedMerges: 0, CleanRun: 1},
		"a clean run beyond window":  {Window: 3, HandlingSeconds: 1, RedMerges: 1, CleanRun: 4},
		"no clean run":               {Window: 3, HandlingSeconds: 1, RedMerges: 1, CleanRun: 0},
	} {
		if _, err := w.f.sched.RecordReleasePolicy(ctx, "p", "parent", in); refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s: %v, want malformed_receipt", name, err)
		}
	}
	if _, err := w.f.sched.RecordReleasePolicy(ctx, "p", "someone", PolicyInput{Window: 3, HandlingSeconds: 1, RedMerges: 1, CleanRun: 1}); refusalReason(err) != "scope_role_mismatch" {
		t.Errorf("a task that is not the parent: %v, want scope_role_mismatch", err)
	}
	if _, err := w.f.sched.RecordReleasePolicy(ctx, "nope", "parent", PolicyInput{Window: 3, HandlingSeconds: 1, RedMerges: 1, CleanRun: 1}); refusalReason(err) != "unregistered_scope" {
		t.Errorf("a plan nobody registered: %v, want unregistered_scope", err)
	}
	first := w.policy(3, 600, 1, 2)
	second := w.policy(4, 600, 1, 2)
	if first.Settings.Seq != 1 || second.Settings.Seq != 2 || w.f.count("SELECT COUNT(*) FROM dag_release_policy") != 2 {
		t.Fatalf("a changed policy is the next row: %+v then %+v", first, second)
	}
	if got := w.f.read("p"); got.ReleasePolicy == nil || got.ReleasePolicy.Settings.Window != 4 {
		t.Fatalf("the policy in force = %+v, want the second", got.ReleasePolicy)
	}
}

func TestLandingResultsAreStatementsOfTheParent(t *testing.T) {
	w := newPolicyWorld(t, "l1")
	ctx := context.Background()
	rec := func(node, actor string, in ResultInput) (ResultRecord, error) {
		return w.f.sched.RecordLandingResult(ctx, "p", node, actor, in)
	}
	good := ResultInput{Kind: ResultDevRed, Commit: "abcdef1234", Evidence: "dev-gate run 123 failed on the merge commit"}
	first, err := rec("l1", "parent", good)
	if err != nil || first.Replayed {
		t.Fatalf("the first statement = %+v, %v", first, err)
	}
	if again, err := rec("l1", "parent", good); err != nil || !again.Replayed || again.ResultID != first.ResultID {
		t.Fatalf("the same statement again = %+v, %v, want a replay", again, err)
	}
	if other, err := rec("l1", "parent", ResultInput{Kind: ResultReverted, Evidence: "reverted by the parent"}); err != nil || other.Replayed {
		t.Fatalf("a second kind for the node = %+v, %v", other, err)
	}
	if w.f.count("SELECT COUNT(*) FROM dag_landing_results") != 2 {
		t.Fatal("a node carries several kinds and a repeat is one row")
	}
	for name, in := range map[string]ResultInput{
		"an unknown kind":    {Kind: "green", Evidence: "x"},
		"no evidence":        {Kind: ResultDevRed, Evidence: "  "},
		"a bad commit":       {Kind: ResultDevRed, Commit: "XYZ", Evidence: "x"},
		"too much evidence":  {Kind: ResultDevRed, Evidence: strings.Repeat("x", MaxResultEvidenceBytes+1)},
		"a control in proof": {Kind: ResultDevRed, Evidence: "a\x01b"},
	} {
		if _, err := rec("l1", "parent", in); refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s: %v, want malformed_receipt", name, err)
		}
	}
	if _, err := rec("l1", "someone", good); refusalReason(err) != "scope_role_mismatch" {
		t.Errorf("a task that is not the parent: %v, want scope_role_mismatch", err)
	}
	if _, err := rec("ghost", "parent", good); refusalReason(err) != "unregistered_scope" {
		t.Errorf("a node the plan does not have: %v, want unregistered_scope", err)
	}
}

// A fenced decision keeps the epoch of the session that made it, as the other decision rows do: a parent that restarts under a later epoch leaves rows that say which session decided.
func TestPolicyAndResultRowsKeepTheCoordinatorEpoch(t *testing.T) {
	w := newPolicyWorld(t, "l1")
	ctx := context.Background()
	claim, err := w.f.sched.ClaimEpoch(ctx, "p", ClaimInput{Actor: "parent", SessionNonce: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	w.f.sched.ExpectedEpoch = claim.Epoch
	w.policy(3, 600, 1, 2)
	w.result("l1", ResultDevGreen)
	for _, table := range []string{"dag_release_policy", "dag_landing_results"} {
		if got := w.f.count("SELECT coordinator_epoch FROM " + table + " WHERE plan_id = 'p'"); int64(got) != claim.Epoch || claim.Epoch < 1 {
			t.Fatalf("%s kept epoch %d, want the claimed epoch %d", table, got, claim.Epoch)
		}
	}
	// a session of another epoch is stopped where it writes, and leaves no row
	w.f.sched.ExpectedEpoch = claim.Epoch + 1
	if _, err := w.f.sched.RecordReleasePolicy(ctx, "p", "parent", PolicyInput{Window: 4, HandlingSeconds: 600, RedMerges: 1, CleanRun: 1}); refusalReason(err) != "stale_coordinator_epoch" {
		t.Fatalf("a policy of another epoch: %v, want stale_coordinator_epoch", err)
	}
	if _, err := w.f.sched.RecordLandingResult(ctx, "p", "l1", "parent", ResultInput{Kind: ResultDevRed, Evidence: "x"}); refusalReason(err) != "stale_coordinator_epoch" {
		t.Fatalf("a result of another epoch: %v, want stale_coordinator_epoch", err)
	}
	if w.f.count("SELECT COUNT(*) FROM dag_release_policy") != 1 || w.f.count("SELECT COUNT(*) FROM dag_landing_results") != 1 {
		t.Fatal("a refused write left a row")
	}
}

// A plan revision that retires a node or changes its kind after its pull request landed leaves the landed work as it was: the parent can still state what dev did, and the release policy reads it.
func TestAResultCanBeStatedForWorkThatAPlanRevisionRetired(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2")
	w.policy(3, 600, 1, 1)
	w.land("l1", 1, 0)
	w.land("l2", 2, 0)
	w.f.putPlan("p", 1, "p-r2", lifeOp("retire_node", "l1"))
	if _, live := nodeOf(w.f.snapshot("p"), "l1"); live {
		t.Fatal("the node was not retired")
	}
	if _, err := w.f.sched.RecordLandingResult(context.Background(), "p", "l1", "parent", ResultInput{Kind: ResultDevRed, Evidence: "dev-gate run of the merge commit failed"}); err != nil {
		t.Fatalf("a result for a retired node that had landed: %v", err)
	}
	// a node the plan never had is still refused
	if _, err := w.f.sched.RecordLandingResult(context.Background(), "p", "ghost", "parent", ResultInput{Kind: ResultDevRed, Evidence: "x"}); refusalReason(err) != "unregistered_scope" {
		t.Fatalf("a node the plan never had: %v, want unregistered_scope", err)
	}
}
