package command

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// markerPrefix begins the body of the summary comment and identifies it: the one comment of a pull request whose body starts with it is the one kept up to date, whatever head it last summarized.
const markerPrefix = "<!-- crw-independent-review v1 "

const maxListed = 10 // the findings the comment lists; the artifact has them all

// code makes text from the artifact or from a setting safe to show: one line, no backtick or pipe, cut to limit characters, inside a code span, which cannot make a mention, a link, an image or HTML.
func code(s string, limit int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r == '|':
			return '/'
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	if s == "" {
		s = "-"
	}
	return "`" + s + "`"
}

func short(s string) string { return s[:min(len(s), 12)] }

// renderSummary is the text of the summary comment for artifact a, whose file has sha256 sha: the marker line, the statement that the review is a reference opinion and not a merge gate, the model, the status and
// the status of each reviewer (an invalid or unavailable reviewer is shown as such), and a short list of the findings.
func renderSummary(a *review.Artifact, sha string) string {
	marker, _ := json.Marshal(struct {
		Head    string `json:"head"`
		PatchID string `json:"patchId"`
		Status  string `json:"status"`
		SHA256  string `json:"sha256"`
	}{a.Head, a.PatchID, string(a.Status), sha})
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n## Independent review: a reference opinion, not a merge gate\n\n", markerPrefix, marker)
	b.WriteString("This is a reference opinion from an independent reviewer that saw only the diff, the changed files and the repository's public rules. It is not a merge gate: it does not replace the required checks or the other reviews, and no merge waits for it. It is kept as this one comment and never as a review thread.\n\n")
	fmt.Fprintf(&b, "- Model: %s, effort %s\n- Change: head %s on base %s, patch-id %s\n- Status: **%s**", code(a.Model, 80), code(a.Effort, 40), code(short(a.Head), 12), code(short(a.Base), 12), code(short(a.PatchID), 12), a.Status)
	if a.Reason != "" {
		fmt.Fprintf(&b, ": %s", code(a.Reason, 160))
	}
	fmt.Fprintf(&b, "\n- Reviewers: %d run, %d failed\n", a.Reviewers.Run, a.Reviewers.Failed)
	type tally struct {
		perspective   string
		calls, normal int
		invalid       bool
		reasons       []string
	}
	byReviewer := map[int]*tally{}
	for _, c := range a.Calls {
		if c.Stage != "review" {
			continue
		}
		t := byReviewer[c.Reviewer]
		if t == nil {
			t = &tally{perspective: c.Perspective}
			byReviewer[c.Reviewer] = t
		}
		t.calls++
		if c.Class == "normal" {
			t.normal++
			continue
		}
		t.invalid = t.invalid || c.Class == "invalid"
		if !slices.Contains(t.reasons, c.Reason) {
			t.reasons = append(t.reasons, c.Reason)
		}
	}
	ids := make([]int, 0, len(byReviewer))
	for id := range byReviewer {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t, state := byReviewer[id], "unavailable"
		switch {
		case t.normal == t.calls:
			state = "ok"
		case t.normal > 0:
			state = "partial"
		case t.invalid:
			state = "invalid"
		}
		fmt.Fprintf(&b, "  - reviewer %d (%s): %s", id, code(t.perspective, 40), state)
		for i, r := range t.reasons {
			fmt.Fprintf(&b, "%s%s", [...]string{" ", ", "}[min(i, 1)], code(r, 120))
		}
		b.WriteString("\n")
	}
	if a.Status != review.StatusComplete {
		b.WriteString("\nA review that is partial, invalid or unavailable is not a clean result and is not \"no findings\": what a reviewer did not read, it did not judge.\n")
	}
	counts := map[review.Grade]int{}
	security := 0
	for _, f := range a.Findings {
		counts[f.Grade]++
		if f.Security {
			security++
		}
	}
	fmt.Fprintf(&b, "\nFindings: %d kept, %d dropped (P0 %d, P1 %d, P2 %d, P3 %d; %d security)\n", len(a.Findings), len(a.Dropped), counts[review.P0], counts[review.P1], counts[review.P2], counts[review.P3], security)
	for i, f := range a.Findings {
		if i == maxListed {
			fmt.Fprintf(&b, "- and %d more in the artifact\n", len(a.Findings)-maxListed)
			break
		}
		label, note := map[bool]string{true: " security"}[f.Security], map[bool]string{true: ", needs context"}[f.NeedsContext]
		fmt.Fprintf(&b, "- **%s**%s: %s at %s; %d of %d reviewers; %s%s\n", f.Grade, label, code(f.Title, 120), code(fmt.Sprintf("%s:%d", f.File, f.Line), 100), f.Support, a.Reviewers.Run, f.Verdict, note)
	}
	return b.String()
}
