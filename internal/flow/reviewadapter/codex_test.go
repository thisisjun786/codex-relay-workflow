package reviewadapter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// codexWorktree is the worktree the fixture's review ran in (/scratch/sion-eval/rv-cmp-wt): Codex prints absolute paths under it.
const codexWorktree = "/scratch/sion-eval/rv-cmp-wt"

// codexOneFinding is a real `codex review --base <sha>` stdout with a single finding (the parent's probe of 10-16 on a detached head
// in /scratch/lt/codex-base-probe, inputs/codex-review-sample-one-finding.txt): its list header is the singular "Review comment:".
const codexOneFinding = "유일한 변경이 add 함수의 연산을 덧셈에서 뺄셈으로 바꾸어 기존 동작을 깨뜨립니다.\n" +
	"\n" +
	"Review comment:\n" +
	"\n" +
	"- [P2] add 함수의 덧셈 연산을 복원하세요 — /scratch/lt/codex-base-probe/m.go:3-3\n" +
	"  `a + b`를 `a - b`로 변경하여 두 번째 인자가 0이 아닌 경우 합 대신 차를 반환합니다. 예를 들어 `add(2, 3)`은 기존의 `5` 대신 `-1`을 반환합니다. 함수 이름과 기존 동작에 맞게 덧셈 연산을 복원해야 합니다.\n"

// codexClean is a clean review's stdout: a summary that says what was reviewed, and no list.
const codexClean = "The change renames a local variable and keeps every caller's behaviour; no issue needs a change.\n"

// fixedPrint is a fingerprint that never changes, for a worktree that need not exist (the fixture's).
func fixedPrint(context.Context, string) (string, error) { return "fp", nil }

// codexCheck runs p over a fake call in worktree.
func codexCheck(t *testing.T, p Profile, worktree string, f fakeRun) Result {
	t.Helper()
	req := Request{Profile: p, Worktree: worktree, Prompt: "review the uncommitted changes", Schema: AnswerSchema, Limit: time.Minute, GuideRev: "guide-r1", Base: rangeBase}
	res, err := Adapter{Run: f.run, Fingerprint: fixedPrint}.Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

// TestCodexReviewProfileReadsTheFindingsList (CRW-1517): the Codex review profile reads `codex review`'s prose: the summary, then
// a "Full review comments:" list (or "Review comment:" for a single finding) whose items are "- [P<n>] title — file:line-line"
// followed by their body. Paths under the worktree become relative to it, and the record names the configured model and effort
// and the gpt family.
func TestCodexReviewProfileReadsTheFindingsList(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "codex-review-findings.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the fixture's three findings", func(t *testing.T) {
		res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), codexWorktree, fakeRun{stdout: string(fixture)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges || res.Model != "gpt-6-astra" || res.Effort != "high" || res.EngineFamily != "gpt" {
			t.Fatalf("result = verdict %q, model %q, effort %q, family %q; want needs_changes, gpt-6-astra, high, gpt", res.Verdict, res.Model, res.Effort, res.EngineFamily)
		}
		want := []struct {
			line        int
			title, body string
		}{
			{182, "홈 상위 경로 검사에서 인자 순서를 복원하세요", "`below(home, p)`로 검사해야 합니다."},
			{169, "경로 포함 검사에 디렉터리 구분자를 유지하세요", "`dir+\"/\"` 접두사를 검사해야 합니다."},
			{160, "홈 경로 해석 실패 시 정리된 원래 경로를 반환하세요", "기존 `filepath.Clean(path)` 대체 반환을 유지해야 합니다."},
		}
		if len(res.Findings) != len(want) {
			t.Fatalf("findings = %+v; want %d", res.Findings, len(want))
		}
		for i, w := range want {
			f := res.Findings[i]
			if f.Severity != "P2" || f.File != "internal/sandboxroot/sandboxroot.go" || f.Line != w.line {
				t.Fatalf("finding %d = %+v; want P2 at internal/sandboxroot/sandboxroot.go:%d", i, f, w.line)
			}
			if !strings.HasPrefix(f.Text, w.title+": ") || !strings.HasSuffix(f.Text, w.body) || strings.Contains(f.Text, "\n") || strings.Contains(f.Text, "  ") {
				t.Fatalf("finding %d text = %q; want %q, its body joined on one line, ending %q", i, f.Text, w.title+": ...", w.body)
			}
			if f.ID != FindingID(f.File, f.Text) {
				t.Fatalf("finding %d id = %s; want FindingID of its relative file and text", i, f.ID)
			}
		}
	})
	t.Run("the singular header of a single finding", func(t *testing.T) {
		res := codexCheck(t, CodexReviewCommittedProfile(CodexReviewConfig{}), "/scratch/lt/codex-base-probe", fakeRun{stdout: codexOneFinding})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges || len(res.Findings) != 1 {
			t.Fatalf("result = %+v; want needs_changes with one finding", res)
		}
		f := res.Findings[0]
		if f.Severity != "P2" || f.File != "m.go" || f.Line != 3 || !strings.HasPrefix(f.Text, "add 함수의 덧셈 연산을 복원하세요: `a + b`를 `a - b`로") {
			t.Fatalf("finding = %+v; want P2 at m.go:3 with its title and body", f)
		}
	})
	t.Run("an unindented body, a title with the dash, a minor finding, a path outside the worktree", func(t *testing.T) {
		out := "One finding blocks.\n\nFull review comments:\n\n" +
			"- [P1] Keep the guard — it stops the loop — /wt/a/b.go:12-14\n" +
			"The guard was dropped.\n" +
			"It loops forever.\n\n" +
			"- [P3] Name the helper — /elsewhere/c.go:3\n" +
			"  A nit.\n" +
			"- [P3] Relative stays — d/e.go:7\n"
		res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: out})
		wantState(t, res, StateOK)
		want := []Finding{
			{Severity: "P1", File: "a/b.go", Line: 12, Text: "Keep the guard — it stops the loop: The guard was dropped. It loops forever."},
			{Severity: "P3", File: "/elsewhere/c.go", Line: 3, Text: "Name the helper: A nit."},
			{Severity: "P3", File: "d/e.go", Line: 7, Text: "Relative stays"},
		}
		for i := range want {
			want[i].ID = FindingID(want[i].File, want[i].Text)
		}
		if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
			t.Fatalf("result = %s %+v\nwant needs_changes %+v", res.Verdict, res.Findings, want)
		}
	})
	t.Run("minor findings alone are verified", func(t *testing.T) {
		out := "Only a nit.\n\nReview comment:\n\n- [P3] Name the helper — /wt/c.go:3\n  A nit.\n"
		res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: out})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].File != "c.go" {
			t.Fatalf("result = %s %+v; want verified with the one P3 finding at c.go", res.Verdict, res.Findings)
		}
	})
	t.Run("a worktree named through a symlink", func(t *testing.T) {
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "wt")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		out := "One finding.\n\nReview comment:\n\n- [P2] Fix it — " + filepath.Join(resolved, "x", "y.go") + ":9-9\n  Body.\n"
		res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), link, fakeRun{stdout: out})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 || res.Findings[0].File != "x/y.go" {
			t.Fatalf("findings = %+v; want one at x/y.go", res.Findings)
		}
	})
}

// TestCodexReviewCleanAnswerIsClean (CRW-1517): a review with no list is a clean answer, however short its summary; a blank stdout is no answer; a list the
// reader cannot read whole is not an answer (invalid), never a clean one.
func TestCodexReviewCleanAnswerIsClean(t *testing.T) {
	summary := "The change only renames a local variable and keeps every caller's behaviour; I found no issue that needs a change.\n"
	res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: summary})
	wantState(t, res, StateOK)
	if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
		t.Fatalf("a summary alone = %s %+v; want verified, no findings", res.Verdict, res.Findings)
	}
	wantState(t, codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: " \n\n"}), StateNoAnswer)
	for _, clean := range []string{
		"검토 결과 문제가 없습니다.",
		"변경 사항에서 문제를 확인하지 못했습니다. 읽기 전용 환경이라 테스트는 실행되지 않았습니다.",
		"The change removes a dead branch, so the loader that did not read the variable now reads it; I did not run the tests, and found no issue.",
		// a short summary, or one in a language written without spaces, is a clean answer: the reader counts no words
		"変更はローカル変数の名前を変えるだけで、呼び出し元の動作は変わらず、修正が必要な問題は見つかりませんでした。",
		"Clean.",
		"No issues.",
	} {
		res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: clean + "\n"})
		if res.State != StateOK || res.Verdict != VerdictVerified {
			t.Fatalf("the clean summary %q = %s %s (%s); want ok verified", clean, res.State, res.Verdict, res.Detail)
		}
	}
	// a summary that describes the bug it found in review code is read: the finding blocks
	quoting := "리뷰를 완료하지 못한 응답도 검증 성공으로 기록하는 문제가 있습니다.\n\nReview comment:\n\n- [P1] 미완료 응답을 verified로 처리하지 마세요 — /wt/codex.go:192-195\n  본문.\n"
	if res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: quoting}); res.State != StateOK || res.Verdict != VerdictNeedsChanges {
		t.Fatalf("a blocking review whose summary quotes the words = %s %s (%s); want ok needs_changes", res.State, res.Verdict, res.Detail)
	}

	item := "- [P2] Fix it — /wt/a.go:3\n  Body.\n"
	for name, out := range map[string]string{
		"a list header with no item":                   "Summary.\n\nFull review comments:\n\n",
		"a singular header with no item":               "Summary.\n\nReview comment:\n",
		"an item without a location":                   "Summary.\n\nFull review comments:\n\n- [P2] Fix it\n  Body.\n",
		"a location that is not file:line":             "Summary.\n\nFull review comments:\n\n- [P2] Fix it — /wt/a.go\n  Body.\n",
		"a severity outside P0-P3":                     "Summary.\n\nFull review comments:\n\n- [P5] Fix it — /wt/a.go:3\n  Body.\n",
		"a later item outside P0-P3":                   "Summary.\n\nFull review comments:\n\n" + item + "- [high] Also — /wt/b.go:4\n",
		"a second list header":                         "Summary.\n\nFull review comments:\n\n" + item + "\nReview comment:\n\n" + item,
		"an item before the header":                    "Summary.\n" + item + "\nFull review comments:\n\n" + item,
		"an item and no header":                        "Summary.\n\n" + item,
		"text between the header and items":            "Summary.\n\nFull review comments:\nI also looked at b.go.\n" + item,
		"an item whose title is blank":                 "Summary.\n\nFull review comments:\n\n- [P2]  — /wt/a.go:3\n  Body.\n",
		"an item whose location has no file":           "Summary.\n\nFull review comments:\n\n- [P2] Fix it — :3\n  Body.\n",
		"a summary that says it could not review":      "I could not review the changes because the repository was inaccessible.\n",
		"a summary that says nothing was reviewed":     "There are no uncommitted changes to review.\n",
		"a Korean summary that says it did not review": "저장소에 접근할 수 없어 변경 사항을 검토하지 못했습니다.\n",
		"a Korean summary that did not finish":         "시간 제한으로 리뷰를 완료하지 못했습니다.\n",
		"a summary that could not complete the review": "I could not complete the review because repository access failed.\n",
		"a wording broken across lines":                "I was unable to\nreview the change in time.\n",
		"a Korean summary that could not finish":       "시간 제한으로 검토를 완료할 수 없었습니다.\n",
		"a Korean summary whose review did not end":    "리뷰가 완료되지 않았습니다.\n",
		"a partial review over a minor finding":        "I only reviewed a.go.\n\nReview comment:\n\n- [P3] Name it — /wt/a.go:3\n  A nit.\n",
	} {
		t.Run(name, func(t *testing.T) {
			wantState(t, codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: out}), StateInvalid)
		})
	}
}

// TestCodexReviewFallbackIsNoCleanReview (CRW-1517): Codex 0.154.0 exits 0 and prints a text of its own when the review gave no
// answer (review_format.rs, REVIEW_FALLBACK_MESSAGE: no overall explanation and no finding) or was interrupted (tasks/review.rs,
// exit_review_mode with no output); neither is a clean review, nor a text with no list whose summary says the review was aborted:
// each is invalid. The real one-finding and many-findings samples still read whole.
func TestCodexReviewFallbackIsNoCleanReview(t *testing.T) {
	for name, out := range map[string]string{
		"Codex's text for a review with no answer":        "Reviewer failed to output a response.\n",
		"Codex's text for an interrupted review":          "Review was interrupted. Please re-run /review and wait for it to complete.\n",
		"the no-answer text padded":                       "\n  Reviewer failed to output a response.  \n\n",
		"a review aborted before it inspected a file":     "Review aborted before inspecting any files.\n",
		"a review that was interrupted":                   "The review was interrupted before it finished.\n",
		"a Korean review that was stopped":                "리뷰가 중단되었습니다.\n",
		"the no-answer text over a minor finding's list":  "Reviewer failed to output a response.\n\nReview comment:\n\n- [P3] Name it — /wt/a.go:3\n  A nit.\n",
		"an aborted review over a minor finding's list":   "Review aborted after a.go.\n\nReview comment:\n\n- [P3] Name it — /wt/a.go:3\n  A nit.\n",
		"the interrupted text with a line after it":       "Review was interrupted. Please re-run /review and wait for it to complete.\nnothing more\n",
		"the no-answer text inside a longer clean answer": "The change looks fine. Reviewer failed to output a response.\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, p := range []Profile{CodexReviewProfile(CodexReviewConfig{}), CodexReviewCommittedProfile(CodexReviewConfig{})} {
				res := codexCheck(t, p, "/wt", fakeRun{stdout: out})
				if res.State != StateInvalid || res.Verdict != "" || len(res.Findings) != 0 {
					t.Fatalf("%s over %q = %s %s %+v (%s); want invalid with no verdict", p.Name, out, res.State, res.Verdict, res.Findings, res.Detail)
				}
			}
		})
	}
	// a blocking finding needs changes whatever the summary says
	blocking := "Reviewer failed to output a response.\n\nReview comment:\n\n- [P1] Keep the guard — /wt/a.go:3\n  Body.\n"
	if res := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{stdout: blocking}); res.State != StateOK || res.Verdict != VerdictNeedsChanges {
		t.Fatalf("a blocking review = %s %s (%s); want ok needs_changes", res.State, res.Verdict, res.Detail)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "codex-review-findings.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		name, worktree, out string
		findings            int
	}{{"many findings", codexWorktree, string(fixture), 3}, {"one finding", "/scratch/lt/codex-base-probe", codexOneFinding, 1}} {
		res := codexCheck(t, CodexReviewCommittedProfile(CodexReviewConfig{}), s.worktree, fakeRun{stdout: s.out})
		if res.State != StateOK || res.Verdict != VerdictNeedsChanges || len(res.Findings) != s.findings {
			t.Fatalf("the real %s sample = %s %s %d findings (%s); want ok needs_changes with %d", s.name, res.State, res.Verdict, len(res.Findings), res.Detail, s.findings)
		}
	}
}

// TestCodexReviewFamilyIsTheModels (CRW-1517): the profiles name the family of the model they run (execution.ModelFamily, the way
// the adapter reads a profile with no family), never gpt for any model: a Claude model configured for the review records claude, so
// the acceptance refuses it over a Claude author. Validate refuses a model whose family cannot be told.
func TestCodexReviewFamilyIsTheModels(t *testing.T) {
	for model, family := range map[string]string{"": "gpt", "gpt-6.1-sol": "gpt", "openai/gpt-5.6-sol": "gpt", "anthropic/claude-sonnet-5": "claude", "Claude-Opus-5-5": "claude", "o9": "o9"} {
		cfg := CodexReviewConfig{Model: model}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v", cfg, err)
		}
		for _, p := range []Profile{CodexReviewProfile(cfg), CodexReviewCommittedProfile(cfg)} {
			if p.Family != family {
				t.Fatalf("%s with model %q names family %q; want %q", p.Name, model, p.Family, family)
			}
			if res := codexCheck(t, p, "/wt", fakeRun{stdout: codexClean}); res.State != StateOK || res.EngineFamily != family {
				t.Fatalf("%s with model %q records %s family %q; want ok %q", p.Name, model, res.State, res.EngineFamily, family)
			}
		}
	}
	for _, model := range []string{"-astra", "openai/", "openai/-x"} {
		if err := (CodexReviewConfig{Model: model}).Validate(); err == nil || !strings.Contains(err.Error(), "family") {
			t.Fatalf("Validate(model %q) = %v; want a refusal naming the family", model, err)
		}
	}
}

// TestCodexReviewArgvPerMode (CRW-1517): the uncommitted profile runs `codex review --uncommitted` and the committed one
// `codex review ... --base <base>`, each with the model (as model and review_model, which Codex's review task prefers), effort and a
// read-only sandbox as -c overrides and no prompt; the
// configuration replaces the model and effort, Validate refuses a value that would break the TOML string or is no effort word, and a
// usage-limit text on a failed call is quota.
func TestCodexReviewArgvPerMode(t *testing.T) {
	overrides := func(model, effort string) []string {
		return []string{"-c", `model="` + model + `"`, "-c", `review_model="` + model + `"`, "-c", `model_reasoning_effort="` + effort + `"`, "-c", `sandbox_mode="read-only"`}
	}
	argv := func(t *testing.T, p Profile, req Request) []string {
		t.Helper()
		args, sent, err := toolArgs(p, req)
		if err != nil {
			t.Fatalf("toolArgs: %v", err)
		}
		if sent {
			t.Fatalf("profile %s sends the prompt; want none", p.Name)
		}
		return append(append([]string{}, p.Argv...), args...)
	}
	req := Request{Prompt: "review the change", Base: "B"}

	p := CodexReviewProfile(CodexReviewConfig{})
	if want := append([]string{"codex", "review", "--uncommitted"}, overrides("gpt-6-astra", "high")...); p.Name != "codex-review" || !slices.Equal(argv(t, p, req), want) {
		t.Fatalf("uncommitted %s argv = %q; want codex-review %q", p.Name, argv(t, p, req), want)
	}
	c := CodexReviewCommittedProfile(CodexReviewConfig{})
	if want := append(append([]string{"codex", "review"}, overrides("gpt-6-astra", "high")...), "--base", "B"); c.Name != "codex-review-committed" || !slices.Equal(argv(t, c, req), want) {
		t.Fatalf("committed %s argv = %q; want codex-review-committed %q", c.Name, argv(t, c, req), want)
	}
	for _, p := range []Profile{p, c} {
		if p.Model != "gpt-6-astra" || p.Effort != "high" || p.Family != "gpt" {
			t.Fatalf("profile %s = model %q effort %q family %q; want gpt-6-astra, high, gpt", p.Name, p.Model, p.Effort, p.Family)
		}
		res := codexCheck(t, p, "/wt", fakeRun{stdout: codexClean})
		if res.State != StateOK || res.PromptSent || res.PromptDigest != digest("") {
			t.Fatalf("profile %s record = state %s prompt_sent %v digest %s; want ok, false, the empty string's digest", p.Name, res.State, res.PromptSent, res.PromptDigest)
		}
	}

	ran := false
	run := func(context.Context, Profile, Request) ([]byte, []byte, int, error) {
		ran = true
		return []byte(codexClean), nil, 0, nil
	}
	blank := Request{Profile: c, Worktree: "/wt", Prompt: "p", Schema: AnswerSchema, Limit: time.Minute, GuideRev: "guide-r1"}
	if _, err := (Adapter{Run: run, Fingerprint: fixedPrint}).Check(t.Context(), blank); err == nil || !strings.Contains(err.Error(), "base") || ran {
		t.Fatalf("a committed check with a blank base: err %v, ran %v; want a refusal naming the base and no call", err, ran)
	}

	cfg := CodexReviewConfig{Model: "gpt-6.1-sol", Effort: "medium"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v", cfg, err)
	}
	p, c = CodexReviewProfile(cfg), CodexReviewCommittedProfile(cfg)
	if want := append([]string{"codex", "review", "--uncommitted"}, overrides("gpt-6.1-sol", "medium")...); !slices.Equal(argv(t, p, req), want) || p.Model != "gpt-6.1-sol" || p.Effort != "medium" {
		t.Fatalf("configured uncommitted = %q model %q effort %q; want %q", argv(t, p, req), p.Model, p.Effort, want)
	}
	if want := append(append([]string{"codex", "review"}, overrides("gpt-6.1-sol", "medium")...), "--base", "B"); !slices.Equal(argv(t, c, req), want) {
		t.Fatalf("configured committed = %q; want %q", argv(t, c, req), want)
	}
	if res := codexCheck(t, p, "/wt", fakeRun{stdout: codexClean}); res.EngineFamily != "gpt" || res.Model != "gpt-6.1-sol" {
		t.Fatalf("configured record = family %q model %q; want gpt, gpt-6.1-sol", res.EngineFamily, res.Model)
	}
	if p := CodexReviewProfile(CodexReviewConfig{Model: " gpt-6.1-sol ", Effort: " low "}); p.Model != "gpt-6.1-sol" || p.Effort != "low" || !slices.Contains(p.Argv, `model="gpt-6.1-sol"`) {
		t.Fatalf("a padded configuration = model %q effort %q argv %q; want the trimmed values", p.Model, p.Effort, p.Argv)
	}
	for _, ok := range []CodexReviewConfig{{}, {Model: "  ", Effort: ""}, {Model: "gpt-6.1-sol"}, {Effort: "xhigh"}} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v; want nil", ok, err)
		}
	}
	for _, bad := range []CodexReviewConfig{{Model: `o9"`}, {Model: "o 9"}, {Model: `o\9`}, {Model: "o'9"}, {Effort: "High!"}, {Effort: "High"}, {Effort: "x high"},
		{Model: "-astra"}, {Model: "openai/"}, {Model: "openai/-x"}} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("Validate(%+v) = nil; want a refusal", bad)
		}
	}

	quota := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{exit: 1, stderr: "ERROR: You've hit your usage limit. Upgrade to Pro or try again later.\n"})
	wantState(t, quota, StateQuota)
	// the stderr of a failed call is Codex's session transcript: words a review of this code quotes are not quota texts
	failed := codexCheck(t, CodexReviewProfile(CodexReviewConfig{}), "/wt", fakeRun{exit: 1, stderr: "StateQuota at adapter.go:429 quota\nERROR: stream disconnected\n"})
	wantState(t, failed, StateToolFailed)
}
