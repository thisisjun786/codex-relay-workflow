package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// A record is one line of ledger.jsonl. Event is started (counts toward the daily cap), finished (the patch-id is reviewed from then on), unavailable (a review that could not run at all for a reason of
// the account or the configuration or because the runner failed: the patch-id is still open for one more attempt, see standing), lock_wait (the attempt's agy was never started because the host-wide lock
// was not free: the attempt counts toward nothing and closes nothing), keep_failed (the attempt ran and its result could not be kept: nothing is closed and the one more attempt is not spent, but it counts toward the daily cap; a retry whose keep_failed line cannot be appended keeps its started line, which counts toward the cap and spends the attempt, see unkeptAttempt), failed (history only) or refused (the run rules stopped it). The line that ends a run which did not end complete -- a finished line
// whose status is partial or unavailable, or the unavailable line that opens the one more attempt -- also records the failure it knows in Reason and AgyCalled.
type record struct {
	Time      string `json:"time"`
	Event     string `json:"event"`
	PatchID   string `json:"patchId"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	Issue     string `json:"issue"`
	Reason    string `json:"reason,omitempty"`
	Artifact  string `json:"artifact,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Status    string `json:"status,omitempty"`
	AgyCalled *bool  `json:"agyCalled,omitempty"` // of a run that did not end complete: whether agy was actually called, when that can be told -- true when a review call ended normal, ended invalid or failed with a reason agy reported itself, false when every review call is a lock wait or agy that never started, absent for a crash or a runner error; a complete run's line carries neither this nor a reason
}

// The ledger event a lock wait writes: the attempt was recorded as started and then agy was never started because the host-wide lock was not free. It closes nothing, counts toward nothing, and is
// followed by the next attempt of the same patch whenever one is made.
const eventLockWait = "lock_wait"

// voided reports whether the started record at i was an attempt whose agy was never started, which the record that follows it (a lock_wait) says. Appends are made only under the run lock, so the record
// after a started is the one that ended that attempt; the next attempt's started is not a lock_wait, so a bare started (a process that died) is not voided.
func voided(recs []record, i int) bool {
	return i+1 < len(recs) && recs[i+1].Event == eventLockWait
}

// The ledger event a result that could not be kept writes: the attempt ran, its result could not be kept in the state directory, and nothing was recorded as finished. Like a lock wait it leaves no result,
// so it does not spend the one more attempt an unavailable review left open; unlike a lock wait the attempt was a review and counts toward the daily cap.
const eventKeepFailed = "keep_failed"

// unkept reports whether the started record at i was an attempt whose result could not be kept, which the record that follows it (a keep_failed) says.
func unkept(recs []record, i int) bool {
	return i+1 < len(recs) && recs[i+1].Event == eventKeepFailed
}

var errBusy = errors.New("another review holds the run lock")

// ledger is the state directory: ledger.jsonl and run.lock. Reads need no lock; appends are made only while the run lock is held.
type ledger struct {
	dir         string
	now         func() time.Time
	publishKept func(path string, data []byte) error // writes a kept copy; nil is the production write
	syncDir     func(dir string) error               // fsyncs a directory; nil is crwdir.SyncDir
	appendFault func(r record) error                 // fails the append of r when it returns an error; nil fails none (a test injects a ledger that takes no line)
	good        int64                                // the length of the complete lines read last; a torn tail beyond it is cut off by the next append
}

func (l *ledger) path() string { return filepath.Join(l.dir, "ledger.jsonl") }

// read returns the records. A complete line that is not a record fails closed with its line number; a trailing fragment without a newline (a torn append) is ignored.
func (l *ledger) read() ([]record, error) {
	data, err := os.ReadFile(l.path())
	if errors.Is(err, fs.ErrNotExist) {
		l.good = 0
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	whole := data[:bytes.LastIndexByte(data, '\n')+1]
	l.good = int64(len(whole))
	var recs []record
	for n, line := range strings.Split(strings.TrimSuffix(string(whole), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r record
		err := json.Unmarshal([]byte(line), &r)
		if err == nil && (r.Event == "" || r.PatchID == "") {
			err = errors.New("no event or patchId")
		}
		if err != nil {
			return nil, fmt.Errorf("%s: line %d is not a ledger record (look at it, then remove it): %w", l.path(), n+1, err)
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// runsOn counts the reviews that started on the UTC day (2006-01-02). An attempt whose agy was never started (a lock wait) is not a review and does not count.
func runsOn(recs []record, day string) (n int) {
	for i, r := range recs {
		if r.Event == "started" && strings.HasPrefix(r.Time, day) && !voided(recs, i) {
			n++
		}
	}
	return n
}

// standing is where one patch-id stands: a finished record closes it, an unavailable one leaves it open for one more attempt, which is spent as soon as a started record that is neither a lock wait nor an attempt whose result could not be kept follows it.
type standing struct {
	finished, unavailable *record
	retried               bool
}

func standingOf(recs []record, patchID string) (s standing) {
	for i, r := range recs {
		switch {
		case r.PatchID != patchID:
		case r.Event == "finished" && s.finished == nil:
			s.finished = &recs[i]
		case r.Event == "unavailable":
			s.unavailable, s.retried = &recs[i], false
		case r.Event == "started" && s.unavailable != nil && !voided(recs, i) && !unkept(recs, i):
			s.retried = true
		}
	}
	return s
}

// newestResult is the last record of the patch that carries a result, a finished or an unavailable one: what the summary comment of the patch should show. It is nil before any result.
func newestResult(recs []record, patchID string) (newest *record) {
	for i, r := range recs {
		if r.PatchID == patchID && (r.Event == "finished" || r.Event == "unavailable") {
			newest = &recs[i]
		}
	}
	return newest
}

// closer is the record that answers a repeated request for the patch: its finished record or, when the one more attempt began and left no result, the unavailable record before it.
func (s standing) closer() (record, bool) {
	switch {
	case s.finished != nil:
		return *s.finished, true
	case s.unavailable != nil && s.retried:
		return *s.unavailable, true
	}
	return record{}, false
}

// open reports whether the one more attempt is still to come.
func (s standing) open() bool { return s.finished == nil && s.unavailable != nil && !s.retried }

// dayOf is the UTC day (2006-01-02) of a record's time.
func dayOf(t string) string { return t[:min(len(t), len(time.DateOnly))] }

// append writes r as one line and syncs it, first cutting off a torn tail.
func (l *ledger) append(r record) error {
	if l.appendFault != nil {
		if err := l.appendFault(r); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path(), os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return err
	} else if st.Size() != l.good {
		if err := f.Truncate(l.good); err != nil {
			return err
		}
	}
	if r.Time == "" {
		r.Time = l.now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	l.good += int64(len(line)) + 1
	return f.Sync()
}

// lock takes the run lock, an exclusive flock on run.lock held for the whole review, polled every 50 ms until wait has passed (negative tries once); errBusy if it stayed busy.
func (l *ledger) lock(ctx context.Context, wait time.Duration) (release func(), err error) {
	return l.lockFile(ctx, "run.lock", wait)
}

// postLock takes the post lock the same way: it is held while the summary comment is listed and created or updated, so two posts cannot interleave and make two comments.
func (l *ledger) postLock(ctx context.Context, wait time.Duration) (release func(), err error) {
	return l.lockFile(ctx, "post.lock", wait)
}

func (l *ledger) lockFile(ctx context.Context, name string, wait time.Duration) (release func(), err error) {
	if err = os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(l.dir, name), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	deadline := time.Now().Add(wait)
	for {
		if err = ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("lock %s: %w", name, err)
		}
		if !time.Now().Before(deadline) {
			_ = unix.Close(fd)
			return nil, errBusy
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}
