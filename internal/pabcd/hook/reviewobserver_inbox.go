package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
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
	// verdict that was valid but whose plan write failed, which stays in the inbox for the next drain.
	reviewObserverInboxFailed goalplan.GoalplanLedgerEvent = "review_signoff_inbox_failed"
	reviewObserverWriteFailed goalplan.GoalplanLedgerEvent = "review_signoff_write_failed"
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

// reviewInboxItem is an entry with the file it came from; an empty path is an entry held in memory only.
type reviewInboxItem struct {
	entry reviewInboxEntry
	path  string
}

func reviewObserverInboxPath(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, reviewObserverInboxDir, state.SanitizeKey(sessionID))
}

// reviewObserverInboxName is the entry's identity: one child's sign-off for one launch of one session and plan is one file, whatever
// the number of times it is delivered or kept.
func reviewObserverInboxName(e reviewInboxEntry) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{e.SessionID, e.Slug, e.LaunchID, e.AgentID}, "\x00")))
	return hex.EncodeToString(sum[:16]) + ".json"
}

// reviewObserverInboxPut keeps e. The file is complete before it is visible (a hard link of a synced temporary file), so a drain never
// reads half an entry, and an entry that exists is kept as the first one wrote it. A full inbox is an error, not a deletion.
func reviewObserverInboxPut(cwd string, e reviewInboxEntry) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	dir := reviewObserverInboxPath(cwd, e.SessionID)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	final := filepath.Join(dir, reviewObserverInboxName(e))
	if _, err := os.Lstat(final); err == nil {
		return nil
	}
	if names, _ := reviewObserverInboxNames(dir); len(names) >= reviewObserverInboxMax {
		return errors.New("the review inbox is full")
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(append(body, '\n'))
	serr := tmp.Sync()
	if err := errors.Join(werr, serr, tmp.Close()); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), final); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func reviewObserverInboxNames(dir string) ([]string, error) {
	infos, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, info := range infos {
		if info.Type().IsRegular() && strings.HasSuffix(info.Name(), ".json") {
			names = append(names, info.Name())
		}
	}
	return names, nil
}

// reviewObserverInboxRead returns the session's kept entries, oldest first, at most reviewObserverInboxMax. A file that cannot be read
// or parsed comes back with ok false so the drain can say so and remove it.
func reviewObserverInboxRead(cwd, sessionID string) (items []reviewInboxItem, unreadable []string) {
	dir := reviewObserverInboxPath(cwd, sessionID)
	names, err := reviewObserverInboxNames(dir)
	if err != nil {
		return nil, nil
	}
	sort.Strings(names)
	if len(names) > reviewObserverInboxMax {
		names = names[:reviewObserverInboxMax]
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		var e reviewInboxEntry
		if err != nil || len(raw) > reviewObserverInboxMaxSize || json.Unmarshal(raw, &e) != nil || e.Version != 1 || e.LaunchID == "" || e.AgentID == "" {
			unreadable = append(unreadable, path)
			continue
		}
		items = append(items, reviewInboxItem{entry: e, path: path})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].entry.ReceivedAt < items[j].entry.ReceivedAt })
	return items, unreadable
}

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
		ReceivedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
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
// first; every one that reached a decision is removed.
func (o reviewObserver) drainInbox(plan *goalplan.Goalplan, st state.State, sessionID string, pending []reviewInboxItem) {
	for _, item := range pending {
		var retry bool
		plan, retry = o.drainEntry(plan, st, sessionID, item.entry)
		if !retry && item.path != "" {
			_ = os.Remove(item.path)
		}
	}
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
	pending, unreadable := reviewObserverInboxRead(cwd, sessionID)
	if len(pending) == 0 && len(unreadable) == 0 {
		return
	}
	observer := reviewObserver{cwd: cwd, slug: st.Slug}
	_, _ = goalplan.WithGoalplanWriteLock(cwd, st.Slug, func(plan *goalplan.Goalplan) (string, error) {
		observer.dropUnreadable(unreadable)
		observer.drainInbox(plan, st, sessionID, pending)
		return "", nil
	}, nil)
}

// dropUnreadable removes inbox files that are not one of ours, with one bounded row each: they can never be judged.
func (o reviewObserver) dropUnreadable(paths []string) {
	for _, path := range paths {
		o.note(goalplan.EventReviewSignoffIgnored, reviewObserverBounded("an unreadable review inbox entry was removed: "+filepath.Base(path)), nil, nil)
		_ = os.Remove(path)
	}
}
