package dagsched

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The continuous conflict observation (CRW-410, criterion c1) over real git repositories: every pair of live heads and every head against the tip is measured and recorded with a ledger row, a conflict
// on a path a node did not declare is marked as drift, and what git cannot be asked is a member that says why.

// sweepWorld is a repository with a base commit and four branches off it that each change a file: I (lands on dev), D and E (both rewrite line 3 of c.txt, as I does) and F (its own file).
type sweepWorld struct {
	k         *integrationKit
	base, tip string
	head      map[string]string
}

func newSweepWorld(t *testing.T) *sweepWorld { return buildSweepWorld(t, newIntegrationKit(t)) }

// buildSweepWorld lays the repository and the plan's other nodes over a kit.
func buildSweepWorld(t *testing.T, k *integrationKit) *sweepWorld {
	t.Helper()
	repo := k.repo
	repo.git("remote", "add", "origin", "https://github.com/owner/repo.git")
	base := repo.commit("c.txt", lines(12, nil))
	k.putPlan("g", 1, "g-r2", addRelNode("E", dag.NodeImplementation), addRelNode("F", dag.NodeImplementation))
	w := &sweepWorld{k: k, base: base, head: map[string]string{}}
	branch := func(node, file, content string) {
		repo.git("checkout", "-q", "-b", "b-"+node, base)
		w.head[node] = repo.commit(file, content)
		repo.git("checkout", "-q", "dev")
	}
	branch("I", "c.txt", lines(12, map[int]string{3: "landed"}))
	branch("D", "c.txt", lines(12, map[int]string{3: "d side"}))
	branch("E", "c.txt", lines(12, map[int]string{3: "e side"}))
	branch("F", "f.txt", "f\n")
	repo.git("merge", "-q", "--no-ff", "-m", "land I", "b-I")
	w.tip = repo.git("rev-parse", "HEAD")
	return w
}

// accept accepts nodes on their branch heads: the heads the relay itself would have read from the forge.
func (w *sweepWorld) accept(nodes ...string) {
	w.k.t.Helper()
	for i, n := range nodes {
		w.k.acceptNode("g", n, acceptOpts{HeadSHA: w.head[n], PR: int64(10 + i), Forge: "owner/repo", Repository: w.k.repo.path})
	}
}

func (w *sweepWorld) sweep(trigger, node, ref string, mutate ...func(*SweepInput)) (SweepResult, error) {
	w.k.t.Helper()
	in := SweepInput{Repository: w.k.repo.path, Trigger: trigger, TriggerNode: node, TriggerRef: ref, Tips: []SweepTip{{Repository: w.k.repo.path, Ref: "dev", SHA: w.tip}}}
	for _, m := range mutate {
		m(&in)
	}
	return w.k.sched.ObserveLive(context.Background(), "g", "parent", in)
}

func (w *sweepWorld) region(node, path, grade string) {
	w.k.t.Helper()
	if _, err := w.k.sched.DeclareRegions(context.Background(), "g", node, "parent", []Region{{Repository: "owner/repo", Path: path, Kind: "file", Change: "edit", Grade: grade}}); err != nil {
		w.k.t.Fatalf("declare %s: %v", node, err)
	}
}

func memberOf(r SweepResult, kind, left, right string) SweepMember {
	for _, m := range r.Members {
		if m.Kind == kind && m.LeftNode == left && m.RightNode == right {
			return m
		}
	}
	return SweepMember{Status: "absent"}
}

// Every pair of the live heads and every head against the tip is measured and recorded, with the sweep's ledger row, in real git: D and E conflict on c.txt with each other and with the tip (which carries
// I's change to the same line); F conflicts with nothing. I landed, so it is not a live head.
func TestSweepMeasuresEveryPairAndTip(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	w.accept("D", "E", "F")
	res, err := w.sweep(TriggerLanding, "I", "dio-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != SweepRecorded || res.Seq != 1 || res.Trigger != TriggerLanding || res.TriggerNode != "I" || res.TriggerRef != "dio-1" || len(res.Members) != 6 {
		t.Fatalf("sweep = %+v", res)
	}
	want := map[string]struct {
		conflicts int
		files     []string
	}{"pair D E": {1, []string{"c.txt"}}, "pair D F": {0, nil}, "pair E F": {0, nil}, "tip D ": {1, []string{"c.txt"}}, "tip E ": {1, []string{"c.txt"}}, "tip F ": {0, nil}}
	for _, m := range res.Members {
		key := m.Kind + " " + m.LeftNode + " " + m.RightNode
		w, ok := want[key]
		if !ok || m.Status != MemberObserved || m.Conflicts != w.conflicts || !reflect.DeepEqual(m.Files, w.files) || m.LeftSource != HeadAcceptance || m.ObservationID == "" {
			t.Errorf("member %s = %+v, want %+v", key, m, w)
		}
	}
	if observed, replayed, unmeasured := res.Counts(); observed != 6 || replayed != 0 || unmeasured != 0 {
		t.Errorf("counts = %d %d %d", observed, replayed, unmeasured)
	}
	for table, n := range map[string]int{"dag_conflict_observations": 3, "dag_tip_conflict_observations": 3, "dag_conflict_sweeps": 1, "dag_conflict_sweep_members": 6} {
		if got := k.count("SELECT COUNT(*) FROM " + table); got != n {
			t.Errorf("%s has %d rows, want %d", table, got, n)
		}
	}
	// the tip observation is bound to the head and the tip it was measured against
	var node, head, source, tip, ref string
	if err := k.s.DB.QueryRow("SELECT node_id, head, head_source, tip_sha, tip_ref FROM dag_tip_conflict_observations WHERE node_id = 'D'").Scan(&node, &head, &source, &tip, &ref); err != nil ||
		head != w.head["D"] || source != HeadAcceptance || tip != w.tip || ref != k.repo.path+"@dev" {
		t.Errorf("tip row = %s %s %s %s %s %v", node, head, source, tip, ref, err)
	}
	// landed I is not a live head, and nothing of the checkout moved
	if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweep_members WHERE left_node_id = 'I' OR right_node_id = 'I'"); got != 0 {
		t.Errorf("a landed node was measured: %d members", got)
	}
}

// A repeat measures the same pairs again: every member is a replay (no second observation row), and the ledger keeps a row for the repeat, as dag_passes keeps one per pass.
func TestSweepRepeatIsReplayedAndLedgered(t *testing.T) {
	w := newSweepWorld(t)
	w.accept("D", "E", "F")
	if _, err := w.sweep(TriggerLanding, "I", "dio-1"); err != nil {
		t.Fatal(err)
	}
	again, err := w.sweep(TriggerManual, "", "")
	if err != nil || again.Seq != 2 {
		t.Fatalf("again = %v %+v", err, again)
	}
	if observed, replayed, unmeasured := again.Counts(); observed != 0 || replayed != 6 || unmeasured != 0 {
		t.Fatalf("counts = %d %d %d", observed, replayed, unmeasured)
	}
	for table, n := range map[string]int{"dag_conflict_observations": 3, "dag_tip_conflict_observations": 3, "dag_conflict_sweeps": 2, "dag_conflict_sweep_members": 12} {
		if got := w.k.count("SELECT COUNT(*) FROM " + table); got != n {
			t.Errorf("%s has %d rows, want %d", table, got, n)
		}
	}
	// a manual sweep repeats freely
	if third, err := w.sweep(TriggerManual, "", ""); err != nil || third.Seq != 3 || third.State != SweepRecorded {
		t.Fatalf("third = %v %+v", err, third)
	}
}

// A landing or an accepted receipt is swept once: a repeat of its trigger reference finds the sweep and writes nothing, also when two writers meet; a failure inside the recording leaves nothing, and
// the repeat records exactly one complete sweep.
func TestSweepOfOneTriggerIsOneRowAndAFailureLeavesNothing(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	w.accept("D", "E", "F")
	tables := []string{"dag_conflict_observations", "dag_conflict_observation_files", "dag_tip_conflict_observations", "dag_tip_conflict_observation_files", "dag_conflict_drift", "dag_conflict_sweeps", "dag_conflict_sweep_members"}
	rows := func() map[string]int {
		out := map[string]int{}
		for _, table := range tables {
			out[table] = k.count("SELECT COUNT(*) FROM " + table)
		}
		return out
	}
	empty := rows()
	k.sched.testInSweepTx = func() error { return errors.New("the process died inside the transaction") }
	if _, err := w.sweep(TriggerLanding, "I", "dio-1"); err == nil || !strings.Contains(err.Error(), "died") {
		t.Fatalf("sweep = %v", err)
	}
	if got := rows(); !reflect.DeepEqual(got, empty) {
		t.Fatalf("a failed sweep left rows: %v", got)
	}
	k.sched.testInSweepTx = nil
	first, err := w.sweep(TriggerLanding, "I", "dio-1")
	if err != nil || first.State != SweepRecorded || first.Seq != 1 {
		t.Fatalf("retry = %v %+v", err, first)
	}
	afterFirst := rows()
	again, err := w.sweep(TriggerLanding, "I", "dio-1")
	if err != nil || again.State != SweepAlready || again.Seq != 1 || len(again.Members) != 0 {
		t.Fatalf("repeat = %v %+v", err, again)
	}
	if got := rows(); !reflect.DeepEqual(got, afterFirst) {
		t.Fatalf("a repeat of the trigger wrote rows: %v, was %v", got, afterFirst)
	}
	// two writers of one trigger: one row, the other finds it
	var wg sync.WaitGroup
	results := make([]SweepResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = w.sweep(TriggerReceipt, "D", "acc-1")
		}()
	}
	wg.Wait()
	states := []string{results[0].State, results[1].State}
	if errs[0] != nil || errs[1] != nil || !((states[0] == SweepRecorded && states[1] == SweepAlready) || (states[0] == SweepAlready && states[1] == SweepRecorded)) {
		t.Fatalf("two writers: %v %v %v", states, errs[0], errs[1])
	}
	if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweeps WHERE trigger_kind = 'receipt' AND trigger_ref = 'acc-1'"); got != 1 {
		t.Fatalf("receipt sweeps = %d", got)
	}
}

// A conflict on a path a node did not declare is drift for that node, and only for it; a path no node declared is drift for every node that edited it. The tip observation checks the node alone.
func TestSweepMarksDeclarationDrift(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	// D declared c.txt; E and F declared f.txt only (a declaration is made before the release)
	w.region("D", "c.txt", "local")
	w.region("E", "f.txt", "local")
	w.region("F", "f.txt", "local")
	w.accept("D", "E", "F")
	res, err := w.sweep(TriggerLanding, "I", "dio-1")
	if err != nil {
		t.Fatal(err)
	}
	pair := memberOf(res, MemberPair, "D", "E")
	if !reflect.DeepEqual(pair.Drift, []DriftMark{{Node: "E", Path: "c.txt"}}) {
		t.Errorf("D-E drift = %v, want E only", pair.Drift)
	}
	if got := memberOf(res, MemberTip, "D", "").Drift; got != nil {
		t.Errorf("D against the tip drifted: %v", got)
	}
	if got := memberOf(res, MemberTip, "E", "").Drift; !reflect.DeepEqual(got, []DriftMark{{Node: "E", Path: "c.txt"}}) {
		t.Errorf("E against the tip: %v", got)
	}
	if got := memberOf(res, MemberPair, "D", "F").Drift; got != nil {
		t.Errorf("a clean pair has drift: %v", got)
	}
	rows := k.count("SELECT COUNT(*) FROM dag_conflict_drift")
	if rows != 2 {
		t.Errorf("drift rows = %d, want 2", rows)
	}
	// a replay reports what was recorded
	again, err := w.sweep(TriggerManual, "", "")
	if err != nil || !reflect.DeepEqual(memberOf(again, MemberPair, "D", "E").Drift, pair.Drift) {
		t.Errorf("replayed drift = %v %v", err, memberOf(again, MemberPair, "D", "E").Drift)
	}
}

// No node declared the path: both nodes of the pair drifted, through the manual dag-conflict-observe too.
func TestObserveConflictsMarksDriftAndLeavesALedgerRow(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	w.accept("D", "E")
	res, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: k.repo.path, LeftNode: "D", RightNode: "E", LeftHead: w.head["D"], RightHead: w.head["E"]})
	if err != nil {
		t.Fatal(err)
	}
	if want := []DriftMark{{Node: "D", Path: "c.txt"}, {Node: "E", Path: "c.txt"}}; !reflect.DeepEqual(res.Drift, want) || res.Conflicts != 1 {
		t.Fatalf("result = %+v", res)
	}
	var kind, source string
	if err := k.s.DB.QueryRow("SELECT s.trigger_kind, m.left_head_source FROM dag_conflict_sweeps s JOIN dag_conflict_sweep_members m ON m.plan_id = s.plan_id AND m.sweep_seq = s.sweep_seq").Scan(&kind, &source); err != nil || kind != TriggerManual || source != HeadExplicit {
		t.Fatalf("ledger = %s %s %v", kind, source, err)
	}
	// the other way round is the same observation, replayed, with the same drift
	again, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: k.repo.path, LeftNode: "E", RightNode: "D", LeftHead: w.head["E"], RightHead: w.head["D"]})
	if err != nil || !again.Replayed || again.ObservationID != res.ObservationID || !reflect.DeepEqual(again.Drift, res.Drift) || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps") != 2 {
		t.Fatalf("again = %v %+v", err, again)
	}
}

// What git cannot be asked is a member with a reason and the sweep goes on: a node with no head, a commit the checkout lacks, a tip that is missing or unreadable, two histories with nothing in common.
func TestSweepAccountsForWhatItCannotMeasure(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	repo := k.repo
	w.accept("D")
	k.startNode("g", "E") // running, no checkout of its own, nothing accepted: no head
	repo.git("checkout", "-q", "--orphan", "unrelated")
	repo.git("rm", "-q", "-rf", ".")
	orphan := repo.commit("o.txt", "o\n")
	repo.git("checkout", "-q", "dev")
	missing := strings.Repeat("1", 40)

	res, err := w.sweep(TriggerManual, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberUnmeasured || m.Reason != ReasonHeadUnknown {
		t.Errorf("a node with no head: %+v", m)
	}
	if m := memberOf(res, MemberTip, "E", ""); m.Status != MemberUnmeasured || m.Reason != ReasonHeadUnknown {
		t.Errorf("its tip: %+v", m)
	}
	if m := memberOf(res, MemberTip, "D", ""); m.Status != MemberObserved || m.Conflicts != 1 {
		t.Errorf("D against the tip is measured all the same: %+v", m)
	}
	res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Heads = map[string]string{"E": missing} })
	if err != nil {
		t.Fatal(err)
	}
	if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberUnmeasured || m.Reason != ReasonCommitMissing || m.Detail == "" || m.LeftHead != w.head["D"] {
		t.Errorf("a commit the checkout lacks: %+v", m)
	}
	if m := memberOf(res, MemberTip, "E", ""); m.Status != MemberUnmeasured || m.Reason != ReasonCommitMissing || m.LeftSource != HeadExplicit {
		t.Errorf("its tip: %+v", m)
	}
	res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Heads = map[string]string{"E": orphan} })
	if err != nil {
		t.Fatal(err)
	}
	if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberUnmeasured || m.Reason != ReasonNoCommonAncestor {
		t.Errorf("histories with nothing in common: %+v", m)
	}
	for name, tip := range map[string]SweepTip{"a tip the checkout lacks": {Repository: repo.path, Ref: "dev", SHA: missing}, "a tip nobody could read": {Repository: repo.path, Ref: "dev"}} {
		res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Tips = []SweepTip{tip} })
		if err != nil {
			t.Fatal(err)
		}
		if m := memberOf(res, MemberTip, "D", ""); m.Status != MemberUnmeasured || m.Reason != ReasonTipUnreadable {
			t.Errorf("%s: %+v", name, m)
		}
	}
	// an unmeasured member writes no observation, only its ledger row
	if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweep_members WHERE status = 'unmeasured' AND observation_id <> ''"); got != 0 {
		t.Errorf("unmeasured members with an observation: %d", got)
	}
}

// The head of a running node is the HEAD of the checkout its child works in, when that is a worktree of the sweep's repository; the parent's word beats it, a current accepted result beats it, and a
// checkout that is another repository, or the sweep's own working tree, is no source.
func TestSweepHeadSources(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	repo := k.repo
	worktree := func(name, base string) *gitRepo {
		dir := filepath.Join(t.TempDir(), name)
		repo.git("worktree", "add", "-q", "-b", "wt-"+name, dir, base)
		return &gitRepo{t: t, path: dir}
	}
	childHead := func(node, cwd string) {
		k.exec("UPDATE relationships SET child_cwd = ? WHERE relationship_id = ?", cwd, "rel-g-"+node)
	}
	w.accept("D")
	k.startNode("g", "E")
	e := worktree("e", w.base)
	eHead := e.commit("e.txt", "pushed by the child\n")
	childHead("E", e.path)

	res, err := w.sweep(TriggerReceipt, "E", "", func(in *SweepInput) { in.Tips = nil })
	if err != nil {
		t.Fatal(err)
	}
	if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberObserved || m.RightHead != eHead || m.RightSource != HeadChildCheckout || m.LeftSource != HeadAcceptance {
		t.Errorf("a running node is measured at its child's HEAD: %+v", m)
	}
	// the parent's word beats the checkout
	res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Heads = map[string]string{"E": w.head["E"]} })
	if err != nil {
		t.Fatal(err)
	}
	if m := memberOf(res, MemberPair, "D", "E"); m.RightHead != w.head["E"] || m.RightSource != HeadExplicit || m.Conflicts != 1 {
		t.Errorf("an explicit head: %+v", m)
	}
	// a checkout of another repository, and the sweep's own working tree, are not the child's
	other := newGitRepo(t)
	childHead("E", other.path)
	if res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Tips = nil }); err != nil {
		t.Fatal(err)
	} else if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberUnmeasured || m.Reason != ReasonCheckoutMismatch {
		t.Errorf("another repository: %+v", m)
	}
	childHead("E", repo.path)
	if res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Tips = nil }); err != nil {
		t.Fatal(err)
	} else if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberUnmeasured || m.Reason != ReasonCheckoutMismatch {
		t.Errorf("the sweep's own checkout: %+v", m)
	}

	// an accepted result that is no longer the current one (a newer report of its generation) gives way to the child's checkout
	d := worktree("d", w.base)
	dHead := d.commit("d2.txt", "after the acceptance\n")
	childHead("D", d.path)
	k.supersedeReport("rel-g-D", "D", "g", "again")
	if res, err = w.sweep(TriggerManual, "", "", func(in *SweepInput) { in.Tips = nil; in.Heads = map[string]string{"E": w.head["E"]} }); err != nil {
		t.Fatal(err)
	} else if m := memberOf(res, MemberPair, "D", "E"); m.LeftHead != dHead || m.LeftSource != HeadChildCheckout {
		t.Errorf("a superseded acceptance: %+v", m)
	}
}

// What a sweep refuses, with the relay's existing reasons, and writes nothing.
func TestSweepRefusals(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	w.accept("D", "E")
	for name, c := range map[string]struct {
		mutate func(*SweepInput)
		actor  string
		reason string
	}{
		"another task":                     {func(in *SweepInput) {}, "intruder", "scope_role_mismatch"},
		"a trigger that is not one":        {func(in *SweepInput) { in.Trigger = "wake" }, "parent", "malformed_receipt"},
		"a relative checkout":              {func(in *SweepInput) { in.Repository = "checkout" }, "parent", "malformed_receipt"},
		"a checkout git cannot read":       {func(in *SweepInput) { in.Repository = filepath.Join(t.TempDir(), "nothing") }, "parent", "merge_target_unreadable"},
		"a trigger node the plan lacks":    {func(in *SweepInput) { in.TriggerNode = "Z" }, "parent", "unregistered_scope"},
		"a head named for a node it lacks": {func(in *SweepInput) { in.Heads = map[string]string{"Z": w.head["D"]} }, "parent", "unregistered_scope"},
		"a head named for a non_pr node":   {func(in *SweepInput) { in.Heads = map[string]string{"K": w.head["D"]} }, "parent", "disposition_conflict"},
		"a head that is not a commit id":   {func(in *SweepInput) { in.Heads = map[string]string{"D": "main"} }, "parent", "malformed_receipt"},
	} {
		t.Run(name, func(t *testing.T) {
			in := SweepInput{Repository: k.repo.path, Trigger: TriggerManual, Tips: []SweepTip{{Repository: k.repo.path, Ref: "dev", SHA: w.tip}}}
			c.mutate(&in)
			if _, err := k.sched.ObserveLive(context.Background(), "g", c.actor, in); refusalReason(err) != c.reason {
				t.Fatalf("sweep = %v, want %s", err, c.reason)
			}
			if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweeps"); got != 0 {
				t.Fatal("a refused sweep wrote a row")
			}
		})
	}
	// with no checkout to find, a hook's sweep is skipped and says so
	res, err := k.sched.ObserveLive(context.Background(), "g", "parent", SweepInput{Trigger: TriggerLanding, TriggerNode: "D", TriggerRef: "dio-9"})
	if err != nil || res.State != SweepSkipped || res.Reason != "no_checkout" || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps") != 0 {
		t.Fatalf("no checkout: %v %+v", err, res)
	}
}
