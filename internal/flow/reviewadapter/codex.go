package reviewadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// The model and effort a Codex review runs when the configuration leaves them blank. The profiles name the family of the model they
// run (codexFamily), so a configured model of another vendor records its own family, never gpt.
const (
	codexReviewModel  = "gpt-6-astra"
	codexReviewEffort = "high"
)

// CodexReviewConfig is the model and effort of the Codex review profiles (the lane reads them from CRW_UNIT_CHECK_MODEL and
// CRW_UNIT_CHECK_EFFORT). A blank member takes its default (codexReviewModel, codexReviewEffort); values are trimmed.
type CodexReviewConfig struct {
	Model, Effort string
}

// codexEffortWord is the shape of an effort: one lower-case word. There is no closed list: codex refuses an effort it does not know
// itself, and that call ends tool_failed and is recorded.
var codexEffortWord = regexp.MustCompile(`^[a-z]+$`)

// Validate refuses a configuration the profiles cannot pass to codex: a set model is given inside a TOML string (model="..."), so it
// may hold no space, quote, backslash or control character, and its family must be one execution.ModelFamily can tell (the record
// names it as provenance, not as an author-family comparison); a set effort must be one lower-case word.
func (c CodexReviewConfig) Validate() error {
	m := strings.TrimSpace(c.Model)
	if m != "" && strings.ContainsFunc(m, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\'' || r == '\\'
	}) {
		return fmt.Errorf("the codex review model %q holds a space, a quote, a backslash or a control character", c.Model)
	}
	if m != "" && codexFamily(m) == "" {
		return fmt.Errorf("the codex review model %q names no model family (its name after any provider prefix, up to the first dash)", c.Model)
	}
	if e := strings.TrimSpace(c.Effort); e != "" && !codexEffortWord.MatchString(e) {
		return fmt.Errorf("the codex review effort %q is not one lower-case word", c.Effort)
	}
	return nil
}

// resolved is the model and effort the profiles run: the trimmed values, or the defaults for blank ones.
func (c CodexReviewConfig) resolved() (model, effort string) {
	model, effort = strings.TrimSpace(c.Model), strings.TrimSpace(c.Effort)
	if model == "" {
		model = codexReviewModel
	}
	if effort == "" {
		effort = codexReviewEffort
	}
	return model, effort
}

// codexQuotaTexts are the texts that mean Codex's quota is spent. UNCONFIRMED: no real Codex quota sample has been captured; the
// binary's own wording is "You've hit your usage limit. ...". They are matched only on a failed call or a blank answer (classify),
// as every profile's. "429" and "quota" are left out on purpose: the stderr of a failed codex call carries the whole session
// transcript, and a review of CRW's own code quotes StateQuota and line numbers such as 429. Known risk: a non-quota failure whose
// transcript quotes one of these texts reads quota; its effect is one fallback call to Bugbot (recorded), never a pass.
var codexQuotaTexts = []string{"usage limit", "rate limit", "too many requests", "resource_exhausted"}

// CodexReviewProfile is the uncommitted mode of the unit check through `codex review --uncommitted` (codex-cli): the model (as model
// and review_model), the effort and a read-only sandbox are given as -c overrides, each one argv element whose value is a TOML string (model="gpt-6-astra"; the
// quotes are bytes of the element, no shell). `--uncommitted` takes no prompt, so the profile's Args give none and the record says
// the tool was given no prompt (PromptSent false). The answer reader is readCodexReview.
func CodexReviewProfile(cfg CodexReviewConfig) Profile {
	p := codexCall("codex-review", cfg, "--uncommitted")
	p.Args = func(Request) ([]string, error) { return nil, nil }
	return p
}

// CodexReviewCommittedProfile is the committed mode of the unit check through `codex review --base <base>`: the same overrides as
// CodexReviewProfile, and Args that give `--base <Request.Base>` and no prompt (whether codex takes a prompt with --base is
// unverified, so none is sent; PromptSent false); a request with a blank Base is refused before any call. Codex reviews the diff from
// its own merge-base(base, HEAD) to the worktree, which in land-head's clean checkout of the range's head is base..head. A commit id
// as --base on a detached head was probed by the parent on 10-16 (codex-cli 0.154.0: exit 0 and a review of the range,
// inputs/codex-review-sample-one-finding.txt). The reader does not read the range from the answer (Codex does not name it), so the
// profile is not Ranged.
func CodexReviewCommittedProfile(cfg CodexReviewConfig) Profile {
	p := codexCall("codex-review-committed", cfg)
	p.Args = func(req Request) ([]string, error) {
		base := strings.TrimSpace(req.Base)
		if base == "" {
			return nil, errors.New("a committed codex review needs the range's base and the request names none")
		}
		return []string{"--base", base}, nil
	}
	return p
}

// codexFamily is the family of a model the way the adapter reads a profile with no family (execution.ModelFamily in canonical
// form), or "" when it cannot be told.
func codexFamily(model string) string {
	family, ok := execution.CanonicalFamily(execution.ModelFamily(model))
	if !ok {
		return ""
	}
	return family
}

// codexCall is the codex review call both Codex profiles make, with mode flags after "review" and before the overrides. The model is
// given as both model and review_model: Codex's review task runs review_model when one is set (codex-rs/core/src/tasks/review.rs,
// start_review_conversation), so a review_model of the user's configuration would otherwise run a model the record does not name.
// The profile's family is the model's (codexFamily); one that cannot be told is blank, and the adapter refuses the request.
func codexCall(name string, cfg CodexReviewConfig, mode ...string) Profile {
	model, effort := cfg.resolved()
	argv := append([]string{"codex", "review"}, mode...)
	argv = append(argv, "-c", `model="`+model+`"`, "-c", `review_model="`+model+`"`, "-c", `model_reasoning_effort="`+effort+`"`,
		"-c", `sandbox_mode="read-only"`)
	return Profile{
		Name:       name,
		Argv:       argv,
		Model:      model,
		Effort:     effort,
		Family:     codexFamily(model),
		QuotaTexts: slices.Clone(codexQuotaTexts),
		Answer:     readCodexReview,
	}
}

// codexListHeaders are the lines (trimmed) that start the findings list of a codex review: the plural header when there are several
// findings, the singular one for a single finding.
var codexListHeaders = []string{"Full review comments:", "Review comment:"}

// codexItemLike is a line that starts like an item head at column 0: "- [<anything>] "; codexItemHead is one whose severity is P0-P3.
var (
	codexItemLike = regexp.MustCompile(`^- \[([^\]]*)\] `)
	codexItemHead = regexp.MustCompile(`^- \[(P[0-3])\] (.*)$`)
	codexLocation = regexp.MustCompile(`^(.*[^\s]):(\d+)(-\d+)?$`)
)

// codexNotReviewed are the wordings of a summary that says the review did not happen, did not finish or covered only part of the
// change. They are matched in lower case, with the summary's lines joined by single spaces (a wording may break across lines), in
// the summary (the text before the list header) of a verified review only, so a finding's body may use them. They are narrow on
// purpose: a clean summary describes the change ("removes a dead branch", "the loader did not read the variable") and often says the
// tests were not run in the read-only sandbox, and none of that is refused. English: a reviewer's verb (review, inspect, examine),
// or complete / finish the review, after could not, couldn't, cannot, can't, unable to, did not, didn't, failed to or was unable to;
// "only reviewed"; "not (been) reviewed"; a review that is incomplete, unfinished or not complete; nothing or no changes to review.
// Korean (codex answers in the configured language): 검토 or 리뷰 (with 를, 가 or 는) followed by 하지 못, 할 수 없, 불가,
// 완료하지/마치지/끝내지 못, 완료할/마칠/끝낼 수 없, 완료되지/끝나지 않; and 검토할 변경(사항)이 없. "확인하지 못" is left out:
// "문제를 확인하지 못했습니다" says no problem was found. The list cannot be complete: Codex prints no mark of a finished review, so
// a wording outside it still reads clean (the known limit of this reader). A review that was aborted, interrupted, cancelled or
// stopped ("review aborted", "the review was interrupted", "aborted before inspecting", 리뷰가 중단) is refused too; a clean summary
// of a change to review code that says those words reads invalid, a fail-closed answer the lane asks once more.
var codexNotReviewed = regexp.MustCompile(`(could not|couldn['’]t|cannot|can['’]t|unable to|did not|didn['’]t|failed to) (fully )?` +
	`(review|inspect|examine|(complete|finish) (the |my |this )?(review|inspection))\b` +
	`|\bonly (partially )?(reviewed|inspected|read)\b|\bnot (been )?(fully )?(reviewed|inspected)\b` +
	`|\b(review|inspection) (is |was )?(incomplete|unfinished|not complete)\b|\b(incomplete|unfinished|partial) review\b` +
	`|\b(nothing|no (uncommitted |staged )?changes?) to review\b` +
	`|\b(review|inspection) (was |has been |is )?(aborted|interrupted|cancell?ed|stopped)\b` +
	`|\b(aborted|interrupted|cancell?ed|stopped) (before|without) (inspecting|reviewing|reading|examining)\b` +
	`|(검토|리뷰)(를|가|는)? ?(하지 못|할 수 없|불가|(완료하|마치|끝내)지 못|(완료할|마칠|끝낼) 수 없|(완료되|끝나)지 않)` +
	`|(검토|리뷰)(가|는|이|를)? ?(중단|취소)` +
	`|(검토|리뷰)할 변경( ?사항)?이 없`)

// codexFallbackTexts are the texts codex-cli 0.154.0 prints, with exit 0, in place of a review, verbatim from its source:
// REVIEW_FALLBACK_MESSAGE of codex-rs/protocol/src/review_format.rs (render_review_output_text, for a review with no overall
// explanation and no finding) and the assistant message of exit_review_mode in codex-rs/core/src/tasks/review.rs (a review that
// ended with no output: aborted, or its events closed before the turn completed). A verified review whose summary holds one is
// invalid, never clean.
var codexFallbackTexts = []string{
	"Reviewer failed to output a response.",
	"Review was interrupted. Please re-run /review and wait for it to complete.",
}

// codexDash is the separator of an item's title and location: an em dash with a space on each side. A title may hold one, so the head
// splits at the last.
const codexDash = " — "

// readCodexReview reads the prose `codex review` prints into a document of AnswerSchema. The text before the list header
// (codexListHeaders, a line of its own) is the summary. Each item of the list starts at column 0 with "- [P<n>] " (P0-P3) and a
// head "title — location", split at the last " — "; the location is file:line or file:line-line (its first line is kept) and a file
// under the request's worktree (as given, or with its symlinks resolved) becomes the slash path relative to it, so FindingID does not
// depend on the worktree; any other file is kept as printed. Every non-blank line after the head up to the next head or the end is
// the item's body, trimmed (indented or not) and joined with single spaces; the finding's text is "title: body", or the title alone.
// The verdict is needs_changes when a finding blocks under the rule decodeAnswer applies (normalize, firstBlocking), else verified.
//
// A text with no header and no item line is a clean review (verified, no findings); a blank one is a blank answer. Not an answer
// (an error, so the record is invalid): a header with no item, a second header, an item line before the header, text between the
// header and its first item, a head with no " — " location, a blank title, a location that is not file:line, or a line that starts
// like an item head with a severity other than P0-P3, a verified review whose summary is or holds one of Codex's own texts for a
// review that gave no answer or was interrupted (codexFallbackTexts), or says the change was not reviewed, or only in part, or that
// the review was aborted (codexNotReviewed), and a review with no list whose summary holds no letter (unicode.IsLetter, any script:
// punctuation or digits only): such a text would otherwise pass as a clean review. A short clean summary ("Clean.") is a clean
// review: no length is asked of a summary, whatever its language. A review with a blocking finding is read whatever its
// summary says (it needs changes either way).
func readCodexReview(stdout []byte, req Request) ([]byte, error) {
	text := strings.TrimSpace(strings.ReplaceAll(string(stdout), "\r\n", "\n"))
	if text == "" {
		return nil, nil
	}
	var items []codexEntry
	var summary []string
	header := -1
	for i, l := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(l)
		if slices.Contains(codexListHeaders, trimmed) {
			if header >= 0 {
				return nil, fmt.Errorf("the review has a second list header %q on line %d", trimmed, i+1)
			}
			header = i
			continue
		}
		like := codexItemLike.FindStringSubmatch(l)
		if header < 0 {
			if like != nil && codexSeverityWord(like[1]) {
				return nil, fmt.Errorf("an item line comes before the list header: %s", trimmed)
			}
			summary = append(summary, trimmed)
			continue
		}
		if like != nil {
			m := codexItemHead.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("an item's severity %q is not P0-P3: %s", like[1], trimmed)
			}
			it, err := codexHead(m[1], m[2], req.Worktree)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
			continue
		}
		if trimmed == "" {
			continue
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("text comes between the list header and its first item: %s", trimmed)
		}
		items[len(items)-1].body = append(items[len(items)-1].body, trimmed)
	}
	if header >= 0 && len(items) == 0 {
		return nil, errors.New("the review's findings list has no item")
	}

	type finding struct {
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Text     string `json:"text"`
	}
	doc := struct {
		Verdict  string    `json:"verdict"`
		Findings []finding `json:"findings"`
	}{Verdict: VerdictVerified, Findings: []finding{}}
	var graded []Finding
	var security []bool
	for _, it := range items {
		text := it.title
		if len(it.body) > 0 {
			text += ": " + strings.Join(it.body, " ")
		}
		doc.Findings = append(doc.Findings, finding{Severity: it.severity, File: it.file, Line: it.line, Text: text})
		g, sec, _ := review.NormalizeGrade(it.severity) // P0-P3 always read
		graded = append(graded, Finding{ID: FindingID(it.file, text), Severity: string(g), File: it.file, Line: it.line, Text: text})
		security = append(security, sec)
	}
	// the verdict follows the rule decodeAnswer applies to this document, as Bugbot's reader does
	if norm, err := normalize(VerdictNeedsChanges, graded, security); err == nil {
		if _, blocks := firstBlocking(norm); blocks {
			doc.Verdict = VerdictNeedsChanges
		}
	}
	// a verified review whose summary says it did not review the change (or only part of it) is no clean review; a blocking finding
	// needs changes whatever the summary says, and a summary of a change to review code may well quote these words
	if doc.Verdict == VerdictVerified {
		joined := strings.Join(summary, " ")
		for _, f := range codexFallbackTexts {
			if strings.Contains(joined, f) {
				return nil, fmt.Errorf("the review is codex's own text for a review that gave no answer or was interrupted: %q", f)
			}
		}
		if w := codexNotReviewed.FindString(strings.ToLower(joined)); w != "" {
			return nil, fmt.Errorf("the review says it did not review the change (%q): %s", w, firstLine([]byte(strings.Join(summary, "\n"))))
		}
		if header < 0 && !strings.ContainsFunc(joined, unicode.IsLetter) {
			return nil, fmt.Errorf("the review has no findings list and its summary holds no letter: %q", strings.TrimSpace(joined))
		}
	}
	return json.Marshal(doc)
}

// codexSeverityWord reports whether an item-like line's bracket holds a P grade (P followed by digits), the shape of an item head.
func codexSeverityWord(s string) bool {
	return len(s) >= 2 && (s[0] == 'P' || s[0] == 'p') && strings.Trim(s[1:], "0123456789") == ""
}

// codexEntry is one item of a codex review's findings list: its severity (P0-P3), title, file (relative to the worktree when under
// it), first line, and body lines (trimmed).
type codexEntry struct {
	severity, title, file string
	line                  int
	body                  []string
}

// codexHead reads an item head "title — location" of severity sev.
func codexHead(sev, head, worktree string) (codexEntry, error) {
	var it codexEntry
	i := strings.LastIndex(head, codexDash)
	if i < 0 {
		return it, fmt.Errorf("an item has no %q location: - [%s] %s", strings.TrimSpace(codexDash), sev, head)
	}
	title, loc := strings.TrimSpace(head[:i]), strings.Trim(strings.TrimSpace(head[i+len(codexDash):]), "`")
	if title == "" {
		return it, fmt.Errorf("an item has a blank title: - [%s] %s", sev, head)
	}
	if !codexLocation.MatchString(loc) {
		return it, fmt.Errorf("an item's location %q is not file:line", loc)
	}
	file, line := splitLocation(loc)
	it.severity, it.title, it.file, it.line = sev, title, codexRelative(file, worktree), line
	return it, nil
}

// codexRelative gives an absolute file under worktree (as given, or with its symlinks resolved) as the slash path relative to it;
// any other file is kept as printed.
func codexRelative(file, worktree string) string {
	if !filepath.IsAbs(file) || strings.TrimSpace(worktree) == "" {
		return file
	}
	roots := []string{filepath.Clean(worktree)}
	if r, err := filepath.EvalSymlinks(worktree); err == nil && r != roots[0] {
		roots = append(roots, r)
	}
	clean := filepath.Clean(file)
	for _, root := range roots {
		rel, err := filepath.Rel(root, clean)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.ToSlash(rel)
	}
	return file
}
