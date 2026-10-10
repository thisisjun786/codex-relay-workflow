package state

// The transition-ledger outbox (CRW-1097). The oracle publishes the session state and appends the transition row afterwards with
// nothing between the two, so a writer that dies, or an append that fails, after the publication leaves an applied phase change that
// no ledger row records and that nothing ever repairs: the phase has moved, so the same request is refused. The port keeps the
// state-first order (a row must never describe a transition that did not happen) and adds an outbox in front of it: the row is
// prepared as a pending event of the session, durably, before the state is published, inside the same session lock, and the append
// drains that event. A pending event that outlives its writer is drained by the next writer of the session that holds the session
// lock (the next hook, the next orchestrate command), which first decides from the state it finds whether the transition the event
// records was published: a published one has its row appended, exactly once, and one that was not is dropped.
//
// The rows keep the oracle's bytes: the event's identity is the outbox record (its id, the row's exact bytes and the ledger size
// at the time it was prepared), so no key is added to a ledger row. A row is already recorded when that exact line stands in the
// ledger at or after the recorded size, which is where an append made after the event was prepared can only be. Two events that
// spell the same line are told apart by order: a drain searches for an event's line only after the end of the row of the event
// before it (the end offset is carried into the next event's file before the earlier one is removed), so each of them records
// its own line. A row counts as recorded only once the ledger file (and its directory) has been fsynced, so an event is removed
// only when its row survives a power loss.
//
// Layout: one file per event in <state file>.ledger-outbox/, named by a zero-padded sequence and the event id so a listing sorts in
// the order the events were prepared. The directory is per session, so its own session lock serialises every writer of it, and it
// is removed again once it is empty: a session with nothing pending leaves no trace of the outbox. A file is a header line (JSON)
// and the row's line.

import (
	"bufio"
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
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// ledgerOutboxSuffix names a session's outbox directory beside its state file.
const ledgerOutboxSuffix = ".ledger-outbox"

// ledgerEventSuffix names one pending event in that directory.
const ledgerEventSuffix = ".event"

// LedgerEvent is one transition row prepared before the state publication it records. The witness fields (the phases and the
// digests of the state before and after, the updatedAt the state carried before) let a later drain tell a published transition
// from one whose writer stopped before the publication. Followup is an opaque payload for work that belongs to the same
// transition and lives outside the session (the P>A plan-audit cleanup of CRW-1100): the drain hands it to the caller's
// followup function, and the event is finished only when that succeeds too.
type LedgerEvent struct {
	ID           string `json:"eventId"`
	SessionID    string `json:"sessionId"`
	Seq          int64  `json:"seq"`
	PrePhase     Phase  `json:"prePhase"`
	PostPhase    Phase  `json:"postPhase"`
	PreDigest    string `json:"preDigest"`
	PostDigest   string `json:"postDigest"`
	PreUpdatedAt string `json:"preUpdatedAt"`
	LedgerOffset int64  `json:"ledgerOffset"`
	Published    bool   `json:"published,omitempty"`
	RowRecorded  bool   `json:"rowRecorded,omitempty"`
	// RowEnd is the ledger offset just after the row once it is recorded, so the events after it never take that row (or an
	// identical one of an earlier event) for their own.
	RowEnd   int64           `json:"rowEnd,omitempty"`
	Followup json.RawMessage `json:"followup,omitempty"`
	// Line is the row exactly as it is appended, without the line feed; nil for an event that carries no row.
	Line []byte `json:"-"`
}

// NewLedgerEvent prepares the event of the transition from pre to post whose row is row (nil for none). It mints the event id
// and reads the ledger's size; it writes nothing. The row is fixed here, byte for byte, so a retry appends exactly what the
// first attempt would have.
func NewLedgerEvent(cwd string, pre, post State, row *LedgerEntry, followup json.RawMessage) (LedgerEvent, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return LedgerEvent{}, err
	}
	ev := LedgerEvent{
		ID: "ev-" + hex.EncodeToString(raw[:]), SessionID: pre.SessionID, Seq: time.Now().UnixNano(),
		PrePhase: pre.Phase, PostPhase: post.Phase, PreDigest: stateDigest(pre), PostDigest: stateDigest(post),
		PreUpdatedAt: pre.UpdatedAt, Followup: followup,
	}
	if row != nil {
		line, err := object(row.members())
		if err != nil {
			return LedgerEvent{}, err
		}
		ev.Line = line
	}
	info, err := os.Stat(filepath.Join(cwd, crwdir.DirName, LedgerFile))
	switch {
	case err == nil:
		ev.LedgerOffset = info.Size()
	case !errors.Is(err, fs.ErrNotExist):
		return LedgerEvent{}, err
	}
	return ev, nil
}

// stateDigest is a digest of s as WriteState would publish it, without updatedAt, which the write stamps from its clock. "" when s
// cannot be encoded, which no witness then matches.
func stateDigest(s State) string {
	s.UpdatedAt, s.Interview = "", interview.Normalize(s.Interview)
	body, err := Encode(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ledgerOutboxDir is the session's outbox directory.
func ledgerOutboxDir(cwd, sessionID string) string {
	return StatePath(cwd, sessionID) + ledgerOutboxSuffix
}

// ledgerEventPath is the file of ev in its session's outbox.
func ledgerEventPath(cwd string, ev LedgerEvent) string {
	return filepath.Join(ledgerOutboxDir(cwd, ev.SessionID), fmt.Sprintf("%020d-%s%s", ev.Seq, ev.ID, ledgerEventSuffix))
}

// encodeLedgerEvent is the file's text: the header line and the row's line.
func encodeLedgerEvent(ev LedgerEvent) ([]byte, error) {
	header, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	out := append(header, '\n')
	return append(out, ev.Line...), nil
}

// writeLedgerEvent publishes ev's file whole: a temp file beside it, fsynced and renamed over it, then the directory fsynced, so a
// crash leaves the previous file or the new one, never a torn one.
func writeLedgerEvent(cwd string, ev LedgerEvent) error {
	if err := makeSessionsDir(cwd); err != nil {
		return err
	}
	dir := ledgerOutboxDir(cwd, ev.SessionID)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	body, err := encodeLedgerEvent(ev)
	if err != nil {
		return err
	}
	final := ledgerEventPath(cwd, ev)
	tmp := tempPath(final)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = removeFile(tmp)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// PrepareLedgerEvent records ev as pending, durably, before the state publication it describes. The caller holds the session
// lock. A failure leaves nothing pending, and the caller must not publish.
func PrepareLedgerEvent(cwd string, ev LedgerEvent) error {
	if err := writeLedgerEvent(cwd, ev); err != nil {
		_ = removeFile(ledgerEventPath(cwd, ev))
		removeEmptyOutbox(cwd, ev.SessionID)
		return err
	}
	return nil
}

// AbortLedgerEvent drops ev after a publication that did not happen. A removal that fails leaves the event for the next drain, which
// finds the state unchanged and drops it then.
func AbortLedgerEvent(cwd string, ev LedgerEvent) error {
	err := removeFile(ledgerEventPath(cwd, ev))
	removeEmptyOutbox(cwd, ev.SessionID)
	return err
}

// removeEmptyOutbox removes the session's outbox directory when nothing is left in it. Best effort: a directory left behind is
// harmless, and the next drain tries again.
func removeEmptyOutbox(cwd, sessionID string) {
	dir := ledgerOutboxDir(cwd, sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}

// removeOrphanEventTemps removes the temp files a writer killed while it staged an event left in the session's
// outbox. Only the holder of the session lock writes there, and the drain's caller holds it, so every temp file
// found is an orphan. Best effort.
func removeOrphanEventTemps(cwd, sessionID string) {
	dir := ledgerOutboxDir(cwd, sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".tmp") && strings.Contains(name, ledgerEventSuffix+".") && entry.Type().IsRegular() {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// PendingLedgerEvents lists the session's pending events in the order they were prepared. A file that cannot be read or decoded
// is reported in damaged by name and left alone: it is never guessed at.
func PendingLedgerEvents(cwd, sessionID string) (events []LedgerEvent, damaged []string, err error) {
	dir := ledgerOutboxDir(cwd, sessionID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ledgerEventSuffix) && entry.Type().IsRegular() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		header, line, _ := bytes.Cut(raw, []byte{'\n'})
		var ev LedgerEvent
		if readErr != nil || json.Unmarshal(header, &ev) != nil || ev.ID == "" || ev.SessionID != sessionID ||
			ledgerEventPath(cwd, ev) != filepath.Join(dir, name) {
			damaged = append(damaged, name)
			continue
		}
		if len(line) > 0 {
			ev.Line = line
		}
		events = append(events, ev)
	}
	return events, damaged, nil
}

// LedgerDrainOptions is what a drain needs from its caller. Published names the events the caller itself has just published, so
// they are not judged from the state at all. Followup runs the work an event's Followup payload describes, once its row is
// recorded; nil leaves an event that carries one pending.
type LedgerDrainOptions struct {
	Published map[string]bool
	Followup  func(ev LedgerEvent) error
	// FollowupDone names the events whose followup the caller has just completed itself.
	FollowupDone map[string]bool
}

// LedgerDrainReport is what a drain did: the rows it appended, the events it dropped as never published, and the events still
// pending afterwards with the first reason one could not be finished (nil when none is left).
type LedgerDrainReport struct {
	Appended, Dropped int
	Pending           []LedgerEvent
	Damaged           []string
	Err               error
}

// DrainLedgerOutbox finishes the session's pending events, oldest first. The caller holds the session lock. For each event the
// drain decides whether its transition was published (the event says so, the caller says so, or the state found now is the one
// the transition produced), appends its row unless that exact line is already in the ledger after the size recorded when the
// event was prepared, runs its followup, and removes it. An event that was not published is removed without a row. An append that
// fails stops the drain, so a later row is never appended ahead of an earlier one; a followup that fails leaves its event pending
// with its row recorded and the drain goes on.
func DrainLedgerOutbox(cwd, sessionID string, o LedgerDrainOptions) LedgerDrainReport {
	report := LedgerDrainReport{}
	removeOrphanEventTemps(cwd, sessionID)
	events, damaged, err := PendingLedgerEvents(cwd, sessionID)
	report.Damaged = damaged
	if err != nil {
		report.Err = err
		return report
	}
	if len(events) == 0 {
		removeEmptyOutbox(cwd, sessionID)
		return report
	}
	current, unreadable := ReadStateStrict(cwd, sessionID)
	if unreadable {
		report.Pending, report.Err = events, errors.New("the session state cannot be read, so its pending ledger rows cannot be judged")
		return report
	}
	currentDigest := stateDigest(current)
	floor := int64(0)
	for i, ev := range events {
		floor = max(floor, ev.RowEnd)
		if !ev.RowRecorded && len(ev.Line) > 0 {
			if !(ev.Published || o.Published[ev.ID] || ledgerEventPublished(ev, current, currentDigest)) {
				if err := AbortLedgerEvent(cwd, ev); err != nil {
					report.Pending = append(report.Pending, ev)
					report.Err = cmpErr(report.Err, err)
					continue
				}
				report.Dropped++
				continue
			}
			from := ev.LedgerOffset
			end, present, err := ledgerFindLine(cwd, from, floor, ev.Line)
			if err == nil && !present {
				if err = AppendLedgerLine(cwd, ev.Line); err == nil {
					report.Appended++
					if end, present, err = ledgerFindLine(cwd, from, floor, ev.Line); err == nil && !present {
						err = errors.New("the appended ledger row cannot be found again")
					}
				}
			}
			if err == nil {
				err = syncLedger(cwd)
			}
			if err != nil {
				// The transition is known to be published now; record that, so a later drain does not judge it again
				// from a state that may have moved on, and stop: the rows after it wait for it.
				ev.Published, ev.LedgerOffset = true, max(from, floor)
				_ = writeLedgerEvent(cwd, ev)
				at := len(report.Pending)
				report.Pending = append(report.Pending, events[i:]...)
				report.Pending[at] = ev
				report.Err = cmpErr(report.Err, err)
				return report
			}
			ev.RowRecorded, ev.RowEnd = true, end
			floor = end
		} else if !ev.RowRecorded && !(ev.Published || o.Published[ev.ID] || ledgerEventPublished(ev, current, currentDigest)) {
			if err := AbortLedgerEvent(cwd, ev); err != nil {
				report.Pending = append(report.Pending, ev)
				report.Err = cmpErr(report.Err, err)
				continue
			}
			report.Dropped++
			continue
		}
		ev.Published, ev.RowRecorded = true, true
		if len(ev.Followup) > 0 && !o.FollowupDone[ev.ID] {
			err := errors.New("no followup handler")
			if o.Followup != nil {
				err = o.Followup(ev)
			}
			if err != nil {
				_ = writeLedgerEvent(cwd, ev)
				report.Pending = append(report.Pending, ev)
				report.Err = cmpErr(report.Err, err)
				continue
			}
		}
		if i+1 < len(events) && events[i+1].LedgerOffset < floor && !events[i+1].RowRecorded {
			// The next event must not take this row for its own: carry the end of it into the next file before this one goes.
			events[i+1].LedgerOffset = floor
			if err := writeLedgerEvent(cwd, events[i+1]); err != nil {
				_ = writeLedgerEvent(cwd, ev)
				report.Pending = append(report.Pending, ev)
				report.Err = cmpErr(report.Err, err)
				continue
			}
		}
		if err := AbortLedgerEvent(cwd, ev); err != nil {
			// The row is in the ledger; a later drain finds it there and only removes the file.
			report.Err = cmpErr(report.Err, err)
		}
	}
	return report
}

// cmpErr keeps the first error.
func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// ledgerEventPublished judges, from the state found now, whether ev's transition was published. The digest of the whole state
// decides when it matches either side; the phase decides next, for a transition that moves it; and a state whose updatedAt is still
// the one it carried before was not written since.
func ledgerEventPublished(ev LedgerEvent, current State, currentDigest string) bool {
	if ev.PostDigest != ev.PreDigest && currentDigest != "" {
		switch currentDigest {
		case ev.PostDigest:
			return true
		case ev.PreDigest:
			return false
		}
	}
	if ev.PrePhase != ev.PostPhase {
		switch current.Phase {
		case ev.PostPhase:
			return true
		case ev.PrePhase:
			return false
		}
	}
	return current.UpdatedAt != ev.PreUpdatedAt
}

// ledgerFindLine reports whether the exact line stands in the transition ledger at or after offset, and the offset just after the
// first such line. A ledger shorter than offset was replaced or cut since, so the whole file is searched; floor, the end of the
// row of an earlier event of the same drain, is then still honoured, since it was read from the ledger as it is now. Lines are read one at a
// time; nothing but the current line is held.
func ledgerFindLine(cwd string, offset, floor int64, line []byte) (end int64, found bool, err error) {
	f, err := os.Open(filepath.Join(cwd, crwdir.DirName, LedgerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	if offset > info.Size() {
		offset = 0
	}
	offset = max(offset, floor)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	r := bufio.NewReader(f)
	pos := offset
	for {
		got, err := r.ReadBytes('\n')
		pos += int64(len(got))
		if bytes.Equal(bytes.TrimSuffix(got, []byte{'\n'}), line) {
			return pos, true, nil
		}
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
	}
}

// syncLedgerFile is the fsync of a path; a variable so a test can fail it.
var syncLedgerFile = func(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// syncLedger makes the ledger's rows and the entry of the ledger file durable: the file, then its directory.
func syncLedger(cwd string) error {
	dir := filepath.Join(cwd, crwdir.DirName)
	if err := syncLedgerFile(filepath.Join(dir, LedgerFile)); err != nil {
		return err
	}
	return syncLedgerFile(dir)
}
