package skill

import (
	"encoding/json"
	"strings"
	"testing"
)

// CRW-1040: the release gate is exported as one helper so a caller (the night batch selector) runs the
// same judgement `crw skill issue-ready check` does, with no second copy of the rules.

func TestCheckIssueReadyAnswersAsTheCommandDoes(t *testing.T) {
	cases := []struct {
		name   string
		issue  ReadyIssue
		wantID string
	}{
		{"ready", ReadyIssue{ID: "CRW-A", Title: "a", Description: readyBody}, "ready"},
		{"empty body", ReadyIssue{ID: "CRW-B", Title: "b", Description: ""}, "not_ready"},
		{"design first label", ReadyIssue{ID: "CRW-C", Title: "c", Description: readyBody, Labels: []string{"design-first"}}, "design_first"},
		{"open decision", ReadyIssue{ID: "CRW-D", Title: "d", Description: strings.Replace(readyBody, "## 열린 결정\n\n없음\n", "## 열린 결정\n\n* 어떤 방식을 쓸까?\n", 1)}, "design_first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := CheckIssueReady(tc.issue)
			if err != nil {
				t.Fatalf("CheckIssueReady: %v", err)
			}
			if verdict.Decision != tc.wantID || verdict.Ready != (tc.wantID == "ready") {
				t.Fatalf("decision %q ready %v, want %q", verdict.Decision, verdict.Ready, tc.wantID)
			}
			// The command's report for the same input must agree on decision and reasons.
			raw, err := json.Marshal(map[string]any{"id": tc.issue.ID, "title": tc.issue.Title, "description": tc.issue.Description, "labels": tc.issue.Labels})
			if err != nil {
				t.Fatal(err)
			}
			_, out, _ := readyCall(string(raw))
			report := decodeReport(t, out)
			if at(report, "decision") != verdict.Decision {
				t.Errorf("the command says %v, the helper %q", at(report, "decision"), verdict.Decision)
			}
			if strings.Join(strs(atList(report, "reasons")), "|") != strings.Join(verdict.Reasons, "|") {
				t.Errorf("reasons differ: command %v helper %v", atList(report, "reasons"), verdict.Reasons)
			}
			if strings.Join(strs(atList(report, "missing")), ",") != strings.Join(verdict.Missing, ",") {
				t.Errorf("missing differ: command %v helper %v", atList(report, "missing"), verdict.Missing)
			}
		})
	}
}

func TestCheckIssueReadyNeverReadsAnException(t *testing.T) {
	// The helper has no exception input: an issue that is not ready stays held, so a batch can not
	// release an issue on a bypass the caller could not have recorded.
	verdict, err := CheckIssueReady(ReadyIssue{ID: "CRW-E", Title: "e", Description: "no sections"})
	if err != nil || verdict.Ready || verdict.Decision != "not_ready" || len(verdict.Missing) != 5 {
		t.Fatalf("verdict %+v err %v", verdict, err)
	}
}

// TestCheckIssueReadyNamesTheEndConditionCommands (CRW-1382 d2): the verdict names the commands of the done condition, read and never
// run, so the flow's design-stage explorer can be given the command it runs once; a body whose done condition names no command names none.
func TestCheckIssueReadyNamesTheEndConditionCommands(t *testing.T) {
	v, err := CheckIssueReady(ReadyIssue{ID: "CRW-A", Title: "a", Description: readyBody})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.EndCondition) != 1 || v.EndCondition[0] != "go test ./internal/skill -run IssueReady" {
		t.Errorf("the end condition commands of the ready body: %q", v.EndCondition)
	}
	none, err := CheckIssueReady(ReadyIssue{ID: "CRW-B", Title: "b", Description: "## 기준\n\n* c1: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.EndCondition) != 0 {
		t.Errorf("a body with no done condition names commands: %q", none.EndCondition)
	}
}
