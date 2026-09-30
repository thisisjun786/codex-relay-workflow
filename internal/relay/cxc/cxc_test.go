package cxc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

type oracleCase struct {
	Function string         `json:"function"`
	Args     []any          `json:"args,omitempty"`
	Keywords map[string]any `json:"kwargs,omitempty"`
}

func Test28_CXCPublicSurfaceLivePython(t *testing.T) {
	cases := []oracleCase{}
	results := []any{}
	add := func(name string, args []any, kwargs map[string]any, result any, err error) {
		cases = append(cases, oracleCase{name, args, kwargs})
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				t.Fatal(err)
			}
			results = append(results, map[string]any{"error": e.Kind, "message": e.Message})
		} else {
			results = append(results, map[string]any{"result": result})
		}
	}
	constants := map[string]any{"PACKAGE": Package, "VERSION": Version, "SOURCES": Sources, "REPORT_STATUSES": ReportStatuses, "MEANING": Meaning, "COMPATIBLE_OUTCOMES": CompatibleOutcomes, "NOT_VERIFICATION": NotVerification, "VERDICT_KINDS": VerdictKinds, "PREFIX": Prefix, "BLOCKERS_MAX": BlockersMax, "WAIT_STATES": WaitStates, "NEVER_AUTHORISES_RERUN": NeverAuthorisesRerun, "DISPATCH_FIELDS": DispatchFields, "DISPATCH_SECTIONS": DispatchSections, "CORRECTION_SECTIONS": CorrectionSections, "SKILL_POINTERS": SkillPointers}
	keys := []string{}
	for k := range constants {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	add("constants", []any{keys}, nil, constants, nil)
	add("provenance", nil, nil, Provenance(), nil)
	for _, source := range append(Sources, Source{Rule: "unknown"}) {
		add("sources_for", []any{source.Rule}, nil, SourcesFor(source.Rule), nil)
	}
	for _, paths := range [][]string{{}, {"unknown"}, {Sources[1].Path, Sources[1].Path}, {Sources[0].Path, Sources[5].Path}} {
		add("affected_by", []any{paths}, nil, AffectedBy(paths), nil)
	}
	values := []any{nil, false, true, 0, 5, "", "unknown", []any{}, map[string]any{}}
	for _, status := range append(append([]any{}, values...), Done, Noop, Blocked, Unsafe, NeedsHuman, BudgetExhausted) {
		add("check_known", []any{status}, nil, nil, CheckKnown(status))
		for _, outcome := range []any{"ready_for_review", "blocked_needs_input", "interrupted", "failed", "verified", nil, 5, []any{}} {
			add("check_status", []any{status, outcome}, nil, nil, CheckStatus(status, outcome))
		}
	}
	facts := append([]any{}, values...)
	for _, key := range slices.Sorted(maps.Keys(NotVerification)) {
		facts = append(facts, key)
	}
	for _, fact := range facts {
		v, e := RefusePromotion(fact)
		add("refuse_promotion", []any{fact}, nil, v, e)
	}
	for _, kind := range append(append([]any{}, values...), Pass, GoWithFixes, Fail) {
		for _, blockers := range []any{nil, false, true, -1, 0, 1, 9999, 10000, json.Number("999999999999999999999999"), json.Number("1.0"), "1"} {
			v, e := VerdictLine(kind, blockers)
			add("verdict_line", []any{kind, blockers}, nil, v, e)
		}
	}
	lines := append(append([]any{}, values...), json.Number("1.5"), "\x1cVERDICT: PASS\x1f", "VERDICT: GO-WITH-FIXES (blockers=\u00b2)", "VERDICT: GO-WITH-FIXES (blockers=\u24601)", "VERDICT: GO-WITH-FIXES (blockers=\u00b2x)", "VERDICT: GO-WITH-FIXES (blockers=\U0001d7cf)")
	for _, line := range []string{"VERDICT: PASS", "  VERDICT: FAIL\n", "VERDICT: GO-WITH-FIXES (blockers=1)", "VERDICT: GO-WITH-FIXES (blockers=9999)", "VERDICT: GO-WITH-FIXES (blockers=10000)", "VERDICT: GO-WITH-FIXES (blockers=0)", "VERDICT: GO-WITH-FIXES (blockers=+1)", "VERDICT: GO-WITH-FIXES (blockers=01)", "VERDICT: GO-WITH-FIXES (blockers=\u0661)", "VERDICT:PASS", "VERDICT: PASS (blockers=1)", "prefix VERDICT: PASS"} {
		lines = append(lines, line)
	}
	for _, line := range lines {
		v, e := ParseVerdictLine(line)
		add("parse_verdict_line", []any{line}, nil, v, e)
	}
	for _, value := range values {
		add("assert_reviewed", []any{value}, nil, nil, AssertReviewed(value))
	}
	names := []string{"terminal_error", "input_requested", "advancing_evidence", "observable", "stagnation_confirmed", "timed_out"}
	add("classify_wait", nil, map[string]any{}, ClassifyWait(nil), nil)
	for mask := 0; mask < 64; mask++ {
		opts := map[string]any{}
		for i, name := range names {
			opts[name] = mask&(1<<i) != 0
		}
		add("classify_wait", nil, opts, ClassifyWait(opts), nil)
	}
	for _, value := range values {
		opts := map[string]any{"terminal_error": value}
		add("classify_wait", nil, opts, ClassifyWait(opts), nil)
	}
	bodies := append(append([]any{}, values...), strings.Join(DispatchSections, "\n"), strings.Join(CorrectionSections, "\n"), "TASK: work\nSCOPE\n bounded\n## MUST DO: tests\n- MUST NOT: push\n**PROOF:** output\nRETURN FORMAT: JSON\nDECISION BOUNDARY: stop", "SUBTASK: wrong\nTASK\nSCOPE: bounded\nmention PROOF: not a section", "TASK\rwork\u2028SCOPE: here", "\x1fTASK: yes\x1f\nCUSTOM: done", "VIOLATED CRITERION: x\nWHAT CHANGED: y\nFIX SCOPE: z\nPRESERVE: p\nREVERIFY AND RETURN: r")
	for _, body := range bodies {
		add("dispatch_problems", []any{body}, nil, DispatchProblems(body), nil)
		add("correction_problems", []any{body}, nil, CorrectionProblems(body), nil)
		sections := []string{"TASK", "CUSTOM"}
		add("section_problems", []any{body, sections}, nil, SectionProblems(body, sections), nil)
	}
	activities := append([]any{}, values...)
	for _, key := range slices.Sorted(maps.Keys(SkillPointers)) {
		activities = append(activities, key)
	}
	for _, activity := range activities {
		v, e := SkillPointer(activity)
		add("skill_pointer", []any{activity}, nil, v, e)
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// The cases are built in a fixed order (map keys sorted), so the recorded answer lines up
	// with them; the key carries the question's digest, so a changed question finds no answer.
	digest := sha256.Sum256(raw)
	out := pyoracle.Answer(t, "capture.py "+hex.EncodeToString(digest[:8]), func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/cxc/testdata/capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%v\n%s", err, out)
		}
		return out, nil
	})
	var want []any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("length: Go %d Python %d", len(got), len(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("case %+v\nGo: %#v\nPython: %#v", cases[i], got[i], want[i])
		}
	}
	t.Logf("compared %d complete results/exceptions", len(cases))
}
