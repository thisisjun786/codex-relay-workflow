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
// the same session, and the A>B transition, which holds the session lock when it judges the review binding. A drain judges every entry
// again against the binding as it is then, and publishes through the same code as a live sign-off, so an entry for a superseded epoch, a
// session that left A, another work-phase, a child that is not the round's reviewer or a round that already holds a verdict is
// ignored with a ledger row and removed. Draining twice is the same as draining once: an approved round is no longer in_flight.
// Nothing here blocks a child or polls, and only the SubagentStop of a reviewer-shaped child (never a worker or executor, which the
// receipt gate owns) can add an entry.
const (
	reviewObserverInboxDir     = "review-inbox"
	reviewObserverInboxMax     = 64
	reviewObserverInboxMaxSize = 4096
	reviewObserverDetailMax    = 200

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
	ReceivedAt  string           `json:"receivedAt"`
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

// reviewObserverInboxPut keeps e. The file is complete before it is visible (a hard link of a synced temporary file), so a drain never
// reads half an entry, and an entry that exists is kept as the first one wrote it. A full inbox is an error, not a deletion.
func reviewObserverInboxPut(cwd string, e reviewInboxEntry) error {
	dir, err := reviewObserverInboxOpen(cwd, e.SessionID, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	dfd := int(dir.Fd())
	final := reviewObserverInboxName(e)
	var existing unix.Stat_t
	if unix.Fstatat(dfd, final, &existing, unix.AT_SYMLINK_NOFOLLOW) == nil {
		return nil
	}
	if names, _ := reviewObserverInboxNames(dir); len(names) >= reviewObserverInboxMax {
		return errors.New("the review inbox is full")
	}
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

// reviewObserverInboxReadFile reads one entry file of dir: never through a link, never a non-regular file, at most
// reviewObserverInboxMaxSize bytes.
func reviewObserverInboxReadFile(dir *os.File, name string) (reviewInboxEntry, error) {
	var e reviewInboxEntry
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return e, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return e, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, reviewObserverInboxMaxSize+1))
	if err != nil || len(raw) > reviewObserverInboxMaxSize || json.Unmarshal(raw, &e) != nil || e.Version != 1 || e.LaunchID == "" || e.AgentID == "" {
		return e, errors.New("not an inbox entry")
	}
	return e, nil
}

// reviewObserverInboxRead opens the session's inbox and returns it with its kept entries, oldest first, at most
// reviewObserverInboxMax. An entry of ours that cannot be read or parsed comes back by name in unreadable so the drain can say so and
// remove it. A missing or refused inbox is a nil dir and no entries. The caller closes dir.
func reviewObserverInboxRead(cwd, sessionID string) (dir *os.File, items []reviewInboxItem, unreadable []string) {
	dir, err := reviewObserverInboxOpen(cwd, sessionID, false)
	if err != nil {
		return nil, nil, nil
	}
	names, err := reviewObserverInboxNames(dir)
	if err != nil {
		return dir, nil, nil
	}
	sort.Strings(names)
	if len(names) > reviewObserverInboxMax {
		names = names[:reviewObserverInboxMax]
	}
	for _, name := range names {
		e, err := reviewObserverInboxReadFile(dir, name)
		if err != nil {
			unreadable = append(unreadable, name)
			continue
		}
		items = append(items, reviewInboxItem{entry: e, name: name})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].entry.ReceivedAt < items[j].entry.ReceivedAt })
	return dir, items, unreadable
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

// drainInbox runs inside the goalplan write lock, with the session lock held by the caller. pending are the entries to judge, oldest
// first; every one that reached a decision is removed from dir. A valid verdict whose plan write failed before the plan was published
// ends the drain: it and every entry after it stay unjudged for the next drain, so no later child's sign-off is recorded ahead of an
// earlier one. unapplied counts the entries that reached no decision (an entry held in memory only among them is lost, with one row).
func (o reviewObserver) drainInbox(dir *os.File, plan *goalplan.Goalplan, st state.State, sessionID string, pending []reviewInboxItem) (next *goalplan.Goalplan, unapplied int) {
	for i, item := range pending {
		var retry bool
		plan, retry = o.drainEntry(plan, st, sessionID, item.entry)
		if retry {
			for _, rest := range pending[i+1:] {
				if rest.name == "" {
					launch := rest.entry.LaunchID
					o.note(reviewObserverWriteFailed, string(rest.entry.Verdict)+" sign-off was not judged: an earlier verdict could not be written", nil, &launch)
				}
			}
			return plan, len(pending) - i
		}
		if item.name != "" {
			reviewObserverInboxRemove(dir, item.name)
		}
	}
	return plan, 0
}

// DrainReviewObserverInbox applies the sign-offs the review observer kept for sessionID to the audit round they name. The caller holds
// the session lock (the A>B transition does); the goalplan lock is taken here, after it. A busy goalplan lock leaves the entries for the
// next drain, and nothing is ever blocked or reported to the caller: the answer is the round's state in the plan, read afterwards.
func DrainReviewObserverInbox(cwd, sessionID string) {
	defer func() { _ = recover() }()
	st := state.ReadState(cwd, sessionID)
	if st.Slug == "" {
		return
	}
	dir, pending, unreadable := reviewObserverInboxRead(cwd, sessionID)
	if dir == nil {
		return
	}
	defer dir.Close()
	if len(pending) == 0 && len(unreadable) == 0 {
		return
	}
	observer := reviewObserver{cwd: cwd, slug: st.Slug}
	_, _ = goalplan.WithGoalplanWriteLock(cwd, st.Slug, func(plan *goalplan.Goalplan) (string, error) {
		observer.dropUnreadable(dir, unreadable)
		_, _ = observer.drainInbox(dir, plan, st, sessionID, pending)
		return "", nil
	}, nil)
}

// DrainReviewObserverInboxInLock is DrainReviewObserverInbox for a caller that already holds the session lock and the goalplan write
// lock (the A>B publication does): it applies the kept sign-offs to plan, which the caller read inside its lock, writes the plan the
// same way a live sign-off does, and returns the plan as it stands afterwards, which the caller judges. It takes no lock and never
// fails. kept counts the entries a valid verdict of which could not be written: they stay for the next drain, and the caller must not
// move the session on past a verdict it could not record.
func DrainReviewObserverInboxInLock(cwd, sessionID string, plan *goalplan.Goalplan) (next *goalplan.Goalplan, kept int) {
	next = plan
	defer func() {
		if recover() != nil {
			next, kept = plan, 0
		}
	}()
	st := state.ReadState(cwd, sessionID)
	if st.Slug == "" || plan == nil {
		return plan, 0
	}
	dir, pending, unreadable := reviewObserverInboxRead(cwd, sessionID)
	if dir == nil {
		return plan, 0
	}
	defer dir.Close()
	if len(pending) == 0 && len(unreadable) == 0 {
		return plan, 0
	}
	observer := reviewObserver{cwd: cwd, slug: st.Slug}
	observer.dropUnreadable(dir, unreadable)
	return observer.drainInbox(dir, plan, st, sessionID, pending)
}

// dropUnreadable removes entries of dir that carry the observer's own name but cannot be read as an entry, with one bounded row
// each: they can never be judged. A file with any other name is not the observer's and is never listed here.
func (o reviewObserver) dropUnreadable(dir *os.File, names []string) {
	for _, name := range names {
		o.note(goalplan.EventReviewSignoffIgnored, reviewObserverBounded("an unreadable review inbox entry was removed: "+name), nil, nil)
		reviewObserverInboxRemove(dir, name)
	}
}
