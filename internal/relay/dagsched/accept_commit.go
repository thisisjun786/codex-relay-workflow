package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The pull-request-less acceptance path (CRW-965). An implementation node whose result is a commit is
// accepted with --commit <head> --base <base> --verification <record> --checkout <path>: the relay
// proves in the local checkout that the commit is the head the generation's ruling fixed
// (dag_verified_heads.head_sha, read through acceptance.RulingHead), that it descends from the base,
// and that the verification record names this commit's tree with a PASS it may reuse. Every failure is
// refused with its own reason. The pull-request path is untouched: this path never reads a forge, never
// calls acceptTarget and writes no dag_acceptance_forge row, and that absence is what tells the two
// paths apart.

// CommitRef is the pull-request-less half of an acceptance input: the commit to accept, the base it
// must descend from, the local checkout that holds both, and the verification-record/1 the parent
// supplied.
type CommitRef struct {
	Head     string
	Base     string
	Checkout string
	Record   string
}

// VerificationRecordSchema is the schema tag a verification record must carry.
const VerificationRecordSchema = "verification-record/1"

func readVerificationRecord(input string) ([]byte, error) {
	if !strings.HasPrefix(input, "@") {
		if err := store.EncodeUTF8(input); err != nil {
			return nil, usage("--verification is not valid UTF-8: " + err.Error())
		}
		return []byte(input), nil
	}
	raw, err := os.ReadFile(input[1:])
	if err != nil {
		return nil, usage("--verification names a file that cannot be read: " + err.Error())
	}
	return raw, nil
}

// gitTreeOf reads the tree object of a commit in a checkout, through the clean-environment git helper

// commitIsAncestor answers whether ancestor is an ancestor of descendant in the checkout.
func commitIsAncestor(ctx context.Context, checkout, ancestor, descendant string) (bool, error) {
	// merge-base --is-ancestor answers with its exit status: 0 contained, 1 not contained, anything
	// else a read failure that must not become an answer.
	code, _, err := runGitExit(ctx, checkout, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	switch {
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, err
}

// acceptCommitInput is the read side of the commit path: everything the checks produce, read before the write
// transaction opens. The event, revision and generation are what the proof stood on; the transaction compares them
// with the current head again (finding d9), and the head it stores is the one proved here.
type acceptCommitInput struct {
	ref        CommitRef
	recordRaw  []byte
	record     VerificationRecord
	tree       string
	event      string
	revision   string
	generation int64
}

// prepareCommitAcceptance runs every check the commit path makes outside the write transaction: the record is
// readable and judged reusable for this commit's tree, base, ci.yml, dependencies and host (JudgeVerificationRecord),
// the commit is the head the generation's ruling fixed, and it descends from the base. A failure here writes nothing.
func (s *Scheduler) prepareCommitAcceptance(ctx context.Context, in CommitRef, head verifiedHead, rel relRow) (acceptCommitInput, error) {
	out := acceptCommitInput{ref: in, event: head.EventID, revision: head.RevisionHash, generation: rel.Generation}
	if in.Head == "" || in.Base == "" || in.Checkout == "" || in.Record == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "a pull-request-less acceptance names --commit, --base, --verification and --checkout")
	}
	if !refreshCommitPattern.MatchString(in.Head) || !refreshCommitPattern.MatchString(in.Base) {
		return out, refuse(contract.RefusalMalformedReceipt, "--commit and --base are full commit ids, not %q and %q", in.Head, in.Base)
	}
	if !filepath.IsAbs(in.Checkout) {
		return out, refuse(contract.RefusalMalformedReceipt, "--checkout is an absolute path to a local checkout, not %q", in.Checkout)
	}
	raw, err := readVerificationRecord(in.Record)
	if err != nil {
		return out, err
	}
	out.recordRaw = raw

	// the commit has to be the head the generation's ruling fixed. The event's own revision hash is the artifact
	// manifest digest, not a commit, so the commit comes from dag_verified_heads.
	q := s.Store.Q(ctx)
	ruled, source, err := acceptance.RulingHead(ctx, q, head.EventID, "")
	if err != nil {
		return out, err
	}
	if source != "verified_heads" || ruled == "" {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the ruling on event %s fixed no head commit, so no commit can be the head of that generation", head.EventID)
	}
	if !strings.EqualFold(ruled, in.Head) {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the ruling on event %s fixed head %s and the call names %s", head.EventID, ruled, in.Head)
	}
	// the commit descends from the base
	if ancestor, err := commitIsAncestor(ctx, in.Checkout, in.Base, in.Head); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s: %v", in.Checkout, err)
	} else if !ancestor {
		return out, refuse(contract.RefusalMergeBaseMismatch, "%s is not an ancestor of %s in %s: the commit does not descend from the base", in.Base, in.Head, in.Checkout)
	}
	base, err := runGit(ctx, in.Checkout, nil, "rev-parse", "--verify", in.Base+"^{commit}")
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not resolve the base %s in %s: %v", in.Base, in.Checkout, err)
	}
	keys, err := CommitVerificationKeys(ctx, in.Checkout, in.Head)
	if err != nil {
		return out, err
	}
	keys.Base = strings.TrimSpace(base)
	keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
	record, err := JudgeVerificationRecord(raw, keys)
	if err != nil {
		return out, err
	}
	out.tree, out.record = keys.Tree, record
	return out, nil
}

// storeCommitVerification appends the verification record with the acceptance, inside the acceptance's
// own transaction (txCtx is the composing transaction's context), so the record and the acceptance it
// justifies are one durable effect.
func (s *Scheduler) storeCommitVerification(txCtx context.Context, acceptanceID, actor string, prepared acceptCommitInput, epoch int64) error {
	return store.RecordAcceptanceVerification(txCtx, s.Store, store.AcceptanceVerificationRow{
		AcceptanceID: acceptanceID, RecordDigest: shaOf(prepared.recordRaw), RecordJSON: string(prepared.recordRaw),
		HeadCommit: prepared.ref.Head, TreeSHA: prepared.tree, BaseCommit: prepared.ref.Base,
		RecordedBy: actor, CoordinatorEpoch: epoch, RecordedAt: s.now(),
	})
}

// storedVerificationDigest reads the verification record digest recorded with an acceptance. found is
// false when the acceptance has none, which is what tells a pull-request acceptance from a commit one.
func storedVerificationDigest(ctx context.Context, q store.Querier, acceptanceID string) (string, bool, error) {
	var digest string
	found, err := queryOne(ctx, q, "SELECT record_digest FROM dag_acceptance_verifications WHERE acceptance_id = ?", []any{acceptanceID}, &digest)
	return digest, found, err
}

// acceptCommitTarget is the repository a pull-request-less acceptance is judged against: the local
// checkout the caller named. The node's outgoing integrated and code-pinned edges have to name that
// same checkout, compared as written; a node with no such edge takes the checkout. This is the one
// deliberate departure from "an implementation node is accepted on its pull request's forge
// repository" (CRW-965): without a pull request there is no forge repository to name, and the commit
// itself is only readable in the checkout.
func (s *Scheduler) acceptCommitTarget(ctx context.Context, q store.Querier, snap dag.Snapshot, node, checkout string) (string, error) {
	targets := map[string]bool{}
	for _, e := range snap.Edges {
		if e.FromNodeID == node && e.TargetRepository != "" && (e.Kind == dag.EdgeIntegrated || (e.Kind == dag.EdgeArtifactVerified && e.PinsCodeHead)) {
			targets[e.TargetRepository] = true
		}
	}
	switch len(targets) {
	case 0:
		return checkout, nil
	case 1:
		for t := range targets {
			if t != checkout {
				return "", refuse(contract.RefusalDispositionConflict, "the outgoing edges of %s name %s as their target and the commit is accepted on the checkout %s", node, t, checkout)
			}
			return t, nil
		}
	}
	return "", refuse(contract.RefusalMalformedReceipt, "the outgoing edges of %s name more than one repository; one acceptance is judged against one", node)
}
