// CXC v0.2.40 recall/src/ingest.ts (3c1459ac): incremental sidecar refresh.
package recall

import (
	"bytes"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

const TOOL_TEXT_CAP = 8192
const BACKFILL_BATCH = 1000

type IngestResult struct {
	Scanned   float64 `json:"scanned"`
	Ingested  float64 `json:"ingested"`
	Appended  float64 `json:"appended"`
	Pruned    float64 `json:"pruned"`
	Msgs      float64 `json:"msgs"`
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
}

type KnownFile struct{ MTimeMS, Size, BytesIngested, LastOrd float64 }

func fingerprintMatches(prev KnownFile, st os.FileInfo) bool {
	return prev.MTimeMS == float64(st.ModTime().UnixMilli()) && prev.Size == float64(st.Size())
}

// Only overlapping paths are statted. Missing/extra are path-set differences;
// a days-scoped walk cannot infer that older indexed files have disappeared.
func measureIndexFreshness(home string, db *RwDb, days float64, budget *FreshnessBudget) (IndexFreshness, error) {
	files, err := ListRolloutFiles(home, days)
	if err != nil {
		return IndexFreshness{}, err
	}
	stmt, err := db.Prepare("SELECT path, mtime_ms, size FROM files")
	if err != nil {
		return IndexFreshness{}, err
	}
	rows, err := stmt.All()
	if err != nil {
		return IndexFreshness{}, err
	}
	known := make(map[string]KnownFile, len(rows))
	for _, row := range rows {
		known[memoryStatusString(row["path"])] = KnownFile{MTimeMS: hitCountNumber(row["mtime_ms"]), Size: hitCountNumber(row["size"])}
	}
	f := IndexFreshness{SourceFiles: float64(len(files)), IndexedFiles: float64(len(known))}
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		paths[file.Path] = true
		if _, ok := known[file.Path]; !ok {
			f.MissingFiles++
		}
	}
	if days == 0 {
		for path := range known {
			if !paths[path] {
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
		if !fingerprintMatches(prev, st) {
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

// Per-file transactions, and separate backfill batches, match the oracle.
func ingestTransaction(db *RwDb, work func() error) error {
	if err := db.Exec("BEGIN"); err != nil {
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

type ingestStatements struct{ delMsgs, delFile, insFile, insMsg *Stmt }

func prepareIngest(db *RwDb) (ingestStatements, error) {
	var s ingestStatements
	for _, item := range []struct {
		dest **Stmt
		sql  string
	}{
		{&s.delMsgs, "DELETE FROM msgs WHERE path = ?"},
		{&s.delFile, "DELETE FROM files WHERE path = ?"},
		{&s.insFile, "INSERT OR REPLACE INTO files (path, mtime_ms, size, thread_id, cwd, source, date, bytes_ingested, last_ord, repo_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"},
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
	started := time.Now().UnixMilli()
	backfillRepoKeysFromThreads(home, db)
	files, err := ListRolloutFiles(home, days)
	if err != nil {
		return IngestResult{}, err
	}
	stmt, err := db.Prepare("SELECT path, mtime_ms, size, bytes_ingested, last_ord FROM files")
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
		known[path] = &KnownFile{hitCountNumber(row["mtime_ms"]), hitCountNumber(row["size"]), hitCountNumber(row["bytes_ingested"]), hitCountNumber(row["last_ord"])}
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
		if prev != nil && fingerprintMatches(*prev, st) {
			continue
		}
		if err := ingestTransaction(db, func() error { return ingestFile(s, file, prev, st, &r) }); err != nil {
			return IngestResult{}, err
		}
	}
	if days == 0 {
		for _, path := range paths {
			if seen[path] {
				continue
			}
			if err := ingestTransaction(db, func() error {
				if _, err := s.delMsgs.Run(path); err != nil {
					return err
				}
				_, err := s.delFile.Run(path)
				return err
			}); err != nil {
				return IngestResult{}, err
			}
			r.Pruned++
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

func ingestFile(s ingestStatements, file RolloutFile, prev *KnownFile, st os.FileInfo, result *IngestResult) error {
	meta, err := ReadRolloutMeta(file.Path)
	if err != nil {
		return err
	}
	appendOnly := prev != nil && prev.BytesIngested > 0 && float64(st.Size()) > prev.Size
	var buf []byte
	ord, offset := float64(0), float64(0)
	if appendOnly {
		buf, err = readSlice(file.Path, int64(prev.BytesIngested), st.Size())
		ord, offset = prev.LastOrd, prev.BytesIngested
	} else {
		buf, err = os.ReadFile(file.Path)
	}
	if err != nil {
		return err
	}
	boundary := completeLineBoundary(buf)
	entries, err := ParseRollout(source.DecodeUTF8(buf[:boundary]), true)
	if err != nil {
		return err
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
	if _, err := s.insFile.Run(file.Path, st.ModTime().UnixMilli(), st.Size(), ingestString(meta.ThreadID), ingestString(meta.Cwd), string(meta.Source), file.Date, offset+float64(boundary), ord, ingestString(meta.RepoKey)); err != nil {
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
