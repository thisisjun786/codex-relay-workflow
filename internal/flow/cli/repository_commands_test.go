package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

func repositoryCommandLab(t *testing.T, failing bool) *laneLab {
	t.Helper()
	l := newLaneLabFor(t, labOwner, "dev", labIssue{Key: "A-1", Milestone: "M1"})
	l.git(l.repo, "checkout", "dev")
	l.git(l.repo, "config", "user.name", "repository-command-test")
	l.git(l.repo, "config", "user.email", "repository-command-test@invalid")
	if err := os.Mkdir(filepath.Join(l.repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	foundation := "#!/bin/sh\necho foundation-ran\n"
	if failing {
		foundation += "exit 7\n"
	}
	files := map[string]string{
		"scripts/foundation.sh": foundation,
		"scripts/integrate.sh":  "#!/bin/sh\nset -eu\ngit show-ref --verify refs/heads/\"$1\"\ntest \"$(git branch --show-current)\" = dev\ngit merge --no-ff --no-edit -m repo-integrate \"$1\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(l.repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l.git(l.repo, "add", "scripts")
	l.git(l.repo, "commit", "-m", "repository commands")
	config := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"repositories": map[string]any{l.repo: map[string]string{
		"verify_command": "sh scripts/foundation.sh", "integrate_command": "sh scripts/integrate.sh",
	}}})
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	if err := os.WriteFile(l.file, l.handoffBytes(func(in *compile.Input) {
		in.Projects[0].Milestones[0].RequiredCommands = []string{"sh scripts/foundation.sh"}
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		return BuildLane(ctx, req, LaneOptions{SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l},
			UnitCheck: l.uc.seam(), Crosscheck: l.cc.seam(), Relay: func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }})
	}
	return l
}

func TestRepositoryCommandsVerifyAcceptAndIntegrate(t *testing.T) {
	for _, required := range []string{"sh scripts/foundation.sh", "make e2e", ""} {
		t.Run("required="+required, func(t *testing.T) {
			testRepositoryCommandsVerifyAcceptAndIntegrate(t, required)
		})
	}
}

func testRepositoryCommandsVerifyAcceptAndIntegrate(t *testing.T, required string) {
	t.Helper()
	l := repositoryCommandLab(t, false)
	if err := os.WriteFile(l.file, l.handoffBytes(func(in *compile.Input) {
		in.Projects[0].Milestones[0].RequiredCommands = nil
		if required != "" {
			in.Projects[0].Milestones[0].RequiredCommands = []string{required}
		}
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	v := l.closed(l.runToEnd(spec.RunID))
	if tip := l.git(l.repo, "rev-parse", "dev"); tip != v.Tip {
		t.Fatalf("dev %s != verified %s", tip, v.Tip)
	}
	if msg := l.git(l.repo, "log", "-1", "--format=%s", "dev"); msg != "repo-integrate" {
		t.Fatalf("integration command did not land: %s", msg)
	}
	verified := 0
	for _, row := range mustRows(l, "crw.verify") {
		data := row.Data
		if data["command"] != "sh scripts/foundation.sh" {
			t.Fatalf("verification command: %s", row.Data)
		}
		raw, _ := data["record"].(string)
		var rec struct {
			Tree    string `json:"treeHash"`
			Result  string `json:"result"`
			Command struct {
				Command   string `json:"command"`
				ExitCode  *int   `json:"exitCode"`
				LogDigest string `json:"logDigest"`
			} `json:"commandVerification"`
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Result != "pass" || rec.Tree == "" || rec.Command.Command != data["command"] || rec.Command.ExitCode == nil || *rec.Command.ExitCode != 0 || !strings.HasPrefix(rec.Command.LogDigest, "sha256:") {
			t.Fatalf("record: %s", raw)
		}
		verified++
	}
	if verified == 0 || len(mustRows(l, "crw.accept")) == 0 {
		t.Fatal("no verification or acceptance")
	}
}

func TestRepositoryCommandFailureRetainsRecordAndCannotAccept(t *testing.T) {
	l := repositoryCommandLab(t, true)
	before := l.git(l.repo, "rev-parse", "dev")
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	_ = l.runToEnd(spec.RunID)
	rows := mustRows(l, "crw.verify")
	if len(rows) == 0 {
		t.Fatal("verification never ran")
	}
	raw, _ := rows[0].Data["record"].(string)
	rec, err := dagsched.DecodeVerificationRecord([]byte(raw))
	if err != nil || rec.Result != "fail" || rec.CommandVerification == nil || rec.CommandVerification.ExitCode == nil || *rec.CommandVerification.ExitCode != 7 || rec.CommandVerification.Command != "sh scripts/foundation.sh" {
		t.Fatalf("failure record missing: %s", rows[0].Data)
	}
	if len(mustRows(l, "crw.accept")) != 0 || l.git(l.repo, "rev-parse", "dev") != before {
		t.Fatal("failed verification accepted or moved dev")
	}
}
