// CXC v0.2.40 recall/src/index-db.ts; storage only, no search or hook activation.
package recall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// IndexSchemaVersion 3 added the unique (path, ord) invariant; 4 adds the file identity and the
// consumed-prefix checkpoint to files. A database of an older version holds only derived rows, so
// it is dropped and rebuilt from the rollouts; the hit history is kept.
const IndexSchemaVersion = "4"
const hitCountsDDL = `
CREATE TABLE IF NOT EXISTS recall_hit_counts (
  ref TEXT PRIMARY KEY,
  hit_count INTEGER NOT NULL DEFAULT 0,
  last_hit_at TEXT NOT NULL
);
`
const indexSchema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files (
  path TEXT PRIMARY KEY,
  mtime_ms INTEGER NOT NULL,
  size INTEGER NOT NULL,
  thread_id TEXT,
  cwd TEXT,
  source TEXT NOT NULL,
  date TEXT NOT NULL,
  bytes_ingested INTEGER NOT NULL DEFAULT 0,
  last_ord INTEGER NOT NULL DEFAULT 0,
  repo_key TEXT,
  file_id TEXT,
  checkpoint TEXT
);
CREATE TABLE IF NOT EXISTS msgs (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL,
  ord INTEGER NOT NULL,
  ts TEXT NOT NULL,
  role TEXT NOT NULL,
  match_field TEXT NOT NULL,
  synthetic INTEGER NOT NULL,
  text TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_msgs_path_ord ON msgs(path, ord);
CREATE INDEX IF NOT EXISTS idx_msgs_ts ON msgs(ts DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS msgs_fts USING fts5(
  text, content='msgs', content_rowid='id', tokenize='unicode61'
);
CREATE VIRTUAL TABLE IF NOT EXISTS msgs_tri USING fts5(
  text, content='msgs', content_rowid='id', tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS msgs_ai AFTER INSERT ON msgs BEGIN
  INSERT INTO msgs_fts(rowid, text) VALUES (new.id, new.text);
  INSERT INTO msgs_tri(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS msgs_ad AFTER DELETE ON msgs BEGIN
  INSERT INTO msgs_fts(msgs_fts, rowid, text) VALUES ('delete', old.id, old.text);
  INSERT INTO msgs_tri(msgs_tri, rowid, text) VALUES ('delete', old.id, old.text);
END;
` + hitCountsDDL

// IndexStatus describes the derived cache; absent ingest metadata stays null.
type IndexStatus struct {
	Path         string  `json:"path"`
	Files        float64 `json:"files"`
	Msgs         float64 `json:"msgs"`
	LastIngestAt *string `json:"lastIngestAt"`
}

func indexPath(env ...host.LookupEnv) (string, error) {
	lookup := host.LookupEnv(os.LookupEnv)
	if len(env) > 0 {
		lookup = env[0]
	}
	if value, _ := lookup("CRW_HOME"); text.Trim(value) == "" {
		if _, err := recallHome(lookup); err != nil { // The default would be the workspace's.
			return "", err
		}
	}
	home, err := host.CRWHome(lookup)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "recall", "index.sqlite"), nil
}

func openIndex(path string) (*RwDb, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700) // Oracle tolerates non-POSIX chmod failures.
	db, err := openDbReadWrite(path)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			_ = db.Close()
		}
	}()
	for _, q := range []string{"PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if err = db.Exec(q); err != nil {
			return nil, err
		}
	}
	if err = initializeIndex(db); err != nil {
		return nil, err
	}
	ensureRepoKeyColumn(db)
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(file, 0o600)
	}
	opened = true
	return db, nil
}

// Schema/reset is atomic: failed destructive DDL cannot erase existing rows.
func initializeIndex(db *RwDb) error {
	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = db.Exec("ROLLBACK")
		}
	}()
	// The version is read before the schema is applied: a unique index cannot be created over
	// the duplicate rows an older version may hold.
	if err := db.Exec("CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);"); err != nil {
		return err
	}
	stmt, err := db.Prepare("SELECT value FROM meta WHERE key = 'schema_version'")
	if err != nil {
		return err
	}
	row, err := stmt.Get()
	if err != nil {
		return err
	}
	if row != nil && row["value"] != IndexSchemaVersion {
		// Only derived tables are rebuilt. Injection history is not rebuildable.
		if err = db.Exec("DROP TRIGGER IF EXISTS msgs_ai; DROP TRIGGER IF EXISTS msgs_ad;\nDROP TABLE IF EXISTS msgs_fts; DROP TABLE IF EXISTS msgs_tri;\nDROP TABLE IF EXISTS msgs; DROP TABLE IF EXISTS files; DROP TABLE IF EXISTS meta;"); err != nil {
			return err
		}
	}
	if err = db.Exec(indexSchema); err != nil {
		return err
	}
	if row == nil || row["value"] != IndexSchemaVersion {
		stmt, err = db.Prepare("INSERT INTO meta (key, value) VALUES ('schema_version', ?)")
		if err != nil {
			return err
		}
		if _, err = stmt.Run(IndexSchemaVersion); err != nil {
			return err
		}
	}
	if err = db.Exec("COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func filesHasColumn(db *RwDb, name string) bool {
	stmt, err := db.Prepare("PRAGMA table_info(files)")
	if err != nil {
		return false
	}
	rows, err := stmt.All()
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row["name"] == name {
			return true
		}
	}
	return false
}

func ensureRepoKeyColumn(db *RwDb) {
	if !filesHasColumn(db, "repo_key") {
		if err := db.Exec("ALTER TABLE files ADD COLUMN repo_key TEXT"); err != nil {
			return
		}
	}
	// Read-only and concurrent migrations degrade exactly as the oracle does.
	_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_files_repo_key ON files(repo_key)")
}

// openIndexReadOnly opens the index for reading rows. An index of another schema version is refused:
// its rows were written under other rules (the repository key, the cursor), so a reader must not
// serve them. Only a writer that goes on to ingest resets it; the refusal sends a search to the scan.
func openIndexReadOnly(path string) (*RwDb, error) {
	db, err := openIndexReadOnlyAnySchema(path)
	if err != nil {
		return nil, err
	}
	if version, current := indexSchemaOf(db); !current {
		_ = db.Close()
		return nil, fmt.Errorf("index at %s is of schema version %s, not %s — a refreshing search or `recall chat index` rebuilds it", path, version, IndexSchemaVersion)
	}
	return db, nil
}

// openIndexReadOnlyAnySchema is for the status, which reports on an index of an older schema.
func openIndexReadOnlyAnySchema(path string) (*RwDb, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("no index at " + path)
	}
	return openDbReadOnly(path)
}

// indexSchemaOf reads the version the index was written under; an index that records none is current.
func indexSchemaOf(db *RwDb) (version string, current bool) {
	stmt, err := db.Prepare("SELECT value FROM meta WHERE key = 'schema_version'")
	if err != nil {
		return "", true
	}
	row, err := stmt.Get()
	if err != nil || row == nil {
		return "", true
	}
	version, _ = row["value"].(string)
	return version, version == IndexSchemaVersion
}

// hitStoreBusyMs bounds how long the hook's history store waits for the write lock of the index.
const hitStoreBusyMs = 1000

// openHitCountStore opens the index file for the repeat history only. It never applies the schema or
// resets derived rows: reading or counting the history is not a migration of the search index, which
// stays as it is until a writer that ingests rebuilds it.
func openHitCountStore(path string) (*RwDb, error) {
	return openHitCountStoreBusy(path, hitStoreBusyMs)
}

// hitStoreBusyWithin is the lock wait the history store may use before until: its own bound, or what is
// left of the time (rounded up to the millisecond), whichever is less; false when nothing is left.
func hitStoreBusyWithin(until time.Time) (int, bool) {
	left := time.Until(until)
	if left <= 0 {
		return 0, false
	}
	return min(hitStoreBusyMs, int((left+time.Millisecond-1)/time.Millisecond)), true
}

// openHitCountStoreBusy is openHitCountStore with the lock wait of the connection set to busyMs.
func openHitCountStoreBusy(path string, busyMs int) (*RwDb, error) {
	db, err := openDbReadWrite(path)
	if err != nil {
		return nil, err
	}
	if err = db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyMs)); err != nil {
		_ = db.Close()
		return nil, err
	}
	stmt, err := db.Prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'recall_hit_counts'")
	if err == nil {
		var row map[string]any
		if row, err = stmt.Get(); err == nil && row == nil {
			err = db.Exec(hitCountsDDL)
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func indexStatus(db *RwDb, path string) (IndexStatus, error) {
	status := IndexStatus{Path: path}
	for _, item := range []struct {
		query string
		dest  *float64
	}{
		{"SELECT COUNT(*) AS n FROM files", &status.Files},
		{"SELECT COUNT(*) AS n FROM msgs", &status.Msgs},
	} {
		stmt, err := db.Prepare(item.query)
		if err != nil {
			return IndexStatus{}, err
		}
		row, err := stmt.Get()
		if err != nil {
			return IndexStatus{}, err
		}
		*item.dest = row["n"].(float64)
	}
	stmt, err := db.Prepare("SELECT value FROM meta WHERE key = 'last_ingest_at'")
	if err != nil {
		return IndexStatus{}, err
	}
	row, err := stmt.Get()
	if err != nil {
		return IndexStatus{}, err
	}
	if row != nil {
		if value, ok := row["value"].(string); ok {
			status.LastIngestAt = &value
		}
	}
	return status, nil
}
