package install

import (
	"context"
	"crypto/sha256"
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

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// The OPS-4.5 route for the additive DAG zone (D-01).
//
// A candidate that declares the zone installs onto a store that predates it (every store there is) without
// changing a byte of it: the zone arrives with the candidate's first write-open. The swap gate still refuses
// that arrival (swapgate.ExtendsZone) because OPS-4.5 requires a copied backup of the whole state directory
// first, and this file is the one route through: --backup-state-to DIR is the operator's acknowledgement, and
// the backup is taken by the command, inside the promotion lock, after the daemon and in-flight cells have
// answered and before anything is promoted, so the backup is guaranteed by the route and not by memory.
// Copy only: the source is opened read-only, and nothing is moved, recreated or deleted, here or on a failure.

// stateBackupStep is a seam: tests make the backup fail, or change the state directory, between the copy and
// its verification. It is called with "copied".
var stateBackupStep = func(step string) error { return nil }

// syncDirectory makes a directory's entries durable; a seam so a test can see which directories were synced and when.
var syncDirectory = syncPath

// ManifestSuffix names the record written beside a backup (not inside it, so the backup holds exactly what the
// state directory held).
const ManifestSuffix = ".manifest.json"

type backedUp struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"` // dir, file, link-file (a symbolic link to a regular file: its bytes are copied)
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	SHA256     string `json:"sha256,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
	source     string // where the bytes are read from
}

// swapGate is OPS-4.4 asked of the relay the record selects now (the candidate's own on a
// first install) and of the candidate binary's declared schema, with the OPS-4.5 route for the additive zone:
// when that arrival is the only thing refusing and the operator acknowledged it, the state directory is copied
// and the verdict is asked again with the copy recorded.
func swapGate(ctx context.Context, o Options, rec Object, candidate string, candidateSchema Object) Object {
	cells := gateCells(ctx, o, rec, candidate, candidateSchema)
	gate := swapgate.DecideWithRelease(cells, nil)
	if o.StateBackup == "" || !swapgate.ZoneArrivalOnly(cells) {
		if o.StateBackup != "" {
			gate = record.Set(gate, "stateBackup", Object{field("requested", true), field("made", false),
				field("reason", "the only schema difference is not the additive zone arriving (or the daemon, the open attempts or a schema reading refuses first), so no backup route applies and none was taken")})
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
			return failure(false, "%s could not be examined: %v", path, err)
		}
	}
	entries, skipped, err := listState(source, selection.DBPath())
	if err != nil {
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
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return failure(true, "interrupted while copying: %v", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(e.Path))
		if e.Kind == "dir" {
			if err := os.Mkdir(target, 0o700); err != nil {
				return failure(true, "%s could not be created: %v", target, err)
			}
			copied = append(copied, e)
			continue
		}
		digest, err := copyFile(ctx, e.source, target)
		if err != nil {
			return failure(true, "%s could not be copied: %v", e.Path, err)
		}
		e.SHA256 = digest
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
	if diff := listingDiff(copied, skipped, again, againSkipped); diff != "" {
		return failure(true, "the state directory changed under the copy (%s), so the backup is not a copy of any one moment", diff)
	}
	for _, e := range copied {
		if e.Kind == "dir" {
			continue
		}
		now, err := digestOf(e.source)
		if err != nil {
			return failure(true, "%s could not be read again: %v", e.Path, err)
		}
		if now != e.SHA256 {
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
	for _, e := range copied {
		if e.Kind != "dir" {
			if err := os.Chmod(filepath.Join(dest, filepath.FromSlash(e.Path)), fs.FileMode(e.Mode)); err != nil {
				return failure(true, "the mode of the copy of %s could not be set: %v", e.Path, err)
			}
		}
	}
	for i := len(copied) - 1; i >= 0; i-- {
		if e := copied[i]; e.Kind == "dir" {
			if err := os.Chmod(filepath.Join(dest, filepath.FromSlash(e.Path)), fs.FileMode(e.Mode)); err != nil {
				return failure(true, "the mode of the copy of %s could not be set: %v", e.Path, err)
			}
		}
	}
	aggregate, files, bytes := aggregateOf(copied)
	manifest := map[string]any{
		"schema": "crw-state-backup/1", "source": source, "destination": dest, "issue": o.Issue, "at": o.stamp(),
		"files": files, "bytes": bytes, "aggregateDigest": aggregate, "skipped": skipped, "entries": copied,
		"meaning": "a copy of the relay state directory taken by crw install before the swap that brings the additive DAG zone (D-01, OPS-4.5); copy only, byte for byte",
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
	return Object{field("requested", true), field("made", true), field("destination", dest), field("manifest", manifestPath), field("source", source),
		field("files", files), field("bytes", bytes), field("skipped", strs(skipped)), field("aggregateDigest", aggregate), field("kept", true), field("at", o.stamp()),
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
// own files are named as SQLite resolves them: when relay.sqlite3 is a link, the real file's -wal and -shm are
// copied beside it as relay.sqlite3-wal and -shm, so a restore opens with the commits only the log held. A socket,
// a FIFO or a device is listed as skipped. A link to anything else is a refusal.
func listState(source, dbPath string) ([]backedUp, []string, error) {
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			entries = append(entries, backedUp{Path: rel, Kind: "dir", Mode: uint32(mode.Perm())})
		case mode.IsRegular():
			entries = append(entries, backedUp{Path: rel, Kind: "file", Size: info.Size(), Mode: uint32(mode.Perm()), source: path})
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
			entries = append(entries, backedUp{Path: rel, Kind: "link-file", Size: real.Size(), Mode: uint32(real.Mode().Perm()), LinkTarget: target, source: resolved})
		default:
			skipped = append(skipped, rel+" ("+kindOf(mode)+")")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// the store's log files, when its database is a link to a file elsewhere
	if resolved, _, err := store.InPlaceRead(dbPath); err == nil && resolved != filepath.Join(source, "relay.sqlite3") {
		have := map[string]bool{}
		for _, e := range entries {
			have[e.Path] = true
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			info, err := os.Lstat(resolved + suffix)
			if err != nil {
				continue
			}
			name := "relay.sqlite3" + suffix
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("%s is not a regular file, so the store's %s cannot be copied", resolved+suffix, suffix)
			}
			if have[name] {
				return nil, nil, fmt.Errorf("the state directory already holds %s, which the store's log %s would have to be copied over", name, resolved+suffix)
			}
			entries = append(entries, backedUp{Path: name, Kind: "link-file", Size: info.Size(), Mode: uint32(info.Mode().Perm()), LinkTarget: resolved + suffix, source: resolved + suffix})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Strings(skipped)
	return entries, skipped, nil
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
	// the resolved file the bytes come from is part of what a reading says: a link in the chain that was pointed
	// elsewhere between the two readings is a change, even where the new file is the same size
	key := func(e backedUp) string {
		return fmt.Sprintf("%s|%s|%d|%s|%s", e.Path, e.Kind, e.Size, e.LinkTarget, e.source)
	}
	seen := map[string]bool{}
	for _, e := range a {
		seen[key(e)] = true
	}
	var changed []string
	for _, e := range b {
		if !seen[key(e)] {
			changed = append(changed, "new or changed: "+e.Path)
		}
		delete(seen, key(e))
	}
	for k := range seen {
		path, _, _ := strings.Cut(k, "|")
		changed = append(changed, "gone or changed: "+path)
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

// copyFile copies source to target (created, never replaced) and returns the digest of what it read; the file is
// synced before it is closed.
func copyFile(ctx context.Context, source, target string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return "", err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				_ = out.Close()
				return "", err
			}
			h.Write(buf[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return "", readErr
		}
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

func syncPath(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s could not be opened to sync it: %v", path, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%s could not be synced: %v", path, err)
	}
	return nil
}
