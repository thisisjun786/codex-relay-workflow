package skill

import (
	"os"
	"strings"
	"testing"
)

// CRW-1344: the work is tracked in a GitHub repository, so the two checks that read one issue (issue-ready and issue-size)
// say in their help how a GitHub issue is given to them, and name no other tracker. The mapping is the one command of
// crw-canon/references/tracking-model.md ("Use the tracking access"): gh issue view prints the keys url, title, body and
// labels, and its --jq makes them the id, title, description and labels the checks read.
func TestIssueChecksHelpTakesAGitHubIssue(t *testing.T) {
	for _, family := range []string{"issue-ready", "issue-size"} {
		for _, args := range [][]string{{family, "--help"}, {family, "-h"}} {
			code, help, errOut := call(args, "")
			if code != 0 {
				t.Fatalf("%v: exit %d: %s", args, code, errOut)
			}
			for _, want := range []string{"GitHub issue", "gh issue view", "--jq"} {
				if !strings.Contains(help, want) {
					t.Errorf("%v: the help does not say %q:\n%s", args, want, help)
				}
			}
			if strings.Contains(strings.ToLower(help), "linear") {
				t.Errorf("%v: the help names a tracker that is not the tracking repository:\n%s", args, help)
			}
		}
	}
}

// The mapping the help and the skills give turns the JSON of gh issue view into the keys the checks read; an issue given that way
// is read as it is, so the mapped fixture reads like any other input.
func TestIssueChecksReadTheMappedGitHubIssue(t *testing.T) {
	body := readyBody
	mapped := readyIssue(t, body, map[string]any{"id": "https://github.com/OWNER/NAME/issues/12", "title": "t", "labels": []string{"P1", "defect"}})
	if code, out, errOut := readyCall(mapped); code != 0 || at(decodeReport(t, out), "decision") != "ready" || errOut != "" {
		t.Errorf("issue-ready: exit %d: %s%s", code, out, errOut)
	}
	if code, out, errOut := call([]string{"issue-size", "check"}, mapped); code != 0 || at(decodeReport(t, out), "issue") != "https://github.com/OWNER/NAME/issues/12" {
		t.Errorf("issue-size: exit %d: %s%s", code, out, errOut)
	}
}

// The command in the help is the command of the skill reference, byte for byte, and the skills that run the checks point to it.
func TestIssueChecksHelpAndTrackingModelGiveOneMapping(t *testing.T) {
	const jq = "--jq '{id: .url, title: .title, description: .body, labels: [.labels[].name]}'"
	if !strings.Contains(githubIssueMapping, jq) {
		t.Fatalf("the help's mapping lacks %s", jq)
	}
	for _, rel := range []string{
		"../../plugins/crw/skills/crw-canon/references/tracking-model.md",
	} {
		data, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), jq) || !strings.Contains(string(data), "gh issue view N --repo OWNER/NAME --json url,title,body,labels") {
			t.Errorf("%s does not give the mapping of the help", rel)
		}
	}
}
