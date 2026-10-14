package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBatchDecideRefusesAHandoffWithAnUnrunnableRequiredCommand (CRW-1285 (3)): `crw manage batch decide --handoff` applies the check of
// the shipped lane's verifier (flow's CheckHandoff), so a handoff whose milestone requires a command other than crw-dev ci local is
// refused before anything is raised at the relay, not when the night batch's run process reads its milestone.
func TestBatchDecideRefusesAHandoffWithAnUnrunnableRequiredCommand(t *testing.T) {
	auditListHome(t)
	exe, record := batchDecideRelay(t, "raised", 0)
	old := batchDecideConfig
	batchDecideConfig = func(*Env) *Config { return &Config{Relay: coreRelay{State: "/relay/state", Socket: "/relay/sock"}} }
	defer func() { batchDecideConfig = old }()
	e, _, errOut := auditTestEnv(t)
	e.Executable = exe
	e.Now = func() time.Time { return time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC) }
	plan := batchDecidePlan(t, "CRW-11")
	doc := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"P","plan_id":"plan-1","issues":[{"key":"CRW-11","title":"t","criteria":["c"],"milestone":"M1"}],` +
		`"milestones":[{"key":"M1","title":"the milestone","criteria":["done"],"surfaces":["logic"],"grade":"C2","required_commands":["make e2e"]}]}]}`
	handoff := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(handoff, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"decide", "--plan", plan, "--flow-plan", "plan-1", "--milestone", "M1", "--token-budget", "750000", "--deadline", "2026-10-13T06:00:00Z", "--by", "manager-task", "--handoff", handoff}
	if code := batchMain(context.Background(), e, args); code == 0 || !strings.Contains(errOut.String(), "make e2e") {
		t.Errorf("a handoff that requires make e2e: exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(record); err == nil {
		t.Errorf("the relay was asked: %q", coreFakeCalls(t, record))
	}
}
