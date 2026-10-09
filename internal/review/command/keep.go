package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A result is kept in the state directory, in results/<sha256>.json, before the ledger record that names it is appended, and the record is appended only when the copy is kept: written, renamed into
// place and the directory entry fsynced (crwdir.PublishDurable; the state directory, which holds the directory of results, is synced as well on every keep), so a record never names a copy that a power
// failure can take away, and a copy that cannot be kept leaves the patch open for another run. The record is what closes the patch, and the two output files are written after it, so a failed write
// leaves a patch that is reviewed and a result that is kept; the kept copy lets the next call write the files again without a model call.
func (l *ledger) keptPath(sha string) string { return filepath.Join(l.dir, "results", sha+".json") }

func (l *ledger) keep(sha string, data []byte) error {
	if err := os.MkdirAll(filepath.Join(l.dir, "results"), 0o700); err != nil {
		return err
	}
	// The entry of results/ in the state directory is made durable by every call that keeps a result, not only by the one that created it: a call whose sync failed, or that ended right after the directory was
	// made, leaves results/ in place, and its existence is no proof that its entry survives a power failure.
	if err := l.sync(l.dir); err != nil {
		return fmt.Errorf("sync %s: %w", l.dir, err)
	}
	publish := l.publishKept
	if publish == nil {
		publish = crwdir.PublishDurable
	}
	return publish(l.keptPath(sha), data)
}

// unkeptAttempt ends the attempt whose result could not be kept (keepErr) and returns the error that says so. The attempt is ended by its keep_failed line, which leaves the patch open; when that line
// cannot be appended either, the attempt's started line stands alone, which closes nothing on a first attempt but spends the one more attempt of an unavailable review (retry), as a killed process does. That
// line is then taken back out of the ledger, cutting it to sizeBefore, the length it had before the line: the attempt leaves no trace, so it is not counted toward the daily cap, and the one more attempt is
// still to come. When the ledger takes no truncation either, the attempt is spent as the ledger stands, and the error gives the line that leaves the attempt to come, to be appended by hand once the state
// directory takes it, instead of telling the operator to run the same command again.
func (l *ledger) unkeptAttempt(keepErr error, started, unkept record, retry bool, sizeBefore int64) error {
	const ran = "the review ran but its result could not be kept in the state directory"
	unkept.Reason, unkept.Time = keepErr.Error(), l.now().UTC().Format(time.RFC3339)
	appendErr := l.append(unkept)
	switch {
	case appendErr == nil:
		return fmt.Errorf("%s; nothing is recorded as finished and no file is written, so run the same command again: %w", ran, keepErr)
	case !retry:
		return fmt.Errorf("%s: %w; nothing is recorded as finished and no file is written, and the keep_failed line could not be appended either (%v), which leaves the patch open as well: run the same command again once the state directory takes writes", ran, keepErr, appendErr)
	}
	withdrawErr := l.withdraw(sizeBefore)
	if withdrawErr == nil {
		return fmt.Errorf("%s: %w; nothing is recorded as finished and no file is written. The keep_failed line could not be appended (%v), so this attempt's started line was taken back out of %s: the one more attempt of the unavailable review is still to come and this attempt is not counted toward the daily cap. Run the same command again once the state directory takes writes", ran, keepErr, appendErr, l.path())
	}
	startedLine, _ := json.Marshal(started)
	unkeptLine, _ := json.Marshal(unkept)
	return fmt.Errorf("%s: %w; nothing is recorded as finished and no file is written. This attempt was the one more attempt of the unavailable review, and neither its keep_failed line could be appended (%v) nor its started line taken back out of %s (%v): as the ledger stands the attempt is spent, and the same command answers already_reviewed with the unavailable result. To leave the attempt to come, once the state directory takes writes, check that the last whole line of the ledger is this attempt's started line\n%s\nremove any incomplete line after it, and append this line on a line of its own\n%s\nthen run the same command again",
		ran, keepErr, appendErr, l.path(), withdrawErr, startedLine, unkeptLine)
}

// withdraw cuts the ledger back to size bytes, the length it had before the started line of the attempt being withdrawn, and syncs it. It is called only under the run lock, by the attempt itself, so whatever
// lies beyond size was appended by that attempt.
func (l *ledger) withdraw(size int64) error {
	f, err := os.OpenFile(l.path(), os.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	l.good = size
	return nil
}

func (l *ledger) sync(dir string) error {
	if l.syncDir != nil {
		return l.syncDir(dir)
	}
	return crwdir.SyncDir(dir)
}

func checksumLine(r record) string { return r.SHA256 + "  " + filepath.Base(r.Artifact) + "\n" }

// ownerOf is the record that owns path in the ledger: the last finished or unavailable record, of any patch, that names path as its artifact. The file name is the head's, so the same head reviewed against another
// base lands on the same name once the first output is gone, and what the path should hold is then the result the ledger assigned to it last, whichever patch asks.
func ownerOf(recs []record, path string) (owner *record) {
	for i, r := range recs {
		if (r.Event == "finished" || r.Event == "unavailable") && r.Artifact == path && r.SHA256 != "" {
			owner = &recs[i]
		}
	}
	return owner
}

// restoreWanted reports which files of the artifact the owner record r recorded a restore would write: the artifact when nothing is at its path, or when what is there is exactly the bytes of a result the
// ledger assigned to the path before r (an unavailable attempt that a retry replaced, or the other patch's result that r replaced) and r's files were not written; the checksum file when it is missing or says
// something else and the artifact it sits beside has the recorded bytes (or is written now), so that a foreign file never gets a checksum that is not its own. Any other file at those paths is not ours to replace.
func restoreWanted(recs []record, r record) (artifact, checksum bool) {
	if _, err := os.Lstat(r.Artifact); errors.Is(err, fs.ErrNotExist) {
		artifact = true
	} else {
		for i, earlier := range recs {
			if earlier.Artifact == r.Artifact && earlier.SHA256 != "" && earlier.SHA256 != r.SHA256 && (earlier.Event == "finished" || earlier.Event == "unavailable") && replaces(&recs[i], r.Artifact) {
				artifact = true
				break
			}
		}
	}
	got, err := os.ReadFile(r.Artifact + ".sha256")
	checksum = err != nil || string(got) != checksumLine(r)
	if checksum && !artifact {
		checksum = replaces(&r, r.Artifact) // replaces: the file at the path has the bytes r recorded
	}
	return artifact, checksum
}

// restore writes the files of the artifact that restoreWanted names, from the copy kept with the record, and returns the paths it wrote. The record restored from is the one that owns the artifact's path in
// the ledger (ownerOf), which is asked's own unless a later result of any patch was assigned to the same path: a path that is missing is restored from that result, and a result that is not the owner's never
// replaces the owner's files. A record with no kept copy (a ledger from before results were kept) restores nothing. The files belong in the directory a concurrent review of the same head may be about to publish
// into, so the run lock is held: a caller that holds it says so, any other tries it once and leaves the repair to its next call when the lock is busy, and then reads the ledger again, since the owner may have changed.
func (l *ledger) restore(ctx context.Context, asked record, recs []record, out string, locked bool) (written []string, err error) {
	if asked.Artifact == "" || asked.SHA256 == "" {
		return nil, nil
	}
	ownerRecord := func(recs []record) record {
		if owner := ownerOf(recs, asked.Artifact); owner != nil {
			return *owner
		}
		return asked
	}
	r := ownerRecord(recs)
	artifact, checksum := restoreWanted(recs, r)
	if !artifact && !checksum {
		return nil, nil
	}
	if _, err := os.Stat(l.keptPath(r.SHA256)); errors.Is(err, fs.ErrNotExist) { // a ledger from before results were kept: nothing to restore from, and no lock to take
		return nil, nil
	}
	if !locked {
		unlock, err := l.lock(ctx, -1)
		if errors.Is(err, errBusy) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		defer unlock()
		if recs, err = l.read(); err != nil {
			return nil, err
		}
		r = ownerRecord(recs)
		if artifact, checksum = restoreWanted(recs, r); !artifact && !checksum {
			return nil, nil
		}
	}
	kept, err := os.ReadFile(l.keptPath(r.SHA256))
	if err != nil {
		return nil, err
	}
	if digest := sha256.Sum256(kept); hex.EncodeToString(digest[:]) != r.SHA256 {
		return nil, fmt.Errorf("the kept copy %s does not have the recorded sha256 %s", l.keptPath(r.SHA256), r.SHA256)
	}
	if dir := filepath.Dir(r.Artifact); dir == out {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	} else if info, err := os.Stat(dir); err != nil || !info.IsDir() { // the directory of an earlier --out that is gone is not recreated
		return nil, nil
	}
	for _, f := range []struct {
		path string
		data []byte
		want bool
	}{{r.Artifact, kept, artifact}, {r.Artifact + ".sha256", []byte(checksumLine(r)), checksum}} {
		if !f.want {
			continue
		}
		if err := crwdir.Publish(f.path, f.data); err != nil {
			return written, fmt.Errorf("the review is recorded as finished but %s could not be restored from the kept result: %w", f.path, err)
		}
		written = append(written, f.path)
	}
	return written, nil
}
