package manage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1363: unknown required commands survive the handoff and become a verifier
// warning with the configured command's record. Empty commands still fail before
// the relay is asked to record a decision.
func TestBatchDecideRequiredCommandsWarnAndRecordOrRefuseEmpty(t *testing.T) {
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
	if code := batchMain(context.Background(), e, args); code != 0 {
		t.Fatalf("a handoff that requires make e2e: exit %d: %s", code, errOut.String())
	}
	calls := coreFakeCalls(t, record)
	if len(calls) != 2 || !strings.Contains(strings.Join(calls[0], " "), handoff) {
		t.Fatalf("unknown command's handoff was not recorded: %q", calls)
	}

	// Exercise the same repository verifier the shipped lane uses, retaining the
	// unknown requirement as a warning and the actual command as receipt evidence.
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "dev")
	git("-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "--allow-empty", "-m", "base")
	head := git("rev-parse", "HEAD")
	command := "printf foundation"
	config, err := json.Marshal(map[string]any{"repositories": map[string]any{repo: map[string]string{"verify_command": command}}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", configPath)
	v := ops.RepositoryVerifier{Commands: crwconfig.RepositoryCommands{VerifyCommand: command}}
	out, err := v.Verify(context.Background(), ops.VerifyRequest{Worktree: repo, Head: head, Base: head, Command: "make e2e"})
	if err != nil || !out.Pass || !strings.Contains(out.Note, `unknown required command "make e2e"`) {
		t.Fatalf("unknown requirement warning: %+v: %v", out, err)
	}
	receipt, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil || receipt.CommandVerification == nil || receipt.CommandVerification.Command != command || receipt.Result != "pass" || receipt.Digest != out.Digest {
		t.Fatalf("configured verification record: %+v: %v", receipt, err)
	}

	for _, command := range []string{"", "   "} {
		if err := os.WriteFile(handoff, []byte(strings.Replace(doc, "make e2e", command, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		errOut.Reset()
		if code := batchMain(context.Background(), e, args); code == 0 || !strings.Contains(errOut.String(), "blank") {
			t.Errorf("empty required command %q: exit %d: %s", command, code, errOut.String())
		}
		if got := coreFakeCalls(t, record); len(got) != len(calls) {
			t.Fatalf("empty command asked the relay: %q", got)
		}
	}
}
