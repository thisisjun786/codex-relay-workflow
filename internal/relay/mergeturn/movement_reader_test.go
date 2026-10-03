package mergeturn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The movement reader against a real local repository and against a forge shim.

func TestCRW403_GitMovementReadsTheFirstParentLine(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	m1 := g.merge(root, 1)
	m2 := g.merge(m1, 2)
	g.setTip(m2)
	got, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, m2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "local_git" || got.Reference != "refs/heads/dev" || got.Repository != g.dir || got.From != root || got.To != m2 {
		t.Fatalf("movement header: %+v", got)
	}
	if len(got.Steps) != 2 || got.Steps[0].SHA != m2 || got.Steps[1].SHA != m1 {
		t.Fatalf("steps: %+v", got.Steps)
	}
	if len(got.Steps[0].Parents) != 2 || got.Steps[0].Parents[0] != m1 || got.Steps[1].Parents[0] != root {
		t.Fatalf("parents: %+v", got.Steps)
	}
	if got.Steps[0].Subject != "Merge pull request #2 from team/topic-2" {
		t.Fatalf("subject %q", got.Steps[0].Subject)
	}
	// The reading stops where the recorded base is: nothing below it is returned.
	short, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", m1, m2)
	if err != nil || len(short.Steps) != 1 || short.Steps[0].SHA != m2 {
		t.Fatalf("short movement: %+v %v", short, err)
	}
	steps, why := outsideMerges(got, root, m2, nil)
	if why != "" || len(steps) != 2 {
		t.Fatalf("judged %d steps: %s", len(steps), why)
	}
}

func TestCRW403_GitMovementStopsAtTheBound(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	tip := root
	for i := 0; i < MaxMovementSteps+3; i++ {
		tip = g.merge(tip, i)
	}
	g.setTip(tip)
	got, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, tip)
	if err != nil || len(got.Steps) != MaxMovementSteps {
		t.Fatalf("%d steps, %v; want %d", len(got.Steps), err, MaxMovementSteps)
	}
	if _, why := outsideMerges(got, root, tip, nil); !strings.Contains(why, "does not lead from") {
		t.Fatalf("a line cut at the bound was judged: %q", why)
	}
	// A gap of exactly the bound is read to its end.
	exact := root
	for i := 0; i < MaxMovementSteps; i++ {
		exact = g.merge(exact, 500+i)
	}
	got, err = (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, exact)
	if err != nil || len(got.Steps) != MaxMovementSteps {
		t.Fatalf("%d steps, %v", len(got.Steps), err)
	}
	if steps, why := outsideMerges(got, root, exact, nil); why != "" || len(steps) != MaxMovementSteps {
		t.Fatalf("a gap of exactly the bound: %d steps, %q", len(steps), why)
	}
}

func TestCRW403_GitMovementIsUnreadableWhenGitCannotRead(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	tip := g.merge(root, 1)
	missing := strings.Repeat("c", 40)
	for name, call := range map[string]func() error{
		"abbreviated names": func() error {
			_, e := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root[:7], tip)
			return e
		},
		"revision syntax": func() error {
			_, e := (TargetReader{}).Movement(context.Background(), g.dir, "dev~1", root, tip)
			return e
		},
		"relative path": func() error {
			_, e := (TargetReader{}).Movement(context.Background(), "relative", "dev", root, tip)
			return e
		},
		"not a directory": func() error {
			_, e := (TargetReader{}).Movement(context.Background(), filepath.Join(t.TempDir(), "gone"), "dev", root, tip)
			return e
		},
		"an unknown commit": func() error {
			_, e := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, missing)
			return e
		},
		"git is missing": func() error {
			_, e := (TargetReader{Git: filepath.Join(t.TempDir(), "no-git")}).Movement(context.Background(), g.dir, "dev", root, tip)
			return e
		},
	} {
		var target *TargetUnreadable
		if err := call(); !errors.As(err, &target) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The environment cannot redirect the read to another repository.
	other := newLBGit(t)
	otherRoot := other.commit("elsewhere")
	t.Setenv("GIT_DIR", other.dir)
	if _, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, tip); err != nil {
		t.Fatalf("GIT_DIR redirected the read: %v (other repository root %s)", err, otherRoot)
	}
}

// A replace ref or a grafts file changes the parents git shows for a commit. The lane's reading is
// of the objects as stored, so neither can make a direct commit read as a merge.
func TestCRW403_GitMovementReadsStoredObjectsNotReplacements(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	side := g.commit("side", root)
	tip := g.commit("a direct commit", root)
	fake := g.commit("Merge pull request #9 from fake/y", root, side)
	g.setTip(tip)
	g.run("", "update-ref", "refs/replace/"+tip, fake)
	got, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, tip)
	if err != nil || len(got.Steps) != 1 || len(got.Steps[0].Parents) != 1 || got.Steps[0].Parents[0] != root {
		t.Fatalf("a replace ref changed what was read: %+v %v", got, err)
	}
	if _, why := outsideMerges(got, root, tip, nil); !strings.Contains(why, "one parent") {
		t.Fatalf("the replaced commit was judged: %q", why)
	}
}

func TestCRW403_GitMovementIgnoresAGraftsFile(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	side := g.commit("side", root)
	tip := g.commit("a direct commit", root)
	g.setTip(tip)
	if err := os.MkdirAll(filepath.Join(g.dir, "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "info", "grafts"), []byte(tip+" "+root+" "+side+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, tip)
	if err != nil || len(got.Steps) != 1 || len(got.Steps[0].Parents) != 1 {
		t.Fatalf("a grafts file changed what was read: %+v %v", got, err)
	}
}

// A tag object is not a commit: cat-file would peel it, so the reader asks for the object's type.
func TestCRW403_GitMovementRefusesATagObject(t *testing.T) {
	g := newLBGit(t)
	root := g.commit("root")
	tip := g.commit("tip", root)
	tag := g.run("object "+tip+"\ntype commit\ntag v1\ntagger Test <test@example.org> 0 +0000\n\nannotated\n", "mktag")
	var target *TargetUnreadable
	if _, err := (TargetReader{}).Movement(context.Background(), g.dir, "dev", root, tag); !errors.As(err, &target) || !strings.Contains(err.Error(), "is a tag, not a commit") {
		t.Fatalf("a tag object was read as a commit: %v", err)
	}
}

// lbForge is a gh shim over files: the branch ref and one file per commit. Every call is logged.
type lbForge struct {
	dir string
	gh  string
}

func newLBForge(t *testing.T) *lbForge {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + dir + "/calls'\ncase \"$6\" in\n" +
		"  repos/owner/repo/git/ref/heads/dev) cat '" + dir + "/ref.json';;\n" +
		"  repos/owner/repo/git/commits/*) f='" + dir + "/commit-'\"$(basename \"$6\")\"'.json'; if [ -f \"$f\" ]; then cat \"$f\"; else echo 'gh: HTTP 404' >&2; exit 1; fi;;\n" +
		"  *) echo 'gh: HTTP 404' >&2; exit 1;;\nesac\n"
	f := &lbForge{dir: dir, gh: filepath.Join(dir, "gh")}
	if err := os.WriteFile(f.gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func lbSHA(n int) string { return fmt.Sprintf("%040x", n) }

func (f *lbForge) commit(t *testing.T, sha, message string, parents ...string) {
	t.Helper()
	var list []string
	for _, p := range parents {
		list = append(list, `{"sha":"`+p+`","url":"https://api.example.invalid/`+p+`"}`)
	}
	body := fmt.Sprintf(`{"sha":"%s","message":%q,"parents":[%s],"verification":{"verified":false}}`, sha, message, strings.Join(list, ","))
	if err := os.WriteFile(filepath.Join(f.dir, "commit-"+sha+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *lbForge) setTip(t *testing.T, sha string) {
	t.Helper()
	body := `{"ref":"refs/heads/dev","object":{"type":"commit","sha":"` + sha + `"}}`
	if err := os.WriteFile(filepath.Join(f.dir, "ref.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *lbForge) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestCRW403_ForgeMovementFollowsTheFirstParentLineWithGETs(t *testing.T) {
	f := newLBForge(t)
	root, side1, m1, side2, m2 := lbSHA(1), lbSHA(2), lbSHA(3), lbSHA(4), lbSHA(5)
	f.commit(t, m2, "Merge pull request #2 from team/b\n\nbody", m1, side2)
	f.commit(t, m1, "Merge pull request #1 from team/a", root, side1)
	got, err := (TargetReader{GH: f.gh}).Movement(context.Background(), "owner/repo", "dev", root, m2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "github:github.com" || got.Reference != "refs/heads/dev" || len(got.Steps) != 2 || got.Steps[0].SHA != m2 || got.Steps[1].SHA != m1 || got.Steps[0].Subject != "Merge pull request #2 from team/b" {
		t.Fatalf("movement: %+v", got)
	}
	want := []string{
		"api --method GET -H Accept: application/vnd.github+json repos/owner/repo/git/commits/" + m2,
		"api --method GET -H Accept: application/vnd.github+json repos/owner/repo/git/commits/" + m1,
	}
	if calls := f.calls(t); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %q, want only GETs along the first-parent line %q", calls, want)
	}
	if steps, why := outsideMerges(got, root, m2, nil); why != "" || len(steps) != 2 {
		t.Fatalf("judged %d steps: %s", len(steps), why)
	}
}

func TestCRW403_ForgeMovementStopsAtTheBoundAndFailsClosed(t *testing.T) {
	f := newLBForge(t)
	root := lbSHA(1)
	tip := root
	next := 10
	for i := 0; i < MaxMovementSteps+2; i++ {
		merged := lbSHA(next)
		f.commit(t, merged, "Merge pull request", tip, lbSHA(next+1))
		tip = merged
		next += 2
	}
	got, err := (TargetReader{GH: f.gh}).Movement(context.Background(), "owner/repo", "dev", root, tip)
	if err != nil || len(got.Steps) != MaxMovementSteps || len(f.calls(t)) != MaxMovementSteps {
		t.Fatalf("%d steps, %d calls, %v; want %d of each", len(got.Steps), len(f.calls(t)), err, MaxMovementSteps)
	}
	var target *TargetUnreadable
	// A commit the forge does not have.
	if _, err := (TargetReader{GH: f.gh}).Movement(context.Background(), "owner/repo", "dev", root, lbSHA(999)); !errors.As(err, &target) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("an unknown commit: %v", err)
	}
	// An answer for another commit, a parent that is not an object name, a body that is not JSON.
	bad := map[string]string{
		lbSHA(900): `{"sha":"` + lbSHA(901) + `","message":"m","parents":[]}`,
		lbSHA(910): `{"sha":"` + lbSHA(910) + `","message":"m","parents":[{"sha":"HEAD~1"}]}`,
		lbSHA(920): `not json`,
	}
	for sha, body := range bad {
		if err := os.WriteFile(filepath.Join(f.dir, "commit-"+sha+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (TargetReader{GH: f.gh}).Movement(context.Background(), "owner/repo", "dev", root, sha); !errors.As(err, &target) {
			t.Fatalf("answer %q: %v", body, err)
		}
	}
	if _, err := (TargetReader{GH: f.gh}).Movement(context.Background(), "-owner/repo", "dev", root, tip); !errors.As(err, &target) {
		t.Fatalf("a repository that is neither a path nor owner/name: %v", err)
	}
}

// The whole path through the service with a forge repository: the lane lands a candidate, a merge
// is made outside it, and the next parent's check restates the landing's base from what the forge
// says about the branch.
func TestCRW403_ForgeLaneRestatesAfterOutsideMerge(t *testing.T) {
	w := newFx(t)
	f := newLBForge(t)
	reader := TargetReader{GH: f.gh}
	root, landed, outside := lbSHA(1), lbSHA(3), lbSHA(5)
	f.commit(t, landed, "Merge pull request #1 from team/a", root, lbSHA(2))
	f.commit(t, outside, "Merge pull request #2 from team/b", landed, lbSHA(4))
	f.setTip(t, root)
	first := w.heldOn(alpha, fxA, "head-a", fxBase)
	b := defaults()
	b.base, b.checks = root, runChecks("head-a", "success", 1, "dev-gate", "run-1")
	w.must(w.m.Check(w.ctx, first, b.actor, b.head, b.base, b.checks, b.review, b.required, reader))
	f.setTip(t, landed)
	w.must(w.m.Land(w.ctx, first, alpha.TaskID, landed, "", "merged by the forge", reader))
	f.setTip(t, outside)
	second := w.heldOn(beta, fxB, "head-b", fxBase)
	b.actor, b.head, b.base, b.checks = beta.TaskID, "head-b", outside, runChecks("head-b", "success", 1, "dev-gate", "run-1")
	got, err := w.m.Check(w.ctx, second, b.actor, b.head, b.base, b.checks, b.review, b.required, reader)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got["landingBaseRestated"] == nil || w.recorded(first) != outside {
		t.Fatalf("recorded base %s, answer %v", w.recorded(first), got["landingBaseRestated"])
	}
	list, _ := w.must(w.m.Turn(w.ctx, first))["baseRestatements"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["from"] != landed || !strings.Contains(list[0].(map[string]any)["evidence"].(string), "Merge pull request #2 from team/b") {
		t.Fatalf("restatements: %v", list)
	}
}
