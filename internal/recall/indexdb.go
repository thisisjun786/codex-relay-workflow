package recall

import (
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

const IndexSchemaVersion = "2"

type IndexStatus struct {
	Path         string  `json:"path"`
	Files        float64 `json:"files"`
	Msgs         float64 `json:"msgs"`
	LastIngestAt *string `json:"lastIngestAt"`
}

func indexPath(...host.LookupEnv) (string, error) { return "", errors.New("index port pending") }
func openIndex(string) (*RwDb, error)             { return nil, errors.New("index port pending") }
func openIndexReadOnly(string) (*RwDb, error)     { return nil, errors.New("index port pending") }
func filesHasColumn(*RwDb, string) bool           { return false }
func ensureRepoKeyColumn(*RwDb)                   {}
func indexStatus(*RwDb, string) (IndexStatus, error) {
	return IndexStatus{}, errors.New("index port pending")
}
