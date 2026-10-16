package pullrequest

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// BodyFacts are the facts of a closed milestone the body of its pull request states.
type BodyFacts struct {
	Milestone  string
	Tip        string
	Gate       Gate
	Crosscheck Crosscheck
}

// Gate is the close gate of the milestone: its state and the evidence it was judged on.
type Gate struct {
	State    string
	Evidence []Evidence
}

// Evidence is one item of gate evidence. Digest is printed only for kind "test": a "qa" digest can be a call id.
type Evidence struct {
	Kind, Command, State, Digest string
}

// Crosscheck is the verdict row of the crosscheck at the milestone's head.
type Crosscheck struct {
	Verdict, Head, EngineFamily, Model, GuideRev string
	Round                                        int
	FollowUps                                    []string
}

const none = "none"

// Body makes the text of the pull request body: five headed parts in a fixed order (Result, Closes, Verification, Crosscheck,
// Follow-ups), each always present and "none" when empty. It does not write the Carries: line: tracking appends it.
func Body(f BodyFacts, carried []tracking.Ref) string {
	var b strings.Builder
	part := func(heading string, lines ...string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## " + heading + "\n")
		if len(lines) == 0 {
			lines = []string{none}
		}
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}

	part("Result", "Milestone "+oneLine(f.Milestone)+" closed at "+short(f.Tip, 12)+"; gate "+orNone(oneLine(f.Gate.State))+".")

	var closes []string
	for _, r := range carried {
		closes = append(closes, "- "+r.String())
	}
	part("Closes", closes...)

	var verification []string
	for _, e := range f.Gate.Evidence {
		line := "- " + strings.Join(nonEmpty(oneLine(e.Kind), oneLine(e.Command), oneLine(e.State)), " ")
		if e.Kind == "test" && e.Digest != "" {
			line += " " + short(oneLine(e.Digest), 12)
		}
		verification = append(verification, line)
	}
	part("Verification", verification...)

	var crosscheck []string
	if c := f.Crosscheck; c.Verdict != "" {
		line := oneLine(c.Verdict) + " at " + short(c.Head, 12) + ", round " + strconv.Itoa(c.Round) + ", " +
			oneLine(c.EngineFamily) + "/" + oneLine(c.Model)
		if c.GuideRev != "" {
			line += ", guide " + oneLine(c.GuideRev)
		}
		crosscheck = append(crosscheck, line)
	}
	part("Crosscheck", crosscheck...)

	var followUps []string
	for _, s := range f.Crosscheck.FollowUps {
		if s = oneLine(s); s != "" {
			followUps = append(followUps, "- "+s)
		}
	}
	part("Follow-ups", followUps...)
	return b.String()
}

// metaLabel is what tracking reads as metadata on a line of a pull request body ("Carries:", "Blocked by:").
var metaLabel = regexp.MustCompile(`(?i)(carries|blocked by):`)

// oneLine collapses every run of white space (newlines included) to one space, so free text cannot add a line or a heading, and
// escapes what tracking reads as metadata: a label such as "Carries:" (it would name issues the pull request does not carry) and a
// leading code fence marker (it would hide the real Carries: line).
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = metaLabel.ReplaceAllString(s, `$1\:`)
	if strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
		s = `\` + s
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
