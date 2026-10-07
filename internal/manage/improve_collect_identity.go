package manage

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The collection pins the identity of every input it reads before it reads it, and compares the
// output against those identities rather than against a list of names rebuilt at the last moment.
// A name list says where an input was, not which file it is, so an input moved aside between the
// read and the rename fell out of the guard and the rename replaced the only copy. The identity
// recorded here is the descriptor the collection opened and the file that descriptor names, which
// no rename changes; the paths are resolved the way the readers resolve theirs, so the guard and
// the read name the same file.

// improveReasonInputChanged is the named refusal of a path that no longer names the file the
// collection read, or that could not be pinned at all. The descriptor it opened is the evidence,
// and a path that reaches a different file after the read is a different input.
const improveReasonInputChanged = "improve_input_changed"

// improveReasonOutputUnreadable is the named refusal of a destination the collection cannot examine
// for a reason other than its absence. A comparison that cannot be made is never a pass.
const improveReasonOutputUnreadable = "improve_output_unreadable"

// improveIdentityEntry is one input the collection opens: the path it opens, that path resolved the
// way the reader resolves it, and the descriptor held open until the bundle is renamed.
//
// sidecar marks a store's write-ahead log and shared-memory index. SQLite unlinks both when the last
// connection checkpoints and closes, which is routine and leaves the database's committed state
// alone, so a sidecar path that no longer exists is not a changed input; a sidecar that now names a
// different file still is.
type improveIdentityEntry struct {
	path     string
	resolved string
	info     os.FileInfo
	file     *os.File
	sidecar  bool
}

// improveIdentitySet is every input one collection opens. It is recorded before the readers run and
// recorded again after each of them returns, so a file that appears while a reader runs is still an
// input of the set by the time the bundle is renamed. The comparison never rebuilds the recorded
// identities from names.
//
// guarded is false for a collection that writes to stdout: it has no destination to protect, so it
// opens nothing and holds no descriptor.
type improveIdentitySet struct {
	entries []improveIdentityEntry
	guarded bool
}

// improveIdentityNew is an empty set that records its inputs when guard is true.
func improveIdentityNew(guard bool) *improveIdentitySet {
	return &improveIdentitySet{guarded: guard}
}

// improveIdentityClose releases every descriptor the set holds open.
func (ids *improveIdentitySet) improveIdentityClose() {
	for i := range ids.entries {
		if ids.entries[i].file != nil {
			_ = ids.entries[i].file.Close()
			ids.entries[i].file = nil
		}
	}
}

// improveIdentityResolved is a path resolved the way the readers resolve theirs, keeping the
// spelling when it cannot be resolved at all.
func improveIdentityResolved(path string) string {
	resolved, err := improveResolvedPath(path)
	if err != nil {
		return path
	}
	return resolved
}

// improveIdentityRecorded reports whether a path is already an input of the set. The comparison is
// on the spelling, never on the resolved name: two configured paths that resolve to the same file
// are two inputs, because a link at either spelling can be retargeted after recording while each
// reader still opens the spelling its source was configured with.
func (ids *improveIdentitySet) improveIdentityRecorded(path string) bool {
	for _, entry := range ids.entries {
		if entry.path == path {
			return true
		}
	}
	return false
}

// improveIdentityAdd opens a path and records it. A path that is absent is recorded by name alone,
// because the read reports it. A path that exists but cannot be opened or examined is a refusal: an
// input the collection cannot pin is one it cannot prove it will not overwrite, and a failure to
// pin is never a pass.
func (ids *improveIdentitySet) improveIdentityAdd(path, resolved string, sidecar bool) error {
	if path == "" || !ids.guarded || ids.improveIdentityRecorded(path) {
		return nil
	}
	if resolved == "" {
		resolved = improveIdentityResolved(path)
	}
	entry := improveIdentityEntry{path: path, resolved: resolved, sidecar: sidecar}
	file, err := os.Open(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("%s: the input %s could not be opened to record its identity: %w", improveReasonInputChanged, path, err)
	default:
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return fmt.Errorf("%s: the input %s could not be examined to record its identity: %w", improveReasonInputChanged, path, statErr)
		}
		entry.info, entry.file = info, file
	}
	ids.entries = append(ids.entries, entry)
	return nil
}

// improveIdentityRecordStore records a relay or DAG source: the configured path, the store file the
// reader opens, and the store's write-ahead log and shared-memory index when they exist. The store
// file is the path store.InPlaceRead returns, which is the file OpenInPlace opens, so the recorded
// identity is the one the read itself uses.
func (ids *improveIdentitySet) improveIdentityRecordStore(configured string) error {
	if err := ids.improveIdentityAdd(configured, "", false); err != nil {
		return err
	}
	storePath, err := improveStorePath(configured)
	if err != nil {
		// The configured path does not name a store the reader can open; the read reports that.
		return nil
	}
	// The store file is the path store.InPlaceRead returns, which is the file OpenInPlace opens, so
	// the recorded spelling and identity are the ones the read itself uses.
	examined := improveIdentityResolved(storePath)
	if inPlace, _, err := store.InPlaceRead(storePath); err == nil {
		examined = inPlace
	}
	if err := ids.improveIdentityAdd(storePath, examined, false); err != nil {
		return err
	}
	for _, sidecar := range []string{examined + "-wal", examined + "-shm"} {
		if _, err := os.Lstat(sidecar); err == nil {
			if err := ids.improveIdentityAdd(sidecar, sidecar, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// improveIdentityRecordDrafts records every draft the drafts reader will open. The candidate files
// come from improveParseDraftFiles, the reader's own enumeration, so the guard records the exact
// paths that read opens: a configured directory spelled through a link and ".." is enumerated at
// one place while the reader opens the joined spelling, and recording only the resolved spelling
// would leave the file that was read unprotected.
func (ids *improveIdentitySet) improveIdentityRecordDrafts(configured string) error {
	files, err := improveParseDraftFiles(configured)
	if err != nil {
		// The drafts source could not be enumerated; the reader reports that.
		return nil
	}
	for _, file := range files {
		if err := ids.improveIdentityAdd(file, "", false); err != nil {
			return err
		}
	}
	return nil
}

// improveIdentityRecord records every input the collection reads: for a relay or DAG source the
// store file the reader opens and its sidecars, the configured relay and DAG directories
// themselves, the drafts directory and every entry the reader would take from it, and the audit,
// intervention and issue-list files. It is called before the readers run and again after each of
// them, so an entry that appeared in the meantime is still an input.
func (ids *improveIdentitySet) improveIdentityRecord(section improveSection) error {
	if !ids.guarded {
		return nil
	}
	for _, kind := range []string{improveKindRelay, improveKindDag} {
		if configured := section.Sources[kind].Path; configured != "" {
			if err := ids.improveIdentityRecordStore(configured); err != nil {
				return err
			}
		}
	}
	for _, kind := range []string{improveKindAudit, improveKindIntervention} {
		if path := section.Sources[kind].Path; path != "" {
			if err := ids.improveIdentityAdd(path, "", false); err != nil {
				return err
			}
		}
	}
	if path := section.Sources[improveKindDraft].Path; path != "" {
		if err := ids.improveIdentityAdd(path, "", false); err != nil {
			return err
		}
		if err := ids.improveIdentityRecordDrafts(path); err != nil {
			return err
		}
	}
	if section.IssueList != "" {
		if err := ids.improveIdentityAdd(section.IssueList, "", false); err != nil {
			return err
		}
	}
	return nil
}

// improveIdentityRefresh records the inputs again and then examines every recorded path, so an input
// that appeared or changed while a reader ran is still caught. It is the step the collection runs
// after each reader returns.
func (ids *improveIdentitySet) improveIdentityRefresh(section improveSection) error {
	if !ids.guarded {
		return nil
	}
	if err := ids.improveIdentityRecord(section); err != nil {
		return err
	}
	return ids.improveIdentityVerify()
}

// improveIdentityRefusal is the one refusal of an output that is, or lies under, an input.
func improveIdentityRefusal(dest, input string) error {
	return fmt.Errorf("%s: the output %s is the input %s the collection opens: a bundle never overwrites its own evidence", improveReasonOutputIsInput, dest, input)
}

// improveIdentityRefuse refuses a resolved destination that is one of the recorded inputs, lies
// under one, or shares its parent directory with one. The destination is compared against the
// identity the descriptor reports rather than against a name, so an input moved onto the output's
// place is still recognised. A destination that cannot be examined for a reason other than its
// absence is refused too: a comparison that cannot be made is never a pass.
func (ids *improveIdentitySet) improveIdentityRefuse(dest string, parent os.FileInfo) error {
	destInfo, err := os.Lstat(dest)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: the output %s could not be examined: %w", improveReasonOutputUnreadable, dest, err)
	}
	for _, entry := range ids.entries {
		if entry.resolved == "" {
			continue
		}
		if dest == entry.resolved || strings.HasPrefix(dest, improvePrefix(entry.resolved)) {
			return improveIdentityRefusal(dest, entry.resolved)
		}
		if entry.info != nil && destInfo != nil && os.SameFile(entry.info, destInfo) {
			return improveIdentityRefusal(dest, entry.resolved)
		}
		if entry.info != nil && entry.info.IsDir() && parent != nil && os.SameFile(entry.info, parent) {
			return improveIdentityRefusal(dest, entry.resolved)
		}
	}
	return nil
}

// improveIdentityVerify examines every recorded path again and refuses when it no longer reaches the
// file the descriptor holds. It runs after each reader returns and again immediately before the
// rename, so an input replaced, moved or removed between the read and the write is refused rather
// than silently replaced by the bundle. A store sidecar SQLite checkpointed away is the one absence
// that is not a refusal: the database still holds its committed state.
func (ids *improveIdentitySet) improveIdentityVerify() error {
	for _, entry := range ids.entries {
		if entry.info == nil {
			continue
		}
		current, err := os.Stat(entry.path)
		if err != nil {
			if entry.sidecar && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%s: the input %s no longer names the file the collection read: %w", improveReasonInputChanged, entry.path, err)
		}
		if !os.SameFile(entry.info, current) {
			return fmt.Errorf("%s: the input %s no longer names the file the collection read", improveReasonInputChanged, entry.path)
		}
	}
	return nil
}
