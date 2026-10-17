package store

// The collector adds provenance beside the immutable execution row. A worker
// path never changes the meaning of that row's worktree column.
var executorCollectionZone = []string{
	`CREATE TABLE IF NOT EXISTS executor_collections (
 execution_id TEXT PRIMARY KEY NOT NULL REFERENCES dag_agent_executions(execution_id),
 task_digest TEXT NOT NULL,
 result_digest TEXT NOT NULL,
 collection_json TEXT NOT NULL
 )`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_pk_guard BEFORE INSERT ON executor_collections
WHEN EXISTS (SELECT 1 FROM executor_collections WHERE execution_id = NEW.execution_id)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: executor_collections.execution_id'); END`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_no_update BEFORE UPDATE ON executor_collections
 BEGIN SELECT RAISE(ABORT, 'executor collections are immutable'); END`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_no_delete BEFORE DELETE ON executor_collections
 BEGIN SELECT RAISE(ABORT, 'executor collections are retained'); END`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_rowid_guard BEFORE INSERT ON executor_collections
WHEN NEW.rowid <> -1 AND EXISTS (SELECT 1 FROM executor_collections WHERE rowid = NEW.rowid)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: executor_collections.rowid'); END`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_rowid_minus1_mark_guard BEFORE INSERT ON executor_collections
WHEN NEW.rowid = -1
BEGIN DELETE FROM dag_zone_rowid_marks WHERE table_name = 'executor_collections';
  INSERT INTO dag_zone_rowid_marks (table_name) SELECT 'executor_collections' WHERE EXISTS (SELECT 1 FROM executor_collections WHERE rowid = -1); END`,
	`CREATE TRIGGER IF NOT EXISTS executor_collections_rowid_minus1_guard AFTER INSERT ON executor_collections
WHEN NEW.rowid = -1 AND EXISTS (SELECT 1 FROM dag_zone_rowid_marks WHERE table_name = 'executor_collections')
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: executor_collections.rowid'); END`,
}
