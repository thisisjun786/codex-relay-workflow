package evidence

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// Flow assignments (CRW-1482, 1209 field trial defect C9). A flow run's executor is spawned by the owner thread of a session run
// and works in the call worktree the engine made for it, while the stop hook judged its receipt under the owner's native cwd: no
// assignment was ever recorded for it (the owner's spawn is flow-bound and gets no evidence block), so the hook looked where the
// child never writes and spent the budget. The engine is now the trusted writer of that contract, as the spawn hook is in CRW-1115:
// at the step's publication it records a tree assignment of origin flow under the owner's state directory, keyed by the owner
// session, with the call worktree as its root and the step's op, and appends the [CRW-EVIDENCE-ASSIGNMENT:<id>] block to the spawn
// message. A child never declares a root here either.
//
// A flow contract differs from a spawn-hook contract in three ways. Its receipt is one fixed file, FlowReceiptPath
// (<root>/.crw/evidence/<id>.md), so the child's final message stays the one RECEIPT JSON value and needs no EVIDENCE_RECORDED line
// after it (an explicit one must name exactly that file). It holds only for a child whose own packet starts with the engine's
// `crw-flow-op: <op>` line of that very op, so the block copied into another step's message is refused, and a child whose transcript
// shows no packet cannot claim it by citing the id. Its claim records the turn and the digest of the receipt bytes, so the
// acceptance path reads back exactly what the hook accepted (AcceptedReceipt), and a repair turn of the same child claims again.
//
// A record is checked before it is reused (CRW-1496). The engine writes the record of a dispatch again whenever it republishes or
// recovers the step, and the id is the same each time. An open, unclaimed record is kept only while it is still the contract a claim
// would accept for the tree now (treeMatch, the tree half of the claim's own check): a call worktree that was removed and made
// again at the same path has a new inode, and one reset to a commit that does not descend from the recorded HEAD has an unrelated
// history, so the old record would refuse every receipt of the child. Such a record is replaced in place under the record's lock. A
// claimed record is the claiming child's contract and is kept as it is. A git that gives no answer is an error, never a mismatch or
// an empty Head, so a failing git holds the call and never drops a record's ancestry check; and the claim judges the receipt again
// under the record's lock against the record it holds, so a record replaced between the judgement and the claim never takes a
// receipt older than itself.

// AssignmentOriginFlow is the Origin of an assignment the flow engine recorded.
const AssignmentOriginFlow = "flow"

// flowOpPrefix opens the first line of every message the flow engine dispatches a child with.
const flowOpPrefix = "crw-flow-op: "

// flowReceiptLimit bounds the receipt bytes the claim and AcceptedReceipt read.
const flowReceiptLimit = 16 << 20

// FlowAssignmentID is the id of the flow assignment of one dispatch: derived from the owner session, the run, the call and the op
// (crw-flow-op marker), so a republished or recovered step names the record it already has. It has the shape of a minted id
// (base32 upper case, 26 characters).
func FlowAssignmentID(owner, run, call, op string) string {
	h := sha256.New()
	for _, part := range []string{"crw-flow-evidence-assignment", owner, run, call, op} {
		fmt.Fprintf(h, "%d:%s\x00", len(part), part)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil)[:16])
}

// NewFlowAssignment builds the flow record of one dispatch for the owner session: worktree is the call worktree (an absolute path
// to an existing directory, stored by its real path with its device, inode and git HEAD now), op the step's crw-flow-op marker and
// id its FlowAssignmentID. A git that gives no answer for HEAD is an error, never an empty Head (gitHeadOf). The record is not yet
// written (PersistNew or EnsureFlowAssignment does that).
func NewFlowAssignment(sessionID, id, worktree, op string, now time.Time) (Assignment, error) {
	switch {
	case sessionID == "":
		return Assignment{}, errors.New("a flow evidence assignment needs the owner's session id")
	case !validAssignmentID(id):
		return Assignment{}, fmt.Errorf("invalid flow evidence assignment id %q", id)
	case op == "" || strings.ContainsFunc(op, unicode.IsSpace):
		return Assignment{}, fmt.Errorf("invalid flow op %q", op)
	case !filepath.IsAbs(worktree):
		return Assignment{}, fmt.Errorf("the flow worktree must be an absolute path: %s", worktree)
	}
	real, dev, ino, err := treeIdentity(worktree)
	if err != nil {
		return Assignment{}, fmt.Errorf("flow worktree %s: %w", worktree, err)
	}
	head, err := gitHeadOf(real)
	if err != nil {
		return Assignment{}, fmt.Errorf("flow worktree %s: %w", worktree, err)
	}
	return Assignment{Version: flowAssignmentVersion, ID: id, SessionID: sessionID, Mode: AssignTree, Root: real, RootDev: dev,
		RootIno: ino, Head: head, CreatedAt: now.UTC().Format(time.RFC3339Nano), Status: AssignmentOpen,
		Origin: AssignmentOriginFlow, Op: op}, nil
}

// EnsureFlowAssignment writes the flow assignment a under cwd, or keeps the record its id already has when that record is still the
// contract: a recovered, republished or adopted step derives the same id. A claimed record is the contract of the child that claimed
// it (it may have stopped before the restart) and is returned as it is, never rewritten. An open, unclaimed record is reused only
// while its tree still matches (treeMatch: the same real path, device and inode as the tree now, and a recorded Head that is an
// ancestor of HEAD now); one that does not is replaced by a under the record's lock (replaceOpen). The returned value is always the
// record on disk. A record of that id for another session, root or op, one that cannot be read or is neither open nor claimed, one
// whose tree git gives no answer for, and a replacement that would drop its recorded HEAD are errors: the caller holds the call.
func EnsureFlowAssignment(cwd string, a Assignment) (Assignment, error) {
	if a.Origin != AssignmentOriginFlow || !recordShapeValid(a) {
		return Assignment{}, errors.New("not a flow evidence assignment")
	}
	err := a.PersistNew(cwd)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, ErrAssignmentExists) {
		return Assignment{}, err
	}
	rec, ok := RecordedAssignment(cwd, a.SessionID, a.ID)
	switch {
	case !ok:
		return Assignment{}, fmt.Errorf("the flow evidence assignment %s is recorded but cannot be read", a.ID)
	case rec.Origin != AssignmentOriginFlow || rec.Mode != AssignTree || rec.Root != a.Root || rec.Op != a.Op:
		return Assignment{}, fmt.Errorf("the flow evidence assignment %s is recorded for root %s op %s, not root %s op %s", a.ID, rec.Root, rec.Op, a.Root, a.Op)
	case rec.Status != AssignmentOpen && rec.Status != AssignmentClaimed:
		return Assignment{}, fmt.Errorf("the flow evidence assignment %s is %s", a.ID, rec.Status)
	case rec.Status == AssignmentOpen && rec.AgentID == "":
		ok, err := treeMatch(rec)
		if err != nil {
			return Assignment{}, fmt.Errorf("the tree of the flow evidence assignment %s cannot be checked: %w", a.ID, err)
		}
		if !ok {
			return a.replaceOpen(cwd)
		}
	}
	return rec, nil
}

// FlowReceiptPath is the one receipt file of a flow assignment: <root>/.crw/evidence/<id>.md.
func FlowReceiptPath(a Assignment) string {
	return filepath.Join(a.Root, crwdir.DirName, Subdir, a.ID+".md")
}

// flowOpBound reports whether the child's own packet (dispatchPacket) is the engine's dispatch of a's op: its first line is
// `crw-flow-op: <op>`, trailing white space aside, as sessionhost.ParseMarker reads it. A transcript that shows no packet is not.
func flowOpBound(a Assignment, transcriptPath string) bool {
	packet, ok := dispatchPacket(transcriptPath)
	if !ok {
		return false
	}
	line, _, _ := strings.Cut(packet, "\n")
	return strings.TrimRight(line, " \t\r") == flowOpPrefix+a.Op
}

// FlowContractOf is the flow contract the stopping child was dispatched with, and whether it has one: a readable flow record its
// own packet names, for that packet's op. The hook uses it for the block reason (VerifierDirectiveFor).
func FlowContractOf(cwd, sessionID, agentID, transcriptPath, message string) (Assignment, bool) {
	if sessionID == "" {
		return Assignment{}, false
	}
	a, found := childAssignment(cwd, sessionID, agentID, transcriptPath, message)
	if found != AssignedAccepted || a.Origin != AssignmentOriginFlow || !flowOpBound(a, transcriptPath) {
		return Assignment{}, false
	}
	return a, true
}

// flowReceiptClaim is the receipt a flow contract judges, and whether the child may claim it at all: the child's packet must be the
// dispatch of the contract's op, and receipt must be "" (the fixed file) or resolve, relative to the root, to exactly that file.
func flowReceiptClaim(a Assignment, transcriptPath, receipt string) (string, bool) {
	if !flowOpBound(a, transcriptPath) {
		return "", false
	}
	fixed := FlowReceiptPath(a)
	if receipt != "" && resolve(a.Root, receipt) != fixed {
		return "", false
	}
	return fixed, true
}

// flowClaimUpdate is the claim's change of a flow record: under the record's lock it checks that the record is still the flow
// contract that was judged (origin, root and op) and judges the receipt again against the record it holds there, with the very
// check the judgement made (assignedReceiptValid: the tree's real path, device and inode, the recorded HEAD's ancestry, and a
// receipt no older than the record's CreatedAt). The record may have been replaced since the judgement (replaceOpen, CRW-1496): a
// receipt is claimed only under the contract it satisfies. It then reads the receipt bytes again and records their digest with
// turnID. A receipt that fails there or cannot be read refuses the claim.
func flowClaimUpdate(judged Assignment, turnID string) func(*Assignment) bool {
	return func(next *Assignment) bool {
		if next.Origin != AssignmentOriginFlow || next.Root != judged.Root || next.Op != judged.Op {
			return false
		}
		if next.Mode != AssignTree || !assignedReceiptValid(*next, FlowReceiptPath(*next)) {
			return false
		}
		raw, ok := readFlowReceipt(*next)
		if !ok {
			return false
		}
		sum := sha256.Sum256(raw)
		next.TurnID, next.ReceiptDigest = turnID, hex.EncodeToString(sum[:])
		return true
	}
}

// readFlowReceipt reads the fixed receipt of a: the tree must still be the one recorded, the file must lie inside its real
// .crw/evidence, be a non-empty regular file that is not a link, no older than a's CreatedAt (a receipt older than the record it is
// read for is another contract's), and fit in flowReceiptLimit. It is opened without following a link and without blocking.
func readFlowReceipt(a Assignment) ([]byte, bool) {
	real, dev, ino, err := treeIdentity(a.Root)
	if err != nil || real != a.Root || dev != a.RootDev || ino != a.RootIno {
		return nil, false
	}
	path := FlowReceiptPath(a)
	if crwdir.RequireDir(filepath.Join(a.Root, crwdir.DirName)) != nil || !receiptInside(filepath.Join(a.Root, crwdir.DirName, Subdir), path) {
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
	if err != nil {
		return nil, false
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.ModTime().Before(created) {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, flowReceiptLimit+1))
	if err != nil || len(raw) == 0 || len(raw) > flowReceiptLimit {
		return nil, false
	}
	return raw, true
}

// FlowReceipt is a receipt the hook accepted for a flow assignment: the file, the digest the claim recorded, the child and turn
// that claimed it, and the bytes read now (which hash to Digest).
type FlowReceipt struct {
	Path, Digest, AgentID, TurnID string
	Bytes                         []byte
}

// AcceptedReceipt is the accepted receipt of the session's flow assignment id under cwd, and whether there is one: the record must
// be a claimed flow record with a digest, and the file's current bytes must still hash to it. A record never claimed, claimed
// without a receipt the hook read, or whose receipt changed, moved or was replaced since is no accepted receipt; the caller tells
// these apart with RecordedAssignment.
func AcceptedReceipt(cwd, sessionID, id string) (FlowReceipt, bool) {
	a, ok := RecordedAssignment(cwd, sessionID, id)
	if !ok || a.Origin != AssignmentOriginFlow || a.Status != AssignmentClaimed || a.AgentID == "" || a.ReceiptDigest == "" {
		return FlowReceipt{}, false
	}
	raw, ok := readFlowReceipt(a)
	if !ok {
		return FlowReceipt{}, false
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != a.ReceiptDigest {
		return FlowReceipt{}, false
	}
	return FlowReceipt{Path: FlowReceiptPath(a), Digest: a.ReceiptDigest, AgentID: a.AgentID, TurnID: a.TurnID, Bytes: raw}, true
}

// ReceiptIgnored reports whether git ignores the receipt of a, its own file FlowReceiptPath (git check-ignore through the bounded
// probe, in a's root): true when it does, false when the root is a checkout that does not, and an error when git could not answer
// (the root in no repository, or git failed). The receipt's own name is asked, so a rule that matches only another name in
// .crw/evidence decides nothing for it.
func ReceiptIgnored(a Assignment) (bool, error) {
	_, err := source.Probe(a.Root, source.ProbeOptions{Limit: 1 << 16, Timeout: gitprobe.Timeout, Env: []string{"LC_ALL=C"}},
		"check-ignore", "-q", "--", filepath.ToSlash(filepath.Join(crwdir.DirName, Subdir, a.ID+".md")))
	if err == nil {
		return true, nil
	}
	var exit *source.ExitError
	if errors.As(err, &exit) && exit.Code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore in %s: %w", a.Root, err)
}
