package stateroot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// AnchorSubdir is the directory under the CRW home (host.CRWHome: CRW_HOME, else ~/.crw) that
// records, per relay-managed thread, the native root a CRW resume path resolved for it. The
// SessionStart bootstrap reads it there, so a hook needs neither a running relay nor its store: a
// thread no CRW resume path has resolved (a standalone terminal session) has no anchor and
// bootstraps in its payload cwd exactly as before.
const AnchorSubdir = "state-roots"

// anchorVersion is the shape of an anchor file; a file of another version is not read.
const anchorVersion = 1

type anchor struct {
	Version   int    `json:"version"`
	SessionID string `json:"sessionId"`
	NativeCwd string `json:"nativeCwd"`
}

// AnchorPath is the anchor file of sessionID, or "" when the CRW home cannot be resolved or the
// id is not canonical.
func AnchorPath(env host.LookupEnv, sessionID string) string {
	if !state.IsCanonicalSessionID(sessionID) {
		return ""
	}
	home, err := host.CRWHome(env)
	if err != nil || home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, AnchorSubdir, sessionID+".json")
}

// maxAnchorBytes bounds what is read of an anchor: a real one is a few hundred bytes.
const maxAnchorBytes = 4096

// readAnchor is the native root recorded for sessionID: "" and nil when no anchor was recorded (the
// file is absent, or no CRW home resolves), and an *AnchorError when one exists and cannot be
// trusted (it cannot be opened or read, is not a regular file, is too large, is not valid, names
// another session or is of another version). A thread whose protection record is unreadable is not
// a thread without one: the caller refuses instead of bootstrapping beside work that may be in
// flight. The file is opened without blocking and judged by fstat of the open descriptor, as the
// session state is (state.ReadStateFile), so a FIFO or a device linked at the anchor path refuses
// at once.
func readAnchor(env host.LookupEnv, sessionID string) (string, error) {
	path := AnchorPath(env, sessionID)
	if path == "" {
		return "", nil
	}
	unreadable := func(err error) (string, error) {
		return "", &AnchorError{SessionID: sessionID, Path: path, Err: err, Unreadable: true}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return "", nil
	}
	if err != nil {
		return unreadable(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return unreadable(err)
	}
	if !info.Mode().IsRegular() {
		return unreadable(errors.New("the anchor is not a regular file"))
	}
	if info.Size() > maxAnchorBytes {
		return unreadable(fmt.Errorf("the anchor is larger than %d bytes", maxAnchorBytes))
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxAnchorBytes+1))
	if err != nil {
		return unreadable(err)
	}
	if len(raw) > maxAnchorBytes {
		return unreadable(fmt.Errorf("the anchor is larger than %d bytes", maxAnchorBytes))
	}
	var a anchor
	if err := json.Unmarshal(raw, &a); err != nil {
		return unreadable(err)
	}
	if a.Version != anchorVersion || a.SessionID != sessionID || !filepath.IsAbs(a.NativeCwd) {
		return unreadable(errors.New("the anchor is not a version 1 record of this thread naming an absolute native cwd"))
	}
	return a.NativeCwd, nil
}

// AnchorCode is the refusal code of a resume or SessionStart whose thread's native root could not
// be recorded as its anchor.
const AnchorCode = "state_root_anchor_unrecorded"

// AnchorUnreadableCode is the refusal code of a thread whose recorded anchor exists but cannot be
// trusted.
const AnchorUnreadableCode = "state_root_anchor_unreadable"

// AnchorError is a refusal about the thread's anchor file. Unreadable: the file exists and cannot
// be trusted, so where the thread's work lives is not known. Otherwise the native root could not be
// recorded (or moved with the thread): without the anchor a later SessionStart of the thread could
// not tell where its state lives, so it could open an empty IDLE state beside work in flight.
type AnchorError struct {
	SessionID  string
	NativeCwd  string // the root the anchor would have named; empty when Unreadable
	Path       string // the anchor file, "" when the CRW home cannot be resolved
	Unreadable bool
	Err        error
}

func (e *AnchorError) Error() string {
	where := "under the CRW home (CRW_HOME, else ~/.crw), which cannot be resolved"
	if e.Path != "" {
		where = "at " + e.Path
	}
	if e.Unreadable {
		return fmt.Sprintf("%s: the recorded PABCD state root of thread %s %s cannot be trusted (%v), so where its work lives "+
			"is not known and no state is created or moved on its account. Repair or remove the file by hand (it names the thread's "+
			"native cwd) and retry", AnchorUnreadableCode, e.SessionID, where, e.Err)
	}
	return fmt.Sprintf("%s: the native cwd %s of thread %s could not be recorded %s (%v), so the thread's SessionStart "+
		"could not be kept from opening an empty IDLE state beside its PABCD work. Make the CRW home writable and retry",
		AnchorCode, e.NativeCwd, e.SessionID, where, e.Err)
}

func (e *AnchorError) Unwrap() error { return e.Err }

// CodeOf is the refusal code of an error Guard, Resolve, Moved or Bootstrap returned: the anchor
// code of an *AnchorError, Code otherwise.
func CodeOf(err error) string {
	if anchorErr := (*AnchorError)(nil); errors.As(err, &anchorErr) {
		if anchorErr.Unreadable {
			return AnchorUnreadableCode
		}
		return AnchorCode
	}
	return Code
}

// writeAnchor records root as sessionID's native root, replacing the file whole (written beside it
// and renamed over it), and reports why it could not: a failure leaves the previous anchor, or none.
func writeAnchor(env host.LookupEnv, sessionID, root string) error {
	path := AnchorPath(env, sessionID)
	fail := func(err error) error {
		return &AnchorError{SessionID: sessionID, NativeCwd: root, Path: path, Err: err}
	}
	if path == "" {
		return fail(errors.New("no absolute CRW home"))
	}
	if !filepath.IsAbs(root) {
		return fail(fmt.Errorf("the root %q is not absolute", root))
	}
	raw, err := json.Marshal(anchor{Version: anchorVersion, SessionID: sessionID, NativeCwd: root})
	if err != nil {
		return fail(err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(dir, "."+sessionID+".*.tmp")
	if err != nil {
		return fail(err)
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fail(err)
	}
	return nil
}

// recordAnchor records root as sessionID's anchor unless the anchor already names it. An absent
// anchor is always written: it is not the empty path, which Same would resolve to the process's own
// cwd. An anchor that exists and cannot be trusted is not replaced.
func recordAnchor(env host.LookupEnv, sessionID, root string) error {
	anchored, err := readAnchor(env, sessionID)
	if err != nil {
		return err
	}
	if anchored != "" && Same(root, anchored) {
		return nil
	}
	return writeAnchor(env, sessionID, clean(root))
}

// nativeRoot is the root that holds the thread's work: the anchored root while it holds work in
// flight, else the host's cwd. An anchor that cannot be trusted is an error, not an absent anchor.
func nativeRoot(env host.LookupEnv, hostCwd, sessionID string) (string, error) {
	anchored, err := readAnchor(env, sessionID)
	if err != nil {
		return "", err
	}
	if anchored != "" {
		if _, flight, _, _ := inFlight(anchored, sessionID); flight {
			return anchored, nil
		}
	}
	return hostCwd, nil
}

// judge is Check of targetCwd (the host's cwd when the resume sends none) against root.
func judge(root, hostCwd, targetCwd, sessionID string) error {
	if targetCwd == "" {
		targetCwd = hostCwd
	}
	if conflict := Check(root, targetCwd, sessionID); conflict != nil {
		return conflict
	}
	return nil
}

// Resolve is Check for a CRW resume path of sessionID, nothing written: hostCwd is the cwd the host
// reports for the thread now, targetCwd the cwd the resume would send ("" when it sends none, so
// the thread runs wherever the host has it). The native root is the thread's anchor while that
// root holds work in flight, whatever the host reports now (an external resume may have moved the
// host's cwd away from it), and the host's cwd otherwise. The target is judged against that root
// with Check; a resume that sends no cwd is judged at the host's cwd. The error is a *Conflict, or
// an *AnchorError when the thread's anchor exists and cannot be trusted.
func Resolve(env host.LookupEnv, hostCwd, targetCwd, sessionID string) error {
	if !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	root, err := nativeRoot(env, hostCwd, sessionID)
	if err != nil {
		return err
	}
	return judge(root, hostCwd, targetCwd, sessionID)
}

// Guard is Resolve preceded by the record of the native root, run before anything is sent. The
// root is recorded as the thread's anchor whether or not a state file exists yet (a thread a CRW
// resume path has resolved is a relay-managed thread, and its first SessionStart is still to
// come), so a SessionStart bootstrap of that thread applies the same resolution. The root is the
// one the thread is on before the resume: it is not moved to a target the resume has not reached
// (Moved does that once the host reports where the thread runs), so a resume that fails leaves the
// protection where the work is. A thread with no anchor that no CRW path resolves (a standalone
// terminal session) has none.
//
// The error is the *Conflict of Resolve, or an *AnchorError when the anchor cannot be trusted or the
// root had to be recorded and could not be (an anchor that already names the root needs no write):
// a resume whose SessionStart could not be guarded is not sent. A host that reports no cwd gives no
// root to record or compare. CodeOf names the refusal code.
func Guard(env host.LookupEnv, hostCwd, targetCwd, sessionID string) error {
	if !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	root, err := nativeRoot(env, hostCwd, sessionID)
	if err != nil {
		return err
	}
	var unrecorded error
	if root != "" {
		unrecorded = recordAnchor(env, sessionID, root)
	}
	if conflict := judge(root, hostCwd, targetCwd, sessionID); conflict != nil {
		return conflict
	}
	return unrecorded
}

// Moved records, after a resume of sessionID reached the host, that the thread now runs at ranAt,
// the cwd the host reported in its answer: that is its native root when nothing was in flight at
// the root it left (a resume that would have detached work was refused before it was sent). The
// requested cwd is not what the thread runs at: a host that ran it elsewhere is followed, and an
// answer that reports no cwd moves nothing (the thread's next SessionStart re-anchors it). A move
// away from work in flight records nothing. The resume has already happened, so an error here (the
// anchor could not be trusted or could not be written) is not a refusal of the resume but of what
// would follow it: the caller withholds the message.
func Moved(env host.LookupEnv, hostCwd, ranAt, sessionID string) error {
	if ranAt == "" || !state.IsCanonicalSessionID(sessionID) {
		return nil
	}
	if err := Resolve(env, hostCwd, ranAt, sessionID); err != nil {
		if conflictOf(err) != nil {
			return nil
		}
		return err
	}
	return recordAnchor(env, sessionID, ranAt)
}

func conflictOf(err error) *Conflict {
	var c *Conflict
	if errors.As(err, &c) {
		return c
	}
	return nil
}

// Hold is the read-only part of Bootstrap: the SessionStart or state-writing hook of sessionID at
// cwd is judged against the thread's anchor, and nothing is written. It is nil when the thread has
// no anchor (a standalone session), when cwd is the anchored root and when the anchored root holds
// nothing in flight; otherwise the *Conflict names the preserved state, or the *AnchorError says
// the anchor cannot be trusted. A state file that already exists at cwd does not exempt it: a
// legacy IDLE state beside work in flight at the anchored root is refused as the resume to cwd is,
// so the agent is sent to the root that holds its work.
func Hold(env host.LookupEnv, cwd, sessionID string) error {
	if env == nil {
		env = os.LookupEnv
	}
	root, err := readAnchor(env, sessionID)
	if err != nil {
		return err
	}
	if root == "" {
		return nil
	}
	return judge(root, cwd, cwd, sessionID)
}

// Bootstrap is Hold for the SessionStart bootstrap, and for a hook about to write the state at cwd
// (a prompt's writers, the idle-edit counter), which then follows the thread: an anchored thread
// whose root held nothing in flight starts its work at cwd, so the anchor moves there before the
// state is created. A bootstrap or write whose anchor cannot follow is refused, so no state exists
// at a cwd the anchor does not track. A thread without an anchor records nothing.
func Bootstrap(env host.LookupEnv, cwd, sessionID string) error {
	if env == nil {
		env = os.LookupEnv
	}
	if err := Hold(env, cwd, sessionID); err != nil {
		return err
	}
	root, err := readAnchor(env, sessionID)
	if err != nil || root == "" || cwd == "" || Same(root, cwd) {
		return err
	}
	return writeAnchor(env, sessionID, clean(cwd))
}
