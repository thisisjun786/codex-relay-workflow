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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
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
// The contract of a child is found from the child itself, never from the order in which receipts arrive. The packet the spawn hook
// injected carries [CRW-EVIDENCE-ASSIGNMENT:<id>]; the SubagentStop payload names that child's own transcript, a Codex rollout whose
// first UserMessage item of the child's own thread is that packet, so the id in it is the assignment this very actor was dispatched
// with. A marker anywhere else in the transcript (a tool's output, the child's words, a packet the child sent to an agent it
// spawned, a forked parent history) is not its contract. A transcript that shows no packet falls back on the id the child cites on a
// line EVIDENCE_ASSIGNMENT: <id>, and an actor that already claimed an assignment keeps it. A child that has a contract is judged by
// it alone: its native-cwd receipt counts for nothing, a contract it names that cannot be read (removed, damaged, an unreadable
// directory) refuses the receipt instead of falling back on the native root, and a different actor, or an actor whose packet did not
// hold the id, cannot claim it. An actor without an agent id claims nothing, but the contract its packet names stands and refuses its
// receipt. A child tied to no dispatch at all (no readable packet, no citation, nothing claimed) is refused while an assignment of the
// session is still unclaimed, because it may be that assignment's child. Only a child with no contract at all keeps the native root.
//
//   - mode tree: the receipt is verified under <root>/.crw/evidence of the registered tree, with the native checks (inside the
//     root lexically and physically, a regular non-empty file that is not a link) and more: the tree must still be the directory
//     that was registered (same real path, device and inode; a link put in its place or a recreated directory is refused), its
//     .crw and .crw/evidence must be real directories, the receipt must not predate the dispatch (at the precision the dispatch
//     time was recorded with), and when the tree was a git checkout its HEAD at dispatch must still be an ancestor of its HEAD now
//     (an unrelated history is foreign). The first actor whose receipt passes claims the assignment; another actor's receipt is
//     refused. Each dispatch has its own id, so a finished dispatch of the same tree is never a candidate for a later one.
//   - mode none: the packet allows no evidence write. The child ends with EVIDENCE_SCOPE_CONFLICT: <id>; the gate releases it at
//     once with a resolvable unverified verdict and marks the assignment scope-conflict, so the parent's own verification
//     (crw pabcd evidence resolve with a receipt the parent recorded) or nothing at all decides completion. It is never a pass,
//     and a receipt in the native cwd does not turn it into one.
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
// at dispatch, "" when it was not a git checkout. ToolUseID is the native tool call that registered it ("" when the call had none),
// the only call whose second pass of the spawn hook may reuse it. AgentID and TurnID name the child that claimed it.
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
	ToolUseID string         `json:"toolUseId,omitempty"`
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
	out, err := source.Probe(dir, source.ProbeOptions{Limit: 1 << 16, Timeout: gitprobe.Timeout}, "rev-parse", "--verify", "-q", "HEAD^{commit}")
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
// state tree. An error means the spawn is refused, so nothing of it may stay open: a record that the rename published before a
// later step failed (its directory sync) is removed again, because an open record no child will ever claim would refuse every
// later child of the session that is tied to no dispatch. When that removal fails too, the error says the record is left and
// where, so the refusal names what the parent has to remove.
func (a Assignment) Persist(cwd string) error {
	dir, err := ensureRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(a.SessionID))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, a.ID+".json")
	err = writeRecord(path, a)
	if err == nil {
		return nil
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("%w; the published record %s could not be removed and stays open until it is removed: %v", err, path, rmErr)
		}
	}
	return err
}

// Remove deletes the assignment's record under cwd, best effort: the spawn that would have used it was refused after the record was
// written, and nothing may be left that no packet names.
func (a Assignment) Remove(cwd string) {
	if dir, err := existingRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(a.SessionID)); err == nil {
		removeFile(filepath.Join(dir, a.ID+".json"))
	}
}

// LeadingAssignmentID is the id of the assignment block that text starts with (the place the spawn hook writes it), and whether it
// does. A marker anywhere later in a packet is the parent's own text and is not looked at.
func LeadingAssignmentID(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, AssignmentMarker+":")
	if !ok {
		return "", false
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 || !validAssignmentID(rest[:end]) {
		return "", false
	}
	return rest[:end], true
}

// RecordedAssignment is the readable record of the session that id names, and whether there is one.
func RecordedAssignment(cwd, sessionID, id string) (Assignment, bool) {
	if sessionID == "" || !validAssignmentID(id) {
		return Assignment{}, false
	}
	dir, err := existingRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(sessionID))
	if err != nil {
		return Assignment{}, false
	}
	a, ok := readAssignment(filepath.Join(dir, id+".json"), id)
	return a, ok && a.SessionID == sessionID
}

// RegisteredBy reports whether a is the record the native tool call toolUseID registered for this very request and that no actor has
// claimed yet: the same call (a call without an id proves nothing), the mode asked for, and for a tree the same real directory. The
// hook running over its own output reuses such a record instead of registering the packet a second time; another call that carries
// a copy of the block (the hook's output with another task) is a dispatch of its own and must not share the assignment.
func (a Assignment) RegisteredBy(toolUseID, worktree string, mode AssignmentMode) bool {
	if toolUseID == "" || a.ToolUseID != toolUseID || a.Status != AssignmentOpen || a.AgentID != "" || a.Mode != mode {
		return false
	}
	if worktree == "" {
		return a.Root == ""
	}
	real, _, _, err := treeIdentity(worktree)
	return err == nil && real == a.Root
}

// The two syncs of a durable record write, as variables so that a test can see their order and fail them.
var (
	syncFile      = func(f *os.File) error { return f.Sync() }
	syncDirectory = crwdir.SyncDir
)

// writeRecord publishes v as JSON at path through a temp file of this attempt and a rename; on a failure before the rename the temp
// file is removed. The data is fsynced before the rename and the directory after it, so a record that this returns nil for survives
// a power failure whole (CRW-1110: a verdict moved beside the main list is the only copy once the list is shortened). An error
// from the directory sync is returned although the rename has happened: the record is in place, but not known to be durable.
func writeRecord(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), rand.Text())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = syncFile(f)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := crwdir.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// syncRecordChain makes the directories ensureRecordDir created, or may have created, durable: each directory below cwd/.crw named
// by parts, then cwd/.crw and cwd, deepest first, so the entry of every directory on the way to a record is on stable storage.
func syncRecordChain(cwd string, parts ...string) error {
	dirs := []string{stateDir(cwd)}
	for _, part := range parts {
		dirs = append(dirs, filepath.Join(dirs[len(dirs)-1], part))
	}
	dirs = append(dirs, cwd)
	var err error
	for i := len(dirs) - 1; i >= 0; i-- {
		err = errors.Join(err, syncDirectory(dirs[i]))
	}
	return err
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

// AssignedVerdict is the outcome of judging a receipt against the child's contract.
type AssignedVerdict int

const (
	NoContract       AssignedVerdict = iota // the child has no recorded contract: the native root decides
	AssignedAccepted                        // the receipt is valid in the child's assigned tree and the child holds the assignment
	AssignedRefused                         // the child has a contract and the receipt does not satisfy it
)

// AssignmentMarker opens the block of a child's packet that names its assignment: [CRW-EVIDENCE-ASSIGNMENT:<id>].
const AssignmentMarker = "[CRW-EVIDENCE-ASSIGNMENT"

// AssignmentCitation is the line a child writes to name its assignment where the harness gives no transcript.
const AssignmentCitation = "EVIDENCE_ASSIGNMENT:"

// transcriptReadLimit bounds how much of a child's transcript is read for its packet, which is near its start.
const transcriptReadLimit = 8 << 20

// dispatchPacket is the text of the packet the child at path was dispatched with, and whether the transcript showed one. The
// transcript is a Codex rollout (JSON lines): the packet is the first UserMessage item of the child's own thread (the id of the
// rollout's session_meta). Nothing else in the transcript is the packet: a tool's output, the child's own words, a message the child
// sent to an agent it spawned, a later input and the items of a forked parent history all live in other lines or other threads, and
// the harness writes each line as one JSON value, so no output can forge one. The file is opened without blocking and must be a
// regular file, so a FIFO or a device at the path never holds the gate.
func dispatchPacket(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(f, transcriptReadLimit))
	if err != nil {
		return "", false
	}
	type rolloutLine struct {
		Type    string `json:"type"`
		Payload struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		} `json:"payload"`
	}
	own, sawMeta := "", false
	for _, text := range bytes.Split(raw, []byte{'\n'}) {
		var line rolloutLine
		if json.Unmarshal(text, &line) != nil {
			continue
		}
		switch {
		case line.Type == "session_meta" && !sawMeta:
			own, sawMeta = line.Payload.ID, true
		case line.Type == "event_msg" && line.Payload.Type == "item_completed" && line.Payload.Item.Type == "UserMessage":
			if own != "" && line.Payload.ThreadID != own {
				continue // an item of another thread: a forked parent history
			}
			var b strings.Builder
			for _, p := range line.Payload.Item.Content {
				if p.Type == "text" {
					b.WriteString(p.Text)
				}
			}
			return b.String(), true
		}
	}
	return "", false
}

// packetAssignmentID is the id of the first [CRW-EVIDENCE-ASSIGNMENT:<id>] block of a packet, "" when it has none. The spawn hook
// puts its block right after the guard, before the parent's own text.
func packetAssignmentID(packet string) string {
	rest := packet
	for {
		i := strings.Index(rest, AssignmentMarker+":")
		if i < 0 {
			return ""
		}
		rest = rest[i+len(AssignmentMarker)+1:]
		if end := strings.IndexByte(rest, ']'); end >= 0 && validAssignmentID(rest[:end]) {
			return rest[:end]
		}
	}
}

// contractID is the assignment id this child was dispatched with, and whether its transcript showed its packet. When the packet was
// found its marker alone decides ("" is a dispatch without a contract, and an id the child cites is ignored, so an actor cannot
// name a packet it was not given); when it was not, the id the child cites on an EVIDENCE_ASSIGNMENT: line stands in for it.
func contractID(transcriptPath, message string) (id string, fromPacket bool) {
	if packet, ok := dispatchPacket(transcriptPath); ok {
		return packetAssignmentID(packet), true
	}
	if cited, ok := lastMarkerValue(message, AssignmentCitation); ok && validAssignmentID(cited) {
		return cited, false
	}
	return "", false
}

// childAssignment finds the assignment of the session that this child was dispatched with and says what decides the child:
// NoContract when it has none (the native root), AssignedRefused when its packet or citation names a contract that cannot be read
// or does not belong to the session (removed, damaged, an unreadable directory: never the native root), and AssignedAccepted with
// the record when it is there. Where the transcript showed no packet and the child cites nothing, the one assignment an actor of
// this id already claimed is its contract; with none claimed, the child is refused while an assignment of the session is open.
func childAssignment(cwd, sessionID, agentID, transcriptPath, message string) (Assignment, AssignedVerdict) {
	id, fromPacket := contractID(transcriptPath, message)
	dir, dirErr := existingRecordDir(cwd, AssignmentsSubdir, sessionRecordDir(sessionID))
	load := func(id string) (Assignment, bool) {
		a, ok := readAssignment(filepath.Join(dir, id+".json"), id)
		return a, ok && a.SessionID == sessionID
	}
	if id != "" {
		if dirErr != nil {
			return Assignment{}, AssignedRefused
		}
		if a, ok := load(id); ok {
			return a, AssignedAccepted
		}
		return Assignment{}, AssignedRefused
	}
	if fromPacket || errors.Is(dirErr, fs.ErrNotExist) {
		return Assignment{}, NoContract
	}
	if dirErr != nil {
		return Assignment{}, AssignedRefused // a contract this actor holds cannot be ruled out
	}
	names, err := dirNames(dir)
	if err != nil {
		return Assignment{}, AssignedRefused
	}
	var held *Assignment
	open := false
	for _, name := range names {
		id, isRecord := strings.CutSuffix(name, ".json")
		if !isRecord || !validAssignmentID(id) {
			continue
		}
		if a, ok := load(id); ok {
			if agentID != "" && a.AgentID == agentID && (held == nil || a.CreatedAt > held.CreatedAt) {
				held = &a
			}
			open = open || a.AgentID == "" && a.Status == AssignmentOpen
		}
	}
	if held != nil {
		return *held, AssignedAccepted
	}
	// The child is tied to no dispatch (no packet in a readable transcript, no citation, nothing claimed) while a recorded
	// assignment of the session is still unclaimed: that child may be the one it was made for, and the parent's native tree is no
	// proof of anything for it. It is refused until its transcript or its citation says which dispatch it is.
	if open {
		return Assignment{}, AssignedRefused
	}
	return Assignment{}, NoContract
}

// JudgeAssignedReceipt judges receipt (absolute, or relative to the assigned tree) of the child agentID against the contract that
// child was dispatched with (see the rules above) and binds the assignment to the child when the receipt passes. A child with no
// contract is NoContract and the caller applies the native root; a child whose contract cannot be read is refused, and so is one
// without an agent id whose transcript names a contract: it can claim nothing, but the contract it was dispatched with stands, and
// the parent's native tree is no way around it (CRW-1106).
func JudgeAssignedReceipt(cwd, sessionID, agentID, transcriptPath, message, receipt string) AssignedVerdict {
	if sessionID == "" {
		return NoContract
	}
	a, found := childAssignment(cwd, sessionID, agentID, transcriptPath, message)
	if found != AssignedAccepted {
		return found
	}
	if agentID == "" {
		return AssignedRefused
	}
	if a.Mode != AssignTree || receipt == "" || (a.AgentID != "" && a.AgentID != agentID) || !assignedReceiptValid(a, receipt) {
		return AssignedRefused
	}
	dir := filepath.Join(stateDir(cwd), AssignmentsSubdir, sessionRecordDir(sessionID))
	if !claimAssignment(filepath.Join(dir, a.ID+".json"), a.ID, sessionID, agentID, nil) {
		return AssignedRefused
	}
	return AssignedAccepted
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
	// The receipt must not predate its own dispatch, at the precision the dispatch time was recorded with: a receipt of an
	// earlier dispatch written in the same second is as stale as one written a day before.
	created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
	info, statErr := os.Lstat(resolved)
	if err != nil || statErr != nil || info.ModTime().Before(created) {
		return false
	}
	if a.Head != "" {
		_, err := source.Probe(a.Root, source.ProbeOptions{Limit: 1 << 16, Timeout: gitprobe.Timeout}, "merge-base", "--is-ancestor", a.Head, "HEAD")
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
// it did. Only an assignment of mode none qualifies, only for the actor the dispatch was made to (the id in its own transcript
// must be this one when the transcript can be read) and only for the actor that claims it first; a forged or unknown id, or an
// agent without an id, is refused, and the gate then treats the stop as it did before.
func ClaimScopeConflict(cwd, sessionID, agentID, transcriptPath, turnID, id string) bool {
	if agentID == "" || sessionID == "" || !validAssignmentID(id) {
		return false
	}
	if own, fromPacket := contractID(transcriptPath, ""); fromPacket && own != id {
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
