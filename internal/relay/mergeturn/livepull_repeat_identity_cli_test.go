package mergeturn_test

import (
	"strings"
	"testing"
)

// repeatIdentityCLIRequest is merge-turn-request for the parent of PRJ-A at the one commit the pull requests point at,
// with the identity arguments a test gives it.
func repeatIdentityCLIRequest(c livePullCLI, identity ...string) (int, map[string]any) {
	args := []string{"merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev", "--project", "PRJ-A",
		"--task", "task-alpha", "--host", "host-a", "--head", "commit-h", "--ready"}
	return c.run(append(args, identity...)...)
}

// CRW-587 through the relay CLI: pull requests 500 and 501 point at one commit, the parent claims with the relationship
// of 500 and no --pr, and the request for 501 at that commit is exit 2 naming the live turn, where it used to be exit 0
// with the turn of 500. A repeat of the claim's own arguments is still exit 0 and the same turn.
func TestRepeatIdentityThroughTheCLI(t *testing.T) {
	c := newLivePullCLI(t)
	code, first := repeatIdentityCLIRequest(c, "--relationship", "rel-500")
	turn, _ := first["turnId"].(string)
	if code != 0 || turn == "" {
		t.Fatalf("first request: %d %v", code, first)
	}
	code, refused := repeatIdentityCLIRequest(c, "--pr", "501")
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["error"] != "refused" || refused["reason"] != "disposition_conflict" || !strings.Contains(detail, turn) ||
		!strings.Contains(detail, "repeat the request with the arguments the claim was made with (its identity arguments: --relationship 'rel-500' and no --pr)") {
		t.Fatalf("pull request 501 at the commit of the claim made for the relationship of 500: exit %d %v", code, refused)
	}
	code, again := repeatIdentityCLIRequest(c, "--relationship", "rel-500")
	if code != 0 || again["turnId"] != turn || again["alreadyClaimed"] != true {
		t.Fatalf("the claim's own arguments again: %d %v", code, again)
	}
}

// A claim made with no identity is repeated without one, and refused with one.
func TestRepeatIdentityBareClaimThroughTheCLI(t *testing.T) {
	c := newLivePullCLI(t)
	code, first := repeatIdentityCLIRequest(c)
	turn, _ := first["turnId"].(string)
	if code != 0 || turn == "" {
		t.Fatalf("bare claim: %d %v", code, first)
	}
	code, refused := repeatIdentityCLIRequest(c, "--pr", "500")
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["reason"] != "disposition_conflict" || !strings.Contains(detail, turn) || !strings.Contains(detail, "neither --pr nor --relationship") {
		t.Fatalf("an identity attached to a bare claim: exit %d %v", code, refused)
	}
	code, again := repeatIdentityCLIRequest(c)
	if code != 0 || again["turnId"] != turn || again["alreadyClaimed"] != true {
		t.Fatalf("the bare claim again: %d %v", code, again)
	}
}
