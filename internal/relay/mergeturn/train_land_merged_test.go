package mergeturn

// CRW-897's completion-node red-first cases: land admits a member pull request the forge reads as
// merged after the bundle merge, the ci.yml job reader counts a job whose value sits on the same line
// and refuses a jobs block it cannot read key by key, and verify compares the head's job set before
// the run's jobs. Every case uses the package's temporary store, forge stand-in and git stand-ins,
// never the live relay.
//
// New top-level identifiers here carry the trainLandMerged prefix so a sibling's new test file in the
// same package cannot collide.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// trainLandMergedTrue is the forge's merged fact for a member the bundle already merged.
func trainLandMergedTrue() *bool { v := true; return &v }

// trainLandMergedFalse is the forge's merged fact for a pull request closed without a merge.
func trainLandMergedFalse() *bool { v := false; return &v }

// trainLandMergedVerified lays out a verified three-member train whose bundle merge M is on dev, the
// shape land expects: the tip is M, M's parents are the base and the verified head, and M carries the
// tree CI passed on.
func trainLandMergedVerified(t *testing.T, w *tr) string {
	t.Helper()
	train := w.verifiedTrain()
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	return train
}

// TestTrainLandMergedMemberLands: a member pull request GitHub closed with merged true after the
// bundle merge is admitted, the landing is recorded, and every member turn closes landed
// (CRW-897, answer 1).
func TestTrainLandMergedMemberLands(t *testing.T) {
	w := newTr(t)
	train := trainLandMergedVerified(t, w)
	// GitHub closed the member pull request with a merge when the bundle merged: head and base are
	// the same ones the bundle carries
	w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", Merged: trainLandMergedTrue(), BaseRef: trBase, HeadSHA: "head-m2"}

	answer, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err != nil {
		t.Fatalf("land with a merged member: %v", err)
	}
	if answer["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", answer["state"])
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 3 {
		t.Fatalf("landed turns = %d, want 3", n)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 1 {
		t.Fatalf("landed events = %d, want 1", n)
	}
	// the landed event maps all three members: the merged member is not excluded
	detail := w.eventDetail(train, "landed")
	members, _ := detail["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("the landed mapping holds %d members, want 3", len(members))
	}
	if excluded, _ := detail["excluded"].([]any); len(excluded) != 0 {
		t.Fatalf("the landed event excluded %v, want none", excluded)
	}
}

// TestTrainLandRefusesAClosedMemberWithoutAMerge: a member the forge read as closed without a merge,
// and one whose closed answer carries no merged field at all, are both refused disposition_conflict
// with nothing written — admitting every closed pull request would let a cancelled member land
// (CRW-897, answer 1).
func TestTrainLandRefusesAClosedMemberWithoutAMerge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		merged *bool
	}{
		{"a closed member the forge read as not merged", trainLandMergedFalse()},
		{"a closed member whose answer carries no merged field", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTr(t)
			train := trainLandMergedVerified(t, w)
			w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", Merged: tc.merged, BaseRef: trBase, HeadSHA: "head-m2"}
			_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
			if err == nil || trReason(err) != "disposition_conflict" {
				t.Fatalf("a closed member without a merge: %v", err)
			}
			if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
				t.Fatalf("a refused land wrote %d landed event(s)", n)
			}
			if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
				t.Fatalf("a refused land landed %d turn(s)", n)
			}
		})
	}
}

// TestTrainLandRefusesAMergedMemberThatMoved: the merged fact does not relax the head check — a
// member whose pull request shows another head is refused even when it reads merged
// (CRW-897, answer 1).
func TestTrainLandRefusesAMergedMemberThatMoved(t *testing.T) {
	w := newTr(t)
	train := trainLandMergedVerified(t, w)
	w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", Merged: trainLandMergedTrue(), BaseRef: trBase, HeadSHA: "head-m2-moved"}
	_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a merged member that moved: %v", err)
	}
	if detail := err.Error(); !strings.Contains(detail, "102") || !strings.Contains(detail, "head-m2-moved") {
		t.Fatalf("the refusal does not name the pull request and both heads: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("a refused land landed %d turn(s)", n)
	}
}

// TestTrainLandExcludesTheLeaderWhoseTurnWasReturned: the issue's own recovery path — a member that
// moves between verify and land has its turn taken out of the lane, and the leader is the member whose
// turn is the holding lane turn, so it is returned rather than withdrawn. Land must then record the
// rest and name the returned leader in the landed event, not refuse the bundle (CRW-897, answer 1).
func TestTrainLandExcludesTheLeaderWhoseTurnWasReturned(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 1", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 1: %v", err)
	}
	leaderTurn := rows[0].Get("turn_id").(string)
	// the leader's own parent returns the holding lane turn, which is what a moved leader needs
	answer, err := w.m.Release(w.ctx, leaderTurn, trLeader, "returned", "the leader moved after verify", "")
	if err != nil {
		t.Fatalf("returning the leader's turn: %v", err)
	}
	released, _ := answer["released"].(map[string]any)
	if released == nil || released["state"] != "returned" {
		t.Fatalf("release answer = %v, want the leader's turn released as returned", answer)
	}
	if state := w.turn(leaderTurn).State; state != "returned" {
		t.Fatalf("the leader's turn = %s, want returned", state)
	}
	// its pull request also moved, which must not refuse the bundle: the turn is out of the lane
	w.forge.pulls[101] = TrainPullRequest{Number: 101, State: "closed", Merged: trainLandMergedTrue(), BaseRef: "main", HeadSHA: "head-lead-moved"}
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}

	landed, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err != nil {
		t.Fatalf("land with a returned leader: %v", err)
	}
	if landed["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", landed["state"])
	}
	// the two waiting members landed; the returned leader did not
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 2 {
		t.Fatalf("landed turns = %d, want 2", n)
	}
	if state := w.turn(leaderTurn).State; state != "returned" {
		t.Fatalf("the excluded leader's turn = %s, want returned", state)
	}
	// the landed event names the returned leader, and the mapping holds the two survivors
	detail := w.eventDetail(train, "landed")
	excluded, _ := detail["excluded"].([]any)
	if len(excluded) != 1 {
		t.Fatalf("the landed event's excluded list = %v, want the leader", detail["excluded"])
	}
	entry, _ := excluded[0].(map[string]any)
	if entry["turnId"] != leaderTurn || entry["prNumber"] != float64(101) || entry["state"] != "returned" {
		t.Fatalf("the excluded entry = %v", entry)
	}
	if reason, _ := entry["closeReason"].(string); !strings.Contains(reason, "moved after verify") {
		t.Fatalf("the excluded entry names no reason: %v", entry)
	}
	members, _ := detail["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("the landed mapping holds %d members, want 2", len(members))
	}
}

// TestTrainJobReaderRefusesAnUnreadableMatrix: a go-product matrix that carries an include or exclude
// list recombines or drops legs, so the part list is no longer the leg set a run reports; the reader
// refuses it rather than reporting an unchanged job set (CRW-897, answer 2).
func TestTrainJobReaderRefusesAnUnreadableMatrix(t *testing.T) {
	base := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix:\n        part: [lint, test-1]\n"
	// the part-only matrix still reads
	if _, err := TrainJobsFromWorkflow(base); err != nil {
		t.Fatalf("a part-only matrix: %v", err)
	}
	for _, tc := range []struct {
		name string
		line string
	}{
		{"an include list", "        include:\n          - part: audit\n"},
		{"an exclude list", "        exclude:\n          - part: lint\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workflow := base + strings.ReplaceAll(tc.line, "\n", "\n")
			if _, err := TrainJobsFromWorkflow(workflow); err == nil {
				t.Fatal("a matrix with include/exclude was read as the part list alone")
			}
		})
	}
	// through verify it is merge_target_unreadable, never a pass
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	withInclude := strings.Replace(string(repository), "\n        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]\n",
		"\n        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]\n        include:\n          - part: audit\n", 1)
	if withInclude == string(repository) {
		t.Fatal("the fixture did not add an include list")
	}
	w.proof.workflow = withInclude
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("a matrix with an include list at verify: %v", err)
	}
}

// TestTrainJobBodyIgnoresAKeyOutsideTheJobsBlock: a two-space key elsewhere in the workflow — an env
// entry, say — is not a job header, so the go-product body is still read from the jobs block and a
// normal head is not refused merge_target_unreadable (CRW-897, answer 2; pre-merge evaluation d2).
func TestTrainJobBodyIgnoresAKeyOutsideTheJobsBlock(t *testing.T) {
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// an env entry named like the matrix job, before the jobs block
	withEnv := strings.Replace(string(repository), "jobs:", "env:\n  go-product: ci\njobs:", 1)
	if withEnv == string(repository) {
		t.Fatal("the fixture did not add an env entry")
	}
	jobs, err := TrainJobsFromWorkflow(withEnv)
	if err != nil {
		t.Fatalf("a workflow with an env go-product key: %v", err)
	}
	if strings.Join(jobs, ",") != strings.Join(TrainExpectedJobs, ",") {
		t.Fatalf("jobs with an env key = %v, want the workflow's own jobs", jobs)
	}
	// through verify the head is accepted, not refused: the env key must not be taken for the job
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.workflow = withEnv
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
		t.Fatalf("a head whose workflow has an env go-product key: %v", err)
	}
}

// TestTrainJobReaderRefusesAnInlineMatrixInclude: an include list written inline on one line adds a
// leg just as the block form does, so the reader must refuse it rather than read the part list alone
// (CRW-897, answer 2; pre-merge evaluation d1).
func TestTrainJobReaderRefusesAnInlineMatrixInclude(t *testing.T) {
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	withInline := strings.Replace(string(repository),
		"        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]",
		"        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]\n        include: [{part: audit}]", 1)
	if withInline == string(repository) {
		t.Fatal("the fixture did not add an inline include")
	}
	if _, err := TrainJobsFromWorkflow(withInline); err == nil {
		t.Fatal("an inline include list was read as the part list alone")
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.workflow = withInline
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an inline include list at verify: %v", err)
	}
}

// TestTrainLandDoesNotRepromoteAnOccupiedTarget: when a member that left the lane is returned, its
// parent's return promotes the next waiter, so the target already has a holder by the time land runs.
// Land must record the rest without promoting again — a second promotion would break the
// one-live-holder constraint and roll the whole landing back (CRW-897, answer 1; evaluation d3).
func TestTrainLandDoesNotRepromoteAnOccupiedTarget(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 1", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 1: %v", err)
	}
	leaderTurn := rows[0].Get("turn_id").(string)
	// a ready turn that is not in the bundle waits behind the leader; returning the holding leader
	// promotes it before land runs, so the target already has a holder
	w.waiting("PRJ-M4", "task-m4", "head-m4", 104)
	if _, err := w.m.Release(w.ctx, leaderTurn, trLeader, "returned", "the leader moved after verify", ""); err != nil {
		t.Fatalf("returning the leader's turn: %v", err)
	}
	// the release promoted the next ready waiter, which is what leaves the target occupied
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'holding'"); n != 1 {
		t.Fatalf("holding turns after the release = %d, want the promoted waiter", n)
	}
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}

	answer, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err != nil {
		t.Fatalf("land with an occupied target: %v", err)
	}
	if answer["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", answer["state"])
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 2 {
		t.Fatalf("landed turns = %d, want 2", n)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 1 {
		t.Fatalf("landed events = %d, want 1", n)
	}
	// the waiting turn that was promoted still holds the target; land did not demote it
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'holding'"); n != 1 {
		t.Fatalf("holding turns = %d, want exactly the promoted waiter", n)
	}
}

// TestTrainMatrixGuardScopesToTheMatrixKeys: the guard must judge the matrix's own keys, not an
// "include" that appears in a step input, an env value or a run script elsewhere in the job. Those
// are not matrix keys, and a normal head must still be verified (CRW-897, answer 2; evaluation d2).
func TestTrainMatrixGuardScopesToTheMatrixKeys(t *testing.T) {
	// an "include:" inside the job but outside its matrix block is not a matrix key
	job := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    env:\n      include: ci\n    strategy:\n      matrix:\n        part: [lint, test-1]\n"
	jobs, err := TrainJobsFromWorkflow(job)
	if err != nil {
		t.Fatalf("an include key outside the matrix: %v", err)
	}
	if strings.Join(jobs, ",") != "validate,go-product (lint),go-product (test-1)" {
		t.Fatalf("jobs = %v, want the part legs", jobs)
	}
	// an "include:" in a step input, after the matrix, is not a matrix key either
	after := job + "    steps:\n      - uses: actions/setup-go\n        with:\n          include: all\n"
	if _, err := TrainJobsFromWorkflow(after); err != nil {
		t.Fatalf("an include key in a step input: %v", err)
	}
	// and the real workflow still reads
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	real, err := TrainJobsFromWorkflow(string(repository))
	if err != nil {
		t.Fatalf("the repository's own ci.yml: %v", err)
	}
	if strings.Join(real, ",") != strings.Join(TrainExpectedJobs, ",") {
		t.Fatalf("the repository's jobs = %v, want TrainExpectedJobs", real)
	}
}

// TestTrainMatrixGuardRefusesANonPartKey: a matrix key that is not part adds or recombines legs the
// part list does not name, so the reader refuses it rather than reporting the part list alone
// (CRW-897, answer 2; evaluation d1).
func TestTrainMatrixGuardRefusesANonPartKey(t *testing.T) {
	base := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix:\n        part: [lint, test-1]\n"
	for _, tc := range []struct {
		name  string
		added string
	}{
		{"an extra axis", "        os: [ubuntu, macos]\n"},
		{"a block include", "        include:\n          - part: audit\n"},
		{"an inline include", "        include: [{part: audit}]\n"},
		{"a block exclude", "        exclude:\n          - part: lint\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TrainJobsFromWorkflow(base + tc.added); err == nil {
				t.Fatal("a matrix key that is not part was read as the part list alone")
			}
		})
	}
	// the matrix header may carry a comment or an anchor; the keys under it are still judged
	commented := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix: # CI legs\n        part: [lint, test-1]\n        include: [{part: audit}]\n"
	if _, err := TrainJobsFromWorkflow(commented); err == nil {
		t.Fatal("a commented matrix header hid the include list")
	}
	anchored := "\njobs:\n  validate:\n    runs-on: ubuntu\n  go-product:\n    strategy:\n      matrix: &legs\n        part: [lint, test-1]\n        include: [{part: audit}]\n"
	if _, err := TrainJobsFromWorkflow(anchored); err == nil {
		t.Fatal("an anchored matrix header hid the include list")
	}
	// and through verify a head whose commented matrix added a leg is refused, not verified
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Replace(string(repository), "      matrix:\n", "      matrix: # CI legs\n", 1)
	head = strings.Replace(head, "        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]\n",
		"        part: [lint, test-1, test-2, test-3, test-4, test-rest, dist]\n        include: [{part: audit}]\n", 1)
	if head == string(repository) {
		t.Fatal("the fixture did not comment the matrix header")
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.workflow = head
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("a commented matrix header with an include leg at verify: %v", err)
	}
}

// TestTrainHalveIsAPrefixOfTheOrderedGroups: the first half is the ordered groups taken whole until
// the next would exceed half, so a smaller group after the overflow stays in the second half
// (CRW-897, answer 3; pre-merge evaluation d2). The review's case: groups of 2, 3, 1, 1, 1 with half
// 4 give first = the first group only, and the rest — the 1-sized groups included — go second.
func TestTrainHalveIsAPrefixOfTheOrderedGroups(t *testing.T) {
	nodes := []TrainMemberNode{
		{PRNumber: 101, NodeID: "A"}, {PRNumber: 102, NodeID: "B"}, {PRNumber: 103, NodeID: "C"},
		{PRNumber: 104, NodeID: "D"}, {PRNumber: 105, NodeID: "E"}, {PRNumber: 106, NodeID: "F"},
		{PRNumber: 107, NodeID: "G"}, {PRNumber: 108, NodeID: "H"},
	}
	order := []int64{101, 102, 103, 104, 105, 106, 107, 108}
	// groups by place: {A,B} {C,D,E} {F} {G} {H}; half = 4, so the first half takes {A,B} and stops
	edges := []TrainPlanEdge{{FromNodeID: "A", ToNodeID: "B"}, {FromNodeID: "C", ToNodeID: "D"}, {FromNodeID: "D", ToNodeID: "E"}}
	first, second := TrainHalve(order, nodes, edges)
	if strings.Join(intsToStrs(first), ",") != "101,102" {
		t.Fatalf("first = %v, want the first whole group only", first)
	}
	if strings.Join(intsToStrs(second), ",") != "103,104,105,106,107,108" {
		t.Fatalf("second = %v, want everything from the first overflow on", second)
	}
}

// intsToStrs renders a pull request list for comparison.
func intsToStrs(list []int64) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, strconv.FormatInt(v, 10))
	}
	return out
}

// TestTrainVerifyRefusesAMergedMember: verify keeps taking open pull requests only, so a member the
// forge already marked merged is refused there with nothing written (CRW-897, answer 1).
func TestTrainVerifyRefusesAMergedMember(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", Merged: trainLandMergedTrue(), BaseRef: trBase, HeadSHA: "head-m2"}
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a merged member at verify: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}

// TestTrainOpenRefusesAMergedMember: open keeps taking open pull requests only (CRW-897, answer 1).
func TestTrainOpenRefusesAMergedMember(t *testing.T) {
	w := newTr(t)
	leader, members := w.threeMembers()
	w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", Merged: trainLandMergedTrue(), BaseRef: trBase, HeadSHA: "head-m2"}
	if _, err := w.open(leader, trLeader, "base-0", members...); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a merged member at open: %v", err)
	}
	if n := w.trainEvents(); n != 0 {
		t.Fatalf("a refused open wrote %d event(s)", n)
	}
}

// TestTrainJobReaderCountsASameLineJob: a two-space-indented key whose value sits on the same line (a
// YAML flow mapping) is a job, and a jobs block this reader cannot read key by key is an error rather
// than an empty pass (CRW-897, answer 2).
func TestTrainJobReaderCountsASameLineJob(t *testing.T) {
	// the flow-mapping job is counted
	flow := "\njobs:\n  validate:\n    runs-on: ubuntu\n  audit: {runs-on: ubuntu, steps: []}\n  go-product:\n    strategy:\n      matrix:\n        part: [lint]\n"
	jobs, err := TrainJobsFromWorkflow(flow)
	if err != nil {
		t.Fatalf("a workflow with a flow-mapping job: %v", err)
	}
	want := []string{"validate", "audit", "go-product (lint)"}
	if strings.Join(jobs, ",") != strings.Join(want, ",") {
		t.Fatalf("jobs = %v, want %v", jobs, want)
	}

}

// TestTrainJobReaderRefusesAnUnreadableJobsBlock: a jobs block this reader cannot read key by key —
// a flow mapping on the jobs: line, or a jobs key with no key under it — is an error, never an empty
// pass, and through verify it is merge_target_unreadable (CRW-897, answer 2).
func TestTrainJobReaderRefusesAnUnreadableJobsBlock(t *testing.T) {
	// a jobs block that is itself a flow mapping
	if _, err := TrainJobsFromWorkflow("\njobs: {validate: {runs-on: ubuntu}}\n"); err == nil {
		t.Fatal("a flow-mapping jobs block was read")
	}
	// a jobs key with no key under it
	if _, err := TrainJobsFromWorkflow("\njobs:\n"); err == nil {
		t.Fatal("a jobs block with no job key was read as an empty pass")
	}
	// through verify both are merge_target_unreadable
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.workflow = "\njobs: {validate: {runs-on: ubuntu}}\n"
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("a flow-mapping jobs block at verify: %v", err)
	}
	w.proof.workflow = "\njobs:\n"
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an empty jobs block at verify: %v", err)
	}
}

// TestTrainJobReaderCountsAQuotedKeyWithAColon: a quoted job key that contains a colon is read whole,
// so a head cannot add it without verify naming it (CRW-897, answer 2).
func TestTrainJobReaderCountsAQuotedKeyWithAColon(t *testing.T) {
	workflow := "\njobs:\n  validate:\n    runs-on: ubuntu\n  \"audit: extra\":\n    runs-on: ubuntu\n"
	jobs, err := TrainJobsFromWorkflow(workflow)
	if err != nil {
		t.Fatalf("a quoted key with a colon: %v", err)
	}
	if strings.Join(jobs, ",") != "validate,audit: extra" {
		t.Fatalf("jobs = %v, want validate and the quoted key", jobs)
	}
	// a quoted key that never closes cannot be read, and a line this reader cannot read key by key
	// is an error rather than a silently skipped job
	unterminated := "\njobs:\n  validate:\n    runs-on: ubuntu\n  \"audit:\n    runs-on: ubuntu\n"
	if _, err := TrainJobsFromWorkflow(unterminated); err == nil {
		t.Fatal("an unterminated quoted key was read as a pass")
	}
}

// TestTrainJobReaderCountsAnchoredAndAliasedJobs: a job whose value is a whole-job YAML anchor
// ("audit: &base_job") or an alias ("audit-copy: *base_job") is a job too; GitHub Actions supports
// those forms, and a reader that judged the value would let an added job through unnoticed
// (CRW-897, answer 2).
func TestTrainJobReaderCountsAnchoredAndAliasedJobs(t *testing.T) {
	workflow := "\njobs:\n  base: &base_job\n    runs-on: ubuntu\n  audit: &audit_job\n    runs-on: ubuntu\n  audit-copy: *audit_job\n"
	jobs, err := TrainJobsFromWorkflow(workflow)
	if err != nil {
		t.Fatalf("a workflow with anchored and aliased jobs: %v", err)
	}
	if strings.Join(jobs, ",") != "base,audit,audit-copy" {
		t.Fatalf("jobs = %v, want the anchored and aliased jobs named", jobs)
	}
	// and the refusal names an aliased job a head added, rather than passing it
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.workflow = strings.Replace(string(repository), "\n  gui:\n", "\n  gui: &gui_job\n  gui-copy: *gui_job\n", 1)
	if w.proof.workflow == string(repository) {
		t.Fatal("the fixture did not add an aliased job")
	}
	_, err = w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("an added aliased job: %v", err)
	}
	if !strings.Contains(err.Error(), "gui-copy") {
		t.Fatalf("the refusal does not name the added aliased job: %v", err)
	}
}

// TestTrainJobBodyEndsAtTheNextJob: the go-product block still ends at the next two-space job key, so
// the matrix read keeps stopping where it should (CRW-897, answer 2).
func TestTrainJobBodyEndsAtTheNextJob(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	body, found := trainJobBody(string(data), trainProductJob)
	if !found {
		t.Fatal("the go-product body was not found in the repository's ci.yml")
	}
	if strings.Contains(body, "  dev-gate:") {
		t.Fatalf("the go-product body ran past the next job header:\n%s", body)
	}
	parts, err := trainWorkflowMatrixParts(string(data))
	if err != nil {
		t.Fatalf("the matrix of the repository's ci.yml: %v", err)
	}
	if len(parts) != 7 {
		t.Fatalf("matrix parts = %v, want 7", parts)
	}
}

// TestTrainVerifyNamesTheRenamedJobBeforeTheRun: a head that renamed a ci.yml job is refused naming
// the missing and added names even when the run carries the new name, because verify compares the
// head's job set before it checks the run's jobs (CRW-897, answer 3).
func TestTrainVerifyNamesTheRenamedJobBeforeTheRun(t *testing.T) {
	repository, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	// the run carries the renamed job, not the one this runtime verifies: the old order refused this
	// as "the run holds no job named gui" and never reached the ci.yml comparison
	run := runFor("head-bundle")
	for i := range run.Jobs {
		if run.Jobs[i].Name == "gui" {
			run.Jobs[i].Name = "gui-renamed"
		}
	}
	w.forge.runs["run-1"] = run
	w.proof.workflow = strings.Replace(string(repository), "\n  gui:\n", "\n  gui-renamed:\n", 1)
	if w.proof.workflow == string(repository) {
		t.Fatal("the fixture did not rename a job")
	}
	_, err = w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a renamed job with a run carrying the new name: %v", err)
	}
	if detail := err.Error(); !strings.Contains(detail, "gui") || !strings.Contains(detail, "gui-renamed") {
		t.Fatalf("the refusal does not name both the missing and the added job: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}
