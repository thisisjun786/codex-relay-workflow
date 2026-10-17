package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Keep the accepted public name map independent of the dispatch table.
var workNames = [][2]string{
	{"freeze", "freeze"}, {"plan", "outline"}, {"receipt", "receipt"},
	{"evidence", "evidence"}, {"memory", "memory"}, {"reset", "reset"},
	{"config", "config"}, {"scan", "scan"}, {"review-round", "assessment"},
	{"metric", "metric"}, {"divergence", "divergence"}, {"loop", "continue"},
	{"orchestrate", "state"},
}

func TestWorkAliasRootAndSingleLineWarning(t *testing.T) {
	for _, suffix := range [][]string{nil, {"--help"}, {"unknown"}, {"unknown\nverb"}} {
		code, out, errOut := workCall(append([]string{"work"}, suffix...)...)
		oldCode, oldOut, oldErr := workCall(append([]string{"pabcd"}, suffix...)...)
		warning, rest, found := strings.Cut(oldErr, "\n")
		if !found || !strings.HasPrefix(warning, "warning: crw pabcd is deprecated; use ") ||
			!strings.Contains(warning, "crw work") || oldCode != code || oldOut != out || rest != errOut {
			t.Errorf("%v: canonical=(%d %q %q), alias=(%d %q %q)", suffix, code, out, errOut, oldCode, oldOut, oldErr)
		}
	}
}

func TestWorkAliasReadsAndWritesTheExistingStore(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	// The namespace rename must not rewrite user values or rename state keys/paths.
	id := "pabcd-fixture"
	if err := state.WriteState(root, state.DefaultState(id, "pabcd-objective")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(state.StatePath(root, id))
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := workCall("work", "state", "status", "--session", id, "--json")
	oldCode, oldOut, oldErr := workCall("pabcd", "orchestrate", "status", "--session", id, "--json")
	_, rest, found := strings.Cut(oldErr, "\n")
	var status struct{ SessionID, Phase string }
	if json.Unmarshal([]byte(out), &status) != nil || status.SessionID != id || status.Phase != "IDLE" ||
		code != 0 || oldCode != code || out != oldOut || errOut != "" || !found || rest != "" {
		t.Fatalf("status differs: %d %q %q / %d %q %q", code, out, errOut, oldCode, oldOut, oldErr)
	}
	after, err := os.ReadFile(state.StatePath(root, id))
	if err != nil || string(after) != string(before) {
		t.Fatalf("status changed stored state: %v", err)
	}
	code, out, errOut = workCall("work", "config", "interview", "always")
	before, err = os.ReadFile(filepath.Join(root, "crw.json"))
	if err != nil || code != 0 || errOut != "" {
		t.Fatalf("config write: %d %q %q, %v", code, out, errOut, err)
	}
	oldCode, oldOut, oldErr = workCall("pabcd", "config", "interview", "always")
	_, rest, found = strings.Cut(oldErr, "\n")
	after, err = os.ReadFile(filepath.Join(root, "crw.json"))
	if err != nil || string(after) != string(before) || oldCode != code || oldOut != out || !found || rest != "" {
		t.Fatalf("alias changed config behavior: %d %q %q, %v", oldCode, oldOut, oldErr, err)
	}
}

func TestWorkGateErrorUsesCanonicalHint(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_THREAD_ID", "")
	s := state.DefaultState("s1", "")
	s.Phase = state.PhaseP
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := workCall("work", "state", "A", "--session", "s1", "--attest", `{"from":"P","to":"A","did":"wrote the task outline"}`)
	if code != 1 || !strings.Contains(out, "crw work outline init") || strings.Contains(strings.ToLower(out+errOut), "pabcd") {
		t.Fatalf("gate hint: %d %q %q", code, out, errOut)
	}
	if got := state.ReadState(root, "s1"); got.Phase != state.PhaseP {
		t.Fatalf("refused transition changed phase: %s", got.Phase)
	}
}

func TestWorkInheritedRefusalsUseCanonicalHints(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_THREAD_ID", "")
	code, out, errOut := workCall("work", "continue", "init", "--objective", "test task", "--session", "s1")
	if code != 1 || !strings.Contains(out, "crw work receipt test") || strings.Contains(strings.ToLower(out+errOut), "pabcd") {
		t.Errorf("source refusal: %d %q %q", code, out, errOut)
	}
	s := state.DefaultState("s1", "")
	s.Phase, s.Interview = state.PhaseI, interview.DefaultInterview(1)
	s.Interview.ScanRounds = 1
	for _, dim := range interview.DimensionOrder() {
		s.Interview.Dimensions.Score(dim).Level = interview.LevelHigh
	}
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = workCall("work", "state", "P", "--session", "s1")
	if code != 1 || !strings.Contains(out, "crw work scan record") || strings.Contains(strings.ToLower(out+errOut), "pabcd") {
		t.Errorf("interview refusal: %d %q %q", code, out, errOut)
	}
}

func TestWorkReceiptPreservesChildStreams(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	receiptSignalRepo(t, root)
	t.Chdir(root)
	t.Setenv("CODEX_THREAD_ID", "")
	epoch := "check-epoch"
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive, s.CheckEpoch = state.PhaseC, true, &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	args := []string{"receipt", "test", "--session", "s1", "--", "/bin/sh", "-c", "printf 'crw pabcd loop\\n'; printf 'crw pabcd plan\\n' >&2; exit 3"}
	code, out, errOut := workCall(append([]string{"work"}, args...)...)
	oldCode, oldOut, oldErr := workCall(append([]string{"pabcd"}, args...)...)
	_, rest, found := strings.Cut(oldErr, "\n")
	if code != 3 || !strings.HasPrefix(out, "crw pabcd loop\n") || errOut != "crw pabcd plan\n" ||
		oldCode != code || oldOut != out || !found || rest != errOut {
		t.Fatalf("child streams changed: %d %q %q / %d %q %q", code, out, errOut, oldCode, oldOut, oldErr)
	}
}

func workCall(args ...string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(context.Background(), "crw", args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestWorkCanonicalHelpAndErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"--help"}, {"work", "--help"}, {"work"}, {"work", "unknown"}} {
		_, out, errOut := workCall(args...)
		if strings.Contains(strings.ToLower(out+errOut), "pabcd") {
			t.Errorf("%v exposes the legacy name: %s%s", args, out, errOut)
		}
	}
	code, out, errOut := workCall("work", "--help")
	want := "usage: crw work [-h] {freeze,outline,receipt,evidence,memory,reset,config,scan,assessment,metric,divergence,continue,state} ...\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("work help: %d %q %q; want %q", code, out, errOut, want)
	}
}

func TestWorkLegacyAliasParity(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, names := range workNames {
		for _, suffix := range [][]string{{"--help"}, {"unknown"}} {
			if names[0] == "freeze" {
				// Freeze ignores unknown positional words; preview so both calls see the same state.
				suffix = append(suffix, "--dry-run")
			}
			t.Run(names[0]+"/"+suffix[0], func(t *testing.T) {
				code, out, errOut := workCall(append([]string{"work", names[1]}, suffix...)...)
				oldCode, oldOut, oldErr := workCall(append([]string{"pabcd", names[0]}, suffix...)...)
				warning, rest, found := strings.Cut(oldErr, "\n")
				if !found || !strings.Contains(warning, "deprecated") || !strings.Contains(warning, "crw work "+names[1]) {
					t.Errorf("missing replacement warning: %q", oldErr)
				}
				if oldCode != code || oldOut != out || rest != errOut {
					t.Errorf("alias differs: old=(%d %q %q), new=(%d %q %q)", oldCode, oldOut, rest, code, out, errOut)
				}
				if strings.Contains(strings.ToLower(out+errOut), "pabcd") {
					t.Errorf("legacy name in public help/error: %q %q", out, errOut)
				}
				if strings.Contains(errOut, "argument verb: invalid choice") {
					t.Errorf("canonical verb is not registered: %s", errOut)
				}
			})
		}
	}
}
