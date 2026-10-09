package evidence

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Evidence assignments (CRW-1115, port: fixed). The oracle accepts a receipt only under the payload cwd's evidence directory, and a
// subagent's payload cwd is always its parent's native cwd. A bounded worker that the parent assigned a separate managed worktree
// does every write there, so its receipt was refused at attempt 1 although identical bytes in the parent's tree passed, and a
// packet that allows no evidence write at all could only spend its three retries.
//
// An assignment is what the parent's spawn packet declared, recorded by the spawn hook (the trusted point: the parent's own tool
// call) under the parent's state directory, keyed by the exact session: AssignmentsSubdir/<session dir>/<id>.json. Its location is
// injected into the child's packet. A child never declares a root: the gate looks only at assignments the hook recorded.
//
//   - mode tree: the receipt is verified under <root>/.crw/evidence of the registered tree, with the native checks (inside the
//     root lexically and physically, a regular non-empty file that is not a link) and more: the tree must still be the directory
//     that was registered (same real path, device and inode; a link put in its place or a recreated directory is refused), its
//     .crw and .crw/evidence must be real directories, the receipt must not predate the dispatch, and when the tree was a git
//     checkout its HEAD at dispatch must still be an ancestor of its HEAD now (an unrelated history is foreign). The first agent
//     whose receipt passes claims the assignment; another agent's receipt in the same tree is foreign and refused.
//   - mode none: the packet allows no evidence write. The child ends with EVIDENCE_SCOPE_CONFLICT: <id>; the gate releases it at
//     once with a resolvable unverified verdict and marks the assignment scope-conflict, so the parent's own verification
//     (crw pabcd evidence resolve with a receipt the parent recorded) or nothing at all decides completion. It is never a pass.
//
// A dispatch without the packet markers registers nothing and the native cwd stays the only root, as before.

// AssignmentsSubdir is the directory, under the state directory, of the evidence assignments.
const AssignmentsSubdir = "evidence-assignments"

// ScopeConflictMarker is the last line a child whose packet allows no evidence write ends with: the marker, a space and its
// assignment id.
const ScopeConflictMarker = "EVIDENCE_SCOPE_CONFLICT:"

// AssignmentMode says where an assigned child's evidence lives.
type AssignmentMode string

const (
	AssignTree AssignmentMode = "tree" // under <root>/.crw/evidence of the assigned tree
	AssignNone AssignmentMode = "none" // nowhere: the packet allows no evidence write (scope conflict)
)

// The statuses of an assignment.
const (
	AssignmentOpen          = "open"
	AssignmentClaimed       = "claimed"
	AssignmentScopeConflict = "scope-conflict"
)

// Assignment is one recorded dispatch contract. RootDev and RootIno identify the tree directory registered; Head is its git HEAD
// at dispatch, "" when it was not a git checkout. AgentID and TurnID name the child that claimed it.
type Assignment struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	SessionID string         `json:"sessionId"`
	Mode      AssignmentMode `json:"mode"`
	Root      string         `json:"root"`
	RootDev   uint64         `json:"rootDev"`
	RootIno   uint64         `json:"rootIno"`
	Head      string         `json:"head"`
	CreatedAt string         `json:"createdAt"`
	Status    string         `json:"status"`
	AgentID   string         `json:"agentId"`
	TurnID    string         `json:"turnId"`
}

const assignmentVersion = 1

// sessionRecordDir is the directory name of a session's records: its sanitised id (cut, for a person to read) and a digest of
// the exact id, so that two sessions whose ids sanitise alike (a/b and a-b), or one whose id continues another's (s1 and s1-x),
// never share a directory.
func sessionRecordDir(sessionID string) string {
	name := state.SanitizeKey(sessionID)
	if len(name) > 64 {
		name = name[:64]
	}
	sum := sha256.Sum256([]byte(sessionID))
	return name + "-" + hex.EncodeToString(sum[:16])
}

// validAssignmentID is the shape of the ids NewAssignment mints (rand.Text: base32 upper case), checked before an id taken from a
// child's message names a file.
func validAssignmentID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// treeIdentity is the real path of dir and the device and inode of what it names, or an error when it is not a directory.
func treeIdentity(dir string) (real string, dev, ino uint64, err error) {
	if real, err = filepath.EvalSymlinks(dir); err != nil {
		return "", 0, 0, err
	}
	info, err := os.Lstat(real)
	if err != nil {
		return "", 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok {
		return "", 0, 0, fmt.Errorf("%s is not a directory", dir)
	}
	return real, uint64(st.Dev), uint64(st.Ino), nil // Dev is an int32 on darwin
}

// gitHead is the commit HEAD names in dir, "" when dir is not a git checkout or has no commit.
func gitHead(dir string) string {
	out, err := source.Run(dir, source.GitEnv(nil), 1<<16, "git", "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return text.Trim(string(out))
}

// NewAssignment builds the record of one dispatch for session. worktree is the tree the packet assigned ("" for none without a
// tree); it must be an absolute path to an existing directory and is stored by its real path. The record is not yet written
// (Persist does that), so a spawn that is refused later leaves nothing behind.
func NewAssignment(sessionID, worktree string, mode AssignmentMode, now time.Time) (Assignment, error) {
	if sessionID == "" {
		return Assignment{}, errors.New("an evidence assignment needs the parent's session id")
	}
	if mode != AssignTree && mode != AssignNone {
		return Assignment{}, fmt.Errorf("unknown evidence mode %q", mode)
	}
	a := Assignment{Version: assignmentVersion, ID: rand.Text(), SessionID: sessionID, Mode: mode, Status: AssignmentOpen,
		CreatedAt: now.UTC().Format(time.RFC3339Nano)}
	if worktree == "" {
		if mode == AssignTree {
			return Assignment{}, errors.New("CRW-WORKTREE names no tree")
		}
		return a, nil
	}
	if !filepath.IsAbs(worktree) {
		return Assignment{}, fmt.Errorf("CRW-WORKTREE must be an absolute path: %s", worktree)
	}
	real, dev, ino, err := treeIdentity(worktree)
	if err != nil {
		return Assignment{}, fmt.Errorf("CRW-WORKTREE %s: %w", worktree, err)
	}
	a.Root, a.RootDev, a.RootIno, a.Head = real, dev, ino, gitHead(real)
	return a, nil
}

// Persist writes the assignment under cwd (the parent's native cwd) through a temp file and a rename, refusing links in the
// state tree.
func (a Assignment) Persist(cwd string) error {
	dir, err := ensureRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(a.SessionID))
	if err != nil {
		return err
	}
	return writeRecord(filepath.Join(dir, a.ID+".json"), a)
}

// writeRecord publishes v as JSON at path through a temp file of this attempt and a rename; on a failure the temp file is removed.
func writeRecord(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), rand.Text())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := crwdir.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readAssignment decodes one record strictly: a single JSON object of the current version whose id matches its file name.
func readAssignment(path, id string) (Assignment, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Assignment{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Assignment{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var a Assignment
	if dec.Decode(&a) != nil {
		return Assignment{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return Assignment{}, false
	}
	return a, a.Version == assignmentVersion && a.ID == id && (a.Mode == AssignTree || a.Mode == AssignNone)
}

// claimAssignment binds the assignment at path to agentID under its lock, re-reading it inside: an open record is claimed, one
// already claimed by agentID is left as it is, and one claimed by another agent is refused. update, when set, changes the record
// before it is written (the scope-conflict mark).
func claimAssignment(path, id, sessionID, agentID string, update func(*Assignment) bool) bool {
	claimed := false
	_ = withFileLock(path+".lock", func() error {
		a, ok := readAssignment(path, id)
		if !ok || a.SessionID != sessionID || (a.AgentID != "" && a.AgentID != agentID) {
			return nil
		}
		next := a
		next.AgentID = agentID
		if next.Status == AssignmentOpen {
			next.Status = AssignmentClaimed
		}
		if update != nil && !update(&next) {
			return nil
		}
		if next != a {
			if err := writeRecord(path, next); err != nil {
				return err
			}
		}
		claimed = true
		return nil
	})
	return claimed
}

// AcceptAssignedReceipt reports whether receipt (absolute, or relative to the assigned tree) is a valid receipt of a tree
// assignment of the session, and binds that assignment to agentID: see the package's assignment rules above. An agent without an
// id cannot claim anything, and a receipt that two assignments would accept is refused as ambiguous.
func AcceptAssignedReceipt(cwd, sessionID, agentID, receipt string) bool {
	if agentID == "" || sessionID == "" || receipt == "" {
		return false
	}
	dir, err := existingRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(sessionID))
	if err != nil {
		return false
	}
	names, err := dirNames(dir)
	if err != nil {
		return false
	}
	var match *Assignment
	for _, name := range names {
		id, isRecord := strings.CutSuffix(name, ".json")
		if !isRecord || !validAssignmentID(id) {
			continue
		}
		a, ok := readAssignment(filepath.Join(dir, name), id)
		if !ok || a.SessionID != sessionID || a.Mode != AssignTree || !assignedReceiptValid(a, receipt) {
			continue
		}
		if match != nil {
			return false
		}
		match = &a
	}
	if match == nil || (match.AgentID != "" && match.AgentID != agentID) {
		return false
	}
	return claimAssignment(filepath.Join(dir, match.ID+".json"), match.ID, sessionID, agentID, nil)
}

// assignedReceiptValid is the receipt check against one tree assignment.
func assignedReceiptValid(a Assignment, receipt string) bool {
	if a.Root == "" || !filepath.IsAbs(a.Root) {
		return false
	}
	real, dev, ino, err := treeIdentity(a.Root)
	if err != nil || real != a.Root || dev != a.RootDev || ino != a.RootIno {
		return false
	}
	evidenceDir := filepath.Join(a.Root, crwdir.DirName, Subdir)
	if requireDirectory(filepath.Join(a.Root, crwdir.DirName)) != nil || requireDirectory(evidenceDir) != nil {
		return false
	}
	resolved := resolve(a.Root, receipt)
	if !receiptInside(evidenceDir, resolved) {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
	info, statErr := os.Lstat(resolved)
	if err != nil || statErr != nil || info.ModTime().Before(created.Truncate(time.Second)) {
		return false
	}
	if a.Head != "" {
		_, err := source.Run(a.Root, source.GitEnv(nil), 1<<16, "git", "merge-base", "--is-ancestor", a.Head, "HEAD")
		if err != nil {
			return false
		}
	}
	return true
}

// ExtractScopeConflict is the assignment id on the last line of message that holds the scope-conflict marker as a line of its own
// (leading and trailing white space aside), or none.
func ExtractScopeConflict(message string) (string, bool) {
	return lastMarkerValue(message, ScopeConflictMarker)
}

// ClaimScopeConflict marks the no-write assignment id of the session as a scope conflict of agentID and turnID and reports whether
// it did. Only an assignment of mode none qualifies, and only for the agent that claims it first; a forged or unknown id, or an
// agent without an id, is refused, and the gate then treats the stop as it did before.
func ClaimScopeConflict(cwd, sessionID, agentID, turnID, id string) bool {
	if agentID == "" || sessionID == "" || !validAssignmentID(id) {
		return false
	}
	dir, err := existingRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(sessionID))
	if err != nil {
		return false
	}
	return claimAssignment(filepath.Join(dir, id+".json"), id, sessionID, agentID, func(a *Assignment) bool {
		if a.Mode != AssignNone {
			return false
		}
		a.Status, a.TurnID = AssignmentScopeConflict, turnID
		return true
	})
}

// lastMarkerValue is the first word after marker on the last line of message that starts with marker (after JavaScript white
// space) and has a word after it. The line ends at \n, \r, U+2028 or U+2029; nothing is read past it, so a marker with nothing
// after it never takes the next line's text.
func lastMarkerValue(message, marker string) (string, bool) {
	lines := strings.FieldsFunc(message, func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == ' ' })
	isSpace := func(r rune) bool { return text.Trim(string(r)) == "" }
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(strings.TrimLeftFunc(lines[i], isSpace), marker)
		if !ok {
			continue
		}
		rest = strings.TrimLeftFunc(rest, isSpace)
		if end := strings.IndexFunc(rest, isSpace); end >= 0 {
			rest = rest[:end]
		}
		if rest != "" {
			return rest, true
		}
	}
	return "", false
}
