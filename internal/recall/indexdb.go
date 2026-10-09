// CXC v0.2.40 recall/src/index-db.ts; storage only, no search or hook activation.
package recall

import (
	"errors"
	"os"
	"path/filepath"

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

func openIndexReadOnly(path string) (*RwDb, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("no index at " + path)
	}
	return openDbReadOnly(path)
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
