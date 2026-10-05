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

// A result is kept in the state directory, in results/<sha256>.json, before the ledger record that names it is appended. The record is what closes the patch, and the
// two output files are written after it, so a failed write leaves a patch that is reviewed and a result that exists nowhere else; the kept copy lets the next call write
// the files again without a model call.
func (l *ledger) keptPath(sha string) string { return filepath.Join(l.dir, "results", sha+".json") }

func (l *ledger) keep(sha string, data []byte) error {
	if err := os.MkdirAll(filepath.Join(l.dir, "results"), 0o700); err != nil {
		return err
	}
	return crwdir.Publish(l.keptPath(sha), data)
}

func checksumLine(r record) string { return r.SHA256 + "  " + filepath.Base(r.Artifact) + "\n" }

// restoreWanted reports which files of the artifact r recorded a restore would write: the artifact when nothing is at its path, or when what is there is exactly the bytes of the earlier
// unavailable attempt that r replaced (a retry whose files were not written); the checksum file when it is missing or says something else and the artifact it sits beside has the recorded
// bytes (or is written now), so that a foreign file, or a newer result that replaced r at the same path, never gets a checksum that is not its own. Any other file at those paths is not ours to replace.
func restoreWanted(r record, earlier *record) (artifact, checksum bool) {
	if _, err := os.Lstat(r.Artifact); errors.Is(err, fs.ErrNotExist) {
		artifact = true
	} else if earlier != nil && earlier.SHA256 != r.SHA256 && replaces(earlier, r.Artifact) {
		artifact = true
	}
	got, err := os.ReadFile(r.Artifact + ".sha256")
	checksum = err != nil || string(got) != checksumLine(r)
	if checksum && !artifact {
		checksum = replaces(&r, r.Artifact) // replaces: the file at the path has the bytes r recorded
	}
	return artifact, checksum
}

// restore writes the files of the artifact r recorded that restoreWanted names, from the copy kept with the record, and returns the paths it wrote. A record with no kept copy (a ledger from before
// results were kept) restores nothing. The files belong in the directory a concurrent review of the same head may be about to publish into, so the run lock is held: a caller that holds it says so, any
// other tries it once and leaves the repair to its next call when the lock is busy.
func (l *ledger) restore(ctx context.Context, r record, earlier *record, out string, locked bool) (written []string, err error) {
	if r.Artifact == "" || r.SHA256 == "" {
		return nil, nil
	}
	artifact, checksum := restoreWanted(r, earlier)
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
		if artifact, checksum = restoreWanted(r, earlier); !artifact && !checksum {
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
