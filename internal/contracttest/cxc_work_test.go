package contracttest

import (
	"maps"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

func TestWorkCLIClaimKeepsNonCLIRecordsAndChecksTheWarning(t *testing.T) {
	s := cxccorpus.Scenario{Steps: []cxccorpus.Step{
		{CLI: []string{"pabcd", "orchestrate", "--help"}},
		{Hook: "stop-checking-pabcd-continuation"},
		{CLI: []string{"pabcd", "orchestrate", "status", "--json"}},
	}}
	want := map[string]string{
		"steps/0/form": "text", "steps/0/stdout": "crw pabcd orchestrate --help\n", "steps/0/stderr": "",
		"steps/1/form": "text", "steps/1/stdout": "run crw pabcd orchestrate P", "steps/1/stderr": "",
		"steps/2/form": "json", "steps/2/stdout": `{"sessionId":"PABCD-1"}`, "steps/2/stderr": "",
		"tree/ws/state": `{"rationale":"crw pabcd loop init"}`,
		"steps/0/exit":  "0", "steps/1/exit": "0", "steps/2/exit": "0",
	}
	claim := cxcClaim{Set: map[string]string{"steps/0/stdout": "usage: crw pabcd orchestrate\n"}}
	adapted := workCLIClaim(s, want, claim)
	if claim.Set["steps/0/stdout"] != "usage: crw pabcd orchestrate\n" || want["steps/0/stderr"] != "" {
		t.Fatal("the adaptation changed the original claim or fixture")
	}
	got := maps.Clone(want)
	warning := "warning: crw pabcd is deprecated; use \"crw work state\" (alias removed in the next major version)\n"
	got["steps/0/stdout"] = "usage: crw work state\n"
	got["steps/0/stderr"], got["steps/2/stderr"] = warning, warning
	if err := compare(maps.Clone(want), got, adapted, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", warning + warning} {
		broken := maps.Clone(got)
		broken["steps/0/stderr"] = bad
		if err := compare(maps.Clone(want), broken, adapted, false); err == nil || !strings.Contains(err.Error(), "stderr") {
			t.Fatalf("missing/duplicate warning passed: %q, %v", bad, err)
		}
	}
	for _, key := range []string{"steps/1/stdout", "steps/2/stdout", "tree/ws/state", "steps/0/exit"} {
		broken := maps.Clone(got)
		broken[key] += "changed"
		if err := compare(maps.Clone(want), broken, adapted, false); err == nil {
			t.Errorf("the adaptation hid a change to %s", key)
		}
	}
}
