package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1205: `crw manage batch decide` records the night batch decision through the relay's own decision commands, in the order and
// with the arguments the relay takes, and refuses what a night run cannot start from. The relay is a fake executable that records
// its arguments and answers as the relay does.

// batchDecideRelay writes a fake crw whose relay answers decision-raise with the state given and decision-answer with a record, and
// records one JSON line of arguments per call.
func batchDecideRelay(t *testing.T, raiseState string, answerExit int) (exe, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls.jsonl")
	exe = filepath.Join(dir, "crw")
	script := `#!/bin/sh
printf '[' >> ` + coreShellQuote(record) + `
sep=
for a in "$@"; do printf '%s"%s"' "$sep" "$(printf '%s' "$a" | sed 's/\\/\\\\/g; s/"/\\"/g')" >> ` + coreShellQuote(record) + `; sep=,; done
printf ']\n' >> ` + coreShellQuote(record) + `
for a in "$@"; do
  case "$a" in
    decision-raise) printf '{"decisionId":"ud-night-1","fingerprint":"f","merged":false,"state":"` + raiseState + `"}'; exit 0;;
    decision-answer) if [ ` + coreShellQuote(string(rune('0'+answerExit))) + ` != 0 ]; then printf '{"ok":false,"reason":"authority_unverified"}'; exit 1; fi; printf '{"decisionId":"ud-night-1","state":"answered"}'; exit 0;;
  esac
done
exit 0
`
	syscall.ForkLock.RLock()
	err := os.WriteFile(exe, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func batchDecidePlan(t *testing.T, selected ...string) string {
	t.Helper()
	plan := BatchPlanDocument{Schema: batchPlanSchema, Label: "night-2026-10-12", Selected: []BatchSelected{}, Held: []BatchHeld{}}
	for _, key := range selected {
		plan.Selected = append(plan.Selected, BatchSelected{Issue: key, Title: "title " + key})
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(batchJSON(t, plan)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// batchDecideHandoff writes a crw-plan handoff whose project is plan-1 with milestone M1 holding the issues, and returns its path.
func batchDecideHandoff(t *testing.T, milestone string, issues ...string) string {
	t.Helper()
	var list []string
	for _, key := range issues {
		list = append(list, `{"key":"`+key+`","title":"title `+key+`","criteria":["criterion of `+key+`"],"milestone":"`+milestone+`"}`)
	}
	doc := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"P","plan_id":"plan-1","issues":[` + strings.Join(list, ",") + `],` +
		`"milestones":[{"key":"` + milestone + `","title":"the milestone","criteria":["done"],"surfaces":["logic"],"grade":"C2","required_commands":[]}]}]}`
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBatchDecideRecordsTheNightBatchDecision(t *testing.T) {
	auditListHome(t)
	exe, record := batchDecideRelay(t, "raised", 0)
	old := batchDecideConfig
	batchDecideConfig = func(*Env) *Config { return &Config{Relay: coreRelay{State: "/relay/state", Socket: "/relay/sock"}} }
	defer func() { batchDecideConfig = old }()
	e, out, errOut := auditTestEnv(t)
	e.Executable = exe
	e.Now = func() time.Time { return time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC) }
	plan := batchDecidePlan(t, "CRW-11", "CRW-12")
	handoff := batchDecideHandoff(t, "M1", "CRW-11", "CRW-12")
	handoffRaw, _ := os.ReadFile(handoff)
	handoffSum := sha256.Sum256(handoffRaw)
	args := []string{"decide", "--plan", plan, "--flow-plan", "plan-1", "--milestone", "M1", "--token-budget", "750000", "--deadline", "2026-10-13T06:00:00Z", "--cap", "2", "--by", "manager-task", "--handoff", handoff}
	if code := batchMain(context.Background(), e, args); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
	calls := coreFakeCalls(t, record)
	// the doctor is not asked: the state is configured
	if len(calls) != 2 {
		t.Fatalf("%d relay calls: %q", len(calls), calls)
	}
	raise, answer := calls[0], calls[1]
	if strings.Join(raise[:6], " ") != "relay --state /relay/state --socket /relay/sock decision-raise" {
		t.Errorf("the raise: %q", raise)
	}
	flags := map[string][]string{}
	for i := 6; i < len(raise); i += 2 {
		flags[raise[i]] = append(flags[raise[i]], raise[i+1])
	}
	var doc struct {
		Schema, Label   string
		Issues          []string
		Plan, Milestone string
		TokenBudget     int64 `json:"token_budget"`
		Deadline        string
		Cap             int
		Handoff         struct{ Path, SHA256 string }
	}
	if err := json.Unmarshal([]byte(flags["--context"][0]), &doc); err != nil || doc.Schema != "crw-night-batch/1" || doc.Label != "night-2026-10-12" || strings.Join(doc.Issues, ",") != "CRW-11,CRW-12" ||
		doc.Plan != "plan-1" || doc.Milestone != "M1" || doc.TokenBudget != 750000 || doc.Deadline != "2026-10-13T06:00:00Z" || doc.Cap != 2 ||
		doc.Handoff.Path != handoff || doc.Handoff.SHA256 != hex.EncodeToString(handoffSum[:]) {
		t.Errorf("the context: %s %v", flags["--context"], err)
	}
	if flags["--kind"][0] != "policy" || flags["--authority"][0] != "delegated-management" || flags["--needed-by"][0] != "2026-10-13T06:00:00Z" || flags["--origin-project"][0] != "plan-1" || flags["--source"][0] != "night_batch=night-2026-10-12" || len(flags["--option"]) != 2 ||
		!strings.HasPrefix(flags["--option"][0], "run=") || !strings.HasPrefix(flags["--option"][1], "skip=") {
		t.Errorf("the raise flags: %v", flags)
	}
	if strings.Join(answer, " ") != "relay --state /relay/state --socket /relay/sock decision-answer --decision ud-night-1 --option run --by manager-task --via management-message --authority delegated-management" {
		t.Errorf("the answer: %q", answer)
	}
	var printed struct {
		DecisionID string `json:"decision_id"`
		State      string
		Start      string
		Issues     []string
	}
	if err := json.Unmarshal([]byte(out.String()), &printed); err != nil || printed.DecisionID != "ud-night-1" || printed.State != "answered" || !strings.Contains(printed.Start, "crw flow start night-batch --decision ud-night-1") || len(printed.Issues) != 2 {
		t.Errorf("the answer printed: %s %v", out.String(), err)
	}

	// a record that is answered already is not answered again
	exe2, record2 := batchDecideRelay(t, "answered", 0)
	e.Executable = exe2
	out.Reset()
	if code := batchMain(context.Background(), e, args); code != 0 || len(coreFakeCalls(t, record2)) != 1 || !strings.Contains(out.String(), `"state":"answered"`) {
		t.Errorf("an answered record: exit %d, %d calls, %s", code, len(coreFakeCalls(t, record2)), out.String())
	}

	// the relay refuses the answer (the recorder is not the manager's seat): exit 3 with the relay's words
	exe3, _ := batchDecideRelay(t, "raised", 1)
	e.Executable = exe3
	errOut.Reset()
	if code := batchMain(context.Background(), e, args); code != 3 || !strings.Contains(errOut.String(), "the relay refused decision-answer") || !strings.Contains(errOut.String(), "authority_unverified") {
		t.Errorf("a refused answer: exit %d %q", code, errOut.String())
	}

	// what a night run cannot start from is refused before the relay is asked
	_, record4 := batchDecideRelay(t, "raised", 0)
	e.Executable = exe
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"no budget", []string{"--token-budget", "0"}, "above zero"},
		{"a budget that is no number", []string{"--token-budget", "lots"}, "above zero"},
		{"a deadline that passed", []string{"--deadline", "2026-10-12T19:00:00Z"}, "has passed"},
		{"a deadline that is no time", []string{"--deadline", "morning"}, "not an RFC 3339 time"},
		{"no issue selected", []string{"--plan", batchDecidePlan(t)}, "selects no issue"},
		{"a document that is no plan", []string{"--plan", filepath.Join(t.TempDir(), "missing.json")}, "no such file"},
		{"no manager task", []string{"--by", " "}, "names the manager task"},
		{"no handoff", []string{"--handoff", " "}, "--handoff needs a value"},
		{"a handoff that cannot be read", []string{"--handoff", filepath.Join(t.TempDir(), "missing.json")}, "no such file"},
		{"a handoff without the milestone", []string{"--handoff", batchDecideHandoff(t, "M2", "CRW-11")}, "no entry of milestone"},
		{"a handoff of no plan", []string{"--flow-plan", "plan-9"}, "no project of plan"},
	}
	for _, tc := range refused {
		errOut.Reset()
		argv := batchReplace(args, tc.args)
		if code := batchMain(context.Background(), e, argv); code != usageExit || !strings.Contains(errOut.String(), tc.want) {
			t.Errorf("%s: exit %d %q", tc.name, code, errOut.String())
		}
	}
	if _, err := os.Stat(record4); err == nil {
		t.Errorf("a refused decide asked the relay: %q", coreFakeCalls(t, record4))
	}
}

// batchReplace is args with the options of with in place of the ones of the same name.
func batchReplace(args, with []string) []string {
	out := append([]string{}, args...)
	for i := 0; i+1 < len(with); i += 2 {
		replaced := false
		for j := 0; j+1 < len(out); j++ {
			if out[j] == with[i] {
				out[j+1], replaced = with[i+1], true
			}
		}
		if !replaced {
			out = append(out, with[i], with[i+1])
		}
	}
	return out
}
