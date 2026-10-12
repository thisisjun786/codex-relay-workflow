package compile

import (
	"strings"
	"testing"
)

func milestone(key string, with ...func(*Milestone)) Milestone {
	m := Milestone{Key: key, Title: "milestone " + key, Criteria: []string{"the milestone " + key + " is done"}, Surfaces: []string{"logic"}, Grade: "C2", RequiredCommands: []string{}}
	for _, w := range with {
		w(&m)
	}
	return m
}

func inMilestone(ms string) func(*Issue) { return func(i *Issue) { i.Milestone = ms } }

// TestCompileMilestonesEntryValidated (CRW-1277 b): the compile input may name the milestones of a project with what the run needs of
// them (title, criteria, surfaces, grade, required commands). The entry is decoded strictly under the same schema id, and each field is
// judged: no blank key or title, at least one criterion, at least one surface of logic, web, tui and desktop, a grade C1..C5 and no
// blank command. Once a project names one entry, an issue whose milestone has no entry and an entry no issue names are refused; a
// project that names none compiles as before.
func TestCompileMilestonesEntryValidated(t *testing.T) {
	good := project(iss("A-1", inMilestone("M1")), iss("A-2", inMilestone("M2"), after("A-1")))
	good.Projects[0].Milestones = []Milestone{milestone("M1"), milestone("M2", func(m *Milestone) {
		m.Surfaces, m.Grade, m.RequiredCommands = []string{"logic", "web"}, "C3", []string{"crw-dev ci local", "make e2e"}
	})}
	plan := compileOne(t, good, nil)
	if plan.Change == nil || len(plan.Change.Changes) == 0 {
		t.Fatalf("a valid handoff with milestones did not compile: %+v", plan)
	}
	// the entries change nothing of the revision: the same handoff without them compiles to the same document
	bare := project(iss("A-1", inMilestone("M1")), iss("A-2", inMilestone("M2"), after("A-1")))
	if string(compileOne(t, bare, nil).Revision) != string(plan.Revision) {
		t.Error("the milestone entries changed the plan revision")
	}
	if got := CriteriaDigest([]string{" one ", "two"}); got != criteriaDigest([]string{"one", "two"}) || got == "" {
		t.Errorf("CriteriaDigest = %q", got)
	}

	// strict decoding: an unknown member of an entry is refused, the entry is read with the project
	raw := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"PX","issues":[{"key":"A-1","title":"t","criteria":["c"],"milestone":"M1"}],` +
		`"milestones":[{"key":"M1","title":"M","criteria":["c"],"surfaces":["logic"],"grade":"C2","required_commands":[],"owner":"x"}]}]}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Errorf("an unknown member of a milestone entry: %v", err)
	}
	in, err := Decode([]byte(strings.Replace(raw, `,"owner":"x"`, "", 1)))
	if err != nil || len(in.Projects[0].Milestones) != 1 || in.Projects[0].Milestones[0].Grade != "C2" {
		t.Fatalf("the entry was not read: %+v %v", in, err)
	}

	for name, tc := range map[string]struct {
		mutate func(*Input)
		want   string
	}{
		"a blank key":                {func(in *Input) { in.Projects[0].Milestones[0].Key = "" }, "key"},
		"a blank title":              {func(in *Input) { in.Projects[0].Milestones[0].Title = " " }, "title"},
		"no criterion":               {func(in *Input) { in.Projects[0].Milestones[0].Criteria = nil }, "criterion"},
		"a blank criterion":          {func(in *Input) { in.Projects[0].Milestones[0].Criteria = []string{"ok", " "} }, "criteria[1]"},
		"no surface":                 {func(in *Input) { in.Projects[0].Milestones[0].Surfaces = nil }, "surface"},
		"an unknown surface":         {func(in *Input) { in.Projects[0].Milestones[0].Surfaces = []string{"logic", "mobile"} }, "mobile"},
		"a repeated surface":         {func(in *Input) { in.Projects[0].Milestones[0].Surfaces = []string{"web", "web"} }, "twice"},
		"no grade":                   {func(in *Input) { in.Projects[0].Milestones[0].Grade = "" }, "grade"},
		"a grade out of the scale":   {func(in *Input) { in.Projects[0].Milestones[0].Grade = "C6" }, "C6"},
		"a lower-case grade":         {func(in *Input) { in.Projects[0].Milestones[0].Grade = "c3" }, "c3"},
		"a blank command":            {func(in *Input) { in.Projects[0].Milestones[1].RequiredCommands = []string{""} }, "required_commands[0]"},
		"a repeated entry":           {func(in *Input) { in.Projects[0].Milestones[1].Key = "M1" }, "twice"},
		"an issue with no entry":     {func(in *Input) { in.Projects[0].Issues[1].Milestone = "M3" }, "M3"},
		"an issue with no milestone": {func(in *Input) { in.Projects[0].Issues[1].Milestone = "" }, "A-2"},
		"an entry no issue names": {func(in *Input) {
			in.Projects[0].Milestones = append(in.Projects[0].Milestones, milestone("M9"))
		}, "M9"},
	} {
		in := project(iss("A-1", inMilestone("M1")), iss("A-2", inMilestone("M2"), after("A-1")))
		in.Projects[0].Milestones = []Milestone{milestone("M1"), milestone("M2")}
		tc.mutate(&in)
		refused := mustRefuse(t, in, nil)
		found := false
		for _, r := range refused.Refusals {
			if r.Rule == "milestone" && strings.Contains(r.Path+" "+r.Detail, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the refusal does not name %q under the milestone rule: %v", name, tc.want, refused)
		}
	}
}
