package mergeturn_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// livePullCLI runs the relay CLI against a temporary store and decodes what it printed.
type livePullCLI struct {
	t     *testing.T
	state string
}

func (c livePullCLI) run(args ...string) (int, map[string]any) {
	c.t.Helper()
	var out, errs bytes.Buffer
	code := cli.Execute(context.Background(), append([]string{"--state", c.state}, args...), &out, &errs)
	var answer map[string]any
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		c.t.Fatalf("%v: exit %d, stdout %q, stderr %q", err, code, out.String(), errs.String())
	}
	return code, answer
}

func (c livePullCLI) request(pr, head string) (int, map[string]any) {
	return c.run("merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev", "--project", "PRJ-A",
		"--task", "task-alpha", "--host", "host-a", "--head", head, "--pr", pr, "--relationship", "rel-"+pr, "--ready")
}

func newLivePullCLI(t *testing.T) livePullCLI {
	t.Helper()
	c := livePullCLI{t: t, state: filepath.Join(t.TempDir(), "state")}
	if code, bound := c.run("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha", "--host", "host-a"); code != 0 {
		t.Fatalf("bind: %d %v", code, bound)
	}
	return c
}

// CRW-538 through the relay CLI: the second request is exit 2 with the reason and a detail that names the live turn,
// and the repeated request for the same pull request is exit 0 with the same turn.
func TestLivePullRequestThroughTheCLI(t *testing.T) {
	c := newLivePullCLI(t)
	code, first := c.request("500", "head-500")
	turn, _ := first["turnId"].(string)
	if code != 0 || turn == "" {
		t.Fatalf("first request: %d %v", code, first)
	}
	code, again := c.request("500", "head-500")
	if code != 0 || again["turnId"] != turn || again["alreadyClaimed"] != true {
		t.Fatalf("the same pull request again: %d %v", code, again)
	}
	code, refused := c.request("501", "head-501")
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["error"] != "refused" || refused["reason"] != "disposition_conflict" || !strings.Contains(detail, turn) || !strings.Contains(detail, "pull request 500") || !strings.Contains(detail, "place 1 of 1") {
		t.Fatalf("another pull request: exit %d %v", code, refused)
	}
}

// The relay CLI reads the pull request head through gh: a fake gh on PATH answers for pull request 500 (its head is
// read from a file the test changes) and for the base branch. This pins the wiring of the readers into the commands.
func TestLivePullReadyAndCheckThroughTheCLIReadThePullRequest(t *testing.T) {
	bin := t.TempDir()
	headFile := filepath.Join(bin, "pr500")
	setHead := func(sha string) {
		t.Helper()
		if err := os.WriteFile(headFile, []byte(sha), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original, refreshed, foreign, tip := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 40)
	setHead(original)
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\ncase \"$last\" in\n" +
		"  repos/owner/repo/pulls/500) printf '{\"number\":500,\"head\":{\"sha\":\"%s\"}}\\n' \"$(cat " + headFile + ")\" ;;\n" +
		"  repos/owner/repo/git/ref/heads/dev) printf '{\"ref\":\"refs/heads/dev\",\"object\":{\"type\":\"commit\",\"sha\":\"" + tip + "\"}}\\n' ;;\n" +
		"  *) echo 'gh: HTTP 404' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := newLivePullCLI(t)
	code, claimed := c.request("500", original)
	turn, _ := claimed["turnId"].(string)
	grant, _ := claimed["grant"].(map[string]any)
	if code != 0 || turn == "" || grant == nil {
		t.Fatalf("claim: %d %v", code, claimed)
	}
	ack := func(grantID string) {
		t.Helper()
		if code, answer := c.run("merge-turn-acknowledge", "--turn", turn, "--actor", "task-alpha", "--grant", grantID, "--evidence", "read the grant"); code != 0 {
			t.Fatalf("acknowledge: %d %v", code, answer)
		}
	}
	ack(grant["grantId"].(string))

	code, refused := c.run("merge-turn-ready", "--turn", turn, "--actor", "task-alpha", "--head", foreign, "--not-ready", "--cause", "refreshed another pull request")
	if code != 2 || refused["reason"] != "merge_candidate_moved" || !strings.Contains(refused["detail"].(string), "pull request 500") {
		t.Fatalf("another pull request's head: exit %d %v", code, refused)
	}

	setHead(refreshed)
	code, restated := c.run("merge-turn-ready", "--turn", turn, "--actor", "task-alpha", "--head", refreshed, "--not-ready", "--cause", "refreshed the base")
	decided, _ := restated["pullRequestHead"].(map[string]any)
	reset, _ := restated["readinessReset"].(map[string]any)
	if code != 0 || decided["decidedBy"] != "forge" || decided["head"] != refreshed || reset == nil {
		t.Fatalf("the pull request's own head: exit %d %v", code, restated)
	}
	ack(reset["grantId"].(string))
	if code, answer := c.run("merge-turn-ready", "--turn", turn, "--actor", "task-alpha", "--head", refreshed, "--ready"); code != 0 {
		t.Fatalf("ready: %d %v", code, answer)
	}

	check := func() (int, map[string]any) {
		return c.run("merge-turn-check", "--turn", turn, "--actor", "task-alpha", "--head-sha", refreshed, "--base-sha", tip,
			"--checks", `[{"runId":"run-1","name":"dev-gate","headSha":"`+refreshed+`","conclusion":"success","attempt":1}]`,
			"--review", `{"hasNextPage":false,"pagesRead":1,"totalCount":1,"threadsSeen":["thread-1"],"unresolved":0}`, "--required", "dev-gate")
	}
	// the pull request moved on after the candidate was declared: the check reads it again and refuses
	setHead(foreign)
	code, stale := check()
	if code != 2 || stale["reason"] != "merge_candidate_moved" || !strings.Contains(stale["detail"].(string), foreign) {
		t.Fatalf("a pull request that moved after the declaration: exit %d %v", code, stale)
	}
	setHead(refreshed)
	code, merging := check()
	decided, _ = merging["pullRequestHead"].(map[string]any)
	if code != 0 || merging["state"] != "merging" || decided["decidedBy"] != "forge" || decided["head"] != refreshed {
		t.Fatalf("the check: exit %d %v", code, merging)
	}
}
