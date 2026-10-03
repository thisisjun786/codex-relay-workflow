// Package configguard is the Go form of CXC v0.2.40's config-guard component.
package configguard

// TomlScalar is the one value type the editor writes.
type TomlScalar = bool

// TomlEditAction says what an edit did.
type TomlEditAction string

// The six outcomes of an edit.
const (
	TomlUpdated           TomlEditAction = "updated"
	TomlInsertedIntoTable TomlEditAction = "inserted-into-table"
	TomlCreatedTable      TomlEditAction = "created-table"
	TomlRemoved           TomlEditAction = "removed"
	TomlNoop              TomlEditAction = "noop"
	TomlUnsupportedValue  TomlEditAction = "unsupported-value"
)

// TomlEditResult is the outcome of SetTableKey and RestoreTableKey.
type TomlEditResult struct {
	Content    string
	PriorValue *string
	Changed    bool
	Action     TomlEditAction
}

// KeyLine is a key line found by FindKeyLine.
type KeyLine struct {
	Index   int
	Indent  string
	Value   string
	Comment string
}

// TomlKeyLookup is what FindKeyLine found.
type TomlKeyLookup int

// The three answers of FindKeyLine.
const (
	TomlKeyAbsent TomlKeyLookup = iota
	TomlKeyFound
	TomlKeyUnsupported
)

func FindTableHeader(lines []string, header string) int { return -1 }

func TomlTableBody(content, header string) (string, bool) { return "", false }

func FindKeyLine(lines []string, headerIdx int, key string) (KeyLine, TomlKeyLookup) {
	return KeyLine{}, TomlKeyAbsent
}

func SetTableKey(content, table, key string, value TomlScalar) TomlEditResult {
	return TomlEditResult{Content: content, Action: TomlNoop}
}

func RestoreTableKey(content, table, key string, priorValue *string) TomlEditResult {
	return TomlEditResult{Content: content, Action: TomlNoop}
}

func ReadTableKey(content, table, key string) (string, bool) { return "", false }
