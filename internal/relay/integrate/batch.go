package integrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The integration batch (CRW-965). One command takes the plan's ready accepted candidates, merges them
// one after another onto a local integration branch with merge commits, and runs one verification on the
// merged tree. The branch only ever moves to a tree whose verification record is a PASS the relay may
// reuse: when the merged tree fails, the candidates are split to find the failing one, only that one is
// left out and returned, and the rest are verified again.
//
// The branch update is git's own compare-and-swap (update-ref with the old value), so a head that moved
// under the command is refused instead of overwritten.

// BatchInput is what one integration batch is asked to do.
type BatchInput struct {
	Plan           string
	Actor          string
	Checkout       string   // the local checkout that holds the candidates' commits and the integration branch
	IntegrationRef string   // the local branch the batch merges onto
	BaseRef        string   // the branch the candidates descend from, read in the same checkout
	Nodes          []string // the candidates to consider; empty means the plan's ready accepted candidates
	Verify         string   // the one verification to run on the merged tree
	ExpectEpoch    int64
}

// SplitCandidate is one candidate the batch left out, and why.
type SplitCandidate struct {
	NodeID       string `json:"node_id"`
	AcceptanceID string `json:"acceptance_id"`
	HeadSHA      string `json:"head_sha"`
	Reason       string `json:"reason"`
}

// MergedCandidate is one candidate the batch merged onto the branch.
type MergedCandidate struct {
	NodeID       string `json:"node_id"`
	AcceptanceID string `json:"acceptance_id"`
	HeadSHA      string `json:"head_sha"`
	MergeCommit  string `json:"merge_commit"`
}

// BatchResult is the answer of one batch.
type BatchResult struct {
	Plan, Actor            string
	Checkout, Ref, BaseRef string
	OldHead, NewHead       string
	Merged                 []MergedCandidate
	Split                  []SplitCandidate
	Verification           dagsched.VerificationRecord
	VerificationDigest     string
	MarkedEvents           []string
	Targets                []string
}

// verifier runs the one verification on the merged tree and returns the record it wrote. The production
// verifier runs the --verify command; a test supplies its own.
type verifier func(ctx context.Context, checkout, tree string) ([]byte, error)

// batchRunner carries the seams the batch's git calls go through, so a test can interleave a ref move
// between the read and the update, or supply a scripted verification.
type batchRunner struct {
	verifier verifier
	update   func(ctx context.Context, dir, ref, newCommit, oldCommit string) error
}

func defaultBatchRunner(in BatchInput) batchRunner {
	return batchRunner{verifier: verifyCommandRunner(in.Verify), update: updateRef}
}

// Batch merges the plan's ready accepted candidates onto the local integration branch.
func Batch(ctx context.Context, s *dagsched.Scheduler, in BatchInput) (BatchResult, error) {
	return batch(ctx, s, in, defaultBatchRunner(in))
}

func batch(ctx context.Context, s *dagsched.Scheduler, in BatchInput, runner batchRunner) (BatchResult, error) {
	out := BatchResult{Plan: in.Plan, Actor: in.Actor, Checkout: in.Checkout, Ref: in.IntegrationRef, BaseRef: in.BaseRef}
	if in.Plan == "" || in.Actor == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an integration batch names --plan and --actor")
	}
	if in.Checkout == "" || in.IntegrationRef == "" || in.BaseRef == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an integration batch names --checkout, --integration-ref and --base")
	}
	if in.Verify == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an integration batch names the one verification to run with --verify")
	}
	if !isGitCheckout(ctx, in.Checkout) {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read", in.Checkout)
	}
	candidates, err := readyCandidates(ctx, s, in)
	if err != nil {
		return out, err
	}
	if len(candidates) == 0 {
		return out, refuse(contract.RefusalDispositionConflict, "plan %s has no ready accepted candidate to integrate", in.Plan)
	}
	// the branch the candidates descend from, read now; the integration branch's current head, when it
	// exists, is what the expected-head guard compares against at the update
	baseTip, found, err := refTip(ctx, in.Checkout, in.BaseRef)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", in.BaseRef, in.Checkout, err)
	}
	if !found {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s names no branch in %s", in.BaseRef, in.Checkout)
	}
	out.OldHead = baseTip
	if head, found, err := refTip(ctx, in.Checkout, in.IntegrationRef); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", in.IntegrationRef, in.Checkout, err)
	} else if found {
		out.OldHead = head
	}
	// the batch merges what the checkout holds and nothing else
	for _, c := range candidates {
		if !hasCommit(ctx, in.Checkout, c.HeadSHA) {
			return out, refuse(contract.RefusalMergeTargetUnreadable, "%s (%s) is not in %s: fetch it there first", c.HeadSHA, c.NodeID, in.Checkout)
		}
	}
	st, err := settle(ctx, in, runner, out.OldHead, candidates)
	if err != nil {
		return out, err
	}
	out.Merged, out.Split, out.NewHead = st.merged, st.split, st.head
	out.Verification, out.VerificationDigest = st.record, st.digest
	if len(st.merged) == 0 {
		return out, nil
	}
	digest := st.digest
	merged := st.merged
	// the branch moves only now, and only if it still holds what was read: git's own compare-and-swap
	if err := runner.update(ctx, in.Checkout, in.IntegrationRef, out.NewHead, out.OldHead); err != nil {
		return out, refuse(contract.RefusalStaleMarkContext, "%s moved while the merged tree was being verified (the batch read %s): read it again and run the batch again", in.IntegrationRef, out.OldHead)
	}
	// the merged mark and the batch row: the mark is what makes the landing readable to every existing
	// reader of integratedAt, and it is recorded exactly as the parent records one
	for _, c := range merged {
		event, err := s.MarkMerged(ctx, in.Plan, c.NodeID, in.Actor, digest)
		if err != nil {
			return out, err
		}
		out.MarkedEvents = append(out.MarkedEvents, event)
	}
	if out.Targets, err = mergedTargets(ctx, s, in, merged); err != nil {
		return out, err
	}
	if err := recordBatch(ctx, s, in, out); err != nil {
		return out, err
	}
	return out, nil
}

// readyCandidates is the candidates a batch considers: the named subset, else the plan's ready accepted
// candidates. A named node the plan does not hold as a ready candidate is refused, so a typo cannot
// silently integrate nothing.
func readyCandidates(ctx context.Context, s *dagsched.Scheduler, in BatchInput) ([]dagsched.Candidate, error) {
	candidates, err := s.AcceptedCandidates(ctx, in.Plan)
	if err != nil {
		return nil, err
	}
	if len(in.Nodes) == 0 {
		return candidates, nil
	}
	byNode := map[string]dagsched.Candidate{}
	for _, c := range candidates {
		byNode[c.NodeID] = c
	}
	var kept []dagsched.Candidate
	for _, id := range in.Nodes {
		c, ok := byNode[id]
		if !ok {
			return nil, refuse(contract.RefusalDispositionConflict, "node %s is not a ready accepted candidate of plan %s", id, in.Plan)
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// mergeCandidates merges the candidates one after another onto the branch head, a merge commit each. A
// candidate git cannot merge (a conflict) is left out and returned rather than merged; the rest are
// merged in order. It answers the merged candidates, the ones left out, the new head and its tree.
func mergeCandidates(ctx context.Context, in BatchInput, candidates []dagsched.Candidate, branchHead string) (merged []MergedCandidate, split []SplitCandidate, head, tree string, err error) {
	head = branchHead
	for _, c := range candidates {
		mergedTree, conflicts, err := mergeTrees(ctx, in.Checkout, head, c.HeadSHA)
		if err != nil {
			return nil, nil, "", "", err
		}
		if len(conflicts) > 0 {
			split = append(split, SplitCandidate{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, Reason: "merge_conflict"})
			continue
		}
		commit, err := mergeCommit(ctx, in.Checkout, mergedTree, head, c.HeadSHA, "CRW-965: integrate "+c.NodeID)
		if err != nil {
			return nil, nil, "", "", err
		}
		merged = append(merged, MergedCandidate{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, MergeCommit: commit})
		head, tree = commit, mergedTree
	}
	return merged, split, head, tree, nil
}

// mergedTargets names the targets the merged candidates land on, so the answer tells the parent exactly
// what to observe.
func mergedTargets(ctx context.Context, s *dagsched.Scheduler, in BatchInput, merged []MergedCandidate) ([]string, error) {
	seen := map[string]bool{}
	for _, m := range merged {
		targets, err := s.CandidateTargets(ctx, in.Plan, m.NodeID)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			seen[t.Repository+"@"+t.BaseRef] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// recordBatch appends the batch row in the store.
func recordBatch(ctx context.Context, s *dagsched.Scheduler, in BatchInput, out BatchResult) error {
	mergedJSON, err := json.Marshal(out.Merged)
	if err != nil {
		return err
	}
	splitJSON, err := json.Marshal(out.Split)
	if err != nil {
		return err
	}
	recordJSON, err := json.Marshal(out.Verification)
	if err != nil {
		return err
	}
	row := store.IntegrationBatchRow{BatchID: batchIDOf(in, out), PlanID: in.Plan, Repository: in.Checkout, IntegrationRef: in.IntegrationRef, BaseRef: in.BaseRef,
		OldHead: out.OldHead, NewHead: out.NewHead, MergedJSON: string(mergedJSON), SplitJSON: string(splitJSON), VerificationJSON: string(recordJSON),
		RecordedBy: in.Actor, CoordinatorEpoch: in.ExpectEpoch, RecordedAt: registry.SystemISO()}
	return store.RecordIntegrationBatch(ctx, s.Store, row)
}

// batchIDOf is the identity of one batch: the plan, the checkout, the branch, the heads and what was
// merged, so the same batch recorded twice is the same row.
func batchIDOf(in BatchInput, out BatchResult) string {
	var nodes []string
	for _, m := range out.Merged {
		nodes = append(nodes, m.NodeID+"="+m.HeadSHA)
	}
	return registry.CoordinationID("dib", in.Plan, in.Checkout, in.IntegrationRef, out.OldHead, out.NewHead, strings.Join(nodes, ","))
}

// verifyCommandRunner is the production verifier: it runs the --verify command with the checkout as its
// working directory and CRW_VERIFICATION_RECORD in its environment, then reads the record the command
// wrote there. The command is split into an explicit argument vector and never handed to a shell.
func verifyCommandRunner(command string) verifier {
	return func(ctx context.Context, checkout, tree string) ([]byte, error) {
		argv, err := splitCommand(command)
		if err != nil {
			return nil, err
		}
		recordPath := filepath.Join(os.TempDir(), "crw-965-verification-"+shaOf([]byte(tree))+".json")
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = checkout
		cmd.Env = append(gitEnv(), "CRW_VERIFICATION_RECORD="+recordPath, "CRW_MERGED_TREE="+tree)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		// a command that exits non-zero still decides the tree when it wrote a record: that record is a
		// FAIL the batch can split on. Without a record there is nothing to judge, so the run is an error.
		runErr := cmd.Run()
		raw, err := os.ReadFile(recordPath)
		if err != nil {
			if runErr != nil {
				return nil, fmt.Errorf("the verification command failed: %v: %s", runErr, strings.TrimSpace(stderr.String()))
			}
			return nil, fmt.Errorf("the verification command wrote no record at %s: %v", recordPath, err)
		}
		return raw, nil
	}
}

// splitCommand splits a command line into an explicit argument vector. It understands single and double
// quotes and a backslash escape, and refuses an unterminated quote, so a command the caller typed is
// never handed to a shell.
func splitCommand(command string) ([]string, error) {
	var argv []string
	var current strings.Builder
	started := false
	var quote rune
	escaped := false
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if started {
				argv = append(argv, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, refuse(contract.RefusalMalformedReceipt, "--verify carries an unterminated quote or escape")
	}
	if started {
		argv = append(argv, current.String())
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, refuse(contract.RefusalMalformedReceipt, "--verify names no command")
	}
	return argv, nil
}

// shaOf is the sha256 of a byte slice in lowercase hex.
func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// refuse is a refusal under an existing reason, with the relay's exit code 2.
func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}
