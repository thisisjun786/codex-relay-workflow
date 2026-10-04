package mergeturn_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// CRW-538 through the relay CLI: the second request is exit 2 with the reason and a detail that names the live turn,
// and the repeated request for the same pull request is exit 0 with the same turn.
func TestLivePullRequestThroughTheCLI(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	run := func(args ...string) (int, map[string]any) {
		t.Helper()
		var out, errs bytes.Buffer
		code := cli.Execute(context.Background(), append([]string{"--state", state}, args...), &out, &errs)
		var answer map[string]any
		if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", err, code, out.String(), errs.String())
		}
		return code, answer
	}
	request := func(pr, head string) (int, map[string]any) {
		return run("merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev", "--project", "PRJ-A",
			"--task", "task-alpha", "--host", "host-a", "--head", head, "--pr", pr, "--relationship", "rel-"+pr, "--ready")
	}
	if code, bound := run("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha", "--host", "host-a"); code != 0 {
		t.Fatalf("bind: %d %v", code, bound)
	}
	code, first := request("500", "head-500")
	turn, _ := first["turnId"].(string)
	if code != 0 || turn == "" {
		t.Fatalf("first request: %d %v", code, first)
	}
	code, again := request("500", "head-500")
	if code != 0 || again["turnId"] != turn || again["alreadyClaimed"] != true {
		t.Fatalf("the same pull request again: %d %v", code, again)
	}
	code, refused := request("501", "head-501")
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["ok"] != false || refused["reason"] != "disposition_conflict" || !strings.Contains(detail, turn) || !strings.Contains(detail, "pull request 500") || !strings.Contains(detail, "place 1 of 1") {
		t.Fatalf("another pull request: exit %d %v", code, refused)
	}
}
