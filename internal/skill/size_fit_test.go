package skill

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// `crw skill size-fit`: the advisory suggester that puts the issue-size report's answer beside the counts of an
// issue body and proposes a size_reason from the closed set. It never refuses.

// sizeReportOf is the real report `crw skill issue-size check` prints for a synthetic issue of c completion
// criteria, with the bundle reason and exception given.
func sizeReportOf(t testing.TB, id string, criteria int, bundleReason string, exception map[string]any) json.RawMessage {
	t.Helper()
	extra := map[string]any{}
	if exception != nil {
		extra["exception"] = exception
	}
	args := []string{}
	if bundleReason != "" {
		args = append(args, "--bundle-reason", bundleReason)
	}
	code, out, errOut := sizeCall(structuredIssue(id, criteria, 0, extra), args...)
	if code != 0 {
		t.Fatalf("issue-size check: exit %d: %s", code, errOut)
	}
	return json.RawMessage(out)
}

func sizeFitInput(report json.RawMessage, counts map[string]any, ownGoal bool) string {
	m := map[string]any{"report": report}
	if counts != nil {
		m["counts"] = counts
	}
	if ownGoal {
		m["own_goal"] = true
	}
	out, _ := json.Marshal(m)
	return string(out)
}

type fitAnswer struct {
	Schema           string   `json:"schema"`
	Issue            string   `json:"issue"`
	IssueSize        string   `json:"issue_size"`
	SizeReason       string   `json:"size_reason"`
	SuggestedSizeFit string   `json:"suggested_size_fit"`
	Triggers         []string `json:"triggers"`
	ExceptionRecord  string   `json:"exception_record"`
	BundleReason     string   `json:"bundle_reason"`
}

func fitCall(t testing.TB, input string) fitAnswer {
	t.Helper()
	code, out, errOut := call([]string{"size-fit"}, input)
	if code != 0 {
		t.Fatalf("size-fit: exit %d, stderr %q", code, errOut)
	}
	var a fitAnswer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("size-fit printed %q: %v", out, err)
	}
	return a
}

func TestSizeFitBoundaries(t *testing.T) {
	exception := map[string]any{"issue": "CRW-FIT", "approved_by": "Jun", "approved_on": "2026-10-11", "statement": "one concept, kept whole"}
	okReport := sizeReportOf(t, "CRW-FIT", 3, "", nil)
	overReport := sizeReportOf(t, "CRW-FIT", 9, "", nil)
	acceptedReport := sizeReportOf(t, "CRW-FIT", 9, "one concept", exception)
	cases := []struct {
		name      string
		report    json.RawMessage
		counts    map[string]any
		ownGoal   bool
		answer    string
		reason    string
		sizeFit   string
		triggers  []string
		exception bool
	}{
		{"ok with nothing counted is fits_executor", okReport, nil, false, "ok", "fits_executor", "executor", nil, false},
		{"over_line with four edit regions is regions_gt3", overReport, map[string]any{"edit_regions": 4}, false, "over_line", "regions_gt3", "child", []string{"regions_gt3"}, false},
		{"over_line_accepted with no signal is fits_executor and carries the exception record", acceptedReport, nil, false, "over_line_accepted", "fits_executor", "executor", nil, true},
		{"ok with 25 minutes of verification is verify_gt20m", okReport, map[string]any{"verify_minutes": 25}, false, "ok", "verify_gt20m", "child", []string{"verify_gt20m"}, false},
		{"three regions, two contract packages, five steps and twenty minutes are the boundary: fits_executor", okReport,
			map[string]any{"edit_regions": 3, "contract_packages": 2, "steps": 5, "verify_minutes": 20}, false, "ok", "fits_executor", "executor", nil, false},
		{"four regions", okReport, map[string]any{"edit_regions": 4}, false, "ok", "regions_gt3", "child", []string{"regions_gt3"}, false},
		{"three contract packages", okReport, map[string]any{"contract_packages": 3}, false, "ok", "packages_contract_gt2", "child", []string{"packages_contract_gt2"}, false},
		{"six steps", okReport, map[string]any{"steps": 6}, false, "ok", "steps_gt5", "child", []string{"steps_gt5"}, false},
		{"21 minutes", okReport, map[string]any{"verify_minutes": 21}, false, "ok", "verify_gt20m", "child", []string{"verify_gt20m"}, false},
		{"own goal with nothing counted", okReport, nil, true, "ok", "own_goal", "child", []string{"own_goal"}, false},
		{"the first reason of the closed set's order is proposed, every one that holds is listed", overReport,
			map[string]any{"edit_regions": 9, "steps": 7, "verify_minutes": 90}, true, "over_line", "regions_gt3", "child",
			[]string{"regions_gt3", "steps_gt5", "verify_gt20m", "own_goal"}, false},
		{"over_line with no signal stays fits_executor: the answer alone forces nothing", overReport, nil, false, "over_line", "fits_executor", "executor", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := fitCall(t, sizeFitInput(c.report, c.counts, c.ownGoal))
			if a.Schema != "crw-size-fit/1" || a.Issue != "CRW-FIT" {
				t.Errorf("schema %q issue %q", a.Schema, a.Issue)
			}
			if a.IssueSize != c.answer || a.SizeReason != c.reason || a.SuggestedSizeFit != c.sizeFit {
				t.Errorf("got answer %q reason %q size_fit %q; want %q %q %q", a.IssueSize, a.SizeReason, a.SuggestedSizeFit, c.answer, c.reason, c.sizeFit)
			}
			if strings.Join(a.Triggers, ",") != strings.Join(c.triggers, ",") {
				t.Errorf("triggers %v, want %v", a.Triggers, c.triggers)
			}
			if c.exception != (a.ExceptionRecord != "") {
				t.Errorf("exception record %q, want present=%v", a.ExceptionRecord, c.exception)
			}
			if c.exception && !strings.Contains(a.ExceptionRecord, "one concept, kept whole") {
				t.Errorf("the report's exception record was not carried: %q", a.ExceptionRecord)
			}
		})
	}
}

// The edit-region count the report itself made is used when the body counts give none.
func TestSizeFitReadsTheReportsEditRegions(t *testing.T) {
	body := "## Completion criteria\n- one\n\n## Scope\n- `internal/a/x.go`\n- `internal/b/x.go`\n- `internal/c/x.go`\n- `internal/d/x.go`\n"
	code, report, errOut := sizeCall(descriptionIssue(body))
	if code != 0 {
		t.Fatalf("issue-size: %d %s", code, errOut)
	}
	a := fitCall(t, sizeFitInput(json.RawMessage(report), nil, false))
	if a.SizeReason != "regions_gt3" {
		t.Errorf("reason %q for a report that counted four regions", a.SizeReason)
	}
	// a count the body states wins over the report's
	a = fitCall(t, sizeFitInput(json.RawMessage(report), map[string]any{"edit_regions": 2}, false))
	if a.SizeReason != "fits_executor" {
		t.Errorf("reason %q for a stated count of two", a.SizeReason)
	}
}

// size-fit never refuses: whatever the counts and the answer, a readable input exits 0, proposes one
// reason of the closed set, and the output holds no grade and no refusal.
func TestSizeFitNeverRefuses(t *testing.T) {
	reports := map[string]json.RawMessage{
		"ok":                 sizeReportOf(t, "CRW-FIT", 2, "", nil),
		"over_line":          sizeReportOf(t, "CRW-FIT", 12, "", nil),
		"over_line_accepted": sizeReportOf(t, "CRW-FIT", 12, "kept whole", nil),
	}
	closed := map[string]bool{}
	for _, r := range SizeReasons {
		closed[r] = true
	}
	values := []any{nil, 0, 1, 3, 4, 100000}
	n := 0
	for answer, report := range reports {
		for _, regions := range values {
			for _, steps := range values {
				for _, minutes := range values {
					for _, own := range []bool{false, true} {
						counts := map[string]any{}
						for k, v := range map[string]any{"edit_regions": regions, "steps": steps, "verify_minutes": minutes, "contract_packages": steps} {
							if v != nil {
								counts[k] = v
							}
						}
						code, out, errOut := call([]string{"size-fit"}, sizeFitInput(report, counts, own))
						n++
						if code != 0 || errOut != "" {
							t.Fatalf("%s %v own=%v: exit %d, stderr %q", answer, counts, own, code, errOut)
						}
						var a map[string]any
						if err := json.Unmarshal([]byte(out), &a); err != nil {
							t.Fatal(err)
						}
						if r, _ := a["size_reason"].(string); !closed[r] {
							t.Fatalf("%s %v: reason %q is outside the closed set", answer, counts, r)
						}
						for _, banned := range []string{"grade", "refused", "error", "blocked", "size_class", "L", "M"} {
							if _, ok := a[banned]; ok {
								t.Fatalf("output holds %q: %s", banned, out)
							}
						}
						if a["advisory"] != true {
							t.Fatalf("output is not marked advisory: %s", out)
						}
					}
				}
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d combinations ran", n)
	}
}

// An input that cannot be read is the only non-zero answer, the way issue-size has it.
func TestSizeFitUnreadableInput(t *testing.T) {
	good := sizeReportOf(t, "CRW-FIT", 3, "", nil)
	cases := map[string]string{
		"not json":              "{",
		"no report":             `{}`,
		"both report and issue": fmt.Sprintf(`{"report":%s,"issue":{"id":"X"}}`, good),
		"unknown answer":        `{"report":{"issue":"X","decision":"grade_L"}}`,
		"negative count":        sizeFitInput(good, map[string]any{"steps": -1}, false),
		"unknown field":         `{"report":{"issue":"X","decision":"ok"},"grade":"L"}`,
	}
	for name, input := range cases {
		code, out, errOut := call([]string{"size-fit"}, input)
		if code != 2 || out != "" || !strings.Contains(errOut, "Unreadable") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
}

// The issue-size input can be given in place of its report; size-fit then runs the check itself.
func TestSizeFitRunsTheCheckOnAnIssue(t *testing.T) {
	a := fitCall(t, `{"issue":`+structuredIssue("CRW-FIT", 9, 0, nil)+`,"counts":{"steps":6}}`)
	if a.IssueSize != "over_line" || a.SizeReason != "steps_gt5" || a.Issue != "CRW-FIT" {
		t.Errorf("%+v", a)
	}
}

// The closed sets other lanes check against (S07 TestSizeFitClosedSet).
func TestSizeFitClosedSets(t *testing.T) {
	if got := strings.Join(SizeReasons, ","); got != "regions_gt3,packages_contract_gt2,steps_gt5,verify_gt20m,own_goal,fits_executor" {
		t.Errorf("size reasons %s", got)
	}
	if got := strings.Join(SizeFits, ","); got != "executor,child,split" {
		t.Errorf("size fits %s", got)
	}
	if got := strings.Join(IssueSizeAnswers, ","); got != "ok,over_line,over_line_accepted" {
		t.Errorf("answers %s", got)
	}
}
