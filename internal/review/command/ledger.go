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

// A record is one line of ledger.jsonl. Event is started (counts toward the daily cap), finished (the patch-id is reviewed from then on), unavailable (a review that could not run at all for a reason of the
// account or the configuration: the patch-id is still open for one more attempt, see standing), failed (history only) or refused (the run rules stopped it).
type record struct {
	Time     string `json:"time"`
	Event    string `json:"event"`
	PatchID  string `json:"patchId"`
	Base     string `json:"base"`
	Head     string `json:"head"`
	Issue    string `json:"issue"`
	Reason   string `json:"reason,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Status   string `json:"status,omitempty"`
}

var errBusy = errors.New("another review holds the run lock")

// ledger is the state directory: ledger.jsonl and run.lock. Reads need no lock; appends are made only while the run lock is held.
type ledger struct {
	dir  string
	now  func() time.Time
	good int64 // the length of the complete lines read last; a torn tail beyond it is cut off by the next append
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

// runsOn counts the reviews that started on the UTC day (2006-01-02).
func runsOn(recs []record, day string) (n int) {
	for _, r := range recs {
		if r.Event == "started" && strings.HasPrefix(r.Time, day) {
			n++
		}
	}
	return n
}

// standing is where one patch-id stands: a finished record closes it, an unavailable one leaves it open for one more attempt, which is spent as soon as a started record follows it.
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
		case r.Event == "started" && s.unavailable != nil:
			s.retried = true
		}
	}
	return s
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
