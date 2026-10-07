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
//
// An input that was absent when it was recorded is still a name the bundle must not be written to,
// and it is examined again at every comparison: a path that is there by the time the collection is
// ready to write is an input it never pinned, and the run is refused rather than reading or
// overwriting it. A store's write-ahead log, shared-memory index and rollback journal are the one
// exception: SQLite creates and removes them as it checkpoints, so their names are refused as
// outputs whether or not they exist, while appearing or disappearing is not a change to the
// store's committed state.

// improveReasonInputChanged is the named refusal of a path that no longer names the file the
// collection read, or that was absent when it was recorded and is there now.
const improveReasonInputChanged = "improve_input_changed"

// improveReasonOutputUnreadable is the named refusal of a destination the collection cannot examine
// for a reason other than its absence. A comparison that cannot be made is never a pass.
const improveReasonOutputUnreadable = "improve_output_unreadable"

// improveIdentityEntry is one input the collection opens: the path it opens, that path resolved the
// way the reader resolves it, and the descriptor held open until the bundle is renamed.
//
// sidecar marks a store's write-ahead log, shared-memory index and rollback journal. Their names
// are refused as outputs whether or not they exist, because SQLite may create one while the store
// is read; a sidecar that is there is pinned like any other input, and one that is not is not a
// reason to refuse, because a clean checkpoint removes them.
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

// improveIdentityPin opens the path and fills the entry's identity from the descriptor it opened,
// keeping that descriptor open so the inode cannot be reused and the identity stays comparable
// until the rename. A path that is absent leaves the entry with its name alone; a path that exists
// but cannot be opened or examined is a refusal, because an input the collection cannot pin is one
// it cannot prove it will not overwrite.
func improveIdentityPin(entry *improveIdentityEntry) error {
	file, err := os.Open(entry.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s: the input %s could not be opened to record its identity: %w", improveReasonInputChanged, entry.path, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("%s: the input %s could not be examined to record its identity: %w", improveReasonInputChanged, entry.path, statErr)
	}
	entry.info, entry.file = info, file
	return nil
}

// improveIdentityAdd records a path as an input. A path already recorded keeps the identity it was
// recorded with; the comparison is on the spelling, never on the resolved name, because two
// configured paths that resolve to the same file are two inputs and a link at either spelling can
// be retargeted after recording while each reader still opens the spelling its source was
// configured with. The one path examined again is a store sidecar recorded while it was absent:
// SQLite creates the write-ahead log and the shared-memory index as it works, so one that appeared
// beside the store is pinned here rather than left as a bare name.
func (ids *improveIdentitySet) improveIdentityAdd(path, resolved string, sidecar bool) error {
	if path == "" || !ids.guarded {
		return nil
	}
	if resolved == "" {
		resolved = improveIdentityResolved(path)
	}
	for i := range ids.entries {
		if ids.entries[i].path != path {
			continue
		}
		if ids.entries[i].info != nil || !sidecar {
			return nil
		}
		ids.entries[i].resolved = resolved
		return improveIdentityPin(&ids.entries[i])
	}
	entry := improveIdentityEntry{path: path, resolved: resolved, sidecar: sidecar}
	if err := improveIdentityPin(&entry); err != nil {
		return err
	}
	ids.entries = append(ids.entries, entry)
	return nil
}

// improveIdentityRecordStore records a relay or DAG source: the configured path, the store file the
// reader opens, and the store's write-ahead log, shared-memory index and rollback journal. The
// store file is the path store.InPlaceRead returns, which is the file OpenInPlace opens, so the
// recorded identity is the one the read itself uses.
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
	// The three sidecar names are inputs whether or not they exist: SQLite creates a write-ahead
	// log, a shared-memory index or a rollback journal beside the store as it works, so a bundle
	// written to one of those names would corrupt a store that is being read.
	for _, sidecar := range []string{examined + "-wal", examined + "-shm", examined + "-journal"} {
		if err := ids.improveIdentityAdd(sidecar, sidecar, true); err != nil {
			return err
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
// place is still recognised. An input that was absent when it was recorded is compared at its
// spelling resolved again now, so a link that appeared at that name is seen as the file it reaches.
// A destination that cannot be examined for a reason other than its absence is refused too: a
// comparison that cannot be made is never a pass.
func (ids *improveIdentitySet) improveIdentityRefuse(dest string, parent os.FileInfo) error {
	destInfo, err := os.Lstat(dest)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: the output %s could not be examined: %w", improveReasonOutputUnreadable, dest, err)
	}
	for _, entry := range ids.entries {
		resolved := entry.resolved
		if entry.info == nil {
			resolved = improveIdentityResolved(entry.path)
		}
		if resolved == "" {
			continue
		}
		if dest == resolved || strings.HasPrefix(dest, improvePrefix(resolved)) {
			return improveIdentityRefusal(dest, resolved)
		}
		if entry.info != nil && destInfo != nil && os.SameFile(entry.info, destInfo) {
			return improveIdentityRefusal(dest, resolved)
		}
		if entry.info != nil && entry.info.IsDir() && parent != nil && os.SameFile(entry.info, parent) {
			return improveIdentityRefusal(dest, resolved)
		}
	}
	return nil
}

// improveIdentityVerify examines every recorded path again and refuses when it no longer reaches the
// file the descriptor holds. It runs after each reader returns and again immediately before the
// rename, so an input replaced, moved or removed between the read and the write is refused rather
// than silently replaced by the bundle.
//
// An input that was absent when it was recorded and is there now is refused as well: it is a file
// this collection never pinned, so the bundle is not written over it. A store sidecar is the one
// exception to both rules: SQLite checkpoints it away and recreates it, so its absence is not a
// change and its appearance is pinned when the inputs are recorded again.
func (ids *improveIdentitySet) improveIdentityVerify() error {
	for _, entry := range ids.entries {
		if entry.info == nil {
			if entry.sidecar {
				continue
			}
			if _, err := os.Lstat(entry.path); err == nil {
				return fmt.Errorf("%s: the input %s was absent when it was recorded and is there now, so the collection never pinned it", improveReasonInputChanged, entry.path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s: the input %s could not be examined after it was recorded: %w", improveReasonInputChanged, entry.path, err)
			}
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
