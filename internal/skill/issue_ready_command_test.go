package skill

import (
	"strings"
	"testing"
)

// CRW-1328: the end condition's command is read, never run, by the release gate. A `go test` of a package that builds only with
// -tags dev (internal/dev, cmd/crw-dev) without the tag, and a -run pattern that does not name tests literally (a placeholder, a
// shell variable, a wildcard), are warnings: go test of such a command can pass having run no test. A warning never holds the issue:
// the decision, its reasons and the exit code are what CRW-1318 left them, and the warnings are a field of their own.

// TestIssueReadyWarnsOnAnEndConditionCommandThatMayRunNoTest (CRW-1328): the missing -tags dev and the unfixed -run pattern are each a
// warning of the report and of CheckIssueReady, printed on stderr; a ready issue stays ready with exit 0, a held one keeps its
// decision and reasons, and a command that names its tests and its tag warns of nothing.
func TestIssueReadyWarnsOnAnEndConditionCommandThatMayRunNoTest(t *testing.T) {
	const line = "* `go test ./internal/skill -run IssueReady`가 통과한다."
	if !strings.Contains(readyBody, line) {
		t.Fatal("the ready body has no end condition line to replace")
	}
	body := func(cmd string) string { return strings.Replace(readyBody, line, "* `"+cmd+"`가 통과한다.", 1) }
	cases := []struct {
		name, cmd string
		warn      []string // words every warning of the case names, one entry per warning
	}{
		{"dev package without the tag", "go test ./internal/dev/ci -run '^TestLocal$' -v", []string{"-tags dev"}},
		{"crw-dev without the tag", "go test -count=1 ./cmd/crw-dev/... -run TestMain", []string{"-tags dev"}},
		{"placeholder run", "go test ./internal/flow/cli -run '<the 13 tests above>' -v", []string{"-run"}},
		{"wildcard run", "go test ./internal/flow/cli -run 'TestSched.*'", []string{"-run"}},
		{"both", "go test ./internal/dev/ci -run $TESTS", []string{"-tags dev", "-run"}},
		{"named and tagged", "go test -tags dev ./internal/dev/ci -run '^(TestLocal|TestPlan)$' -v", nil},
		{"tag after the package", "go test ./internal/dev/ci -tags=dev,integration -run TestLocal", nil},
		{"no run", "go test ./internal/skill", nil},
		{"not go test", "go run -tags dev ./cmd/crw-dev ci validate", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := readyCall(readyIssue(t, body(tc.cmd), nil))
			r := decodeReport(t, out)
			if code != 0 || at(r, "decision") != "ready" || len(atList(r, "reasons")) != 0 {
				t.Fatalf("the decision moved: exit %d: %s%s", code, out, errOut)
			}
			warnings := strs(atList(r, "warnings"))
			if len(warnings) != len(tc.warn) {
				t.Fatalf("warnings %q, want %d naming %q", warnings, len(tc.warn), tc.warn)
			}
			for i, w := range warnings {
				if !strings.Contains(w, tc.warn[i]) || !strings.Contains(w, "end condition") {
					t.Errorf("warning %q does not name %q", w, tc.warn[i])
				}
				if !strings.Contains(errOut, "crw skill issue-ready: warning: CRW-SYN "+w+"\n") {
					t.Errorf("the warning %q is not on stderr: %s", w, errOut)
				}
			}
			if len(tc.warn) == 0 && errOut != "" {
				t.Errorf("a command that cannot run nothing warns: %s", errOut)
			}
			verdict, err := CheckIssueReady(ReadyIssue{ID: "CRW-SYN", Title: "synthetic", Description: body(tc.cmd)})
			if err != nil || verdict.Decision != "ready" || !verdict.Ready || strings.Join(verdict.Warnings, "|") != strings.Join(warnings, "|") {
				t.Errorf("CheckIssueReady: %+v %v, the command warned %q", verdict, err, warnings)
			}
		})
	}
	// a held issue keeps its decision and its reasons, and warns besides
	held := strings.Replace(body("go test ./internal/dev/ci"), "## 정한 답\n\n해제 판정은 `crw skill issue-ready check`가 한다. 아키텍트 제안과 부모 결정은 이 절에 적는다.\n\n", "", 1)
	code, out, errOut := readyCall(readyIssue(t, held, nil))
	r := decodeReport(t, out)
	if code != 0 || at(r, "decision") != "not_ready" || strings.Join(strs(atList(r, "missing")), ",") != "decided_answer" || len(strs(atList(r, "warnings"))) != 1 ||
		!strings.Contains(errOut, "warning: CRW-SYN is not_ready: missing decided_answer") {
		t.Errorf("a held issue with a warning: exit %d: %s%s", code, out, errOut)
	}
}
