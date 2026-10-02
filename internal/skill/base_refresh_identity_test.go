package skill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tree-identity rule (CRW-313): a refreshed head passes only when git's merge of the verified
// head and the dev tip exits 0, its tree is the tree of the new head, and the new head's parents are
// exactly (verified head, dev tip) in that order. These tests build real repositories and ask git
// for the expected values themselves, so a mistake in the check cannot hide behind a mistake in the
// fixture.

func TestBaseRefreshPassesOnTreeIdentityAndPrintsTheEvidence(t *testing.T) {
	s := newScenario(t)
	dev := s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	// what git itself merges from the two commits, asked outside the check
	want := s.r.git("merge-tree", "--write-tree", s.previous, dev)
	if got := s.r.git("rev-parse", head+"^{tree}"); got != want {
		t.Fatalf("the fixture is not a tree-identical update: merge-tree %s, head tree %s", want, got)
	}
	got := check(s.r, s.previous, head, "dev")
	if got.exit != 0 || !strings.HasPrefix(got.stdout, "ok: "+head+" is "+s.previous+" plus the tip of dev ("+dev+") and nothing else\n") {
		t.Fatalf("got %+v", got)
	}
	// the line a parent copies into the merge record and the merged mark: five fields, one rule
	line := "evidence: previous=" + s.previous + " dev_tip=" + dev + " head=" + head + " tree=" + want + " rule=tree_identity"
	if !strings.Contains(got.stdout, "\n"+line+"\n") {
		t.Errorf("stdout lacks %q:\n%s", line, got.stdout)
	}
	for _, need := range []string{
		"parents: (" + s.previous + ", " + dev + ") in that order",
		"tree identity: git merge-tree --write-tree " + s.previous + " " + dev + " exits 0 and its tree " + want + " is the tree of " + head,
	} {
		if !strings.Contains(got.stdout, need) {
			t.Errorf("stdout lacks %q:\n%s", need, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "refused") {
		t.Errorf("a pass names no refusal:\n%s", got.stdout)
	}
}

func TestBaseRefreshRefusesAHeadWhoseParentsAreNotExactlyThePreviousHeadAndTheDevTip(t *testing.T) {
	type refusal struct {
		name  string
		build func(s *scenario, dev string) (previous, head string)
		code  string
		says  string
		safe  string
	}
	cases := []refusal{
		{"the parent order swapped", func(s *scenario, dev string) (string, string) {
			// the right tree, the right two parents, the wrong order: it is a merge of the branch into dev
			tree := s.r.git("merge-tree", "--write-tree", s.previous, dev)
			return s.previous, s.r.git("commit-tree", tree, "-p", dev, "-p", s.previous, "-m", "Merge branch 'feature' into dev")
		}, "parents_swapped", "(previous head, dev tip) in that order", "Return the candidate to its child"},
		{"one mixed line in the merge", func(s *scenario, dev string) (string, string) {
			s.r.git("checkout", "-q", "feature")
			s.r.git("merge", "-q", "--no-ff", "--no-commit", "dev")
			// one line of an existing file differs from what git merges, and nothing else does
			s.r.write("a.txt", numbered(40, map[int]string{20: "one line that is in neither parent"}))
			s.r.git("add", "a.txt")
			s.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
			return s.previous, s.r.git("rev-parse", "HEAD")
		}, "tree_differs", "a.txt", "Return the candidate to its child"},
		{"two updates in one head", func(s *scenario, dev string) (string, string) {
			s.r.update("feature", "dev")
			s.devMoves("dev2.txt", "more dev work\n")
			return s.previous, s.r.update("feature", "dev")
		}, "not_built_on_previous", "an earlier update", "one step at a time"},
		{"an update of an older tip after the base moved", func(s *scenario, dev string) (string, string) {
			head := s.r.update("feature", "dev")
			s.devMoves("dev2.txt", "newer\n")
			return s.previous, head
		}, "not_the_dev_tip", "the base moved", "read the tip of the base from the forge again"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			dev := s.devMoves("dev.txt", "dev work\n")
			previous, head := c.build(s, dev)
			got := check(s.r, previous, head, "dev")
			if got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: "+c.code+":") || !strings.Contains(got.stdout, c.says) || strings.Contains(got.stdout, "ok:") || strings.Contains(got.stdout, "evidence:") {
				t.Fatalf("want refused %s (%q) and no evidence, got %+v", c.code, c.says, got)
			}
			// the skill's safe side is named in the answer: back to the child, a recheck is not a pass
			// the line itself: the refusal's detail may quote the same words
			var line string
			for _, l := range strings.Split(got.stdout, "\n") {
				if strings.HasPrefix(l, "safe side: ") {
					line = l
				}
			}
			if !strings.Contains(line, c.safe) {
				t.Errorf("the safe-side line lacks %q:\n%s", c.safe, got.stdout)
			}
		})
	}
}

func TestBaseRefreshProvesAChainOfUpdatesOneStepAtATime(t *testing.T) {
	s := newScenario(t)
	d1 := s.devMoves("dev.txt", "dev work\n")
	h1 := s.r.update("feature", "dev")
	if got := check(s.r, s.previous, h1, "dev"); got.exit != 0 {
		t.Fatalf("the first update: %+v", got)
	}
	d2 := s.devMoves("dev2.txt", "more dev work\n")
	h2 := s.r.update("feature", "dev")
	// the whole chain from the verified head is not one update ...
	if got := check(s.r, s.previous, h2, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_built_on_previous:") {
		t.Fatalf("the chain in one step: %+v", got)
	}
	// ... each step is, with the head the earlier proof named as the previous head
	got := check(s.r, h1, h2, "dev")
	want := "evidence: previous=" + h1 + " dev_tip=" + d2 + " head=" + h2 + " tree=" + s.r.git("rev-parse", h2+"^{tree}") + " rule=tree_identity"
	if got.exit != 0 || !strings.Contains(got.stdout, "\n"+want+"\n") {
		t.Fatalf("the second update: %+v", got)
	}
	if d1 == d2 {
		t.Fatal("the fixture moved dev once")
	}
}

func TestBaseRefreshPassesAnUpdateAcrossARename(t *testing.T) {
	// dev renames a file the branch edited: git's merge carries the edit to the new name, and the
	// check reads the same merge, so an honest update is not refused for it
	s := newScenario(t)
	s.r.git("checkout", "-q", "feature")
	prev := s.r.commit("b.txt", "base b\nfeature edit\n", "feature edits b")
	s.r.git("checkout", "-q", "dev")
	s.r.git("mv", "b.txt", "c.txt")
	s.r.git("commit", "-q", "-m", "dev renames b")
	head := s.r.update("feature", "dev")
	if blob := s.r.git("show", head+":c.txt"); !strings.Contains(blob, "feature edit") {
		t.Fatalf("the fixture was meant to carry the edit to the new name: %s", blob)
	}
	if got := check(s.r, prev, head, "dev"); got.exit != 0 || !strings.Contains(got.stdout, "rule=tree_identity") {
		t.Fatalf("got %+v", got)
	}
}

func TestBaseRefreshRecoversFromABaseThatMovedUnderTheUpdate(t *testing.T) {
	s := newScenario(t)
	pinned := s.devMoves("dev.txt", "dev work\n") // the tip the parent read before the update call
	newer := s.devMoves("dev2.txt", "newer\n")    // dev moved before the call, so the call merged this one
	head := s.r.update("feature", "dev")
	// the pinned tip does not name what the update merged: refused, and the line says to read the tip again
	got := check(s.r, s.previous, head, pinned)
	if got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_from_base:") || !strings.Contains(got.stdout, "\nsafe side: this head is not accepted against the commit named. If the base moved") {
		t.Fatalf("pinned tip: %+v", got)
	}
	// the tip read again is what was merged: N is current and proved
	if got := check(s.r, s.previous, head, "dev"); got.exit != 0 || !strings.Contains(got.stdout, "dev_tip="+newer+" ") {
		t.Fatalf("tip read again: %+v", got)
	}
	// dev moves once more: the update merged an older commit than the tip, which is a refusal of its own
	s.devMoves("dev3.txt", "newest\n")
	if got := check(s.r, s.previous, head, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_the_dev_tip:") || !strings.Contains(got.stdout, "parents="+s.previous+","+newer) {
		t.Fatalf("older commit than the tip: %+v", got)
	}
	// nothing proves the second parent against a tip no forge reading named, so the answer stays a refusal
}

func TestBaseRefreshProvesAgainstTheCommitItIsNamed(t *testing.T) {
	// The check proves an update against whatever commit it is named, tip or not: a commit that reached
	// dev inside a merged side branch passes when named, and so would an intermediate commit of a push
	// that moved dev by several commits. Only the procedure says what may be named (a tip read from
	// the forge), and the check cannot tell the two apart, so this test pins both answers.
	s := newScenario(t)
	s.r.branchFrom("side", s.fork)
	side := s.r.commit("side.txt", "side\n", "side work")
	s.r.git("checkout", "-q", "dev")
	s.r.git("merge", "-q", "--no-ff", "-m", "Merge side", "side")
	s.devMoves("dev.txt", "dev work\n")
	s.r.git("checkout", "-q", "feature")
	s.r.git("merge", "-q", "--no-ff", "-m", "Merge a commit of the side branch", side)
	head := s.r.git("rev-parse", "HEAD")
	if got := check(s.r, s.previous, head, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_the_dev_tip:") {
		t.Fatalf("named by the branch: %+v", got)
	}
	if got := check(s.r, s.previous, head, side); got.exit != 0 {
		t.Fatalf("the check proves the update against the commit it is named: %+v", got)
	}
}

func TestBaseRefreshEvidenceLineReplaysTheCheck(t *testing.T) {
	// the line is a complete input: the five fields, read back, give the same answer, which is how a
	// parent that resumes (or an audit after the landing) reproduces the proof
	s := newScenario(t)
	s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	first := check(s.r, s.previous, head, "dev")
	evidenceLine := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "evidence: ") {
				return l
			}
		}
		return ""
	}
	line := evidenceLine(first.stdout)
	fields := map[string]string{}
	for _, kv := range strings.Fields(strings.TrimPrefix(line, "evidence: ")) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("not a key=value pair: %q in %q", kv, line)
		}
		fields[k] = v
	}
	if len(fields) != 5 || fields["rule"] != "tree_identity" {
		t.Fatalf("want the five fields of the evidence line, got %v from %q", fields, line)
	}
	s.devMoves("dev2.txt", "dev moved afterwards\n")
	again := check(s.r, fields["previous"], fields["head"], fields["dev_tip"])
	if again.exit != 0 || evidenceLine(again.stdout) != line {
		t.Fatalf("replay from the evidence line differs:\n%s\n---\n%s", first.stdout, again.stdout)
	}
}

func TestBaseRefreshReplaysAfterTheLandingWithTheTipSeenBefore(t *testing.T) {
	s := newScenario(t)
	d1 := s.devMoves("dev.txt", "dev work\n")
	head := s.r.update("feature", "dev")
	s.r.git("checkout", "-q", "dev")
	s.r.git("merge", "-q", "--no-ff", "-m", "Merge pull request", "feature")
	// a head that has landed is on the base: the branch name no longer answers
	if got := check(s.r, s.previous, head, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: not_built_on_previous:") || !strings.Contains(got.stdout, "already on the base") {
		t.Fatalf("after the landing, by branch name: %+v", got)
	}
	// the tip seen before the landing still does
	if got := check(s.r, s.previous, head, d1); got.exit != 0 || !strings.Contains(got.stdout, "dev_tip="+d1+" ") {
		t.Fatalf("after the landing, by the tip seen before: %+v", got)
	}
}

// gitShim puts a git ahead of the real one on PATH that hands every command to the real git
// except merge-tree, which answers with the given script body.
func gitShim(t *testing.T, mergeTreeBody string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = merge-tree ]; then\n" + mergeTreeBody + "\n  fi\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBaseRefreshIsNoPassWhenMergeTreeDoesNotExitZero(t *testing.T) {
	t.Run("a conflict is a refusal", func(t *testing.T) {
		s := newScenario(t)
		s.r.git("checkout", "-q", "feature")
		prev := s.r.commit("a.txt", numbered(40, map[int]string{5: "feature side"}), "feature edits a")
		s.devMoves("a.txt", numbered(40, map[int]string{5: "dev side"}))
		s.r.git("checkout", "-q", "feature")
		s.r.gitFails("merge", "--no-ff", "dev")
		s.r.write("a.txt", numbered(40, map[int]string{5: "resolved by hand"}))
		s.r.git("add", "a.txt")
		s.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
		head := s.r.git("rev-parse", "HEAD")
		if got := check(s.r, prev, head, "dev"); got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: merge_conflicts:") || strings.Contains(got.stdout, "evidence:") {
			t.Fatalf("got %+v", got)
		}
	})
	for _, c := range []struct {
		name, body, says string
	}{
		{"a merge-tree that fails", "    echo 'fatal: merge-tree is broken' >&2; exit 128", "git merge-tree could not merge"},
		{"a merge-tree that exits 0 without a tree", "    echo 'not a tree'; exit 0", "not a merge result"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.devMoves("dev.txt", "dev work\n")
			head := s.r.update("feature", "dev")
			gitShim(t, c.body)
			got := check(s.r, s.previous, head, "dev")
			if got.exit != 2 || got.stdout != "" || !strings.Contains(got.stderr, c.says) {
				t.Fatalf("git could not answer, which is not a pass: %+v", got)
			}
		})
	}
}
