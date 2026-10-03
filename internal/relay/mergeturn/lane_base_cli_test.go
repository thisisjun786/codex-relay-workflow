package mergeturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// CRW-403 through the relay's own command line, on a temporary store and a temporary local
// repository: the production wiring (merge-turn-check builds its reader from TargetReader) and the
// JSON a parent reads.
func TestCRW403_CLIRestatesAfterOutOfLaneMerge(t *testing.T) {
	g := newLBGit(t)
	state := filepath.Join(t.TempDir(), "state")
	call := func(args ...string) (int, map[string]any) {
		t.Helper()
		var out, stderr bytes.Buffer
		code := dispatch.Execute(context.Background(), "codex-session-relay", append([]string{"--state", state}, args...), &out, &stderr)
		if stderr.Len() > 0 {
			t.Fatalf("%v stderr: %s", args, stderr.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("%v: %s: %v", args, out.String(), err)
		}
		return code, payload
	}
	must := func(args ...string) map[string]any {
		t.Helper()
		code, payload := call(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %v", args, code, payload)
		}
		return payload
	}
	claim := func(project, task, host, head string) string {
		result := must("merge-turn-request", "--repository", g.dir, "--base-ref", "dev", "--project", project, "--task", task, "--host", host, "--head", head, "--ready")
		turn := result["turnId"].(string)
		grant := result["grant"].(map[string]any)["grantId"].(string)
		must("merge-turn-acknowledge", "--turn", turn, "--actor", task, "--grant", grant, "--evidence", "read the grant")
		return turn
	}
	check := func(turn, task, head, base string) (int, map[string]any) {
		checks := fmt.Sprintf(`[{"runId":"run-%s","name":"required","headSha":%q,"conclusion":"success","attempt":1}]`, task, head)
		return call("merge-turn-check", "--turn", turn, "--actor", task, "--head-sha", head, "--base-sha", base, "--checks", checks, "--review", `{"hasNextPage":false,"pagesRead":1,"totalCount":1,"threadsSeen":["thread-1"],"unresolved":0}`, "--required", "required")
	}
	must("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha", "--host", "host-a")
	must("linkage-bind", "--role", "parent", "--scope", "PRJ-B", "--task", "task-beta", "--host", "host-b")

	root := g.commit("root")
	g.setTip(root)
	first := claim("PRJ-A", "task-alpha", "host-a", "head-a")
	if code, payload := check(first, "task-alpha", "head-a", root); code != 0 {
		t.Fatalf("first check exited %d: %v", code, payload)
	}
	landed := g.merge(root, 1)
	g.setTip(landed)
	must("merge-turn-land", "--turn", first, "--actor", "task-alpha", "--landed-sha", landed, "--evidence", "merged by the forge")

	outside := g.merge(landed, 2)
	g.setTip(outside)
	second := claim("PRJ-B", "task-beta", "host-b", "head-b")
	code, payload := check(second, "task-beta", "head-b", outside)
	if code != 0 {
		t.Fatalf("the next parent's check exited %d: %v", code, payload)
	}
	restated, _ := payload["landingBaseRestated"].(map[string]any)
	if restated == nil || restated["turnId"] != first || restated["from"] != landed || restated["to"] != outside || restated["sequence"] != float64(1) {
		t.Fatalf("the check's answer does not report the restatement: %v", payload["landingBaseRestated"])
	}
	commits, _ := restated["mergeCommits"].([]any)
	if len(commits) != 1 || commits[0].(map[string]any)["sha"] != outside || commits[0].(map[string]any)["subject"] != "Merge pull request #2 from team/topic-2" {
		t.Fatalf("merge commits: %v", restated["mergeCommits"])
	}
	shown := must("merge-turn-show", "--turn", first)
	list, _ := shown["baseRestatements"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["to"] != outside || shown["observedBaseSha"] != outside {
		t.Fatalf("merge-turn-show for the landing: observedBaseSha %v, restatements %v", shown["observedBaseSha"], list)
	}
}
