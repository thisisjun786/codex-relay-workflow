package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// VerificationRecord is the part of verification-record/1 this build reads. The writer of the full
// record belongs to the local-verification tooling issue; the field names below are the ones that issue
// fixes, and DecodeVerificationRecord is the single place they are pinned. The integration command
// reads the same document through this decoder, so a record means one thing in the relay.
type VerificationRecord struct {
	Schema   string `json:"schema"`
	Head     string `json:"head_commit"`
	Base     string `json:"base_commit"`
	Tree     string `json:"tree"`
	Result   string `json:"result"`
	Reusable *bool  `json:"reusable"`
	// PinMismatch is the tooling issue's mark that the tools did not match the pinned versions: a
	// record that carries it is not reusable.
	PinMismatch bool `json:"pin_mismatch"`
	// The reuse keys are read so a record that names none is still readable; the record body itself is
	// stored unchanged, so a later reader recomputes the digest from what the parent supplied.
	Repository string `json:"repository"`
	CiDigest   string `json:"ci_digest"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

// DecodeVerificationRecord reads a verification-record/1 and refuses a document that is not one or that
// lacks a field the decision needs.
func DecodeVerificationRecord(raw []byte) (VerificationRecord, error) {
	var record VerificationRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&record); err != nil {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record is not a JSON document: %v", err)
	}
	if decoder.More() {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record is one JSON object")
	}
	if record.Schema != VerificationRecordSchema {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record names schema %q; this path reads %s", record.Schema, VerificationRecordSchema)
	}
	if record.Head == "" || record.Tree == "" || record.Result == "" {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record names no head commit, tree or result")
	}
	return record, nil
}

// readVerificationRecord reads the record a --verification option names: the document itself, or @file.
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
// the base-refresh proof uses.
func gitTreeOf(ctx context.Context, checkout, commit string) (string, error) {
	out, err := runGit(ctx, checkout, nil, "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

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

// acceptCommitInput is the read side of the commit path: everything the checks produce, read before the
// write transaction opens, exactly as the pull-request path reads its forge before it.
type acceptCommitInput struct {
	ref       CommitRef
	recordRaw []byte
	record    VerificationRecord
	tree      string
}

// prepareCommitAcceptance runs every check the commit path makes outside the write transaction: the
// record is readable, the commit is the ruled head, it descends from the base, and the record names this
// commit's tree with a PASS that may be reused. A failure here writes nothing.
func (s *Scheduler) prepareCommitAcceptance(ctx context.Context, in CommitRef, headEvent string) (acceptCommitInput, error) {
	out := acceptCommitInput{ref: in}
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
	record, err := DecodeVerificationRecord(raw)
	if err != nil {
		return out, err
	}
	out.recordRaw, out.record = raw, record

	// the commit has to be the head the generation's ruling fixed. The event's own revision hash is the
	// artifact manifest digest, not a commit, so the commit comes from dag_verified_heads.
	q := s.Store.Q(ctx)
	ruled, source, err := acceptance.RulingHead(ctx, q, headEvent, "")
	if err != nil {
		return out, err
	}
	if source != "verified_heads" || ruled == "" {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the ruling on event %s fixed no head commit, so no commit can be the head of that generation", headEvent)
	}
	if !strings.EqualFold(ruled, in.Head) {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the ruling on event %s fixed head %s and the call names %s", headEvent, ruled, in.Head)
	}
	if !strings.EqualFold(record.Head, in.Head) {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the verification record names head %s and the call names %s", record.Head, in.Head)
	}
	// the commit descends from the base
	if ancestor, err := commitIsAncestor(ctx, in.Checkout, in.Base, in.Head); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s: %v", in.Checkout, err)
	} else if !ancestor {
		return out, refuse(contract.RefusalMergeBaseMismatch, "%s is not an ancestor of %s in %s: the commit does not descend from the base", in.Base, in.Head, in.Checkout)
	}
	// the record names this commit's tree
	tree, err := gitTreeOf(ctx, in.Checkout, in.Head)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read the tree of %s in %s: %v", in.Head, in.Checkout, err)
	}
	out.tree = tree
	if !strings.EqualFold(record.Tree, tree) {
		return out, refuse(contract.RefusalRevisionMismatch, "the verification record names tree %s and the tree of %s is %s", record.Tree, in.Head, tree)
	}
	// a PASS the relay may reuse
	if !strings.EqualFold(record.Result, "PASS") {
		return out, refuse(contract.RefusalDispositionConflict, "the verification record of %s is %s: only a PASS is accepted", in.Head, record.Result)
	}
	if record.Reusable != nil && !*record.Reusable {
		return out, refuse(contract.RefusalDispositionConflict, "the verification record of %s is not reusable", in.Head)
	}
	if record.PinMismatch {
		return out, refuse(contract.RefusalDispositionConflict, "the verification record of %s records a tool pin mismatch, so it cannot be reused", in.Head)
	}
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
