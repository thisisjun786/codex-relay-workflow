package recall

type ThreadMeta struct {
	Title, Cwd              string
	GitBranch, GitOriginURL *string
	UpdatedAtMs             *float64
}
type ThreadMetaResult struct {
	ByID    map[string]ThreadMeta
	IDs     []string
	Warning string
}

func openReadOnlyDb(path string) (*RwDb, error)   { return openDbReadOnly(path) }
func loadThreadMeta(path string) ThreadMetaResult { return ThreadMetaResult{} }
