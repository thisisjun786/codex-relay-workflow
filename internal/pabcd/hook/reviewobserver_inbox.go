package hook

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"golang.org/x/sys/unix"
)

// The review observer's inbox (CRW-1113, A3-06, port: fixed). The oracle holds a parsed sign-off in a local variable only: a session or
// goalplan lock held by a live writer for the observer's whole wait drops it, the hook still releases the child with an empty answer,
// and the audit round stays in_flight for good although the reviewer finished. The observer now persists the small parsed sign-off first,
// with the identity it will be judged by, and then tries to publish it. An entry that was not published stays; the next legitimate
// moment drains it under the session lock then the goalplan lock (the order every writer of this tree follows): the next SubagentStop of
// the same session, and the A>B transition, which drains inside the goalplan lock of its own publication under the session lock it holds. A drain judges every entry
// again against the binding as it is then, and publishes through the same code as a live sign-off, so an entry for a superseded epoch, a
// session that left A, another work-phase, a child that is not the round's reviewer or a round that already holds a verdict is
// ignored with a ledger row and removed. Draining twice is the same as draining once: an approved round is no longer in_flight.
// Nothing here blocks a child or polls, and only the SubagentStop of a reviewer-shaped child (never a worker or executor, which the
// receipt gate owns) can add an entry.
//
// Three properties keep the queue honest. The bound of reviewObserverInboxMax entries is checked and the entry linked under one
// exclusive lock of the session's inbox directory, so observers that run at once cannot exceed it. A drain reads every entry there is
// (up to reviewObserverInboxReadMax, a margin for the entries a racing or older observer left over the bound) and applies them in the
// order they arrived in, never the first names of an arbitrary listing. And an inbox that cannot be read, or an entry that cannot be
// opened or read for a reason that says nothing about its content (access refused, an I/O error), is not an empty inbox and not a
// corrupt entry: nothing is deleted or applied ahead of what could not be read, the drain reports the entries it left, and the A>B
// transition refuses until it can judge them. Only an entry that was read and is not an entry (not a regular file, too large, not
// JSON, no launch or agent) is corrupt and removed.
const (
	reviewObserverInboxDir     = "review-inbox"
	reviewObserverInboxMax     = 64
	reviewObserverInboxReadMax = 4 * reviewObserverInboxMax
	reviewObserverInboxMaxSize = 4096
	reviewObserverDetailMax    = 200
	reviewObserverLockWait     = 2 * time.Second

	// reviewObserverInboxFailed is the ledger event of a sign-off that could not be kept; reviewObserverWriteFailed is that of a
	// verdict that was valid but whose plan write failed before publication, which stays in the inbox for the next drain;
	// reviewObserverWriteUnsynced is that of a verdict whose plan was published but whose directory sync failed afterwards: it is
	// recorded, and only its durability is in doubt.
	reviewObserverInboxFailed   goalplan.GoalplanLedgerEvent = "review_signoff_inbox_failed"
	reviewObserverWriteFailed   goalplan.GoalplanLedgerEvent = "review_signoff_write_failed"
	reviewObserverWriteUnsynced goalplan.GoalplanLedgerEvent = "review_signoff_write_unsynced"
)

// reviewInboxEntry is one kept sign-off with the identity it is judged by: the session and plan it arrived in, the plan epoch and
// work-phase it was received under, the launch it names and the child that wrote it. It holds no message text.
type reviewInboxEntry struct {
	Version     int              `json:"version"`
	SessionID   string           `json:"sessionId"`
	Slug        string           `json:"slug"`
	PlanEpoch   string           `json:"planEpoch"`
	WorkPhaseID string           `json:"workPhaseId"`
	LaunchID    string           `json:"launchId"`
	AgentID     string           `json:"agentId"`
	Verdict     goalplan.Verdict `json:"verdict"`
	// CRW-1116: keep the counted verdict whole, including when a lock defers its publication.
	Blockers   int      `json:"blockers,omitempty"`
	Findings   []string `json:"findings,omitempty"`
	ReceivedAt string   `json:"receivedAt"`
}

// reviewInboxItem is an entry with the name of the inbox file it came from; an empty name is an entry held in memory only.
type reviewInboxItem struct {
	entry reviewInboxEntry
	name  string
}

// reviewObserverInboxName is the entry's identity: one child's sign-off for one launch of one session and plan is one file, whatever
// the number of times it is delivered or kept.
func reviewObserverInboxName(e reviewInboxEntry) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{e.SessionID, e.Slug, e.LaunchID, e.AgentID}, "\x00")))
	return hex.EncodeToString(sum[:16]) + ".json"
}

// reviewObserverInboxOwnName reports whether name has the shape reviewObserverInboxName gives: 32 lower-case hex digits and ".json".
// The observer reads, counts and removes only such names; anything else in the directory is not its own and is left alone.
func reviewObserverInboxOwnName(name string) bool {
	hexPart, ok := strings.CutSuffix(name, ".json")
	if !ok || len(hexPart) != 32 {
		return false
	}
	for _, c := range hexPart {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// reviewObserverInboxOpen opens the session's inbox directory cwd/.crw/review-inbox/<session> one component at a time from the
// workspace, never following a link: a link or a non-directory where .crw, review-inbox or the session directory belongs is an
// error, so nothing outside the workspace's own inbox is ever created, read or removed. Every later step (create, list, read,
// remove) runs relative to the returned descriptor, so a component swapped after this open is not followed either. create makes the
// missing review-inbox and session directories; without it a missing one is an error.
func reviewObserverInboxOpen(cwd, sessionID string, create bool) (*os.File, error) {
	key := state.SanitizeKey(sessionID)
	if key == "." || key == ".." {
		return nil, errors.New("the session id names no inbox directory")
	}
	if create {
		if _, err := crwdir.EnsureDir(cwd); err != nil {
			return nil, err
		}
	}
	base, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: base, Err: err}
	}
	dir, path := os.NewFile(uintptr(fd), base), base
	for _, name := range []string{crwdir.DirName, reviewObserverInboxDir, key} {
		path = filepath.Join(path, name)
		if create && name != crwdir.DirName {
			if err := unix.Mkdirat(int(dir.Fd()), name, 0o777); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = dir.Close()
				return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
			}
		}
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = dir.Close()
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		dir = os.NewFile(uintptr(fd), path)
	}
	return dir, nil
}

// reviewObserverInboxCounted is the point between the capacity check and the link of an entry, inside the inbox lock. It is a no-op;
// a test replaces it (export_test.go) to hold an invocation exactly here. Production never assigns it.
var reviewObserverInboxCounted = func() {}

// reviewObserverInboxLock takes the exclusive lock of the inbox directory dir, the one that serialises bounded admission. The lock
// belongs to this open directory descriptor, so another observer (a process, or a goroutine with its own descriptor) waits for it;
// it is released with the descriptor or by the returned function. Its holders only count and link, so the wait is short and bounded:
// an inbox that stays locked is an inbox failure, which the caller reports and judges in memory.
func reviewObserverInboxLock(dir *os.File) (release func(), err error) {
	fd := int(dir.Fd())
	deadline := time.Now().Add(reviewObserverLockWait)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) || !time.Now().Before(deadline) {
			return nil, &os.PathError{Op: "flock", Path: dir.Name(), Err: err}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// reviewObserverInboxPut keeps e. The file is complete before it is visible (a hard link of a synced temporary file), so a drain never
// reads half an entry, and an entry that exists is kept as the first one wrote it. A full inbox is an error, not a deletion. The
// capacity check, the receipt time and the link are one step under the inbox lock, so the order of the receipt times is the order
// the entries became visible in: a drain that sees a later sign-off sees every earlier one.
func reviewObserverInboxPut(cwd string, e reviewInboxEntry) error {
	dir, err := reviewObserverInboxOpen(cwd, e.SessionID, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	dfd := int(dir.Fd())
	final := reviewObserverInboxName(e)
	unlock, err := reviewObserverInboxLock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	var existing unix.Stat_t
	if unix.Fstatat(dfd, final, &existing, unix.AT_SYMLINK_NOFOLLOW) == nil {
		return nil
	}
	names, err := reviewObserverInboxNames(dir)
	if err != nil {
		return err
	}
	if len(names) >= reviewObserverInboxMax {
		return errors.New("the review inbox is full")
	}
	reviewObserverInboxCounted()
	e.ReceivedAt = time.Now().UTC().Format(reviewObserverReceivedAtLayout)
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmpName := ".tmp-" + rand.Text()
	fd, err := unix.Openat(dfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "open", Path: filepath.Join(dir.Name(), tmpName), Err: err}
	}
	tmp := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), tmpName))
	defer func() { _ = unix.Unlinkat(dfd, tmpName, 0) }()
	_, werr := tmp.Write(append(body, '\n'))
	serr := tmp.Sync()
	if err := errors.Join(werr, serr, tmp.Close()); err != nil {
		return err
	}
	if err := unix.Linkat(dfd, tmpName, dfd, final, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return &os.LinkError{Op: "link", Old: tmpName, New: final, Err: err}
	}
	return nil
}

// reviewObserverInboxNames lists the observer's own entry names in dir. It reads a fresh descriptor of the same directory, so dir's
// own read offset is never consumed.
func reviewObserverInboxNames(dir *os.File) ([]string, error) {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	listing := os.NewFile(uintptr(fd), dir.Name())
	defer listing.Close()
	all, err := listing.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, name := range all {
		if reviewObserverInboxOwnName(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// errReviewInboxCorrupt marks a file that was read and is not an inbox entry. It is the only reason an entry is removed unjudged.
var errReviewInboxCorrupt = errors.New("not an inbox entry")

// reviewObserverInboxReadFile reads one entry file of dir: never through a link, never a non-regular file, at most
// reviewObserverInboxMaxSize bytes. An error that is errReviewInboxCorrupt (or a link under the entry's name, ELOOP) says the file
// holds no entry; ENOENT says another drain took it; any other error is a failure to read that says nothing about the file.
func reviewObserverInboxReadFile(dir *os.File, name string) (reviewInboxEntry, error) {
	var e reviewInboxEntry
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return e, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return e, err
	}
	if !info.Mode().IsRegular() {
		return e, errReviewInboxCorrupt
	}
	raw, err := io.ReadAll(io.LimitReader(f, reviewObserverInboxMaxSize+1))
	if err != nil {
		return e, err
	}
	if len(raw) > reviewObserverInboxMaxSize || json.Unmarshal(raw, &e) != nil || e.Version != 1 || e.LaunchID == "" || e.AgentID == "" {
		return e, errReviewInboxCorrupt
	}
	return e, nil
}

// reviewInboxRead is what a read of a session's inbox found. dir is the open inbox directory (nil when there is none or it could
// not be opened); items are the kept entries, oldest first; corrupt are the names of entries that were read and are not entries;
// blocked says, in bounded text, what could not be read, so the entries it holds are unknown: while it is not empty nothing is
// applied, because an unread entry decides how the others are judged. The caller closes dir.
type reviewInboxRead struct {
	dir     *os.File
	items   []reviewInboxItem
	corrupt []string
	blocked []string
}

// reviewObserverInboxAbsent reports whether err says there is no inbox of ours at the place: nothing there, or a link or a
// non-directory where a directory belongs (reviewObserverInboxOpen refuses to create or use one, so no entry can be kept there).
func reviewObserverInboxAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP)
}

// reviewObserverInboxReadAll opens the session's inbox and reads every entry of ours in it, oldest first (receipt time, then name).
// A missing or refused-by-shape inbox is no entries; an inbox that cannot be opened or listed for any other reason, an entry that
// cannot be read, and an inbox with more entries than reviewObserverInboxReadMax are blocked, not empty.
func reviewObserverInboxReadAll(cwd, sessionID string) (r reviewInboxRead) {
	dir, err := reviewObserverInboxOpen(cwd, sessionID, false)
	if err != nil {
		if !reviewObserverInboxAbsent(err) {
			r.blocked = append(r.blocked, reviewObserverBounded("the review inbox could not be opened: "+err.Error()))
		}
		return r
	}
	r.dir = dir
	names, err := reviewObserverInboxNames(dir)
	if err != nil {
		r.blocked = append(r.blocked, reviewObserverBounded("the review inbox could not be listed: "+err.Error()))
		return r
	}
	if len(names) > reviewObserverInboxReadMax {
		r.blocked = append(r.blocked, "the review inbox holds "+strconv.Itoa(len(names))+" entries, more than can be ordered")
		return r
	}
	sort.Strings(names)
	for _, name := range names {
		e, err := reviewObserverInboxReadFile(dir, name)
		switch {
		case err == nil:
			r.items = append(r.items, reviewInboxItem{entry: e, name: name})
		case errors.Is(err, unix.ENOENT):
			// taken by another drain between the listing and the read
		case errors.Is(err, errReviewInboxCorrupt), errors.Is(err, unix.ELOOP):
			r.corrupt = append(r.corrupt, name)
		default:
			r.blocked = append(r.blocked, reviewObserverBounded("the review inbox entry "+name+" could not be read: "+err.Error()))
		}
	}
	sort.SliceStable(r.items, func(i, j int) bool { return r.items[i].entry.ReceivedAt < r.items[j].entry.ReceivedAt })
	return r
}

// reviewObserverInboxRemove removes one of the observer's own entries from dir, never anything else.
func reviewObserverInboxRemove(dir *os.File, name string) {
	if dir == nil || !reviewObserverInboxOwnName(name) {
		return
	}
	_ = unix.Unlinkat(int(dir.Fd()), name, 0)
}

// reviewObserverReceivedAtLayout orders kept entries: fixed-width nanoseconds, so the text sorts as the time does and two sign-offs a
// millisecond apart are not left to their file names' order.
const reviewObserverReceivedAtLayout = "2006-01-02T15:04:05.000000000Z"

func reviewObserverBounded(s string) string {
	if len(s) > reviewObserverDetailMax {
		return strings.ToValidUTF8(s[:reviewObserverDetailMax], "") + "..."
	}
	return s
}

// reviewObserverNewEntry is the identity a sign-off is kept with. The work-phase is the one the round was opened for when the launch
// names a round the plan shows, else the one active now; both are read without a lock, which is why a drain judges them again.
func reviewObserverNewEntry(sessionID string, st state.State, plan *goalplan.Goalplan, agentID string, signoff *review.ReviewSignoff) reviewInboxEntry {
	e := reviewInboxEntry{Version: 1, SessionID: sessionID, Slug: st.Slug, LaunchID: signoff.LaunchID, AgentID: agentID, Verdict: signoff.Verdict,
		Blockers: signoff.Blockers, Findings: signoff.Findings,
		ReceivedAt: time.Now().UTC().Format(reviewObserverReceivedAtLayout)}
	if st.PlanEpoch != nil {
		e.PlanEpoch = *st.PlanEpoch
	}
	if plan != nil {
		if round := review.RoundByLaunchID(plan, goalplan.PurposePlanAudit, signoff.LaunchID); round != nil && round.WorkPhaseID != "" {
			e.WorkPhaseID = round.WorkPhaseID
		} else if active := goalplan.EffectiveActiveWorkPhaseID(plan); active != nil {
			e.WorkPhaseID = *active
		}
	}
	return e
}

// drainEntry judges one kept sign-off against the binding as it is now and publishes it through observe. retry is true for an entry that
// was valid but could not be written, which stays.
func (o reviewObserver) drainEntry(plan *goalplan.Goalplan, st state.State, sessionID string, e reviewInboxEntry) (next *goalplan.Goalplan, retry bool) {
	launch := e.LaunchID
	ignore := func(reason string) {
		o.note(goalplan.EventReviewSignoffIgnored, string(e.Verdict)+" sign-off was not recorded: "+reason, nil, &launch)
	}
	if e.SessionID != sessionID || e.Slug != st.Slug {
		ignore("the sign-off was kept for another session or plan")
		return plan, false
	}
	return o.observe(plan, st, sessionID, e)
}

// ReviewObserverDrain is the outcome of a drain: Plan is the plan as it stands afterwards (what the caller judges); Kept counts the
// entries that reached no decision, because a valid verdict could not be written or because the inbox could not be read, and the
// caller must not move the session on past them; Wrote says the drain made a durable change (a verdict, a ledger row, a removed
// entry), so a caller that answers cancellation as "nothing was written" must not do so after it.
type ReviewObserverDrain struct {
	Plan  *goalplan.Goalplan
	Kept  int
	Wrote bool
}

// drainInbox runs inside the goalplan write lock, with the session lock held by the caller. It applies the entries of r, oldest
// first, then extra (the sign-off in hand, which arrived last); every entry that reached a decision is removed from the inbox. A
// valid verdict whose plan write failed before the plan was published ends the drain: it and every entry after it stay unjudged for
// the next drain, so no later child's sign-off is recorded ahead of an earlier one. An inbox with anything unread (r.blocked) is
// applied to nothing for the same reason. beforeWrite, when set, runs once before the drain's first durable change; an error stops
// the drain with nothing written. Entries held in memory only among the unapplied are lost, with one row each.
func (o reviewObserver) drainInbox(r reviewInboxRead, plan *goalplan.Goalplan, st state.State, sessionID string, extra []reviewInboxItem, beforeWrite func() error) (ReviewObserverDrain, error) {
	pending := append(append([]reviewInboxItem{}, r.items...), extra...)
	out := ReviewObserverDrain{Plan: plan}
	if len(r.corrupt) == 0 && len(r.blocked) == 0 && len(pending) == 0 {
		return out, nil
	}
	if beforeWrite != nil {
		if err := beforeWrite(); err != nil {
			return out, err
		}
	}
	out.Wrote = true
	o.dropCorrupt(r.dir, r.corrupt)
	if len(r.blocked) > 0 {
		o.note(reviewObserverInboxFailed, reviewObserverBounded(r.blocked[0]+"; the kept sign-offs are not applied until it can be read"), nil, nil)
		for _, item := range pending {
			if item.name == "" {
				launch := item.entry.LaunchID
				o.note(reviewObserverWriteFailed, string(item.entry.Verdict)+" sign-off was not judged: the review inbox could not be read", nil, &launch)
			}
		}
		out.Kept = len(pending) + len(r.blocked)
		return out, nil
	}
	for i, item := range pending {
		var retry bool
		out.Plan, retry = o.drainEntry(out.Plan, st, sessionID, item.entry)
		if retry {
			for _, rest := range pending[i+1:] {
				if rest.name == "" {
					launch := rest.entry.LaunchID
					o.note(reviewObserverWriteFailed, string(rest.entry.Verdict)+" sign-off was not judged: an earlier verdict could not be written", nil, &launch)
				}
			}
			out.Kept = len(pending) - i
			return out, nil
		}
		if item.name != "" {
			reviewObserverInboxRemove(r.dir, item.name)
		}
	}
	return out, nil
}

// DrainReviewObserverInboxInLock applies the sign-offs the review observer kept for sessionID to the audit round they name, for a
// caller that holds the session lock and the goalplan write lock (the A>B publication does): it applies them to plan, which the
// caller read inside its lock, writes the plan the same way a live sign-off does, and returns the plan as it stands afterwards, which
// the caller judges. It takes no lock. Kept counts the entries that reached no decision (see ReviewObserverDrain): they stay for the
// next drain, and the caller must not move the session on past them. beforeWrite, when set, runs once before the first durable
// change and a non-nil error from it ends the drain with nothing written and is returned, so a cancelled caller writes nothing.
func DrainReviewObserverInboxInLock(cwd, sessionID string, plan *goalplan.Goalplan, beforeWrite func() error) (out ReviewObserverDrain, err error) {
	out = ReviewObserverDrain{Plan: plan}
	defer func() {
		if recover() != nil {
			out = ReviewObserverDrain{Plan: plan, Kept: 1, Wrote: out.Wrote}
			err = nil
		}
	}()
	st := state.ReadState(cwd, sessionID)
	if st.Slug == "" || plan == nil {
		return out, nil
	}
	r := reviewObserverInboxReadAll(cwd, sessionID)
	if r.dir != nil {
		defer r.dir.Close()
	}
	return reviewObserver{cwd: cwd, slug: st.Slug}.drainInbox(r, plan, st, sessionID, nil, beforeWrite)
}

// dropCorrupt removes entries of dir that carry the observer's own name but were read and hold no entry, with one bounded row each:
// they can never be judged. A file with any other name is not the observer's and is never listed here.
func (o reviewObserver) dropCorrupt(dir *os.File, names []string) {
	for _, name := range names {
		o.note(goalplan.EventReviewSignoffIgnored, reviewObserverBounded("an unreadable review inbox entry was removed: "+name), nil, nil)
		reviewObserverInboxRemove(dir, name)
	}
}
