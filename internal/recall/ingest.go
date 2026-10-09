// CXC v0.2.40 recall/src/ingest.ts (3c1459ac): incremental sidecar refresh.
package recall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

const TOOL_TEXT_CAP = 8192
const BACKFILL_BATCH = 1000

type IngestResult struct {
	Scanned  float64 `json:"scanned"`
	Ingested float64 `json:"ingested"`
	Appended float64 `json:"appended"`
	Pruned   float64 `json:"pruned"`
	Msgs     float64 `json:"msgs"`
	// Skipped counts the files that could not be read this time; the rest were indexed.
	Skipped   float64 `json:"skipped,omitempty"`
	ElapsedMs float64 `json:"elapsedMs"`
}

type FreshnessBudget struct {
	MaxStats float64 `json:"maxStats"`
	MaxMs    float64 `json:"maxMs"`
}

// BannerFreshnessBudget ports BANNER_FRESHNESS_BUDGET without startup work.
func BannerFreshnessBudget() FreshnessBudget { return FreshnessBudget{512, 50} }

type IndexFreshness struct {
	SourceFiles  float64 `json:"sourceFiles"`
	IndexedFiles float64 `json:"indexedFiles"`
	MissingFiles float64 `json:"missingFiles"`
	ChangedFiles float64 `json:"changedFiles"`
	ExtraFiles   float64 `json:"extraFiles"`
	StaleFiles   float64 `json:"staleFiles"`
	Truncated    bool    `json:"truncated"`
	// Verified is set when the counts were decided from file content (the explicit strong path),
	// not from size and mtime alone. A zero value means metadata-only freshness.
	Verified bool `json:"verified,omitempty"`
	// RebuildRequired is set when a content check was asked for on an index of an older schema: it
	// holds no checkpoints to check against, so the counts are metadata-only and the next writer
	// rebuilds the index.
	RebuildRequired bool `json:"rebuildRequired,omitempty"`
}

// KnownFile is a file's stored cursor. FileID and Checkpoint are empty in the metadata-only reads.
type KnownFile struct {
	MTimeMS, Size, BytesIngested, LastOrd float64
	FileID, Checkpoint                    string
}

// sameFile is false only when both sides name a file and the names differ.
func sameFile(prev KnownFile, st os.FileInfo) bool {
	id := fileIdentity(st)
	return prev.FileID == "" || id == "" || prev.FileID == id
}

// fingerprintMatches is the metadata comparison: size, millisecond mtime and, where the cursor
// stores one, the device and inode.
func fingerprintMatches(prev KnownFile, st os.FileInfo) bool {
	return prev.MTimeMS == float64(st.ModTime().UnixMilli()) && prev.Size == float64(st.Size()) && sameFile(prev, st)
}

// The checkpoint covers the consumed prefix [0, n) of a rollout by two bounded windows: its first
// and its last checkpointWindow bytes. It proves that the file still begins and ends, at the
// consumed offset, as it did when those bytes were indexed. It does not prove the bytes in between:
// the producer of a rollout only appends, and a rewrite that keeps both windows is not detected.
const checkpointWindow = 4096

func checkpointDigest(head, tail []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d:", len(head), len(tail))
	h.Write(head)
	h.Write(tail)
	return "1:" + hex.EncodeToString(h.Sum(nil))
}

func prefixWindows(prefix []byte) (head, tail []byte) {
	return prefix[:min(len(prefix), checkpointWindow)], prefix[max(0, len(prefix)-checkpointWindow):]
}

// extendWindows returns the windows of the prefix that continues one with windows head and tail by appended.
func extendWindows(head, tail, appended []byte) ([]byte, []byte) {
	if len(head) < checkpointWindow {
		head = append(bytes.Clone(head), appended[:min(len(appended), checkpointWindow-len(head))]...)
	}
	all := append(bytes.Clone(tail), appended...)
	return head, all[max(0, len(all)-checkpointWindow):]
}

// readCheckpointWindows reads the windows of the first n bytes of the file; a file shorter than n is an error.
func readCheckpointWindows(path string, n int64) (head, tail []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	read := func(from, size int64) ([]byte, error) {
		buf := make([]byte, size)
		got, err := f.ReadAt(buf, from)
		if got == len(buf) {
			return buf, nil
		}
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	size := min(n, checkpointWindow)
	if head, err = read(0, size); err != nil {
		return nil, nil, err
	}
	if tail, err = read(n-size, size); err != nil {
		return nil, nil, err
	}
	return head, tail, nil
}

// checkpointHolds reports whether the file still has, at the consumed offset, the content the cursor stored.
func checkpointHolds(path string, prev KnownFile) (head, tail []byte, ok bool) {
	if prev.Checkpoint == "" || prev.BytesIngested < 0 {
		return nil, nil, false
	}
	head, tail, err := readCheckpointWindows(path, int64(prev.BytesIngested))
	if err != nil || checkpointDigest(head, tail) != prev.Checkpoint {
		return nil, nil, false
	}
	return head, tail, true
}

// unchanged is the skip decision for a file with a stored cursor. The strong path also requires the checkpoint to hold.
func (o ingestOptions) unchanged(prev KnownFile, st os.FileInfo, path string) bool {
	if !fingerprintMatches(prev, st) {
		return false
	}
	if !o.Verify {
		return true
	}
	_, _, ok := checkpointHolds(path, prev)
	return ok
}

// Only overlapping paths are statted. Missing/extra are path-set differences;
// a days-scoped walk cannot infer that older indexed files have disappeared.
func measureIndexFreshness(home string, db *RwDb, days float64, budget *FreshnessBudget) (IndexFreshness, error) {
	return measureIndexFreshnessMode(home, db, days, budget, false)
}

// ingestFileError marks a failure of one rollout (it could not be read), as against the index database's.
type ingestFileError struct{ err error }

func (e ingestFileError) Error() string { return e.err.Error() }
func (e ingestFileError) Unwrap() error { return e.err }

// ingestOptions selects how far a refresh looks before it trusts a file's stored state.
type ingestOptions struct {
	// Verify re-reads every indexed file's checkpoint, not only the files whose size or mtime moved.
	Verify bool
}

func measureIndexFreshnessMode(home string, db *RwDb, days float64, budget *FreshnessBudget, verify bool) (IndexFreshness, error) {
	files, unread, err := listRolloutFiles(home, days)
	if err != nil {
		return IndexFreshness{}, err
	}
	// An index written before the cursor carried a file identity and a checkpoint has nothing to
	// verify content against; the next writer rebuilds it. It is reported as such, from metadata.
	rebuild := verify && !(filesHasColumn(db, "file_id") && filesHasColumn(db, "checkpoint"))
	if rebuild {
		verify = false
	}
	query := "SELECT path, mtime_ms, size FROM files"
	if verify {
		query = "SELECT path, mtime_ms, size, bytes_ingested, file_id, checkpoint FROM files"
	}
	stmt, err := db.Prepare(query)
	if err != nil {
		return IndexFreshness{}, err
	}
	rows, err := stmt.All()
	if err != nil {
		return IndexFreshness{}, err
	}
	known := make(map[string]KnownFile, len(rows))
	for _, row := range rows {
		prev := KnownFile{MTimeMS: hitCountNumber(row["mtime_ms"]), Size: hitCountNumber(row["size"])}
		if verify {
			prev.BytesIngested = hitCountNumber(row["bytes_ingested"])
			prev.FileID, _ = row["file_id"].(string)
			prev.Checkpoint, _ = row["checkpoint"].(string)
		}
		known[memoryStatusString(row["path"])] = prev
	}
	f := IndexFreshness{SourceFiles: float64(len(files)), IndexedFiles: float64(len(known)), Verified: verify, RebuildRequired: rebuild}
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		paths[file.Path] = true
		if _, ok := known[file.Path]; !ok {
			f.MissingFiles++
		}
	}
	if days == 0 {
		for path := range known {
			if !paths[path] && !underAnyDir(path, unread) {
				f.ExtraFiles++
			}
		}
	}
	started, stats := time.Now().UnixMilli(), float64(0)
	for _, file := range files {
		prev, ok := known[file.Path]
		if !ok {
			continue
		}
		if budget != nil && (stats >= budget.MaxStats || float64(time.Now().UnixMilli()-started) >= budget.MaxMs) {
			f.Truncated = true
			break
		}
		stats++
		st, err := os.Stat(file.Path)
		if err != nil {
			continue
		}
		if !(ingestOptions{Verify: verify}).unchanged(prev, st, file.Path) {
			f.ChangedFiles++
		}
	}
	f.StaleFiles = f.MissingFiles + f.ChangedFiles + f.ExtraFiles
	return f, nil
}

func completeLineBoundary(buf []byte) int { return bytes.LastIndexByte(buf, '\n') + 1 }

func readSlice(path string, from, to int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if from < 0 || to < from {
		return nil, errors.New("invalid rollout byte range")
	}
	buf := make([]byte, to-from)
	n, err := f.ReadAt(buf, from)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// Per-file transactions, and separate backfill batches, match the oracle. Each one takes the
// SQLite write permit first (BEGIN IMMEDIATE) so that what work reads is what it then writes:
// a deferred BEGIN would let a decision taken from an older read commit over another refresh.
func ingestTransaction(db *RwDb, work func() error) error {
	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := work(); err != nil {
		if rollback := db.Exec("ROLLBACK"); rollback != nil {
			return rollback
		}
		return err
	}
	if err := db.Exec("COMMIT"); err != nil {
		if rollback := db.Exec("ROLLBACK"); rollback != nil {
			return rollback
		}
		return err
	}
	return nil
}

// ingestBeforeFileLock is a test seam: it runs after a file is chosen for work and before
// that file's write transaction begins.
var ingestBeforeFileLock func(path string)

type ingestStatements struct{ delMsgs, delFile, insFile, insMsg *Stmt }

func prepareIngest(db *RwDb) (ingestStatements, error) {
	var s ingestStatements
	for _, item := range []struct {
		dest **Stmt
		sql  string
	}{
		{&s.delMsgs, "DELETE FROM msgs WHERE path = ?"},
		{&s.delFile, "DELETE FROM files WHERE path = ?"},
		{&s.insFile, "INSERT OR REPLACE INTO files (path, mtime_ms, size, thread_id, cwd, source, date, bytes_ingested, last_ord, repo_key, file_id, checkpoint) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"},
		{&s.insMsg, "INSERT INTO msgs (path, ord, ts, role, match_field, synthetic, text) VALUES (?, ?, ?, ?, ?, ?, ?)"},
	} {
		stmt, err := db.Prepare(item.sql)
		if err != nil {
			return s, err
		}
		*item.dest = stmt
	}
	return s, nil
}

func ingest(home string, db *RwDb, days float64) (IngestResult, error) {
	return ingestWith(home, db, days, ingestOptions{})
}

func ingestWith(home string, db *RwDb, days float64, opts ingestOptions) (IngestResult, error) {
	started := time.Now().UnixMilli()
	backfillRepoKeysFromThreads(home, db)
	files, unread, err := listRolloutFiles(home, days)
	if err != nil {
		return IngestResult{}, err
	}
	stmt, err := db.Prepare("SELECT path, mtime_ms, size, bytes_ingested, last_ord, file_id, checkpoint FROM files")
	if err != nil {
		return IngestResult{}, err
	}
	rows, err := stmt.All()
	if err != nil {
		return IngestResult{}, err
	}
	known, paths := make(map[string]*KnownFile, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		path := memoryStatusString(row["path"])
		paths = append(paths, path)
		known[path] = ingestKnown(row)
	}
	s, err := prepareIngest(db)
	if err != nil {
		return IngestResult{}, err
	}
	r := IngestResult{Scanned: float64(len(files))}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		seen[file.Path] = true
		st, err := os.Stat(file.Path) // Before any content read, as in the oracle.
		if err != nil {
			continue
		}
		prev := known[file.Path]
		if prev != nil && opts.unchanged(*prev, st, file.Path) {
			continue
		}
		if ingestBeforeFileLock != nil {
			ingestBeforeFileLock(file.Path)
		}
		// Only the lock holder decides: the cursor is read again inside the transaction, because
		// another refresh may have committed this file since the snapshot above. No lock is held
		// while the home is listed or while unchanged files are skipped.
		if err := ingestTransaction(db, func() error { return ingestLocked(db, s, file, opts, &r) }); err != nil {
			var unreadable ingestFileError
			if errors.As(err, &unreadable) {
				r.Skipped++ // The file is left as it was; one unreadable rollout does not stop the refresh.
				continue
			}
			return IngestResult{}, err
		}
	}
	if days == 0 {
		for _, path := range paths {
			if seen[path] || underAnyDir(path, unread) {
				continue // Listed, or in a directory that could not be read: not shown to be gone.
			}
			// The listing is a snapshot, and may be old by now: the file may have come back and
			// been appended by another refresh. Only the lock holder decides, and it prunes a path
			// only when the file is absent at that moment.
			if err := ingestTransaction(db, func() error {
				if !rolloutPathGone(path) {
					return nil
				}
				if _, err := s.delMsgs.Run(path); err != nil {
					return err
				}
				gone, err := s.delFile.Run(path)
				if gone.Changes > 0 {
					r.Pruned++ // Another refresh may have pruned it since the snapshot.
				}
				return err
			}); err != nil {
				return IngestResult{}, err
			}
		}
	}
	if r.Ingested+r.Appended+r.Pruned > 0 {
		stmt, err := db.Prepare("INSERT OR REPLACE INTO meta (key, value) VALUES ('last_ingest_at', ?)")
		if err != nil {
			return IngestResult{}, err
		}
		if _, err := stmt.Run(time.Now().UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
			return IngestResult{}, err
		}
	}
	r.ElapsedMs = float64(time.Now().UnixMilli() - started)
	return r, nil
}

// rolloutPathGone is true only when the path is shown to be absent, or to be no longer a regular file
// (a directory, a link to nothing). A stat that fails for any other reason (permission, I/O) proves
// nothing, and the file's rows stay.
func rolloutPathGone(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
	}
	return !st.Mode().IsRegular()
}

// underAnyDir reports whether path lies below one of the directories.
func underAnyDir(path string, dirs []string) bool {
	for _, dir := range dirs {
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ingestLocked runs inside the write transaction. It reads the file's committed cursor and the
// file's state now, and decides from those: skip, append or rebuild. An append is chosen only
// while the file is shown to continue what was indexed: the same file (device and inode), grown,
// still holding at the consumed offset the content the cursor stored, with exactly the rows the
// cursor counts. Any other change replaces the file's rows in this transaction.
func ingestLocked(db *RwDb, s ingestStatements, file RolloutFile, opts ingestOptions, result *IngestResult) error {
	st, err := os.Stat(file.Path)
	if err != nil {
		return nil // Gone since it was listed; a later refresh prunes its rows.
	}
	prev, err := ingestCursor(db, file.Path)
	if err != nil {
		return err
	}
	if prev != nil && opts.unchanged(*prev, st, file.Path) {
		return nil // Another refresh already took this state.
	}
	var head, tail []byte
	if prev != nil {
		appendable := prev.BytesIngested > 0 && float64(st.Size()) > prev.Size && sameFile(*prev, st)
		if appendable {
			var held bool
			head, tail, held = checkpointHolds(file.Path, *prev)
			appendable = held
		}
		if appendable {
			// The cursor is trusted only while it equals what is committed for the file.
			stmt, err := db.Prepare("SELECT COUNT(*) AS n, MAX(ord) + 1 AS top FROM msgs WHERE path = ?")
			if err != nil {
				return err
			}
			row, err := stmt.Get(file.Path)
			if err != nil {
				return err
			}
			n := hitCountNumber(row["n"])
			appendable = n == prev.LastOrd && (n == 0 || hitCountNumber(row["top"]) == n)
		}
		if !appendable {
			prev = nil
		}
	}
	return ingestFile(s, file, prev, head, tail, st, result)
}

func ingestKnown(row map[string]any) *KnownFile {
	k := &KnownFile{MTimeMS: hitCountNumber(row["mtime_ms"]), Size: hitCountNumber(row["size"]), BytesIngested: hitCountNumber(row["bytes_ingested"]), LastOrd: hitCountNumber(row["last_ord"])}
	k.FileID, _ = row["file_id"].(string)
	k.Checkpoint, _ = row["checkpoint"].(string)
	return k
}

func ingestCursor(db *RwDb, path string) (*KnownFile, error) {
	stmt, err := db.Prepare("SELECT mtime_ms, size, bytes_ingested, last_ord, file_id, checkpoint FROM files WHERE path = ?")
	if err != nil {
		return nil, err
	}
	row, err := stmt.Get(path)
	if err != nil || row == nil {
		return nil, err
	}
	return ingestKnown(row), nil
}

// ingestFile indexes the file. A non-nil prev is an append from its consumed offset, and head and
// tail are the windows of the prefix already indexed; nil prev replaces the file's rows.
func ingestFile(s ingestStatements, file RolloutFile, prev *KnownFile, head, tail []byte, st os.FileInfo, result *IngestResult) error {
	meta, err := ReadRolloutMeta(file.Path)
	if err != nil {
		return ingestFileError{err}
	}
	appendOnly := prev != nil
	var buf []byte
	ord, offset := float64(0), float64(0)
	if appendOnly {
		buf, err = readSlice(file.Path, int64(prev.BytesIngested), st.Size())
		ord, offset = prev.LastOrd, prev.BytesIngested
	} else {
		buf, err = os.ReadFile(file.Path)
	}
	if err != nil {
		return ingestFileError{err}
	}
	boundary := completeLineBoundary(buf)
	if appendOnly {
		head, tail = extendWindows(head, tail, buf[:boundary])
	} else {
		head, tail = prefixWindows(buf[:boundary])
	}
	entries, err := ParseRollout(source.DecodeUTF8(buf[:boundary]), true)
	if err != nil {
		return ingestFileError{err}
	}
	if !appendOnly {
		if _, err := s.delMsgs.Run(file.Path); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		text := entry.Text
		if entry.MatchField == "tool_log" {
			units := utf16.Encode([]rune(text))
			if len(units) > TOOL_TEXT_CAP {
				text = string(utf16.Decode(units[:TOOL_TEXT_CAP]))
			}
		}
		synthetic := 0
		if entry.Synthetic {
			synthetic = 1
		}
		if _, err := s.insMsg.Run(file.Path, ord, entry.TS, entry.Role, entry.MatchField, synthetic, text); err != nil {
			return err
		}
		ord++
		result.Msgs++
	}
	if _, err := s.insFile.Run(file.Path, st.ModTime().UnixMilli(), st.Size(), ingestString(meta.ThreadID), ingestString(meta.Cwd), string(meta.Source), file.Date, offset+float64(boundary), ord, ingestString(meta.RepoKey), fileIdentity(st), checkpointDigest(head, tail)); err != nil {
		return err
	}
	if appendOnly {
		result.Appended++
	} else {
		result.Ingested++
	}
	return nil
}

func ingestString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// An older CLI's NULL keys are enriched before unchanged files can be skipped.
// The Codex state db is read-only; absent/locked/column-shy metadata is fail-soft.
func backfillRepoKeysFromThreads(home string, db *RwDb) {
	stmt, err := db.Prepare("SELECT COUNT(*) AS n FROM files WHERE repo_key IS NULL AND cwd IS NOT NULL AND thread_id IS NOT NULL")
	if err != nil {
		return
	}
	row, err := stmt.Get()
	if err != nil || hitCountNumber(row["n"]) == 0 {
		return
	}
	path, err := stateDbPath(home)
	if err != nil {
		return
	}
	meta := loadThreadMeta(path)
	pairs := [][2]string{}
	for _, id := range meta.IDs {
		if raw := meta.ByID[id].GitOriginURL; raw != nil {
			if key := normalizeRepoKey(*raw); key != "" {
				pairs = append(pairs, [2]string{key, id})
			}
		}
	}
	if len(pairs) == 0 {
		return
	}
	stmt, err = db.Prepare("UPDATE files SET repo_key = ? WHERE thread_id = ? AND repo_key IS NULL AND cwd IS NOT NULL")
	if err != nil {
		return
	}
	for i := 0; i < len(pairs); i += BACKFILL_BATCH {
		if err := ingestTransaction(db, func() error {
			for _, pair := range pairs[i:min(i+BACKFILL_BATCH, len(pairs))] {
				if _, err := stmt.Run(pair[0], pair[1]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return
		}
	}
}
