package testsupport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// HistoryIndex is one index CRW-301 added to the frozen v1 schema: what a store of the previous version lacks and
// a store of this version holds.
type HistoryIndex struct {
	Name   string
	Table  string
	SQL    string // the CREATE text SQLite keeps in sqlite_master (no IF NOT EXISTS)
	Reason string
}

// HistoryIndexesPath is the golden delta that lists them (contract/schema/relay-sqlite-history-indexes.json), the one
// place the tests that read the schema take the list from. A text in it never changes once shipped: the swap gate
// compares a store's objects with a candidate's by that text.
func HistoryIndexesPath() string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(FrozenStorePath()))), "schema", "relay-sqlite-history-indexes.json")
}

// HistoryIndexes reads the golden delta, in name order.
func HistoryIndexes() ([]HistoryIndex, error) {
	data, err := os.ReadFile(HistoryIndexesPath())
	if err != nil {
		return nil, err
	}
	var delta struct {
		Indexes map[string]struct {
			Table  string `json:"table"`
			SQL    string `json:"sql"`
			Reason string `json:"reason"`
		} `json:"indexes"`
	}
	if err := json.Unmarshal(data, &delta); err != nil {
		return nil, err
	}
	out := make([]HistoryIndex, 0, len(delta.Indexes))
	for name, index := range delta.Indexes {
		out = append(out, HistoryIndex{Name: name, Table: index.Table, SQL: index.SQL, Reason: index.Reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
