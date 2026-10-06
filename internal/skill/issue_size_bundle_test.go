package skill

import (
	"strings"
	"testing"
)

// The advisory size check (CRW-808). The command reports what the issue states and the
// concept-boundary questions; it never answers split_recommended and never exits non-zero on the
// count alone. A --bundle-reason records why an over-baseline bundle is accepted.

// bundleReasonCall runs the issue-size check with input on stdin.
func bundleReasonCall(input string, args ...string) (int, string, string) {
	return call(append([]string{"issue-size", "check"}, args...), input)
}

func TestIssueSizeIsAdvisoryOnTheCountAlone(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		args []string
		want string
	}{
		{"under every baseline", structuredIssue("CRW-SYN", 3, 0, nil), nil, "ok"},
		{"over a baseline without a reason", structuredIssue("CRW-SYN", 9, 0, nil), nil, "over_line"},
		{"over a baseline with a reason", structuredIssue("CRW-SYN", 9, 0, nil), []string{"--bundle-reason", "one package, one contract, verified together"}, "over_line_accepted"},
		{"over a baseline with an empty reason", structuredIssue("CRW-SYN", 9, 0, nil), []string{"--bundle-reason", "   "}, "over_line"},
		{"a scaled estimate over the ceiling", structuredIssue("CRW-SYN", 3, 0, map[string]any{"scope": "estimate 900 lines"}), nil, "over_line"},
	} {
		code, out, errOut := bundleReasonCall(test.in, test.args...)
		if code != 0 || errOut != "" {
			t.Errorf("%s: exit %d, stderr %q, want exit 0\n%s", test.name, code, errOut, out)
			continue
		}
		r := decodeReport(t, out)
		if got := at(r, "decision"); got != test.want {
			t.Errorf("%s: decision %v, want %s\n%s", test.name, got, test.want, out)
		}
		if strings.Contains(out, "split_recommended") {
			t.Errorf("%s: the report still names split_recommended:\n%s", test.name, out)
		}
		if _, has := r["assignable"]; has {
			t.Errorf("%s: the report still carries assignable:\n%s", test.name, out)
		}
		if at(r, "schema") != "crw-issue-size-check/2" {
			t.Errorf("%s: schema %v, want crw-issue-size-check/2", test.name, at(r, "schema"))
		}
	}
}

func TestIssueSizeRecordsTheBundleReason(t *testing.T) {
	const reason = "same fault class in three files of one package"
	code, out, errOut := bundleReasonCall(structuredIssue("CRW-SYN", 9, 0, nil), "--bundle-reason", reason)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q\n%s", code, errOut, out)
	}
	r := decodeReport(t, out)
	if got := at(r, "bundle_reason"); got != reason {
		t.Errorf("bundle_reason %v, want %q", got, reason)
	}
	// The reason is recorded even when the issue is under every baseline.
	code, out, _ = bundleReasonCall(structuredIssue("CRW-SYN", 3, 0, nil), "--bundle-reason", reason)
	if code != 0 {
		t.Fatalf("exit %d on an ok issue with a reason\n%s", code, out)
	}
	r = decodeReport(t, out)
	if got := at(r, "bundle_reason"); got != reason || at(r, "decision") != "ok" {
		t.Errorf("an ok issue keeps its reason: %s", out)
	}
	// Without the flag the field is absent.
	code, out, _ = bundleReasonCall(structuredIssue("CRW-SYN", 9, 0, nil))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, has := decodeReport(t, out)["bundle_reason"]; has {
		t.Errorf("bundle_reason is present without the flag: %s", out)
	}
}

func TestIssueSizeShowsTheConceptQuestions(t *testing.T) {
	code, out, errOut := bundleReasonCall(structuredIssue("CRW-SYN", 9, 0, nil))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q\n%s", code, errOut, out)
	}
	r := decodeReport(t, out)
	questions := atList(r, "concept_questions")
	if len(questions) < 4 {
		t.Fatalf("concept_questions has %d items, want the boundary questions:\n%s", len(questions), out)
	}
	words := make([]string, 0, len(questions))
	for _, q := range questions {
		if s, ok := q.(string); ok {
			words = append(words, s)
		}
	}
	joined := strings.ToLower(strings.Join(words, " "))
	for _, want := range []string{"concept", "verif"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the concept questions do not mention %q:\n%s", want, out)
		}
	}
	// The estimate stays a displayed signal.
	code, out, _ = bundleReasonCall(structuredIssue("CRW-SYN", 3, 0, map[string]any{"scope": "estimate 900 lines"}))
	if code != 0 {
		t.Fatalf("exit %d on a scaled estimate\n%s", code, out)
	}
	r = decodeReport(t, out)
	if at(r, "estimate") == nil {
		t.Errorf("a stated estimate is not shown:\n%s", out)
	}
	if at(r, "decision") != "over_line" {
		t.Errorf("a scaled estimate over the ceiling reads %v, want over_line", at(r, "decision"))
	}
}
