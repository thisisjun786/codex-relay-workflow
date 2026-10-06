package install

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// The OPS-4.5 route for the additive DAG zone (D-01) and for ordinary indexes on tables the store already holds (CRW-472).
//
// A candidate that declares the zone installs onto a store that predates it (every store there is) without
// changing a byte of it: the zone arrives with the candidate's first write-open. The swap gate still refuses
// that arrival (swapgate.ExtendsZone) because OPS-4.5 requires a copied backup of the whole state directory
// first, and this file is the one route through: --backup-state-to DIR is the operator's acknowledgement, and
// the backup is taken by the command, inside the promotion lock, after the daemon and in-flight cells have
// answered and before anything is promoted, so the backup is guaranteed by the route and not by memory.
// Ordinary indexes arrive the same way (swapgate.ExtendsIndex): the candidate builds them on its first write-open,
// and the same acknowledgement and the same backup carry them.
// Copy only: the source is opened read-only, and nothing is moved, recreated or deleted, here or on a failure.

// stateBackupStep is a seam: tests make the backup fail, or change the state directory, between the copy and
// its verification. It is called with "copied".
var stateBackupStep = func(step string) error { return nil }

// stateBackupListed is a seam: it is called after the first listing of the state directory and before anything
// is copied, which is where another connection to the store makes SQLite's sidecars appear or go. It is a seam
// of its own and not stateBackupStep: the tests that replace that one act after the copy, and calling the same
// one twice would change them.
var stateBackupListed = func() error { return nil }

// stateBackupVerified is a seam: it is called once every byte of the copy is in place and verified, and before the
// integrity gate opens its scratch duplicate. A test damages the copy here to prove the gate refuses it (CRW-862).
var stateBackupVerified = func() error { return nil }

// integrityCheckPath is a seam: it opens the scratch duplicate and returns SQLite's PRAGMA integrity_check answer.
// nil runs the real check. A test that cannot corrupt a real store substitutes the answer instead.
var integrityCheckPath = sqliteIntegrityCheck

// stateBackupWalk is a seam: it is called for each entry of a listing after the directory was read and before the
// entry's own information is asked, which is where another connection to the store makes a sidecar go. It is called
// with the entry's path.
var stateBackupWalk = func(path string) error { return nil }

// syncDirectory makes a directory's entries durable; a seam so a test can see which directories were synced and when.
var syncDirectory = syncPath

// syncAfterMode makes a copy's mode durable: a mode set on an inode reaches the disk only when that inode is synced
// after it is set, and syncing its parent persists the name and not the mode. A seam so a test can see the mode an
// entry had when it was synced.
var syncAfterMode = syncPath

// ManifestSuffix names the record written beside a backup (not inside it, so the backup holds exactly what the
// state directory held).
const ManifestSuffix = ".manifest.json"

// The store's two sidecars. SQLite keeps a write-ahead log beside the database while a connection is open, and a
// shared-memory index beside that; the last connection to close deletes them. The index is an accelerator SQLite
// rebuilds from the log when it next opens the file, so a backup never carries it. The log holds the commits, so a
// backup carries it when it is there at its copy. An empty log that goes or arrives while the copy is being made is
// tolerated — that is the churn this route exists for — but one that was not copied and holds frames in the second
// listing refuses: its commits would otherwise be lost without the store's own file changing to show it.
const (
	walSidecar = "relay.sqlite3-wal"
	shmSidecar = "relay.sqlite3-shm"
)

// The four readings the manifest records for the store's write-ahead log.
const (
	sidecarAbsent   = "absent"
	sidecarCopied   = "copied"
	sidecarGone     = "gone before its copy"
	sidecarAppeared = "appeared after the listing, not copied"
)

// walHeaderBytes is SQLite's write-ahead log header: a log no longer than it holds no frame. It is the threshold that
// tells a log another connection left empty (the case the backup route exists for) from one that carries a commit the
// copy does not hold.
const walHeaderBytes = 32

// storeSidecarWal is the manifest's reading of the store's write-ahead log from what happened to it while the backup
// was made: its bytes were copied, it was listed and had gone by its copy, it was absent from the first listing and
// appeared in the second so it was not copied, or it was absent from both listings. The appeared reading cannot be
// reached end to end — the swap gate's own read of the store leaves an empty log in the state directory before the
// first listing — so it is pinned by the white-box test that calls this through export_test.go.
func storeSidecarWal(copied, gone, appeared bool) string {
	switch {
	case gone:
		return sidecarGone
	case copied:
		return sidecarCopied
	case appeared:
		return sidecarAppeared
	default:
		return sidecarAbsent
	}
}

// storeSidecars are the store's two sidecars as SQLite resolves them: the path store.InPlaceRead
// resolves the database to, with -wal and -shm appended. A store whose database is a link has its
// sidecars beside the file the link names, wherever that lies, so every reading recognises them by
// this resolved path and never by the top-level name alone (CRW-862, PR #735 P1 2). When the store's
// path cannot be resolved the state directory's own two names are used, which is what a listing did
// before the identity was resolved.
type storeSidecars struct{ wal, shm string }

// sidecarsFor is the store's sidecars: the resolved database path's -wal and -shm, or the state
// directory's own two names when the store's path cannot be resolved.
func sidecarsFor(source, dbPath string) storeSidecars {
	if resolved, _, err := store.InPlaceRead(dbPath); err == nil {
		return storeSidecars{wal: resolved + "-wal", shm: resolved + "-shm"}
	}
	return storeSidecars{wal: filepath.Join(source, walSidecar), shm: filepath.Join(source, shmSidecar)}
}

func (s storeSidecars) isWal(path string) bool { return path == s.wal }
func (s storeSidecars) isShm(path string) bool { return path == s.shm }
func (s storeSidecars) is(path string) bool    { return s.isWal(path) || s.isShm(path) }

// listingHasWal is whether a listing holds the store's write-ahead log.
func listingHasWal(entries []backedUp) bool {
	for _, e := range entries {
		if e.storeWal {
			return true
		}
	}
	return false
}

type backedUp struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"` // dir, file, link-file (a symbolic link to a regular file: its bytes are copied)
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`     // the source permission bits
	CopyMode   uint32 `json:"copyMode"` // the copy: the source, with the owner able to open it (see copyMode)
	SHA256     string `json:"sha256,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
	source     string // where the bytes are read from
	storeWal   bool   // the store's write-ahead log, whichever path it was resolved to (CRW-862)
	vanished   bool   // the store's log went between the walk's read and its own information (CRW-862, PR #735 P1 3)
}

// swapGate is OPS-4.4 asked of the relay the record selects now (the candidate's own on a
// first install) and of the candidate binary's declared schema, with the OPS-4.5 route for the additive zone:
// when that arrival (or ordinary indexes arriving) is the only thing refusing and the operator acknowledged it,
// the state directory is copied and the verdict is asked again with the copy recorded.
func swapGate(ctx context.Context, o Options, rec Object, candidate string, candidateSchema Object) Object {
	cells := gateCells(ctx, o, rec, candidate, candidateSchema)
	gate := swapgate.DecideWithRelease(cells, nil)
	if o.StateBackup == "" || !swapgate.AdditiveArrivalOnly(cells) {
		if o.StateBackup != "" {
			gate = record.Set(gate, "stateBackup", Object{field("requested", true), field("made", false),
				field("reason", "the only schema difference is not the additive zone or ordinary indexes arriving (or the daemon, the open attempts or a schema reading refuses first), so no backup route applies and none was taken")})
		}
		return gate
	}
	backup, err := backupState(ctx, o, o.StateBackup)
	if err != nil {
		gate = record.Set(gate, "stateBackup", backup)
		blocked, _ := record.Get(gate, "blockedBy").([]any)
		return record.Set(gate, "blockedBy", append([]any{"stateBackup: " + err.Error()}, blocked...))
	}
	return swapgate.DecideWithRelease(cells, &swapgate.Release{Backup: backup})
}

// gateFields is the swap gate as a result carries it, nothing when the gate was not asked.
func gateFields(gate Object) []contract.Field {
	if gate == nil {
		return nil
	}
	return []contract.Field{field("swapGate", gate)}
}

// afterBackup is what a refusal that comes after the gate must still say when the gate took the backup: the copy
// exists and is kept, so "nothing was written" would not be true of the state directory's neighbour.
func afterBackup(gate Object, note string) string {
	backup, _ := record.Get(gate, "stateBackup").(Object)
	if record.Get(backup, "made") != true {
		return note
	}
	return strings.Replace(note, "nothing was written", "nothing but the backup was written", 1) + " The copy of the state directory taken before this step stays at " + record.Text(backup, "destination") + " (with its manifest beside it), and a rerun needs a new destination."
}

// refusedAfterGate is a refusal of the swap after the gate answered: it carries the gate, and with it the backup.
func (r *run) refusedAfterGate(detail, note string, extra ...contract.Field) (Object, int) {
	return refusedResult(r.command, detail, afterBackup(r.gate, note), append(extra, gateFields(r.gate)...)...)
}

// backupState copies the whole state directory the gate read to dest and returns the record of it. dest and
// its manifest must not exist and must not lie inside the source or inside the install's own destination tree
// (a failed run removes its candidate runtime, and a backup there would go with it); every regular file is
// copied with its bytes, hashed while it is read, then the source is read again and every file, the listing
// and the store's three files must be what was copied. A partial copy after an error stays where it is.
func backupState(ctx context.Context, o Options, dest string) (Object, error) {
	failure := func(created bool, format string, args ...any) (Object, error) {
		err := fmt.Errorf(format, args...)
		return Object{field("requested", true), field("made", false), field("destination", dest), field("partial", created), field("kept", created), field("error", err.Error())}, err
	}
	selection, err := store.ResolveStateDir(o.State, o.Socket)
	if err != nil {
		return failure(false, "the state directory could not be resolved: %v", err)
	}
	source, err := filepath.EvalSymlinks(selection.Path)
	if err != nil {
		return failure(false, "the state directory %s could not be read: %v", selection.Path, err)
	}
	if err := refuseDestination(dest, source, o.Dest); err != nil {
		return failure(false, "%v", err)
	}
	manifestPath := dest + ManifestSuffix
	for _, path := range []string{dest, manifestPath} {
		if _, err := os.Lstat(path); err == nil {
			return failure(false, "%s already exists: a backup is never written over anything", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return failure(false, "%s could not be examined: %w", path, err)
		}
	}
	entries, skipped, err := listState(source, selection.DBPath())
	if err != nil {
		return failure(false, "%v", err)
	}
	if err := stateBackupListed(); err != nil {
		return failure(false, "%v", err)
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	if err := enoughRoom(filepath.Dir(dest), total); err != nil {
		return failure(false, "%v", err)
	}
	// the nearest directory that exists now: the directories made below it are synced, up to it, once the backup is whole
	existing := filepath.Dir(dest)
	for {
		if _, err := os.Lstat(existing); err == nil || filepath.Dir(existing) == existing {
			break
		}
		existing = filepath.Dir(existing)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return failure(false, "the backup's parent could not be created: %v", err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return failure(false, "the backup directory could not be created: %v", err)
	}
	copied := make([]backedUp, 0, len(entries))
	var walCopied, walGone bool
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return failure(true, "interrupted while copying: %v", err)
		}
		if e.vanished {
			// the log went between the listing's read and its own information: it is recorded as gone before its copy
			walGone = true
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(e.Path))
		if e.Kind == "dir" {
			if err := os.Mkdir(target, 0o700); err != nil {
				return failure(true, "%s could not be created: %v", target, err)
			}
			copied = append(copied, e)
			continue
		}
		digest, size, err := copyFile(ctx, e.source, target)
		if err != nil {
			// The store's log was checkpointed away between the listing and this copy: the store's own file still has
			// to be what was read, and that check is kept, so a log that went is not a refusal. Every other file that
			// goes between the two readings still refuses.
			if e.storeWal && errors.Is(err, fs.ErrNotExist) {
				walGone = true
				continue
			}
			return failure(true, "%s could not be copied: %v", e.Path, err)
		}
		e.SHA256 = digest
		if e.storeWal {
			// Only the log's entry takes the size of the bytes copied. Every other file keeps the size of the first
			// listing, so a file that changed size between that listing and its copy is still a change the comparison
			// below catches: the store's log is the one file another connection may rewrite under the copy, and its
			// entry has to describe what the copy holds.
			e.Size = size
			walCopied = true
		}
		copied = append(copied, e)
	}
	for _, e := range copied {
		if e.Kind == "dir" {
			if err := syncDirectory(filepath.Join(dest, filepath.FromSlash(e.Path))); err != nil {
				return failure(true, "%v", err)
			}
		}
	}
	if err := syncDirectory(dest); err != nil {
		return failure(true, "%v", err)
	}
	if err := stateBackupStep("copied"); err != nil {
		return failure(true, "%v", err)
	}
	// The verification: what is in the backup is what was read, and what the state directory holds now is
	// what was read as well, so the copy is neither damaged nor a mixture of two moments.
	again, againSkipped, err := listState(source, selection.DBPath())
	if err != nil {
		return failure(true, "the state directory could not be read again: %v", err)
	}
	// The appeared reading is the log absent from the first listing and present in the second, so it was not copied.
	walAppeared := !walCopied && !walGone && !listingHasWal(entries) && listingHasWal(again)
	// A log that was not copied but holds frames in the second listing is a commit the copy does not hold. Another
	// connection wrote it while the copy was being made, and a commit that stays in the log does not touch
	// relay.sqlite3 until a checkpoint, so the digest check below would not see it: the backup would report success
	// while the live store held a row the copy lacks. An empty log (SQLite's header or less) holds no frame and stays
	// tolerated, which is the case this route exists for: a read-only open of a store no connection holds leaves one.
	if !walCopied {
		for _, e := range again {
			if e.storeWal && e.Size > walHeaderBytes {
				return failure(true, "relay.sqlite3-wal holds commits that were not copied (%d bytes appeared or were left after the first listing), so the backup is not a copy of any one moment", e.Size)
			}
		}
	}
	if diff := listingDiff(copied, skipped, again, againSkipped); diff != "" {
		return failure(true, "the state directory changed under the copy (%s), so the backup is not a copy of any one moment", diff)
	}
	for _, e := range copied {
		if e.Kind == "dir" {
			continue
		}
		now, err := digestOf(e.source)
		switch {
		case err != nil && e.storeWal && errors.Is(err, fs.ErrNotExist):
			// The log was checkpointed away by the time of the verification, so the source cannot be compared; that is
			// not a refusal. The copy is still read below: skipping the whole entry would let a copy that was damaged
			// while its source went pass as a good backup.
		case err != nil:
			return failure(true, "%s could not be read again: %v", e.Path, err)
		case now != e.SHA256:
			return failure(true, "%s changed under the copy (it digested to %s and now to %s)", e.Path, e.SHA256, now)
		}
		held, err := digestOf(filepath.Join(dest, filepath.FromSlash(e.Path)))
		if err != nil {
			return failure(true, "the copy of %s could not be read: %v", e.Path, err)
		}
		if held != e.SHA256 {
			return failure(true, "the copy of %s is not the file it copies (%s against %s)", e.Path, held, e.SHA256)
		}
	}
	// the modes of the source, now that every byte is in place and verified (a mode set earlier could make a copy
	// unreadable to the verification, a directory first would refuse its own children): files, then directories
	// deepest first. The directory the backup is made in keeps the 0700 it was created with.
	// Each is synced right after its mode is set, while it can still be opened: a mode the source's owner could
	// read through is one the copy's owner can open to sync.
	setMode := func(e backedUp) error {
		path := filepath.Join(dest, filepath.FromSlash(e.Path))
		if err := os.Chmod(path, fs.FileMode(e.CopyMode)); err != nil {
			return fmt.Errorf("the mode of the copy of %s could not be set: %v", e.Path, err)
		}
		if err := syncAfterMode(path); err != nil {
			return fmt.Errorf("the mode of the copy of %s could not be made durable: %w", e.Path, err)
		}
		return nil
	}
	for _, e := range copied {
		if e.Kind != "dir" {
			if err := setMode(e); err != nil {
				return failure(true, "%v", err)
			}
		}
	}
	for i := len(copied) - 1; i >= 0; i-- {
		if e := copied[i]; e.Kind == "dir" {
			if err := setMode(e); err != nil {
				return failure(true, "%v", err)
			}
		}
	}
	if err := stateBackupVerified(); err != nil {
		return failure(true, "%v", err)
	}
	// The integrity gate (CRW-862, docs/port/decisions.md section 83 item 4 and slice S7): the copy is only a
	// restore candidate when PRAGMA integrity_check passes on it. The check runs on a scratch duplicate beside the
	// copy, never on the copy itself, because opening a database checkpoints its write-ahead log and would change the
	// bytes the backup holds. The duplicate is removed either way.
	integrity, err := integrityCheckPath(dest, copied)
	restoreCandidate := err == nil
	if err != nil && integrity == "" {
		integrity = err.Error()
	}
	aggregate, files, bytes := aggregateOf(copied)
	manifest := map[string]any{
		"schema": "crw-state-backup/1", "source": source, "destination": dest, "issue": o.Issue, "at": o.stamp(),
		"files": files, "bytes": bytes, "aggregateDigest": aggregate, "skipped": skipped, "entries": copied,
		"storeSidecars":  map[string]string{shmSidecar: "not copied: the WAL index SQLite rebuilds from the log on open", walSidecar: storeSidecarWal(walCopied, walGone, walAppeared)},
		"integrityCheck": integrity, "restoreCandidate": restoreCandidate,
		"meaning": "a copy of the relay state directory taken by crw install before the swap that brings the additive DAG zone or ordinary indexes (D-01, CRW-472, OPS-4.5); copy only, byte for byte",
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return failure(true, "the manifest could not be encoded: %v", err)
	}
	out, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return failure(true, "the manifest could not be created: %v", err)
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		_ = out.Close()
		return failure(true, "the manifest could not be written: %v", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return failure(true, "the manifest could not be synced: %v", err)
	}
	if err := out.Close(); err != nil {
		return failure(true, "the manifest could not be closed: %v", err)
	}
	// the entries of the backup and of its manifest live in their parent, and in the directories made above it: until
	// those are synced a power loss can keep the files and lose the names that reach them
	for dir := filepath.Dir(dest); ; dir = filepath.Dir(dir) {
		if err := syncDirectory(dir); err != nil {
			return failure(true, "%v", err)
		}
		if dir == existing || filepath.Dir(dir) == dir {
			break
		}
	}
	if !restoreCandidate {
		// The copy and its manifest are there and stay there: the manifest is what records that this artifact is not a
		// restore candidate, so the refusal carries it rather than a partial copy with no record.
		err := fmt.Errorf("the copy is not a restore candidate: PRAGMA integrity_check answered %q", integrity)
		return Object{field("requested", true), field("made", false), field("destination", dest), field("partial", true), field("kept", true),
			field("manifest", manifestPath), field("files", files), field("bytes", bytes), field("aggregateDigest", aggregate),
			field("integrityCheck", integrity), field("restoreCandidate", false), field("error", err.Error())}, err
	}
	return Object{field("requested", true), field("made", true), field("destination", dest), field("manifest", manifestPath), field("source", source),
		field("files", files), field("bytes", bytes), field("skipped", strs(skipped)), field("aggregateDigest", aggregate), field("kept", true), field("at", o.stamp()),
		field("integrityCheck", integrity), field("restoreCandidate", restoreCandidate),
		field("meaning", "the state directory was copied, byte for byte, after the daemon and in-flight cells answered and before the swap; nothing was moved or deleted, and the copy stays whatever happens next (a rerun needs a new destination)")}, nil
}

// refuseDestination is why dest cannot hold a backup, or nil: it lies in the source, or in the install's own
// destination tree (the candidate runtime, every bin-* directory and the pointer, which a failed run removes).
func refuseDestination(dest, source, installDest string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("%s is not an absolute path", dest)
	}
	resolved, err := resolveWithRemainder(dest)
	if err != nil {
		return fmt.Errorf("%s could not be resolved: %v", dest, err)
	}
	if inside(resolved, source) {
		return fmt.Errorf("%s is the state directory or inside it, and a copy of a directory into itself is not a backup of it", dest)
	}
	for _, tree := range []string{installDest} {
		if tree == "" {
			continue
		}
		root, err := resolveWithRemainder(tree)
		if err != nil {
			return fmt.Errorf("%s could not be resolved: %v", tree, err)
		}
		if inside(resolved, root) {
			return fmt.Errorf("%s is inside %s, the runtime destination tree: a run that fails there removes its candidate runtime, and the backup must outlive it", dest, tree)
		}
	}
	return nil
}

// resolveWithRemainder is the path with its nearest existing ancestor resolved through symbolic links and the
// rest appended, so a destination that does not exist yet is judged by where it would land.
func resolveWithRemainder(path string) (string, error) {
	path = filepath.Clean(path)
	var rest []string
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		if filepath.Dir(current) == current {
			return "", fmt.Errorf("no part of %s exists", path)
		}
		rest = append(rest, filepath.Base(current))
	}
}

func inside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// enoughRoom refuses before any byte is copied when the filesystem the backup lands on cannot hold it.
func enoughRoom(parent string, need int64) error {
	probe, err := resolveWithRemainder(parent)
	if err != nil {
		return err
	}
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		probe = filepath.Dir(probe)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(probe, &stat); err != nil {
		return fmt.Errorf("the free space under %s could not be read: %v", probe, err)
	}
	if free := uint64(stat.Bavail) * uint64(stat.Bsize); free < uint64(need)+1<<20 {
		return fmt.Errorf("%s has %d bytes free and the state directory holds %d, so the backup would not fit", probe, free, need)
	}
	return nil
}

// listState walks the state directory without following links: directories, regular files, and symbolic links to
// regular files (whose bytes are copied under the link's name, since a link alone would back up nothing). The store's
// own two sidecars are recognised by the path SQLite resolves the database to, not by the top-level name: a store
// whose relay.sqlite3 is a link to a file inside the state directory has its -wal and -shm beside that file, and the
// walk leaves the -shm out and hands the -wal to the sidecar rules there as it does at the top level (CRW-862, PR
// #735 P1 2). Whichever path the log was resolved to, it is listed under the restore-compatible name
// relay.sqlite3-wal, so the copy opens with the commits only the log held. Its -shm is not listed at all: SQLite
// rebuilds that index from the log on open, so a backup neither copies it nor refuses when one appears or goes. A
// socket, a FIFO or a device is listed as skipped. A link to anything else is a refusal.
func listState(source, dbPath string) ([]backedUp, []string, error) {
	sidecars := sidecarsFor(source, dbPath)
	var entries []backedUp
	var skipped []string
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// The store's shared-memory index is never listed, wherever SQLite resolved it to. Its write-ahead log is
		// listed under the restore-compatible name when it is the state directory's own; a log beside a linked store
		// is registered below, under that same name, so the walk leaves it here.
		if sidecars.isShm(path) {
			return nil
		}
		storeWal := sidecars.isWal(path)
		if storeWal && rel != walSidecar {
			return nil
		}
		if err := stateBackupWalk(path); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			// Another connection to the store may make its sidecars go between the walk's ReadDir and this Info. A log
			// or index of the store that went there is recorded as gone before its copy, not a refusal; every other
			// file's ENOENT, and every other error, still refuses (CRW-862, PR #735 P1 3).
			if errors.Is(err, fs.ErrNotExist) && (storeWal || sidecars.isShm(path)) {
				if storeWal {
					// the log is recorded in the listing as gone before its copy, so the manifest's reading and the copy
					// loop see what happened even though there is nothing left to copy
					entries = append(entries, backedUp{Path: rel, Kind: "file", storeWal: true, vanished: true})
				}
				return nil
			}
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			entries = append(entries, backedUp{Path: rel, Kind: "dir", Mode: uint32(mode.Perm()), CopyMode: uint32(copyMode(true, mode.Perm()))})
		case mode.IsRegular():
			entries = append(entries, backedUp{Path: rel, Kind: "file", Size: info.Size(), Mode: uint32(mode.Perm()), CopyMode: uint32(copyMode(false, mode.Perm())), source: path, storeWal: storeWal})
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("%s is a link to %s, which cannot be followed: %v", rel, target, err)
			}
			real, err := os.Stat(resolved)
			if err != nil || !real.Mode().IsRegular() {
				return fmt.Errorf("%s is a link to %s, which is not a regular file, so there is no byte-identical copy of it to make", rel, target)
			}
			entries = append(entries, backedUp{Path: rel, Kind: "link-file", Size: real.Size(), Mode: uint32(real.Mode().Perm()), CopyMode: uint32(copyMode(false, real.Mode().Perm())), LinkTarget: target, source: resolved, storeWal: storeWal})
		default:
			skipped = append(skipped, rel+" ("+kindOf(mode)+")")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// the store's log file, when its database is a link to a file elsewhere in the state directory or outside it (its
	// -shm is never listed): it is copied beside the link's own copy under the restore-compatible name
	if resolved, _, err := store.InPlaceRead(dbPath); err == nil && resolved != filepath.Join(source, "relay.sqlite3") {
		have := map[string]bool{}
		for _, e := range entries {
			have[e.Path] = true
		}
		if info, err := os.Lstat(resolved + "-wal"); err == nil {
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("%s is not a regular file, so the store's write-ahead log cannot be copied", resolved+"-wal")
			}
			if have[walSidecar] {
				return nil, nil, fmt.Errorf("the state directory already holds %s, which the store's log %s would have to be copied over", walSidecar, resolved+"-wal")
			}
			entries = append(entries, backedUp{Path: walSidecar, Kind: "link-file", Size: info.Size(), Mode: uint32(info.Mode().Perm()), CopyMode: uint32(copyMode(false, info.Mode().Perm())), LinkTarget: resolved + "-wal", source: resolved + "-wal", storeWal: true})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Strings(skipped)
	return entries, skipped, nil
}

// copyMode is the mode a copy is given: the source, with the owner able to open it. The installer owns every copy,
// and a source it could read through its group (a file of another user) may leave its own owner no read bit, which
// would leave the copy unopenable to the user who took the backup, to sync it or to restore from it. The manifest
// records both modes.
func copyMode(dir bool, mode fs.FileMode) fs.FileMode {
	if dir {
		return mode | 0o700
	}
	return mode | 0o400
}

func kindOf(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeNamedPipe != 0:
		return "fifo"
	case mode&fs.ModeDevice != 0:
		return "device"
	}
	return "other"
}

// listingDiff is how two readings of the state directory differ, or "".
func listingDiff(a []backedUp, askipped []string, b []backedUp, bskipped []string) string {
	// The store's write-ahead log is compared by its identity and not by its size or its presence: another
	// connection may checkpoint it away and write a fresh one under the copy, so its size and its coming and going
	// belong to the sidecar rules (the manifest's reading, and the frames-left-uncopied refusal). Its identity -
	// which entry it is, whether it is a link, and which file the bytes come from - is a fact about the copy, and a
	// log whose link target or resolved source moved between the two readings refuses (CRW-862, PR #735 P1 1). The
	// store's shared-memory index is never listed. relay.sqlite3 itself and every other file keep the full key,
	// size included, so a store written under the copy is still a change.
	full := func(e backedUp) string {
		return fmt.Sprintf("%s|%s|%d|%s|%s", e.Path, e.Kind, e.Size, e.LinkTarget, e.source)
	}
	identity := func(e backedUp) string {
		return fmt.Sprintf("%s|%s|%s|%s", e.Path, e.Kind, e.LinkTarget, e.source)
	}
	var walA, walB *string
	seen := map[string]bool{}
	for _, e := range a {
		if e.storeWal {
			value := identity(e)
			walA = &value
			continue
		}
		seen[full(e)] = true
	}
	var changed []string
	for _, e := range b {
		if e.storeWal {
			value := identity(e)
			walB = &value
			continue
		}
		if !seen[full(e)] {
			changed = append(changed, "new or changed: "+e.Path)
		}
		delete(seen, full(e))
	}
	for k := range seen {
		path, _, _ := strings.Cut(k, "|")
		changed = append(changed, "gone or changed: "+path)
	}
	// the log is a change only when both readings hold it and its identity moved: its absence from one reading is
	// the churn the sidecar rules already carry
	if walA != nil && walB != nil && *walA != *walB {
		changed = append(changed, "the store's write-ahead log changed: "+walSidecar)
	}
	if strings.Join(askipped, "\n") != strings.Join(bskipped, "\n") {
		changed = append(changed, "an entry that is not a file appeared or went")
	}
	sort.Strings(changed)
	if len(changed) > 5 {
		changed = append(changed[:5], fmt.Sprintf("and %d more", len(changed)-5))
	}
	return strings.Join(changed, "; ")
}

func aggregateOf(entries []backedUp) (string, int, int64) {
	h := sha256.New()
	var files int
	var bytes int64
	for _, e := range entries {
		if e.Kind == "dir" {
			fmt.Fprintf(h, "%s\tdir\n", e.Path)
			continue
		}
		files++
		bytes += e.Size
		fmt.Fprintf(h, "%s\t%s\t%d\t%s\n", e.Path, e.Kind, e.Size, e.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)), files, bytes
}

// copyFile copies source to target (created, never replaced) and returns the digest of what it read and how many
// bytes that was; the file is synced before it is closed. The size is the one actually copied, which is what the
// manifest records for a store log another connection may be rewriting under the copy.
func copyFile(ctx context.Context, source, target string) (string, int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return "", 0, err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				_ = out.Close()
				return "", 0, err
			}
			h.Write(buf[:n])
			size += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return "", 0, readErr
		}
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func digestOf(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sqliteIntegrityCheck is the integrity gate of a backup copy: it duplicates the copied store beside the copy and
// asks SQLite PRAGMA integrity_check on the duplicate. The copy itself is never opened: opening a database
// checkpoints its write-ahead log and would change the bytes the backup holds, so the duplicate is the one SQLite
// touches. The duplicate holds the copy's log too when the backup carries one, so a store whose commits live only
// in the log is checked with them. The duplicate is removed either way, and the copy's bytes are not touched. The
// answer is SQLite's own, joined when it is several rows; an error is why the copy is not a restore candidate.
func sqliteIntegrityCheck(dest string, copied []backedUp) (string, error) {
	scratch, err := os.MkdirTemp(filepath.Dir(dest), ".crw-integrity-")
	if err != nil {
		return "", fmt.Errorf("the scratch directory for the integrity check could not be created: %v", err)
	}
	defer os.RemoveAll(scratch)
	var database string
	for _, e := range copied {
		if e.Kind == "dir" || !strings.HasPrefix(e.Path, "relay.sqlite3") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(e.Path)))
		if err != nil {
			return "", fmt.Errorf("the copy of %s could not be read for the integrity check: %v", e.Path, err)
		}
		target := filepath.Join(scratch, filepath.Base(e.Path))
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			return "", fmt.Errorf("the integrity check's duplicate of %s could not be written: %v", e.Path, err)
		}
		if e.Path == "relay.sqlite3" {
			database = target
		}
	}
	if database == "" {
		return "", errors.New("the copy holds no relay.sqlite3, so there is nothing for the integrity check to open")
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return "", fmt.Errorf("the integrity check could not open the copy: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return "", fmt.Errorf("the integrity check could not run: %v", err)
	}
	defer rows.Close()
	var answers []string
	for rows.Next() {
		var answer string
		if err := rows.Scan(&answer); err != nil {
			return "", fmt.Errorf("the integrity check's answer could not be read: %v", err)
		}
		answers = append(answers, answer)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("the integrity check did not finish: %v", err)
	}
	if len(answers) == 0 {
		return "", errors.New("the integrity check answered nothing")
	}
	answer := strings.Join(answers, "; ")
	if answer != "ok" {
		return answer, fmt.Errorf("PRAGMA integrity_check answered %q", answer)
	}
	return answer, nil
}

func syncPath(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s could not be opened to sync it: %w", path, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%s could not be synced: %w", path, err)
	}
	return nil
}
