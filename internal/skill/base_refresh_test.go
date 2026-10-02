package skill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// refreshRepo is a real synthetic repository: dev is the base branch, feature is a pull request
// branch whose last commit is the verified head, and the helpers make the merges a base update
// produces, and the merges that look like one and are not.
type refreshRepo struct {
	t    *testing.T
	path string
}

func newRefreshRepoFormat(t *testing.T, format string) *refreshRepo {
	t.Helper()
	r := &refreshRepo{t: t, path: t.TempDir()}
	r.git("init", "-q", "-b", "dev", "--object-format="+format)
	r.git("config", "user.email", "t@example.com")
	r.git("config", "user.name", "t")
	r.git("config", "commit.gpgsign", "false")
	r.commit("a.txt", numbered(40, nil), "base a")
	r.commit("b.txt", "base b\n", "base b")
	return r
}

func numbered(n int, replace map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if v, ok := replace[i]; ok {
			b.WriteString(v + "\n")
			continue
		}
		b.WriteString("line " + strings.Repeat("x", i) + "\n")
	}
	return b.String()
}

func (r *refreshRepo) env() []string {
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_DATE=2026-10-02T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-02T00:00:00Z", "LC_ALL=C", "HOME=" + r.path}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") && !strings.HasPrefix(v, "HOME=") && !strings.HasPrefix(v, "LC_ALL=") {
			env = append(env, v)
		}
	}
	return env
}

func (r *refreshRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitFails runs a command the fixture expects git to refuse, such as a merge that stops on a conflict.
func (r *refreshRepo) gitFails(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	cmd.Env = r.env()
	if out, err := cmd.CombinedOutput(); err == nil {
		r.t.Fatalf("git %v was meant to fail:\n%s", args, out)
	}
}

func (r *refreshRepo) write(file, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.path, file), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *refreshRepo) commit(file, content, message string) string {
	r.t.Helper()
	r.write(file, content)
	r.git("add", file)
	r.git("commit", "-q", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// branchFrom starts a branch at a revision and checks it out.
func (r *refreshRepo) branchFrom(name, rev string) { r.git("checkout", "-q", "-b", name, rev) }

// update is what the forge's update-branch call does: on the pull request branch, merge the base
// into it with a merge commit. It returns the new head.
func (r *refreshRepo) update(branch, base string) string {
	r.t.Helper()
	r.git("checkout", "-q", branch)
	r.git("merge", "-q", "--no-ff", "-m", "Merge branch '"+base+"' into "+branch, base)
	return r.git("rev-parse", "HEAD")
}

// scenario is the usual picture: dev moves by one commit in another file while feature carries
// one commit of its own, and previous is feature's tip before any update.
type scenario struct {
	r        *refreshRepo
	fork     string // the dev commit feature branched from
	previous string
}

func newScenario(t *testing.T) *scenario { return newScenarioFormat(t, "sha1") }

func newScenarioFormat(t *testing.T, format string) *scenario {
	r := newRefreshRepoFormat(t, format)
	s := &scenario{r: r, fork: r.git("rev-parse", "dev")}
	r.branchFrom("feature", s.fork)
	s.previous = r.commit("feature.txt", "feature work\n", "feature work")
	r.git("checkout", "-q", "dev")
	return s
}

// devMoves adds a commit to dev.
func (s *scenario) devMoves(file, content string) string {
	s.r.git("checkout", "-q", "dev")
	return s.r.commit(file, content, "dev change "+file)
}

func check(r *refreshRepo, previous, head, base string) skillProcessResult {
	return runSkillInProcess("base-refresh", "check", "--repo", r.path, "--previous", previous, "--head", head, "--base", base)
}

func TestBaseRefreshPassesAHeadThatOnlyMergesTheBase(t *testing.T) {
	t.Run("one update, files apart", func(t *testing.T) {
		s := newScenario(t)
		dev := s.devMoves("dev.txt", "dev work\n")
		head := s.r.update("feature", "dev")
		got := check(s.r, s.previous, head, "dev")
		if got.exit != 0 || !strings.HasPrefix(got.stdout, "ok: "+head+" is "+s.previous+" plus 1 merge of dev") {
			t.Fatalf("got %+v", got)
		}
		for _, want := range []string{"merge " + head, "first parent " + s.previous, "second parent " + dev, "head contains the tip of dev: yes"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("stdout lacks %q: %s", want, got.stdout)
			}
		}
	})
	t.Run("one update that merges a file both sides changed", func(t *testing.T) {
		// the merged blob is neither side's: a check that only compared files with the dev side
		// would refuse the most ordinary conflict-free update
		s := newScenario(t)
		s.r.git("checkout", "-q", "feature")
		prev := s.r.commit("a.txt", numbered(40, map[int]string{2: "feature edit"}), "feature edits a")
		s.devMoves("a.txt", numbered(40, map[int]string{38: "dev edit"}))
		head := s.r.update("feature", "dev")
		if blob := s.r.git("show", head+":a.txt"); !strings.Contains(blob, "feature edit") || !strings.Contains(blob, "dev edit") {
			t.Fatalf("the fixture did not merge both edits: %s", blob)
		}
		if got := check(s.r, prev, head, "dev"); got.exit != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("two updates in a row", func(t *testing.T) {
		s := newScenario(t)
		s.devMoves("dev.txt", "dev work\n")
		s.r.update("feature", "dev")
		s.devMoves("dev2.txt", "more dev work\n")
		head := s.r.update("feature", "dev")
		got := check(s.r, s.previous, head, "dev")
		if got.exit != 0 || !strings.HasPrefix(got.stdout, "ok: "+head+" is "+s.previous+" plus 2 merges of dev") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the base moved again after the update", func(t *testing.T) {
		// the answer is about what the head is made of; whether it is current is the forge's
		s := newScenario(t)
		s.devMoves("dev.txt", "dev work\n")
		head := s.r.update("feature", "dev")
		tip := s.devMoves("dev2.txt", "newer\n")
		got := check(s.r, s.previous, head, "dev")
		if got.exit != 0 || !strings.Contains(got.stdout, "note: head does not contain the tip of dev ("+tip+")") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the checkout is left as it was", func(t *testing.T) {
		s := newScenario(t)
		s.devMoves("dev.txt", "dev work\n")
		head := s.r.update("feature", "dev")
		before := s.r.git("count-objects", "-v") + s.r.git("for-each-ref") + s.r.git("status", "--porcelain")
		if got := check(s.r, s.previous, head, "dev"); got.exit != 0 {
			t.Fatalf("got %+v", got)
		}
		if after := s.r.git("count-objects", "-v") + s.r.git("for-each-ref") + s.r.git("status", "--porcelain"); after != before {
			t.Fatalf("the check changed the checkout:\n%s\n---\n%s", before, after)
		}
	})
}

func TestBaseRefreshRefusesAHeadThatIsAnythingElse(t *testing.T) {
	type refusal struct {
		name string
		// build returns previous and head; the scenario's dev has already moved once
		build func(s *scenario) (previous, head string)
		code  string
		says  string
	}
	cases := []refusal{
		{"the head is the previous head", func(s *scenario) (string, string) { return s.previous, s.previous }, "no_update", ""},
		{"a commit of its own on top of the update", func(s *scenario) (string, string) {
			s.r.update("feature", "dev")
			return s.previous, s.r.commit("feature.txt", "changed after the update\n", "fix after update")
		}, "not_a_merge", ""},
		{"a plain commit instead of an update", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			return s.previous, s.r.commit("feature.txt", "more work\n", "more work")
		}, "not_a_merge", ""},
		{"an ordinary commit and then the update", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			s.r.commit("feature2.txt", "slipped in before the update\n", "extra work")
			return s.previous, s.r.update("feature", "dev")
		}, "not_a_merge", ""},
		{"a rebase of the branch onto the base", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			s.r.git("rebase", "-q", "dev")
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "not_a_merge", ""},
		{"an octopus merge", func(s *scenario) (string, string) {
			// two sides neither of which contains the other, or git drops one as redundant
			s.r.branchFrom("extra1", s.fork)
			s.r.commit("extra1.txt", "extra one\n", "extra one")
			s.r.branchFrom("extra2", s.fork)
			s.r.commit("extra2.txt", "extra two\n", "extra two")
			s.r.git("checkout", "-q", "feature")
			s.r.git("merge", "-q", "--no-ff", "-m", "octopus", "extra1", "extra2")
			if n := len(strings.Fields(s.r.git("rev-list", "--parents", "-n", "1", "HEAD"))) - 1; n != 3 {
				s.r.t.Fatalf("the fixture was meant to be an octopus of three parents, got %d", n)
			}
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "not_a_merge", ""},
		{"a merge of a branch that is not the base", func(s *scenario) (string, string) {
			s.r.branchFrom("side", s.fork)
			s.r.commit("side.txt", "side\n", "side work")
			return s.previous, s.r.update("feature", "side")
		}, "not_from_base", ""},
		{"the merge in the other direction", func(s *scenario) (string, string) {
			s.r.branchFrom("reverse", "dev")
			s.r.git("merge", "-q", "--no-ff", "-m", "Merge feature into dev", "feature")
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "not_from_base", ""},
		{"a merge that discards the base's change", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			s.r.git("merge", "-q", "--no-ff", "-s", "ours", "-m", "Merge branch 'dev' into feature", "dev")
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "tree_differs", "dev.txt"},
		{"a merge that carries an edit of its own", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			s.r.git("merge", "-q", "--no-ff", "--no-commit", "dev")
			s.r.write("smuggled.txt", "not from dev\n")
			s.r.git("add", "smuggled.txt")
			s.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "tree_differs", "smuggled.txt"},
		{"a merge the author resolved by hand", func(s *scenario) (string, string) {
			s.r.git("checkout", "-q", "feature")
			prev := s.r.commit("a.txt", numbered(40, map[int]string{5: "feature side"}), "feature edits a")
			s.devMoves("a.txt", numbered(40, map[int]string{5: "dev side"}))
			s.r.git("checkout", "-q", "feature")
			// the merge stops on the conflict; the author picks a line of their own
			s.r.gitFails("merge", "--no-ff", "dev")
			s.r.write("a.txt", numbered(40, map[int]string{5: "resolved by hand"}))
			s.r.git("add", "a.txt")
			s.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
			s.previous = prev
			return prev, s.r.git("rev-parse", "HEAD")
		}, "merge_conflicts", "a.txt"},
		{"a head that is the tip of the base", func(s *scenario) (string, string) {
			return s.previous, s.r.git("rev-parse", "dev")
		}, "not_built_on_previous", ""},
		{"a branch that never carried the previous head", func(s *scenario) (string, string) {
			s.r.branchFrom("other", s.fork)
			return s.previous, s.r.update("other", "dev")
		}, "not_built_on_previous", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.devMoves("dev.txt", "dev work\n")
			previous, head := c.build(s)
			got := check(s.r, previous, head, "dev")
			if got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: "+c.code+":") || !strings.Contains(got.stdout, c.says) || strings.Contains(got.stdout, "\nok:") {
				t.Fatalf("want refused %s (%q), got %+v", c.code, c.says, got)
			}
		})
	}
}

func TestBaseRefreshAnswersFromGitAlone(t *testing.T) {
	t.Run("a merge driver and attributes in the checkout are not run", func(t *testing.T) {
		s := newScenario(t)
		s.r.git("checkout", "-q", "dev")
		s.r.commit(".gitattributes", "a.txt merge=evil\n", "attributes")
		s.r.git("checkout", "-q", "feature")
		s.r.git("merge", "-q", "--no-edit", "dev")
		prev := s.r.commit("a.txt", numbered(40, map[int]string{2: "feature edit"}), "feature edits a")
		s.devMoves("a.txt", numbered(40, map[int]string{38: "dev edit"}))
		head := s.r.update("feature", "dev")
		marker := filepath.Join(s.r.path, "driver-ran")
		s.r.git("config", "merge.evil.driver", "touch "+marker+"; cp %A %A.out; exit 1")
		got := check(s.r, prev, head, "dev")
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the checkout's merge driver ran")
		}
		if got.exit != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a replace ref in the checkout changes nothing", func(t *testing.T) {
		s := newScenario(t)
		s.devMoves("dev.txt", "dev work\n")
		head := s.r.update("feature", "dev")
		s.r.git("replace", s.previous, s.fork)
		s.r.git("replace", head, s.fork)
		if got := check(s.r, s.previous, head, "dev"); got.exit != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("revisions are read as revisions, not as options", func(t *testing.T) {
		s := newScenario(t)
		got := runSkillInProcess("base-refresh", "check", "--repo", s.r.path, "--previous", "--output=x", "--head", "dev", "--base", "dev")
		if got.exit != 2 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestBaseRefreshBoundsTheWalk(t *testing.T) {
	old := maxRefreshMerges
	t.Cleanup(func() { maxRefreshMerges = old })
	maxRefreshMerges = 2
	s := newScenario(t)
	s.devMoves("dev.txt", "dev work\n")
	s.r.update("feature", "dev")
	s.devMoves("dev2.txt", "more\n")
	two := s.r.update("feature", "dev")
	if got := check(s.r, s.previous, two, "dev"); got.exit != 0 || !strings.Contains(got.stdout, "plus 2 merges") {
		t.Fatalf("two merges are within the bound: %+v", got)
	}
	s.devMoves("dev3.txt", "more again\n")
	three := s.r.update("feature", "dev")
	if got := check(s.r, s.previous, three, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_built_on_previous:") || !strings.Contains(got.stdout, "more than 2") {
		t.Fatalf("three merges are over the bound: %+v", got)
	}
}

func TestBaseRefreshCannotAnswerExitsTwo(t *testing.T) {
	s := newScenario(t)
	s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	empty := t.TempDir()
	shallow := filepath.Join(t.TempDir(), "shallow")
	s.r.git("clone", "-q", "--depth", "1", "--branch", "feature", "file://"+s.r.path, shallow)
	for _, c := range []struct {
		name string
		args []string
		says string
	}{
		{"an unknown previous head", []string{"--previous", strings.Repeat("0", 40), "--head", head, "--base", "dev"}, "cannot read"},
		{"an unknown head", []string{"--previous", s.previous, "--head", "nope", "--base", "dev"}, "cannot read"},
		{"an unknown base", []string{"--previous", s.previous, "--head", head, "--base", "origin/dev"}, "cannot read"},
		{"a directory that is not a repository", []string{"--repo", empty, "--previous", s.previous, "--head", head, "--base", "dev"}, "cannot read"},
		{"a shallow checkout that lacks the previous head", []string{"--repo", shallow, "--previous", s.previous, "--head", head, "--base", "dev"}, "cannot read"},
		{"no previous head", []string{"--head", head, "--base", "dev"}, "--previous is required"},
		{"no head", []string{"--previous", s.previous, "--base", "dev"}, "--head is required"},
		{"no base", []string{"--previous", s.previous, "--head", head}, "--base is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"base-refresh", "check"}, c.args...)
			if !containsFlag(c.args, "--repo") {
				args = append(args, "--repo", s.r.path)
			}
			got := runSkillInProcess(args...)
			if got.exit != 2 || !strings.Contains(got.stderr, c.says) || got.stdout != "" {
				t.Fatalf("want exit 2 saying %q, got %+v", c.says, got)
			}
		})
	}
}

func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBaseRefreshReadsTheAttributesCommittedInTheBranch(t *testing.T) {
	// merge=union keeps both sides' lines where they changed the same one, so git merges the update
	// cleanly; a check that ignored the committed attributes would see a conflict and refuse an honest
	// update, and one that read a working tree's would depend on whoever ran it
	s := newScenario(t)
	s.r.git("checkout", "-q", "dev")
	s.r.commit(".gitattributes", "a.txt merge=union\n", "union attribute")
	s.r.git("checkout", "-q", "feature")
	s.r.git("merge", "-q", "--no-edit", "dev")
	prev := s.r.commit("a.txt", numbered(40, map[int]string{5: "feature side"}), "feature edits a")
	s.devMoves("a.txt", numbered(40, map[int]string{5: "dev side"}))
	head := s.r.update("feature", "dev")
	if blob := s.r.git("show", head+":a.txt"); !strings.Contains(blob, "feature side") || !strings.Contains(blob, "dev side") {
		t.Fatalf("the fixture was meant to keep both sides' line: %s", blob)
	}
	if got := check(s.r, prev, head, "dev"); got.exit != 0 || !strings.HasPrefix(got.stdout, "ok: ") {
		t.Fatalf("got %+v", got)
	}
}

func TestBaseRefreshNeedsAGitThatReadsCommittedAttributes(t *testing.T) {
	for _, c := range []struct {
		version string
		ok      bool
	}{
		{"git version 2.53.0", true},
		{"git version 2.41.0", true},
		{"git version 2.40.1", false},
		{"git version 2.38.5", false},
		{"git version 1.9.5", false},
		{"git version 3.0.0", true},
		{"git version 2.43.0 (Apple Git-146)", true},
		{"not a version", false},
		{"", false},
	} {
		if err := requireGit(c.version); (err == nil) != c.ok {
			t.Errorf("requireGit(%q) = %v, want ok=%v", c.version, err, c.ok)
		}
	}
	// the same refusal through the command: a git that says it is older answers exit 2, never a verdict
	s := newScenario(t)
	s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.40.1'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got := check(s.r, s.previous, head, "dev")
	if got.exit != 2 || got.stdout != "" || !strings.Contains(got.stderr, "git 2.41 or newer is needed") {
		t.Fatalf("got %+v", got)
	}
}

func TestBaseRefreshWorksInASHA256Checkout(t *testing.T) {
	// the throwaway repository has to use the checkout's object format, or it cannot read the
	// objects it borrows and every honest update in such a checkout would end in exit 2
	s := newScenarioFormat(t, "sha256")
	s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	if len(head) != 64 {
		t.Fatalf("the fixture was meant to be a SHA-256 repository, got commit id %q", head)
	}
	got := check(s.r, s.previous, head, "dev")
	if got.exit != 0 || !strings.HasPrefix(got.stdout, "ok: "+head+" is "+s.previous+" plus 1 merge of dev") {
		t.Fatalf("got %+v", got)
	}
	if refused := check(s.r, s.previous, s.previous, "dev"); refused.exit != 1 || !strings.HasPrefix(refused.stdout, "refused: no_update:") {
		t.Fatalf("got %+v", refused)
	}
}
