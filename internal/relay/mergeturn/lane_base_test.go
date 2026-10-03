package mergeturn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-403: an out-of-lane merge moves the base branch past the base the lane's last landing
// recorded. These tests drive the real service against a real local git repository through the
// production TargetReader, so what the check observes is what git says about the branch.

// lbGit is a bare repository whose commits the tests write by hand: a commit with two parents
// is a merge commit, one with a single parent is an ordinary commit.
type lbGit struct {
	t    *testing.T
	dir  string
	tree string
	n    int
}

func newLBGit(t *testing.T) *lbGit {
	t.Helper()
	g := &lbGit{t: t, dir: filepath.Join(t.TempDir(), "dev.git")}
	if out, err := exec.Command("git", "init", "--bare", "-q", g.dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	g.tree = g.run("", "hash-object", "-w", "-t", "tree", "--stdin")
	return g
}

func (g *lbGit) run(stdin string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + g.dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	env := make([]string, 0)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a commit whose first parent is the first of parents; every call yields a new
// object name, because the author time counts up.
func (g *lbGit) commit(subject string, parents ...string) string {
	g.n++
	var b strings.Builder
	b.WriteString("tree " + g.tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	fmt.Fprintf(&b, "author Test <test@example.org> %d +0000\ncommitter Test <test@example.org> %d +0000\n\n%s\n", g.n, g.n, subject)
	return g.run(b.String(), "hash-object", "-w", "-t", "commit", "--stdin")
}

// merge is what a pull request merge leaves on the branch: the old tip and the branch's head.
func (g *lbGit) merge(tip string, number int) string {
	side := g.commit(fmt.Sprintf("work for pull request %d", number), tip)
	return g.commit(fmt.Sprintf("Merge pull request #%d from team/topic-%d", number, number), tip, side)
}

func (g *lbGit) setTip(sha string) { g.run("", "update-ref", "refs/heads/dev", sha) }

// lbLane is the merge lane over a real repository, with two parents bound to it.
type lbLane struct {
	*fx
	git    *lbGit
	reader TargetReader
}

func newLBLane(t *testing.T) *lbLane {
	t.Helper()
	return &lbLane{fx: newFx(t), git: newLBGit(t)}
}

// hold claims the lane for a candidate and answers the grant, as a parent does.
func (l *lbLane) hold(e registry.Endpoint, project, head string) string {
	l.t.Helper()
	turn := l.must(l.m.Request(l.ctx, l.git.dir, fxBase, project, e.TaskID, e.HostID, head, true))["turnId"].(string)
	l.answer(turn, e.TaskID)
	return turn
}

func (l *lbLane) asAlpha(head string) string { return l.hold(alpha, fxA, head) }
func (l *lbLane) asBeta(head string) string  { return l.hold(beta, fxB, head) }

func (l *lbLane) check(turn, actor, head, base string) (map[string]any, error) {
	b := defaults()
	b.actor, b.head, b.base, b.checks = actor, head, base, runChecks(head, "success", 1, "dev-gate", "run-1")
	return l.m.Check(l.ctx, turn, b.actor, b.head, b.base, b.checks, b.review, b.required, l.reader)
}

// landed carries one candidate through the lane: checked against the tip, merged by the caller's
// function, landed with the tip read afterwards.
func (l *lbLane) landed(turn, actor, head string, merge func(tip string) string) string {
	l.t.Helper()
	tip := l.git.run("", "rev-parse", "refs/heads/dev")
	l.must(l.check(turn, actor, head, tip))
	merged := merge(tip)
	l.git.setTip(merged)
	l.must(l.m.Land(l.ctx, turn, actor, merged, "", "merged by the forge", l.reader))
	return merged
}

func (l *lbLane) recordedBase(turn string) string {
	l.t.Helper()
	row, err := l.s.One(l.ctx, "SELECT observed_base_sha FROM merge_turns WHERE turn_id = ?", turn)
	if err != nil {
		l.t.Fatal(err)
	}
	return row.Text("observed_base_sha")
}

func (l *lbLane) restatements(turn string) []any {
	l.t.Helper()
	list, _ := l.must(l.m.Turn(l.ctx, turn))["baseRestatements"].([]any)
	return list
}

func (l *lbLane) restatementRows(turn string) int {
	l.t.Helper()
	rows, err := l.s.All(l.ctx, "SELECT entry_id FROM merge_turn_ledger WHERE turn_id = ? AND evidence_kind = 'landing_base_restated'", turn)
	if err != nil {
		l.t.Fatal(err)
	}
	return len(rows)
}

// setup is the incident: parent A lands in the lane, then dev moves by merges made outside it.
// It returns A's landing turn, the base that landing recorded, and the dev tip afterwards.
func (l *lbLane) setup(outside func(tip string) string) (landing, recorded, tip string) {
	l.t.Helper()
	root := l.git.commit("root")
	l.git.setTip(root)
	landing = l.asAlpha("head-a")
	recorded = l.landed(landing, alpha.TaskID, "head-a", func(tip string) string { return l.git.merge(tip, 1) })
	if got := l.recordedBase(landing); got != recorded {
		l.t.Fatalf("the landing recorded %s, want %s", got, recorded)
	}
	tip = outside(recorded)
	l.git.setTip(tip)
	return landing, recorded, tip
}

func refusalDetail(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("the check was not refused")
	}
	if reasonOf(err) != "merge_currency_stale" {
		t.Fatalf("refused %s: %v", reasonOf(err), err)
	}
	return err.Error()
}

// A merge outside the lane does not stop the next parent: the check sees dev moved by a merge
// commit no landing recorded, records the base again and goes on.
func TestCRW403_OutOfLaneMergeDoesNotBlockTheNextCheck(t *testing.T) {
	l := newLBLane(t)
	landing, recorded, tip := l.setup(func(tip string) string { return l.git.merge(tip, 2) })

	second := l.asBeta("head-b")
	got, err := l.check(second, beta.TaskID, "head-b", tip)
	if err != nil {
		t.Fatalf("the next parent's check was refused: %v", err)
	}
	if got["state"] != Merging {
		t.Fatalf("state %v, want merging", got["state"])
	}
	if base := l.recordedBase(landing); base != tip {
		t.Fatalf("the landing's recorded base is %s, want the dev tip %s", base, tip)
	}
	list := l.restatements(landing)
	if len(list) != 1 {
		t.Fatalf("restatements: %v", list)
	}
	entry := list[0].(map[string]any)
	if entry["from"] != recorded || entry["to"] != tip || entry["actorTaskId"] != beta.TaskID {
		t.Fatalf("restatement %v: the old value must stay beside the new one", entry)
	}
	evidence, _ := entry["evidence"].(string)
	if !strings.Contains(evidence, "automatic") || !strings.Contains(evidence, tip) || !strings.Contains(evidence, "Merge pull request #2") {
		t.Fatalf("the evidence does not say what was read: %q", evidence)
	}
	reported, _ := got["landingBaseRestated"].(map[string]any)
	if reported == nil || reported["turnId"] != landing || reported["from"] != recorded || reported["to"] != tip {
		t.Fatalf("the check's answer does not report the restatement: %v", got["landingBaseRestated"])
	}
	// The lane carries on from the restated base: B lands, and the next parent's check needs no
	// further restatement, of the first landing or of B's.
	merged := l.git.merge(tip, 5)
	l.git.setTip(merged)
	l.must(l.m.Land(l.ctx, second, beta.TaskID, merged, "", "merged by the forge", l.reader))
	third := l.asAlpha("head-c")
	if _, err := l.check(third, alpha.TaskID, "head-c", merged); err != nil {
		t.Fatalf("the check after the restated landing was refused: %v", err)
	}
	if n, m := l.restatementRows(landing), l.restatementRows(second); n != 1 || m != 0 {
		t.Fatalf("restatement rows: first landing %d, second landing %d; want 1 and 0", n, m)
	}
}

// A recorded base stored in capitals is the same commit (object names compare ignoring case), so
// the move from it is read and confirmed like any other.
func TestCRW403_ARecordedBaseInCapitalsIsTheSameCommit(t *testing.T) {
	l := newLBLane(t)
	landing, _, tip := l.setup(func(tip string) string { return l.git.merge(tip, 2) })
	l.exec("UPDATE merge_turns SET observed_base_sha = upper(observed_base_sha) WHERE turn_id = ?", landing)
	second := l.asBeta("head-b")
	if _, err := l.check(second, beta.TaskID, "head-b", tip); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got := l.recordedBase(landing); got != tip || l.restatementRows(landing) != 1 {
		t.Fatalf("recorded base %s, %d restatements", got, l.restatementRows(landing))
	}
}

// Two merges outside the lane since the landing are one restatement covering both.
func TestCRW403_SeveralOutOfLaneMergesAreOneRestatement(t *testing.T) {
	l := newLBLane(t)
	landing, recorded, tip := l.setup(func(tip string) string { return l.git.merge(l.git.merge(tip, 2), 3) })
	second := l.asBeta("head-b")
	if _, err := l.check(second, beta.TaskID, "head-b", tip); err != nil {
		t.Fatalf("refused: %v", err)
	}
	list := l.restatements(landing)
	if len(list) != 1 || list[0].(map[string]any)["from"] != recorded {
		t.Fatalf("restatements: %v", list)
	}
	evidence, _ := list[0].(map[string]any)["evidence"].(string)
	for _, want := range []string{"Merge pull request #2", "Merge pull request #3"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("the evidence misses %q: %q", want, evidence)
		}
	}
}

// What the refusal says when nothing was restated: the cause, and the command that repairs it.
func assertRecoveryNamed(t *testing.T, detail, landing, holder string) {
	t.Helper()
	for _, want := range []string{"out-of-lane merge", "merge-turn-restate-base --turn " + landing + " --actor " + holder, "--evidence"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the refusal does not contain %q: %s", want, detail)
		}
	}
}

// A commit that is not a merge is not known to be a merged pull request, so nothing is restated.
func TestCRW403_ADirectCommitIsNotConfirmedAsAMerge(t *testing.T) {
	l := newLBLane(t)
	landing, recorded, tip := l.setup(func(tip string) string { return l.git.commit("direct commit on dev", tip) })
	second := l.asBeta("head-b")
	_, err := l.check(second, beta.TaskID, "head-b", tip)
	detail := refusalDetail(t, err)
	assertRecoveryNamed(t, detail, landing, alpha.TaskID)
	if !strings.Contains(detail, "one parent") {
		t.Fatalf("the refusal does not say why the commit is not confirmed: %s", detail)
	}
	if got := l.recordedBase(landing); got != recorded {
		t.Fatalf("the recorded base changed to %s without a confirmed cause", got)
	}
	if n := l.restatementRows(landing); n != 0 {
		t.Fatalf("%d restatement rows, want none", n)
	}
}

// A branch that was rewritten no longer passes through the recorded base.
func TestCRW403_ARewrittenBranchIsNotConfirmed(t *testing.T) {
	l := newLBLane(t)
	var root string
	landing, recorded, tip := l.setup(func(string) string {
		root = l.git.run("", "rev-list", "--max-parents=0", "refs/heads/dev")
		return l.git.merge(l.git.commit("replacement history", root), 9)
	})
	second := l.asBeta("head-b")
	_, err := l.check(second, beta.TaskID, "head-b", tip)
	detail := refusalDetail(t, err)
	assertRecoveryNamed(t, detail, landing, alpha.TaskID)
	if got := l.recordedBase(landing); got != recorded {
		t.Fatalf("the recorded base changed to %s", got)
	}
	if n := l.restatementRows(landing); n != 0 {
		t.Fatalf("%d restatement rows, want none", n)
	}
}

// A gap longer than the lane reads (more than 32 merges) is not read to the end, so it is not
// confirmed either.
func TestCRW403_ALongGapIsNotConfirmed(t *testing.T) {
	l := newLBLane(t)
	landing, recorded, tip := l.setup(func(tip string) string {
		for i := 0; i < 40; i++ {
			tip = l.git.merge(tip, 100+i)
		}
		return tip
	})
	second := l.asBeta("head-b")
	_, err := l.check(second, beta.TaskID, "head-b", tip)
	assertRecoveryNamed(t, refusalDetail(t, err), landing, alpha.TaskID)
	if got := l.recordedBase(landing); got != recorded {
		t.Fatalf("the recorded base changed to %s", got)
	}
}

// A commit that is itself a recorded landing of this lane is the lane's own, not an outside one:
// the legacy shape where a landing recorded the base from before its own merge stays a manual
// correction. The landed commit is matched whatever the case it was stated in.
func TestCRW403_ALandingOfTheLaneIsNotAnOutsideMerge(t *testing.T) {
	l := newLBLane(t)
	root := l.git.commit("root")
	l.git.setTip(root)
	landing := l.asAlpha("head-a")
	merged := l.landed(landing, alpha.TaskID, "head-a", func(tip string) string { return l.git.merge(tip, 1) })
	l.exec("UPDATE merge_turns SET observed_base_sha = ?, landed_sha = upper(landed_sha) WHERE turn_id = ?", root, landing)
	second := l.asBeta("head-b")
	_, err := l.check(second, beta.TaskID, "head-b", merged)
	assertRecoveryNamed(t, refusalDetail(t, err), landing, alpha.TaskID)
	if got := l.recordedBase(landing); got != root {
		t.Fatalf("the recorded base changed to %s", got)
	}
	if n := l.restatementRows(landing); n != 0 {
		t.Fatalf("%d restatement rows, want none", n)
	}
	// The manual command still repairs it, and then the check goes through.
	l.must(l.m.RestateBase(l.ctx, landing, alpha.TaskID, "", "the landing recorded the base from before its own merge", l.reader))
	if _, err := l.check(second, beta.TaskID, "head-b", merged); err != nil {
		t.Fatalf("the check after the manual restatement: %v", err)
	}
}

// A commit that git is told to show as a merge (a replace ref) is still the direct commit it was
// stored as, so it is not confirmed.
func TestCRW403_AReplacedTipIsNotConfirmed(t *testing.T) {
	l := newLBLane(t)
	landing, recorded, tip := l.setup(func(tip string) string {
		side := l.git.commit("side", tip)
		direct := l.git.commit("direct commit on dev", tip)
		fake := l.git.commit("Merge pull request #9 from fake/y", tip, side)
		l.git.run("", "update-ref", "refs/replace/"+direct, fake)
		return direct
	})
	second := l.asBeta("head-b")
	_, err := l.check(second, beta.TaskID, "head-b", tip)
	assertRecoveryNamed(t, refusalDetail(t, err), landing, alpha.TaskID)
	if got := l.recordedBase(landing); got != recorded {
		t.Fatalf("the recorded base changed to %s", got)
	}
	if n := l.restatementRows(landing); n != 0 {
		t.Fatalf("%d restatement rows, want none", n)
	}
}
