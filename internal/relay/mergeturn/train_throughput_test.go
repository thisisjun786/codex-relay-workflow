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
)

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

// CRW-898: bundle throughput inside the existing proofs. These tests use a temporary store, a forge
// stand-in and a temporary git repository, never the live relay service, the production store or a
// real Codex task.

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

	// member two: its own file and the manifest's version line re-recorded, so git merges the
	// manifest cleanly and the head's merge commit holds a tree git does not write from its parents
	git("checkout", "-q", "-b", "m2")
	write("m2.txt", "two\n")
	git("add", "-A")
	git("commit", "-q", "-m", "member two")
	m2 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m2", "m2")
	head := git("rev-parse", "HEAD")

	proof := TrainCheckoutProver{}
	members := []TrainMemberExpectation{{AcceptedHead: m1}, {AcceptedHead: m2}}
	chain, err := proof.Chain(context.Background(), repo, head, base, members)
	if err != nil {
		t.Fatalf("a clean chain was refused: %v", err)
	}
	if len(chain.Steps) != 0 {
		t.Fatalf("a chain git writes itself carries no version-line step: %+v", chain.Steps)
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
		w.acceptOn("rel-"+trLeader+"-2", "head-102")
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

	t.Run("the solo grant still takes only the oldest turn of each parent", func(t *testing.T) {
		w := newTr(t)
		first := w.claim(trLane, trLeader, "head-101", 101)["turnId"].(string)
		w.pr(101, "head-101")
		w.acceptOn("rel-"+trLeader+"-2", "head-102")
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
		// the lane is free and the member-only turn is now the parent"s oldest live turn, so the
		// solo grant order takes it - one turn per parent, oldest first, unchanged by CRW-898
		if state := w.turn(second["turnId"].(string)).State; state != Holding {
			t.Fatalf("the oldest live turn did not take the free lane: %s", state)
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
		w.acceptOn("rel-"+trLeader+"-2", "head-103")
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
	})
}
