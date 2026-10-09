package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A result is kept in the state directory, in results/<sha256>.json, before the ledger record that names it is appended, and the record is appended only when the copy is kept: written, renamed into
// place and the directory entry fsynced (crwdir.PublishDurable; the directory of results itself is synced into the state directory when it is created), so a record never names a copy that a power
// failure can take away, and a copy that cannot be kept leaves the patch open for another run. The record is what closes the patch, and the two output files are written after it, so a failed write
// leaves a patch that is reviewed and a result that is kept; the kept copy lets the next call write the files again without a model call.
func (l *ledger) keptPath(sha string) string { return filepath.Join(l.dir, "results", sha+".json") }

func (l *ledger) keep(sha string, data []byte) error {
	results := filepath.Join(l.dir, "results")
	_, statErr := os.Lstat(results)
	if err := os.MkdirAll(results, 0o700); err != nil {
		return err
	}
	if statErr != nil { // the directory was just created: its entry in the state directory is made durable too
		if err := crwdir.SyncDir(l.dir); err != nil {
			return fmt.Errorf("sync %s: %w", l.dir, err)
		}
	}
	publish := l.publishKept
	if publish == nil {
		publish = crwdir.PublishDurable
	}
	return publish(l.keptPath(sha), data)
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
