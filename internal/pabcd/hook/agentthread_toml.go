package hook

type tomlKind string

const (
	tomlTable  tomlKind = "table"
	tomlArray  tomlKind = "array"
	tomlScalar tomlKind = "scalar"
)

type tomlEntry struct {
	kind     tomlKind
	children map[string]*tomlEntry
	declared bool
	latest   *tomlEntry
}

type tomlKeyPathResult struct {
	parts []string
	end   int
}

func newTomlTable() *tomlEntry {
	return &tomlEntry{kind: tomlTable, children: make(map[string]*tomlEntry)}
}
func finiteTomlNumber(string) (bool, error)                { return false, nil }
func validTomlDateTime(string) bool                        { return false }
func tomlKeyPath(string, int) *tomlKeyPathResult           { return nil }
func tomlAssignKey(*tomlEntry, []string) bool              { return false }
func tomlEnterTable(*tomlEntry, []string, bool) *tomlEntry { return nil }
func validTomlValue(string) bool                           { return false }
