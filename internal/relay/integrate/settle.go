package integrate

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// verifyMergedTree runs the one verification on the merged tree every time. It never reuses an earlier
// record: a record keyed by tree alone would not notice a criteria change, so each batch verifies again.
func verifyMergedTree(ctx context.Context, in BatchInput, runner batchRunner, tree string) (dagsched.VerificationRecord, string, error) {
	raw, err := runner.verifier(ctx, in.Checkout, tree)
	if err != nil {
		return dagsched.VerificationRecord{}, "", err
	}
	record, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil {
		return dagsched.VerificationRecord{}, "", err
	}
	if !strings.EqualFold(record.Tree, tree) {
		return dagsched.VerificationRecord{}, "", refuse(contract.RefusalRevisionMismatch, "the verification record names tree %s and the merged tree is %s", record.Tree, tree)
	}
	return record, shaOf(raw), nil
}

// settled is the answer of settle: the candidates that verify together on the branch, the ones left out,
// the head and tree they produce, and the record of that tree. An empty merged list means nothing
// verified and the branch must not move.
type settled struct {
	merged []MergedCandidate
	split  []SplitCandidate
	head   string
	tree   string
	record dagsched.VerificationRecord
	digest string
}

// settle merges the candidates onto oldHead and verifies the merged tree once. When that verification
// does not pass, it takes the candidates one after another and keeps each one that still verifies with
// the ones kept before it; a candidate that breaks the verification (or does not merge) is left out and
// returned to its parent. The branch only ever moves to a tree whose record is a PASS.
func settle(ctx context.Context, in BatchInput, runner batchRunner, oldHead string, candidates []dagsched.Candidate) (settled, error) {
	merged, split, head, tree, err := mergeCandidates(ctx, in, candidates, oldHead)
	if err != nil {
		return settled{}, err
	}
	if len(merged) == 0 {
		return settled{split: split}, nil
	}
	record, digest, err := verifyMergedTree(ctx, in, runner, tree)
	if err != nil {
		return settled{}, err
	}
	if strings.EqualFold(record.Result, "PASS") {
		return settled{merged: merged, split: split, head: head, tree: tree, record: record, digest: digest}, nil
	}
	// the merged tree failed: take the candidates in order and keep the ones that still verify together
	var kept []dagsched.Candidate
	out := settled{split: split}
	for _, c := range candidates {
		if containsSplit(out.split, c.NodeID) {
			continue
		}
		try := append(append([]dagsched.Candidate(nil), kept...), c)
		m2, s2, h2, t2, err := mergeCandidates(ctx, in, try, oldHead)
		if err != nil {
			return settled{}, err
		}
		if len(s2) > 0 || len(m2) != len(try) {
			out.split = append(out.split, SplitCandidate{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, Reason: "merge_conflict"})
			continue
		}
		rec, dig, err := verifyMergedTree(ctx, in, runner, t2)
		if err != nil {
			return settled{}, err
		}
		if strings.EqualFold(rec.Result, "PASS") {
			kept = try
			out.merged, out.head, out.tree, out.record, out.digest = m2, h2, t2, rec, dig
			continue
		}
		out.split = append(out.split, SplitCandidate{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, Reason: "verification_failed"})
	}
	return out, nil
}

// containsSplit reports whether a node is already in the split list.
func containsSplit(split []SplitCandidate, node string) bool {
	for _, s := range split {
		if s.NodeID == node {
			return true
		}
	}
	return false
}
