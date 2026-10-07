package manage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// collection read: the descriptor it opened is the evidence, and a path that reaches a different
// file after the read is a different input.
const improveReasonInputChanged = "improve_input_changed"

// improveReasonOutputUnreadable is the named refusal of a destination the collection cannot examine
// for a reason other than its absence. A comparison that cannot be made is never a pass.
const improveReasonOutputUnreadable = "improve_output_unreadable"

// improveIdentityEntry is one input the collection opens: the path it opens, that path resolved the
// way the reader resolves it, and the descriptor held open until the bundle is renamed. A configured
// path that cannot be opened keeps its resolved spelling and no descriptor: the read reports that
// failure itself, and the spelling still lets the output guard compare the destination against the
// name the read would use.
type improveIdentityEntry struct {
	path     string
	resolved string
	info     os.FileInfo
	file     *os.File
}

// improveIdentitySet is every input one collection opens. It is recorded once, before any reader
// runs, and compared against after the readers return and again immediately before the rename, so
// the comparison never rebuilds the name list it replaced.
type improveIdentitySet struct {
	entries []improveIdentityEntry
}

// improveIdentityNew is an empty set.
func improveIdentityNew() *improveIdentitySet { return &improveIdentitySet{} }

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

// improveIdentityRecorded reports whether a resolved path is already an input of the set, so one
// file reached under two spellings is opened once and held once.
func (ids *improveIdentitySet) improveIdentityRecorded(resolved string) bool {
	for _, entry := range ids.entries {
		if entry.resolved == resolved {
			return true
		}
	}
	return false
}

// improveIdentityAdd opens a path and records it under its resolved spelling. A path that cannot be
// opened keeps the resolved spelling and no descriptor.
func (ids *improveIdentitySet) improveIdentityAdd(path, resolved string) {
	if path == "" {
		return
	}
	if resolved == "" {
		resolved = improveIdentityResolved(path)
	}
	if ids.improveIdentityRecorded(resolved) {
		return
	}
	entry := improveIdentityEntry{path: path, resolved: resolved}
	if file, err := os.Open(path); err == nil {
		if info, statErr := file.Stat(); statErr == nil {
			entry.info, entry.file = info, file
		} else {
			_ = file.Close()
		}
	}
	ids.entries = append(ids.entries, entry)
}

// improveIdentityRecordStore records a relay or DAG source: the configured path, the store file the
// reader opens, and the store's write-ahead log and shared-memory index when they exist. The store
// file is the path store.InPlaceRead returns, which is the file OpenInPlace opens, so the recorded
// identity is the one the read itself uses.
func (ids *improveIdentitySet) improveIdentityRecordStore(configured string) {
	ids.improveIdentityAdd(configured, "")
	storePath, err := improveStorePath(configured)
	if err != nil {
		// The configured path does not name a store the reader can open; the read reports that.
		return
	}
	resolved := improveIdentityResolved(storePath)
	if examined, _, err := store.InPlaceRead(storePath); err == nil {
		resolved = examined
	}
	ids.improveIdentityAdd(storePath, resolved)
	for _, sidecar := range []string{resolved + "-wal", resolved + "-shm"} {
		if _, err := os.Lstat(sidecar); err == nil {
			ids.improveIdentityAdd(sidecar, sidecar)
		}
	}
}

// improveIdentityRecordDrafts records every entry of a drafts directory the collection reads, so a
// link to a file elsewhere is an input too.
func (ids *improveIdentitySet) improveIdentityRecordDrafts(configured string) {
	resolved := improveIdentityResolved(configured)
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		ids.improveIdentityAdd(resolved+string(filepath.Separator)+entry.Name(), "")
	}
}

// improveIdentityRecord records every input the collection reads, before any reader runs: for a
// relay or DAG source the store file the reader opens and its sidecars, the configured relay and
// DAG directories themselves, the drafts directory and every .json entry read from it, and the
// audit, intervention and issue-list files.
func (ids *improveIdentitySet) improveIdentityRecord(section improveSection) {
	for _, kind := range []string{improveKindRelay, improveKindDag} {
		if configured := section.Sources[kind].Path; configured != "" {
			ids.improveIdentityRecordStore(configured)
		}
	}
	for _, kind := range []string{improveKindAudit, improveKindIntervention} {
		if path := section.Sources[kind].Path; path != "" {
			ids.improveIdentityAdd(path, "")
		}
	}
	if path := section.Sources[improveKindDraft].Path; path != "" {
		ids.improveIdentityAdd(path, "")
		ids.improveIdentityRecordDrafts(path)
	}
	if section.IssueList != "" {
		ids.improveIdentityAdd(section.IssueList, "")
	}
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
// file the descriptor holds. It runs after the readers return and again immediately before the
// rename, so an input replaced, moved or removed between the read and the write is refused rather
// than silently replaced by the bundle.
func (ids *improveIdentitySet) improveIdentityVerify() error {
	for _, entry := range ids.entries {
		if entry.info == nil {
			continue
		}
		current, err := os.Stat(entry.path)
		if err != nil {
			return fmt.Errorf("%s: the input %s no longer names the file the collection read: %w", improveReasonInputChanged, entry.path, err)
		}
		if !os.SameFile(entry.info, current) {
			return fmt.Errorf("%s: the input %s no longer names the file the collection read", improveReasonInputChanged, entry.path)
		}
	}
	return nil
}
