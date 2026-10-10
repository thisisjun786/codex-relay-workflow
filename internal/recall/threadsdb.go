// Read-only metadata enrichment from CXC v0.2.40 recall/src/threads-db.ts.
package recall

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ThreadMeta keeps null distinct from empty strings, and accepts REAL timestamps.
type ThreadMeta struct {
	Title, Cwd              string
	GitBranch, GitOriginURL *string
	UpdatedAtMs             *float64
}

// IDs preserves Map order; empty Warning is null; failed enrichment keeps empty collections.
type ThreadMetaResult struct {
	ByID    map[string]ThreadMeta
	IDs     []string
	Warning string
}

func openReadOnlyDb(path string) (*RwDb, error) { return openDbReadOnly(path) }

func loadThreadMeta(path string) ThreadMetaResult {
	result := ThreadMetaResult{ByID: make(map[string]ThreadMeta), IDs: []string{}}
	if path == "" {
		result.Warning = "state db not found (metadata enrichment off)"
		return result
	}
	db, err := openReadOnlyDb(path)
	if err != nil {
		result.Warning = "state db unreadable (" + err.Error() + ")"
		return result
	}
	defer db.Close()
	read := func(query string) ([]map[string]any, error) {
		stmt, err := db.Prepare(query)
		if err != nil {
			return nil, err
		}
		return stmt.All()
	}
	// The columns are named by alias, so the rows carry these names whatever case the table declared.
	rows, err := read("SELECT id AS id, title AS title, cwd AS cwd, git_branch AS git_branch, git_origin_url AS git_origin_url, updated_at_ms AS updated_at_ms FROM threads")
	if err != nil && strings.EqualFold(err.Error(), "no such column: git_origin_url") {
		// Only a database from before the origin column is read without it; any other failure is reported.
		rows, err = read("SELECT id AS id, title AS title, cwd AS cwd, git_branch AS git_branch, updated_at_ms AS updated_at_ms FROM threads")
	}
	if err != nil {
		result.Warning = "state db unreadable (" + err.Error() + ")"
		return result
	}
	for _, row := range rows {
		id, ok := row["id"].(string)
		if !ok {
			continue
		}
		meta := ThreadMeta{}
		meta.Title, _ = row["title"].(string)
		meta.Cwd, _ = row["cwd"].(string)
		if value, ok := row["git_branch"].(string); ok {
			meta.GitBranch = &value
		}
		if value, ok := row["git_origin_url"].(string); ok && text.Trim(value) != "" {
			meta.GitOriginURL = &value
		}
		if value, ok := row["updated_at_ms"].(float64); ok {
			meta.UpdatedAtMs = &value
		}
		if _, exists := result.ByID[id]; !exists {
			result.IDs = append(result.IDs, id)
		}
		result.ByID[id] = meta
	}
	return result
}
