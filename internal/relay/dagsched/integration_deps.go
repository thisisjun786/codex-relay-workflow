package dagsched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/execgroup"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The wiring `crw relay dag-integrate` gives an integration batch, shared with the run process of the flow engine (CRW-1277): the
// verification command as an argument vector and git's compare-and-swap of the integration branch. A run integrates its nodes with
// the same two seams the relay command uses, so there is one way an integration branch moves.

// CommandVerifier runs the one verification command, given as an argument vector (never a shell), in the directory the batch names,
// with the batch's CRW_VERIFY_* variables added to the inherited environment. A non-zero exit is a failing run; a command that cannot
// start is an IntegrationVerifierHostError.
func CommandVerifier(argv []string) IntegrationBatchVerifier {
	return CommandVerifierEnv(argv, os.Environ)
}

// CommandVerifierEnv is CommandVerifier over the environment base gives instead of the inherited one (CRW-1463): the command runs with
// base's variables minus GIT_*, then the batch's. A caller that must not pass its own environment to the command builds base itself.
func CommandVerifierEnv(argv []string, base func() []string) IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		if len(argv) == 0 || argv[0] == "" {
			return &IntegrationVerifierHostError{Detail: "the verification command names no program"}
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = dir
		// inherited GIT_* variables would redirect the command's git reads to another repository, so they are dropped
		cmd.Env = []string{}
		for _, kv := range base() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		// both streams go to one bounded tail (os/exec serializes the shared writer), so a failure's error carries the
		// tail of stdout and stderr (CRW-1595)
		tail := &integrationOutputTail{}
		cmd.Stdout, cmd.Stderr = tail, tail
		// the command leads its own group, so a cancel ends everything it started (CRW-1573), not only its first process
		execgroup.Bind(cmd, execgroup.KillGrace)
		if err := cmd.Run(); err != nil {
			if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
				// the command passed and a child it left holds stderr past the grace: the exit is the result
				return nil
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				// the command never ran: a host failure, not a verification result
				return &IntegrationVerifierHostError{Detail: fmt.Sprintf("the verification command could not start: %v", err)}
			}
			return fmt.Errorf("the verification command failed: %v: %s", err, strings.TrimSpace(tail.String()))
		}
		return nil
	}
}

// UpdateIntegrationRef moves a local branch to newCommit only when it still holds oldCommit: git's own compare-and-swap. The all-zero
// oldCommit creates the branch and refuses when it already exists, so a ref that moved under the batch fails the update instead of
// being overwritten.
func UpdateIntegrationRef(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
	// a branch a worktree has checked out never moves (CRW-1595): git's own update-ref would move it and leave the
	// worktree's files and index on the old head
	if err := checkIntegrationCheckout(ctx, checkout, ref); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", checkout, "update-ref", "refs/heads/"+ref, newCommit, oldCommit)
	// inherited GIT_* variables would redirect the write to another repository (finding d4)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git update-ref refs/heads/%s: %v: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// IntegrationDeps are the seams of a batch: the verifier given, and git's compare-and-swap of the branch.
func IntegrationDeps(verify IntegrationBatchVerifier) IntegrationBatchDeps {
	return IntegrationBatchDeps{Verify: verify, Update: UpdateIntegrationRef}
}

// OfferingOrder is the order the scheduler offers a plan's nodes in when they are ready together (Ready): the longest chain that starts
// at a node first, then the most nodes below it, then the node id. Ready ranks nodes that became ready at different times by that time
// too; nodes of one reading ready since the same revision fall back to this order.
func OfferingOrder(snap dag.Snapshot) []string {
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	outgoing := map[string][]string{}
	for _, e := range snap.Edges {
		outgoing[e.FromNodeID] = append(outgoing[e.FromNodeID], e.ToNodeID)
	}
	critical, descendants := graphMetrics(nodes, outgoing)
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].NodeID, nodes[j].NodeID
		switch {
		case critical[a] != critical[b]:
			return critical[a] > critical[b]
		case descendants[a] != descendants[b]:
			return descendants[a] > descendants[b]
		}
		return a < b
	})
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.NodeID
	}
	return out
}
