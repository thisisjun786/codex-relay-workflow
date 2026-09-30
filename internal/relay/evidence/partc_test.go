package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

const head = "c68be165ae8ee4a645f3266eae3e9c543a851382"

func cleanReview() map[string]any {
	return map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}
}
func run(name, head, conclusion string, attempt int) map[string]any {
	return map[string]any{"runId": "run-" + name, "name": name, "headSha": head, "conclusion": conclusion, "attempt": attempt}
}

func Test24_FGE_2_UnfinishedEnumerationCodes(t *testing.T) {
	total := 2
	x, _ := EnumerateConnection("items", 3, func(any) (Page, error) { return Page{[]any{"a"}, total, "same"}, nil }, func(v any) any { return v })
	whole(t, "FGE-2", []any{[]any{x.Record(), problemRows(x.Problems)}})
}
func Test24_FGE_3_ReviewSetUnstableVerdict(t *testing.T) {
	var rows []any
	for _, s := range []*collectorScript{
		{threads: 3, secondThreads: 2, unresolved: map[int]bool{}},
		{threads: 3, unresolved: map[int]bool{}, secondUnresolved: map[int]bool{2: true}},
	} {
		snapshot, _ := Collect(fixedForge(s), "owner/name", 7)
		rows = append(rows, []any{snapshot["verdict"], snapshot["problems"]})
	}
	whole(t, "FGE-3", rows)
}
func Test24_FGE_6_CheckIdentityAndAttempt(t *testing.T) {
	checks := []any{run("dev-gate", head, "success", 1), run("dev-gate", head, "failure", 2)}
	checks[0].(map[string]any)["runId"] = "run-1"
	checks[1].(map[string]any)["runId"] = "run-1"
	whole(t, "FGE-6", problemRows(ChecksProblems(head, []string{"dev-gate"}, checks)))
}
func Test24_FGE_7_TruncatedChecksUnknown(t *testing.T) {
	checks := make([]any, 121)
	for i := range checks {
		checks[i] = map[string]any{"id": 900 + i, "name": "noise", "head_sha": collectorHead, "status": "completed", "conclusion": "success", "app": map[string]any{"slug": "other"}}
	}
	truncatedChecks := fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, checks: checks})
	truncatedChecks.PageBudget = 1
	a, _ := Collect(truncatedChecks, "owner/name", 7)
	checks = append(checks, map[string]any{"id": 5000, "name": "dev-gate", "head_sha": collectorHead, "status": "completed", "conclusion": "failure", "app": map[string]any{"slug": "other"}})
	b, _ := Collect(fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, checks: checks}), "owner/name", 7)
	runs := make([]any, 150)
	jobs := map[int][]any{}
	for i := range runs {
		id := i + 1
		runs[i] = map[string]any{"id": id, "name": "CI", "head_sha": collectorHead, "workflow_id": id, "event": "pull_request"}
		jobs[id] = []any{map[string]any{"id": id, "name": "dev-gate", "run_attempt": 1, "status": "completed", "conclusion": "success"}}
	}
	truncatedRuns := fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, runs: runs, jobs: jobs})
	truncatedRuns.PageBudget = 1
	c, _ := Collect(truncatedRuns, "owner/name", 7)
	d, _ := Collect(fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, runs: []any{}, jobs: map[int][]any{}, statuses: []any{map[string]any{"context": "dev-gate", "state": "pending", "target_url": "u"}}}), "owner/name", 7)
	whole(t, "FGE-7", []any{a["verdict"], a["problems"], b["verdict"], b["problems"], c["verdict"], c["problems"], d["verdict"], d["problems"]})
}
func Test24_FGE_8_DeclaredEmptyDiffersFromUnknown(t *testing.T) {
	red := []any{run("dev-gate", head, "failure", 1), run("lint", head, "success", 1)}
	whole(t, "FGE-8", []any{problemRows(ChecksProblemsWith(head, nil, red, true, nil)), problemRows(ChecksProblemsWith(head, []string{}, red, true, nil))})
}
func Test24_FGE_9_DisabledReviewerConflict(t *testing.T) {
	optional, _ := Collect(fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, rules: []any{}, runs: []any{}, jobs: map[int][]any{}, checks: []any{map[string]any{"id": 9, "name": "codex", "head_sha": collectorHead, "status": "completed", "conclusion": "failure", "app": map[string]any{"slug": "codex"}}}}), "owner/name", 7)
	rules := []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false, "required_status_checks": []any{map[string]any{"context": "dev-gate"}, map[string]any{"context": "codex"}}}}}
	conflict, _ := Collect(fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, rules: rules, checks: []any{map[string]any{"id": 11, "name": "dev-gate", "head_sha": collectorHead, "status": "completed", "conclusion": "success", "app": map[string]any{"slug": "actions"}}}}), "owner/name", 7)
	handoff := mapOf(conflict["handoff"])
	delete(handoff, "baseVerifiedAt")
	whole(t, "FGE-9", []any{optional["verdict"], optional["problems"], conflict["verdict"], conflict["problems"], handoff, conflict["provenance"]})
}
func Test24_FGE_10_CandidateTable(t *testing.T) {
	var rows []any
	for _, tc := range []struct {
		s       string
		d, m, b bool
	}{{"clean", false, false, false}, {"dirty", false, false, false}, {"behind", false, false, true}, {"marvellous", false, false, false}, {"clean", true, false, false}, {"clean", false, true, false}, {"blocked", false, false, false}, {"has_hooks", false, false, false}} {
		rows = append(rows, problemRows(CandidateProblems(map[string]any{"state": "open", "merged": tc.m, "isDraft": tc.d, "mergeStateStatus": tc.s}, tc.b)))
	}
	whole(t, "FGE-10", rows)
}
func Test24_FGE_11_SupersededRunsField(t *testing.T) {
	whole(t, "FGE-11", []any{[]any{map[string]any{"runId": "1"}}})
}
func Test24_FGE_12_ProviderBinding(t *testing.T) {
	checks := []any{map[string]any{"runId": "run-dev-gate", "name": "dev-gate", "headSha": head, "conclusion": "success", "attempt": 1, "provider": "99"}}
	a := problemRows(ChecksProblemsWith(head, []string{"dev-gate"}, checks, true, map[string][]string{"dev-gate": {"42"}}))
	checks[0].(map[string]any)["provider"] = "42"
	whole(t, "FGE-12", []any{a, problemRows(ChecksProblemsWith(head, []string{"dev-gate"}, checks, true, map[string][]string{"dev-gate": {"42"}}))})
}
func Test24_FGE_13_ChangesRequestedLatestPerAuthor(t *testing.T) {
	r := []any{map[string]any{"author": "anna", "state": "CHANGES_REQUESTED", "submittedAt": "1"}, map[string]any{"author": "anna", "state": "COMMENTED", "submittedAt": "2"}}
	a := problemRows(ReviewStateProblems(r))
	r = append(r, map[string]any{"author": "anna", "state": "APPROVED", "submittedAt": "3"})
	whole(t, "FGE-13", []any{a, problemRows(ReviewStateProblems(r))})
}
func Test24_FGE_14_RestatementProblemCodes(t *testing.T) {
	whole(t, "FGE-14", []any{LateFinding, CandidateMoved, GatesMoved, RecordInvalid, Malformed})
}
func Test24_FGE_15_ReadOnlyArgumentValidation(t *testing.T) {
	var rows []any
	for _, v := range []any{"--owner/name", "owner name/x", "owner", "own/er/name", ""} {
		a, b, e := SplitRepository(v)
		rows = append(rows, errorRow([]any{a, b}, e))
	}
	for _, v := range []any{0, -1, "seven", nil} {
		a, e := PullRequestNumber(v)
		rows = append(rows, errorRow(a, e))
	}
	for _, v := range []any{"../etc", "/dev", "de v", "dev?x", ""} {
		a, e := BranchRef(v)
		rows = append(rows, errorRow(a, e))
	}
	a, e := BranchRef("release/1.2")
	rows = append(rows, errorRow(a, e))
	whole(t, "FGE-15", rows)
}
func Test24_FGE_Gap_CountDisagreesAndBudget(t *testing.T) {
	total := 2
	x, _ := EnumerateConnection("items", 2, func(any) (Page, error) { return Page{[]any{"one"}, total, nil}, nil }, func(v any) any { return v })
	if x.Problems[0].Code != EnumerationCountDisagrees || VerdictOf([]Problem{{Code: UnreadableCode}}) != UnknownVerdict {
		t.Fatal(x.Problems)
	}
}

func Test24_MEE_1_PredicateCorpus(t *testing.T) {
	r := cleanReview()
	a := problemRows(ReviewProblems(r))
	delete(r, "pagesRead")
	whole(t, "MEE-1", []any{a, problemRows(ReviewProblems(r))})
}
func Test24_MEE_3_UnstatedFieldsShortCircuit(t *testing.T) {
	whole(t, "MEE-3", problemRows(ReviewProblems(map[string]any{"hasNextPage": true, "pagesRead": 0})))
}
func Test24_MEE_4_EachReviewRule(t *testing.T) {
	cases := []map[string]any{}
	for _, mutate := range []func(map[string]any){
		func(r map[string]any) { r["hasNextPage"] = true },
		func(r map[string]any) { r["pagesRead"] = 0 },
		func(r map[string]any) { r["threadsSeen"] = []any{"t1", "   "}; r["totalCount"] = 1 },
		func(r map[string]any) { r["threadsSeen"] = []any{"t1", "t1"}; r["totalCount"] = 1 },
		func(r map[string]any) { r["totalCount"] = 14 },
		func(r map[string]any) { r["unresolved"] = 14 },
	} {
		r := cleanReview()
		mutate(r)
		cases = append(cases, r)
	}
	rows := make([]any, 0, len(cases))
	for _, r := range cases {
		problems := ReviewProblems(r)
		rows = append(rows, []any{problemRows(problems), map[string]any{"reason": "merge_review_incomplete", "detail": strings.Join(Details(problems), "; ")}})
	}
	whole(t, "MEE-4", rows)
}
func Test24_MEE_5_ShapeBeforeSemantics(t *testing.T) {
	r := cleanReview()
	r["threadsSeen"] = "ab"
	whole(t, "MEE-5", []any{problemRows(ReviewProblems(r)), problemRows(ShapeProblems(r, []any{}, nil, nil))})
}
func Test24_MEE_6_OmittedAttempt(t *testing.T) {
	e := run("dev-gate", head, "success", 1)
	delete(e, "attempt")
	whole(t, "MEE-6", problemRows(ShapeProblems(cleanReview(), []any{e}, nil, nil)))
}
func Test24_MEE_7_UndeclaredRequired(t *testing.T) {
	red := []any{run("dev-gate", head, "failure", 1), run("lint", head, "success", 1)}
	whole(t, "MEE-7", []any{problemRows(ChecksProblemsWith(head, nil, red, true, nil)), problemRows(ChecksProblemsWith(head, []string{}, red, true, nil))})
}

func Test24_SEV_9_DirectivePointerCoveredByRegistry(t *testing.T) {
	sevDirectiveBytes(t, false)
}
func Test24_SEV_10_DirectiveCLIContract(t *testing.T) {
	sevDirectiveBytes(t, true)
}

// envelopeMessageID is envelope.message_id for well-formed fields: the first 32 hex digits of
// the sha256 of direction|relation|purpose|subject, which the registry's pointer check recomputes.
func envelopeMessageID(direction, relation, purpose, subject string) string {
	sum := sha256.Sum256([]byte(direction + "|" + relation + "|" + purpose + "|" + subject))
	return hex.EncodeToString(sum[:])[:32]
}

// Replay pointer parsing through the registry's real CLI, including the complete
// refusal, rather than just comparing the shared message-id hash. Go sets up and runs every
// command on its own store; Python's answer to the same sequence, run on its own store with the
// recordedAt Go stamped, is recorded (internal/testsupport/pyoracle).
func sevDirectiveBytes(t *testing.T, correlationOnly bool) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binary := filepath.Join(root, "crw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/crw")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	env := append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
	supervise := []string{"linkage-supervise", "--initiative", "INI-1", "--project", "PRJ-1", "--supervisor-task", "supervisor", "--supervisor-host", "host", "--parent-task", "parent", "--parent-host", "host"}
	goState := filepath.Join(root, "go-state")
	setup := exec.Command(binary, append([]string{"relay", "--state", goState}, supervise...)...)
	setup.Env = env
	raw, err := setup.Output()
	if err != nil {
		t.Fatalf("setup: %v %s", err, raw)
	}
	var edge map[string]any
	if err := json.Unmarshal(raw, &edge); err != nil {
		t.Fatal(err)
	}
	link, ok := edge["linkId"].(string)
	if !ok {
		t.Fatalf("link missing: %s", raw)
	}
	base := []string{"linkage-directive", "--scope-kind", "project", "--scope", "PRJ-1", "--from-task", "supervisor", "--from-scope", "INI-1", "--link", link, "--digest", "digest"}
	cases := [][]string{{"--correlation", "msg-1"}}
	if !correlationOnly {
		good := envelopeMessageID("supervisor_to_parent", link, "project_assignment", "digest")
		other := envelopeMessageID("supervisor_to_parent", link, "project_assignment", "another")
		cases = [][]string{{"--reference", "relay-envelope/1|supervisor_to_parent|project_assignment|" + other + "|-"}, {"--reference", "relay-envelope/1|supervisor_to_parent|project_assignment|" + good + "|msg-1"}, {"--purpose", "project_assignment", "--correlation", "-"}}
	}
	code := func(err error) int {
		if err == nil {
			return 0
		}
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		t.Fatal(err)
		return -1
	}
	// Python's store, set up by Python's own linkage-supervise, only when Python is asked.
	python := filepath.Join(repo, ".venv/bin/python")
	pythonState := filepath.Join(root, "state")
	pythonReady := false
	pythonSetup := func() error {
		if pythonReady {
			return nil
		}
		setup := exec.Command(python, append([]string{"-c", `import sys
from codex_session_relay import cli
raise SystemExit(cli.main(sys.argv[1:]))`, "--state", pythonState}, supervise...)...)
		setup.Env = env
		if raw, err := setup.Output(); err != nil {
			return fmt.Errorf("Python setup: %v %s", err, raw)
		}
		pythonReady = true
		return nil
	}
	for _, extra := range cases {
		args := append(append([]string{}, base...), extra...)
		goCmd := exec.Command(binary, append([]string{"relay", "--state", goState}, args...)...)
		goCmd.Env = env
		got, goErr := goCmd.CombinedOutput()
		var reply map[string]any
		if err := json.Unmarshal(got, &reply); err != nil {
			t.Fatal(err)
		}
		at, _ := reply["recordedAt"].(string)
		var want struct {
			Code   int    `json:"code"`
			Output string `json:"output"`
		}
		pyoracle.JSON(t, strings.Join(extra, " "), &want, func() (any, error) {
			if err := pythonSetup(); err != nil {
				return nil, err
			}
			pyCmd := exec.Command(python, append([]string{"-c", `import sys
from codex_session_relay import cli,clock
clock.SystemClock.iso=lambda self: sys.argv[1]
raise SystemExit(cli.main(sys.argv[2:]))`, at, "--state", pythonState}, args...)...)
			pyCmd.Env = env
			out, pyErr := pyCmd.CombinedOutput()
			exit := 0
			if pyErr != nil {
				e, ok := pyErr.(*exec.ExitError)
				if !ok {
					return nil, pyErr
				}
				exit = e.ExitCode()
			}
			return map[string]any{"code": exit, "output": string(out)}, nil
		}, pyoracle.Substitute(at, "<recordedAt>"))
		if code(goErr) != want.Code || string(got) != want.Output {
			t.Errorf("directive CLI byte diff %v\nGo(%d): %s\nPython(%d): %s", extra, code(goErr), got, want.Code, want.Output)
		}
	}
}

func TestCaptureJSONStable(t *testing.T) {
	var v any
	if json.Unmarshal([]byte(`{"a":1}`), &v) != nil || !strings.Contains(Dumps(v, true, true, false), "a") {
		t.Fatal(v)
	}
}
