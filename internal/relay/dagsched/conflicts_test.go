package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parallel is two branches off one base that change the files named, each file with the content the branch gives it.
func (r *gitRepo) parallel(left, right map[string]string) (string, string) {
	r.t.Helper()
	base := r.git("rev-parse", "HEAD")
	branch := func(name string, files map[string]string) string {
		r.git("checkout", "-q", "-b", name, base)
		var head string
		for file, content := range files {
			head = r.commit(file, content)
		}
		r.git("checkout", "-q", "dev")
		return head
	}
	return branch("left-"+base[:8], left), branch("right-"+base[:8], right)
}

func lines(n int, replace map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if v, ok := replace[i]; ok {
			b.WriteString(v + "\n")
			continue
		}
		b.WriteString("line " + string(rune('a'+i%26)) + "\n")
	}
	return b.String()
}

// Criterion c7: the number of merge-tree conflicts between the heads of two parallel branches is recorded. Real repositories: disjoint files, the same lines of one file, two files, and
// two distant hunks of one file (which merge cleanly).
func TestMergeTreeConflictCount(t *testing.T) {
	cases := []struct {
		name        string
		left, right map[string]string
		want        []string
	}{
		{"disjoint files", map[string]string{"a.txt": "left a\n"}, map[string]string{"b.txt": "right b\n"}, nil},
		{"the same lines of one file", map[string]string{"c.txt": lines(12, map[int]string{3: "left"})}, map[string]string{"c.txt": lines(12, map[int]string{3: "right"})}, []string{"c.txt"}},
		{"two conflicting files", map[string]string{"c.txt": lines(12, map[int]string{3: "left"}), "d.txt": lines(12, map[int]string{5: "left"})},
			map[string]string{"c.txt": lines(12, map[int]string{3: "right"}), "d.txt": lines(12, map[int]string{5: "right"})}, []string{"c.txt", "d.txt"}},
		{"two distant hunks of one file", map[string]string{"c.txt": lines(40, map[int]string{2: "left"})}, map[string]string{"c.txt": lines(40, map[int]string{38: "right"})}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newIntegrationKit(t)
			repo := k.repo
			// the base holds the files the branches change
			repo.commit("c.txt", lines(12, nil))
			if strings.Contains(c.name, "distant") {
				repo.commit("c.txt", lines(40, nil))
			}
			repo.commit("d.txt", lines(12, nil))
			before := repo.git("count-objects", "-v")
			left, right := repo.parallel(c.left, c.right)
			objectsBefore := repo.git("count-objects", "-v")
			res, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: repo.path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(res.Files, ",") != strings.Join(c.want, ",") || res.Conflicts != len(c.want) || res.Method != "git merge-tree --write-tree" || res.BaseSHA == "" {
				t.Fatalf("conflicts = %+v, want %v", res, c.want)
			}
			if after := repo.git("count-objects", "-v"); after != objectsBefore {
				t.Fatalf("observing the conflicts changed the checkout's objects:\n%s\n%s\n(before the branches: %s)", objectsBefore, after, before)
			}
			// the nodes are stored in sorted order with their heads
			var l, r, lh, rh string
			var count int
			if err := k.s.DB.QueryRow("SELECT left_node_id, right_node_id, left_head, right_head, conflict_count FROM dag_conflict_observations").Scan(&l, &r, &lh, &rh, &count); err != nil ||
				l != "D" || r != "I" || lh != left || rh != right || count != len(c.want) {
				t.Fatalf("row = %s %s %s %s %d %v", l, r, lh, rh, count, err)
			}
			// asked the other way round it is the same row
			again, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: repo.path, LeftNode: "I", RightNode: "D", LeftHead: right, RightHead: left})
			if err != nil || !again.Replayed || again.ObservationID != res.ObservationID || k.count("SELECT COUNT(*) FROM dag_conflict_observations") != 1 {
				t.Fatalf("the pair the other way round = %v %+v", err, again)
			}
		})
	}
}

// What is not a pair of branches of a plan is refused and writes nothing: a node of another kind, two heads that are not commits of the checkout, a repository that is not a local path,
// and a caller that is not the project's parent.
func TestObserveConflictsRefusals(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	left, right := repo.parallel(map[string]string{"a.txt": "left\n"}, map[string]string{"b.txt": "right\n"})
	ok := ConflictInput{Repository: repo.path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right}
	for name, c := range map[string]struct {
		mutate func(*ConflictInput)
		actor  string
		reason string
	}{
		"a node that has no branch": {func(in *ConflictInput) { in.RightNode = "K" }, "parent", "disposition_conflict"},
		"a node the plan lacks":     {func(in *ConflictInput) { in.RightNode = "Z" }, "parent", "unregistered_scope"},
		"a node with itself":        {func(in *ConflictInput) { in.RightNode = "D" }, "parent", "malformed_receipt"},
		"a head that is not a sha":  {func(in *ConflictInput) { in.LeftHead = "main" }, "parent", "malformed_receipt"},
		"a head the checkout lacks": {func(in *ConflictInput) { in.LeftHead = strings.Repeat("1", 40) }, "parent", "merge_target_unreadable"},
		"a forge repository":        {func(in *ConflictInput) { in.Repository = "owner/repo" }, "parent", "malformed_receipt"},
		"another task":              {func(in *ConflictInput) {}, "intruder", "scope_role_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			in := ok
			c.mutate(&in)
			if _, err := k.sched.ObserveConflicts(context.Background(), "g", c.actor, in); refusalReason(err) != c.reason {
				t.Fatalf("observe = %v, want %s", err, c.reason)
			}
			if k.count("SELECT COUNT(*) FROM dag_conflict_observations") != 0 {
				t.Fatal("a refused observation wrote a row")
			}
		})
	}
}

// The checkout is whatever git says it is: a linked working tree, a path with a colon in it (an environment list of alternates would split it) and a file whose name is a single space are all
// measured, and a merge git could not compute is a failure and never a count of zero.
func TestObserveConflictsInUnusualCheckouts(t *testing.T) {
	conflict := func(t *testing.T, repo *gitRepo, name string) (string, string) {
		t.Helper()
		repo.commit(name, lines(12, nil))
		return repo.parallel(map[string]string{name: lines(12, map[int]string{3: "left"})}, map[string]string{name: lines(12, map[int]string{3: "right"})})
	}
	observe := func(t *testing.T, k *integrationKit, path, left, right string) (ConflictResult, error) {
		t.Helper()
		return k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right})
	}
	t.Run("a linked working tree", func(t *testing.T) {
		k := newIntegrationKit(t)
		left, right := conflict(t, k.repo, "c.txt")
		linked := filepath.Join(t.TempDir(), "linked")
		k.repo.git("worktree", "add", "-q", linked, "-b", "linked-branch")
		res, err := observe(t, k, linked, left, right)
		if err != nil || res.Conflicts != 1 || strings.Join(res.Files, ",") != "c.txt" {
			t.Fatalf("conflicts = %v %+v", err, res)
		}
	})
	t.Run("a path with a colon", func(t *testing.T) {
		k := newIntegrationKit(t)
		colon := newGitRepoAt(t, filepath.Join(t.TempDir(), "a:b"))
		left, right := conflict(t, colon, "c.txt")
		res, err := observe(t, k, colon.path, left, right)
		if err != nil || res.Conflicts != 1 {
			t.Fatalf("conflicts = %v %+v", err, res)
		}
	})
	t.Run("a file named with a space", func(t *testing.T) {
		k := newIntegrationKit(t)
		left, right := conflict(t, k.repo, " ")
		res, err := observe(t, k, k.repo.path, left, right)
		if err != nil || res.Conflicts != 1 || len(res.Files) != 1 || res.Files[0] != " " {
			t.Fatalf("conflicts = %v %+v", err, res)
		}
	})
	t.Run("a merge git cannot compute is not zero conflicts", func(t *testing.T) {
		k := newIntegrationKit(t)
		missing := strings.Repeat("7", 40)
		if files, err := mergeTreeConflicts(context.Background(), k.repo.path, missing, missing); err == nil {
			t.Fatalf("an impossible merge answered %v", files)
		}
		if k.count("SELECT COUNT(*) FROM dag_conflict_observations") != 0 {
			t.Fatal("a row was written")
		}
	})
	t.Run("the throwaway directory is removed", func(t *testing.T) {
		k := newIntegrationKit(t)
		left, right := conflict(t, k.repo, "c.txt")
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		if _, err := observe(t, k, k.repo.path, left, right); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Fatalf("left behind: %v", entries)
		}
	})
}

// The checkout's own configuration and working tree are not part of the question: a merge driver it configures would be a command run for whoever observes it, and an uncommitted
// .gitattributes would change the count of the same two commits. Neither runs, and neither changes the answer.
func TestObserveConflictsIgnoresTheCheckoutsConfigurationAndAttributes(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.commit("c.txt", lines(12, nil))
	left, right := repo.parallel(map[string]string{"c.txt": lines(12, map[int]string{3: "left"})}, map[string]string{"c.txt": lines(12, map[int]string{3: "right"})})
	marker := filepath.Join(t.TempDir(), "driver-ran")
	repo.git("config", "merge.x.driver", "touch "+marker)
	if err := os.WriteFile(filepath.Join(repo.path, ".gitattributes"), []byte("c.txt merge=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: repo.path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right})
	if err != nil || res.Conflicts != 1 {
		t.Fatalf("with a merge driver configured = %v %+v", err, res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checkout's merge driver was run")
	}
	// an uncommitted attribute that would make git resolve the file by union
	if err := os.WriteFile(filepath.Join(repo.path, ".gitattributes"), []byte("c.txt merge=union\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := k.sched.ObserveConflicts(context.Background(), "g", "parent", ConflictInput{Repository: repo.path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right}); err != nil || !again.Replayed || again.Conflicts != 1 {
		t.Fatalf("with an uncommitted attribute = %v %+v", err, again)
	}
}
