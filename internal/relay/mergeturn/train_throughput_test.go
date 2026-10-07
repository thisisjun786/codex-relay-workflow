package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// trainRecordDerivedVersion resolves the manifest conflict by recording the version the plugin
// payload derives, the way the lane re-records it when two members both touched the manifest.
func trainRecordDerivedVersion(t *testing.T, repo string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(pluginversion.ManifestRepoPath))
	// a clean manifest first: the merge left conflict markers, which the payload reader refuses
	clean := "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0\",\n  \"description\": \"base\"\n}\n"
	if err := os.WriteFile(path, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, errs := pluginversion.DirectoryPayload(filepath.Join(repo, filepath.FromSlash(pluginversion.PluginRelative)))
	if len(errs) > 0 {
		t.Fatalf("the plugin payload: %v", errs)
	}
	recorded, err := pluginversion.ManifestVersion(payload)
	if err != nil {
		t.Fatal(err)
	}
	next, err := pluginversion.PayloadVersion(payload, recorded)
	if err != nil {
		t.Fatal(err)
	}
	body := "{\n  \"name\": \"crw\",\n  \"version\": \"" + next + "\",\n  \"description\": \"base\"\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trainGitIn is the git runner the chain tests use: one temporary repository, no ambient config.
func trainGitIn(t *testing.T, repo string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
}

// trainGitTry runs git in the same environment as trainGitIn and returns its error, for a command
// the fixture expects to fail (a merge that stops on a conflict).
func trainGitTry(repo string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd.Run()
}

// CRW-898: bundle throughput inside the existing proofs. These tests use a temporary store, a forge
// stand-in and a temporary git repository, never the live relay service, the production store or a
// real Codex task.

// turnIDOf is the live turn of one pull request on the fixture's target, read from the store.
func (w *tr) turnIDOf(pr int64) string {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_turns WHERE pr_number = ?", pr)
	if err != nil || len(rows) != 1 {
		w.t.Fatalf("the turn of pull request %d: %v %d", pr, err, len(rows))
	}
	return rows[0].Get("turn_id").(string)
}

// TestBundleCarriesAMemberRefreshedWithoutRestatingItsTurn is criterion c1 item 4: a waiting member
// whose acceptance stands on a recorded base-refresh head keeps its eligibility even though its turn
// still holds the head the acceptance was originally taken on. The bundle carries the stand head (the
// head the member's pull request shows), so open, the in-transaction guard and land must all read the
// stand rather than the turn's own candidate head. A member whose head moved with no record still
// drops, which the sibling subtest pins.
func TestBundleCarriesAMemberRefreshedWithoutRestatingItsTurn(t *testing.T) {
	t.Run("a recorded refresh keeps the member eligible without restating its turn", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
		// the parent refreshed the member's branch and recorded it: the stand head is the refreshed
		// head, the member's pull request shows it, and the waiting turn is left where it was
		w.refreshStand("acc-rel-task-m2", "rel-task-m2", "head-m2-refreshed")
		w.pr(102, "head-m2-refreshed")
		if head := w.turn(w.turnIDOf(102)).CandidateHead; head != "head-m2" {
			t.Fatalf("the fixture restated the turn to %s, and this case is about a turn that did not move", head)
		}
		answer, err := w.open(leader, trLeader, "base-0", 101, 102)
		if err != nil {
			t.Fatalf("a member whose acceptance stands on a recorded refresh head: %v", err)
		}
		train := answer["train"].(map[string]any)["trainId"].(string)
		rows, err := w.s.All(w.ctx, "SELECT detail_json FROM merge_train_events WHERE train_id = ? AND kind = 'opened'", train)
		if err != nil || len(rows) != 1 {
			t.Fatalf("the opened event: %v %d", err, len(rows))
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(rows[0].Get("detail_json").(string)), &detail); err != nil {
			t.Fatal(err)
		}
		memberList, _ := detail["members"].([]any)
		second, _ := memberList[1].(map[string]any)
		if second["acceptedHead"] != "head-m2-refreshed" {
			t.Fatalf("the opened mapping records acceptedHead %v, want the refresh head", second["acceptedHead"])
		}
		// verify and land reread the same stand: the bundle's head is the refreshed head, so the member
		// turn lands although it still holds the accepted head
		w.pr(900, "head-bundle", TrainLaneLabel)
		w.forge.runs["run-1"] = runFor("head-bundle")
		if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
			t.Fatalf("verify over a member refreshed without restating its turn: %v", err)
		}
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "merge-1", w.tip, w.forge); err != nil {
			t.Fatalf("land over a member refreshed without restating its turn: %v", err)
		}
		if state := w.turn(w.turnIDOf(102)).State; state != "landed" {
			t.Fatalf("the member turn is %s after the landing, want landed", state)
		}
	})

	t.Run("a member whose head moved with no record still drops", func(t *testing.T) {
		w := newTr(t)
		leader, members := w.threeMembers()
		// the member's pull request moved to a head no record names, and the turn did not
		w.pr(102, "head-m2-moved")
		if _, err := w.open(leader, trLeader, "base-0", members...); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member whose head moved with no record: %v", err)
		}
		if n := w.trainEvents(); n != 0 {
			t.Fatalf("a refused open wrote %d event(s)", n)
		}
	})
}

// requestOn asks for a turn for one pull request of a parent, the way dag-merge-request does.
func (w *tr) requestOn(project, task, relationship, head string, pr int64) (map[string]any, error) {
	return w.m.Request(w.ctx, trRepo, trBase, project, task, trHost, head, true,
		ClaimOptions{PR: sql.NullInt64{Int64: pr, Valid: true}, Relationship: sql.NullString{String: relationship, Valid: true}})
}

// TestChainVersionLineStep: a chain step whose tree differs from git's own merge only in the plugin
// manifest's version line is proved by the built-in regenerate:plugin-version rule and recorded with
// the train; a difference on any other path, or a conflict on one, still fails the chain.
func TestChainVersionLineStep(t *testing.T) {
	repo := t.TempDir()
	git := trainGitIn(t, repo)
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := func(version, description string) string {
		return "{\n  \"name\": \"crw\",\n  \"version\": \"" + version + "\",\n  \"description\": \"" + description + "\"\n}\n"
	}
	git("init", "-q", "-b", "dev")
	write("a.txt", "base\n")
	write("plugins/crw/.codex-plugin/plugin.json", manifest("0.4.0", "base"))
	git("add", "-A")
	git("commit", "-q", "-m", "D")
	base := git("rev-parse", "HEAD")

	// member one: its own file, merged with --no-ff
	git("checkout", "-q", "-b", "m1")
	write("m1.txt", "one\n")
	git("add", "-A")
	git("commit", "-q", "-m", "member one")
	m1 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m1", "m1")

	// member two and member three both branch from the same point and both re-record the manifest,
	// so git cannot merge the version line: the chain step resolves by recording the version the
	// step's own payload derives, which is what the lane does. The merge's tree is then not
	// what git writes from its parents, and the only difference is the manifest.
	proof := TrainCheckoutProver{}
	git("checkout", "-q", "-b", "m2")
	write("m2.txt", "two\n")
	write("plugins/crw/.codex-plugin/plugin.json", manifest("0.4.0+222222222222", "base"))
	git("add", "-A")
	git("commit", "-q", "-m", "member two")
	m2 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("checkout", "-q", "-b", "m3")
	write("m3.txt", "three\n")
	write("plugins/crw/.codex-plugin/plugin.json", manifest("0.4.0+333333333333", "base"))
	git("add", "-A")
	git("commit", "-q", "-m", "member three")
	m3 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m2", "m2")
	if err := trainGitTry(repo, "merge", "-q", "--no-ff", "-m", "merge m3", "m3"); err == nil {
		t.Fatalf("the fixture's merge was meant to conflict on the manifest")
	}
	// the step records the version its own merged payload derives, which is what the lane does
	trainRecordDerivedVersion(t, repo)
	git("add", "-A")
	git("commit", "-q", "-m", "Merge branch 'dev' into m3")
	merged := git("rev-parse", "HEAD")
	chain3, err := proof.Chain(context.Background(), repo, merged, base, []TrainMemberExpectation{{AcceptedHead: m1}, {AcceptedHead: m2}, {AcceptedHead: m3}})
	if err != nil {
		t.Fatalf("a version-line step the payload derives was refused: %v", err)
	}
	if len(chain3.Steps) != 1 || chain3.Steps[0].Commit != merged || chain3.Steps[0].Rule != TrainVersionLineRule || chain3.Steps[0].Path != pluginversion.ManifestRepoPath {
		t.Fatalf("the version-line proof was not recorded with the chain: %+v", chain3.Steps)
	}
	// a version the step's own payload does not derive is still a hand resolution: the built-in rule
	// proves only the line the payload derives, so the chain refuses it and records no step
	write("plugins/crw/.codex-plugin/plugin.json", manifest("0.4.0+999999999999", "base"))
	git("add", "-A")
	git("commit", "-q", "--amend", "--no-edit")
	forged := git("rev-parse", "HEAD")
	if _, err := proof.Chain(context.Background(), repo, forged, base, []TrainMemberExpectation{{AcceptedHead: m1}, {AcceptedHead: m2}, {AcceptedHead: m3}}); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a version line the payload does not derive = %v, want a refusal", err)
	}
}

// TestMemberOnlyWaitingTurn: a parent"s further ready turn for one target becomes a
// member-only waiting turn instead of the CRW-538 refusal, it never takes the solo grant while an
// older turn of the same parent stands on the target, and the bundle carries it.
func TestMemberOnlyWaitingTurn(t *testing.T) {
	t.Run("a second accepted candidate becomes a member-only waiting turn", func(t *testing.T) {
		w := newTr(t)
		first := w.claim(trLane, trLeader, "head-101", 101)["turnId"].(string)
		w.pr(101, "head-101")
		w.acceptOnFor("rel-"+trLeader+"-2", "head-102", 102)
		w.pr(102, "head-102")
		second, err := w.requestOn(trLane, trLeader, "rel-"+trLeader+"-2", "head-102", 102)
		if err != nil {
			t.Fatalf("the parent's further ready turn was refused: %v", err)
		}
		if second["state"] != MemberWaiting || second["turnId"] == first {
			t.Fatalf("the second turn = %v, want its own member-only waiting turn", second)
		}
		if second["prNumber"] != int64(102) || second["relationshipId"] != "rel-"+trLeader+"-2" {
			t.Fatalf("the second turn carries another identity: %v", second)
		}
		if n := w.count("SELECT count(*) FROM merge_turns WHERE target_key = (SELECT target_key FROM merge_turns WHERE turn_id = ?)", first); n != 2 {
			t.Fatalf("%d live turns on the target", n)
		}
	})

	t.Run("a member-only turn never takes the solo grant, even when the target frees", func(t *testing.T) {
		w := newTr(t)
		first := w.claim(trLane, trLeader, "head-101", 101)["turnId"].(string)
		w.pr(101, "head-101")
		w.acceptOnFor("rel-"+trLeader+"-2", "head-102", 102)
		w.pr(102, "head-102")
		second, err := w.requestOn(trLane, trLeader, "rel-"+trLeader+"-2", "head-102", 102)
		if err != nil {
			t.Fatal(err)
		}
		// while the parent"s older turn holds the lane, the member-only one waits: it takes no
		// solo grant of its own
		if state := w.turn(second["turnId"].(string)).State; state != MemberWaiting {
			t.Fatalf("the member-only turn is %s while an older turn of its parent holds the lane", state)
		}
		if _, err := w.m.Release(w.ctx, first, trLeader, "returned", "the lane is free", ""); err != nil {
			t.Fatal(err)
		}
		// The lane is free and the parent now has no waiting turn, so promotion has nothing to take.
		// The member-only turn stays member-only: it rides bundles and never becomes the solo holder
		// (CRW-898 item 1), so a parent cannot reserve a later solo position with an extra candidate.
		if state := w.turn(second["turnId"].(string)).State; state != MemberWaiting {
			t.Fatalf("a member-only turn took the free lane as %s, and it must ride bundles only", state)
		}
	})

	t.Run("a request no active acceptance covers keeps the CRW-538 refusal", func(t *testing.T) {
		w := newTr(t)
		first := w.claim(trLane, trLeader, "head-101", 101)["turnId"].(string)
		w.pr(101, "head-101")
		w.pr(102, "head-102")
		_, err := w.requestOn(trLane, trLeader, "rel-"+trLeader+"-2", "head-102", 102)
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a request no acceptance covers: %v", err)
		}
		if !strings.Contains(err.Error(), first) {
			t.Fatalf("the refusal does not name the live turn: %v", err)
		}
	})

	t.Run("the bundle carries a member-only waiting turn", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
		w.pr(102, "head-m2")
		w.acceptOnFor("rel-"+trLeader+"-2", "head-103", 103)
		w.pr(103, "head-103")
		if _, err := w.requestOn(trLane, trLeader, "rel-"+trLeader+"-2", "head-103", 103); err != nil {
			t.Fatal(err)
		}
		answer, err := w.open(leader, trLeader, "base-0", 101, 102, 103)
		if err != nil {
			t.Fatalf("a bundle over a member-only waiting turn: %v", err)
		}
		train := answer["train"].(map[string]any)["trainId"].(string)
		rows, err := w.s.All(w.ctx, "SELECT detail_json FROM merge_train_events WHERE train_id = ? AND kind = 'opened'", train)
		if err != nil || len(rows) != 1 {
			t.Fatalf("the opened event: %v %d", err, len(rows))
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(rows[0].Get("detail_json").(string)), &detail); err != nil {
			t.Fatal(err)
		}
		members, _ := detail["members"].([]any)
		if len(members) != 3 {
			t.Fatalf("the bundle carries %d members, want 3 (one parent twice)", len(members))
		}
		// the bundle is verified and landed with the member-only turn riding it: its own turn ledger
		// records the landing, which is what item 1 requires of every member (CRW-898)
		w.pr(900, "head-bundle", TrainLaneLabel)
		w.forge.runs["run-1"] = runFor("head-bundle")
		if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
			t.Fatalf("verify over a bundle carrying a member-only turn: %v", err)
		}
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "merge-1", w.tip, w.forge); err != nil {
			t.Fatalf("land over a bundle carrying a member-only turn: %v", err)
		}
		if state := w.turn(w.turnIDOf(103)).State; state != "landed" {
			t.Fatalf("the member-only turn is %s after the landing, want landed on its own turn", state)
		}
	})
}

// TestVerifiedEventRecordsTheVersionLineSteps is criterion c1 item 2 on the train record: a chain
// step the built-in version-line rule settled is recorded with the verified event, so a reader can
// tell a normal git merge from one the rule admitted. The prover here reports a step directly, which
// is the shape TrainChain carries; the real git shape is exercised by TestChainVersionLineStep.
func TestVerifiedEventRecordsTheVersionLineSteps(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.proof.steps = []TrainChainStep{{Commit: "merge-m2", Path: "plugins/crw/.codex-plugin/plugin.json", Rule: TrainVersionLineRule, Blob: "blob-m2"}}
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
		t.Fatal(err)
	}
	rows, err := w.s.All(w.ctx, "SELECT detail_json FROM merge_train_events WHERE train_id = ? AND kind = 'verified'", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the verified event: %v %d", err, len(rows))
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[0].Get("detail_json").(string)), &detail); err != nil {
		t.Fatal(err)
	}
	steps, ok := detail["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("the verified event records %v, want one step", detail["steps"])
	}
	step, _ := steps[0].(map[string]any)
	if step["commit"] != "merge-m2" || step["path"] != "plugins/crw/.codex-plugin/plugin.json" || step["rule"] != TrainVersionLineRule || step["blob"] != "blob-m2" {
		t.Fatalf("the recorded step = %v, want the commit, path, rule and blob the chain proved", step)
	}
}

// TestBundleDropsTheMemberABlockingFindingPointsAt is criterion c1 item 3 end to end on the relay's
// own train: a three-member bundle is opened and verified, a blocking finding is answered by dropping
// one member, the bundle is abandoned and reopened without it, and the survivors verify and land
// once more. The dropped member's turn is left where its own parent put it, and only the survivors'
// turns record the landing.
func TestBundleDropsTheMemberABlockingFindingPointsAt(t *testing.T) {
	w := newTr(t)
	leader, members := w.threeMembers()
	first, err := w.open(leader, trLeader, "base-0", members...)
	if err != nil {
		t.Fatal(err)
	}
	train := first["train"].(map[string]any)["trainId"].(string)
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
		t.Fatal(err)
	}
	// a blocking finding points at the second member: that member is dropped, so its parent returns
	// its turn, and the bundle is abandoned and reopened without it
	dropped := w.turnIDOf(102)
	if _, err := w.m.Release(w.ctx, dropped, "task-m2", "cancelled", "a blocking finding points at this member", "https://example.invalid/thread"); err == nil {
		// a waiting member is withdrawn, not released; either way it leaves the lane
		t.Fatalf("a waiting member turn was released, and a waiting turn is withdrawn")
	}
	if _, err := w.m.Withdraw(w.ctx, dropped, "task-m2"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.m.Close(w.ctx, train, trLeader, "abandoned", "a blocking finding points at one member"); err != nil {
		t.Fatal(err)
	}
	// the leader still holds its turn, so the survivors reopen under it; the dropped member is gone
	second, err := w.open(leader, trLeader, "base-0", 101, 103)
	if err != nil {
		t.Fatalf("reopening the bundle without the dropped member: %v", err)
	}
	reopened := second["train"].(map[string]any)["trainId"].(string)
	if n := w.count("SELECT count(*) FROM merge_train_members WHERE train_id = ?", reopened); n != 2 {
		t.Fatalf("the reopened bundle carries %d members, want the two survivors", n)
	}
	w.pr(900, "head-bundle-2", TrainLaneLabel)
	w.forge.runs["run-2"] = runFor("head-bundle-2")
	if _, err := w.m.Verify(w.ctx, reopened, trLeader, "900", "head-bundle-2", "run-2", "/checkout", w.forge, w.proof); err != nil {
		t.Fatalf("verifying the survivors once more: %v", err)
	}
	w.tip.set(trRepo, trBase, "merge-2")
	w.forge.commits["merge-2"] = TrainCommit{SHA: "merge-2", Parents: []string{"base-0", "head-bundle-2"}, Tree: "tree-bundle"}
	if _, err := w.m.TrainLand(w.ctx, reopened, trLeader, "merge-2", "merge-2", w.tip, w.forge); err != nil {
		t.Fatalf("landing the survivors: %v", err)
	}
	if state := w.turn(w.turnIDOf(101)).State; state != "landed" {
		t.Fatalf("the leader's turn is %s, want landed", state)
	}
	if state := w.turn(w.turnIDOf(103)).State; state != "landed" {
		t.Fatalf("the surviving member's turn is %s, want landed", state)
	}
	if state := w.turn(dropped).State; state != "withdrawn" {
		t.Fatalf("the dropped member's turn is %s, want withdrawn and not landed", state)
	}
}
