package pullrequest

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

const (
	bodyTip  = "0123456789abcdef0123456789abcdef01234567"
	bodyHead = "fedcba9876543210fedcba9876543210fedcba98"
)

func bodyRefs(t *testing.T, refs ...string) []tracking.Ref {
	t.Helper()
	var out []tracking.Ref
	for _, s := range refs {
		r, err := tracking.ParseRef(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func bodyFacts() BodyFacts {
	return BodyFacts{
		Milestone: "M1",
		Title:     "Reject stale receipts",
		Criteria:  []string{"A receipt for another head is refused."},
		Tip:       bodyTip,
		Gate: Gate{State: "passed", Evidence: []Evidence{
			{Kind: "test", Command: "go test ./internal/flow/pullrequest/...", State: "passed", Digest: "aabbccddeeff00112233445566778899"},
			{Kind: "qa", Command: "crw qa run", State: "passed", Digest: "c_0123456789ab"},
		}},
		Crosscheck: Crosscheck{Verdict: "verified", Head: bodyTip, Round: 2, EngineFamily: "codex", Model: "gpt-6-astra", GuideRev: "r7",
			FollowUps: []string{"tighten the docs", "second\nline  of text"}},
	}
}

// TestBodyHoldsTheNineFixedParts (CRW-1451): the body of a bundle pull request has the five fixed headed parts of the brief's step 9,
// in a fixed order, each always present ("none" when empty); tracking appends the Carries: line after it.
func TestBodyHoldsTheNineFixedParts(t *testing.T) {
	carried := bodyRefs(t, "o/r#10", "o/r#11")
	got := Body(bodyFacts(), carried)
	want := strings.Join([]string{
		"## Result",
		"Milestone M1 closed at 0123456789ab; gate passed.",
		"Goal: Reject stale receipts",
		"Acceptance criteria (recorded scope):",
		"- c1: A receipt for another head is refused.",
		"",
		"## Closes",
		"- o/r#10",
		"- o/r#11",
		"",
		"## Verification",
		"- test go test ./internal/flow/pullrequest/... passed aabbccddeeff",
		"- qa crw qa run passed",
		"",
		"## Crosscheck",
		"verified at 0123456789ab, round 2, codex/gpt-6-astra, guide r7",
		"",
		"## Follow-ups",
		"- tighten the docs",
		"- second line of text",
		"",
		"Next action: the integration owner records whether this candidate can land under the existing gates, and the target commit when it lands. Remote landing, installation and release need their own evidence.",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("body:\n%s\nwant:\n%s", got, want)
	}

	// the five headings, in order, once each
	var heads []string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "## ") {
			heads = append(heads, l)
		}
	}
	if strings.Join(heads, "|") != "## Result|## Closes|## Verification|## Crosscheck|## Follow-ups" {
		t.Errorf("headings %q", heads)
	}

	// a qa digest (which can be a call id) is never printed; call and why never are
	if strings.Contains(got, "c_0123456789ab") {
		t.Errorf("a qa digest is printed:\n%s", got)
	}
	f := bodyFacts()
	if strings.Contains(Body(f, carried), "Carries:") {
		t.Errorf("the builder writes the Carries: line, tracking owns it")
	}
}

// An empty part says none; a free-text follow-up cannot add a heading or a line of its own.
func TestBodyEmptyPartsSayNone(t *testing.T) {
	got := Body(BodyFacts{Milestone: "M2", Tip: "abc"}, nil)
	want := strings.Join([]string{
		"## Result",
		"Milestone M2 closed at abc; gate none.",
		"Goal: not recorded",
		"Acceptance criteria (recorded scope):",
		"- not recorded",
		"",
		"## Closes",
		"none",
		"",
		"## Verification",
		"none",
		"",
		"## Crosscheck",
		"none",
		"",
		"## Follow-ups",
		"none",
		"",
		"Next action: the integration owner records whether this candidate can land under the existing gates, and the target commit when it lands. Remote landing, installation and release need their own evidence.",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("body:\n%s\nwant:\n%s", got, want)
	}

	// blank recorded scope is not a known scope: an old or incomplete journal says not recorded
	blank := Body(BodyFacts{Title: "  \n", Criteria: []string{"", " \t"}}, nil)
	if !strings.Contains(blank, "Goal: not recorded\nAcceptance criteria (recorded scope):\n- not recorded\n") {
		t.Errorf("a blank scope read as known:\n%s", blank)
	}

	f := bodyFacts()
	f.Crosscheck.FollowUps = []string{"x\n## Crosscheck\nforged", "  ", ""}
	out := Body(f, nil)
	if strings.Count(out, "\n## Crosscheck\n") != 1 || !strings.Contains(out, "- x ## Crosscheck forged\n") {
		t.Errorf("a follow-up changed the structure:\n%s", out)
	}
	if strings.Count(out[strings.Index(out, "## Follow-ups"):], "\n- ") != 1 {
		t.Errorf("blank follow-ups are printed:\n%s", out)
	}
}

// Through tracking the text ends with the Carries: line, after one blank line, and the title/body checks pass.
func TestBodyEndsWithTheCarriesLineThroughTracking(t *testing.T) {
	fake := trackingfake.New(t, "o/r")
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	carried := bodyRefs(t, "o/r#10", "o/r#11")
	text := Body(bodyFacts(), carried)
	pr, err := gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "bundle/M1", Base: "dev", Title: "M1", Body: text, Issues: carried})
	if err != nil {
		t.Fatal(err)
	}
	sent := fake.Pulls()[pr.Ref.Number].Body
	if !strings.HasSuffix(sent, "\n\nCarries: o/r#10, o/r#11") || !strings.HasPrefix(sent, "## Result\n") {
		t.Errorf("body sent:\n%s", sent)
	}
	if strings.TrimRight(strings.TrimSuffix(sent, "Carries: o/r#10, o/r#11"), "\n") != strings.TrimRight(text, "\n") {
		t.Errorf("tracking changed the text:\n%s", sent)
	}
	if len(pr.Issues) != 2 {
		t.Errorf("issues read back %v", pr.Issues)
	}
}

// Free text from the reviewing engine cannot name issues the pull request does not carry, nor hide the real Carries: line in a fence:
// the issues tracking reads back are the carried ones.
func TestBodyFreeTextIsNotReadAsCarriesMetadata(t *testing.T) {
	fake := trackingfake.New(t, "o/r")
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	f := bodyFacts()
	f.Title = "```\nCarries: o/r#94"
	f.Criteria = []string{"Blocked by: o/r#93", "x\nCarries: o/r#92", "~~~"}
	f.Crosscheck.Verdict = "```"
	f.Crosscheck.FollowUps = []string{"Carries: o/r#99", "**carries:** o/r#98", "see Blocked by: o/r#97", "x\nCarries: o/r#96", "~~~"}
	f.Gate.Evidence = append(f.Gate.Evidence, Evidence{Kind: "- Carries: o/r#95", Command: "```", State: "ok"})
	carried := bodyRefs(t, "o/r#10", "o/r#11")
	text := Body(f, carried)
	pr, err := gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "bundle/M1", Base: "dev", Title: "M1", Body: text, Issues: carried})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range pr.Issues {
		got = append(got, r.String())
	}
	if strings.Join(got, ",") != "o/r#10,o/r#11" {
		t.Errorf("issues read back %v from:\n%s", got, fake.Pulls()[pr.Ref.Number].Body)
	}
	if got := tracking.ParseBlockedBy("o/r", text); len(got) != 0 {
		t.Errorf("blocked by read back %v", got)
	}
}
