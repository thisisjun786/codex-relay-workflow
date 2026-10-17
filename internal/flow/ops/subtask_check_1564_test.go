package ops

// CRW-1564: the scoped unit check of a split node's sub-task (crw.unitCheck(node, call, {sub: <id>})). Its prompt scopes the
// review to that sub-task alone — its own DONE CONDITION and EDIT REGIONS, earlier sub-tasks as committed context, later ones
// as NOT DUE YET, and none of the node's whole-change criteria — and the tool must provably have received that prompt
// (PromptSent) before the check is a judgment: a scoped answer a prompt-dropping profile produced is no_answer and holds the
// node, never a verdict.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// newUCWorldSplit is newUCWorld whose DESIGN splits node A into the sub-tasks one and two.
func newUCWorldSplit(t *testing.T) *ucWorld {
	t.Helper()
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
	w.relay.plan.Nodes[0].Criteria = []string{"the first thing holds", "the second thing holds"}
	w.relay.plan.Nodes[0].CriteriaDigest = ucCriteria
	d := designOf("A")
	d.Issues[0].SizeFit = "split"
	d.Issues[0].SizeReason = "regions_gt3"
	d.Issues[0].EditRegions = []string{"a/"}
	d.Issues[0].SubTasks = []SubTask{
		{ID: "one", Summary: "write one", EditRegions: []string{"a/one.txt"}, DoneCondition: "a/one.txt is written"},
		{ID: "two", Summary: "write two", EditRegions: []string{"a/two.txt"}, DoneCondition: "a/two.txt is written"},
	}
	w.ledgered(true, d)
	w.must("release", "plan-1", "A", "M")
	repo, first := newRepo(t)
	u := &ucWorld{world: w, repo: repo, start: first, wt: addWorktree(t, repo, "attempt1", first), checker: &scriptedChecker{}}
	w.relay.tip = first
	w.lane.d.Verifier = &fakeVerifier{out: VerifyOutcome{Pass: true, Command: CILocalCommand, Digest: strings.Repeat("d", 64), Summary: "pass", Failures: []string{}}}
	w.lane.d.UnitCheck = &UnitCheck{Checker: u.checker, Limit: time.Minute, Profile: func(mode string) (reviewadapter.Profile, error) {
		u.modes = append(u.modes, mode)
		return reviewadapter.JSONProfile("lab-"+mode, []string{"lab-check"}, "grok-4.7-medium", "medium"), nil
	}}
	return u
}

// scopedCheck runs crw.unitCheck of the executor call's pending change scoped to sub, and decodes the outcome.
func (u *ucWorld) scopedCheck(t *testing.T, ex CallRef, opts map[string]any) (UnitCheckOutcome, error) {
	t.Helper()
	if opts == nil {
		opts = map[string]any{}
	}
	if _, has := opts["round"]; !has {
		opts["round"] = 1
	}
	raw, err := u.world.op("unitCheck", "A", ex, opts)
	if err != nil {
		return UnitCheckOutcome{}, err
	}
	var out UnitCheckOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the answer is not a check outcome: %s %v", raw, err)
	}
	return out, nil
}

// TestScopedSubTaskPrompt: a check scoped to a sub-task judges that sub-task's change alone — the prompt names it, its done
// condition and edit regions, the earlier sub-tasks as committed context and the later ones as absent by design, and none of
// the node's whole-change criteria.
func TestScopedSubTaskPrompt(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"}); err != nil {
		t.Fatalf("the scoped check: %v", err)
	}
	if len(u.checker.got) != 1 {
		t.Fatalf("%d checks", len(u.checker.got))
	}
	p := u.checker.got[0].Prompt
	for _, want := range []string{"Review the uncommitted changes in this working tree.", "SUB-TASK 1 of 2 (one)", "DONE CONDITION: a/one.txt is written",
		"EDIT REGIONS: a/one.txt", "NOT DUE YET", "2. two"} {
		if !strings.Contains(p, want) {
			t.Errorf("the scoped prompt lacks %q:\n%s", want, p)
		}
	}
	for _, banned := range []string{"c1: the first thing holds", "c2: the second thing holds", "EARLIER SUB-TASKS", "of the whole change of the node"} {
		if strings.Contains(p, banned) {
			t.Errorf("the scoped prompt carries %q of another scope:\n%s", banned, p)
		}
	}
	// the second sub-task's check names the first as an earlier committed sub-task
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "two"}); err != nil {
		t.Fatalf("the second scoped check: %v", err)
	}
	p = u.checker.got[1].Prompt
	for _, want := range []string{"SUB-TASK 2 of 2 (two)", "DONE CONDITION: a/two.txt is written", "EDIT REGIONS: a/two.txt",
		"EARLIER SUB-TASKS", "1. one"} {
		if !strings.Contains(p, want) {
			t.Errorf("the second scoped prompt lacks %q:\n%s", want, p)
		}
	}
	for _, banned := range []string{"NOT DUE YET", "c1: the first thing holds"} {
		if strings.Contains(p, banned) {
			t.Errorf("the second scoped prompt carries %q:\n%s", banned, p)
		}
	}
}

// TestScopedCheckRejectsAnUnknownSubTask: a scope the node's DESIGN does not name, and a scope on a committed (whole-range)
// check, are input errors before any tool call.
func TestScopedCheckRejectsAnUnknownSubTask(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "three"}); err == nil {
		t.Fatal("a sub-task the DESIGN does not name was checked")
	}
	if len(u.checker.got) != 0 {
		t.Fatalf("%d checks ran for an unknown sub-task", len(u.checker.got))
	}
	// a committed check takes the whole range: a sub scope on it is an error
	head := gitIn(t, u.wt, "rev-parse", "HEAD")
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "one", "commit": head, "base": u.start}); err == nil {
		t.Fatal("a committed check was given a sub-task scope")
	}
	if len(u.checker.got) != 0 {
		t.Fatalf("%d checks ran for a committed scope", len(u.checker.got))
	}
}

// TestScopedCheckNeedsThePromptHanded: the scope lives in the prompt, so an answer from a tool that was not given it
// (PromptSent false) is no judgment: the row is no_answer and the node is held, never verified; a tool that was given it
// judges.
func TestScopedCheckNeedsThePromptHanded(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	// an answer from a tool that saw no prompt: no_answer and a hold, not a verdict
	out, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != reviewadapter.StateNoAnswer || out.Hold == "" || out.Verdict == reviewadapter.VerdictVerified {
		t.Fatalf("a scoped check the tool was not given the prompt of: %+v", out)
	}
	// the same scope with the prompt provably handed is a judgment
	u2 := newUCWorldSplit(t)
	ex2 := u2.build("c_ex1", "work\n")
	u2.checker.replies = []checkReply{{res: func() reviewadapter.Result {
		r := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
		r.PromptSent = true
		return r
	}()}}
	out, err = u2.scopedCheck(t, ex2, map[string]any{"sub": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Hold != "" {
		t.Fatalf("a scoped check whose tool saw the prompt: %+v", out)
	}
}

// TestScopedCheckFallsBackWithTheScope: a spent quota falls back only to a profile that also receives the scope prompt; a
// fallback that drops it holds, one that takes it judges.
func TestScopedCheckFallsBackWithTheScope(t *testing.T) {
	// fallback that omits the prompt: the scope was not handed, the node is held
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	u.lane.d.UnitCheck.Fallback = func(mode string) (reviewadapter.Profile, error) {
		return reviewadapter.JSONProfile("lab-fallback", []string{"lab-check"}, "gpt-6-astra", "high"), nil
	}
	quota := checkRecord(reviewadapter.StateQuota, "")
	dropped := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified) // PromptSent false
	u.checker.replies = []checkReply{{res: quota}, {res: dropped}}
	out, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != reviewadapter.StateNoAnswer || out.Hold == "" {
		t.Fatalf("a fallback that dropped the scope: %+v", out)
	}
	// fallback that receives it: a judgment
	u2 := newUCWorldSplit(t)
	ex2 := u2.build("c_ex1", "work\n")
	u2.lane.d.UnitCheck.Fallback = func(mode string) (reviewadapter.Profile, error) {
		return reviewadapter.JSONProfile("lab-fallback", []string{"lab-check"}, "gpt-6-astra", "high"), nil
	}
	handed := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
	handed.PromptSent = true
	u2.checker.replies = []checkReply{{res: quota}, {res: handed}}
	out, err = u2.scopedCheck(t, ex2, map[string]any{"sub": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Fallback == nil {
		t.Fatalf("a fallback that received the scope: %+v", out)
	}
}

// TestScopedCheckKeepsJournalShape: a scoped check is a call option: its row has no sub, scope or scope_handed member —
// nothing new is stored.
func TestScopedCheckKeepsJournalShape(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	u.checker.replies = []checkReply{{res: func() reviewadapter.Result {
		r := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
		r.PromptSent = true
		return r
	}()}}
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"}); err != nil {
		t.Fatal(err)
	}
	row := u.lastCheck()
	for _, banned := range []string{"sub", "scope", "scope_handed"} {
		if _, has := row.Data[banned]; has {
			t.Errorf("the check's row carries %s: %v", banned, row.Data[banned])
		}
	}
	if str(row.Data, "mode") != UnitCheckUncommitted {
		t.Errorf("a scoped check is uncommitted: %v", row.Data)
	}
}

// TestScopedCheckPicksTheSubtaskProfile: when the unit check's wiring offers a prompt-taking sub-task profile, a scoped
// check uses it instead of the mode profile — the shipped profiles take no prompt, so a scoped check must not run them.
func TestScopedCheckPicksTheSubtaskProfile(t *testing.T) {
	u := newUCWorldSplit(t)
	used := ""
	uc := u.lane.d.UnitCheck
	// the optional seam is selected through the struct so the test answers on the baseline too: a profile factory the check
	// calls is the proof the scoped path asks for a prompt-taking tool
	orig := uc.Profile
	uc.Profile = func(mode string) (reviewadapter.Profile, error) {
		used = "mode:" + mode
		return orig(mode)
	}
	// SubtaskProfile does not exist on the baseline; the reflection below sets it when it does
	setSubtaskProfile(uc, func() (reviewadapter.Profile, error) {
		used = "subtask"
		return reviewadapter.JSONProfile("lab-subtask", []string{"lab-check"}, "grok-4.7-medium", "medium"), nil
	})
	ex := u.build("c_ex1", "work\n")
	u.checker.replies = []checkReply{{res: func() reviewadapter.Result {
		r := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
		r.PromptSent = true
		return r
	}()}}
	if _, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"}); err != nil {
		t.Fatal(err)
	}
	if used != "subtask" {
		t.Fatalf("a scoped check used %q, want the sub-task profile", used)
	}
}

// TestScopedCheckRequiresASubtaskAwareTool: on a wiring that has no prompt-taking sub-task profile (the baseline's), a scoped
// check whose tool was not given the prompt is no judgment — it may not run as a verified whole-node check in sub disguise.
func TestScopedCheckRequiresASubtaskAwareTool(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_ex1", "work\n")
	// a verified answer from a tool that was not given the prompt: the scoped check holds, it does not pass
	out, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != reviewadapter.StateNoAnswer || out.Hold == "" {
		t.Fatalf("a scoped check on a prompt-dropping tool passed: %+v", out)
	}
}

// setSubtaskProfile installs a sub-task profile factory on the unit check when the seam exists (CRW-1564 adds it); on a
// baseline without the field it is a no-op, so the selection test fails on the behavior — the mode profile runs — not on a
// missing symbol.
func setSubtaskProfile(uc *UnitCheck, f func() (reviewadapter.Profile, error)) {
	v := reflect.ValueOf(uc).Elem().FieldByName("SubtaskProfile")
	if !v.IsValid() || !v.CanSet() {
		return
	}
	v.Set(reflect.ValueOf(f))
}

// TestScopedCheckCannotAcceptTheWholeNode (CRW-1564, the P1 the independent review reproduced): a scoped check is a
// judgment of one sub-task's change, never of the node's whole range — its row names no criteria digest, so the relay's
// criteria-binding gate and crw.accept cannot take it as the whole-node review. A split node is accepted only on a verified
// check of its committed range, while a matching negative judgment of the head still refuses.
func TestScopedCheckCannotAcceptTheWholeNode(t *testing.T) {
	u := newUCWorldSplit(t)
	write(t, u.wt, "a/one.txt", "one\n", 0o644)
	ex := u.build("c_first", "initial scaffolding\n")
	answer := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
	answer.PromptSent = true
	u.checker.replies = []checkReply{{res: answer}}
	out, err := u.scopedCheck(t, ex, map[string]any{"sub": "one"})
	if err != nil || out.State != reviewadapter.StateOK {
		t.Fatalf("scoped check: %+v %v", out, err)
	}
	scoped := u.lastCheck()
	var c CommitOutcome
	if err := json.Unmarshal(u.must("commit", "A", ex, []string{"a/", "a.txt"}), &c); err != nil {
		t.Fatal(err)
	}
	u.must("verify", "A", c.Commit)
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	if _, err := u.op("accept", "A", c.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_missing" || len(capture.got) != 0 {
		t.Fatalf("scoped evidence accepted the whole node: %v; requests=%d", err, len(capture.got))
	}
	if _, exists := scoped.Data["criteria_digest"]; !exists || dataStr(scoped, "criteria_digest") != "" {
		t.Errorf("scoped evidence claimed the node's criteria: %v", scoped.Data)
	}
	// the second sub-task commits and is verified; the check of the node's whole committed range is the acceptance basis
	write(t, u.wt, "a/two.txt", "two\n", 0o644)
	ex2 := u.build("c_second", "finished scaffolding\n")
	var all CommitOutcome
	if err := json.Unmarshal(u.must("commit", "A", ex2, []string{"a/", "a.txt"}), &all); err != nil {
		t.Fatal(err)
	}
	u.must("verify", "A", all.Commit)
	u.checkOnTree(u.snapshot())
	u.checker.replies[0].res.PromptSent = true
	decodeCheck(t, u.must("unitCheck", "A", ex2, map[string]any{"commit": all.Commit, "base": u.start}))
	whole := u.lastCheck()
	if _, err := u.op("accept", "A", all.Commit); err != nil {
		t.Fatal(err)
	}
	if len(capture.got) != 1 || capture.got[0].ReviewerCall != whole.CallID || dataStr(whole, "criteria_digest") != ucCriteria {
		t.Fatalf("acceptance did not use the full-range check: %+v, %v", capture.got, whole.Data)
	}
}

// TestScopedNegativeJudgmentStillRefuses: the scoped check's exemption from acceptance is only for a verified answer — a
// sub-task check that rules needs_changes on the head's tree is still the newest judgment and refuses, and never falls back
// to the older verified check of the whole change.
func TestScopedNegativeJudgmentStillRefuses(t *testing.T) {
	u := newUCWorldSplit(t)
	ex := u.build("c_first", "work\n")
	var c CommitOutcome
	if err := json.Unmarshal(u.must("commit", "A", ex, []string{"a/", "a.txt"}), &c); err != nil {
		t.Fatal(err)
	}
	u.must("verify", "A", c.Commit)
	// a verified check of the whole committed range
	u.checkOnTree(u.snapshot())
	u.checker.replies[0].res.PromptSent = true
	decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"commit": c.Commit, "base": u.start}))
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	// a scoped needs_changes on a pending tree equal to the head's (the same bytes: no new change) is a judgment of it
	ex2 := u.build("c_second", "work\n")
	bad := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictNeedsChanges, reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "a.txt", Line: 1, Text: "wrong"})
	bad.PromptSent = true
	u.checker.replies = []checkReply{{res: bad}}
	if _, err := u.scopedCheck(t, ex2, map[string]any{"sub": "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.op("accept", "A", c.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_not_verified" || len(capture.got) != 0 {
		t.Fatalf("a scoped needs_changes on the head's tree did not refuse: %v; requests=%d", err, len(capture.got))
	}
}
